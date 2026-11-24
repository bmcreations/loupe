package android

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/target/ios"

	"github.com/bmcreations/loupe/internal/target"

	"github.com/bmcreations/loupe/internal/scrcpy"
)

// An adb stream's remote track is a second scrcpy session on the same device,
// with its own scid, sized for a slow link. It runs only while the session
// keeps the gate open, which it does while a viewer is on the track.
//
// scrcpy maps onto RemoteStream less directly than simcap does:
//
//   - The gate cannot hold the encoder. Open starts the second session and
//     closed stops it after remoteLinger; in between it encodes every capture
//     up to its max fps. Frames a link cannot take are shed by the viewer's
//     queue, which then waits for a keyframe, as on the main track.
//   - RemoteKeyframe is a RESET_VIDEO on the second session's control socket.
//     It never touches the main session, so a slow viewer's resyncs no longer
//     restart the encoder a local viewer is watching.
//   - scrcpy fixes size, rate and bitrate at launch, so RemoteCap picks a rung
//     (adbRemoteRungsFor: the table under the main session's bitrate, topped
//     by the main session's own settings) and a rung change is a relaunch.
//     The new session starts beside the old one and takes over at its first
//     parameter set, so the track has no gap.
//   - RemoteMaxSize lowers the next launch's longest edge; it does not
//     relaunch.
//   - Periodic keyframes arrive without a parameter set before them. The
//     session replays the last one it saw to a joiner, so nothing waits on it.
//
// If no second session starts, the track carries the main session's frames
// instead, as it would have before this track existed, and a keyframe ask
// resets the main session.

// remoteRung is one setting the second session runs at.
type remoteRung struct {
	MaxSize, MaxFPS, BitRate int
	// From is the smallest cap that selects the rung.
	From int
}

// adbRemoteRungs are measured on a Solana Seeker (MT6878, c2.mtk.avc.encoder)
// under continuous scrolling, beside a 1440px 60 fps 4 Mbps main session that
// held 57-62 fps throughout:
//
//   - 480px 30 fps 300 kbps ran 0.36-0.47 Mbps, keyframes 6.5-9 KB;
//   - 720px 30 fps 750 kbps ran 0.6-1.1 Mbps, keyframes 8-18 KB;
//   - 1080px 30 fps 1.5 Mbps ran 1.3-1.8 Mbps, keyframes 11-29 KB;
//   - 1080px 60 fps 3 Mbps ran 2.4-3.1 Mbps, keyframes 3.5-35 KB.
//
// The encoder does not go under about 1.4 Mbps at 1080px or 0.9 Mbps at
// 720px, at 30 fps or at 15, so the lower rungs are smaller rather than just
// slower. From sits under each rung's rate because rate control raises only
// while the encoder fills half its cap: a rung that needed its own rate would
// never be reached.
var adbRemoteRungs = []remoteRung{
	{MaxSize: 480, MaxFPS: 30, BitRate: 300_000, From: 0},
	{MaxSize: 720, MaxFPS: 30, BitRate: 750_000, From: 600_000},
	{MaxSize: 1080, MaxFPS: 30, BitRate: 1_500_000, From: 1_200_000},
	{MaxSize: 1080, MaxFPS: 60, BitRate: 3_000_000, From: 2_400_000},
}

// adbRemoteLaunchRung is the rung a fresh track starts at: 720px, which held
// 30 fps on a 1 Mbps link. The 480px rung is reached only by cuts, so a fast
// viewer does not start on a 216x480 picture.
const adbRemoteLaunchRung = 1

// adbRemoteRungsFor is adbRemoteRungs under the main session's bitrate, no
// larger or faster than the main session, with the main session's own
// settings on top, so a viewer whose link carries the main stream's rate gets
// the picture the main track would have given it.
func adbRemoteRungsFor(main remoteLaunch) []remoteRung {
	fps := main.MaxFPS
	if fps <= 0 {
		fps = target.DefaultMaxFPS
	}
	clamp := func(g remoteRung) remoteRung {
		g.MaxFPS = min(g.MaxFPS, fps)
		if main.MaxSize > 0 {
			g.MaxSize = min(g.MaxSize, main.MaxSize)
		}
		return g
	}
	var rungs []remoteRung
	for _, g := range adbRemoteRungs {
		if main.BitRate > 0 && g.BitRate >= main.BitRate {
			break
		}
		rungs = append(rungs, clamp(g))
	}
	if len(rungs) == 0 {
		return []remoteRung{clamp(adbRemoteRungs[0])}
	}
	if main.BitRate <= 0 {
		return rungs
	}
	// From stays within twice the rung below's rate, or rate control, which
	// raises only while the encoder fills half its cap, could not reach it.
	prev := rungs[len(rungs)-1]
	from := min(main.BitRate*4/5, prev.BitRate*8/5)
	if from <= prev.From {
		return rungs
	}
	return append(rungs, remoteRung{MaxSize: main.MaxSize, MaxFPS: fps, BitRate: main.BitRate, From: from})
}

