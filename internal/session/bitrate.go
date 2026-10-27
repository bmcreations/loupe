package session

import "time"

// Bounds and thresholds for adaptive bitrate.
//
// The numbers come from a 5G trace, not from taste. The encoder was configured
// for 3000 kbps while the wire carried 191-276 kbps, and roughly half of every
// motion burst was discarded: 19 of 85 seconds captured 20 or more frames, and
// those 19 seconds account for 302 of the 406 frames shed awaiting a keyframe.
// Nothing in the system told the device any of that, because scrcpy has no
// runtime bitrate control -- opcodes run 0 to 17 and none of them is a bitrate
// -- so the only way to act on it is to relaunch the encoder.
const (
	// minAdaptiveBitRate floors the controller. Below roughly this, the
	// picture has stopped being worth the bytes, and a controller chasing a
	// link that is simply down would otherwise walk toward zero and keep
	// paying an encoder restart for each step.
	minAdaptiveBitRate = 250_000

	// downWindow is how long shedding must persist before the encoder is
	// changed. One bad second is a burst; ten is the link.
	downWindow = 10 * time.Second

	// upWindow is deliberately six times downWindow. Coming down is a
	// response to something already going wrong, and going back up is a guess
	// that costs a visible restart if it is wrong.
	upWindow = 60 * time.Second

	// settleWindow suppresses decisions after a change. A relaunch disturbs
	// the very counters the decision is read from -- the stream stops, every
	// viewer waits for a keyframe, and shed spikes -- so measuring inside it
	// reads the restart as evidence the link got worse.
	settleWindow = 12 * time.Second

	// maxAdaptiveChanges caps how many times one session may re-encode. A
	// link sitting exactly on the threshold would otherwise hunt up and down
	// for as long as the session lasts, and each step is a blip the viewer
	// sees.
	maxAdaptiveChanges = 6

	// downShedFraction is the share of frames that must be shed to justify a
	// change. The trace sat near 0.5; this triggers well below that while
	// staying clear of the 2% a healthy run shows.
	downShedFraction = 0.20

	// upShedFraction is what counts as clean enough to probe upward.
	upShedFraction = 0.02

	// minChangeFraction stops a change too small to be worth a restart.
	//
	// It is shared with live changes, which cost no restart, because it
	// guards a second thing. Every step up is by half, so only a step down
	// can be this small, and a step down is aimed at goodput: shedding a
	// fifth of the frames while the link carries within a tenth of the
	// encoder's rate is nearly the linkCarried case, where the shedding has
	// some other cause and a smaller encoder fixes nothing.
	minChangeFraction = 0.10

	// liveSettleWindow, liveUpWindow and maxLiveAdaptiveChanges replace
	// settleWindow, upWindow and maxAdaptiveChanges for an encoder that takes
	// a new bitrate while it runs (target.LiveBitRater, a simulator's simcap).
	// Each of those three is sized by what a relaunch costs, and a live change
	// costs none of it: no second encoder, no keyframe wait, no viewer resync.
	// downWindow does not change, because it is sized by how long it takes to
	// tell the link from a burst, which is the same whichever way the change
	// is applied.

	// liveSettleWindow is two sample intervals: the one the change landed in,
	// which mixes bytes from both rates, and the one after, in which the
	// encoder converges. simprobe -setbitrate measured VideoToolbox following
	// a new AverageBitRate within a second, 4 Mbps to 480-520 kbps and back
	// to 1.8-2.1 Mbps, with no config packet and no keyframe. Nothing is
	// disturbed that settleWindow exists to wait out.
	liveSettleWindow = 2 * time.Second

	// liveUpWindow is what a wrong probe upward costs when it cannot cost a
	// restart: at most downWindow of shedding before the next decision brings
	// it back. It stays longer than downWindow, so that a step up still needs
	// more evidence than a step down, but not six times longer. From the floor
	// to the 4 Mbps default is seven steps of half; at settleWindow+upWindow
	// that took 8.4 minutes, and at these values it takes about two.
	liveUpWindow = 15 * time.Second

	// maxLiveAdaptiveChanges and liveCapPeriod cap a live encoder at 24
	// changes in any 30 minutes, where a relaunched one gets six for the
	// whole session.
	//
	// With the relaunch cap, a single congestion episode spent it: 4 Mbps
	// down to the floor is one or two changes and back up is seven, so a
	// simulator stopped at 1.3-1.9 Mbps for the rest of the session.
	// Twenty-four is two full episodes with room to spare. The cap is still
	// needed, because a link sitting on the threshold hunts, and every cycle
	// of that is a step up followed by up to downWindow of shedding before
	// the step back down -- twelve cycles is two minutes of it.
	//
	// It rolls rather than lasting the session because a simulator session
	// has no reason to end: a phone left watching one through an afternoon
	// meets separate episodes an hour apart, and a lifetime cap spends itself
	// on the first ones and leaves the encoder wherever the last change put
	// it. Over 30 minutes the hunting bound is two minutes of shedding in
	// thirty, and a cycle is about 30s (2s settle, 10s shedding, 2s settle,
	// 15s clean), so twelve of them spend the cap in about six minutes and
	// the controller then holds for the rest of the period.
	maxLiveAdaptiveChanges = 24
	liveCapPeriod          = 30 * time.Minute

	// starvedAwaitFraction is the share of produced frames suppressed awaiting
	// a keyframe that marks the link as starved rather than merely lossy.
	//
	// Trap 27 excluded keyframe waits from the shed fraction, and that stays
	// right: one drop causes many waits, so counting them lets a single frame
	// argue for a change sixteen times over. What it missed is that a wait
	// sustained across a whole downWindow is not the echo of one drop. The
	// only thing that clears a wait is a keyframe landing, and a keyframe is
	// the largest frame there is -- so a wait that has not cleared in ten
	// seconds is the encoder stating that its own smallest useful unit does
	// not fit. Measured at 0.87 on a link shaped to 400 kbps against a 4 Mbps
	// encoder; a healthy run reads 0.
	starvedAwaitFraction = 0.50

	// undercarriedGoodputFraction and undercarriedAwaitFraction mark the gap
	// between the shed and starved gates: a link carrying under half of what
	// the encoder is set to, while the viewer waits on keyframes for a
	// meaningful share of the window. Two cellular sessions sat there for
	// their whole length: Pixel_10 on 20 October carried 340-667 kbps of 4
	// Mbps with shedFrac 0.008-0.079 and awaitFrac 0.28-0.44, and a simulator
	// on 21 October carried 0.8-1.1 Mbps of 4 with awaitFrac 0.35-0.49. Both
	// decoded under 10fps and neither changed the encoder. Goodput alone is
	// not enough, because a static screen reads far under the bitrate with
	// every frame delivered; its awaitFrac is 0, as a healthy run's is.
	undercarriedGoodputFraction = 0.50
	undercarriedAwaitFraction   = 0.20

	// upAwaitFraction is the most keyframe waiting a window may hold and
	// still count as clean enough to probe upward. One drop and the sixteen
	// frames behind it is 0.03 of a 60fps window.
	upAwaitFraction = 0.05

	// minWindowFrameRate is the fewest frames a second a window must average
	// before any gate may act on it. A near-idle screen sends two or three
	// frames a second, so the one keyframe a viewer awaits on attach is a
	// quarter to a half of the window, and goodput is the size of a few small
	// frames rather than what the link can carry. Two phone sessions on 26
	// October were cut to the floor over direct links that way: 26 frames in
	// 12s read as undercarried, 22 in 10s as starved. A starved link with
	// anything moving on screen still produces at the encoder's rate, so this
	// hides no real starvation. The idle test's 10fps clears it.
	minWindowFrameRate = 6
)

