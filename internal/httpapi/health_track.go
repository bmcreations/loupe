package httpapi

import (
	"math"
	"time"

	"github.com/bmcreations/loupe/internal/session"
)

// healthCounters is the subset of *session.Session state healthTracker reads
// each tick, narrowed to an interface so the sliding window and the
// per-viewer attach-time gating below can be pinned by table tests against a
// fake instead of a live session pump. *session.Session satisfies it as-is.
type healthCounters interface {
	MediaCount() uint64
	ShedCount() uint64
	ClientStatsAt() (map[string]any, time.Time)
}

// healthWindowTicks bounds how many stats ticks the delivery-ratio judgement
// can look back across. It is a ceiling, not the window: judged reaches back
// only until it holds session.MinProducedForRatio frames, which at a full
// frame rate is the newest report alone. Always summing five made the dot lag
// the stream by four seconds each way -- a loss read good until it outweighed
// the healthy seconds before it, and a recovery read bad until the lost ones
// aged out (trap 48, TestAFullRateStreamRecoversOnTheFirstHealthyReport).
//
// A single tick's produced-vs-delivered pair compares two numbers that were
// never describing the same interval: "produced" is the server's diff since
// the last tick, "delivered" is the newest client decFps report, which can be
// up to clientRTTMaxAge old and is sampled on the client's own clock, not the
// server's ticker. Live on loopback synthetic -- a perfectly healthy link --
// a single tick read `degraded "delivered 54 of 65"` purely because
// synthetic's own per-tick production swings 40-72. Summing several ticks'
// worth of both counters smooths that mismatch without hiding a real,
// sustained shortfall (see session.MinProducedForRatio, and
// TestASyntheticSwingDoesNotReadDegraded).
const healthWindowTicks = 5

// healthSample is one stats tick's contribution to the delivery-ratio window,
// already normalised to a one-second rate. The normalisation matters because
// the stats ticker shares a select with SendFrame and can be delayed past a
// full second when a write is in flight, so the wall-clock gap between two
// ticks is not reliably 1s; a raw frame count from a stretched tick would
// otherwise inflate "produced" relative to the client's decFps, which is
// already expressed as a rate.
type healthSample struct {
	// producedPerSec is the production rate this sample's report is paired
	// against: the tick BEFORE the one the report landed on.
	producedPerSec float64
	// landedPerSec is the production rate of the tick the report landed on.
	// Only the newest sample's is read, as the window's leading edge (see
	// phaseAllowance).
	landedPerSec float64
	delivered    float64
}

// healthTick is what one call to healthTracker.Sample hands back: the
// windowed session.HealthInput ready for session.Health, plus the raw
// per-tick rates the stats tick and trace record still report on their own,
// independent of the window.
type healthTick struct {
	ShedRate     uint64
	ProducedRate uint64
	// RttMs and Delivered are the most recently CONSUMED client report --
	// not necessarily this tick's, since a tick with no fresh report carries
	// the last one forward (see sampleAt) -- which is what MsgStats's
	// "rttMs" and "fps" fields show a viewer.
	RttMs     float64
	Delivered int
	Input     session.HealthInput
}

// healthTracker turns one viewer's raw session counters into a
// session.HealthInput each stats tick. It owns everything that has to
// persist between ticks -- the produced/config/shed diff base, the
// delivery-ratio window, the one-tick production history a fresh report is
// paired against, and the viewer's own attach time -- so serveViewer's stats
// case stays a single call, and every one of these pieces can be pinned
// directly by health_track_test.go against a fake clock and fake counters
// instead of only through a live session and a real ticker.
type healthTracker struct {
	attachedAt time.Time
	clock      func() time.Time

	have      bool
	lastMedia uint64
	lastShed  uint64
	lastTick  time.Time

	// prevProducedPerSec is the PREVIOUS tick's normalised production rate.
	// A fresh report landing this tick is paired against it rather than this
	// tick's own rate -- see the pairing comment in sampleAt.
	prevProducedPerSec float64

	// lastReportAt is when the last CONSUMED (fresh, post-attach) client
	// report was recorded, the zero value until the first one arrives. rttMs
	// and delivered are that report's values, carried forward on ticks with
	// nothing fresh to read.
	lastReportAt  time.Time
	lastRttMs     float64
	lastDelivered int

	// lastDecodeErrors is the client's cumulative decodeErrors at the last
	// consumed report, and decodeErrorRate the difference that report made.
	// The rate is the newest report's alone, never windowed, so the dot turns
	// green on the first second the decoder takes everything.
	lastDecodeErrors float64
	decodeErrorRate  int

	window []healthSample
}

