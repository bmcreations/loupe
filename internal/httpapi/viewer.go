package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/session"
)

// frameDeadline is how long after queuing a frame is still worth delivering.
// Past it the picture is history, and on a transport that can cancel an
// in-flight frame, continuing to send it only delays the next one.
//
// It is a floor, not the whole answer: see frameDeadlineFor.
const frameDeadline = 250 * time.Millisecond

// endFlushTimeout bounds how long an ended session's last control messages
// may take to write.
const endFlushTimeout = 500 * time.Millisecond

// frameDeadlineCap bounds frameDeadlineFor, so a wild baseline cannot leave a
// frame occupying the transport indefinitely.
const frameDeadlineCap = time.Second

// frameDeadlineFor returns the write deadline for a path whose baseline round
// trip is base.
//
// The constant alone assumes the path is fast, which is the assumption trap 5
// is about. On 5G with a 233ms baseline, half of the 250ms budget is gone to
// propagation before anything has gone wrong, and the 07:36 trace has twelve
// rows reporting writeMaxMs within 8ms of 250.0 with `payload: deadline
// exceeded`, against an average write of 27ms. Those frames were abandoned by
// the clock, not by the link.
//
// Half the baseline is the one-way estimate, matching session.StalenessFor so
// the two thresholds cannot disagree about what a healthy path costs.
func frameDeadlineFor(base time.Duration) time.Duration {
	if base < 0 {
		base = 0
	}
	if d := frameDeadline + base/2; d < frameDeadlineCap {
		return d
	}
	return frameDeadlineCap
}

// keyframeDeadline is the floor for a keyframe or a parameter set, and it is
// deliberately far above frameDeadline.
//
// The shed gate two hundred lines down already states the policy -- parameter
// sets and keyframes are never shed, because dropping them leaves the decoder
// with nothing to resynchronize against -- and then handed the frame to a
// deadline that abandoned it anyway. Trap 35 is what that contradiction costs:
// a 400 kbps link with a 380ms baseline gave every frame 440ms, and the viewer
// got no pictures at all at any encoder rate. No bitrate step can fix a frame
// the clock kills.
//
// Measured on that link, a keyframe write takes 220-570ms in a normal second
// and peaked at 1186.5ms in the second the shaping engaged. So 440ms was short
// by more than a factor of two at the worst, and the floor here carries the
// observed worst case with roughly 2x to spare. The queue wait ahead of the
// write is not the term that matters: it reads 0.0ms in most seconds.
//
// An inter-frame past its deadline is worth abandoning because a newer one is
// already behind it. A keyframe has no such successor: it is what every
// suppressed frame is waiting for, and the encoder will not send another
// unasked. Waiting is the cheaper mistake.
const keyframeDeadline = 2 * time.Second

// keyframeDeadlineCap bounds keyframeDeadlineFor. A keyframe may hold the
// transport far longer than a picture, but not indefinitely -- past this the
// link is not one a keyframe fits through at all, and the session is better
// served by the stream ending than by one frame occupying it.
//
// The value is not a round number chosen for comfort. `maxPlausibleTransitMs`
// in web/jitter.js is 5s, and trap 31 justifies it by this cap: a frame the
// transport delivers cannot legitimately imply a larger transit change than the
// longest it was ever willing to wait. Staying strictly under that keeps the
// client's guard sound. The measured worst keyframe write is 1186.5ms, so this
// still carries it three times over and has never been reached.
const keyframeDeadlineCap = 4 * time.Second

// keyframeDeadlineFor returns the write deadline for a keyframe or parameter
// set on a path whose baseline round trip is base.
//
// It scales on the baseline for the same reason frameDeadlineFor does: the
// propagation on a slow path is spent before anything has gone wrong. The
// multiplier is 2x rather than that function's 1/2x because the frame is an
// order of magnitude larger, so it pays the path cost over many more round
// trips.
func keyframeDeadlineFor(base time.Duration) time.Duration {
	if base < 0 {
		base = 0
	}
	if d := keyframeDeadline + 2*base; d < keyframeDeadlineCap {
		return d
	}
	return keyframeDeadlineCap
}