// remoteRateRange is rate control's floor, start and ceiling for rungs: the
// bottom rung's rate, the launch rung's, and the top rung's. The session's
// default floor is 2 Mbps, which the bottom three rungs sit under.
func remoteRateRange(rungs []remoteRung) (lo, start, hi int) {
	return rungs[0].BitRate, rungs[launchRung(rungs)].BitRate, rungs[len(rungs)-1].BitRate
}

func launchRung(rungs []remoteRung) int {
	return min(adbRemoteLaunchRung, len(rungs)-1)
}

const (
	// remoteLinger is how long the second session outlives a closed gate, so
	// a viewer that reconnects finds it running.
	remoteLinger = 5 * time.Second
	// remoteRaiseHold is how long the cap must stay above the running rung,
	// while it carries the track, before it moves up one. A relaunch costs
	// about half a second on the device and a keyframe on the link; rate
	// control moves every 100-300 ms.
	remoteRaiseHold = 2 * time.Second
	// remoteRaiseRegret: a raise cut within this long, after the raised rung
	// carried the track, doubles the hold into that rung, up to
	// remoteRaiseHoldMax, so a link that cannot take it is not relaunched
	// into every few seconds. The other rungs' holds are unchanged.
	remoteRaiseRegret  = 10 * time.Second
	remoteRaiseHoldMax = time.Minute
	// remoteFirstFrame is how long a launched session has to send its
	// parameter sets. Measured launches send them about 0.5 s after Launch
	// returns.
	remoteFirstFrame = 3 * time.Second
	// remoteMinLife: a session that ends sooner than this after starting
	// counts as a failed launch, so one that dies at once is not relaunched
	// forever.
	remoteMinLife = 10 * time.Second
	// remoteLaunchTimeout bounds one launch.
	remoteLaunchTimeout = 10 * time.Second
	// remoteFailLimit is how many launches in a row may fail, with no session
	// running, before the track falls back to the main session's frames.
	remoteFailLimit = 3
	// remoteBackoffMax bounds the wait between failed launches.
	remoteBackoffMax = 30 * time.Second
	// remoteKeyWait is how long one keyframe ask is outstanding: further asks
	// in that window are the same ask. A reset answers in 100-215 ms.
	remoteKeyWait = time.Second
	// remoteStep is how often the manager re-reads its state without a poke.
	remoteStep = 250 * time.Millisecond
	// remoteMinPTSGap is the least a source switch advances the timeline.
	remoteMinPTSGap = time.Millisecond
	// remoteOutLen is the track's buffer, like the main stream's.
	remoteOutLen = 64
)

// errNoRemote is what the RemoteStream methods return on a stream without a
// remote track. The session never calls them on one, since Remote is nil.
var errNoRemote = errors.New("adb: no remote track")

// remoteLaunch is what one second session is started with.
type remoteLaunch struct {
	MaxSize, MaxFPS, BitRate int
}

// remoteLauncher starts a second session.
type remoteLauncher func(ctx context.Context, l remoteLaunch) (target.RemoteSession, error)

// adbRemoteWanted decides whether an adb stream gets a remote track:
// physical devices by default, where it was measured, and emulators only
// with LOUPE_REMOTE_TRACK=on. LOUPE_REMOTE_TRACK=off turns it off for both.
func adbRemoteWanted(kind target.Kind) bool {
	return ios.RemoteTrackWanted(kind == target.KindDevice)
}

// rungPicker turns rate control's cap into a rung. It cuts at once, to the
// rung the cap selects, and raises one rung at a time, after the cap has held
// above the next rung while the chosen rung carries the track.
//
// A cut follows the cap even straight after a raise. At 1 Mbps on the Seeker
// a 1.5 Mbps rung that ran two seconds left a backlog that drove the cap to
// its floor. Back on 720px, which runs at about the link's rate, delay fell
// only from 1.37 to 1.02 s in seven seconds; on 480px it fell from 0.96 s to
// 44 ms in two.
type rungPicker struct {
	rungs []remoteRung
	cur   int
	// hold[i] is how long the cap must hold before a raise into rungs[i].
	hold    []time.Duration
	upSince time.Time
	// raisedAt is when cur was raised into, zero after a cut.
	raisedAt time.Time
}

