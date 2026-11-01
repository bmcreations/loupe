package session

import (
	"math"
	"time"
)

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

	// The starved and undercarried gates also need their waits spread over
	// time: the share of the window's active time that held a wait must
	// reach the same fraction as the frame share. At a steady frame rate the
	// two agree. On a near-idle screen they do not, because a burst of
	// frames behind one late keyframe outnumbers the trickle around it. The
	// Flipcash simulator on 30 October read awaitFrac 0.62 at 2fps with the
	// waits in 2 of 11 seconds, and the starved gate cut 4 Mbps to 256 kbps.

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

	// busyFrameRate, minBusyTime and minBusyFraction decide whether a window
	// measured the link or measured the screen. Goodput is bytes over the time
	// the encoder produced in, but an encoder producing two frames a second of
	// a static screen is "producing", and what it sends then is what the
	// screen needed, not what the link can carry.
	//
	// The 12:15:50 cut in the 5G trace: ~10s of static screen at ~2 frames a
	// second and 5-150 kbps actually sent, then a burst and a short keyframe
	// wait after an abandon. The undercarried gate read goodput 535 kbps
	// against a 4 Mbps encoder and cut to it, and the size ladder took the
	// phone from 664x1440 to 248x544. The link was carrying 2.1 Mbps at 60fps
	// at the time.
	//
	// An interval is busy when it produced at least busyFrameRate frames a
	// second. Moving content runs at the encoder's 30-60fps and the idle test's
	// 10fps still counts; a static screen sits at 2-3. Goodput is taken over
	// busy intervals only, and only once there are minBusyTime of them making
	// up at least minBusyFraction of the active time. Below that there is no
	// capacity measurement, and the gates that aim at goodput decline.
	busyFrameRate   = 10
	minBusyTime     = 2 * time.Second
	minBusyFraction = 0.5

	// recoverRTTFactor and recoverRTTSlack define a link that is clearly
	// healthy: the current round trip (not the smoothed one, which lags a
	// recovery by seconds) within the larger of recoverRTTFactor times the
	// path baseline or the baseline plus recoverRTTSlack. The slack keeps a
	// 5ms LAN from reading a 12ms blip as congestion.
	recoverRTTFactor = 2.0
	recoverRTTSlack  = 40 * time.Millisecond

	// recoverWindow and liveRecoverWindow are how long the link must stay
	// clearly healthy, with no capacity shed (pacer, stale, abandon), before
	// the controller steps up without waiting for upWindow. A relaunch costs a
	// restart, so it waits longer than a live change does.
	//
	// A trace of a Tailscale relay slowing for ~18s left a session at the
	// floor for the 60s upWindow after the link was back, with the change cap
	// possibly spent on the way down.
	recoverWindow     = 10 * time.Second
	liveRecoverWindow = 5 * time.Second

	// recoverHold is how long fast recovery is off after a step down that
	// followed a fast-recovery step up closely. It is the hysteresis: a link
	// that cannot hold the rate fast recovery chose is left to the slow path.
	recoverHold = 2 * time.Minute

	// recoverHuntWindow is how soon after a fast step up a step down counts
	// as the step up having been wrong.
	recoverHuntWindow = 45 * time.Second

	// delayCutFactor and delayCutSlack define a queueing link: the current
	// round trip (not the smoothed one) above the larger of delayCutFactor
	// times the path baseline or the baseline plus delayCutSlack. The slack
	// keeps a 5ms LAN from reading a 15ms blip as a queue.
	//
	// The 12:38 trace on 5G: base ~100ms, the phone carried ~1 Mbps against a
	// 4 Mbps encoder, rttNow went 100 -> 507 and 570ms, and the windowed
	// gates cut only after ~10s, during which 3 of 18 frames were delivered.
	// A queue building is visible in the round trip long before it is in the
	// shed counters.
	delayCutFactor = 2.0
	delayCutSlack  = 80 * time.Millisecond

	// delayCutDwell is how long the round trip must stay above that line
	// before it counts, so a single spike (5G reads 400ms for a moment)
	// does not cut. Samples arrive once a second, so in practice a cut needs
	// two consecutive samples over the line.
	delayCutDwell = 500 * time.Millisecond

	// delayCutGoodput aims a delay cut at this share of what the link carried
	// while the round trip was over the line (a saturated link delivers its
	// capacity), leaving headroom for the queue to drain. delayCutBlind is
	// the multiplicative decrease when nothing measured that. delayCutMax is
	// the largest share of the current rate a delay cut may leave, so a
	// measurement at the current rate still cuts.
	delayCutGoodput = 0.8
	delayCutBlind   = 0.7
	delayCutMax     = 0.85

	// delayCutPeriod is how far back the delay-cut limit counts. These cuts
	// are outside maxAdaptiveChanges (a session that spent its changes on a
	// queue would stay blurry once the link was back), so they have their
	// own limit: bitrateWindows.maxDelayCuts per delayCutPeriod, which keeps
	// a relaunched encoder from restarting over and over.
	delayCutPeriod = 10 * time.Minute

	// lossDeliveredFraction is the share of produced frames a viewer must have
	// decoded for a sample to read as loss when the sample also holds a fresh
	// loss event (a stale shed or an abandon). lossDwell is how many
	// consecutive such samples cut. Samples are a second each, so one abandon
	// and the keyframe wait behind it, which is one sample, never cuts.
	//
	// The 13:23 trace on 5G: round trip flat at 100-130ms, so the delay gate
	// never fired, while the link policed instead of queueing. Health read
	// "delivered 23 of 60", "30 of 58", "26 of 60" for seconds on end, each
	// second carrying a stale shed or an abandon and the 13-33 keyframe-wait
	// frames behind it. The wait frames are never sent, so the cut reads the
	// event, not the wait, as the loss: a second path counts what was sent
	// whole and not decoded, plus abandons (lossSentFraction).
	lossDeliveredFraction = 0.7
	lossDwell             = 2
	lossSentFraction      = 0.3
	// lossMinFrames is the fewest frames a reading must cover to be judged.
	lossMinFrames = 10

	// lossCutGoodput, lossCutBlind and lossCutMax are delayCutGoodput,
	// delayCutBlind and delayCutMax for the loss cut.
	lossCutGoodput = 0.8
	lossCutBlind   = 0.7
	lossCutMax     = 0.85

	// floorSizeHits is how many consecutive undercarried windows at the
	// bitrate floor step the picture down.
	floorSizeHits = 2

	// awaitHealthyGrace is how long the link must have read healthy before
	// keyframe waits stop counting toward the starved and undercarried gates.
	awaitHealthyGrace = 3 * time.Second
)