// deadlineFor returns the write deadline for one frame. Keyframes and parameter
// sets get their own, because they are the frames nothing else can replace.
func deadlineFor(key, config bool, base time.Duration) time.Duration {
	if key || config {
		return keyframeDeadlineFor(base)
	}
	return frameDeadlineFor(base)
}

// clientRTTMaxAge is how old a phone-reported round trip may be and still be
// worth feeding to the pacer.
//
// The client reports once a second, so anything approaching three seconds old
// means reports have stopped arriving -- which on this path is itself the
// symptom. Re-reading a stale low sample is how the pacer talked itself into
// staying wide open through 07:36:43 to 45 while shed climbed by 45 frames.
const clientRTTMaxAge = 2500 * time.Millisecond

// pickRTT chooses the round trip the pacer steers on, and names the source so a
// trace row says which signal the controller was actually using.
//
// QUIC first. It maintains SmoothedRTT per ACK, underneath anything that could
// queue it, so it stays fresh through exactly the congestion that stops the
// client's reports arriving. It is zero until the first ACK, and on WebSocket
// there is none at all.
//
// The phone's own number is the fallback, and it was the original signal for
// good reason: it is the only measure that includes every buffer in the path,
// including ones on the phone itself (see session.Pacer). What it cannot do is
// arrive on time, because it travels over /api/log, queued behind the video
// being throttled.
//
// A stale reading is discarded rather than re-fed. Returning zero holds the
// send fraction where it is, which is an honest answer to having no signal;
// re-reading a three-second-old 111ms sample is how the pacer sat at 1.00
// through 07:36:43 to 45 while shed climbed by 45 frames.
func pickRTT(quicRTT time.Duration, clientMs float64, clientAge time.Duration) (time.Duration, string) {
	if quicRTT > 0 {
		return quicRTT, "quic"
	}
	if clientMs > 0 && clientAge <= clientRTTMaxAge {
		return time.Duration(clientMs * float64(time.Millisecond)), "client"
	}
	return 0, "stale"
}

// healthInputFromClientReport reads the three session.Health inputs that come
// off the phone's own report: its measured rtt, its decoded-frames-per-second
// (Delivered), and how long ago that whole sample was recorded.
//
// All three share one staleness clock deliberately. cs is one JSON object the
// client sent as a unit -- web/app.js's sendClientStats -- so an rtt that
// stopped updating three seconds ago means the decFps sitting beside it in
// the same object is exactly as stale (trap 26: an old reading is no
// reading, for every field it carried, not only the one clientRTTAt used to
// track alone).
//
// Delivered comes from decFps, not from any byte count the server itself
// wrote to the transport. That is what keeps trap 35 from recurring at this
// layer: a frame the transport reset mid-flight was never decoded, so
// nothing here can read it as delivered. When cs is nil -- no report has ever
// arrived -- reportAge is pinned above clientRTTMaxAge so Health reads
// "stale" rather than judging rttMs 0 and delivered 0 as a healthy silence.
//
// at.Before(attachedAt) is judged the same way: clientStats is session
// scoped, not viewer scoped, so a viewer that joins a session another viewer
// was already watching would otherwise be handed that other connection's
// leftover report -- against its own pacer's Base() of 0, which read as "rtt
// Xms over a 0ms base" for a link nobody had measured yet on this viewer's
// behalf. A report timestamped before this viewer attached is treated as
// though none had arrived, so Health reads "stale" until this viewer's own
// report lands.
//
// Known gap, not fixed here: clientStats is session scoped, not viewer
// scoped, even after this change. Once more than one viewer has reported,
// every viewer in the session is judged on whichever report landed last,
// not its own -- this attach-time gate only makes the single-viewer case
// correct.
//
// now is a parameter rather than time.Now() so reportAge can be pinned
// exactly by a table test instead of racing the wall clock.
func healthInputFromClientReport(cs map[string]any, at, attachedAt, now time.Time) (rttMs float64, delivered int, reportAge time.Duration) {
	if cs == nil || at.Before(attachedAt) {
		return 0, 0, clientRTTMaxAge + time.Second
	}
	if v, ok := cs["rttMs"].(float64); ok {
		rttMs = v
	}
	// rttNowMs is the last second's pings alone. rttMs is the display's
	// running average, which takes about four seconds to fall back after a
	// burst, so health read degraded for four seconds of a link that had
	// already recovered. A client too old to send it still has rttMs.
	if v, ok := cs["rttNowMs"].(float64); ok {
		rttMs = v
	}
	if v, ok := cs["decFps"].(float64); ok {
		delivered = int(v)
	}
	return rttMs, delivered, now.Sub(at)
}