func newRungPicker(rungs []remoteRung) *rungPicker {
	hold := make([]time.Duration, len(rungs))
	for i := range hold {
		hold[i] = remoteRaiseHold
	}
	return &rungPicker{rungs: rungs, cur: launchRung(rungs), hold: hold}
}

// rungFor is the highest rung bps selects.
func (p *rungPicker) rungFor(bps int) int {
	i := 0
	for j, r := range p.rungs {
		if bps >= r.From {
			i = j
		}
	}
	return i
}

// pick takes the cap at now and returns the rung to run. carrying is the rung
// of the session carrying the track, -1 for none. A cap of zero is no cap yet.
func (p *rungPicker) pick(now time.Time, bps, carrying int) int {
	if bps <= 0 {
		return p.cur
	}
	want := p.rungFor(bps)
	settled := carrying == p.cur
	switch {
	case want < p.cur:
		p.upSince = time.Time{}
		if settled && !p.raisedAt.IsZero() && now.Sub(p.raisedAt) < remoteRaiseRegret {
			// The raise carried the track and did not hold.
			p.hold[p.cur] = min(2*p.hold[p.cur], remoteRaiseHoldMax)
		}
		p.cur, p.raisedAt = want, time.Time{}
	case want > p.cur:
		if !settled {
			p.upSince = time.Time{}
			break
		}
		if p.upSince.IsZero() {
			p.upSince = now
		} else if now.Sub(p.upSince) >= p.hold[p.cur+1] {
			p.raisedAt = now
			p.cur++
			p.upSince = time.Time{}
		}
	default:
		p.upSince = time.Time{}
	}
	return p.cur
}

// remoteSource is one origin of the track's frames: a second session, or the
// main session in fallback. scrcpy starts every session's PTS at zero, so each
// source is rebased onto the track's timeline at its first media frame.
type remoteSource struct {
	based  bool
	offset time.Duration
}

// remoteRun is one second session.
type remoteRun struct {
	sess      target.RemoteSession
	rung      int
	started   time.Time
	gotConfig bool
	src       remoteSource
}

// adbRemote is an adb stream's remote track.
type adbRemote struct {
	launch  remoteLauncher
	rungs   []remoteRung
	maxSize int // the main session's longest edge, 0 for none
	mainKey func() error
	now     func() time.Time

	out  chan target.Frame
	poke chan struct{}
	ctx  context.Context
	stop context.CancelFunc

	mu sync.Mutex
	// closed is set once out is closed; nothing sends after.
	closed bool
	// open is the gate; closedAt is when it last closed.
	open     bool
	closedAt time.Time
	wish     int // the cap rate control last asked for, 0 before it has
	sizeCap  int // RemoteMaxSize, 0 for none
	picker   *rungPicker

	cur       *remoteRun // the session the track carries
	next      *remoteRun // launched, taking over at its first parameter set
	launching bool
	fails     int // failed launches in a row
	retryAt   time.Time
	launches  int // sessions started, for tests

	keyUntil time.Time // a keyframe ask is outstanding until then
	waitKey  bool      // out was full: drop until a keyframe

	// fallback carries the main session's frames for the rest of the stream.
	fallback bool
	mainCfg  *target.Frame // the main session's last parameter set
	needCfg  bool          // send mainCfg ahead of the next main keyframe
	mainSrc  remoteSource

	// The track's timeline, which every source is rebased onto.
	lastPTS time.Duration
	lastAt  time.Time
	havePTS bool

	resetMsg []byte
}

// newADBRemote starts the track's manager. main is the main session's
// settings, which top the rungs; mainKey resets the main session, for the
// fallback.
func newADBRemote(launch remoteLauncher, main remoteLaunch, mainKey func() error) *adbRemote {
	r := newADBRemoteAt(launch, main, mainKey, time.Now)
	go r.run()
	return r
}

