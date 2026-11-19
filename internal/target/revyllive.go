package target

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// A viewer's press on a Revyl device goes to the device as it happens, over
// the worker websocket Revyl's browser viewer uses, so a drag moves the
// screen while the finger is down. The socket takes text frames
//
//	{"event_type": "STREAM", "action": "MANUAL_TOUCH_DOWN" | "MANUAL_TOUCH_MOVE" | "MANUAL_TOUCH_UP",
//	 "action_id": n, "client_sent_at_ms": ms, "x": x, "y": y}
//
// in the screen size worker-connection reports: pixels on Android, points
// on iOS. MANUAL_TOUCH_UP adds "gesture" and "duration_ms". On a live account
// (2026-11-19) it answered each frame with an ACTION_RECEIVED frame and the
// UP with an ACTION_ACK frame carrying the same action_id and success; it
// opened with a CONNECTION frame and a DEVICE_INIT_STATUS frame whose
// manual_input_ready says whether it takes touches yet. Nothing about it is
// documented, so every failure falls back to the device proxy.
const (
	revylLiveOrigin = "https://app.revyl.ai" // the socket wants the viewer's origin
	// revylMoveEvery joins moves so at most 60 a second are sent.
	revylMoveEvery = 16 * time.Millisecond
	// revylLiveWrite bounds one frame's write.
	revylLiveWrite = 2 * time.Second
	// revylAckWait is how long the next gesture waits for a lift's ack, so
	// it cannot reach the device first.
	revylAckWait = 3 * time.Second
	// revylLiveIdle lifts a press nothing has been heard of for this long,
	// so a viewer that lost its finger cannot hold the queue.
	revylLiveIdle = 30 * time.Second
	// A socket that would not open is tried again on a later press, no
	// sooner than this, doubling to the most.
	revylLiveRetry    = 2 * time.Second
	revylLiveRetryMax = time.Minute
	// revylShapeSlack is how far the stream's aspect ratio may be from the
	// worker's screen size before a press stays off the socket. Revyl's
	// viewer turns input off past the same 1% on some iPhones rather than
	// guess where a touch lands.
	revylShapeSlack = 0.01
)

// revylLiveConn is one open worker socket. Only the input's worker writes
// to it; its own goroutine reads.
type revylLiveConn struct {
	c      *websocket.Conn
	cancel context.CancelFunc // ends the reader
	gone   chan struct{}      // closed once the reader stopped
	w, h   int                // the screen size worker-connection reports; 0 when it gave none

	mu     sync.Mutex
	ready  bool // manual_input_ready
	dead   bool
	diedAt time.Time
	nextID int64
	acks   map[int64]chan bool
}

// openLive looks up and dials the session's worker socket.
func (r *RevylREST) openLive(ctx context.Context, key, runID string) (*revylLiveConn, error) {
	ws, err := r.workerSocket(ctx, key, runID)
	if err != nil {
		return nil, err
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c, resp, err := websocket.Dial(dctx, ws.url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {revylLiveOrigin}, "User-Agent": {r.UserAgent}},
	})
	if err != nil {
		// The dial's error quotes the URL, token and all.
		msg := "the worker refused the socket"
		if resp != nil {
			msg += fmt.Sprintf(" (status %d)", resp.StatusCode)
		} else if s := revylTransportError(dctx, err); s == "timed out" || s == "cancelled" {
			msg = "dialling the worker " + s
		}
		return nil, errors.New("revyl live touch: " + msg)
	}
	// Binary frames are skipped unread, but a text frame past the default
	// 32 KiB would close the socket.
	c.SetReadLimit(-1)
	rctx, rcancel := context.WithCancel(context.Background())
	lc := &revylLiveConn{c: c, cancel: rcancel, gone: make(chan struct{}), w: ws.w, h: ws.h, nextID: 1, acks: map[int64]chan bool{}}
	go lc.read(rctx)
	return lc, nil
}

// read takes the socket's frames until it closes. It keeps
// manual_input_ready and hands each ack to the lift waiting for it; every
// other frame is dropped unread.
func (lc *revylLiveConn) read(ctx context.Context) {
	defer close(lc.gone)
	defer lc.fail()
	for {
		typ, r, err := lc.c.Reader(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			if _, err := io.Copy(io.Discard, r); err != nil {
				return
			}
			continue
		}
		b, err := io.ReadAll(io.LimitReader(r, 1<<20))
		if err == nil {
			_, err = io.Copy(io.Discard, r)
		}
		if err != nil {
			return
		}
		var f struct {
			EventType        string `json:"event_type"`
			ManualInputReady *bool  `json:"manual_input_ready"`
			ActionID         int64  `json:"action_id"`
			Success          *bool  `json:"success"`
		}
		if json.Unmarshal(b, &f) != nil {
			continue
		}
		lc.mu.Lock()
		switch f.EventType {
		case "DEVICE_INIT_STATUS":
			if f.ManualInputReady != nil {
				lc.ready = *f.ManualInputReady
			}
		case "ACTION_ACK":
			if ch := lc.acks[f.ActionID]; ch != nil {
				delete(lc.acks, f.ActionID)
				ch <- f.Success == nil || *f.Success
			}
		}
		lc.mu.Unlock()
	}
}