// statsMsgBody builds the JSON body of the per-second MsgStats message a
// viewer receives. Pulled out of serveViewer's stats case so the reviewer's
// "health or healthReason dropped from MsgStats" mutation has something to
// be tested against directly, rather than only through a live transport.
func statsMsgBody(tick healthTick, verdict, healthReason string, shed uint64, baseMs, keep float64, transport string) map[string]any {
	msg := map[string]any{
		"shed":         shed,
		"shedRate":     tick.ShedRate,
		"health":       verdict,
		"healthReason": healthReason,
		"baseMs":       baseMs,
		"keep":         keep,
		"transport":    transport,
	}
	// A stale verdict means tick.RttMs and tick.Delivered are either zero (no
	// report has ever arrived for this viewer) or older than staleAfter --
	// reporting them as this second's fps/rtt would repeat a stale or
	// synthetic number as though it were current (finding 6).
	if verdict != "stale" {
		msg["fps"] = tick.Delivered
		msg["rttMs"] = tick.RttMs
	}
	return msg
}

// keyframeRequestInterval bounds how often a viewer may ask the device for a
// fresh keyframe. Each request costs a full intra-coded frame, and under
// sustained shedding the request path runs once per frame, so unbounded it
// would ask ~60 times a second and flood the very link that is already behind.
const keyframeRequestInterval = 500 * time.Millisecond

// statsSource is the subset of *session.Session one stats tick reads, beyond
// what healthTracker itself needs (healthCounters). Narrowed to an interface
// so runStatsTick can be driven by a fake session in a test, the same way
// healthCounters lets healthTracker be tested without a live pump.
type statsSource interface {
	healthCounters
	FrameCount() uint64
	ShedBreakdown() session.ShedCounts
	DropCount() uint64
	WaitCount() uint64
	KeyCount() uint64
	Stats() map[string]any
	ViewerCount() int
	ClientStats() map[string]any
}

// runStatsTick does the work of one stats tick: sample health, build the
// MsgStats body and the trace record. Pulled out of serveViewer's stats case
// so the three mutations round 3's review found reachable only through a
// live transport -- baseMs silently zeroed before Sample (M20), MsgStats
// hardcoding "good"/"" regardless of the real verdict (M21), and the trace
// record dropping "health" (M22) -- each have a call site a test can drive
// directly instead. serveViewer's stats case is now just this call plus the
// two writes (trace, wire).
func runStatsTick(targetID string, sess statsSource, healthTrack *healthTracker, pacer *session.Pacer, conn Conn, rttSource string) (body []byte, rec map[string]any, err error) {
	baseMs := float64(pacer.Base().Microseconds()) / 1000

	tick := healthTrack.Sample(sess, baseMs)
	verdict, healthReason := session.Health(tick.Input)

	// shed and produced are read again here as the session's running
	// totals: healthTrack.Sample already diffed them against its own
	// last-seen values for tick.ShedRate and tick.ProducedRate, but the
	// trace record additionally wants each counter's cumulative total.
	shed := sess.ShedCount()
	produced := sess.FrameCount()

	body, err = json.Marshal(statsMsgBody(tick, verdict, healthReason, shed, baseMs, pacer.Keep(), conn.Name()))
	if err != nil {
		return nil, nil, err
	}

	shedBy := sess.ShedBreakdown()
	rec = map[string]any{
		"target":    targetID,
		"transport": conn.Name(),
		"pacerKeep": pacer.Keep(),
		// Microsecond resolution, because both shed thresholds are now
		// derived from this and a baseline that truncates to 0 is
		// indistinguishable from having no signal at all.
		"pacerBaseMs":  baseMs,
		"rttSource":    rttSource,
		"shedPacer":    shedBy.Pacer,
		"shedStale":    shedBy.Stale,
		"shedAwaitKey": shedBy.AwaitKey,
		"shedAbandon":  shedBy.Abandon,
		"shed":         shed,
		"shedRate":     tick.ShedRate,
		"serverDrop":   sess.DropCount(),
		"awaitKey":     sess.WaitCount(),
		"keysIn":       sess.KeyCount(),
		"framesOut":    sess.Stats()["framesOut"],
		// Named "produced", not "framesIn": trap 41 already gives framesIn a
		// meaning on stream-end (the per-encoder cumulative count at
		// close), and this is a different record kind reported once a
		// second.
		"produced":     produced,
		"viewers":      sess.ViewerCount(),
		"health":       verdict,
		"healthReason": healthReason,
	}
	if cs := sess.ClientStats(); cs != nil {
		rec["client"] = cs
	}
	// Optional so the Conn interface stays transport-blind: only the
	// WebTransport side has a send path that can refuse a frame two
	// different ways, and only it implements this.
	if ts, ok := conn.(interface{ TransportStats() map[string]any }); ok {
		rec["send"] = ts.TransportStats()
	}
	return body, rec, nil
}

