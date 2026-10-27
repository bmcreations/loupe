// Command uiprobe drives one live WebTransport session end to end -- touch,
// events, health -- against a running loupe server, the way cmd/wtprobe
// verifies the video path.
//
// A browser cannot tell you which half of the round trip failed: an event
// that never arrives looks the same whether the send was silently dropped,
// the gesture assembler never closed it, or the seq ring wrapped before the
// viewer read it. This probe sends real input over a real control stream and
// checks the server's own replies against docs/protocol.md's fixtures, so a
// disagreement is a decode mismatch against a pinned byte layout rather than
// a guess from a screenshot.
//
//	go run ./cmd/uiprobe "https://100.64.0.10:18456/wt?target=synthetic"
//	go run ./cmd/uiprobe -agent "https://100.64.0.10:18456/wt?target=emulator-5554"
//
// Auth: -token, or LOUPE_TOKEN from the environment. It rides the /wt URL's
// query string (WebTransport sends no cookies, and CLAUDE.md's auth section
// is explicit that redirecting ?token= off /wt would strip the one
// credential that transport has) and as an Authorization: Bearer header on
// the plain HTTP calls (/api/input, /api/events).
//
// Target guard: refuses to run against anything but "synthetic" or
// "emulator-*" unless -target-ok is passed. The probe drives real input --
// taps, a swipe, and with -agent a "home" button press -- so this is what
// stops a copy-pasted command line reaching a device someone is using. See
// the warning in CLAUDE.md about physical devices.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/httpapi"
	"github.com/bmcreations/loupe/internal/session"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
)

func main() {
	agent := flag.Bool("agent", false, "also drive POST /api/input and require the matching agent-sourced event (check 5)")
	targetOK := flag.Bool("target-ok", false, "override the synthetic/emulator-* target guard")
	token := flag.String("token", "", "shared token (default: LOUPE_TOKEN)")
	insecure := flag.Bool("insecure", true, "skip certificate verification (dialing by IP)")
	timeout := flag.Duration("timeout", 20*time.Second, "overall probe deadline")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: uiprobe [flags] https://host:port/wt?target=ID")
		os.Exit(2)
	}

	results, err := run(flag.Arg(0), *agent, *targetOK, *token, *insecure, *timeout)
	for _, r := range results {
		fmt.Println(r)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe failed:", err)
		os.Exit(1)
	}
	if anyFail(results) {
		os.Exit(1)
	}
}

// anyFail reports whether results contains a FAIL, which is main's whole
// exit-code decision -- pulled out so a table test can drive it directly
// instead of only through main's os.Exit, and so a SKIP or PASS cannot be
// mistaken for a FAIL (or vice versa) without a test catching it.
func anyFail(results []checkResult) bool {
	for _, r := range results {
		if r.status == "FAIL" {
			return true
		}
	}
	return false
}

// run performs the guard check, then every wire check it can against the
// target, and returns one checkResult per line printed. A non-nil error
// means the probe could not even get far enough to produce a useful set of
// PASS/FAIL/SKIP lines (a bad URL, an auth failure, a dial failure); the
// guard refusal is instead reported as a single FAIL result with a nil
// error, so it prints the same way every other check does.
//
// This is runWithOptions with the CLI's own defaults (forceInput false,
// healthTimeout the package default) -- main never needs anything else, but
// hermetic_test.go's httptest checks do: forceInput to get tap/swipe/seq/
// replay to run against synthetic's control:false session, and a shrunk
// healthTimeout so a server that never sends MsgStats fails the test in
// milliseconds rather than on run's own wall-clock timeout.
func run(rawURL string, agentMode, targetOK bool, token string, insecure bool, timeout time.Duration) ([]checkResult, error) {
	return runWithOptions(rawURL, probeOptions{
		agentMode: agentMode,
		targetOK:  targetOK,
		token:     token,
		insecure:  insecure,
		timeout:   timeout,
	})
}