// newADBRemoteAt is newADBRemote on a given clock, without the manager
// goroutine: the caller runs step.
func newADBRemoteAt(launch remoteLauncher, main remoteLaunch, mainKey func() error, now func() time.Time) *adbRemote {
	ctx, stop := context.WithCancel(context.Background())
	var buf [scrcpy.ResetVideoMessageLen]byte
	n, _ := scrcpy.EncodeResetVideo(buf[:])
	rungs := adbRemoteRungsFor(main)
	r := &adbRemote{
		launch:   launch,
		rungs:    rungs,
		maxSize:  main.MaxSize,
		mainKey:  mainKey,
		now:      now,
		out:      make(chan target.Frame, remoteOutLen),
		poke:     make(chan struct{}, 1),
		ctx:      ctx,
		stop:     stop,
		picker:   newRungPicker(rungs),
		resetMsg: append([]byte(nil), buf[:n]...),
	}
	return r
}

func (r *adbRemote) nudge() {
	select {
	case r.poke <- struct{}{}:
	default:
	}
}

// gate implements RemoteGate. It never blocks: the session calls it from a
// 10 ms loop.
func (r *adbRemote) gate(open bool) {
	r.mu.Lock()
	if r.open && !open {
		r.closedAt = r.now()
	}
	r.open = open
	r.mu.Unlock()
	r.nudge()
}

// setCap implements RemoteCap.
func (r *adbRemote) setCap(bps int) {
	r.mu.Lock()
	r.wish = bps
	r.mu.Unlock()
	r.nudge()
}

// setMaxSize implements RemoteMaxSize. It applies from the next launch.
func (r *adbRemote) setMaxSize(px int) {
	r.mu.Lock()
	r.sizeCap = px
	r.mu.Unlock()
}

// keyframe implements RemoteKeyframe. With no session running it does
// nothing, since every launch leads with a parameter set and a keyframe.
func (r *adbRemote) keyframe() {
	now := r.now()
	r.mu.Lock()
	if now.Before(r.keyUntil) {
		r.mu.Unlock()
		return
	}
	var send func() error
	switch {
	case r.fallback:
		send = r.mainKey
	case r.cur != nil:
		sess, msg := r.cur.sess, r.resetMsg
		send = func() error { return sess.SendControl(msg) }
	default:
		r.mu.Unlock()
		return
	}
	r.keyUntil = now.Add(remoteKeyWait)
	r.mu.Unlock()
	// Off the caller: a viewer's write loop asks from here, and a control
	// write can wait out its two-second deadline.
	go func() { _ = send() }()
}

// encoderBps is the bitrate the running session was launched at, 0 for none.
func (r *adbRemote) encoderBps() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		return 0
	}
	return r.rungs[r.cur.rung].BitRate
}

// close stops the manager and every session, and closes out.
func (r *adbRemote) close() {
	r.stop()
	r.mu.Lock()
	runs := []*remoteRun{r.cur, r.next}
	r.cur, r.next = nil, nil
	if !r.closed {
		r.closed = true
		close(r.out)
	}
	r.mu.Unlock()
	closeRuns(runs)
}

func closeRuns(runs []*remoteRun) {
	for _, run := range runs {
		if run != nil {
			_ = run.sess.Close()
		}
	}
}

// run is the manager: it starts, replaces and stops second sessions.
func (r *adbRemote) run() {
	tick := time.NewTicker(remoteStep)
	defer tick.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-r.poke:
		case <-tick.C:
		}
		r.step(r.now())
	}
}

// step decides once what should be running.
func (r *adbRemote) step(now time.Time) {
	var stale []*remoteRun
	defer func() { closeRuns(stale) }()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.fallback {
		return
	}
	want := r.open || (!r.closedAt.IsZero() && now.Sub(r.closedAt) < remoteLinger)
	if !want {
		stale = append(stale, r.cur, r.next)
		r.cur, r.next = nil, nil
		// The next viewer starts at the launch rung, where rate control
		// starts it.
		r.picker = newRungPicker(r.rungs)
		r.keyUntil = time.Time{}
		return
	}
	// A launch that sent nothing in time is a failed launch.
	if r.next != nil && !r.next.gotConfig && now.Sub(r.next.started) > remoteFirstFrame {
		stale = append(stale, r.next)
		r.next = nil
		r.failLocked(now)
		if r.fallback {
			return
		}
	}
	carrying := -1
	if r.cur != nil {
		carrying = r.cur.rung
	}
	rung := r.picker.pick(now, r.wish, carrying)
	if r.next != nil && r.next.rung != rung {
		// The cap moved again while the replacement was starting.
		stale = append(stale, r.next)
		r.next = nil
	}
	if r.launching || r.next != nil || now.Before(r.retryAt) {
		return
	}
	if r.cur != nil && r.cur.rung == rung {
		return
	}
	r.launching = true
	go r.start(rung, r.launchFor(rung))
}