func (lc *revylLiveConn) fail() {
	lc.mu.Lock()
	if !lc.dead {
		lc.dead, lc.diedAt = true, time.Now()
	}
	lc.mu.Unlock()
}

// usable reports whether the socket is open and takes touches.
func (lc *revylLiveConn) usable() bool {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	return lc.ready && !lc.dead
}

func (lc *revylLiveConn) isDead() (bool, time.Time) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	return lc.dead, lc.diedAt
}

func (lc *revylLiveConn) close() {
	lc.fail()
	lc.cancel()
	_ = lc.c.CloseNow()
}

// revylLiveFrame is one touch frame.
type revylLiveFrame struct {
	EventType      string `json:"event_type"`
	Action         string `json:"action"`
	ActionID       int64  `json:"action_id"`
	ClientSentAtMs int64  `json:"client_sent_at_ms"`
	X              int    `json:"x"`
	Y              int    `json:"y"`
	Gesture        string `json:"gesture,omitempty"`
	DurationMs     *int64 `json:"duration_ms,omitempty"`
}

// send writes one touch frame. ack, for a lift, gets the worker's answer:
// true when it says success. The error says nothing the server sent.
func (lc *revylLiveConn) send(ctx context.Context, f revylLiveFrame, ack bool) (chan bool, error) {
	lc.mu.Lock()
	if lc.dead {
		lc.mu.Unlock()
		return nil, errRevylLiveClosed
	}
	f.EventType, f.ActionID, f.ClientSentAtMs = "STREAM", lc.nextID, time.Now().UnixMilli()
	lc.nextID++
	var ch chan bool
	if ack {
		ch = make(chan bool, 1)
		lc.acks[f.ActionID] = ch
	}
	lc.mu.Unlock()
	b, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	wctx, cancel := context.WithTimeout(ctx, revylLiveWrite)
	defer cancel()
	if err := lc.c.Write(wctx, websocket.MessageText, b); err != nil {
		lc.fail()
		if s := websocket.CloseStatus(err); s != -1 {
			return nil, fmt.Errorf("revyl live touch: the socket closed (status %d)", int(s))
		}
		return nil, errRevylLiveClosed
	}
	return ch, nil
}

var errRevylLiveClosed = errors.New("revyl live touch: the socket closed")

// revylLivePoint is a point of a live press: x, y where the socket puts it,
// px, py where the device proxy would, for the rest of a press the socket
// fails partway through.
type revylLivePoint struct {
	x, y   int
	px, py int
}

// revylLivePress is a press going to the device as it happens. The viewer's
// touches update it; the worker sends it.
type revylLivePress struct {
	w, h  int // the size its points are mapped onto; 0 for the session's
	p0    revylLivePoint
	start time.Time // in.now at the down
	nudge chan struct{}

	mu        sync.Mutex
	p         revylLivePoint // newest point
	heard     time.Time
	ended     bool
	cancelled bool
	gesture   string    // MANUAL_TOUCH_UP's gesture: tap, long_press or drag
	held      int64     // ms from down to lift
	finish    *revylJob // the press as one gesture, for the fallback
}

func (lp *revylLivePress) poke() {
	select {
	case lp.nudge <- struct{}{}:
	default:
	}
}

func (lp *revylLivePress) move(pt revylLivePoint) {
	lp.mu.Lock()
	lp.p, lp.heard = pt, time.Now()
	lp.mu.Unlock()
	lp.poke()
}

// lift ends the press at pt. finish is the same press as one gesture.
func (lp *revylLivePress) lift(pt revylLivePoint, gesture string, held time.Duration, finish *revylJob) {
	lp.mu.Lock()
	lp.p, lp.heard = pt, time.Now()
	lp.ended, lp.gesture, lp.held, lp.finish = true, gesture, held.Milliseconds(), finish
	lp.mu.Unlock()
	lp.poke()
}

// cancel lifts the press where it is: its down already reached the
// device, and a press left down would hold the screen.
func (lp *revylLivePress) cancel(held time.Duration) {
	lp.mu.Lock()
	lp.ended, lp.cancelled, lp.held = true, true, held.Milliseconds()
	if lp.gesture == "" {
		lp.gesture = "drag"
	}
	lp.mu.Unlock()
	lp.poke()
}

