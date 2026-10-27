package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/httpapi"
	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
	"github.com/bmcreations/loupe/web"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

// This file is uiprobe's own version of what cmd/loupe/main.go wires
// together, built in-process rather than as a subprocess. It exists so the
// checks in run/checks -- the guard's wiring, the Bearer/?token= plumbing,
// the health-timeout path, and the control:false SKIP -- have something real
// to run against without the cost or the safety surface of a live device.
// Nothing here touches the network beyond 127.0.0.1.
//
// This is deliberately not cmd/wtprobe's job: wtprobe (and this package's own
// run) verify the *client* side against a real server, already covered by the
// "Verification discipline" runs recorded in the commit. What is untested
// without this file is uiprobe's own decision logic -- run/checks itself:
// guard wiring, header presence, and the health timeout all survived
// mutation in review because nothing exercised them end to end.
//
// The WebTransport server setup below (TLSConfig, QUICConfig,
// AdditionalSettings, webtransport.Config, ConfigureHTTP3Server) mirrors
// cmd/loupe/main.go's quicConfig, newWebTransportServer, sessionFlowControl
// and webTransportSettings. It cannot import them: both are `package main`,
// and Go does not allow importing a main package from anywhere, exported or
// not. Duplicated rather than factored into a shared internal package, since
// that refactor touches the live server's own wiring and is out of scope
// here; see those four functions' doc comments in cmd/loupe/main.go for why
// each field is load-bearing.

// hermeticServer is a full httpapi.Server -- manager, one registered
// target.Synthetic, HTTP over TLS and WebTransport over QUIC on the same
// address -- listening on 127.0.0.1. It is what a real loupe process would
// serve with `-synthetic -tls-cert ... -tls-key ...`, minus adb discovery.
type hermeticServer struct {
	addr    string
	token   string // "" if auth is off
	mgr     *httpapi.Manager
	httpSrv *http.Server
	wtSrv   *webtransport.Server
	httpLn  net.Listener
	tlsCert tls.Certificate
}

// wtURL is the /wt URL for target "synthetic", with no ?token= -- callers
// add that themselves, which is the point: it lets a test send the wrong
// one, or none at all.
func (h *hermeticServer) wtURL() string {
	return fmt.Sprintf("https://%s/wt?target=synthetic", h.addr)
}

// httpBase is the plain https:// origin /api/input and /api/events live
// under, matching what httpBase() in main.go derives from a /wt URL.
func (h *hermeticServer) httpBase() string {
	return fmt.Sprintf("https://%s", h.addr)
}

func (h *hermeticServer) stop() {
	_ = h.httpSrv.Close()
	if h.wtSrv != nil {
		_ = h.wtSrv.Close()
	}
	h.mgr.Shutdown()
}

// requireFFmpeg skips a test that opens a /wt session. That starts
// target.Synthetic, which runs ffmpeg, and without it the server answers
// the dial with 502.
func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH; target.Synthetic needs it to stream")
	}
}

// startHermeticServer wires up a synthetic-only server, with auth on iff
// token != "". Both listeners share one address, the same way main.go's
// *http.Server and *webtransport.Server do -- one on TCP for HTTP, one on
// UDP for QUIC -- both bound up front by bindLoopbackPair rather than
// rebuilt from a freed port number.
func startHermeticServer(t *testing.T, token string) *hermeticServer {
	t.Helper()

	httpLn, udpConn, addr := bindLoopbackPair(t)
	// Registered immediately, before hermeticCert below can t.Fatal: both
	// listeners are live the moment bindLoopbackPair returns, and a cleanup
	// registered only after cert generation would leak them on that failure
	// path instead of closing what was already bound.
	t.Cleanup(func() { _ = httpLn.Close() })
	t.Cleanup(func() { _ = udpConn.Close() })

	cert := hermeticCert(t)

	opts := target.Options{}
	mgr := httpapi.NewManager(opts)
	mgr.Register(&target.Synthetic{})

	api := httpapi.NewServer(mgr, web.FS, nil)
	if token != "" {
		api.SetToken(token)
	}
	handler := api.Handler()

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}},
	}
	go func() {
		// Empty cert/key file names: ServeTLS falls back to TLSConfig.Certificates
		// when they're empty, per net/http's own doc comment on the method.
		_ = httpSrv.ServeTLS(httpLn, "", "")
	}()

	wtSrv := newHermeticWebTransportServer(addr, cert)
	wtSrv.H3.Handler = handler
	api.EnableWebTransport(wtSrv)

	go func() {
		_ = wtSrv.Serve(udpConn)
	}()

	h := &hermeticServer{addr: addr, token: token, mgr: mgr, httpSrv: httpSrv, wtSrv: wtSrv, httpLn: httpLn, tlsCert: cert}
	t.Cleanup(h.stop)

	waitServerUp(t, h)
	return h
}