// serveViewer runs one viewer to completion over any transport.
//
// Everything about deciding *what* to send lives here: the pacer, the keyframe
// gate, staleness, stats and tracing. Transports decide only *how* to put bytes
// on the wire, and what they can do about a frame that has already gone stale.
func (s *Server) serveViewer(targetID string, sess *session.Session, conn Conn) {
	viewerID, out := sess.AddViewer()
	defer s.awake.Hold()()
	defer func() {
		sess.RemoveViewer(viewerID)
		s.mgr.Release(targetID)
		_ = conn.Close()
	}()

	// Whichever side stops first names why the viewer stopped. Each one
	// closes the connection on its way out, so the other side's error is
	// usually that close coming back, not a second fault. In the 07:05 trace
	// on 28 September the phone reset the control stream as the page was
	// backgrounded, and ten records blamed the writer's next open.
	var stops viewerStops
	writeDone := make(chan struct{})
	go func() {
		// A send that fails ends this goroutine, and the read loop below does
		// not notice: it sits in Recv for as long as the client keeps sending.
		// The phone kept delivering swipes for 21s after its video and stats
		// stopped at 12:56:03, with nothing in the log, because the viewer
		// stayed attached with no writer behind it. Closing the connection
		// here ends Recv, so the session ends and the client reconnects.
		var stopErr error
		defer func() {
			// Noted even with no error: a writer that ends because the
			// session did still stopped first, and the read error its
			// close then causes is not the reason.
			stops.note("write", stopErr)
			_ = conn.Close()
			close(writeDone)
		}()
		keepalive := time.NewTicker(20 * time.Second)
		defer keepalive.Stop()
		stats := time.NewTicker(time.Second)
		defer stats.Stop()

		// health seeds its shed/produced/config diff base from the session's
		// current counters, not from zero, for the same reason the old
		// lastShed here did: those counters are session scoped and survive a
		// viewer, so a fresh viewer diffs against the session's running
		// total rather than assuming the stream started at zero when it
		// joined. The 07:36 trace has two rows, 200 and 207, where a naive
		// zero seed reported the session's whole accumulated shed as one
		// second's rate. attachedAt additionally gates which clientStats
		// report this viewer is allowed to read -- see
		// healthInputFromClientReport's doc comment.
		healthTrack := newHealthTracker(time.Now(), time.Now)
		// askForKey asks the encoder for a keyframe, at most once per
		// keyframeRequestInterval.
		//
		// Shedding an inter-frame leaves the decoder unable to resynchronize
		// until a keyframe arrives, and scrcpy does not emit them on a timer.
		// On a real device that made a single shed permanent: the trace shows
		// `shed` climbing for a minute while sent stayed at zero and every
		// write counter sat frozen. Synthetic hid it by emitting a keyframe per
		// second, which is why this only ever appeared on a real target.
		//
		// Rate-limited because under sustained shedding this runs once per
		// frame, and each request costs the device a full intra-coded frame.
		var lastKeyReq time.Time
		askForKey := func() {
			now := time.Now()
			if now.Sub(lastKeyReq) < keyframeRequestInterval {
				return
			}
			lastKeyReq = now
			// ErrNoControl on targets that cannot be asked; nothing to do.
			_ = sess.RequestKeyframe()
		}

		// needKey gates decode-unusable frames after a shed: an inter-frame
		// whose predecessors were dropped decodes into visible garbage.
		needKey := false
		pacer := session.NewPacer()

		// rttSource records which signal the last pacer sample came from, so a
		// trace row says whether the controller was steering on transport
		// measurements or on the phone's reports.
		rttSource := "none"
		// pathRTT gathers the two signals and applies pickRTT to them.
		pathRTT := func() time.Duration {
			var quicRTT time.Duration
			if pr, ok := conn.(interface{ PathRTT() time.Duration }); ok {
				quicRTT = pr.PathRTT()
			}
			ms, at := sess.ClientRTTAt()
			var age time.Duration
			if at.IsZero() {
				age = clientRTTMaxAge + time.Second
			} else {
				age = time.Since(at)
			}
			var rtt time.Duration
			rtt, rttSource = pickRTT(quicRTT, ms, age)
			return rtt
		}

		// checkPeerWindow asks the client to move to a fresh connection when
		// the browser will not refill this one's flow-control window
		// (peerwindow.go, trap 52). It reports false when the writer should
		// stop. Closing a stuck connection is watchPeerWindow's job, not this
		// one's: this loop is what gets stuck.
		pw, hasPW := conn.(peerWindow)
		rotateAsked := false
		checkPeerWindow := func() bool {
			if !hasPW {
				return true
			}
			p := pw.PeerWindow()
			action, reason := decidePeerWindow(p, rotateAsked)
			if action != peerWindowRotate {
				return true
			}
			rotateAsked = true
			s.trace.Record(map[string]any{"kind": "wt", "action": "rotate", "target": targetID,
				"viewer": viewerID, "reason": reason, "bytes": p.Bytes, "streams": p.Streams,
				"starvedMs": p.Starved.Milliseconds()})
			if err := conn.SendControl(encodeRotate(p.Bytes, p.Streams)); err != nil {
				stopErr = err
				return false
			}
			return true
		}
		if hasPW {
			go s.watchPeerWindow(pw, targetID, viewerID, writeDone)
		}

		for {
			select {
			case msg, ok := <-out:
				if !ok {
					return
				}
				// Control messages are not video and must not go down a
				// transport's video path, which may strip framing that only
				// applies to frames.
				if msg.Control {
					if err := conn.SendControl(msg.Data); err != nil {
						stopErr = err
						return
					}
					continue
				}

				pacer.Observe(pathRTT())

				// Parameter sets and keyframes are never shed: dropping them
				// leaves the decoder with nothing to resynchronize against.
				if !msg.Key && !msg.Config {
					if needKey {
						askForKey()
						sess.NoteShed(session.ShedAwaitKey)
						continue
					}
					if !pacer.Admit() {
						needKey = true
						askForKey()
						sess.NoteShed(session.ShedPacer)
						continue
					}
					if time.Since(msg.QueuedAt) > session.StalenessFor(pacer.Base()) {
						needKey = true
						askForKey()
						sess.NoteShed(session.ShedStale)
						continue
					}
				}

				err := conn.SendFrame(msg, msg.QueuedAt.Add(deadlineFor(msg.Key, msg.Config, pacer.Base())))
				delivered, fatal := frameDelivered(err)
				if fatal {
					stopErr = err
					return
				}
				if !checkPeerWindow() {
					return
				}
				if !delivered {
					// Reset mid-flight. The client has a truncated frame it
					// will discard, so this breaks the decode chain exactly
					// like a shed does -- including when the abandoned frame
					// was the keyframe that was meant to end the last wait.
					needKey = true
					askForKey()
					sess.NoteShed(session.ShedAbandon)
					continue
				}
				// What the link actually carried, which is what the bitrate
				// controller compares against what the encoder was asked for.
				// Data carries the WebSocket envelope's 18-byte header and
				// WebTransport replaces it with 13, so this overstates the
				// wire by a few bytes in a frame of thousands.
				sess.NoteSent(viewerID, len(msg.Data))
				if msg.Key {
					needKey = false
				}

			case <-stats.C:
				body, rec, err := runStatsTick(targetID, sess, healthTrack, pacer, conn, rttSource)
				if err != nil {
					continue
				}
				s.trace.Record(rec)
				if !checkPeerWindow() {
					return
				}

				if err := conn.SendControl(append([]byte{session.MsgStats}, body...)); err != nil {
					stopErr = err
					return
				}

			case <-keepalive.C:
				if err := conn.Keepalive(); err != nil {
					stopErr = err
					return
				}

			case <-sess.Done():
				// select picks at random among ready cases, so a control
				// message queued just before the close (Session.End's
				// MsgEnded) may still be waiting. Write those before the
				// connection goes; frames are not worth sending now.
				//
				// A client that stopped reading must not hold this
				// goroutine, so the flush has half a second where the
				// transport can bound it.
				if d, ok := conn.(interface{ SetEndDeadline(time.Time) }); ok {
					d.SetEndDeadline(time.Now().Add(endFlushTimeout))
				}
				// A transport whose close discards unsent control bytes
				// carries the reason in the close itself.
				if reason := sess.EndReason(); reason != "" {
					if c, ok := conn.(interface {
						CloseWithReason(code uint32, msg string)
					}); ok {
						defer c.CloseWithReason(wtEnded, reason)
					}
				}
				for {
					select {
					case msg, ok := <-out:
						if !ok {
							return
						}
						if msg.Control {
							if err := conn.SendControl(msg.Data); err != nil {
								stopErr = err
								return
							}
						}
					default:
						return
					}
				}
			}
		}
	}()

	for {
		payload, err := conn.Recv()
		if err != nil {
			stops.note("read", err)
			break
		}
		if len(payload) == 0 {
			continue
		}
		if err := s.dispatch(conn, sess, viewerID, payload); err != nil {
			fmt.Printf("[%s %s] dispatch: %v\n", conn.Name(), targetID, err)
		}
	}

	_ = conn.Close()
	<-writeDone
	if rec := stops.record(); rec != nil {
		rec["target"] = targetID
		rec["transport"] = conn.Name()
		fmt.Printf("[%s %s] %s: %v (%s)\n", conn.Name(), targetID, rec["side"], rec["err"], rec["errType"])
		s.trace.Record(rec)
	}
}