// probeOptions is run's parameters plus the two test-only knobs
// (forceInput, healthTimeout) that have no CLI flag. Both are documented on
// probeRun, which is where they end up.
type probeOptions struct {
	agentMode bool
	targetOK  bool
	token     string
	insecure  bool
	timeout   time.Duration

	forceInput    bool
	healthTimeout time.Duration // 0 means checkTimeout

	// checkTimeout, when non-zero, overrides the package checkTimeout const
	// as the wait bound for checks 2/3/5 (tap, swipe, agent-button). It
	// exists so a test whose gesture is expected to arrive quickly (or not
	// at all) is not stuck paying the full default on every case; run itself
	// never sets it.
	checkTimeout time.Duration

	// skipDrain disables the drainVideoUniStreams goroutine below. It exists
	// only for TestHealthFailsWithoutDrainingVideoStreams, which proves the
	// comment at that call site is true of the actual code rather than just
	// asserted in prose: without draining, OpenUniStream starts failing once
	// the session's uni-stream credit runs out -- MaxIncomingUniStreams: 64
	// in runWithOptions below, so around the 64th access unit synthetic
	// produces -- frameDelivered treats that as fatal, and the viewer is torn
	// down well before check 4 sees a MsgStats.
	skipDrain bool
}

func runWithOptions(rawURL string, opts probeOptions) ([]checkResult, error) {
	agentMode, targetOK, token, insecure, timeout := opts.agentMode, opts.targetOK, opts.token, opts.insecure, opts.timeout
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	targetID := u.Query().Get("target")

	if token == "" {
		token = os.Getenv("LOUPE_TOKEN")
	}

	// Guard first, before any network activity: a wrong URL must not so much
	// as open a connection to the wrong device.
	if ok, detail := guardDecision(targetID, targetOK); !ok {
		return []checkResult{fail("guard", detail)}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()

	qconn, err := quic.DialAddr(ctx, u.Host,
		&tls.Config{InsecureSkipVerify: insecure, NextProtos: []string{"h3"}},
		&quic.Config{EnableDatagrams: true, InitialPacketSize: 1200, EnableStreamResetPartialDelivery: true},
	)
	if err != nil {
		return nil, fmt.Errorf("dial quic: %w", err)
	}

	tr := &webtransport.Transport{
		Config: &webtransport.Config{
			MaxIncomingStreams:    64,
			MaxIncomingUniStreams: 64,
			MaxIncomingData:       16 << 20,
		},
	}
	cc, err := tr.NewClientConn(qconn)
	if err != nil {
		return nil, fmt.Errorf("new client conn: %w", err)
	}

	rsp, wtSess, err := cc.Dial(ctx, u.String(), nil)
	// webtransport-go's ClientConn.dial returns the response alongside a
	// non-nil error for any non-2xx CONNECT reply (client_conn.go: "received
	// status %d"), specifically so the caller can read the status code
	// rather than parse it back out of the error text. Check rsp before
	// err, or a 401 falls into the generic dial-failure path below and
	// never gets its own clear line.
	if rsp != nil && rsp.StatusCode == http.StatusUnauthorized {
		return []checkResult{fail("auth", "CONNECT returned 401; token rejected")}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	if rsp.StatusCode < 200 || rsp.StatusCode >= 300 {
		return nil, fmt.Errorf("CONNECT returned %d", rsp.StatusCode)
	}

	control, err := wtSess.OpenStreamSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("open control stream: %w", err)
	}
	if _, err := control.Write(frameControl([]byte{session.MsgHello})); err != nil {
		return nil, fmt.Errorf("send hello: %w", err)
	}

	c := newCollector(control)
	go c.readLoop()

	// Drain and discard every video access unit, the same way cmd/wtprobe
	// does. This probe has no use for the frames themselves, but the server
	// ships one unidirectional stream per access unit (CLAUDE.md's Shape
	// section), and OpenUniStream does not block on a stream nobody accepts
	// -- it refuses immediately once session stream credit runs out
	// (webtransport.go's SendFrame, "open frame stream: too many open
	// streams"). frameDelivered treats that as fatal, so the writer
	// goroutine returns and the viewer is torn down, not blocked: without
	// this, check 4 loses the session outright within the first few frames
	// on a synthetic target producing them as fast as a real one would,
	// which is exactly what a browser or cmd/wtprobe accepting the streams
	// avoids. TestHealthFailsWithoutDrainingVideoStreams reproduces it.
	if !opts.skipDrain {
		go drainVideoUniStreams(ctx, wtSess)
	}

	healthTimeout := opts.healthTimeout
	if healthTimeout == 0 {
		healthTimeout = checkTimeout
	}
	gestureTimeout := opts.checkTimeout
	if gestureTimeout == 0 {
		gestureTimeout = checkTimeout
	}
	p := &probeRun{
		c:             c,
		control:       control,
		wtSess:        wtSess,
		baseURL:       httpBase(*u),
		targetID:      targetID,
		token:         token,
		insecure:      insecure,
		agentMode:     agentMode,
		forceInput:    opts.forceInput,
		healthTimeout: healthTimeout,
		checkTimeout:  gestureTimeout,
	}
	results := p.checks(ctx)

	_ = wtSess.CloseWithError(0, "probe done")
	return results, nil
}

// drainVideoUniStreams accepts every server-opened unidirectional stream --
// one per access unit over WebTransport -- and reads it to completion,
// discarding the bytes. It exists only to keep the server's send path
// unblocked; see the comment at its call site in run. Returns once ctx is
// done or AcceptUniStream otherwise errors (session closed).
func drainVideoUniStreams(ctx context.Context, wtSess *webtransport.Session) {
	for {
		st, err := wtSess.AcceptUniStream(ctx)
		if err != nil {
			return
		}
		go func() {
			_, _ = io.Copy(io.Discard, st)
		}()
	}
}

// httpBase derives the plain https:// origin (scheme + host) that
// /api/input and /api/events live under, from the /wt URL.
func httpBase(u url.URL) string {
	u.Path = ""
	u.RawQuery = ""
	u.Scheme = "https"
	return u.String()
}

// probeRun holds everything the checks need. Splitting it from run keeps
// run's own job -- dial, guard, teardown -- separate from what each check
// does, and lets a check be read on its own.
type probeRun struct {
	c         *collector
	control   *webtransport.Stream
	wtSess    *webtransport.Session
	baseURL   string
	targetID  string
	token     string
	insecure  bool
	agentMode bool

	// forceInput bypasses the init.Control gate on checks 2/3, so a test can
	// drive tap/swipe against a target that reports control:false and still
	// see the checks run to completion. Synthetic reports control:false --
	// target.Synthetic's Touch always returns ErrNoControl -- but
	// Session.ViewerTouch publishes the gesture event regardless of that
	// error ("Forwarding comes first and is not conditional on the
	// assembler", session.go), so tap/swipe/seq/replay all still have
	// something real to check against a synthetic session. Left false in
	// run, where the gate exists to keep the printed SKIP honest about what
	// a real emulator or device target will do.
	forceInput bool

	// healthTimeout bounds check 4's wait for a MsgStats carrying health.
	// run sets it to checkTimeout; a test shrinks it so a fake server that
	// never sends one fails fast rather than on the wall clock.
	healthTimeout time.Duration

	// checkTimeout bounds checks 2/3/5's wait for the event a sent gesture
	// or agent action should have produced. run sets it to the package
	// checkTimeout const; a test may shrink it the same way healthTimeout is
	// shrunk above.
	checkTimeout time.Duration

	seenSeqs []uint64
}

// checkTimeout is how long checks 2, 3, 5 and 6 wait for the event a sent
// gesture or agent action should have produced, and the default for
// healthTimeout above. TestCheckTimeoutIsTwoSeconds pins it: the tests in
// hermetic_test.go rely on the default being small enough that a passing run
// stays fast, and on it being this exact value rather than whatever a future
// edit leaves behind.
const checkTimeout = 2 * time.Second

func (p *probeRun) checks(ctx context.Context) []checkResult {
	var out []checkResult

	// Check 1: join, hello, init.
	init, err := p.c.waitInit(5 * time.Second)
	if err != nil {
		out = append(out, fail("join", err.Error()))
		// Nothing below this can run without codedWidth/codedHeight.
		out = append(out, skip("tap", "no init"), skip("swipe", "no init"),
			skip("health", "no init"))
		return out
	}
	out = append(out, pass("join", fmt.Sprintf(
		"init: codec=%s coded=%dx%d kind=%s control=%v",
		init.Codec, init.CodedWidth, init.CodedHeight, init.Kind, init.Control)))

	// Checks 2/3: tap and swipe, only against a target that accepts input.
	// Synthetic's session reports control:false (session.go's
	// t.Kind() != target.KindSynthetic), and target.Synthetic's Touch
	// returns ErrNoControl -- sending a touch would just produce a silent
	// no-op, not a gesture to check, so these are reported as SKIP rather
	// than as a misleading PASS or a FAIL for behaviour that was never
	// expected to work.
	if !init.Control && !p.forceInput {
		out = append(out, skip("tap", "target has no input (init.control=false)"))
		out = append(out, skip("swipe", "target has no input (init.control=false)"))
	} else {
		out = append(out, p.checkTap(init))
		out = append(out, p.checkSwipe(init))
	}

	// Check 4: at least one MsgStats carrying health.
	if p.c.waitStatsHealth(p.healthTimeout) {
		out = append(out, pass("health", "received a MsgStats with a health field"))
	} else {
		out = append(out, fail("health", fmt.Sprintf("no MsgStats carrying health arrived within %v", p.healthTimeout)))
	}

	// Check 5: agent-sourced button, only with -agent and only against a
	// controllable target.
	if !p.agentMode {
		out = append(out, skip("agent-button", "run with -agent to exercise POST /api/input"))
	} else if !init.Control {
		out = append(out, skip("agent-button", "target has no input (init.control=false)"))
	} else {
		out = append(out, p.checkAgentButton())
	}

	// Check 6: seq strictly increasing, then replay from /api/events.
	seqs := p.c.eventSeqs()
	seqResult, runReplay := seqCheck(seqs)
	out = append(out, seqResult)
	switch {
	case runReplay:
		out = append(out, p.checkReplay(ctx, seqs))
	case len(seqs) == 0:
		out = append(out, skip("replay", "no events observed"))
	default:
		out = append(out, skip("replay", "seq check failed"))
	}

	return out
}

// seqCheck is check 6's decision, pulled out of checks() so a test can drive
// it with a fabricated out-of-order slice -- a real session's own seqs are
// always strictly increasing, so nothing short of this split can exercise
// the FAIL branch without a live server that misbehaves. runReplay is false
// whenever seq itself is not a clean PASS: replay has nothing trustworthy to
// check against if seq was empty or out of order.
func seqCheck(seqs []uint64) (result checkResult, runReplay bool) {
	if len(seqs) == 0 {
		return skip("seq", "no events observed"), false
	}
	if !seqStrictlyIncreasing(seqs) {
		return fail("seq", fmt.Sprintf("not strictly increasing: %v", seqs)), false
	}
	return pass("seq", fmt.Sprintf("%d events, strictly increasing: %v", len(seqs), seqs)), true
}

func (p *probeRun) checkTap(init session.InitPayload) checkResult {
	x, y := int32(init.CodedWidth/2), int32(init.CodedHeight/2)
	if err := p.sendTap(x, y); err != nil {
		return fail("tap", err.Error())
	}
	want := gestureWant{
		source: "viewer", kind: "tap",
		from: [2]float64{float64(x) / float64(init.CodedWidth), float64(y) / float64(init.CodedHeight)},
	}
	e, ok := p.c.waitEvent(func(e session.Event) bool { return matchesGestureIgnoringErr(e, want) }, p.checkTimeout)
	if ok {
		p.seenSeqs = append(p.seenSeqs, e.Seq)
	}
	return gestureOutcome("tap", init.Control, p.checkTimeout, e, ok)
}

func (p *probeRun) checkSwipe(init session.InitPayload) checkResult {
	x := int32(init.CodedWidth / 2)
	y0 := int32(float64(init.CodedHeight) * 0.75)
	y1 := int32(float64(init.CodedHeight) * 0.25)
	if err := p.sendSwipe(x, y0, y1); err != nil {
		return fail("swipe", err.Error())
	}
	want := gestureWant{
		source: "viewer", kind: "swipe",
		from:  [2]float64{float64(x) / float64(init.CodedWidth), 0.75},
		hasTo: true,
		to:    [2]float64{float64(x) / float64(init.CodedWidth), 0.25},
	}
	e, ok := p.c.waitEvent(func(e session.Event) bool { return matchesGestureIgnoringErr(e, want) }, p.checkTimeout)
	if ok {
		p.seenSeqs = append(p.seenSeqs, e.Seq)
	}
	return gestureOutcome("swipe", init.Control, p.checkTimeout, e, ok)
}

func (p *probeRun) checkAgentButton() checkResult {
	req := httpapi.InputRequest{Action: "button", Button: "home"}
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequest(http.MethodPost,
		p.baseURL+"/api/input?target="+url.QueryEscape(p.targetID), bytes.NewReader(body))
	if err != nil {
		return fail("agent-button", err.Error())
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.token)
	httpReq.Header.Set("Content-Type", "application/json")
	rsp, err := httpClient(p.insecure).Do(httpReq)
	if err != nil {
		return fail("agent-button", fmt.Sprintf("POST /api/input: %v", err))
	}
	defer rsp.Body.Close()
	if rsp.StatusCode == http.StatusUnauthorized {
		return fail("agent-button", "POST /api/input returned 401; token rejected")
	}
	if rsp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(rsp.Body)
		return fail("agent-button", fmt.Sprintf("POST /api/input returned %d: %s", rsp.StatusCode, b))
	}

	e, ok := p.c.waitEvent(func(e session.Event) bool { return matchesButton(e, "agent", "home") }, p.checkTimeout)
	if !ok {
		return fail("agent-button", fmt.Sprintf("no matching agent/button(home) event within %v", p.checkTimeout))
	}
	p.seenSeqs = append(p.seenSeqs, e.Seq)
	return pass("agent-button", fmt.Sprintf("seq=%d", e.Seq))
}

func (p *probeRun) checkReplay(ctx context.Context, seen []uint64) checkResult {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.baseURL+"/api/events?target="+url.QueryEscape(p.targetID)+"&after=0", nil)
	if err != nil {
		return fail("replay", err.Error())
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.token)
	rsp, err := httpClient(p.insecure).Do(httpReq)
	if err != nil {
		return fail("replay", fmt.Sprintf("GET /api/events: %v", err))
	}
	defer rsp.Body.Close()
	if rsp.StatusCode == http.StatusUnauthorized {
		return fail("replay", "GET /api/events returned 401; token rejected")
	}
	if rsp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(rsp.Body)
		return fail("replay", fmt.Sprintf("GET /api/events returned %d: %s", rsp.StatusCode, b))
	}
	var page session.EventPage
	if err := json.NewDecoder(rsp.Body).Decode(&page); err != nil {
		return fail("replay", fmt.Sprintf("decode EventPage: %v", err))
	}
	missing := missingFromPage(seen, page)
	if len(missing) > 0 {
		return fail("replay", fmt.Sprintf("seq %v seen live but missing from /api/events?after=0 (session=%s oldest=%d latest=%d)",
			missing, page.Session, page.Oldest, page.Latest))
	}
	return pass("replay", fmt.Sprintf("all %d observed seq present in /api/events?after=0", len(seen)))
}

// sendTap sends a realistic down/up pair at one point.
func (p *probeRun) sendTap(x, y int32) error {
	const pointerID = 1
	if err := p.writeTouch(encodeTouch(0, pointerID, x, y, 0xffff)); err != nil {
		return fmt.Errorf("touch down: %w", err)
	}
	time.Sleep(16 * time.Millisecond)
	if err := p.writeTouch(encodeTouch(1, pointerID, x, y, 0)); err != nil {
		return fmt.Errorf("touch up: %w", err)
	}
	return nil
}

// sendSwipe sends down, three moves 16ms apart, then up -- the spacing the
// plan calls for, matching what a real 60Hz touchscreen driver produces
// rather than one down/up pair that a gesture assembler would read as a
// single large jump.
func (p *probeRun) sendSwipe(x, y0, y1 int32) error {
	const pointerID = 1
	if err := p.writeTouch(encodeTouch(0, pointerID, x, y0, 0xffff)); err != nil {
		return fmt.Errorf("touch down: %w", err)
	}
	steps := 3
	for i := 1; i <= steps; i++ {
		time.Sleep(16 * time.Millisecond)
		y := y0 + int32(float64(y1-y0)*float64(i)/float64(steps))
		if err := p.writeTouch(encodeTouch(2, pointerID, x, y, 0xffff)); err != nil {
			return fmt.Errorf("touch move %d: %w", i, err)
		}
	}
	time.Sleep(16 * time.Millisecond)
	if err := p.writeTouch(encodeTouch(1, pointerID, x, y1, 0)); err != nil {
		return fmt.Errorf("touch up: %w", err)
	}
	return nil
}

