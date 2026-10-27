package session

import (
	"fmt"
	"time"
)

// HealthInput is everything Health needs to judge one second of a viewer's
// stream. Every field is what the stats tick in internal/httpapi/viewer.go
// already has to hand each second; Health itself touches no session state so
// it can be pinned by table tests instead of a live pacer.
type HealthInput struct {
	// Produced is not one second's count: it is the windowed sum, over up to
	// healthWindowTicks fresh client reports, of the media-frame production
	// rate each was paired with (session.MediaCount(), diffed per tick --
	// see internal/httpapi/health_track.go's healthTracker). It is the
	// denominator: an idle panel producing 6 frames a second is not a
	// failing stream (trap 20), so delivery is judged against what was
	// offered, never against 60.
	Produced int
	// Delivered is the viewer's own decoded-frames-per-second report
	// (web/app.js sendClientStats's decFps). Reading it from the client
	// rather than from bytes written to the transport is what keeps trap 35
	// from recurring here: a frame the transport reset mid-flight was never
	// decoded, so it can never inflate Delivered the way it once inflated
	// goodput.
	Delivered int
	// Allowance is how many frames of shortfall the caller knows are
	// measurement error rather than loss, and forgives before judging the
	// ratio. It lifts the judged count and nothing else: the reason string
	// still names Delivered as reported, so a trace reader sees what the
	// client decoded rather than a number adjusted for phase.
	// internal/httpapi's healthTracker derives it from how the client's
	// report clock straddles the server's ticks.
	Allowance int
	// ShedRate is frames shed this second, all reasons combined (pacer,
	// stale, await-key, abandon -- see ShedCounts). It is the session's own
	// signal that the link cannot carry what was offered to it.
	ShedRate uint64
	// BaseMs is the pacer's learned path baseline in milliseconds
	// (Pacer.Base()). Rtt is judged against this, not against an absolute
	// number, because trap 5 is exactly a healthy relayed path with a
	// 150-280ms baseline reading as permanently bad against a fixed
	// threshold. Zero means the pacer has not sampled anything yet -- a
	// viewer's first ticks, before pacer.Observe has run once -- and the rtt
	// check below is skipped entirely rather than judged against a baseline
	// that has not been learnt.
	BaseMs float64
	// RttMs is the client's own measured round trip (web/app.js's
	// sendClientStats, the same report ReportAge is timed from), not the
	// pacer's internal pathRTT: the two can disagree, and it is the phone's
	// number that determines what the phone experiences.
	RttMs float64
	// DecodeErrors is how many frames the phone's decoder rejected in the
	// report's second. A rejected frame is no picture, whatever the counters
	// above say: an idle simulator produces two frames a second, under
	// MinProducedForRatio, so a viewer failing every one of them read good
	// on delivery, shed and rtt alike (trap 48).
	DecodeErrors int
	// ReportAge is how long ago the client stats this second's Delivered and
	// RttMs came from were recorded. An old reading is no reading (trap 26):
	// it reads as "no report" regardless of what the stale numbers say.
	ReportAge time.Duration
}

// Health thresholds. Each is pinned from both sides by health_test.go: moving
// it either direction fails a boundary test.
const (
	// staleAfter matches web/health.js's STALE_AFTER_MS and the spec's "2.5s"
	// (trap 26): the client stats report travels over the transport as
	// MsgClientStats, queued behind the video itself, so anything older than
	// that is not a fresh measurement of the current second. The boundary is
	// inclusive -- exactly 2.5s reads stale -- to match web/health.js's
	// `>=`.
	staleAfter = 2500 * time.Millisecond

	// MinProducedForRatio is how many frames a second must have produced
	// before the delivery ratio is judged at all. Below it a single missed
	// frame swings the ratio wildly, and an idle panel legitimately produces
	// as few as one frame every few seconds (trap 20); 5 is the smallest
	// count exercised by the idle-panel test in health_test.go, chosen (not
	// measured) to sit just above that floor.
	MinProducedForRatio = 5

	// degradedRatio and badRatio are delivered/produced. Chosen to line up
	// with the client's own absolute thresholds (decFps < 55 / < 40 against
	// an assumed 60fps stream -- web/app.js's grade(el.mDec, ...), which is
	// still live and unrelated to this server-side verdict) once restated as
	// a fraction of what was actually produced: 55/60 = 0.917 and 40/60 =
	// 0.667, rounded to 0.9 and 0.6. The restatement is what fixes trap 20 --
	// the client's absolute form reads an idle 6fps panel as failing.
	degradedRatio = 0.9
	badRatio      = 0.6

	// degradedShedRate and badShedRate are copied verbatim from web/app.js's
	// grade(el.mShed, state.shedRate, 5, 20). They were tuned there against
	// real shedding while diagnosing traps 27 and 29, but that tuning has not
	// been independently repeated against this server-side counter -- the
	// values are carried over, not re-measured.
	degradedShedRate = 5
	badShedRate      = 20

	// degradedRttExcess and badRttExcess are likewise copied verbatim from
	// web/app.js's grade(el.mRtt, state.rtt, 60, 120), and are judged against
	// BaseMs rather than as an absolute rtt for the same reason trap 5 forced
	// pacer.go's targetExcess/recoverExcess to become relative.
	degradedRttExcess = 60 * time.Millisecond
	badRttExcess      = 120 * time.Millisecond
)