// bitrateWindows is the part of the policy that depends on what a change
// costs. Every threshold on the evidence itself is shared.
type bitrateWindows struct {
	settle     time.Duration
	up         time.Duration
	maxChanges int
	// capPeriod is how far back maxChanges counts, or zero for the whole
	// session.
	capPeriod time.Duration
}

var (
	relaunchWindows = bitrateWindows{settle: settleWindow, up: upWindow, maxChanges: maxAdaptiveChanges}
	liveWindows     = bitrateWindows{settle: liveSettleWindow, up: liveUpWindow, maxChanges: maxLiveAdaptiveChanges, capPeriod: liveCapPeriod}
)

// BitrateSample is one interval's worth of session counters.
type BitrateSample struct {
	// Frames is how many frames the encoder produced in the interval.
	Frames uint64
	// Shed is how many were discarded for pacer or staleness reasons.
	// Keyframe waits are deliberately excluded: they are the consequence of a
	// shed, not independent evidence, and counting them would let one dropped
	// frame argue for a change sixteen times over.
	Shed uint64
	// AwaitKey is how many were suppressed because the viewer was waiting for
	// a keyframe. It is kept apart from Shed for trap 27's reason and read
	// only by the starvation gate, which needs it sustained across the whole
	// window rather than counted frame by frame.
	AwaitKey uint64
	// Bytes is what actually reached the transport, which is the only
	// measurement of what the link will carry.
	Bytes uint64
}