// launchFor is rung's launch, no larger than the main session or
// RemoteMaxSize. The caller holds mu.
func (r *adbRemote) launchFor(rung int) remoteLaunch {
	g := r.rungs[rung]
	size := g.MaxSize
	if r.maxSize > 0 && r.maxSize < size {
		size = r.maxSize
	}
	if r.sizeCap > 0 && r.sizeCap < size {
		size = r.sizeCap
	}
	return remoteLaunch{MaxSize: size, MaxFPS: g.MaxFPS, BitRate: g.BitRate}
}

// start launches one second session and hands it to the manager.
func (r *adbRemote) start(rung int, l remoteLaunch) {
	ctx, cancel := context.WithTimeout(r.ctx, remoteLaunchTimeout)
	sess, err := r.launch(ctx, l)
	cancel()
	now := r.now()
	r.mu.Lock()
	r.launching = false
	if err != nil {
		if !r.closed {
			fmt.Fprintf(os.Stderr, "[remote] second scrcpy session at %dpx %d fps %d bps: %v\n", l.MaxSize, l.MaxFPS, l.BitRate, err)
			r.failLocked(now)
		}
		r.mu.Unlock()
		r.nudge()
		return
	}
	if r.closed || r.fallback {
		r.mu.Unlock()
		_ = sess.Close()
		return
	}
	run := &remoteRun{sess: sess, rung: rung, started: now}
	r.next = run
	r.launches++
	r.mu.Unlock()
	go r.read(run)
	r.nudge()
}

// failLocked counts a failed launch and backs off. With no session running,
// remoteFailLimit failures in a row switch the track to the main session's
// frames. The caller holds mu.
func (r *adbRemote) failLocked(now time.Time) {
	r.fails++
	if r.cur == nil && r.fails >= remoteFailLimit {
		fmt.Fprintf(os.Stderr, "[remote] %d second sessions failed in a row; the remote track carries the main stream\n", r.fails)
		stale := r.next
		r.next = nil
		r.fallback, r.needCfg, r.waitKey = true, true, true
		r.keyUntil = time.Time{}
		if stale != nil {
			go func() { _ = stale.sess.Close() }()
		}
		// The track's viewers need a keyframe from the main session now.
		if r.mainKey != nil {
			go func() { _ = r.mainKey() }()
		}
		return
	}
	backoff := time.Second << min(r.fails-1, 5)
	r.retryAt = now.Add(min(backoff, remoteBackoffMax))
}

// read forwards one session's frames until it ends.
func (r *adbRemote) read(run *remoteRun) {
	for f := range run.sess.Frames() {
		r.onFrame(run, f)
	}
	now := r.now()
	r.mu.Lock()
	ended := false
	switch run {
	case r.cur:
		r.cur, ended = nil, true
	case r.next:
		r.next, ended = nil, true
	}
	if ended && !r.closed && !r.fallback {
		if !run.gotConfig || now.Sub(run.started) < remoteMinLife {
			r.failLocked(now)
		}
	}
	r.mu.Unlock()
	_ = run.sess.Close()
	r.nudge()
}

// onFrame takes one frame from a second session.
func (r *adbRemote) onFrame(run *remoteRun, f scrcpy.Frame) {
	var old *remoteRun
	r.mu.Lock()
	if f.Config && !run.gotConfig {
		run.gotConfig = true
		if run == r.next {
			old, r.cur, r.next = r.cur, run, nil
			r.fails = 0
			// What was asked of the old session is the new one's first
			// keyframe.
			r.keyUntil = r.now().Add(remoteKeyWait)
		}
	}
	if run == r.cur {
		if f.Key {
			// Asked for or periodic, it answers whatever was outstanding.
			r.keyUntil = time.Time{}
		}
		r.sendLocked(&run.src, target.Frame{PTS: f.PTS, Key: f.Key, Config: f.Config, Data: f.Data, Captured: f.Captured})
	}
	r.mu.Unlock()
	if old != nil {
		_ = old.sess.Close()
	}
}