func (p *probeRun) writeTouch(b []byte) error {
	_, err := p.control.Write(frameControl(b))
	return err
}

func httpClient(insecure bool) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure},
		},
	}
}

// collector reads every control-stream frame in the background and sorts it
// by type so the sequential checks above can wait on the piece they need
// without racing the reader.
type collector struct {
	r io.Reader

	mu     sync.Mutex
	init   *session.InitPayload
	initCh chan struct{}
	initOK bool

	sawStatsHealth bool
	statsCh        chan struct{}

	events   []session.Event
	eventsCh chan struct{} // closed and replaced on every new event
}

func newCollector(r io.Reader) *collector {
	return &collector{
		r:        r,
		initCh:   make(chan struct{}),
		statsCh:  make(chan struct{}),
		eventsCh: make(chan struct{}),
	}
}

func (c *collector) readLoop() {
	for {
		frame, err := readControlFrame(c.r)
		if err != nil {
			return
		}
		if len(frame) == 0 {
			continue
		}
		switch frame[0] {
		case session.MsgInit:
			var init session.InitPayload
			if err := json.Unmarshal(frame[1:], &init); err == nil {
				c.mu.Lock()
				if !c.initOK {
					c.init, c.initOK = &init, true
					close(c.initCh)
				}
				c.mu.Unlock()
			}
		case session.MsgStats:
			var s struct {
				Health string `json:"health"`
			}
			if err := json.Unmarshal(frame[1:], &s); err == nil && s.Health != "" {
				c.mu.Lock()
				if !c.sawStatsHealth {
					c.sawStatsHealth = true
					close(c.statsCh)
				}
				c.mu.Unlock()
			}
		case session.MsgEvent:
			e, err := decodeEvent(frame)
			if err == nil {
				c.mu.Lock()
				c.events = append(c.events, e)
				close(c.eventsCh)
				c.eventsCh = make(chan struct{})
				c.mu.Unlock()
			}
		}
	}
}