// BitrateGate names the branch a decision came out of.
//
// It exists because the controller had never been observed acting, and could
// not be: bitrate.go and relaunch.go wrote nothing to the trace, so a run that
// evaluated the window and declined is byte-identical to one that never
// reached the window at all. Naming the branch is what separates them. This is
// the same unmeasured-branch shape as traps 19, 22, 27 and 29.
type BitrateGate string

const (
	// GateAnchor is the first sample, which describes an interval ending
	// before the window opened and is discarded.
	GateAnchor BitrateGate = "anchor"
	// GateSettling is inside settleWindow after a change.
	GateSettling BitrateGate = "settling"
	// GateCapped is the change cap already spent: maxAdaptiveChanges for
	// the session, or maxLiveAdaptiveChanges within liveCapPeriod.
	GateCapped BitrateGate = "capped"
	// GateWindow is a window still shorter than downWindow.
	GateWindow BitrateGate = "window"
	// GateNoFrames is a window the encoder produced nothing in.
	GateNoFrames BitrateGate = "noFrames"
	// GateSparse is a window with too few frames for goodput to measure the
	// link, so its keyframe waits are not evidence either.
	GateSparse BitrateGate = "sparse"
	// GateLinkCarried is shedding above the threshold on a link that
	// nonetheless carried everything asked of it, so bandwidth is not what is
	// missing and re-encoding smaller would fix nothing.
	GateLinkCarried BitrateGate = "linkCarried"
	// GateTooSmall is a move real enough to consider but too small to be
	// worth the encoder restart it costs.
	GateTooSmall BitrateGate = "tooSmall"
	// GateUpWindow is a clean window that has not yet lasted upWindow.
	GateUpWindow BitrateGate = "upWindow"
	// GateInconclusive is a window neither bad enough to act on nor clean
	// enough to count as recovery, so it starts over.
	GateInconclusive BitrateGate = "inconclusive"
	// GateNoViewer is an interval with no viewer attached throughout. There
	// is nothing measuring the link, and a zero reads identically to a link
	// that carried nothing, so the window restarts instead.
	GateNoViewer BitrateGate = "noViewer"
	// GateStarved is a window spent mostly awaiting a keyframe that never
	// arrived, which is a link too slow to carry one.
	GateStarved BitrateGate = "starved"
	// GateUndercarried is a link carrying well under half the bitrate while
	// the viewer spends a meaningful share of the window awaiting keyframes,
	// below what either the shed or the starved gate reads as bad.
	GateUndercarried BitrateGate = "undercarried"
	// GateDown and GateUp are the two decisions.
	GateDown BitrateGate = "down"
	GateUp   BitrateGate = "up"
)

