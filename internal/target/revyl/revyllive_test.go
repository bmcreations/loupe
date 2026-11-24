package revyl

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/bmcreations/loupe/internal/target"
)

// testWSToken stands in for the token Revyl puts in worker_ws_url.
const testWSToken = "wstok-test-0001"

// fakeWorker stands in for Revyl's worker-connection endpoint and the
// worker websocket it points at, as seen on a live account on 2026-11-19:
// the socket opens with a CONNECTION frame and a DEVICE_INIT_STATUS frame,
// answers each frame with ACTION_RECEIVED, and answers MANUAL_TOUCH_UP with
// ACTION_ACK. It is served through a fakeProxy, at the same base URL.
type fakeWorker struct {
	t *testing.T

	mu sync.Mutex
	// status, when set, is worker-connection's answer instead of a socket.
	status int
	// refuse refuses the websocket upgrade.
	refuse bool
	// closeAfter closes the socket once it has had this many frames; 0
	// never does.
	closeAfter int
	// notReady says manual_input_ready: false.
	notReady bool
	// devW and devH are the device_width and device_height
	// worker-connection reports, left out when 0.
	devW, devH int

	gets, upgrades, conns int
	origin, auth          string
	frames                []map[string]any
	conn                  *websocket.Conn // the newest socket, for push

	closed chan struct{} // closed once closeAfter closed a socket
	ended  chan struct{} // one value per socket that stopped reading
}

func newFakeWorker(t *testing.T, p *fakeProxy) *fakeWorker {
	t.Helper()
	w := &fakeWorker{t: t, closed: make(chan struct{}), ended: make(chan struct{}, 16), devW: 1080, devH: 2400}
	p.mu.Lock()
	p.live = w
	p.mu.Unlock()
	return w
}

func (w *fakeWorker) set(f func(w *fakeWorker)) {
	w.mu.Lock()
	f(w)
	w.mu.Unlock()
}

func (w *fakeWorker) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/v1/execution/streaming/worker-connection/"+testRunID:
		w.mu.Lock()
		w.gets++
		w.auth = r.Header.Get("Authorization")
		status, devW, devH := w.status, w.devW, w.devH
		w.mu.Unlock()
		if r.Method != http.MethodGet {
			http.Error(rw, "method", http.StatusMethodNotAllowed)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		if status != 0 {
			rw.WriteHeader(status)
			_, _ = io.WriteString(rw, `{"detail":"no worker for this run"}`)
			return
		}
		answer := map[string]any{
			"status": "ready", "message": nil, "workflow_run_id": testRunID,
			"worker_ws_url": "ws://" + r.Host + "/ws/stream?token=" + testWSToken,
		}
		if devW > 0 {
			answer["device_width"], answer["device_height"] = devW, devH
		}
		_ = json.NewEncoder(rw).Encode(answer)
	case r.URL.Path == "/ws/stream":
		w.mu.Lock()
		w.upgrades++
		w.origin = r.Header.Get("Origin")
		refuse := w.refuse
		w.mu.Unlock()
		if refuse || r.URL.Query().Get("token") != testWSToken {
			http.Error(rw, "forbidden", http.StatusForbidden)
			return
		}
		c, err := websocket.Accept(rw, r, &websocket.AcceptOptions{OriginPatterns: []string{"app.revyl.ai"}})
		if err != nil {
			return
		}
		w.serveSocket(c)
	default:
		http.NotFound(rw, r)
	}
}

func (w *fakeWorker) serveSocket(c *websocket.Conn) {
	defer func() {
		select {
		case w.ended <- struct{}{}:
		default:
		}
	}()
	defer c.CloseNow()
	ctx := context.Background()
	w.mu.Lock()
	w.conns++
	w.conn = c
	ready, devW, devH := !w.notReady, w.devW, w.devH
	w.mu.Unlock()
	send := func(v any) {
		b, _ := json.Marshal(v)
		_ = c.Write(ctx, websocket.MessageText, b)
	}
	send(map[string]any{"event_type": "CONNECTION", "status": "established", "workflow_run_id": testRunID})
	send(map[string]any{"event_type": "DEVICE_INIT_STATUS", "status": "initialized", "workflow_run_id": testRunID,
		"manual_input_ready": ready, "execution_ready": true, "device_width": devW, "device_height": devH})
	// A binary frame past the library's default 32 KiB read limit: the
	// client must skip it rather than drop the socket.
	_ = c.Write(ctx, websocket.MessageBinary, make([]byte, 64<<10))
	for {
		typ, b, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var f map[string]any
		if json.Unmarshal(b, &f) != nil {
			w.t.Errorf("frame %q is not JSON", b)
			continue
		}
		w.mu.Lock()
		w.frames = append(w.frames, f)
		n, closeAfter := len(w.frames), w.closeAfter
		w.mu.Unlock()
		if closeAfter > 0 && n == closeAfter {
			_ = c.Close(websocket.StatusGoingAway, "test closed it")
			close(w.closed)
			return
		}
		action, _ := f["action"].(string)
		now := time.Now().UnixMilli()
		send(map[string]any{"event_type": "ACTION_RECEIVED", "action": strings.ToLower(strings.TrimPrefix(action, "MANUAL_")),
			"action_id": f["action_id"], "server_received_at_ms": now, "client_sent_at_ms": f["client_sent_at_ms"]})
		if action == "MANUAL_TOUCH_UP" {
			send(map[string]any{"event_type": "ACTION_ACK", "action": f["gesture"], "action_id": f["action_id"], "success": true,
				"x": f["x"], "y": f["y"], "server_completed_at_ms": now, "queue_wait_ms": 1, "device_exec_ms": 0})
		}
	}
}

// push sends a text frame on the open socket, as the worker would at any
// time.
func (w *fakeWorker) push(frame string) {
	w.t.Helper()
	w.mu.Lock()
	c := w.conn
	w.mu.Unlock()
	if c == nil {
		w.t.Fatal("no socket to push on")
	}
	if err := c.Write(context.Background(), websocket.MessageText, []byte(frame)); err != nil {
		w.t.Fatalf("push: %v", err)
	}
}