func (c *collector) waitInit(timeout time.Duration) (session.InitPayload, error) {
	select {
	case <-c.initCh:
		c.mu.Lock()
		defer c.mu.Unlock()
		return *c.init, nil
	case <-time.After(timeout):
		return session.InitPayload{}, fmt.Errorf("no MsgInit within %v", timeout)
	}
}

func (c *collector) waitStatsHealth(timeout time.Duration) bool {
	select {
	case <-c.statsCh:
		return true
	case <-time.After(timeout):
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.sawStatsHealth
	}
}

// waitEvent polls the already-seen events plus every newly arrived one until
// pred matches or timeout elapses.
func (c *collector) waitEvent(pred func(session.Event) bool, timeout time.Duration) (session.Event, bool) {
	deadline := time.Now().Add(timeout)
	for {
		c.mu.Lock()
		for _, e := range c.events {
			if pred(e) {
				c.mu.Unlock()
				return e, true
			}
		}
		ch := c.eventsCh
		c.mu.Unlock()

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return session.Event{}, false
		}
		select {
		case <-ch:
		case <-time.After(remaining):
			return session.Event{}, false
		}
	}
}

func (c *collector) eventSeqs() []uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]uint64, len(c.events))
	for i, e := range c.events {
		out[i] = e.Seq
	}
	return out
}

// readControlFrame reads one length-prefixed message: a u32 big-endian
// length followed by that many bytes, the same framing
// internal/httpapi/webtransport.go's wtReadControlFrame reads on the server
// side and testdata/protocol/wt-control-hello.hex pins.
func readControlFrame(r io.Reader) ([]byte, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(prefix[:])
	const limit = 1 << 20
	if n > limit {
		return nil, fmt.Errorf("control frame of %d bytes exceeds %d", n, limit)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