// BitrateReport is one interval's decision together with the evidence behind
// it. It comes out of Observe rather than being read off the adapter
// afterwards, because several branches reset the window before returning and a
// caller reading a.acc would see the cleared value, not the one decided on.
type BitrateReport struct {
	// Gate names the branch this interval came out of.
	Gate BitrateGate
	// Current is the bitrate the encoder is running at.
	Current int
	// From and To are set only when a change was committed.
	From, To int
	// Frames, Shed and Bytes are the window's accumulated totals, not this
	// one interval's.
	Frames, Shed, Bytes uint64
	ShedFrac            float64
	// AwaitKey and AwaitFrac are the window's keyframe waits. They are
	// reported whether or not the starvation gate fired, because a run that
	// evaluated them and declined has to be distinguishable from one that
	// never saw them (traps 19, 22, 27, 29).
	AwaitKey   uint64
	AwaitFrac  float64
	GoodputBps int
	WindowMs   int64
	// ActiveMs is how much of WindowMs the encoder was producing in. Goodput
	// is measured over this, not over WindowMs: the two diverge exactly when
	// the screen goes idle, which is when the difference costs the most.
	ActiveMs int64
	Changes  int
	// Live is set when the change was applied to the running encoder rather
	// than by relaunching it.
	Live bool
	// MaxSize is the longest edge a live change moved the encoder to
	// (resizeForBitRate), 0 when it did not ask for one.
	MaxSize int
	// Err is set when the relaunch a decision asked for failed. A decision
	// that was made and then did not happen is not the same as one that was
	// never made, and without this the trace cannot tell them apart.
	Err string
}

// BitrateAdapter decides when the encoder is producing more than the link can
// deliver. It is pure: it holds no clock and touches no stream, so the policy
// can be tested without an encoder.
type BitrateAdapter struct {
	configured int
	current    int
	changes    int
	windows    bitrateWindows

	// changedAt holds when each change counted against the cap was made. It
	// is pruned to windows.capPeriod, so it holds at most maxChanges entries.
	changedAt []time.Time

	windowStart time.Time
	lastChange  time.Time
	acc         BitrateSample

	// lastSample and active measure how much of the window the encoder was
	// actually producing in. Goodput divided by the whole window reads every
	// idle second as a second the link carried nothing, and an idle screen
	// produces nothing for as long as nobody touches it (trap 20).
	lastSample time.Time
	active     time.Duration
}

// NewBitrateAdapter returns an adapter that will never exceed configured, for
// an encoder that changes bitrate by relaunching.
func NewBitrateAdapter(configured int) *BitrateAdapter {
	return &BitrateAdapter{configured: configured, current: configured, windows: relaunchWindows}
}

// NewLiveBitrateAdapter returns an adapter for an encoder that takes a new
// bitrate while it runs. It decides on the same evidence as NewBitrateAdapter
// and waits less around each change, because a change costs the viewer nothing.
func NewLiveBitrateAdapter(configured int) *BitrateAdapter {
	return &BitrateAdapter{configured: configured, current: configured, windows: liveWindows}
}

// Current reports the bitrate the adapter believes the encoder is running at.
func (a *BitrateAdapter) Current() int { return a.current }