// liveFor returns the open socket a new press can go live on, or nil.
// With no socket it starts opening one, unless the last try is too recent;
// the press in hand still goes as a finished gesture.
func (in *revylInput) liveFor() *revylLiveConn {
	if in.rest == nil || in.runID == "" {
		return nil
	}
	in.lmu.Lock()
	defer in.lmu.Unlock()
	if lc := in.live; lc != nil {
		if lc.usable() {
			return lc
		}
		dead, at := lc.isDead()
		if !dead {
			return nil // open, but not taking touches yet
		}
		in.live = nil
		lc.close()
		in.retryAt = at.Add(max(in.backoff, revylLiveRetry))
	}
	if in.dialing || in.liveClosed || time.Now().Before(in.retryAt) {
		return nil
	}
	in.dialing = true
	in.dials.Add(1)
	go in.dialLive()
	return nil
}

func (in *revylInput) dialLive() {
	defer in.dials.Done()
	var lc *revylLiveConn
	err := errors.New("revyl live touch: no API key")
	if key := in.rest.key(); key != "" {
		lc, err = in.rest.openLive(in.ctx, key, in.runID)
	}
	in.lmu.Lock()
	defer in.lmu.Unlock()
	in.dialing = false
	if err != nil || in.liveClosed {
		if lc != nil {
			lc.close()
		}
		in.backoff = min(max(2*in.backoff, revylLiveRetry), revylLiveRetryMax)
		in.retryAt = time.Now().Add(in.backoff)
		if err != nil {
			in.liveErr = err.Error()
		}
		return
	}
	in.live, in.backoff, in.liveErr = lc, 0, ""
}

// closeLive closes the socket once the worker stopped.
func (in *revylInput) closeLive() {
	in.dials.Wait()
	in.lmu.Lock()
	lc := in.live
	in.live = nil
	in.lmu.Unlock()
	if lc != nil {
		lc.close()
	}
}

// noLive is why the last press did not go live, for the trace; "" when
// nothing failed.
func (in *revylInput) noLive() string {
	in.lmu.Lock()
	defer in.lmu.Unlock()
	return in.liveErr
}

// runLive sends a live press: the down, then the newest point at most every
// revylMoveEvery, then the lift. The worker stays on it until the finger
// lifts, so nothing else reaches the device mid-press.
//
// If the socket fails partway, the rest of the press goes over the device
// proxy once the finger lifts. With nothing sent, that is the whole press.
// With the down sent, it is a drag from the last point sent to the lift,
// and nothing when the press did not move on from there: a second tap or
// long-press would land twice.
func (in *revylInput) runLive(j *revylJob) {
	lp := j.live
	lp.mu.Lock()
	ended, finish := lp.ended, lp.finish
	lp.mu.Unlock()
	if ended && (finish == nil || finish.action != "tap") {
		// It waited behind other gestures for all of its press. Sent now,
		// a drag's moves would land at once, a fling, and a long-press
		// would lose its hold; they go as one gesture instead.
		in.liveFallback(lp, false, 0, 0, nil)
		return
	}
	in.lmu.Lock()
	lc := in.live
	in.lmu.Unlock()
	if lc == nil || !lc.usable() {
		in.liveFallback(lp, false, 0, 0, errRevylLiveClosed)
		return
	}
	sent := 0
	last := lp.p0
	var lastMove time.Time
	if _, err := lc.send(in.ctx, revylLiveFrame{Action: "MANUAL_TOUCH_DOWN", X: last.x, Y: last.y}, false); err != nil {
		in.liveFallback(lp, false, 0, 0, err)
		return
	}
	sent++
	for {
		lp.mu.Lock()
		pt, ended, heard := lp.p, lp.ended, lp.heard
		lp.mu.Unlock()
		if pt != last {
			if wait := revylMoveEvery - time.Since(lastMove); wait > 0 {
				select {
				case <-time.After(wait):
				case <-in.ctx.Done():
					in.liftNow(lc, lp, last.x, last.y, sent)
					return
				}
				continue
			}
			if _, err := lc.send(in.ctx, revylLiveFrame{Action: "MANUAL_TOUCH_MOVE", X: pt.x, Y: pt.y}, false); err != nil {
				in.liveFallback(lp, true, last.px, last.py, err)
				return
			}
			sent++
			last, lastMove = pt, time.Now()
			continue
		}
		if ended {
			break
		}
		select {
		case <-lp.nudge:
		case <-in.ctx.Done():
			in.liftNow(lc, lp, last.x, last.y, sent)
			return
		case <-time.After(in.liveIdle - time.Since(heard)):
			if time.Since(heard) >= in.liveIdle {
				in.liftNow(lc, lp, last.x, last.y, sent)
				return
			}
		}
	}
	lp.mu.Lock()
	gesture, held, cancelled := lp.gesture, lp.held, lp.cancelled
	lp.mu.Unlock()
	t0 := time.Now()
	ack, err := lc.send(in.ctx, revylLiveFrame{Action: "MANUAL_TOUCH_UP", X: last.x, Y: last.y, Gesture: gesture, DurationMs: &held}, true)
	if err != nil {
		in.liveFallback(lp, true, last.px, last.py, err)
		return
	}
	sent++
	rec := map[string]any{"action": "input", "gesture": revylLiveGesture(gesture), "via": "ws", "frames": sent}
	if cancelled {
		rec["cancelled"] = true
	}
	select {
	case ok := <-ack:
		if !ok {
			err = errors.New("revyl live touch: the device refused it")
		}
	case <-lc.gone:
		rec["ack"] = "socket closed"
	case <-time.After(revylAckWait):
		rec["ack"] = "none"
	case <-in.ctx.Done():
	}
	rec["ms"] = time.Since(t0).Milliseconds()
	in.done(nil, rec, err)
}