func (w *fakeWorker) counts() (gets, upgrades, conns int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.gets, w.upgrades, w.conns
}

func (w *fakeWorker) got() []map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]map[string]any(nil), w.frames...)
}

func (w *fakeWorker) until(what string, ok func() bool) {
	w.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			gets, upgrades, conns := w.counts()
			w.t.Fatalf("timed out waiting for %s (worker-connection %d, upgrades %d, sockets %d, frames %v)",
				what, gets, upgrades, conns, w.got())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitFrames waits for n frames and returns them all.
func (w *fakeWorker) waitFrames(n int) []map[string]any {
	w.t.Helper()
	w.until("frames", func() bool { return len(w.got()) >= n })
	return w.got()
}

// waitSocket waits for a socket the client can send on: the server has
// sent DEVICE_INIT_STATUS, and the client has had a moment to read it.
func (w *fakeWorker) waitSocket() {
	w.t.Helper()
	w.until("a socket", func() bool { _, _, c := w.counts(); return c >= 1 })
	time.Sleep(50 * time.Millisecond)
}

// liveFrame is a frame's action and point.
func liveFrame(f map[string]any) (action string, x, y int) {
	action, _ = f["action"].(string)
	fx, _ := f["x"].(float64)
	fy, _ := f["y"].(float64)
	return action, int(fx), int(fy)
}

func checkFrames(t *testing.T, got []map[string]any, want [][3]any) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d frames, want %d: %v", len(got), len(want), got)
	}
	for i, f := range got {
		a, x, y := liveFrame(f)
		if a != want[i][0] || x != want[i][1] || y != want[i][2] {
			t.Fatalf("frame %d = %s (%d,%d), want %s (%d,%d): %v", i, a, x, y, want[i][0], want[i][1], want[i][2], got)
		}
	}
}

func liveTouch(t *testing.T, in *revylInput, m target.Meta) func(target.TouchAction, int32, int32) {
	return func(a target.TouchAction, x, y int32) {
		t.Helper()
		var p float32 = 1
		if a == target.TouchUp {
			p = 0
		}
		if err := in.touch(target.Touch{Action: a, PointerID: 1, X: x, Y: y, Pressure: p}, m); err != nil {
			t.Fatalf("touch %d at %d,%d: %v", a, x, y, err)
		}
	}
}

// A press reaches the device as it happens: the down before the finger
// moves, each move before the lift. A tap goes the same way and is still
// where typing goes.
func TestRevylLiveTouchReachesTheDeviceBeforeTheLift(t *testing.T) {
	m := target.Meta{Width: 540, Height: 1200} // half the screen's size
	f := newFakeRevyl(t)
	p := newFakeProxy(t)
	w := newFakeWorker(t, p)
	g := restGestures(t, f, p.srv.URL, "android", 1080, 2400)
	w.waitSocket()
	touch := liveTouch(t, g.in, m)

	touch(target.TouchDown, 100, 1000)
	w.waitFrames(1)
	touch(target.TouchMove, 100, 900)
	w.waitFrames(2)
	touch(target.TouchMove, 100, 500)
	w.waitFrames(3)
	touch(target.TouchUp, 100, 500)
	w.waitFrames(4)

	touch(target.TouchDown, 270, 600)
	touch(target.TouchUp, 270, 600)
	got := w.waitFrames(6)
	checkFrames(t, got, [][3]any{
		{"MANUAL_TOUCH_DOWN", 200, 2000},
		{"MANUAL_TOUCH_MOVE", 200, 1800},
		{"MANUAL_TOUCH_MOVE", 200, 1000},
		{"MANUAL_TOUCH_UP", 200, 1000},
		{"MANUAL_TOUCH_DOWN", 540, 1200},
		{"MANUAL_TOUCH_UP", 540, 1200},
	})
	var lastID float64
	for i, fr := range got {
		id, _ := fr["action_id"].(float64)
		if fr["event_type"] != "STREAM" || id <= lastID {
			t.Fatalf("frame %d: event_type %v, action_id %v after %v", i, fr["event_type"], fr["action_id"], lastID)
		}
		lastID = id
		if ms, _ := fr["client_sent_at_ms"].(float64); ms < 1e12 {
			t.Fatalf("frame %d: client_sent_at_ms %v", i, fr["client_sent_at_ms"])
		}
	}
	for i, gesture := range map[int]string{3: "drag", 5: "tap"} {
		if got[i]["gesture"] != gesture {
			t.Fatalf("frame %d gesture %v, want %s", i, got[i]["gesture"], gesture)
		}
		if _, ok := got[i]["duration_ms"].(float64); !ok {
			t.Fatalf("frame %d has no duration_ms: %v", i, got[i])
		}
	}

	// Typing goes where the tap landed, over the device proxy.
	if err := g.in.text("hi"); err != nil {
		t.Fatal(err)
	}
	checkBodies(t, p.waitFor(1), []any{
		[2]any{"input", map[string]any{"x": float64(540), "y": float64(1200), "text": "hi", "clear_first": false}},
	})
	recs := g.log.wait(t, 3)
	for i, want := range []string{"drag", "tap"} {
		if recs[i]["via"] != "ws" || recs[i]["gesture"] != want {
			t.Fatalf("trace %d = %v, want %s via ws", i, recs[i], want)
		}
	}
	if recs[2]["via"] != "rest" {
		t.Fatalf("trace 2 = %v, want the text via rest", recs[2])
	}
	for _, rec := range recs {
		b, _ := json.Marshal(rec)
		if s := string(b); strings.Contains(s, testWSToken) || strings.Contains(s, testRevylKey) || strings.Contains(s, "ws://") {
			t.Fatalf("token, key or URL in the trace: %s", s)
		}
	}
	w.mu.Lock()
	origin, auth := w.origin, w.auth
	w.mu.Unlock()
	if origin != "https://app.revyl.ai" || auth != "Bearer "+testRevylKey {
		t.Fatalf("Origin %q, Authorization %q", origin, auth)
	}
	if c := f.calls(); len(c) != 0 {
		t.Fatalf("the CLI ran: %q", c)
	}
	if err := g.in.err(); err != nil {
		t.Fatal(err)
	}
}

// Moves that come faster than 60 a second are joined: each frame carries
// the newest point, frames are at least a frame apart, and the last one is
// where the finger lifted.
func TestRevylLiveJoinsMoves(t *testing.T) {
	m := target.Meta{Width: 540, Height: 1200}
	f := newFakeRevyl(t)
	p := newFakeProxy(t)
	w := newFakeWorker(t, p)
	g := restGestures(t, f, p.srv.URL, "android", 1080, 2400)
	w.waitSocket()
	touch := liveTouch(t, g.in, m)

	touch(target.TouchDown, 100, 1100)
	w.waitFrames(1)
	t0 := time.Now()
	const n = 100
	for i := 1; i <= n; i++ {
		touch(target.TouchMove, 100, int32(1100-5*i))
		time.Sleep(time.Millisecond)
	}
	took := time.Since(t0)
	touch(target.TouchUp, 100, 600)
	w.until("the lift", func() bool {
		fr := w.got()
		a, _, _ := liveFrame(fr[len(fr)-1])
		return a == "MANUAL_TOUCH_UP"
	})
	got := w.got()
	var moves []map[string]any
	for _, fr := range got[1 : len(got)-1] {
		if a, _, _ := liveFrame(fr); a != "MANUAL_TOUCH_MOVE" {
			t.Fatalf("frame %v between down and up", fr)
		}
		moves = append(moves, fr)
	}
	if limit := int(took/(15*time.Millisecond)) + 2; len(moves) == 0 || len(moves) > limit {
		t.Fatalf("%d moves over %v went as %d frames, want 1 to %d", n, took, len(moves), limit)
	}
	lastY, lastAt := 2201, 0.0
	for i, fr := range moves {
		_, x, y := liveFrame(fr)
		at, _ := fr["client_sent_at_ms"].(float64)
		if x != 200 || y >= lastY {
			t.Fatalf("move %d at %d,%d after y %d", i, x, y, lastY)
		}
		if i > 0 && at-lastAt < 15 {
			t.Fatalf("move %d sent %v ms after the one before", i, at-lastAt)
		}
		lastY, lastAt = y, at
	}
	if _, x, y := liveFrame(moves[len(moves)-1]); x != 200 || y != 1200 {
		t.Fatalf("last move at %d,%d, want 200,1200", x, y)
	}
	if _, x, y := liveFrame(got[len(got)-1]); x != 200 || y != 1200 {
		t.Fatalf("lift at %d,%d, want 200,1200", x, y)
	}
	if r := p.requests(); len(r) != 0 {
		t.Fatalf("device proxy got %d requests", len(r))
	}
}

// With no socket, a press goes as one finished gesture on the lift, as
// before. The socket is tried once, not again on every gesture.
func TestRevylLiveFallsBackWhenTheSocketCannotOpen(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(w *fakeWorker)
		tried func(gets, upgrades int) bool
	}{
		{"worker-connection refused", func(w *fakeWorker) { w.status = http.StatusServiceUnavailable },
			func(gets, _ int) bool { return gets >= 1 }},
		{"upgrade refused", func(w *fakeWorker) { w.refuse = true },
			func(_, upgrades int) bool { return upgrades >= 1 }},
		{"input not ready", func(w *fakeWorker) { w.notReady = true },
			func(_, upgrades int) bool { return upgrades >= 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := target.Meta{Width: 540, Height: 1200}
			f := newFakeRevyl(t)
			p := newFakeProxy(t)
			w := newFakeWorker(t, p)
			w.set(tc.setup)
			g := restGestures(t, f, p.srv.URL, "android", 1080, 2400)
			w.until("an attempt at the socket", func() bool { gets, upgrades, _ := w.counts(); return tc.tried(gets, upgrades) })
			time.Sleep(50 * time.Millisecond)
			touch := liveTouch(t, g.in, m)

			touch(target.TouchDown, 100, 1000)
			touch(target.TouchMove, 100, 900)
			touch(target.TouchMove, 100, 500)
			touch(target.TouchUp, 100, 500)
			checkBodies(t, p.waitFor(1), []any{
				[2]any{"drag", map[string]any{"start_x": float64(200), "start_y": float64(2000), "end_x": float64(200), "end_y": float64(1000)}},
			})
			if fr := w.got(); len(fr) != 0 {
				t.Fatalf("frames sent: %v", fr)
			}
			if gets, _, _ := w.counts(); gets != 1 {
				t.Fatalf("worker-connection asked %d times, want once", gets)
			}
			rec := g.log.wait(t, 1)[0]
			if rec["via"] != "rest" {
				t.Fatalf("trace %v, want via rest", rec)
			}
			b, _ := json.Marshal(rec)
			if s := string(b); strings.Contains(s, testWSToken) || strings.Contains(s, testRevylKey) || strings.Contains(s, testRunID) {
				t.Fatalf("token, key or run in the trace: %s", s)
			}
		})
	}
}

// A socket that fails mid-press leaves the rest of the press to the device
// proxy: a drag from the last point sent to where the finger lifted. A
// press that has nothing left to move is not resent, since its down may
// have landed.
func TestRevylLiveFallsBackMidGesture(t *testing.T) {
	m := target.Meta{Width: 540, Height: 1200}
	t.Run("drag", func(t *testing.T) {
		f := newFakeRevyl(t)
		p := newFakeProxy(t)
		w := newFakeWorker(t, p)
		w.set(func(w *fakeWorker) { w.closeAfter = 2 })
		g := restGestures(t, f, p.srv.URL, "android", 1080, 2400)
		w.waitSocket()
		touch := liveTouch(t, g.in, m)

		touch(target.TouchDown, 100, 1000)
		touch(target.TouchMove, 100, 900)
		<-w.closed
		touch(target.TouchMove, 100, 700)
		touch(target.TouchMove, 100, 500)
		touch(target.TouchUp, 100, 500)
		checkBodies(t, p.waitFor(1), []any{
			[2]any{"drag", map[string]any{"start_x": float64(200), "start_y": float64(1800), "end_x": float64(200), "end_y": float64(1000)}},
		})
		checkFrames(t, w.got(), [][3]any{{"MANUAL_TOUCH_DOWN", 200, 2000}, {"MANUAL_TOUCH_MOVE", 200, 1800}})
		rec := g.log.wait(t, 1)[0]
		if rec["via"] != "rest" || rec["gesture"] != "drag" {
			t.Fatalf("trace %v, want the rest as a drag via rest", rec)
		}
	})
	// The worker's size and the session's differ: the frames are in the
	// worker's units, and the rest of the drag in the session's.
	t.Run("drag in two sizes", func(t *testing.T) {
		f := newFakeRevyl(t)
		p := newFakeProxy(t)
		w := newFakeWorker(t, p)
		w.set(func(w *fakeWorker) { w.closeAfter, w.devW, w.devH = 2, 720, 1600 })
		g := restGestures(t, f, p.srv.URL, "android", 1080, 2400)
		w.waitSocket()
		touch := liveTouch(t, g.in, m)

		touch(target.TouchDown, 100, 1000)
		touch(target.TouchMove, 100, 900)
		<-w.closed
		touch(target.TouchMove, 100, 700)
		touch(target.TouchMove, 100, 500)
		touch(target.TouchUp, 100, 500)
		checkBodies(t, p.waitFor(1), []any{
			[2]any{"drag", map[string]any{"start_x": float64(200), "start_y": float64(1800), "end_x": float64(200), "end_y": float64(1000)}},
		})
		checkFrames(t, w.got(), [][3]any{{"MANUAL_TOUCH_DOWN", 133, 1333}, {"MANUAL_TOUCH_MOVE", 133, 1200}})
	})
	t.Run("tap", func(t *testing.T) {
		f := newFakeRevyl(t)
		p := newFakeProxy(t)
		w := newFakeWorker(t, p)
		w.set(func(w *fakeWorker) { w.closeAfter = 1 })
		g := restGestures(t, f, p.srv.URL, "android", 1080, 2400)
		w.waitSocket()
		touch := liveTouch(t, g.in, m)

		touch(target.TouchDown, 270, 600)
		<-w.closed
		touch(target.TouchUp, 270, 600)
		rec := g.log.wait(t, 1)[0]
		if rec["via"] != "ws" || rec["error"] == nil {
			t.Fatalf("trace %v, want the failed tap via ws with an error", rec)
		}
		// A later tap goes over the proxy, and only that one.
		touch(target.TouchDown, 300, 700)
		touch(target.TouchUp, 300, 700)
		checkBodies(t, p.waitFor(1), []any{[2]any{"tap", map[string]any{"x": float64(600), "y": float64(1400)}}})
		time.Sleep(50 * time.Millisecond)
		if r := p.requests(); len(r) != 1 {
			t.Fatalf("device proxy got %d requests, want the later tap only", len(r))
		}
	})
}

// An agent's gesture waits for a press in progress to lift, so the two
// never interleave on the device.
func TestRevylLiveKeepsAgentInputOutOfAPress(t *testing.T) {
	m := target.Meta{Width: 540, Height: 1200}
	f := newFakeRevyl(t)
	p := newFakeProxy(t)
	w := newFakeWorker(t, p)
	g := restGestures(t, f, p.srv.URL, "android", 1080, 2400)
	w.waitSocket()
	touch := liveTouch(t, g.in, m)

	touch(target.TouchDown, 100, 1000)
	touch(target.TouchMove, 100, 900)
	tapped := make(chan error, 1)
	go func() { tapped <- g.in.agentTap(context.Background(), 270, 600, m) }()
	time.Sleep(100 * time.Millisecond)
	if r := p.requests(); len(r) != 0 {
		t.Fatalf("agent tap sent while the finger was down: %v", r)
	}
	touch(target.TouchMove, 100, 500)
	touch(target.TouchUp, 100, 500)
	if err := <-tapped; err != nil {
		t.Fatal(err)
	}
	checkBodies(t, p.waitFor(1), []any{[2]any{"tap", map[string]any{"x": float64(540), "y": float64(1200)}}})
	fr := w.got()
	if a, _, _ := liveFrame(fr[len(fr)-1]); a != "MANUAL_TOUCH_UP" {
		t.Fatalf("frames %v: the press had not lifted when the agent's tap went", fr)
	}
}

// The socket closes with the stream.
func TestRevylLiveSocketClosesWithTheStream(t *testing.T) {
	f := newFakeRevyl(t)
	p := newFakeProxy(t)
	w := newFakeWorker(t, p)
	g := restGestures(t, f, p.srv.URL, "android", 1080, 2400)
	w.waitSocket()
	g.in.close()
	select {
	case <-w.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the socket is still open after the stream closed")
	}
}

// A press the viewer never lifts lets go of the queue once it goes quiet,
// so an agent's gesture behind it still goes: with the socket up, and
// with the socket failed partway while the finger was down.
func TestRevylLiveLetsGoOfAQuietPress(t *testing.T) {
	m := target.Meta{Width: 540, Height: 1200}
	for _, failed := range []bool{false, true} {
		name := "socket up"
		if failed {
			name = "socket failed"
		}
		t.Run(name, func(t *testing.T) {
			f := newFakeRevyl(t)
			p := newFakeProxy(t)
			w := newFakeWorker(t, p)
			if failed {
				w.set(func(w *fakeWorker) { w.closeAfter = 1 })
			}
			g := restGestures(t, f, p.srv.URL, "android", 1080, 2400)
			g.in.liveIdle = 200 * time.Millisecond
			w.waitSocket()
			touch := liveTouch(t, g.in, m)

			touch(target.TouchDown, 100, 1000)
			if failed {
				<-w.closed
				w.until("the client to see the close", func() bool {
					g.in.lmu.Lock()
					defer g.in.lmu.Unlock()
					dead, _ := g.in.live.isDead()
					return dead
				})
				touch(target.TouchMove, 100, 900) // its write fails
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := g.in.agentTap(ctx, 270, 600, m); err != nil {
				t.Fatalf("agent tap behind a quiet press: %v", err)
			}
			checkBodies(t, p.waitFor(1), []any{[2]any{"tap", map[string]any{"x": float64(540), "y": float64(1200)}}})
		})
	}
}

// An iOS press goes live too, in points: the size worker-connection
// reports, which on a live iPhone was 402x874 for its 1206x2622 stream.
func TestRevylLiveTouchOnIOSIsInPoints(t *testing.T) {
	m := target.Meta{Width: 1206, Height: 2622}
	f := newFakeRevyl(t)
	p := newFakeProxy(t)
	w := newFakeWorker(t, p)
	w.set(func(w *fakeWorker) { w.devW, w.devH = 402, 874 })
	g := restGestures(t, f, p.srv.URL, "ios", 402, 874)
	w.waitSocket()
	touch := liveTouch(t, g.in, m)

	touch(target.TouchDown, 903, 1311)
	w.waitFrames(1)
	touch(target.TouchMove, 603, 1311)
	w.waitFrames(2)
	touch(target.TouchUp, 603, 1311)
	checkFrames(t, w.waitFrames(3), [][3]any{
		{"MANUAL_TOUCH_DOWN", 301, 437},
		{"MANUAL_TOUCH_MOVE", 201, 437},
		{"MANUAL_TOUCH_UP", 201, 437},
	})
	if rec := g.log.wait(t, 1)[0]; rec["via"] != "ws" || rec["gesture"] != "drag" {
		t.Fatalf("trace %v, want a drag via ws", rec)
	}
	if r := p.requests(); len(r) != 0 {
		t.Fatalf("device proxy got %d requests, want none", len(r))
	}
}

// A live press is mapped with the size worker-connection reports, not the
// session's, and with the session's when the worker gives none.
func TestRevylLiveMapsWithTheWorkersSize(t *testing.T) {
	for _, tc := range []struct {
		name             string
		platform         string
		screenW, screenH int // the session's size
		devW, devH       int // the worker's; 0 leaves it out
		m                target.Meta
		down, move       [2]int32
		want             [3][2]int // down, move, up
	}{
		{"android", "android", 1080, 2400, 720, 1600, target.Meta{Width: 540, Height: 1200},
			[2]int32{270, 900}, [2]int32{270, 600}, [3][2]int{{360, 1200}, {360, 800}, {360, 800}}},
		{"ios", "ios", 393, 852, 402, 874, target.Meta{Width: 1206, Height: 2622},
			[2]int32{603, 1311}, [2]int32{603, 1749}, [3][2]int{{201, 437}, {201, 583}, {201, 583}}},
		{"none reported", "android", 1080, 2400, 0, 0, target.Meta{Width: 540, Height: 1200},
			[2]int32{270, 900}, [2]int32{270, 600}, [3][2]int{{540, 1800}, {540, 1200}, {540, 1200}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeRevyl(t)
			p := newFakeProxy(t)
			w := newFakeWorker(t, p)
			w.set(func(w *fakeWorker) { w.devW, w.devH = tc.devW, tc.devH })
			g := restGestures(t, f, p.srv.URL, tc.platform, tc.screenW, tc.screenH)
			w.waitSocket()
			touch := liveTouch(t, g.in, tc.m)

			touch(target.TouchDown, tc.down[0], tc.down[1])
			w.waitFrames(1)
			touch(target.TouchMove, tc.move[0], tc.move[1])
			w.waitFrames(2)
			touch(target.TouchUp, tc.move[0], tc.move[1])
			checkFrames(t, w.waitFrames(3), [][3]any{
				{"MANUAL_TOUCH_DOWN", tc.want[0][0], tc.want[0][1]},
				{"MANUAL_TOUCH_MOVE", tc.want[1][0], tc.want[1][1]},
				{"MANUAL_TOUCH_UP", tc.want[2][0], tc.want[2][1]},
			})
		})
	}
}

// A stream whose shape is more than 1% off the size the worker reports
// keeps its press off the socket: the press goes as one gesture, mapped
// with the session's size, and says why. A stream turned to landscape is
// the same shape turned, and goes live.
func TestRevylLiveKeepsOffAStreamOfAnotherShape(t *testing.T) {
	f := newFakeRevyl(t)
	p := newFakeProxy(t)
	w := newFakeWorker(t, p)
	w.set(func(w *fakeWorker) { w.devW, w.devH = 402, 874 })
	g := restGestures(t, f, p.srv.URL, "ios", 402, 874)
	w.waitSocket()

	touch := liveTouch(t, g.in, target.Meta{Width: 1206, Height: 2400})
	touch(target.TouchDown, 603, 2000)
	time.Sleep(100 * time.Millisecond) // a live press has sent its down by now
	touch(target.TouchMove, 603, 1500)
	touch(target.TouchUp, 603, 1000)
	checkBodies(t, p.waitFor(1), []any{
		[2]any{"drag", map[string]any{"start_x": float64(201), "start_y": float64(728), "end_x": float64(201), "end_y": float64(364)}},
	})
	if fr := w.got(); len(fr) != 0 {
		t.Fatalf("frames sent for a stream of another shape: %v", fr)
	}
	rec := g.log.wait(t, 1)[0]
	if why, _ := rec["live"].(string); rec["via"] != "rest" || !strings.Contains(why, "1206x2400") {
		t.Fatalf("trace %v, want the drag via rest and why it was not live", rec)
	}

	touch = liveTouch(t, g.in, target.Meta{Width: 2622, Height: 1206})
	touch(target.TouchDown, 1311, 603)
	touch(target.TouchUp, 1311, 603)
	checkFrames(t, w.waitFrames(2), [][3]any{{"MANUAL_TOUCH_DOWN", 437, 201}, {"MANUAL_TOUCH_UP", 437, 201}})
}

// traceRecs is the records the trace has for action, in order.
func traceRecs(l *traceLog, action string) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, rec := range l.recs {
		if rec["action"] == action {
			out = append(out, rec)
		}
	}
	return out
}

// waitSize waits for the trace to say the socket took a size, and returns
// that record. It fails the test without stopping it, so what the next
// press does with the old size shows too.
func waitSize(t *testing.T, l *traceLog, size string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, rec := range traceRecs(l, "live-size") {
			if rec["size"] == size {
				return rec
			}
		}
		if time.Now().After(deadline) {
			t.Errorf("no live-size record for %s after 2s: %v", size, traceRecs(l, "live-size"))
			return nil
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// The socket can report another screen size after it opens, as the
// "elements" frames Revyl's viewer maps its touches with do. A press that
// starts after it is mapped onto that size, turned with the stream as
// before, and the trace says the size changed.
func TestRevylLiveFollowsTheSocketsSize(t *testing.T) {
	m := target.Meta{Width: 540, Height: 1200}
	f := newFakeRevyl(t)
	p := newFakeProxy(t)
	w := newFakeWorker(t, p)
	g := restGestures(t, f, p.srv.URL, "android", 1080, 2400)
	w.waitSocket()
	touch := liveTouch(t, g.in, m)

	touch(target.TouchDown, 270, 600)
	touch(target.TouchUp, 270, 600)
	w.waitFrames(2)
	w.push(`{"type": "elements", "elements": [], "device_width": 720, "device_height": 1600}`)
	rec := waitSize(t, g.log, "720x1600")
	touch(target.TouchDown, 270, 600)
	touch(target.TouchUp, 270, 600)
	w.waitFrames(4)
	touch = liveTouch(t, g.in, target.Meta{Width: 1200, Height: 540}) // turned to landscape
	touch(target.TouchDown, 600, 270)
	touch(target.TouchUp, 600, 270)
	checkFrames(t, w.waitFrames(6), [][3]any{
		{"MANUAL_TOUCH_DOWN", 540, 1200}, {"MANUAL_TOUCH_UP", 540, 1200},
		{"MANUAL_TOUCH_DOWN", 360, 800}, {"MANUAL_TOUCH_UP", 360, 800},
		{"MANUAL_TOUCH_DOWN", 800, 360}, {"MANUAL_TOUCH_UP", 800, 360},
	})
	if rec == nil || rec["was"] != "1080x2400" || rec["frame"] != "elements" {
		t.Fatalf("size record %v, want 720x1600 was 1080x2400 from an elements frame", rec)
	}
	for _, rec := range g.log.wait(t, 4) {
		b, _ := json.Marshal(rec)
		if s := string(b); strings.Contains(s, testWSToken) || strings.Contains(s, testRevylKey) || strings.Contains(s, "ws://") {
			t.Fatalf("token, key or URL in the trace: %s", s)
		}
	}
	if r := p.requests(); len(r) != 0 {
		t.Fatalf("device proxy got %d requests, want none", len(r))
	}
}

// On iOS a size from the socket is taken in the touches' units, points,
// as worker-connection's is: a DEVICE_INIT_STATUS with another size, then
// one nested under device_dimensions, each moves the next press.
func TestRevylLiveFollowsTheSocketsSizeOnIOS(t *testing.T) {
	m := target.Meta{Width: 1206, Height: 2622}
	f := newFakeRevyl(t)
	p := newFakeProxy(t)
	w := newFakeWorker(t, p)
	w.set(func(w *fakeWorker) { w.devW, w.devH = 402, 874 })
	g := restGestures(t, f, p.srv.URL, "ios", 402, 874)
	w.waitSocket()
	touch := liveTouch(t, g.in, m)

	touch(target.TouchDown, 603, 1311)
	touch(target.TouchUp, 603, 1311)
	w.waitFrames(2)
	w.push(`{"event_type": "DEVICE_INIT_STATUS", "status": "initialized", "manual_input_ready": true, "device_width": 440, "device_height": 956}`)
	waitSize(t, g.log, "440x956")
	touch(target.TouchDown, 603, 1311)
	touch(target.TouchUp, 603, 1311)
	w.waitFrames(4)
	w.push(`{"type": "elements", "device_dimensions": {"width": 402, "height": 874}}`)
	waitSize(t, g.log, "402x874")
	touch(target.TouchDown, 603, 1311)
	touch(target.TouchUp, 603, 1311)
	checkFrames(t, w.waitFrames(6), [][3]any{
		{"MANUAL_TOUCH_DOWN", 201, 437}, {"MANUAL_TOUCH_UP", 201, 437},
		{"MANUAL_TOUCH_DOWN", 220, 478}, {"MANUAL_TOUCH_UP", 220, 478},
		{"MANUAL_TOUCH_DOWN", 201, 437}, {"MANUAL_TOUCH_UP", 201, 437},
	})
	if r := p.requests(); len(r) != 0 {
		t.Fatalf("device proxy got %d requests, want none", len(r))
	}
}

// A size that comes while the finger is down leaves that press on the size
// it started with, so its moves and lift land where its down did. The
// next press takes the new size.
func TestRevylLiveKeepsAPressOnItsSize(t *testing.T) {
	m := target.Meta{Width: 540, Height: 1200}
	f := newFakeRevyl(t)
	p := newFakeProxy(t)
	w := newFakeWorker(t, p)
	g := restGestures(t, f, p.srv.URL, "android", 1080, 2400)
	w.waitSocket()
	touch := liveTouch(t, g.in, m)

	touch(target.TouchDown, 100, 1000)
	w.waitFrames(1)
	w.push(`{"type": "elements", "device_width": 720, "device_height": 1600}`)
	waitSize(t, g.log, "720x1600")
	touch(target.TouchMove, 100, 500)
	w.waitFrames(2)
	touch(target.TouchUp, 100, 500)
	w.waitFrames(3)
	touch(target.TouchDown, 270, 600)
	touch(target.TouchUp, 270, 600)
	checkFrames(t, w.waitFrames(5), [][3]any{
		{"MANUAL_TOUCH_DOWN", 200, 2000},
		{"MANUAL_TOUCH_MOVE", 200, 1000},
		{"MANUAL_TOUCH_UP", 200, 1000},
		{"MANUAL_TOUCH_DOWN", 360, 800},
		{"MANUAL_TOUCH_UP", 360, 800},
	})
}

// A size the client cannot read is no size: the socket keeps the one it
// had, the rest of that frame still counts, and the socket keeps taking
// touches and later sizes.
func TestRevylLiveIgnoresASizeItCannotRead(t *testing.T) {
	m := target.Meta{Width: 540, Height: 1200}
	f := newFakeRevyl(t)
	p := newFakeProxy(t)
	w := newFakeWorker(t, p)
	w.set(func(w *fakeWorker) { w.notReady = true })
	g := restGestures(t, f, p.srv.URL, "android", 1080, 2400)
	w.waitSocket()
	touch := liveTouch(t, g.in, m)

	for _, fr := range []string{
		`{"type": "elements", "device_width": "720", "device_height": 1600}`,
		`{"type": "elements", "device_width": -720, "device_height": -1600}`,
		`{"type": "elements", "device_width": 0, "device_height": 1600}`,
		`{"type": "elements", "device_width": 0.4, "device_height": 1600}`,
		`{"type": "elements", "device_width": 65536, "device_height": 1600}`,
		`{"type": "elements", "device_width": 720}`,
		`{"type": "elements", "device_width": null, "device_height": 1600}`,
		`{"type": "elements", "deviceDimensions": {"width": "720", "height": 1600}}`,
		`{"type": "elements", "deviceDimensions": [720, 1600]}`,
		`{"type": "elements", "device_width": 720, "device_height": 1600`,
		`not json`,
		// Its size is no use, but it says the device takes touches now.
		`{"event_type": "DEVICE_INIT_STATUS", "manual_input_ready": true, "device_width": {"px": 720}, "device_height": 1600}`,
	} {
		w.push(fr)
	}
	w.until("the socket to take touches", func() bool {
		g.in.lmu.Lock()
		defer g.in.lmu.Unlock()
		return g.in.live != nil && g.in.live.usable()
	})
	touch(target.TouchDown, 270, 600)
	touch(target.TouchUp, 270, 600)
	w.waitFrames(2)
	if rec := g.log.wait(t, 1)[0]; rec["via"] != "ws" || rec["ack"] != nil || rec["error"] != nil {
		t.Fatalf("trace %v, want a tap via ws that was acked", rec)
	}
	if got := traceRecs(g.log, "live-size"); len(got) != 0 {
		t.Fatalf("size records for sizes that cannot be read: %v", got)
	}
	w.push(`{"type": "elements", "device_width": 720, "device_height": 1600}`)
	waitSize(t, g.log, "720x1600")
	touch(target.TouchDown, 270, 600)
	touch(target.TouchUp, 270, 600)
	checkFrames(t, w.waitFrames(4), [][3]any{
		{"MANUAL_TOUCH_DOWN", 540, 1200}, {"MANUAL_TOUCH_UP", 540, 1200},
		{"MANUAL_TOUCH_DOWN", 360, 800}, {"MANUAL_TOUCH_UP", 360, 800},
	})
	if r := p.requests(); len(r) != 0 {
		t.Fatalf("device proxy got %d requests, want none", len(r))
	}
}

// On iOS a size that is the points size at 3x, or at 2x either way up,
// is the screen in pixels: touches stay on points, and the trace says the
// frame was ignored. A size that is no whole multiple still moves them.
func TestRevylLiveKeepsPointsOverPixelsOnIOS(t *testing.T) {
	m := target.Meta{Width: 1206, Height: 2622}
	f := newFakeRevyl(t)
	p := newFakeProxy(t)
	w := newFakeWorker(t, p)
	w.set(func(w *fakeWorker) { w.devW, w.devH = 402, 874 })
	g := restGestures(t, f, p.srv.URL, "ios", 402, 874)
	w.waitSocket()
	touch := liveTouch(t, g.in, m)

	w.push(`{"type": "elements", "device_width": 1206, "device_height": 2622}`)
	w.push(`{"type": "elements", "deviceDimensions": {"width": 1748, "height": 804}}`)
	rec := waitSize(t, g.log, "1748x804")
	touch(target.TouchDown, 603, 1311)
	touch(target.TouchUp, 603, 1311)
	w.waitFrames(2)
	w.push(`{"type": "elements", "device_width": 440, "device_height": 956}`)
	waitSize(t, g.log, "440x956")
	touch(target.TouchDown, 603, 1311)
	touch(target.TouchUp, 603, 1311)
	checkFrames(t, w.waitFrames(4), [][3]any{
		{"MANUAL_TOUCH_DOWN", 201, 437}, {"MANUAL_TOUCH_UP", 201, 437},
		{"MANUAL_TOUCH_DOWN", 220, 478}, {"MANUAL_TOUCH_UP", 220, 478},
	})
	if px := waitSize(t, g.log, "1206x2622"); px == nil || px["ignored"] == nil || px["was"] != "402x874" {
		t.Fatalf("size record %v, want 1206x2622 ignored, was 402x874", px)
	}
	if rec == nil || rec["ignored"] == nil {
		t.Fatalf("size record %v, want 1748x804 ignored", rec)
	}
	for _, r := range traceRecs(g.log, "live-size") {
		if r["size"] == "440x956" && r["ignored"] != nil {
			t.Fatalf("440x956 was ignored: %v", r)
		}
	}
}

// Android sizes are pixels on both sides, so a 2x size is followed.
func TestRevylLiveFollowsADoubledSizeOnAndroid(t *testing.T) {
	m := target.Meta{Width: 540, Height: 1200}
	f := newFakeRevyl(t)
	p := newFakeProxy(t)
	w := newFakeWorker(t, p)
	g := restGestures(t, f, p.srv.URL, "android", 1080, 2400)
	w.waitSocket()
	touch := liveTouch(t, g.in, m)

	w.push(`{"type": "elements", "device_width": 540, "device_height": 1200}`)
	if rec := waitSize(t, g.log, "540x1200"); rec == nil || rec["ignored"] != nil {
		t.Fatalf("size record %v, want 540x1200 followed", rec)
	}
	touch(target.TouchDown, 270, 600)
	touch(target.TouchUp, 270, 600)
	checkFrames(t, w.waitFrames(2), [][3]any{
		{"MANUAL_TOUCH_DOWN", 270, 600}, {"MANUAL_TOUCH_UP", 270, 600},
	})
}

// A stream more than 1% off the shape of the size worker-connection
// reports keeps its press off the socket, until the socket reports a size
// of the stream's shape. The next press then goes live, on that size.
func TestRevylLiveGoesLiveOnceTheSizeFitsTheStream(t *testing.T) {
	m := target.Meta{Width: 1206, Height: 2400}
	f := newFakeRevyl(t)
	p := newFakeProxy(t)
	w := newFakeWorker(t, p)
	w.set(func(w *fakeWorker) { w.devW, w.devH = 402, 874 })
	g := restGestures(t, f, p.srv.URL, "ios", 402, 874)
	w.waitSocket()
	touch := liveTouch(t, g.in, m)

	touch(target.TouchDown, 603, 2000)
	touch(target.TouchUp, 603, 2000)
	checkBodies(t, p.waitFor(1), []any{[2]any{"tap", map[string]any{"x": float64(201), "y": float64(728)}}})
	if fr := w.got(); len(fr) != 0 {
		t.Fatalf("frames sent for a stream of another shape: %v", fr)
	}
	w.push(`{"type": "elements", "deviceDimensions": {"width": 402, "height": 800}}`)
	waitSize(t, g.log, "402x800")
	touch(target.TouchDown, 603, 2000)
	w.waitFrames(1)
	touch(target.TouchMove, 603, 1000)
	w.waitFrames(2)
	touch(target.TouchUp, 603, 1000)
	checkFrames(t, w.waitFrames(3), [][3]any{
		{"MANUAL_TOUCH_DOWN", 201, 667},
		{"MANUAL_TOUCH_MOVE", 201, 333},
		{"MANUAL_TOUCH_UP", 201, 333},
	})
	if r := p.requests(); len(r) != 1 {
		t.Fatalf("device proxy got %d requests, want the first tap only", len(r))
	}
}

// The worker pings its viewers and drops one that does not answer. Each
// ping gets a pong with its id, string or number, as Revyl's viewer sends,
// and the socket still takes touches after.
func TestRevylLiveAnswersTheWorkersPings(t *testing.T) {
	m := target.Meta{Width: 540, Height: 1200}
	f := newFakeRevyl(t)
	p := newFakeProxy(t)
	w := newFakeWorker(t, p)
	g := restGestures(t, f, p.srv.URL, "android", 1080, 2400)
	w.waitSocket()
	w.push(`{"type": "ping", "id": 7, "prev_rtt_ms": 12.5}`)
	w.push(`{"type": "ping", "id": "p-8"}`)
	got := w.waitFrames(2)
	for i, want := range []any{float64(7), "p-8"} {
		if got[i]["type"] != "pong" || got[i]["id"] != want || len(got[i]) != 2 {
			t.Fatalf("frame %d = %v, want a pong with id %v and nothing else", i, got[i], want)
		}
	}
	touch := liveTouch(t, g.in, m)
	touch(target.TouchDown, 270, 600)
	touch(target.TouchUp, 270, 600)
	checkFrames(t, w.waitFrames(4)[2:], [][3]any{{"MANUAL_TOUCH_DOWN", 540, 1200}, {"MANUAL_TOUCH_UP", 540, 1200}})
}

// The trace says when the socket opened and why it closed: the worker's
// close status, how long it was open and the pings it answered, with no
// token or URL.
func TestRevylLiveTracesTheSocketsOpenAndClose(t *testing.T) {
	f := newFakeRevyl(t)
	p := newFakeProxy(t)
	w := newFakeWorker(t, p)
	w.set(func(w *fakeWorker) { w.closeAfter = 2 })
	g := restGestures(t, f, p.srv.URL, "android", 1080, 2400)
	w.waitSocket()
	if len(traceRecs(g.log, "live-open")) != 1 {
		t.Fatalf("live-open records %v, want one", traceRecs(g.log, "live-open"))
	}
	w.push(`{"type": "ping", "id": 1}`)
	w.push(`{"type": "ping", "id": 2}`)
	select {
	case <-w.closed:
	case <-time.After(5 * time.Second):
		t.Fatalf("the worker never got 2 pongs to close on: %v", w.got())
	}
	var rec map[string]any
	w.until("a live-close record", func() bool {
		if r := traceRecs(g.log, "live-close"); len(r) > 0 {
			rec = r[0]
		}
		return rec != nil
	})
	if rec["by"] != "worker" || rec["status"] != int(websocket.StatusGoingAway) || rec["pongs"] != 2 {
		t.Fatalf("live-close %v, want by worker, status 1001, 2 pongs", rec)
	}
	if _, ok := rec["uptimeMs"].(int64); !ok {
		t.Fatalf("live-close %v has no uptimeMs", rec)
	}
	g.in.close()
	b, _ := json.Marshal(traceRecs(g.log, "live-close"))
	if s := string(b); strings.Contains(s, testWSToken) || strings.Contains(s, "ws://") {
		t.Fatalf("token or URL in the trace: %s", s)
	}
}