// bitrateWindows is the part of the policy that depends on what a change
// costs. Every threshold on the evidence itself is shared.
type bitrateWindows struct {
	settle     time.Duration
	up         time.Duration
	maxChanges int
	// recover is the healthy stretch fast recovery needs.
	recover time.Duration
	// capPeriod is how far back maxChanges counts, or zero for the whole
	// session.
	capPeriod time.Duration
	// maxDelayCuts is how many delay-based cuts (GateDelay) fit in
	// delayCutPeriod. A relaunch is a visible restart, so it gets fewer.
	maxDelayCuts int
	// sizeAtFloor lets the undercarried gate ask for a smaller picture once
	// the bitrate cannot go lower: the size ladder only exists on a stream
	// that resizes live.
	sizeAtFloor bool
}

var (
	relaunchWindows = bitrateWindows{settle: settleWindow, up: upWindow, maxChanges: maxAdaptiveChanges, recover: recoverWindow, maxDelayCuts: 3}
	liveWindows     = bitrateWindows{settle: liveSettleWindow, up: liveUpWindow, maxChanges: maxLiveAdaptiveChanges, recover: liveRecoverWindow, capPeriod: liveCapPeriod, maxDelayCuts: 8, sizeAtFloor: true}
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
	// RTT and BaseRTT are the slowest viewer's current round trip and its
	// path baseline, or zero when no viewer had a fresh reading. They let the
	// controller tell a recovered link from a slow one without waiting for
	// shed counters or a smoothed rtt to say so.
	RTT, BaseRTT time.Duration
	// Delivery is the newest client report's delivery reading, when one
	// arrived in the interval (Valid is false otherwise).
	Delivery DeliveryReading
}