// viewerStops keeps the error each side of a viewer stopped with, and which
// came first.
type viewerStops struct {
	mu    sync.Mutex
	first string // "read" or "write"
	read  error
	write error
}

func (v *viewerStops) note(side string, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.first == "" {
		v.first = side
	}
	if side == "read" {
		v.read = err
	} else {
		v.write = err
	}
}

// record is the viewer-stop trace record, or nil when the side that stopped
// first did so without an error, as a writer does when its session ends.
// err is the side that stopped first, and the other side's error goes in
// readErr or writeErr. A read ending in io.EOF is the phone closing cleanly,
// and is still recorded when it came first.
func (v *viewerStops) record() map[string]any {
	v.mu.Lock()
	defer v.mu.Unlock()
	err, other, otherKey := v.read, v.write, "writeErr"
	if v.first == "write" {
		err, other, otherKey = v.write, v.read, "readErr"
	}
	if err == nil {
		return nil
	}
	rec := map[string]any{
		"kind": "viewer-stop",
		"side": v.first,
		"err":  err.Error(),
		// A closed WebTransport session has no message of its own; wtErr
		// fills in which side closed it and the code.
		"errType": fmt.Sprintf("%T", err),
	}
	// The wrapper's type says only where the error was wrapped; the
	// innermost one says what actually failed.
	if errors.Unwrap(err) != nil {
		rec["innerType"] = fmt.Sprintf("%T", innermost(err))
	}
	if other != nil {
		rec[otherKey] = other.Error()
	}
	return rec
}

// innermost follows Unwrap to the error at the bottom of err's chain. A joined
// error has no single bottom, so it stops there.
func innermost(err error) error {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return err
		}
		err = next
	}
}