// severity orders the three verdicts below "stale", which Health returns
// before any of these are considered.
type severity int

const (
	sevGood severity = iota
	sevDegraded
	sevBad
)

func (v severity) String() string {
	switch v {
	case sevBad:
		return "bad"
	case sevDegraded:
		return "degraded"
	default:
		return "good"
	}
}

// finding is one input's verdict and the reason string that names it.
type finding struct {
	sev    severity
	reason string
}

// Health judges one second of a viewer's stream and returns a verdict --
// "good", "degraded", "bad" or "stale" -- plus a reason naming the counter
// that drove it. It touches no session state: every input is a value the
// stats tick already computed, which is what lets every threshold be pinned
// by a table test instead of a live pacer.
//
// Verdicts are not layered: a stale report is not "the last verdict, stale".
// It is judged first and returned immediately, before the delivery ratio,
// shed rate or rtt are read at all (trap 26). Among the three live inputs the
// worst one wins, with delivery ratio checked before shed rate before rtt --
// an explicit, fixed order, since two inputs can tie in severity and the
// reason string can only name one.
func Health(in HealthInput) (verdict string, reason string) {
	if in.ReportAge >= staleAfter {
		return "stale", "no report"
	}

	var worst finding

	// Delivery ratio: delivered frames against what the encoder actually
	// produced over the window, never against 60 (trap 20).
	if in.Produced >= MinProducedForRatio {
		ratio := float64(in.Delivered+in.Allowance) / float64(in.Produced)
		f := finding{reason: fmt.Sprintf("delivered %d of %d frames", in.Delivered, in.Produced)}
		switch {
		case ratio < badRatio:
			f.sev = sevBad
		case ratio < degradedRatio:
			f.sev = sevDegraded
		}
		if f.sev > worst.sev {
			worst = f
		}
	}

	// Decode errors: the phone received frames and could not show them.
	if in.DecodeErrors > 0 {
		f := finding{sev: sevBad, reason: fmt.Sprintf("decoder rejected %d frames", in.DecodeErrors)}
		if f.sev > worst.sev {
			worst = f
		}
	}

	// Shed rate: how much the session itself is discarding, all reasons
	// combined.
	{
		f := finding{reason: fmt.Sprintf("shed %d/s", in.ShedRate)}
		switch {
		case in.ShedRate >= badShedRate:
			f.sev = sevBad
		case in.ShedRate >= degradedShedRate:
			f.sev = sevDegraded
		}
		if f.sev > worst.sev {
			worst = f
		}
	}

	// Round trip against the path's own learned baseline, not an absolute
	// number (trap 5). Skipped while BaseMs is 0: that is not a
	// zero-latency path, it is a pacer that has not sampled anything yet, and
	// every one of a fresh viewer's first ticks would otherwise read "rtt
	// Xms over a 0ms base" regardless of how healthy the link actually is.
	if in.BaseMs > 0 {
		excess := time.Duration((in.RttMs - in.BaseMs) * float64(time.Millisecond))
		f := finding{reason: fmt.Sprintf("rtt %.0fms over a %.0fms base", in.RttMs, in.BaseMs)}
		switch {
		case excess >= badRttExcess:
			f.sev = sevBad
		case excess >= degradedRttExcess:
			f.sev = sevDegraded
		}
		if f.sev > worst.sev {
			worst = f
		}
	}

	if worst.sev == sevGood {
		return "good", ""
	}
	return worst.sev.String(), worst.reason
}