// DeliveryReading is what a viewer's newest report says about delivery,
// paired with the server's counters for the same second (httpapi's
// healthTracker does the pairing). Counts are rates per second.
type DeliveryReading struct {
	Valid bool
	// Produced is frames the encoder made, Delivered the frames the client
	// decoded, Allowance the shortfall that is report-clock phase, not loss.
	Produced, Delivered, Allowance float64
	// Sent is frames handed to the transport whole: produced less every shed,
	// keyframe waits included, since those were never sent.
	Sent float64
	// Events are frames lost to a stale shed or an abandon (each starts a
	// keyframe wait), Abandons the part of them reset mid-flight.
	Events, Abandons float64
}

// DeliveredFraction is decoded over produced with the phase allowance
// forgiven, or 1 when the reading covers too few frames to judge.
func (r DeliveryReading) DeliveredFraction() float64 {
	if r.Produced < lossMinFrames {
		return 1
	}
	return math.Min(1, (r.Delivered+r.Allowance)/r.Produced)
}

// SentLossFraction is what the client failed to receive of what was sent,
// plus abandons, over everything attempted. A keyframe wait is not in either
// side: one abandon cascading into thirty waits counts once.
func (r DeliveryReading) SentLossFraction() float64 {
	attempted := r.Sent + r.Abandons
	if attempted < lossMinFrames {
		return 0
	}
	failed := r.Abandons + math.Max(0, r.Sent-r.Delivered-r.Allowance)
	return failed / attempted
}