// newHealthTracker starts a tracker attached at attachedAt. clock supplies
// "now" on every Sample; production code passes time.Now, tests pass a fake
// so the window and every staleness comparison can be driven exactly rather
// than raced against the wall clock. A nil clock defaults to time.Now.
//
// attachedAt must not be the zero time: it doubles as the sentinel
// lastReportAt starts at (see sampleAt), so a zero attach time would make a
// pre-attach report from a genuinely zero-valued clientStats read as "fresh".
// serveViewer always passes time.Now(), never a literal zero value; this
// panics rather than silently misjudging a session if that ever changes.
func newHealthTracker(attachedAt time.Time, clock func() time.Time) *healthTracker {
	if attachedAt.IsZero() {
		panic("newHealthTracker: attachedAt must not be the zero time")
	}
	if clock == nil {
		clock = time.Now
	}
	return &healthTracker{attachedAt: attachedAt, clock: clock, lastTick: attachedAt}
}

// Sample reads counters, folds this tick into the delivery-ratio window, and
// returns what session.Health needs plus the per-tick rates the trace record
// and MsgStats body still report individually. baseMs is the pacer's
// Base(), in milliseconds -- the tracker does not own a pacer, so it is
// passed in rather than read.
func (t *healthTracker) Sample(counters healthCounters, baseMs float64) healthTick {
	return t.sampleAt(t.clock(), counters, baseMs)
}

// sampleAt is Sample with the clock call factored out, so a test can pass
// `now` values that do not have to agree with wall-clock timing at all.
func (t *healthTracker) sampleAt(now time.Time, counters healthCounters, baseMs float64) healthTick {
	// A single atomic read: MediaCount is the pump's own count of frames
	// that are not a config frame (see session.Session.mediaIn), so there is
	// no second counter to diff against and subtract, and no read-order race
	// to close. An earlier version read FrameCount and ConfigCount as two
	// separate loads and subtracted one from the other; a config frame
	// landing between those two reads could be counted in "produced" a
	// moment before it was counted in "config", understating that tick's
	// media count by one. Moving the exclusion into the pump removes the
	// race outright rather than bounding it with a clamp.
	media := counters.MediaCount()
	shed := counters.ShedCount()

	if !t.have {
		t.lastMedia = media
		t.lastShed = shed
		t.have = true
	}

	mediaProduced := diffUint64(media, t.lastMedia)
	shedRate := uint64(diffUint64(shed, t.lastShed))

	elapsed := now.Sub(t.lastTick)

	t.lastMedia = media
	t.lastShed = shed
	t.lastTick = now

	curProducedPerSec := 0.0
	if elapsed > 0 {
		curProducedPerSec = float64(mediaProduced) / elapsed.Seconds()
	}

	cs, csAt := counters.ClientStatsAt()
	// A sample is pushed only for a FRESH report: one recorded at or after
	// this viewer's own attach (clientStats is session scoped -- see
	// healthInputFromClientReport) AND strictly after the last report this
	// tracker has already consumed. Without the second condition, every tick
	// with nothing new to report -- which is most of them, since the client
	// reports on its own once-a-second timer while the stats ticker can run
	// faster or slower (trap: the ticker shares a select with SendFrame) --
	// would push a (real produced count, 0 delivered) sample into the
	// window. On a join that read every one of the first several ticks as
	// "bad", because the window filled with manufactured shortfalls before
	// the viewer's first real report ever arrived (finding 1).
	fresh := cs != nil && !csAt.Before(t.attachedAt) && csAt.After(t.lastReportAt)
	if fresh {
		rttMs, delivered, _ := healthInputFromClientReport(cs, csAt, t.attachedAt, now)

		// The pairing: web/app.js's sendClientStats reports decFps for the
		// second BEFORE its own once-a-second timer fired, so a report
		// landing on THIS tick measured what the encoder produced during
		// the PREVIOUS tick, not this one. Pairing it with this tick's own
		// rate instead compares numbers describing different seconds every
		// single time, which is exactly what stretched the idle-to-motion
		// transition into a multi-tick "degraded" read before this fix
		// (finding 2).
		t.push(healthSample{producedPerSec: t.prevProducedPerSec, landedPerSec: curProducedPerSec, delivered: float64(delivered)})

		errs, _ := cs["decodeErrors"].(float64)
		// The count is per page load, not per viewer: a reconnect keeps what
		// the last connection counted, so the first report here is only the
		// baseline. A report below the last is a reloaded page counting from
		// zero.
		if t.lastReportAt.IsZero() {
			t.lastDecodeErrors = errs
		} else if errs < t.lastDecodeErrors {
			t.lastDecodeErrors = 0
		}
		t.decodeErrorRate = int(errs - t.lastDecodeErrors)
		t.lastDecodeErrors = errs

		t.lastRttMs = rttMs
		t.lastDelivered = delivered
		t.lastReportAt = csAt
	}
	t.prevProducedPerSec = curProducedPerSec

	// reportAge is derived fresh every tick from lastReportAt, not stored:
	// that is what makes a report going stale and a report resuming the same
	// fix. A tick with nothing new simply lets this grow; once it crosses
	// session.Health's staleAfter the verdict reads "stale" on its own, with
	// no separate bookkeeping required.
	var reportAge time.Duration
	if t.lastReportAt.IsZero() {
		reportAge = clientRTTMaxAge + time.Second
	} else {
		reportAge = now.Sub(t.lastReportAt)
	}

	sumProduced, sumDelivered := t.sums()

	in := session.HealthInput{
		Produced:  int(math.Round(sumProduced)),
		Delivered: int(math.Round(sumDelivered)),
		Allowance: int(math.Round(t.phaseAllowance())),
		ShedRate:  shedRate,
		BaseMs:    baseMs,
		RttMs:     t.lastRttMs,
		ReportAge: reportAge,

		DecodeErrors: t.decodeErrorRate,
	}

	return healthTick{
		ShedRate:     shedRate,
		ProducedRate: uint64(mediaProduced),
		RttMs:        t.lastRttMs,
		Delivered:    t.lastDelivered,
		Input:        in,
	}
}