// newHermeticWebTransportServer builds a *webtransport.Server configured the
// same way cmd/loupe/main.go's quicConfig, newWebTransportServer,
// sessionFlowControl and webTransportSettings do (see this file's own doc
// comment above for why it cannot import them directly). Shared by
// startHermeticServer and startFakeInitOnlyServer so the two test servers
// cannot drift apart -- startFakeInitOnlyServer's own copy was missing
// EnableStreamResetPartialDelivery and ConnContext until this was pulled out.
func newHermeticWebTransportServer(addr string, cert tls.Certificate) *webtransport.Server {
	wtSrv := &webtransport.Server{
		H3: &http3.Server{
			Addr: addr,
			TLSConfig: &tls.Config{
				Certificates: []tls.Certificate{cert},
				NextProtos:   []string{http3.NextProtoH3},
			},
			QUICConfig: &quic.Config{
				EnableDatagrams:                  true,
				InitialPacketSize:                1200,
				EnableStreamResetPartialDelivery: true,
			},
			// The draft-07 max-sessions setting cmd/loupe's
			// webTransportSettings() adds by hand, for the same reason: the
			// pinned webtransport-go version only advertises its own draft's
			// key under this name.
			AdditionalSettings: map[uint64]uint64{0xc671706a: 16},
			ConnContext:        httpapi.WithQUICConn,
		},
		Config: &webtransport.Config{
			MaxIncomingStreams:    64,
			MaxIncomingUniStreams: 64,
			MaxIncomingData:       16 << 20,
		},
	}
	webtransport.ConfigureHTTP3Server(wtSrv.H3)
	return wtSrv
}