// lossy reports a reading that argues for a smaller encoder: few frames
// decoded with a fresh loss event behind it, or much of what was sent not
// decoded.
func (r DeliveryReading) lossy() bool {
	if !r.Valid || r.Produced < lossMinFrames {
		return false
	}
	return (r.DeliveredFraction() < lossDeliveredFraction && r.Events >= 0.5) ||
		r.SentLossFraction() > lossSentFraction
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
	// GateIdle is a window whose busy time was too short or too small a share
	// of its active time for goodput to measure the link (see busyFrameRate).
	GateIdle BitrateGate = "idle"
	// GateRecover is a step up taken early because the link has been clearly
	// healthy for recoverWindow. It does not count against the change cap.
	GateRecover BitrateGate = "recover"
	// GateDelay is a cut taken because the current round trip stayed well
	// above baseline: a queue building on the link. It does not count against
	// the change cap and does not move the picture size.
	GateDelay BitrateGate = "delay"
	// GateLoss is a cut taken because the viewer decoded under
	// lossDeliveredFraction of what was produced, with a fresh stale shed or
	// abandon behind it, for lossDwell samples while the round trip stayed
	// flat: a link that drops rather than queues. Like GateDelay it does not
	// count against the change cap and does not move the picture size.
	GateLoss BitrateGate = "loss"
	// GateSharpen is a one-frame quality boost after motion stopped
	// (sharpen.go). It changes no steady rate.
	GateSharpen BitrateGate = "sharpen"
	// GateSharpenSkipped is a still period after motion that did not sharpen,
	// with the reason in Detail ("why"). Reported once per still period.
	GateSharpenSkipped BitrateGate = "sharpenSkipped"
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
	// AwaitMs is how much of ActiveMs held a keyframe wait.
	AwaitMs int64
	// BusyMs is how much of ActiveMs produced at busyFrameRate or more, the
	// time GoodputBps is measured over. GoodputBps is 0 when it is too little.
	BusyMs  int64
	Changes int
	// Live is set when the change was applied to the running encoder rather
	// than by relaunching it.
	Live bool
	// MaxSize is the longest edge a live change moved the encoder to
	// (resizeForBitRate), 0 when it did not ask for one.
	MaxSize int
	// Boost is the rate a sharpen raised the encoder to for one frame (GateSharpen).
	Boost int
	// Err is set when the relaunch a decision asked for failed. A decision
	// that was made and then did not happen is not the same as one that was
	// never made, and without this the trace cannot tell them apart.
	Err string
	// Detail is the inputs behind a decision the gate name alone does not
	// carry (a delay or loss cut, a sharpen, a skipped sharpen). The manager
	// copies it into the trace record.
	Detail map[string]any
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

	// busy and busyBytes are the part of active in which the encoder produced
	// at busyFrameRate or more, and the bytes sent in it. See goodput.
	busy      time.Duration
	busyBytes uint64

	// rttOKFor is how long the current rtt has read near baseline whatever
	// was shed, where healthyFor also needs nothing shed. A keyframe wait
	// that follows our own abandon reads healthyFor 0 because the abandon is a
	// shed, so healthyFor cannot tell the wait from a starved link.
	rttOKFor time.Duration

	// healthyFor is how long the link has read clearly healthy without
	// interruption (see linkHealthy), and healthyAt is when it was last
	// updated.
	healthyFor time.Duration
	healthyAt  time.Time
	// lastRecover is when fast recovery last stepped up, and holdUntil is when
	// it may next (see recoverHold).
	lastRecover time.Time
	holdUntil   time.Time
	// awaiting is how much of active held a keyframe wait.
	awaiting time.Duration

	// badSince, badBytes and badBusy describe the current run of intervals
	// whose round trip was over the delay line (delayBad): when it began, and
	// the bytes and time of its busy intervals, which measure what the link
	// carried while it queued. badSince is zero outside a run.
	badSince time.Time
	badBytes uint64
	badBusy  time.Duration
	// delayAt holds when each delay cut was made, pruned to delayCutPeriod.
	delayAt []time.Time
	// lossRun counts consecutive lossy samples, and lossBytes and lossBusy
	// the bytes and time of the run's intervals (every one is busy). lossAt
	// holds when each loss cut was made, pruned to delayCutPeriod.
	lossRun   int
	lossBytes uint64
	lossBusy  time.Duration
	lossAt    []time.Time
	// floorHits counts consecutive undercarried windows that could not lower
	// the bitrate because it was at its floor.
	floorHits int
	// interval is the span of the sample being folded in.
	interval time.Duration
	// afterDelay is set by a delay cut and cleared by the next step up. While
	// set, an interval with keyframe waits is not healthy, so recovery needs
	// delivery to have resumed as well as the round trip to have come back.
	afterDelay bool
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
			if float64(s.Frames) >= busyFrameRate*dt.Seconds() {
				a.busy += dt
				a.busyBytes += s.Bytes
			}
			if s.AwaitKey > 0 {
				a.awaiting += dt
			}
		}
	}
	a.interval = now.Sub(a.lastSample)
	a.lastSample = now
	a.acc.Frames += s.Frames
	a.acc.Shed += s.Shed
	a.acc.AwaitKey += s.AwaitKey
	a.acc.Bytes += s.Bytes
	a.trackHealth(now, s)

	if !a.lastChange.IsZero() && now.Sub(a.lastChange) < a.windows.settle {
		// Keep the window anchored to the end of the settle period rather than
		// to the change, so the first measured window is a full one.
		rep := a.report(now, GateSettling)
		a.clearBad()
		a.clearLoss()
		a.reset(now)
		return 0, rep
	}
	// A queue building on the link is answered at once, ahead of the cap and
	// the windows. It runs after the settle check so a cut is judged on
	// intervals measured at the new rate.
	if next, rep := a.delayCut(now, s); next != 0 {
		return next, rep
	}
	// Then loss: the round trip is flat but the viewer is not decoding.
	if next, rep := a.lossCut(now, s); next != 0 {
		return next, rep
	}
	// Fast recovery runs ahead of the cap: a session that spent its changes
	// on the way down must not stay blurry after the link is back. Its steps
	// are not counted (commitRecover), and recoverHold bounds hunting.
	if a.current < a.configured && a.healthyFor >= a.windows.recover && !now.Before(a.holdUntil) {
		next := a.current * 3 / 2
		if next > a.configured {
			next = a.configured
		}
		if rn, rep := a.commitRecover(now, next); rn != 0 {
			return rn, rep
		}
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

	// A keyframe wait is a consequence of an earlier drop, so it argues for a
	// smaller encoder only while the link still looks slow. Once the current
	// rtt is back near baseline the wait is draining the backlog the slow
	// stretch left, and cutting the bitrate for it punishes a link that has
	// already recovered.
	//
	// It reads the rtt alone, not healthyFor, which also needs nothing shed:
	// the abandon that started the wait is itself a shed, so after one the
	// healthy stretch is zero for exactly the wait it caused, and in the 5G
	// trace the precondition never held while health read degraded on that
	// wait.
	awaitIsSignal := a.rttOKFor < awaitHealthyGrace

	if awaitIsSignal && awaitFrac >= starvedAwaitFraction && a.awaitTimeFrac() >= starvedAwaitFraction &&
		a.goodput() > 0 {
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
			// Nothing measured the link: no interval both produced frames
			// and spanned any time, or too little of the window was busy for
			// its bytes to say what the link carries (busyFrameRate).
			// Declining is the honest answer; a zero here would relaunch at
			// the floor.
			gate := GateNoFrames
			if a.active > 0 {
				gate = GateIdle
			}
			rep := a.report(now, gate)
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

	if awaitIsSignal && awaitFrac >= undercarriedAwaitFraction && a.awaitTimeFrac() >= undercarriedAwaitFraction {
		if goodput := a.goodput(); goodput > 0 &&
			float64(goodput) < float64(a.current)*undercarriedGoodputFraction {
			// Aim at what the link carried, as the other two gates do. It is
			// under half the current rate by the condition, so it cannot be
			// an increase.
			next := goodput
			if next < minAdaptiveBitRate {
				next = minAdaptiveBitRate
			}
			if a.windows.sizeAtFloor && float64(a.current-next)/float64(a.current) < minChangeFraction {
				// The bitrate cannot go lower, so the picture is the only
				// lever left. One window at the floor could be a blip; the
				// second is not.
				a.floorHits++
				if a.floorHits >= floorSizeHits {
					return a.commitSize(now, GateUndercarried)
				}
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
	rep.AwaitMs = a.awaiting.Milliseconds()
	rep.BusyMs = a.busy.Milliseconds()
	rep.GoodputBps = a.goodput()
	return rep
}

// trackHealth extends or breaks the healthy stretch. An interval is healthy
// when a current rtt is known and near baseline and nothing was shed for
// capacity. An interval the encoder produced nothing in says nothing, so it
// neither extends nor breaks the stretch.
func (a *BitrateAdapter) trackHealth(now time.Time, s BitrateSample) {
	dt := now.Sub(a.healthyAt)
	a.healthyAt = now
	if s.Frames == 0 && s.Shed == 0 {
		return
	}
	if linkHealthy(s.RTT, s.BaseRTT) {
		if dt > 0 && dt < 5*time.Second {
			a.rttOKFor += dt
		}
	} else {
		a.rttOKFor = 0
	}
	if linkHealthy(s.RTT, s.BaseRTT) && s.Shed == 0 && !(a.afterDelay && s.AwaitKey > 0) {
		if dt > 0 && dt < 5*time.Second {
			a.healthyFor += dt
		}
		return
	}
	a.healthyFor = 0
}

// linkHealthy reports a current rtt within recoverRTTFactor of baseline, or
// within recoverRTTSlack of it.
func linkHealthy(rtt, base time.Duration) bool {
	if rtt <= 0 || base <= 0 {
		return false
	}
	limit := time.Duration(recoverRTTFactor * float64(base))
	if l := base + recoverRTTSlack; l > limit {
		limit = l
	}
	return rtt <= limit
}

// commitRecover steps up for fast recovery. Unlike commit it neither counts
// against the cap nor stays quiet on a too-small move that would loop: it
// restarts the healthy stretch so the next step needs a fresh one.
func (a *BitrateAdapter) commitRecover(now time.Time, next int) (int, BitrateReport) {
	if next <= a.current || float64(next-a.current)/float64(a.current) < minChangeFraction {
		return 0, BitrateReport{}
	}
	from := a.current
	a.current = next
	a.lastChange = now
	a.lastRecover = now
	rep := a.report(now, GateRecover)
	rep.From, rep.To = from, next
	a.reset(now)
	a.healthyFor = 0
	a.afterDelay = false
	return next, rep
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
	if next < from && !a.lastRecover.IsZero() && now.Sub(a.lastRecover) < recoverHuntWindow {
		// A step down soon after a fast step up: the link could not hold what
		// recovery chose. Leave further recovery to the slow upWindow path.
		a.holdUntil = now.Add(recoverHold)
	}
	a.current = next
	a.healthyFor = 0
	a.floorHits = 0
	if gate == GateUp {
		a.afterDelay = false
	}
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
func (a *BitrateAdapter) Reset(now time.Time) {
	a.reset(now)
	a.healthyFor = 0
	a.rttOKFor = 0
	a.clearBad()
	a.clearLoss()
}

func (a *BitrateAdapter) reset(now time.Time) {
	a.windowStart = now
	a.lastSample = now
	a.active = 0
	a.awaiting = 0
	a.busy = 0
	a.busyBytes = 0
	a.acc = BitrateSample{}
}

// awaitTimeFrac is the share of active time that held a keyframe wait.
func (a *BitrateAdapter) awaitTimeFrac() float64 {
	if a.active <= 0 {
		return 0
	}
	return a.awaiting.Seconds() / a.active.Seconds()
}

// goodput is the rate the link carried over the time the encoder was busy,
// which is the rate a relaunch should aim at, or zero when the window did not
// measure the link. Dividing by the whole window instead understates it by the
// idle fraction, and bitrate.go uses this figure directly as the new encoder
// bitrate.
//
// Active time is not enough either: a static screen produces a couple of tiny
// frames a second, so bytes over active time is what the screen needed rather
// than what the link can carry (see busyFrameRate). Only busy intervals count,
// and there must be enough of them to trust.
func (a *BitrateAdapter) goodput() int {
	if a.busy < minBusyTime || a.busy.Seconds() < minBusyFraction*a.active.Seconds() {
		return 0
	}
	return int(float64(a.busyBytes) * 8 / a.busy.Seconds())
}

// delayBad reports a current round trip over the delay line. Unknown readings
// are not bad: a cut needs evidence.
func delayBad(rtt, base time.Duration) bool {
	if rtt <= 0 || base <= 0 {
		return false
	}
	limit := time.Duration(delayCutFactor * float64(base))
	if l := base + delayCutSlack; l > limit {
		limit = l
	}
	return rtt > limit
}

func (a *BitrateAdapter) clearBad() {
	a.badSince = time.Time{}
	a.badBytes, a.badBusy = 0, 0
}

// delayCut decides the delay-based cut for one interval. It returns zero when
// the round trip is not over the line, has not stayed there for
// delayCutDwell, or the cut limit is spent.
//
// It aims at what the link carried during the run (a saturated link delivers
// its capacity), else a multiplicative decrease.
// The cut never moves the picture size (the caller skips the size ladder for
// GateDelay), and is not counted against the change cap.
func (a *BitrateAdapter) delayCut(now time.Time, s BitrateSample) (int, BitrateReport) {
	// A queue only argues for a smaller encoder while the encoder is the one
	// filling it: a near-idle screen's round trip says nothing about our rate.
	busy := s.Frames > 0 && a.interval > 0 && float64(s.Frames) >= busyFrameRate*a.interval.Seconds()
	if !busy || !delayBad(s.RTT, s.BaseRTT) {
		a.clearBad()
		return 0, BitrateReport{}
	}
	if a.badSince.IsZero() {
		// The run began somewhere in the interval that ended now; counting it
		// from the previous sample's end is the conservative reading of the
		// dwell, so one bad sample alone never satisfies it.
		a.badSince = now
	}
	// Only intervals in which every frame was offered to the link measure
	// what it carries; a keyframe wait sends almost nothing by design.
	if s.AwaitKey == 0 {
		a.badBytes += s.Bytes
		a.badBusy += a.interval
	}
	if now.Sub(a.badSince) < delayCutDwell {
		return 0, BitrateReport{}
	}
	cutoff := now.Add(-delayCutPeriod)
	keep := a.delayAt[:0]
	for _, t := range a.delayAt {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	a.delayAt = keep
	if len(a.delayAt) >= a.windows.maxDelayCuts || a.current <= minAdaptiveBitRate {
		return 0, BitrateReport{}
	}

	var next int
	switch {
	case a.badBusy > 0:
		next = int(delayCutGoodput * float64(a.badBytes) * 8 / a.badBusy.Seconds())
	default:
		next = int(delayCutBlind * float64(a.current))
	}
	if max := int(delayCutMax * float64(a.current)); next > max {
		next = max
	}
	if next < minAdaptiveBitRate {
		next = minAdaptiveBitRate
	}
	if float64(a.current-next)/float64(a.current) < minChangeFraction {
		return 0, BitrateReport{}
	}

	a.delayAt = append(a.delayAt, now)
	runMs, runBytes := a.badBusy.Milliseconds(), a.badBytes
	since := now.Sub(a.badSince).Milliseconds()
	n, rep := a.commitCut(now, next, GateDelay)
	rep.Detail = map[string]any{
		"rttMs": s.RTT.Milliseconds(), "baseMs": s.BaseRTT.Milliseconds(),
		"badForMs": since, "runMs": runMs, "runBytes": runBytes,
	}
	return n, rep
}

// commitCut carries out a delay or loss cut: not counted against the change
// cap, no size change, and recovery waits for delivery as well as the round
// trip.
func (a *BitrateAdapter) commitCut(now time.Time, next int, gate BitrateGate) (int, BitrateReport) {
	from := a.current
	if !a.lastRecover.IsZero() && now.Sub(a.lastRecover) < recoverHuntWindow {
		a.holdUntil = now.Add(recoverHold)
	}
	a.current = next
	a.healthyFor = 0
	a.rttOKFor = 0
	a.afterDelay = true
	a.lastChange = now
	rep := a.report(now, gate)
	rep.From, rep.To = from, next
	a.clearBad()
	a.clearLoss()
	a.reset(now)
	return next, rep
}

// commitSize reports a decision that moves the picture and not the bitrate:
// From and To are both the current rate. It counts against the change cap.
func (a *BitrateAdapter) commitSize(now time.Time, gate BitrateGate) (int, BitrateReport) {
	a.floorHits = 0
	a.changes++
	a.changedAt = append(a.changedAt, now)
	a.lastChange = now
	rep := a.report(now, gate)
	rep.From, rep.To = a.current, a.current
	a.reset(now)
	return a.current, rep
}

func (a *BitrateAdapter) clearLoss() {
	a.lossRun, a.lossBytes, a.lossBusy = 0, 0, 0
}

// lossCut decides the loss-based cut for one interval. The round trip is
// flat on a link that polices rather than queues, so the delay gate stays
// quiet while retransmits miss the frame deadline, abandons force keyframes
// and the frames behind them are shed.
//
// It needs lossDwell consecutive lossy readings while busy and not queueing.
// An interval without a fresh reading neither extends nor ends the run. It
// aims at lossCutGoodput of what the run's intervals carried, else a
// multiplicative decrease, never moves the picture size, and is not counted
// against the change cap. Its rate limit is the delay cut's, kept separately.
func (a *BitrateAdapter) lossCut(now time.Time, s BitrateSample) (int, BitrateReport) {
	busy := s.Frames > 0 && a.interval > 0 && float64(s.Frames) >= busyFrameRate*a.interval.Seconds()
	if !busy || delayBad(s.RTT, s.BaseRTT) {
		a.clearLoss()
		return 0, BitrateReport{}
	}
	if !s.Delivery.Valid {
		if a.lossRun > 0 {
			a.lossBytes += s.Bytes
			a.lossBusy += a.interval
		}
		return 0, BitrateReport{}
	}
	if !s.Delivery.lossy() {
		a.clearLoss()
		return 0, BitrateReport{}
	}
	a.lossRun++
	a.lossBytes += s.Bytes
	a.lossBusy += a.interval
	if a.lossRun < lossDwell {
		return 0, BitrateReport{}
	}
	cutoff := now.Add(-delayCutPeriod)
	keep := a.lossAt[:0]
	for _, t := range a.lossAt {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	a.lossAt = keep
	if len(a.lossAt) >= a.windows.maxDelayCuts || a.current <= minAdaptiveBitRate {
		return 0, BitrateReport{}
	}

	next := int(lossCutBlind * float64(a.current))
	carried := 0
	if a.lossBusy > 0 {
		carried = int(float64(a.lossBytes) * 8 / a.lossBusy.Seconds())
		next = int(lossCutGoodput * float64(carried))
	}
	if max := int(lossCutMax * float64(a.current)); next > max {
		next = max
	}
	if next < minAdaptiveBitRate {
		next = minAdaptiveBitRate
	}
	if float64(a.current-next)/float64(a.current) < minChangeFraction {
		return 0, BitrateReport{}
	}
	runMs, runBytes := a.lossBusy.Milliseconds(), a.lossBytes
	a.lossAt = append(a.lossAt, now)
	n, rep := a.commitCut(now, next, GateLoss)
	d := s.Delivery
	rep.Detail = map[string]any{
		"rttMs": s.RTT.Milliseconds(), "baseMs": s.BaseRTT.Milliseconds(),
		"produced": round1(d.Produced), "delivered": round1(d.Delivered), "sent": round1(d.Sent),
		"events": round1(d.Events), "abandons": round1(d.Abandons),
		"deliveredFrac": round3(d.DeliveredFraction()), "sentLossFrac": round3(d.SentLossFraction()),
		"runMs": runMs, "runBytes": runBytes, "carriedBps": carried, "run": lossDwell,
	}
	return n, rep
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }
