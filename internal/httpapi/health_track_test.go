package httpapi

import (
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/session"
)

// fakeCounters is a healthCounters healthTracker can be driven against
// without a live session pump -- everything the reviewer's mutation list
// named (produced not diffed, lastProduced never advancing, shed passed
// through cumulative, config not excluded) is reachable through it directly.
type fakeCounters struct {
	media uint64
	shed  uint64
	cs    map[string]any
	csAt  time.Time
}

func (f fakeCounters) MediaCount() uint64                         { return f.media }
func (f fakeCounters) ShedCount() uint64                          { return f.shed }
func (f fakeCounters) ClientStatsAt() (map[string]any, time.Time) { return f.cs, f.csAt }

// producedRate is diffed against the tracker's own last-seen value, not read
// as a raw cumulative count. A mutation that skipped the diff (passed the
// cumulative FrameCount straight through) fails this on the second tick.
func TestProducedRateIsDiffedNotCumulative(t *testing.T) {
	base := time.Unix(1000, 0)
	tr := newHealthTracker(base, func() time.Time { return base })

	// The very first call seeds the diff base rather than reporting anything
	// (see TestFirstTickSeedsFromCurrentCountersNotZero); the second and
	// third calls are what this test is checking.
	tr.sampleAt(base.Add(time.Second), fakeCounters{media: 60, csAt: base}, 20)

	tick := tr.sampleAt(base.Add(2*time.Second), fakeCounters{media: 120, csAt: base}, 20)
	if tick.ProducedRate != 60 {
		t.Fatalf("second tick ProducedRate = %d, want 60 (120-60, not the cumulative 120)", tick.ProducedRate)
	}

	tick = tr.sampleAt(base.Add(3*time.Second), fakeCounters{media: 180, csAt: base}, 20)
	if tick.ProducedRate != 60 {
		t.Fatalf("third tick ProducedRate = %d, want 60 (180-120, not the cumulative 180)", tick.ProducedRate)
	}
}

// lastProduced (and lastConfig, lastShed) seed from the FIRST sample's
// counters, not from zero: a viewer attaching mid-session must not read the
// session's entire history as this one second's rate. A mutation seeding
// lastProduced at 0 fails this: it would report ProducedRate 500000 on the
// first tick instead of the true per-tick delta.
func TestFirstTickSeedsFromCurrentCountersNotZero(t *testing.T) {
	base := time.Unix(2000, 0)
	tr := newHealthTracker(base, func() time.Time { return base })

	tick := tr.sampleAt(base.Add(time.Second), fakeCounters{media: 500000, shed: 900, csAt: base}, 20)
	if tick.ProducedRate != 0 {
		t.Fatalf("first tick after a mid-session attach: ProducedRate = %d, want 0 (nothing has happened since attach yet)", tick.ProducedRate)
	}
	if tick.ShedRate != 0 {
		t.Fatalf("first tick after a mid-session attach: ShedRate = %d, want 0", tick.ShedRate)
	}

	tick = tr.sampleAt(base.Add(2*time.Second), fakeCounters{media: 500040, shed: 903, csAt: base}, 20)
	if tick.ProducedRate != 40 {
		t.Fatalf("second tick ProducedRate = %d, want 40", tick.ProducedRate)
	}
	if tick.ShedRate != 3 {
		t.Fatalf("second tick ShedRate = %d, want 3", tick.ShedRate)
	}
}

// The rate paired against a fresh report (prevProducedPerSec, fed into the
// delivery-ratio window) is normalised by the real wall-clock gap between
// ticks, not assumed to always be one second. The stats ticker shares a
// select with SendFrame (finding 2's doc comment on healthSample), so a tick
// stretched by an in-flight write must not inflate the produced count the
// window compares against the client's decFps, which is already a rate.
//
// Constructed so the two readings disagree on the verdict: a stretched tick
// producing 120 frames over 2s is 60/s, matching a 60-delivered report
// exactly (ratio 1.0, "good"); read as a raw un-normalised diff it is
// 120/s against the same 60 delivered (ratio 0.5, "bad").
// A single stretched tick, or a window whose first paired entry is the (0,
// delivered) seed pairing, is not enough to pin this: the phase allowance
// (see phaseAllowance) forgives the difference between the window's edge
// rates, and a zero-produced seed entry as the trailing edge makes that a
// whole tick's production, so the slack and the un-normalised tick cancel by
// coincidence and the mutation survives regardless of window length.
//
// This test warms the tracker up with one un-reported tick first, so
// prevProducedPerSec already holds a real (not zero) rate before the first
// fresh report ever arrives, then feeds five reported ticks so every window
// entry pairs a genuine rate against a genuine decFps. Every tick is
// stretched to 2s by the same amount: correctly normalised, every pairing is
// a perfect 60-for-60 match; left un-normalised, every pairing reads 120
// against 60, and with equal edges the allowance is only its floor.
func TestProducedRateIsNormalisedByElapsedTime(t *testing.T) {
	base := time.Unix(2500, 0)
	tr := newHealthTracker(base, func() time.Time { return base })

	// Seed tick: establishes the media baseline.
	tr.sampleAt(base.Add(2*time.Second), fakeCounters{media: 1000, csAt: base}, 20)
	// Warm-up tick, no report: prevProducedPerSec becomes a real 60/s (or a
	// mutated 120/s) before any pairing happens.
	tr.sampleAt(base.Add(4*time.Second), fakeCounters{media: 1120, csAt: base}, 20)

	report := map[string]any{"rttMs": 10.0, "decFps": 60.0}
	var tick healthTick
	for i := 1; i <= 5; i++ {
		now := base.Add(time.Duration(2*i+4) * time.Second)
		tick = tr.sampleAt(now, fakeCounters{media: 1120 + uint64(120*i), csAt: now, cs: report}, 20)
	}

	verdict, reason := session.Health(tick.Input)
	if verdict != "good" {
		t.Fatalf("produced %d of window, delivered %d: verdict = %q (%q), want good -- normalised, every tick is a perfect 60-for-60 match; un-normalised, every pairing reads as only half delivered with nothing left to absorb it",
			tick.Input.Produced, tick.Input.Delivered, verdict, reason)
	}
}

// ShedRate is the diff since the last tick, never the session's cumulative
// shed count -- feeding the cumulative value straight into session.Health as
// ShedRate would read every session as permanently "bad" after 20 total shed
// frames, forever.
func TestShedRateIsDiffedNotCumulative(t *testing.T) {
	base := time.Unix(3000, 0)
	tr := newHealthTracker(base, func() time.Time { return base })

	tr.sampleAt(base.Add(time.Second), fakeCounters{shed: 1000, csAt: base}, 20)
	tick := tr.sampleAt(base.Add(2*time.Second), fakeCounters{shed: 1002, csAt: base}, 20)
	if tick.ShedRate != 2 {
		t.Fatalf("ShedRate = %d, want 2 (1002-1000), not the cumulative 1002", tick.ShedRate)
	}
	if tick.Input.ShedRate != 2 {
		t.Fatalf("Input.ShedRate = %d, want 2 fed into session.Health, not the cumulative total", tick.Input.ShedRate)
	}
}

// Config-frame exclusion (finding 5) is no longer this tracker's job: the
// pump excludes config frames from MediaCount itself now (session.Session's
// mediaIn). internal/session/rotation_test.go's
// TestThePumpCountsConfigFramesSeparatelyFromMediaFrames pins it, by feeding
// the pump one config frame and two media frames and requiring
// MediaCount() == 2. That exclusion is what closed the FrameCount/ConfigCount
// read-order race the previous version of this file worked around with a
// clamp (round 3 finding 4). fakeCounters here models MediaCount directly, so
// there is nothing left to subtract at this layer.

// The delivery ratio is judged over a sliding window (finding 1), not a
// single tick: synthetic's own per-tick production swings 40-72 on a
// perfectly healthy link, and a single tick comparing THIS tick's produced
// count against a report that measured the PREVIOUS tick would misjudge it
// as degraded on the mismatch alone.
//
// This models the client's actual lag (finding 2): web/app.js's
// sendClientStats reports decFps for the second BEFORE its own once-a-second
// timer fires, so a report landing on tick i always describes what was
// produced during tick i-1, never tick i. report[i] is set to
// perTickProduced[i-1] here, not perTickProduced[i], for exactly that
// reason -- pairing same-tick values the way the old code did would not
// reproduce what app.js actually sends.
//
// Every one of a fresh session's first two ticks is measuring or reporting
// something that predates any real production (there is nothing to report
// at i=0, and the pair pushed at i=1 is unavoidably measured against the
// tracker's own zero-produced seed tick -- see
// TestFirstTickSeedsFromCurrentCountersNotZero), so those two mismatched
// entries have to age out of the healthWindowTicks-deep window before every
// remaining pair lines up exactly. Seven ticks is two more than
// healthWindowTicks(5): the first two (i=0, i=1) age out, leaving a window
// of five pairs that all match by construction.
func TestASyntheticSwingDoesNotReadDegraded(t *testing.T) {
	base := time.Unix(5000, 0)
	tr := newHealthTracker(base, func() time.Time { return base })

	perTickProduced := []uint64{40, 72, 54, 65, 48, 55, 60}
	var cumulative uint64
	var lastTick healthTick
	for i, n := range perTickProduced {
		cumulative += n
		now := base.Add(time.Duration(i+1) * time.Second)
		var cs map[string]any
		if i > 0 {
			cs = map[string]any{"rttMs": 20.0, "decFps": float64(perTickProduced[i-1])}
		}
		lastTick = tr.sampleAt(now, fakeCounters{media: cumulative, csAt: now, cs: cs}, 20)
	}

	if lastTick.Input.Produced == 0 {
		t.Fatalf("window produced sum is 0, want > 0")
	}
	if lastTick.Input.Produced != lastTick.Input.Delivered {
		t.Fatalf("window produced %d != delivered %d: once the seed-tick transient has aged out of the window, a perfectly healthy, wildly swinging link must sum to an exact match",
			lastTick.Input.Produced, lastTick.Input.Delivered)
	}
	if verdict, reason := session.Health(lastTick.Input); verdict != "good" {
		t.Fatalf("verdict = %q (%q), want good", verdict, reason)
	}
}

// A sustained low rate -- 4 frames/s produced, 0 delivered, every tick, as on
// a slow panel (traps 22 and 38) -- must eventually read bad. Accumulating
// across the window is what makes this possible at all: any single tick's
// Produced (4) never reaches session's MinProducedForRatio (5) on its own,
// so without the window this stream would read "good" forever purely
// because no individual tick is ever judged (finding 3). This replaces
// TestTooFewFramesToJudgeIsGood's old wiring-level counterpart, which
// asserted the same single-tick fact the session package's own
// TestTooFewFramesToJudgeIsGood already covers and said nothing about
// whether a sustained stall is ever caught.
func TestSustainedLowProducedEventuallyReadsBad(t *testing.T) {
	base := time.Unix(6000, 0)
	tr := newHealthTracker(base, func() time.Time { return base })

	var cumulative uint64
	var lastTick healthTick
	for i := 0; i < healthWindowTicks; i++ {
		cumulative += 4
		now := base.Add(time.Duration(i+1) * time.Second)
		cs := map[string]any{"rttMs": 20.0, "decFps": 0.0}
		lastTick = tr.sampleAt(now, fakeCounters{media: cumulative, csAt: now, cs: cs}, 20)
	}

	verdict, reason := session.Health(lastTick.Input)
	if verdict != "bad" {
		t.Fatalf("after %d ticks of 4/s produced, 0 delivered: verdict = %q (%q), want bad", healthWindowTicks, verdict, reason)
	}
}

// A report timestamped before this viewer's attachedAt reads as no report at
// all, regardless of how healthy or unhealthy its numbers are (finding 4):
// clientStats is session scoped, so a fresh viewer must not be judged on the
// connection before it.
func TestReportBeforeAttachIsIgnored(t *testing.T) {
	attachedAt := time.Unix(7000, 0)
	tr := newHealthTracker(attachedAt, func() time.Time { return attachedAt })

	staleReport := attachedAt.Add(-5 * time.Second)
	now := attachedAt.Add(time.Second)
	cs := map[string]any{"rttMs": 12.0, "decFps": 60.0}
	tick := tr.sampleAt(now, fakeCounters{media: 60, csAt: staleReport, cs: cs}, 20)
	if tick.Input.ReportAge < clientRTTMaxAge {
		t.Fatalf("ReportAge = %v, want >= clientRTTMaxAge for a report predating attach", tick.Input.ReportAge)
	}
	if tick.RttMs != 0 || tick.Delivered != 0 {
		t.Fatalf("got RttMs %v Delivered %v, want 0, 0 for a pre-attach report", tick.RttMs, tick.Delivered)
	}
}

// ReportAge is computed from the clock passed to sampleAt, never from
// time.Now(): a mutation that read time.Now() instead would pass this in
// production but fail here, where "now" is fixed and unrelated to the
// process's real wall clock.
func TestReportAgeUsesTheInjectedClockNotWallTime(t *testing.T) {
	attachedAt := time.Unix(1, 0) // deliberately far from the real wall clock
	tr := newHealthTracker(attachedAt, func() time.Time { return attachedAt })

	reportAt := attachedAt.Add(500 * time.Millisecond)
	now := attachedAt.Add(750 * time.Millisecond)
	cs := map[string]any{"rttMs": 12.0, "decFps": 60.0}
	tick := tr.sampleAt(now, fakeCounters{media: 1, csAt: reportAt, cs: cs}, 20)
	if tick.Input.ReportAge != 250*time.Millisecond {
		t.Fatalf("ReportAge = %v, want exactly 250ms (now - reportAt on the injected clock)", tick.Input.ReportAge)
	}
}

// newHealthTracker panics on a zero attachedAt, rather than silently letting
// a caller's bug (e.g. viewer.go passing a zero-valued time.Time instead of
// time.Now()) make every pre-attach report read as fresh: lastReportAt's
// zero value IS the "nothing has arrived yet" sentinel, so a zero attach
// time collides with it.
func TestConstructorRejectsAZeroAttachTime(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("newHealthTracker(time.Time{}, ...) did not panic")
		}
	}()
	newHealthTracker(time.Time{}, func() time.Time { return time.Time{} })
}

// A join must not read "bad" on a perfect link (finding 1). Before this fix,
// every tick before the viewer's first report pushed a (real produced
// count, 0 delivered) sample into the window, so the window filled with
// manufactured shortfalls and several ticks after the viewer's first honest
// report still read "bad" or "degraded" before finally catching up. Live on
// loopback synthetic that read as t3 `bad "delivered 60 of 120"`, then
// degraded until t7.
//
// The fix is that a tick with nothing fresh to report pushes NOTHING: the
// window only ever holds real report-backed pairs, so a join with no report
// yet reads "stale" (there being nothing to judge), never "bad".
func TestAJoinDoesNotReadBadBeforeTheFirstReport(t *testing.T) {
	attachedAt := time.Unix(11000, 0)
	tr := newHealthTracker(attachedAt, func() time.Time { return attachedAt })

	var cumulative uint64
	for i := 0; i < 6; i++ {
		cumulative += 60
		now := attachedAt.Add(time.Duration(i+1) * time.Second)
		var cs map[string]any
		if i >= 1 {
			// The viewer's own report starts arriving from the second
			// tick onward, reporting exactly what was delivered.
			cs = map[string]any{"rttMs": 12.0, "decFps": 60.0}
		}
		tick := tr.sampleAt(now, fakeCounters{media: cumulative, csAt: now, cs: cs}, 0)
		verdict, reason := session.Health(tick.Input)
		if i == 0 {
			if verdict != "stale" {
				t.Fatalf("tick %d (before any report): verdict = %q (%q), want stale", i, verdict, reason)
			}
			continue
		}
		if verdict != "good" {
			t.Fatalf("tick %d: verdict = %q (%q), want good on a perfect link", i, verdict, reason)
		}
	}
}

// The idle-to-motion transition (finding 2) must not read as several ticks
// of degraded delivery just because the report pairing is one tick behind
// what it is judging.
//
// Measured against this fix (see the exploration this test formalises):
// an idle panel at 6fps stepping straight to 60fps motion, with every
// report perfectly delivered and lagging by exactly one tick the way
// web/app.js's sendClientStats does, reads "good" on every single tick,
// including the transition tick itself. The old same-tick pairing would
// have read the transition tick as a mismatch between this tick's high
// produced count and the previous (low, idle) tick's report.
func TestIdleToMotionTransitionStaysGood(t *testing.T) {
	attachedAt := time.Unix(12000, 0)
	tr := newHealthTracker(attachedAt, func() time.Time { return attachedAt })

	produced := []uint64{6, 6, 6, 6, 60, 60, 60, 60, 60}
	var cumulative uint64
	for i, n := range produced {
		cumulative += n
		now := attachedAt.Add(time.Duration(i+1) * time.Second)
		var cs map[string]any
		if i > 0 {
			// This tick's report reflects exactly what the previous tick
			// produced, the way a perfectly-delivered link's decFps would.
			cs = map[string]any{"rttMs": 12.0, "decFps": float64(produced[i-1])}
		}
		tick := tr.sampleAt(now, fakeCounters{media: cumulative, csAt: now, cs: cs}, 0)
		verdict, reason := session.Health(tick.Input)
		if i == 0 {
			continue // no report has arrived yet; covered by the join test above
		}
		if verdict != "good" {
			t.Fatalf("tick %d (idle=%d): verdict = %q (%q), want good throughout the idle-to-motion transition",
				i, produced[i], verdict, reason)
		}
	}
}

// A real, sustained shortfall must still reach "bad" -- the window and the
// phase allowance (see phaseAllowance) exist to absorb a report arriving out
// of phase with the server's ticker, not to hide a genuinely bad link. The
// allowance comes out of the shortfall, not out of Produced, and reaching
// "bad" needs the tracker's zero-produced seed tick (see
// TestFirstTickSeedsFromCurrentCountersNotZero) to age out of the window,
// since as the trailing edge it makes the allowance a whole tick's
// production. That needs healthWindowTicks+1 real reports.
//
// Measured: a link sustained at exactly a third delivered (60 produced, 20
// delivered every tick) reads "good" while Produced is still below
// session.MinProducedForRatio (tick 1), "good" again at ticks 2-3 while the
// window's sum still includes the seed tick's free pass, "degraded" at ticks
// 4-5 once the shortfall starts to bite, and "bad" from tick 6 onward, once
// the seed tick has fully aged out of the healthWindowTicks-deep window.
func TestASustainedThirdDeliveredStillReachesBad(t *testing.T) {
	attachedAt := time.Unix(13000, 0)
	tr := newHealthTracker(attachedAt, func() time.Time { return attachedAt })

	const reports = healthWindowTicks + 1 // the seed tick must fully age out
	var cumulative uint64
	var verdict, reason string
	for i := 0; i <= reports; i++ {
		cumulative += 60
		now := attachedAt.Add(time.Duration(i+1) * time.Second)
		var cs map[string]any
		if i > 0 {
			cs = map[string]any{"rttMs": 12.0, "decFps": 20.0}
		}
		tick := tr.sampleAt(now, fakeCounters{media: cumulative, csAt: now, cs: cs}, 0)
		verdict, reason = session.Health(tick.Input)
	}
	if verdict != "bad" {
		t.Fatalf("after %d reports of a sustained third delivered: verdict = %q (%q), want bad", reports, verdict, reason)
	}
}

// runPhaseSweepScenario drives produced through the tracker with each fresh
// report modeled as a phase-weighted mix of the tick it lands in and the one
// before it: a report timestamped f seconds into tick i covers real time
// [tick(i)+f-1s, tick(i)+f], i.e. fraction (1-f) of tick i-1's production and
// fraction f of tick i's, scaled by lossFactor to model a lossy link. This is
// the client's actual failure mode (finding 1, round 3): web/app.js's report
// timer runs on its own once-a-second clock, independent of the server's
// ticker, so a report's own one-second window straddles a tick boundary at
// whatever phase the two clocks happen to be offset by.
func runPhaseSweepScenario(t *testing.T, f, lossFactor float64, produced []uint64) (finalVerdict string) {
	t.Helper()
	verdicts, _ := runPhaseSweepVerdicts(t, f, lossFactor, produced)
	return verdicts[len(verdicts)-1]
}

// runPhaseSweepVerdicts is runPhaseSweepScenario returning every tick's
// verdict and reason, index for index with produced. Index 0 is the seed
// tick, which carries no report.
func runPhaseSweepVerdicts(t *testing.T, f, lossFactor float64, produced []uint64) (verdicts, reasons []string) {
	t.Helper()
	attachedAt := time.Unix(20000, 0)
	tr := newHealthTracker(attachedAt, func() time.Time { return attachedAt })

	var cumulative uint64
	prev := 0.0
	for i, n := range produced {
		cumulative += n
		now := attachedAt.Add(time.Duration(i+1) * time.Second)
		var cs map[string]any
		if i > 0 {
			mix := (f*float64(n) + (1-f)*prev) * lossFactor
			cs = map[string]any{"rttMs": 12.0, "decFps": mix}
		}
		tick := tr.sampleAt(now, fakeCounters{media: cumulative, csAt: now, cs: cs}, 0)
		v, why := session.Health(tick.Input)
		verdicts = append(verdicts, v)
		reasons = append(reasons, why)
		prev = float64(n)
	}
	return verdicts, reasons
}

// TestPhaseSweepStaysGoodOnAPerfectLinkAndStillReachesBadOnALoss is finding
// 1's required table test. It sweeps the client-report phase f over
// {0, 0.1, 0.3, 0.5, 0.7, 0.9, 1.0} against four scenarios on a perfect link
// -- each must read "good" throughout, matching what was measured live on
// loopback synthetic before this fix: at f=0.5, motion stopping read
// "degraded" three times then "bad", and a single 12-frame burst on an idle
// panel read "bad 6 of 12". In the same sweep, a link genuinely delivering
// only a third must still reach "bad" regardless of phase, which is the
// other side of the fix: the phase allowance absorbs a phase mismatch,
// not a real loss.
//
// Every one-of-a-kind scenario here duplicates the setup of an existing,
// more heavily-commented single-phase test above (join, idle-to-motion,
// sustained-third): this test's job is breadth across f, not depth on any
// one of them.
func TestPhaseSweepStaysGoodOnAPerfectLinkAndStillReachesBadOnALoss(t *testing.T) {
	phases := []float64{0, 0.1, 0.3, 0.5, 0.7, 0.9, 1.0}

	healthyScenarios := map[string][]uint64{
		"motion-then-still": {60, 60, 60, 60, 0, 0, 0, 0, 0, 0},
		"idle-then-burst":   {0, 0, 0, 12, 0, 0, 0, 0},
		"join-steady-60":    {60, 60, 60, 60, 60, 60, 60, 60},
		"steady-60":         {60, 60, 60, 60, 60, 60, 60, 60, 60, 60, 60, 60},
	}

	for _, f := range phases {
		for name, produced := range healthyScenarios {
			if verdict := runPhaseSweepScenario(t, f, 1.0, produced); verdict != "good" {
				t.Errorf("scenario %q at phase f=%.1f: final verdict = %q, want good on a perfect link", name, f, verdict)
			}
		}

		// A third delivered, at the same phase, must still end up "bad" --
		// enough reports for the seed tick to age fully out of the window
		// (see TestASustainedThirdDeliveredStillReachesBad).
		lossy := make([]uint64, healthWindowTicks+2)
		for i := range lossy {
			lossy[i] = 60
		}
		if verdict := runPhaseSweepScenario(t, f, 1.0/3.0, lossy); verdict != "bad" {
			t.Errorf("a third delivered at phase f=%.1f: final verdict = %q, want bad -- the phase allowance must not hide a real, sustained loss", f, verdict)
		}
	}
}

// phases is the report-clock phase sweep every phase test below runs across.
var phases = []float64{0, 0.1, 0.3, 0.5, 0.7, 0.9, 1.0}

// A perfect link reads good on every tick, not only the last. Checking the
// final verdict alone let a zero allowance pass: motion stopping at f=0.5
// reads degraded and then bad for several ticks mid-scenario and is good
// again by the end, once the window holds nothing but idle ticks.
func TestPhaseSweepReadsGoodOnEveryTickOfAPerfectLink(t *testing.T) {
	scenarios := map[string][]uint64{
		"motion-then-still": {60, 60, 60, 60, 0, 0, 0, 0, 0, 0},
		"still-then-motion": {0, 0, 0, 0, 60, 60, 60, 60, 60, 60},
		"idle-then-burst":   {0, 0, 0, 12, 0, 0, 0, 0},
		"idle-10hz-panel":   {10, 10, 10, 10, 10, 10, 10, 10, 10, 10},
		"motion-idle-mix":   {60, 60, 10, 10, 60, 10, 0, 60, 60, 10, 10, 10},
		"steady-60":         {60, 60, 60, 60, 60, 60, 60, 60, 60, 60, 60, 60},
	}
	for _, f := range phases {
		for name, produced := range scenarios {
			verdicts, reasons := runPhaseSweepVerdicts(t, f, 1.0, produced)
			for i := 1; i < len(verdicts); i++ {
				if verdicts[i] != "good" {
					t.Errorf("%s f=%.1f tick %d: %q (%q), want good on a perfect link", name, f, i, verdicts[i], reasons[i])
				}
			}
		}
	}
}

// Loss at a steady rate is not phase error, and the allowance must not pass
// it as such. At a steady 60fps the window's edges are equal, so only the
// floor is forgiven: 80% delivered judges as 242 of 300 and is degraded, 50%
// as 152 of 300 and is bad. The previous allowance forgave the largest tick
// in the window, a fifth of it, and 80% read good at every phase.
func TestLossAtASteadyRateIsNotForgivenAsPhase(t *testing.T) {
	steady := make([]uint64, healthWindowTicks+7)
	for i := range steady {
		steady[i] = 60
	}
	for _, f := range phases {
		if v := runPhaseSweepScenario(t, f, 0.8, steady); v != "degraded" {
			t.Errorf("80%% delivered at 60fps, f=%.1f: %q, want degraded", f, v)
		}
		if v := runPhaseSweepScenario(t, f, 0.5, steady); v != "bad" {
			t.Errorf("50%% delivered at 60fps, f=%.1f: %q, want bad", f, v)
		}
	}
}

// The reason names what the client decoded, not the count the allowance
// lifted it to. At 50% of a steady 60fps the newest second holds 30 of 60.
func TestTheReasonNamesTheFramesActuallyDelivered(t *testing.T) {
	steady := make([]uint64, healthWindowTicks+7)
	for i := range steady {
		steady[i] = 60
	}
	verdicts, reasons := runPhaseSweepVerdicts(t, 0.5, 0.5, steady)
	last := len(verdicts) - 1
	if verdicts[last] != "bad" || reasons[last] != "delivered 30 of 60 frames" {
		t.Fatalf("got %q %q, want bad \"delivered 30 of 60 frames\"", verdicts[last], reasons[last])
	}
}

// A report recorded shortly before this viewer attached belongs to whoever
// was watching before, even when it is recent enough not to be stale. 500ms
// is well inside staleAfter, so only the attach check can reject it.
func TestAReportRecordedJustBeforeAttachIsIgnored(t *testing.T) {
	attachedAt := time.Unix(50000, 0)
	tr := newHealthTracker(attachedAt, nil)
	cs := map[string]any{"rttMs": 12.0, "decFps": 60.0}
	tick := tr.sampleAt(attachedAt.Add(time.Second), fakeCounters{media: 60, cs: cs, csAt: attachedAt.Add(-500 * time.Millisecond)}, 20)
	if v, why := session.Health(tick.Input); v != "stale" {
		t.Fatalf("verdict %q (%q), want stale: the only report predates this viewer", v, why)
	}
	if tick.Delivered != 0 || tick.RttMs != 0 {
		t.Fatalf("consumed a pre-attach report: delivered %d rtt %.1f", tick.Delivered, tick.RttMs)
	}
}

// One report is one sample. A tick with no new report leaves the window as
// it was, rather than pushing the same report again or pushing an empty
// sample against real production.
func TestAReportIsConsumedOnlyOnce(t *testing.T) {
	attachedAt := time.Unix(51000, 0)
	tr := newHealthTracker(attachedAt, nil)
	cs := map[string]any{"rttMs": 12.0, "decFps": 60.0}
	tr.sampleAt(attachedAt.Add(1*time.Second), fakeCounters{media: 60}, 20)
	reportAt := attachedAt.Add(1500 * time.Millisecond)
	first := tr.sampleAt(attachedAt.Add(2*time.Second), fakeCounters{media: 120, cs: cs, csAt: reportAt}, 20)
	again := tr.sampleAt(attachedAt.Add(3*time.Second), fakeCounters{media: 180, cs: cs, csAt: reportAt}, 20)
	if first.Input.Produced != again.Input.Produced || first.Input.Delivered != again.Input.Delivered {
		t.Fatalf("window changed without a new report: %+v -> %+v", first.Input, again.Input)
	}
	if len(tr.window) != 1 {
		t.Fatalf("window holds %d samples after one report, want 1", len(tr.window))
	}
}

// A report is paired with the production of the tick before the one it
// landed on, even when that earlier tick carried no report of its own. Here
// the report landing on tick 4 is paired with tick 3's 50, not tick 4's 20,
// and the one landing on tick 2 with the seed tick's 0.
func TestAReportIsPairedWithThePreviousTicksProduction(t *testing.T) {
	attachedAt := time.Unix(52000, 0)
	tr := newHealthTracker(attachedAt, nil)
	cs := map[string]any{"rttMs": 12.0, "decFps": 1.0}
	at := func(d time.Duration) time.Time { return attachedAt.Add(d) }
	tr.sampleAt(at(1*time.Second), fakeCounters{media: 0}, 20)
	tr.sampleAt(at(2*time.Second), fakeCounters{media: 10, cs: cs, csAt: at(1900 * time.Millisecond)}, 20)
	tr.sampleAt(at(3*time.Second), fakeCounters{media: 60, cs: cs, csAt: at(1900 * time.Millisecond)}, 20)
	tick := tr.sampleAt(at(4*time.Second), fakeCounters{media: 80, cs: cs, csAt: at(3900 * time.Millisecond)}, 20)
	if tick.Input.Produced != 50 {
		t.Fatalf("window produced %d, want 50 (0 + 50)", tick.Input.Produced)
	}
}

// The allowance's edges come from the window. Motion at 60fps followed by
// an idle 10fps panel the client decodes none of must read bad once the
// motion has aged out; a trailing edge taken from before the window would
// forgive 50 frames and read the idle panel's total loss as good.
func TestThePhaseAllowanceComesFromTheWindowOnly(t *testing.T) {
	attachedAt := time.Unix(53000, 0)
	tr := newHealthTracker(attachedAt, nil)
	var cumulative uint64
	var tick healthTick
	for i, n := range []uint64{60, 60, 60, 10, 10, 10, 10, 10, 10, 10, 10} {
		cumulative += n
		now := attachedAt.Add(time.Duration(i+1) * time.Second)
		var cs map[string]any
		if i > 0 {
			decFps := 0.0
			if i < 3 {
				decFps = 60
			}
			cs = map[string]any{"rttMs": 12.0, "decFps": decFps}
		}
		tick = tr.sampleAt(now, fakeCounters{media: cumulative, cs: cs, csAt: now}, 20)
	}
	if v, why := session.Health(tick.Input); v != "bad" {
		t.Fatalf("idle panel delivering nothing after motion: %q %q, want bad", v, why)
	}
}

// A loss clears from the verdict within five reports of it ending. This pins
// the window from above: a sixth tick of memory keeps one lost second in the
// sum, 300 of 360 delivered, and reads degraded after the link has already
// recovered. Five is written out rather than read from healthWindowTicks so
// that widening the constant fails here instead of moving the test with it.
func TestTheVerdictRecoversWithinFiveReports(t *testing.T) {
	attachedAt := time.Unix(55000, 0)
	tr := newHealthTracker(attachedAt, nil)
	var cumulative uint64
	var v, why string
	const lost, recovered = 8, 5
	for i := 0; i <= lost+recovered; i++ {
		cumulative += 60
		now := attachedAt.Add(time.Duration(i+1) * time.Second)
		var cs map[string]any
		if i > 0 {
			decFps := 60.0
			if i <= lost {
				decFps = 0
			}
			cs = map[string]any{"rttMs": 12.0, "decFps": decFps}
		}
		tick := tr.sampleAt(now, fakeCounters{media: cumulative, cs: cs, csAt: now}, 20)
		v, why = session.Health(tick.Input)
		if i == lost && v != "bad" {
			t.Fatalf("after %d lost seconds: %q (%q), want bad", lost, v, why)
		}
	}
	if v != "good" {
		t.Fatalf("%d reports after the loss ended: %q (%q), want good", recovered, v, why)
	}
}

// On an idle panel at 1fps a frame arriving on the client's timer boundary
// lands in the next report, so a perfect link can report 0 and then 2. A
// window ending on the 0 holds 4 of 5, which is below 0.9; the allowance's
// floor is what keeps that from reading degraded when the window's edges
// are equal.
func TestAFrameCountedInTheClientsNextSecondIsNotALoss(t *testing.T) {
	attachedAt := time.Unix(56000, 0)
	tr := newHealthTracker(attachedAt, nil)
	var cumulative uint64
	reports := []float64{1, 1, 1, 1, 1, 0, 2, 1, 1, 1}
	for i := 0; i <= len(reports); i++ {
		cumulative++
		now := attachedAt.Add(time.Duration(i+1) * time.Second)
		var cs map[string]any
		if i > 0 {
			cs = map[string]any{"rttMs": 12.0, "decFps": reports[i-1]}
		}
		tick := tr.sampleAt(now, fakeCounters{media: cumulative, cs: cs, csAt: now}, 20)
		if i > 0 {
			if v, why := session.Health(tick.Input); v != "good" {
				t.Errorf("tick %d: %q (%q), want good", i, v, why)
			}
		}
	}
}

// At a full frame rate one second holds enough frames to judge, so the dot
// follows that second rather than the window behind it. With the window
// always five reports long, the first healthy second after a loss still
// summed four lost ones and read bad, and the dot stayed red for four seconds
// after video was back on the phone.
func TestAFullRateStreamRecoversOnTheFirstHealthyReport(t *testing.T) {
	attachedAt := time.Unix(57000, 0)
	tr := newHealthTracker(attachedAt, nil)
	var cumulative uint64
	const lost = 8
	for i := 0; i <= lost+1; i++ {
		cumulative += 60
		now := attachedAt.Add(time.Duration(i+1) * time.Second)
		var cs map[string]any
		if i > 0 {
			decFps := 60.0
			if i <= lost {
				decFps = 0
			}
			cs = map[string]any{"rttMs": 12.0, "decFps": decFps}
		}
		tick := tr.sampleAt(now, fakeCounters{media: cumulative, cs: cs, csAt: now}, 20)
		v, why := session.Health(tick.Input)
		if i == lost && v != "bad" {
			t.Fatalf("after %d lost seconds: %q (%q), want bad", lost, v, why)
		}
		if i == lost+1 && v != "good" {
			t.Fatalf("first healthy report after the loss: %q (%q), want good", v, why)
		}
	}
}

// A slow stream still reaches back for enough frames: 4 a second is under
// session.MinProducedForRatio on its own, so two reports are judged together
// and a stall reads bad once two reports have landed. The first report pairs
// with the tick before any production was counted, so it takes three.
func TestASlowStreamWidensToEnoughFramesToJudge(t *testing.T) {
	attachedAt := time.Unix(58000, 0)
	tr := newHealthTracker(attachedAt, nil)
	var cumulative uint64
	var v, why string
	for i := 0; i <= 3; i++ {
		cumulative += 4
		now := attachedAt.Add(time.Duration(i+1) * time.Second)
		var cs map[string]any
		if i > 0 {
			cs = map[string]any{"rttMs": 12.0, "decFps": 0.0}
		}
		tick := tr.sampleAt(now, fakeCounters{media: cumulative, cs: cs, csAt: now}, 20)
		v, why = session.Health(tick.Input)
	}
	if v != "bad" || why != "delivered 0 of 8 frames" {
		t.Fatalf("got %q %q, want bad \"delivered 0 of 8 frames\"", v, why)
	}
}

// Trap 48's session: an idle simulator producing two frames a second, every
// one rejected by the phone's decoder. That is under MinProducedForRatio, so
// delivery was never judged and the dot read good with nothing on screen.
// Errors in a report read bad for that second, and the next clean report
// reads good again. The first report is only a baseline, because the count
// is per page load and a reconnect carries the old connection's errors.
func TestDecodeErrorsReadBadOnlyWhileTheyHappen(t *testing.T) {
	attachedAt := time.Unix(59000, 0)
	tr := newHealthTracker(attachedAt, nil)
	var cumulative uint64
	errs := []float64{5, 6, 7, 7}
	want := []string{"good", "bad", "bad", "good"}
	for i, e := range errs {
		cumulative += 2
		now := attachedAt.Add(time.Duration(i+1) * time.Second)
		cs := map[string]any{"rttMs": 12.0, "decFps": 0.0, "decodeErrors": e}
		tick := tr.sampleAt(now, fakeCounters{media: cumulative, cs: cs, csAt: now}, 20)
		if v, why := session.Health(tick.Input); v != want[i] {
			t.Fatalf("report %d (errors %v): %q (%q), want %s", i, e, v, why, want[i])
		}
	}
}

// A reloaded page counts from zero again; that is not a negative rate.
func TestAReloadedPagesDecodeErrorsStartOver(t *testing.T) {
	attachedAt := time.Unix(60000, 0)
	tr := newHealthTracker(attachedAt, nil)
	for i, e := range []float64{9, 0, 1} {
		now := attachedAt.Add(time.Duration(i+1) * time.Second)
		cs := map[string]any{"rttMs": 12.0, "decFps": 60.0, "decodeErrors": e}
		tick := tr.sampleAt(now, fakeCounters{media: uint64(60 * (i + 1)), cs: cs, csAt: now}, 20)
		if got, want := tick.Input.DecodeErrors, []int{0, 0, 1}[i]; got != want {
			t.Fatalf("report %d: DecodeErrors %d, want %d", i, got, want)
		}
	}
}