func (t *healthTracker) push(s healthSample) {
	t.window = append(t.window, s)
	if len(t.window) > healthWindowTicks {
		t.window = t.window[len(t.window)-healthWindowTicks:]
	}
}

// judged is the newest stretch of the window holding enough production to
// judge: the newest samples back to the first point where their production
// reaches session.MinProducedForRatio, or the whole window if it never does.
// A full-rate stream is judged on its newest report alone, so the verdict
// follows the stream second by second; a slow one widens until a single
// missed frame no longer swings the ratio.
func (t *healthTracker) judged() []healthSample {
	produced := 0.0
	for i := len(t.window) - 1; i >= 0; i-- {
		produced += t.window[i].producedPerSec
		if produced >= session.MinProducedForRatio {
			return t.window[i:]
		}
	}
	return t.window
}

// sums totals the judged stretch's two counters.
func (t *healthTracker) sums() (produced, delivered float64) {
	for _, s := range t.judged() {
		produced += s.producedPerSec
		delivered += s.delivered
	}
	return produced, delivered
}

// minPhaseAllowance is the least shortfall phaseAllowance ever forgives. The
// edge term below is exact only for the idealised model: the client counts
// whole frames on its own timer, which is not exactly one second, so a frame
// arriving on its boundary lands in whichever report its clock puts it in.
// On an idle panel at 1fps that is a report of 0 followed by one of 2, and a
// window ending on the 0 reads 4 of 5 -- degraded on a perfect link. Two
// frames absorbs that and a rounding either way. Chosen, not measured.
const minPhaseAllowance = 2

// phaseAllowance is how much of the window's shortfall is the report clock's
// phase rather than loss.
//
// The client's report timer runs at a random phase f against the server's
// ticker, so a report landing on tick i counts (1-f) of tick i-1's
// production and f of tick i's, while the window pairs it with all of tick
// i-1's. Summed over the window's reports, landing on ticks k..n, the error
// is
//
//	sum_i [(1-f)p(i-1) + f p(i)] - sum_i p(i-1) = f * sum_i [p(i) - p(i-1)]
//	                                            = f * (p(n) - p(k-1))
//
// which telescopes to the two edges: the rate of the tick the newest report
// landed on, and the rate the oldest report was paired with. Everything in
// between cancels, so with f in [0,1] the error is at most |p(n) - p(k-1)|,
// whatever the panel did in the middle of the window. A steady stream has
// equal edges and gets only the floor, which is what keeps 80% delivery at
// 60fps from passing as phase error. An earlier version forgave the largest
// single tick in the window, a fifth of it at any steady rate, and 80% read
// good.
//
// Both edges come from the judged stretch itself. A rate from a tick outside
// it describes a report that is no longer being judged.
func (t *healthTracker) phaseAllowance() float64 {
	w := t.judged()
	if len(w) == 0 {
		return 0
	}
	leading := w[len(w)-1].landedPerSec
	trailing := w[0].producedPerSec
	return math.Max(minPhaseAllowance, math.Abs(leading-trailing))
}

// diffUint64 is cur-prev, floored at 0. Nothing in this codebase decrements
// MediaCount or ShedCount -- an encoder relaunch continues counting rather
// than resetting -- so in production this floor never fires; it exists so a
// test fixture that does not model monotonic growth fails safe instead of
// wrapping to a huge int64.
func diffUint64(cur, prev uint64) int64 {
	if cur < prev {
		return 0
	}
	return int64(cur - prev)
}