// liftNow lifts a press the viewer did not: the stream closed or the press
// went quiet. It is a best effort with a short bound of its own, since the
// input's context may be the one that ended.
func (in *revylInput) liftNow(lc *revylLiveConn, lp *revylLivePress, x, y, sent int) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	held := in.now().Sub(lp.start).Milliseconds()
	_, err := lc.send(ctx, revylLiveFrame{Action: "MANUAL_TOUCH_UP", X: x, Y: y, Gesture: "drag", DurationMs: &held}, false)
	why := "the press went quiet"
	if in.ctx.Err() != nil {
		why = "the stream closed"
	}
	if err == nil {
		err = errors.New("revyl live touch: lifted the finger: " + why)
	}
	in.done(nil, map[string]any{"action": "input", "gesture": "drag", "via": "ws", "frames": sent + 1}, err)
}

// liveFallback sends what is left of a press the socket could not carry,
// once the finger lifts. wrote says whether its down was sent; x, y is the
// last point sent, in the device proxy's units. cause is why, nil when the
// socket did not fail.
func (in *revylInput) liveFallback(lp *revylLivePress, wrote bool, x, y int, cause error) {
	for {
		lp.mu.Lock()
		ended, heard := lp.ended, lp.heard
		lp.mu.Unlock()
		if ended {
			break
		}
		select {
		case <-lp.nudge:
		case <-in.ctx.Done():
			return
		case <-time.After(in.liveIdle - time.Since(heard)):
			if time.Since(heard) >= in.liveIdle {
				// As in runLive, a quiet press lets go of the queue.
				lp.mu.Lock()
				lp.ended, lp.cancelled = true, true
				lp.mu.Unlock()
			}
		}
	}
	lp.mu.Lock()
	finish, cancelled := lp.finish, lp.cancelled
	lp.mu.Unlock()
	rec := map[string]any{"action": "input"}
	if cause != nil {
		rec["liveError"] = cause.Error()
	}
	switch {
	case cancelled || finish == nil:
		if !wrote {
			return // a cancelled press nothing of which was sent
		}
		rec["gesture"], rec["via"] = "drag", "ws"
		in.done(nil, rec, cause)
	case !wrote:
		rec["gesture"] = finish.action
		in.finish(finish, rec)
	case finish.action == "drag" && (finish.x1 != x || finish.y1 != y):
		rest := &revylJob{action: "drag", x: x, y: y, x1: finish.x1, y1: finish.y1}
		rec["gesture"] = rest.action
		in.finish(rest, rec)
	default:
		rec["gesture"], rec["via"] = finish.action, "ws"
		in.done(nil, rec, cause)
	}
}

// livePoint maps a stream pixel for a live press: onto the w x h screen
// the worker reports, or the session's when w is 0, and by toDevice for
// the device proxy. It fails where toDevice does, so a press that could
// not go as one gesture does not go live either.
func (in *revylInput) livePoint(w, h int, x, y int32, m Meta) (revylLivePoint, error) {
	px, py, err := in.toDevice(x, y, m)
	if err != nil {
		return revylLivePoint{}, err
	}
	pt := revylLivePoint{x: px, y: py, px: px, py: py}
	if w > 0 && h > 0 {
		pt.x, pt.y = scaleTo(x, y, m, w, h)
	}
	return pt, nil
}

// revylSameShape reports whether a stream of a known size is the w x h
// screen's shape, turned with the stream, within revylShapeSlack.
func revylSameShape(m Meta, w, h int) bool {
	w, h = turnedTo(m, w, h)
	want := float64(w) / float64(h)
	got := float64(m.Width) / float64(m.Height)
	return math.Abs(got-want) <= revylShapeSlack*want
}

// revylLiveGesture is the trace's name for a lift's gesture, the same as
// the device proxy's action.
func revylLiveGesture(g string) string {
	return strings.ReplaceAll(g, "_", "")
}