// Observe folds one interval in and returns the bitrate the encoder should be
// relaunched at, or zero to leave it alone.
func (a *BitrateAdapter) Observe(now time.Time, s BitrateSample) (int, BitrateReport) {
	if a.windowStart.IsZero() {
		// Anchor the window and discard this sample. It describes the
		// interval ending at windowStart, which is before the window opened,
		// and folding it in would put N+1 samples of bytes over N seconds of
		// elapsed time -- a goodput estimate overstated by a tenth at a
		// ten-second window, biased in the direction that hides congestion.
		a.windowStart = now
		a.lastSample = now
		return 0, a.report(now, GateAnchor)
	}
	// An interval counts toward active time only if the encoder produced in
	// it. The interval is measured rather than assumed to be one second, so a
	// late or coalesced sample is charged what it actually spanned.
	if s.Frames > 0 && !a.lastSample.IsZero() {
		if dt := now.Sub(a.lastSample); dt > 0 {
			a.active += dt
		}
	}
	a.lastSample = now
	a.acc.Frames += s.Frames
	a.acc.Shed += s.Shed
	a.acc.AwaitKey += s.AwaitKey
	a.acc.Bytes += s.Bytes

	if !a.lastChange.IsZero() && now.Sub(a.lastChange) < a.windows.settle {
		// Keep the window anchored to the end of the settle period rather than
		// to the change, so the first measured window is a full one.
		rep := a.report(now, GateSettling)
		a.reset(now)
		return 0, rep
	}
	if a.recentChanges(now) >= a.windows.maxChanges {
		return 0, a.report(now, GateCapped)
	}

	elapsed := now.Sub(a.windowStart)
	if elapsed < downWindow {
		return 0, a.report(now, GateWindow)
	}
	if a.acc.Frames == 0 {
		return 0, a.report(now, GateNoFrames)
	}
	if float64(a.acc.Frames) < minWindowFrameRate*elapsed.Seconds() {
		rep := a.report(now, GateSparse)
		a.reset(now)
		return 0, rep
	}
	shedFrac := float64(a.acc.Shed) / float64(a.acc.Frames)
	awaitFrac := float64(a.acc.AwaitKey) / float64(a.acc.Frames)

	if awaitFrac >= starvedAwaitFraction {
		// Aim at the measured goodput, with a halving as the ceiling on the
		// step rather than the step itself.
		//
		// Goodput used to be unusable here: a frame the transport reset
		// mid-flight returned nil from SendFrame, so it was credited as
		// delivered, and during a run shaped to 400 kbps this read 1.56 Mbps
		// while nothing but parameter sets arrived. The abandoned frames are
		// counted as shed now, so what is left is what the link carried.
		//
		// Aiming at it matters because starvation is not a near miss. On that
		// run honest goodput was about 1 kbps, so this reaches the floor in one
		// step where halving took four -- at 22s a step, that was 85 of the 90
		// impaired seconds spent still oversubscribed. The halving stays as a
		// cap: goodput is measured over intervals the encoder produced in, and
		// a short window with one large frame in it can read above the current
		// rate, which must not turn a starved window into an increase.
		next := a.goodput()
		if half := a.current / 2; next > half {
			next = half
		}
		if next < minAdaptiveBitRate {
			next = minAdaptiveBitRate
		}
		return a.commit(now, next, GateStarved)
	}

	if shedFrac >= downShedFraction {
		// Goodput is what the link actually carried, which is a direct
		// measurement rather than a guess at a multiplier.
		goodput := a.goodput()
		if goodput <= 0 {
			// No interval in this window both produced frames and spanned any
			// time, so there is nothing measuring the link. Declining is the
			// honest answer; a zero here would relaunch at the floor.
			rep := a.report(now, GateNoFrames)
			a.reset(now)
			return 0, rep
		}
		if goodput >= a.current {
			// The link carried everything the encoder was asked for, so the
			// shedding has some other cause -- jitter, a stalled write, a
			// viewer awaiting a keyframe. Re-encoding smaller does not help
			// any of those, and costs a restart.
			rep := a.report(now, GateLinkCarried)
			a.reset(now)
			return 0, rep
		}
		next := goodput
		if next < minAdaptiveBitRate {
			next = minAdaptiveBitRate
		}
		return a.commit(now, next, GateDown)
	}

	if awaitFrac >= undercarriedAwaitFraction {
		if goodput := a.goodput(); goodput > 0 &&
			float64(goodput) < float64(a.current)*undercarriedGoodputFraction {
			// Aim at what the link carried, as the other two gates do. It is
			// under half the current rate by the condition, so it cannot be
			// an increase.
			next := goodput
			if next < minAdaptiveBitRate {
				next = minAdaptiveBitRate
			}
			return a.commit(now, next, GateUndercarried)
		}
	}

	if elapsed < a.windows.up {
		return 0, a.report(now, GateUpWindow)
	}
	if shedFrac <= upShedFraction && awaitFrac <= upAwaitFraction && a.current < a.configured {
		next := a.current * 3 / 2
		if next > a.configured {
			next = a.configured
		}
		return a.commit(now, next, GateUp)
	}
	// A window that was neither bad enough to act on nor clean enough to
	// count as recovery starts over, so evidence does not accumulate forever.
	rep := a.report(now, GateInconclusive)
	a.reset(now)
	return 0, rep
}