// waitServerUp polls /healthz -- open even with auth on, per CLAUDE.md's
// auth section -- until the HTTPS listener answers, so a test does not race
// the goroutine that calls ServeTLS.
func waitServerUp(t *testing.T, h *hermeticServer) {
	t.Helper()
	cl := httpClient(true)
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		rsp, err := cl.Get(h.httpBase() + "/healthz")
		if err == nil {
			rsp.Body.Close()
			return
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("hermetic server never came up: %v", lastErr)
}

// bindLoopbackPair binds both listeners the server needs -- TCP for the
// HTTP/1.1+TLS side, UDP for QUIC -- and hands them straight back, rather
// than closing either and handing back just a port number for a caller to
// rebind. The same TIME_WAIT-adjacent race trap 33 describes for a much
// larger port range applies here at a smaller scale: a closed-then-reopened
// listener on an exact port can lose to another process, or another test in
// the same -race run, in the gap between the two calls. Both webtransport.
// Server and http3.Server take a pre-bound net.PacketConn via Serve, which
// is what removes the gap on the UDP side; http.Server.ServeTLS already
// takes a net.Listener directly.
//
// The UDP port is bound first, then the TCP side is bound explicitly to
// that same port number -- a single net.Listen call, not a close-and-retry
// -- because httpBase() and wtURL() below share one addr, the same way a
// real loupe process serves HTTP and WebTransport on one port number over
// two different transports.
func bindLoopbackPair(t *testing.T) (net.Listener, *net.UDPConn, string) {
	t.Helper()
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	addr := udpConn.LocalAddr().String()
	tcpLn, err := net.Listen("tcp", addr)
	if err != nil {
		udpConn.Close()
		t.Fatal(err)
	}
	return tcpLn, udpConn, addr
}

// hermeticCert is cmd/loupe/quic_test.go's selfSignedCert, duplicated here
// because that one is unexported in a different package. Loopback-only, one
// hour of validity, no CA -- good for nothing but this process's own dial.
func hermeticCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "uiprobe-hermetic-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// TestHermeticProbeDrivesTapSwipeSeqAndReplayEndToEnd is round 2's item
// 1(b): a real Manager, a real target.Synthetic session (Options{}, so
// control:false -- Synthetic accepts no input, session.go), served over an
// actual httptest-style TLS+QUIC listener on loopback. forceInput bypasses
// the init.Control gate so tap/swipe/seq/replay all run against it, which
// Session.ViewerTouch makes meaningful even on a session with no control:
// "Forwarding comes first and is not conditional on the assembler" publishes
// the gesture event regardless of what the target itself does with the
// touch.
//
// This is what proves run's own wiring -- not just the pure helpers probe.go
// already covers -- reaches a real server: the guard, the dial, the ?token=
// and Bearer headers, and the checks together, with no live device or
// wtprobe subprocess involved.
//
// tap and swipe are SKIP here, not PASS: target.Synthetic's Touch always
// returns ErrNoControl (round 2's item 2), so the event ViewerTouch
// publishes carries that Err, matchesGesture excludes it, and checkTap/
// checkSwipe fall through to rejectedGesture -- an accurate SKIP naming the
// rejection, not a PASS for input that never reached anything. seq and
// replay still see those events (Err set or not, they are still real
// events with real seqs), so those two stay PASS.
func TestHermeticProbeDrivesTapSwipeSeqAndReplayEndToEnd(t *testing.T) {
	requireFFmpeg(t)
	t.Parallel()
	const token = "hermetic-test-token"
	h := startHermeticServer(t, token)

	// h.wtURL() deliberately carries no ?token= of its own: probeOptions.token
	// below must be what run's own query-attaching code turns into the
	// credential the server checks, or this test would pass by coincidence
	// off a URL built directly by the test rather than by the code under
	// test.
	results, err := runWithOptions(h.wtURL(), probeOptions{
		token:      token,
		insecure:   true,
		timeout:    15 * time.Second,
		forceInput: true,
	})
	if err != nil {
		t.Fatalf("runWithOptions: %v", err)
	}

	byName := make(map[string]checkResult, len(results))
	for _, r := range results {
		byName[r.name] = r
	}
	for _, name := range []string{"join", "health", "seq", "replay"} {
		r, ok := byName[name]
		if !ok {
			t.Errorf("check %q did not run at all; got %v", name, results)
			continue
		}
		if r.status != "PASS" {
			t.Errorf("check %q = %s (%s), want PASS", name, r.status, r.detail)
		}
	}
	for _, name := range []string{"tap", "swipe"} {
		r, ok := byName[name]
		if !ok {
			t.Errorf("check %q did not run at all; got %v", name, results)
			continue
		}
		if r.status != "SKIP" {
			t.Errorf("check %q = %s (%s), want SKIP (synthetic rejects every touch with ErrNoControl)", name, r.status, r.detail)
		}
		const wantErr = "target: this target does not accept input"
		if !strings.Contains(r.detail, wantErr) {
			t.Errorf("check %q detail = %q, want it to name the rejection %q", name, r.detail, wantErr)
		}
	}
	if anyFail(results) {
		t.Errorf("anyFail(results) = true, want false: %v", results)
	}
}

// TestHermeticProbeRequiresTokenOnTheWTURL is the other half of 1(b): with
// auth on, a /wt CONNECT carrying no ?token= must fail as check "auth",
// which is only reachable if the real server is actually enforcing the token
// it was given -- proving the query parameter, not just the Bearer header
// elsewhere, is what run sends.
func TestHermeticProbeRequiresTokenOnTheWTURL(t *testing.T) {
	t.Parallel()
	const token = "hermetic-test-token"
	h := startHermeticServer(t, token)

	results, err := runWithOptions(h.wtURL(), probeOptions{ // no token in probeOptions -> no ?token=
		insecure:   true,
		timeout:    10 * time.Second,
		forceInput: true,
	})
	if err != nil {
		t.Fatalf("runWithOptions: %v", err)
	}
	if len(results) != 1 || results[0].status != "FAIL" || results[0].name != "auth" {
		t.Fatalf("results = %v, want a single FAIL auth", results)
	}
}

// TestHermeticProbeRejectsWrongToken is the wrong-token half of the auth
// coverage: run applies one token to both the /wt query string and every
// HTTP call's Bearer header (runWithOptions sets q.Set("token", token) from
// the same opts.token that checkAgentButton and checkReplay put in the
// Authorization header), so a wrong token fails at the earliest point that
// needs it -- the CONNECT itself -- exactly like an empty one does in
// TestHermeticProbeRequiresTokenOnTheWTURL. Together the two pin that a
// missing credential and a wrong one are treated the same way, rather than a
// wrong-but-present token slipping through some laxer check.
func TestHermeticProbeRejectsWrongToken(t *testing.T) {
	t.Parallel()
	const token = "hermetic-test-token"
	h := startHermeticServer(t, token)

	results, err := runWithOptions(h.wtURL(), probeOptions{
		token:      "wrong-token",
		insecure:   true,
		timeout:    10 * time.Second,
		forceInput: true,
	})
	if err != nil {
		t.Fatalf("runWithOptions: %v", err)
	}
	if len(results) != 1 || results[0].status != "FAIL" || results[0].name != "auth" {
		t.Fatalf("results = %v, want a single FAIL auth", results)
	}
}

// TestServerRejectsAPICallsWithNoAuthorizationHeader proves the server side
// of the contract: GET /api/events with no Authorization header at all gets
// a 401. It does not run any of uiprobe's own code (round 3's item 5 -- the
// original name, TestHermeticProbeSendsBearerHeaderOnReplay, claimed
// otherwise), so it cannot by itself catch checkReplay dropping the header.
// TestCheckReplaySendsBearerHeader and TestCheckReplayRejectsWrongToken
// below are what call checkReplay itself; this test is what establishes the
// server enforces the header those two rely on.
func TestServerRejectsAPICallsWithNoAuthorizationHeader(t *testing.T) {
	t.Parallel()
	const token = "hermetic-test-token"
	h := startHermeticServer(t, token)

	req, err := http.NewRequest(http.MethodGet, h.httpBase()+"/api/events?target=synthetic&after=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately no Authorization header.
	rsp, err := httpClient(true).Do(req)
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	defer rsp.Body.Close()
	if rsp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /api/events with no Authorization header returned %d, want 401", rsp.StatusCode)
	}
}

// bareProbeRun builds a probeRun with no live control stream -- baseURL,
// targetID, token and insecure only -- for testing checkAgentButton and
// checkReplay in isolation. Both send exactly one HTTP request and only
// fall through to p.c.waitEvent (which needs a real collector) on a 200,
// which target.Synthetic's lack of an Actuator makes checkAgentButton never
// reach; checkReplay never touches p.c at all. That is what makes a bare
// struct enough here, rather than a full WT dial through runWithOptions.
func bareProbeRun(h *hermeticServer, token string) *probeRun {
	return &probeRun{baseURL: h.httpBase(), targetID: "synthetic", token: token, insecure: true}
}

// fakeCollector builds a collector with init and stats already delivered and
// no reader goroutine ever started, so checks() can be driven directly
// against a fabricated event history instead of a real session's own
// (always strictly increasing) seqs.
func fakeCollector(init session.InitPayload, events []session.Event) *collector {
	c := newCollector(nil)
	c.init, c.initOK = &init, true
	close(c.initCh)
	c.sawStatsHealth = true
	close(c.statsCh)
	c.events = events
	return c
}

// findResult returns the named result from checks()'s slice, or ok=false if
// that check never ran at all.
func findResult(results []checkResult, name string) (checkResult, bool) {
	for _, r := range results {
		if r.name == name {
			return r, true
		}
	}
	return checkResult{}, false
}

// TestCheckAgentButtonSendsBearerHeader is round 3's item 5: it calls
// checkAgentButton directly, uiprobe's own code, against a real server with
// auth on. Synthetic has no target.Actuator, so a correctly authorized
// POST /api/input reaches internal/httpapi/agent.go's actuator check and
// gets 501 -- the response that proves the request was accepted as
// authorized and rejected for an unrelated reason. If the Authorization
// header were dropped, the server would answer 401 instead, which is what
// distinguishes this from a header-dropped mutation: asserting only
// "status != 200" would not.
func TestCheckAgentButtonSendsBearerHeader(t *testing.T) {
	t.Parallel()
	const token = "hermetic-test-token"
	h := startHermeticServer(t, token)
	p := bareProbeRun(h, token)

	r := p.checkAgentButton()
	if r.status != "FAIL" || !strings.Contains(r.detail, "501") {
		t.Fatalf("checkAgentButton() = %s (%s), want FAIL naming 501 (synthetic has no Actuator; a 401 here would mean the Bearer header was not sent)", r.status, r.detail)
	}
}

// TestCheckAgentButtonRejectsWrongToken is the wrong-token half: a bad
// token must reach the server as a bad token, not as a missing one, and
// checkAgentButton must report that distinctly (401, not 501).
func TestCheckAgentButtonRejectsWrongToken(t *testing.T) {
	t.Parallel()
	const token = "hermetic-test-token"
	h := startHermeticServer(t, token)
	p := bareProbeRun(h, "wrong-token")

	r := p.checkAgentButton()
	if r.status != "FAIL" || !strings.Contains(r.detail, "401") {
		t.Fatalf("checkAgentButton() with wrong token = %s (%s), want FAIL naming 401", r.status, r.detail)
	}
}

// TestAgentButtonControlGateSkipsOnSynthetic is round 3's item 5's other
// half: checks()'s own gate around check 5 (agentMode && init.Control) must
// stay in front of checkAgentButton, not just checkAgentButton's own
// behaviour once called. Driven through runWithOptions/checks() end to end
// -- forceInput deliberately left false, since the gate this test protects
// is exactly the one forceInput exists to bypass for checks 2/3, and this
// test needs it intact for check 5. A mutation removing the gate would call
// checkAgentButton against synthetic's control:false session and get
// TestCheckAgentButtonSendsBearerHeader's FAIL(501) instead of this SKIP.
func TestAgentButtonControlGateSkipsOnSynthetic(t *testing.T) {
	requireFFmpeg(t)
	t.Parallel()
	const token = "hermetic-test-token"
	h := startHermeticServer(t, token)

	results, err := runWithOptions(h.wtURL(), probeOptions{
		agentMode: true,
		token:     token,
		insecure:  true,
		timeout:   15 * time.Second,
	})
	if err != nil {
		t.Fatalf("runWithOptions: %v", err)
	}
	for _, r := range results {
		if r.name == "agent-button" {
			if r.status != "SKIP" {
				t.Errorf("agent-button = %s (%s), want SKIP (synthetic reports control:false)", r.status, r.detail)
			}
			return
		}
	}
	t.Fatalf("check %q did not run at all; got %v", "agent-button", results)
}

// TestCheckReplaySendsBearerHeader and TestCheckReplayRejectsWrongToken are
// checkReplay's own pair, the same shape as the agent-button tests above.
// checkReplay never calls p.c, so bareProbeRun is enough; an empty seen
// slice makes a correctly authorized call PASS outright (nothing to be
// missing from the page).
func TestCheckReplaySendsBearerHeader(t *testing.T) {
	t.Parallel()
	const token = "hermetic-test-token"
	h := startHermeticServer(t, token)
	p := bareProbeRun(h, token)

	r := p.checkReplay(context.Background(), nil)
	if r.status != "PASS" {
		t.Fatalf("checkReplay() = %s (%s), want PASS (a 401 here would mean the Bearer header was not sent)", r.status, r.detail)
	}
}

func TestCheckReplayRejectsWrongToken(t *testing.T) {
	t.Parallel()
	const token = "hermetic-test-token"
	h := startHermeticServer(t, token)
	p := bareProbeRun(h, "wrong-token")

	r := p.checkReplay(context.Background(), nil)
	if r.status != "FAIL" || !strings.Contains(r.detail, "401") {
		t.Fatalf("checkReplay() with wrong token = %s (%s), want FAIL naming 401", r.status, r.detail)
	}
}

// TestCheckReplayFailsWhenASeenSeqIsMissingFromThePage is round 3's item 4:
// it feeds checkReplay a seen list containing a seq the real /api/events
// page cannot possibly have -- nothing on this fresh session has produced
// billions of events -- through a real server, so missingFromPage's result
// actually decides the outcome rather than a mutation that always returns
// PASS or that ignores len(missing) > 0.
func TestCheckReplayFailsWhenASeenSeqIsMissingFromThePage(t *testing.T) {
	t.Parallel()
	const token = "hermetic-test-token"
	h := startHermeticServer(t, token)
	p := bareProbeRun(h, token)

	r := p.checkReplay(context.Background(), []uint64{999999999})
	if r.status != "FAIL" {
		t.Fatalf("checkReplay() with a phantom seen seq = %s (%s), want FAIL", r.status, r.detail)
	}
	if !strings.Contains(r.detail, "999999999") {
		t.Fatalf("checkReplay() detail = %q, want it to name the missing seq", r.detail)
	}
}

// TestSeqCheckFailsOnOutOfOrderAndSkipsReplay is round 3's item 4's other
// half, for the seq branch itself: a real session's own seqs are always
// strictly increasing, so this drives seqCheck -- the exact function
// checks() calls -- with a fabricated out-of-order slice, the only way to
// reach its FAIL branch at all.
func TestSeqCheckFailsOnOutOfOrderAndSkipsReplay(t *testing.T) {
	t.Parallel()
	result, runReplay := seqCheck([]uint64{1, 3, 2})
	if result.status != "FAIL" {
		t.Errorf("seqCheck(out of order) status = %s, want FAIL", result.status)
	}
	if runReplay {
		t.Errorf("seqCheck(out of order) runReplay = true, want false (replay has nothing trustworthy to check against)")
	}
}

// TestChecksSkipsReplayWhenSeqCheckFails drives checks() itself, not just
// seqCheck in isolation -- round 4 flagged this gap directly: mutation M16
// ran checkReplay even after seqResult was a FAIL, against a real server, so
// nothing caught it because seqCheck's own table test above never reaches
// checks()'s switch. The collector is fabricated (fakeCollector) with an
// out-of-order event history, which a real session's own seqs can never
// produce, so this is the only way to drive the FAIL branch through checks()
// itself.
func TestChecksSkipsReplayWhenSeqCheckFails(t *testing.T) {
	t.Parallel()
	const token = "hermetic-test-token"
	h := startHermeticServer(t, token)
	p := bareProbeRun(h, token)
	p.checkTimeout = checkTimeout
	p.healthTimeout = checkTimeout
	p.c = fakeCollector(session.InitPayload{Control: false}, []session.Event{{Seq: 2}, {Seq: 1}})

	replay, ok := findResult(p.checks(context.Background()), "replay")
	if !ok {
		t.Fatal("no replay result in checks() output")
	}
	if replay.status != "SKIP" || replay.detail != "seq check failed" {
		t.Errorf("replay = %s (%q), want SKIP (\"seq check failed\")", replay.status, replay.detail)
	}
}

// TestChecksSkipsReplayOnEmptySeqs is M15b: checks() must report SKIP
// "no events observed" when no events were seen at all, rather than running
// checkReplay against an empty seen list -- which would PASS trivially
// against a real server (nothing is missing from an empty want), a
// different status/detail than the SKIP this pins, so the two are not
// interchangeable even though both are "nothing failed."
func TestChecksSkipsReplayOnEmptySeqs(t *testing.T) {
	t.Parallel()
	const token = "hermetic-test-token"
	h := startHermeticServer(t, token)
	p := bareProbeRun(h, token)
	p.checkTimeout = checkTimeout
	p.healthTimeout = checkTimeout
	p.c = fakeCollector(session.InitPayload{Control: false}, nil)

	replay, ok := findResult(p.checks(context.Background()), "replay")
	if !ok {
		t.Fatal("no replay result in checks() output")
	}
	if replay.status != "SKIP" || replay.detail != "no events observed" {
		t.Errorf("replay = %s (%q), want SKIP (\"no events observed\")", replay.status, replay.detail)
	}
}

// TestHealthFailsWithinTimeoutWhenNoStatsArrive is item 1(c): a fake server
// that completes the WT handshake and answers MsgHello with MsgInit, but
// never sends a MsgStats. Health must FAIL, and must do so within
// healthTimeout rather than run's own overall deadline -- proven by shrinking
// healthTimeout to well under checkTimeout and asserting the call returns
// close to that shrunk bound.
func TestHealthFailsWithinTimeoutWhenNoStatsArrive(t *testing.T) {
	t.Parallel()
	h := startFakeInitOnlyServer(t)

	const shrunk = 300 * time.Millisecond
	start := time.Now()
	results, err := runWithOptions(h.wtURL(), probeOptions{
		insecure: true,
		timeout:  10 * time.Second,
		// forceInput left false: the fake's InitPayload reports
		// control:false, same as synthetic, so tap/swipe SKIP immediately
		// instead of spending 2*checkTimeout waiting on gesture events this
		// fake has no session behind it to ever produce.
		healthTimeout: shrunk,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("runWithOptions: %v", err)
	}

	var health checkResult
	found := false
	for _, r := range results {
		if r.name == "health" {
			health, found = r, true
		}
	}
	if !found {
		t.Fatalf("check %q did not run at all; got %v", "health", results)
	}
	if health.status != "FAIL" {
		t.Errorf("health = %s (%s), want FAIL", health.status, health.detail)
	}
	// Generous slack for scheduling, but this must not be anywhere near
	// run's own 10s overall timeout or the "within the timeout" half of this
	// test's name is untested.
	if elapsed > 3*time.Second {
		t.Errorf("health check took %v, want close to healthTimeout=%v (a mutation dropping healthTimeout and falling back to run's overall deadline would pass every other assertion here and only show up as a suite that got seconds slower)", elapsed, shrunk)
	}
}

// TestRunItselfForwardsTheTokenAndLeavesTestOnlyKnobsAtTheirDefaults is
// round 3's item 3: every test above this point drives runWithOptions, which
// is what hermetic_test.go needs to reach the test-only knobs (forceInput,
// healthTimeout, skipDrain) -- but nothing proved run itself, main's actual
// entry point, wires probeOptions the same way. A mutation that flipped
// run's forceInput or skipDrain to true, or that stopped passing token
// through to runWithOptions, survived review because of exactly that gap.
//
// This calls run, not runWithOptions, and checks all three at once against
// one real session: token must authorize it (join PASS, not a guard/auth
// FAIL), tap/swipe must SKIP (forceInput false -- synthetic's control:false
// session, undisturbed), and health must PASS within run's own default
// healthTimeout (skipDrain false, so the video streams get drained and the
// writer survives to send a MsgStats).
func TestRunItselfForwardsTheTokenAndLeavesTestOnlyKnobsAtTheirDefaults(t *testing.T) {
	requireFFmpeg(t)
	t.Parallel()
	const token = "hermetic-test-token"
	h := startHermeticServer(t, token)

	results, err := run(h.wtURL(), false, false, token, true, 10*time.Second)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	byName := make(map[string]checkResult, len(results))
	for _, r := range results {
		byName[r.name] = r
	}
	if r, ok := byName["join"]; !ok || r.status != "PASS" {
		t.Errorf("join = %v, want PASS (token not forwarded from run to runWithOptions?)", r)
	}
	// Both forceInput=false (the init.Control gate) and forceInput=true
	// (synthetic accepting the dial and then rejecting the gesture) produce
	// a SKIP status for tap/swipe, so status alone cannot tell them apart --
	// that gap is exactly how a forceInput-forced-true mutation survived
	// once. The detail text is what distinguishes them: only the gate path
	// says "init.control=false" before ever sending anything.
	for _, name := range []string{"tap", "swipe"} {
		r, ok := byName[name]
		if !ok || r.status != "SKIP" {
			t.Errorf("%s = %v, want SKIP", name, r)
			continue
		}
		if !strings.Contains(r.detail, "init.control=false") {
			t.Errorf("%s detail = %q, want it to name the init.control=false gate (run's forceInput must default to false, not bypass the gate and get a different SKIP from a rejected gesture)", name, r.detail)
		}
	}
	if r, ok := byName["health"]; !ok || r.status != "PASS" {
		t.Errorf("health = %v, want PASS (run's skipDrain must default to false)", r)
	}
}

// TestRunFallsBackToLOUPETokenEnvVar is round 3's item 6: with no -token
// flag, run must still authorize off LOUPE_TOKEN, matching main's own doc
// comment ("Auth: -token, or LOUPE_TOKEN from the environment"). Driven
// through run itself, not runWithOptions, since runWithOptions's caller in
// this case is the thing whose env-var fallback is under test.
func TestRunFallsBackToLOUPETokenEnvVar(t *testing.T) {
	requireFFmpeg(t)
	const token = "hermetic-test-token"
	h := startHermeticServer(t, token)

	t.Setenv("LOUPE_TOKEN", token)
	results, err := run(h.wtURL(), false, false, "", true, 10*time.Second)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, r := range results {
		if r.name == "join" {
			if r.status != "PASS" {
				t.Errorf("join = %s (%s), want PASS (LOUPE_TOKEN fallback not applied?)", r.status, r.detail)
			}
			return
		}
	}
	t.Fatalf("check %q did not run at all; got %v", "join", results)
}

// TestRunUsesTheTwoSecondDefaultHealthTimeoutNotTwentySeconds is round 3's
// item 6's other half. TestCheckTimeoutIsTwoSeconds in probe_test.go pins
// the constant; this pins that run's own code path actually uses it as
// check 4's default, rather than falling back to something else (run's own
// -timeout flag defaults to 20s, which is what a mutation swapping the two
// would produce and which TestCheckTimeoutIsTwoSeconds alone cannot catch).
// Against a fake server that never sends MsgStats, health must FAIL well
// under run's own 10s overall deadline passed here.
func TestRunUsesTheTwoSecondDefaultHealthTimeoutNotTwentySeconds(t *testing.T) {
	t.Parallel()
	h := startFakeInitOnlyServer(t)

	start := time.Now()
	results, err := run(h.wtURL(), false, true, "", true, 10*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, r := range results {
		if r.name == "health" {
			if r.status != "FAIL" {
				t.Errorf("health = %s (%s), want FAIL", r.status, r.detail)
			}
			if elapsed > 5*time.Second {
				t.Errorf("health check took %v, want close to the 2s default (a mutation to 20s would pass the FAIL assertion and only show up as a suite that got seconds slower)", elapsed)
			}
			return
		}
	}
	t.Fatalf("check %q did not run at all; got %v", "health", results)
}

// TestControlFalseYieldsSkip is item 1(d): the default synthetic session
// (forceInput left false, matching what run itself always passes) reports
// init.Control=false, and checks 2/3 must be SKIP rather than PASS, FAIL, or
// silently absent.
func TestControlFalseYieldsSkip(t *testing.T) {
	requireFFmpeg(t)
	t.Parallel()
	const token = "hermetic-test-token"
	h := startHermeticServer(t, token)

	results, err := runWithOptions(h.wtURL(), probeOptions{
		token:    token,
		insecure: true,
		timeout:  15 * time.Second,
		// forceInput left false: this is what run itself always passes.
	})
	if err != nil {
		t.Fatalf("runWithOptions: %v", err)
	}

	byName := make(map[string]checkResult, len(results))
	for _, r := range results {
		byName[r.name] = r
	}
	for _, name := range []string{"tap", "swipe"} {
		r, ok := byName[name]
		if !ok {
			t.Fatalf("check %q did not run at all; got %v", name, results)
		}
		if r.status != "SKIP" {
			t.Errorf("check %q = %s (%s), want SKIP (synthetic reports control:false)", name, r.status, r.detail)
		}
	}
	if anyFail(results) {
		t.Errorf("anyFail(results) = true, want false: %v", results)
	}
}

// TestRunRefusesADisallowedTargetWithoutDialing is round 2's item 1(a)'s
// second half: TestGuardDecision in probe_test.go covers guardDecision as a
// pure function, but that proves nothing about whether run/runWithOptions
// actually calls it. This drives runWithOptions itself, end to end, against a
// target the guard must refuse -- with the URL pointed at a TEST-NET-1
// address (192.0.2.1, RFC 5737: guaranteed unroutable, so a real dial has
// nothing to succeed or fail against quickly) and a short overall timeout, so
// a mutation that skips the guard call surfaces as this test failing on a
// context-deadline dial error instead of the single FAIL "guard" result the
// wired-in check should short-circuit to before any network I/O happens.
func TestRunRefusesADisallowedTargetWithoutDialing(t *testing.T) {
	t.Parallel()
	results, err := runWithOptions("https://192.0.2.1:1/wt?target=ACME00000000001", probeOptions{
		insecure: true,
		timeout:  1 * time.Second,
	})
	if err != nil {
		t.Fatalf("runWithOptions returned an error instead of a guard result (guard not reached before dialing?): %v", err)
	}
	if len(results) != 1 || results[0].status != "FAIL" || results[0].name != "guard" {
		t.Fatalf("results = %v, want a single FAIL guard", results)
	}
}

// TestHealthFailsWithoutDrainingVideoStreams is round 2's item 3 (and round
// 3's item 1, which caught this test passing with the drain left ON): it
// proves the comment at drainVideoUniStreams's call site in run is actually
// true of the code, not just asserted in prose, and that draining is what
// makes the difference rather than health failing either way.
//
// MsgStats goes out once a second (internal/httpapi/viewer.go's stats
// ticker), so a healthTimeout has to be at least that long for a PASS to be
// possible at all -- round 2's version used 500ms, under which health FAILs
// regardless of the drain and the test would still be green with skipDrain
// forced true by a mutation. It is checkTimeout (2s) here, the package's own
// documented floor for this kind of wait: measured, drain on PASSes around
// 1.0s and drain off FAILs around 3.0s, since 10 access units land in the
// first 150ms or so at 60fps and MaxIncomingUniStreams is 64 but each stream
// is refused, not queued, once that credit runs out.
//
// Two cases in one table: drain on must PASS, drain off must FAIL. A single
// skipDrain:true case cannot tell "health correctly needs the drain" apart
// from "health always fails at this timeout", which is exactly how the
// original test passed for the wrong reason. Only health is exercised --
// forceInput is left false, since check 4 needs nothing from checks 2/3.
func TestHealthFailsWithoutDrainingVideoStreams(t *testing.T) {
	requireFFmpeg(t)
	t.Parallel()
	cases := []struct {
		name      string
		skipDrain bool
		want      string
	}{
		{"drain on", false, "PASS"},
		{"drain off", true, "FAIL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const token = "hermetic-test-token"
			h := startHermeticServer(t, token)

			start := time.Now()
			results, err := runWithOptions(h.wtURL(), probeOptions{
				token:         token,
				insecure:      true,
				timeout:       10 * time.Second,
				healthTimeout: checkTimeout,
				skipDrain:     tc.skipDrain,
			})
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("runWithOptions: %v", err)
			}

			var health checkResult
			found := false
			for _, r := range results {
				if r.name == "health" {
					health, found = r, true
				}
			}
			if !found {
				t.Fatalf("check %q did not run at all; got %v", "health", results)
			}
			if health.status != tc.want {
				t.Errorf("skipDrain=%v: health = %s (%s) after %v, want %s", tc.skipDrain, health.status, health.detail, elapsed, tc.want)
			}
		})
	}
}

// fakeInitServer is a bare WebTransport listener that speaks just enough of
// the control protocol to answer MsgHello with a MsgInit -- no Manager, no
// session, no video, no MsgStats ever. It exists only for
// TestHealthFailsWithinTimeoutWhenNoStatsArrive: a real Session always sends
// periodic stats once a viewer is attached, so proving check 4's timeout
// path needs a server that structurally cannot.
type fakeInitServer struct {
	addr  string
	wtSrv *webtransport.Server
}

func (f *fakeInitServer) wtURL() string {
	return fmt.Sprintf("https://%s/wt?target=synthetic", f.addr)
}

func startFakeInitOnlyServer(t *testing.T) *fakeInitServer {
	t.Helper()
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	// Registered immediately, before hermeticCert below can t.Fatal -- see
	// the identical comment in startHermeticServer.
	t.Cleanup(func() { _ = udpConn.Close() })
	addr := udpConn.LocalAddr().String()
	cert := hermeticCert(t)

	mux := http.NewServeMux()
	wtSrv := newHermeticWebTransportServer(addr, cert)
	t.Cleanup(func() { _ = wtSrv.Close() })

	mux.HandleFunc("/wt", func(w http.ResponseWriter, r *http.Request) {
		sess, err := wtSrv.Upgrade(w, r)
		if err != nil {
			return
		}
		st, err := sess.AcceptStream(r.Context())
		if err != nil {
			return
		}
		defer st.Close()
		// Read and discard MsgHello, then send MsgInit and nothing else --
		// no MsgStats, ever, which is the entire point of this fake.
		buf := make([]byte, 4)
		if _, err := io.ReadFull(st, buf); err != nil {
			return
		}
		n := int(buf[0])<<24 | int(buf[1])<<16 | int(buf[2])<<8 | int(buf[3])
		hello := make([]byte, n)
		if _, err := io.ReadFull(st, hello); err != nil {
			return
		}
		initBody, _ := json.Marshal(session.InitPayload{
			Codec: "avc1.42C02A", CodedWidth: 720, CodedHeight: 1560,
			Kind: "synthetic", Control: false,
		})
		frame := append([]byte{session.MsgInit}, initBody...)
		st.Write(frameControl(frame))
		<-r.Context().Done()
	})
	wtSrv.H3.Handler = mux

	go func() { _ = wtSrv.Serve(udpConn) }()

	f := &fakeInitServer{addr: addr, wtSrv: wtSrv}

	waitFakeServerUp(t, f)
	return f
}

// waitFakeServerUp polls the /wt endpoint's underlying QUIC listener the same
// way waitServerUp does for the full hermetic server, by dialing until the
// WT handshake itself succeeds (there is no /healthz on this fake).
func waitFakeServerUp(t *testing.T, f *fakeInitServer) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		qconn, err := quic.DialAddr(ctx, f.addr,
			&tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h3"}},
			&quic.Config{EnableDatagrams: true, InitialPacketSize: 1200},
		)
		cancel()
		if err == nil {
			_ = qconn.CloseWithError(0, "probe")
			return
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fake server never came up: %v", lastErr)
}