// offerMain takes one of the main session's frames. Only a fallback track
// carries it; otherwise it only keeps the parameter set.
func (r *adbRemote) offerMain(f target.Frame) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f.Config {
		c := f
		c.Data = append([]byte(nil), f.Data...)
		r.mainCfg = &c
	}
	if !r.fallback {
		return
	}
	if f.Key {
		r.keyUntil = time.Time{}
	}
	if f.Config {
		r.needCfg = false
	} else if f.Key && r.needCfg && r.mainCfg != nil {
		c := *r.mainCfg
		c.Data = append([]byte(nil), c.Data...)
		c.Captured = f.Captured
		r.sendLocked(&r.mainSrc, c)
		r.needCfg = false
	}
	// The main session's reader keeps the original.
	f.Data = append([]byte(nil), f.Data...)
	r.sendLocked(&r.mainSrc, f)
}

// sendLocked puts one frame on the track without blocking, rebasing its PTS
// onto the track's timeline. A full channel drops frames until a keyframe and
// asks for one. The caller holds mu.
func (r *adbRemote) sendLocked(src *remoteSource, f target.Frame) {
	if r.closed {
		return
	}
	at := f.Captured
	if at.IsZero() {
		at = r.now()
	}
	if f.Config {
		// A parameter set carries no PTS of its own; scrcpy sends zero.
		f.PTS = r.lastPTS
	} else {
		if !src.based {
			src.based = true
			if r.havePTS {
				src.offset = r.lastPTS + max(at.Sub(r.lastAt), remoteMinPTSGap) - f.PTS
			}
		}
		f.PTS += src.offset
		r.lastPTS, r.lastAt, r.havePTS = f.PTS, at, true
	}
	if r.waitKey && !f.Key && !f.Config {
		return
	}
	select {
	case r.out <- f:
		if f.Key {
			r.waitKey = false
		}
	default:
		if !r.waitKey {
			r.waitKey = true
			go r.keyframe()
		}
	}
}

// remoteLauncher starts second sessions on t at the main session's GOP. The
// track is always H.264, since the session reads its codec from an SPS.
func (t *Device) remoteLauncher(opts target.Options) remoteLauncher {
	return func(ctx context.Context, l remoteLaunch) (target.RemoteSession, error) {
		sess, err := scrcpy.Launch(ctx, scrcpy.Config{
			ADBPath:             t.adb.Path,
			Serial:              t.info.Serial,
			ServerJar:           t.jar,
			ServerVersion:       t.version,
			MaxSize:             l.MaxSize,
			MaxFPS:              l.MaxFPS,
			BitRate:             l.BitRate,
			VideoCodec:          scrcpy.DefaultVideoCodec,
			KeyframeIntervalSec: opts.KeyframeIntervalSec,
			Secondary:           true,
		})
		if err != nil {
			return nil, err
		}
		return sess, nil
	}
}

// Remote implements RemoteStream. The channel stays open across second
// sessions and closes with the stream.
func (s *adbStream) Remote() <-chan target.Frame {
	if s.remote == nil {
		return nil
	}
	return s.remote.out
}

// RemoteGate implements RemoteStream: open starts the second session, closed
// stops it after remoteLinger.
func (s *adbStream) RemoteGate(open bool) error {
	if s.remote == nil {
		return errNoRemote
	}
	s.remote.gate(open)
	return nil
}

// RemoteKeyframe implements RemoteStream: a RESET_VIDEO on the second session.
func (s *adbStream) RemoteKeyframe() error {
	if s.remote == nil {
		return errNoRemote
	}
	s.remote.keyframe()
	return nil
}

// RemoteCap implements RemoteStream by choosing a rung.
func (s *adbStream) RemoteCap(bps int) error {
	if s.remote == nil {
		return errNoRemote
	}
	s.remote.setCap(bps)
	return nil
}

// RemoteMaxSize implements RemoteStream, from the next launch.
func (s *adbStream) RemoteMaxSize(px int) error {
	if s.remote == nil {
		return errNoRemote
	}
	s.remote.setMaxSize(px)
	return nil
}

// RemoteRateRange implements SteppedRemoteStream.
func (s *adbStream) RemoteRateRange() (lo, start, hi int) {
	if s.remote == nil {
		return remoteRateRange(adbRemoteRungs)
	}
	return remoteRateRange(s.remote.rungs)
}

// RemoteEncoderBps implements SteppedRemoteStream.
func (s *adbStream) RemoteEncoderBps() int {
	if s.remote == nil {
		return 0
	}
	return s.remote.encoderBps()
}