// report snapshots the window as it stands, for the gate just decided. It must
// be called before any reset, which is why every branch that resets builds the
// report first.
func (a *BitrateAdapter) report(now time.Time, gate BitrateGate) BitrateReport {
	rep := BitrateReport{
		Gate:     gate,
		Current:  a.current,
		Frames:   a.acc.Frames,
		Shed:     a.acc.Shed,
		AwaitKey: a.acc.AwaitKey,
		Bytes:    a.acc.Bytes,
		Changes:  a.changes,
	}
	if !a.windowStart.IsZero() {
		rep.WindowMs = now.Sub(a.windowStart).Milliseconds()
	}
	if rep.Frames > 0 {
		rep.ShedFrac = float64(rep.Shed) / float64(rep.Frames)
		rep.AwaitFrac = float64(rep.AwaitKey) / float64(rep.Frames)
	}
	rep.ActiveMs = a.active.Milliseconds()
	rep.GoodputBps = a.goodput()
	return rep
}

// commit accepts a proposed bitrate if the move is large enough to be worth an
// encoder restart, and resets the window either way.
func (a *BitrateAdapter) commit(now time.Time, next int, gate BitrateGate) (int, BitrateReport) {
	if next <= 0 || a.current <= 0 {
		rep := a.report(now, GateTooSmall)
		a.reset(now)
		return 0, rep
	}
	delta := next - a.current
	if delta < 0 {
		delta = -delta
	}
	if float64(delta)/float64(a.current) < minChangeFraction {
		rep := a.report(now, GateTooSmall)
		a.reset(now)
		return 0, rep
	}
	from := a.current
	a.current = next
	a.changes++
	a.changedAt = append(a.changedAt, now)
	a.lastChange = now
	rep := a.report(now, gate)
	rep.From, rep.To = from, next
	a.reset(now)
	return next, rep
}

// recentChanges counts the changes inside the cap period, dropping the ones
// that have aged out of it.
func (a *BitrateAdapter) recentChanges(now time.Time) int {
	if a.windows.capPeriod <= 0 {
		return a.changes
	}
	keep := a.changedAt[:0]
	for _, t := range a.changedAt {
		if now.Sub(t) < a.windows.capPeriod {
			keep = append(keep, t)
		}
	}
	a.changedAt = keep
	return len(keep)
}

// Reset discards the current window. The controller's evidence is only
// meaningful while it is measuring something, so a caller that cannot supply a
// sample says so here rather than feeding a zero, which reads exactly like a
// link that carried nothing.
func (a *BitrateAdapter) Reset(now time.Time) { a.reset(now) }

func (a *BitrateAdapter) reset(now time.Time) {
	a.windowStart = now
	a.lastSample = now
	a.active = 0
	a.acc = BitrateSample{}
}

// goodput is the rate the link carried over the time the encoder was producing,
// which is the rate a relaunch should aim at. Dividing by the whole window
// instead understates it by the idle fraction, and bitrate.go uses this figure
// directly as the new encoder bitrate.
func (a *BitrateAdapter) goodput() int {
	if a.active <= 0 {
		return 0
	}
	return int(float64(a.acc.Bytes) * 8 / a.active.Seconds())
}
