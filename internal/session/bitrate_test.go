package session

import (
	"testing"
	"time"
)

// kbps converts to bits per second, so the tests read in the units the traces
// and the scrcpy flag use.
func kbps(n int) int { return n * 1000 }

// feed folds a run of identical one-second samples in and returns the last
// decision, so a test can express "ten seconds of this" without a loop body.
func feed(a *BitrateAdapter, start time.Time, secs int, s BitrateSample) (int, time.Time) {
	var last int
	now := start
	for i := 0; i < secs; i++ {
		now = now.Add(time.Second)
		if got, _ := a.Observe(now, s); got != 0 {
			last = got
		}
	}
	return last, now
}

// A link carrying everything offered must not be touched. This is the case
// that runs on a desk, and a controller that fiddles with it is worse than no
// controller at all.
func TestAdapterHoldsOnAHealthyLink(t *testing.T) {
	a := NewBitrateAdapter(kbps(3000))
	got, _ := feed(a, time.Now(), 120, BitrateSample{Frames: 60, Shed: 0, Bytes: 375_000})
	if got != 0 {
		t.Fatalf("healthy link was adapted to %d bps, want no change", got)
	}
}

// The 5G trace: 3000 kbps configured, the wire carrying 191-276 kbps, and
// roughly half of every burst shed. The encoder must come down to what the
// link actually delivered, in one step rather than creeping.
func TestAdapterDropsToMeasuredGoodput(t *testing.T) {
	a := NewBitrateAdapter(kbps(3000))
	// 31_250 bytes/s == 250 kbps delivered, with half the frames shed.
	got, _ := feed(a, time.Now(), 12, BitrateSample{Frames: 20, Shed: 10, Bytes: 31_250})
	if got == 0 {
		t.Fatal("sustained shedding at 12x over-configuration produced no change")
	}
	// Exact, not approximate. The target is the measurement, so a tolerance
	// here would hide an arithmetic error in the goodput estimate -- and the
	// direction that error takes is the one that reads a congested link as
	// carrying more than it did.
	if got != kbps(250) {
		t.Fatalf("adapted to %d bps, want the measured 250 kbps goodput", got)
	}
}

// An idle panel produces almost nothing and delivers almost nothing (trap 20).
// Reading that as a slow link would ratchet the encoder down on a connection
// that was never tested, and the next burst would arrive at a bitrate chosen
// from silence.
func TestAdapterIgnoresAnIdleScreen(t *testing.T) {
	a := NewBitrateAdapter(kbps(3000))
	got, _ := feed(a, time.Now(), 120, BitrateSample{Frames: 10, Shed: 0, Bytes: 2_000})
	if got != 0 {
		t.Fatalf("idle screen was adapted to %d bps, want no change", got)
	}
}

// A near-idle screen with a few keyframe waits in it: a viewer attaching
// awaits one keyframe, and on a screen producing two or three frames a
// second that one wait is a large share of the window. Two phone sessions on
// 26 October were cut to the floor this way over links that were direct and
// carrying everything: 26 frames in 12s with awaitFrac 0.27 read as
// undercarried, and 22 frames in 10s read as starved. Neither window had
// enough frames for goodput to say anything about the link.
func TestAdapterIgnoresASparseWindowWithKeyframeWaits(t *testing.T) {
	cases := []struct {
		name   string
		sample BitrateSample
	}{
		{"undercarried shape", BitrateSample{Frames: 3, AwaitKey: 1, Bytes: 1_000}},
		{"starved shape", BitrateSample{Frames: 2, AwaitKey: 1, Bytes: 1_000}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := NewBitrateAdapter(kbps(4000))
			got, _ := feed(a, time.Now(), 60, tc.sample)
			if got != 0 {
				t.Fatalf("sparse window was adapted to %d bps, want no change", got)
			}
		})
	}
}

// Below a certain bitrate the picture stops being worth the bytes, and a
// controller chasing a link that is simply down would otherwise walk to zero.
func TestAdapterNeverGoesBelowTheFloor(t *testing.T) {
	a := NewBitrateAdapter(kbps(3000))
	now := time.Now()
	var last int
	for i := 0; i < 40; i++ {
		// Almost nothing getting through, sustained.
		got, next := feed(a, now, 12, BitrateSample{Frames: 20, Shed: 19, Bytes: 500})
		now = next
		if got != 0 {
			last = got
		}
	}
	if last == 0 {
		t.Fatal("a collapsing link produced no change at all")
	}
	if last < minAdaptiveBitRate {
		t.Fatalf("adapted to %d bps, below the %d floor", last, minAdaptiveBitRate)
	}
}

// After a long clean window the bitrate steps back toward what was configured,
// because walking back into signal should not leave the session degraded for
// as long as it lasts.
func TestAdapterProbesUpAfterASustainedCleanWindow(t *testing.T) {
	a := NewBitrateAdapter(kbps(3000))
	down, now := feed(a, time.Now(), 12, BitrateSample{Frames: 20, Shed: 10, Bytes: 31_250})
	if down == 0 {
		t.Fatal("no downshift to recover from")
	}
	up, _ := feed(a, now, int(settleWindow.Seconds())+int(upWindow.Seconds())+5, BitrateSample{Frames: 60, Shed: 0, Bytes: 375_000})
	if up <= down {
		t.Fatalf("after a clean window the bitrate went %d -> %d, want an increase", down, up)
	}
}

// The configured value is the user's ceiling. Probing up must approach it and
// stop, never overshoot it because the link looked good.
func TestAdapterNeverExceedsTheConfiguredBitrate(t *testing.T) {
	// Deliberately close to minAdaptiveBitRate: probing up from the floor
	// reaches this ceiling in two steps, well inside maxAdaptiveChanges, so
	// the ceiling is what has to stop it rather than the change cap.
	cfg := kbps(400)
	a := NewBitrateAdapter(cfg)
	_, now := feed(a, time.Now(), 12, BitrateSample{Frames: 20, Shed: 10, Bytes: 31_250})
	var last int
	for i := 0; i < 20; i++ {
		got, next := feed(a, now, int(upWindow.Seconds())+2, BitrateSample{Frames: 60, Shed: 0, Bytes: 375_000})
		now = next
		if got != 0 {
			last = got
		}
	}
	if last > cfg {
		t.Fatalf("probed up to %d bps, above the configured %d", last, cfg)
	}
}

// A marginal link sits right on the threshold and would otherwise hunt up and
// down forever, and every change costs a visible encoder restart. The number
// of changes in one session is capped.
func TestAdapterStopsChangingAfterTheCap(t *testing.T) {
	a := NewBitrateAdapter(kbps(3000))
	now := time.Now()
	changes := 0
	for i := 0; i < 60; i++ {
		// Alternate: bad enough to drop, then clean long enough to probe up.
		got, next := feed(a, now, 12, BitrateSample{Frames: 20, Shed: 10, Bytes: 31_250})
		now = next
		if got != 0 {
			changes++
		}
		got, next = feed(a, now, int(upWindow.Seconds())+2, BitrateSample{Frames: 60, Shed: 0, Bytes: 375_000})
		now = next
		if got != 0 {
			changes++
		}
	}
	if changes > maxAdaptiveChanges {
		t.Fatalf("made %d changes, cap is %d", changes, maxAdaptiveChanges)
	}
}

// An encoder restart disturbs the very counters the decision is made from: the
// stream stops, viewers wait for a keyframe, and shed spikes. Deciding again
// inside that window would read the restart as evidence the link got worse.
func TestAdapterSettlesAfterAChange(t *testing.T) {
	a := NewBitrateAdapter(kbps(3000))
	// 125_000 bytes/s == 1000 kbps goodput, so the first change lands well
	// clear of the floor and a second one would be a large, real move rather
	// than something minChangeFraction would reject anyway.
	first, now := feed(a, time.Now(), 12, BitrateSample{Frames: 20, Shed: 10, Bytes: 125_000})
	if first == 0 {
		t.Fatal("no first change")
	}
	// Immediately afterwards, the restart's own disruption: nothing getting
	// through, every viewer awaiting a keyframe.
	got, _ := feed(a, now, int(settleWindow.Seconds())-2, BitrateSample{Frames: 20, Shed: 20, Bytes: 100})
	if got != 0 {
		t.Fatalf("adapted to %d bps inside the settle window, from %d", got, first)
	}
}

// Every change costs an encoder restart the viewer sees, so a link measuring a
// few percent below what the encoder is already producing is not worth acting
// on. Without this the controller restarts the encoder to move 5%.
func TestAdapterIgnoresAChangeTooSmallToBeWorthARestart(t *testing.T) {
	a := NewBitrateAdapter(kbps(1000))
	// Shedding enough to be considered, but 118_750 bytes/s is 950 kbps --
	// within 5% of the 1000 kbps already configured.
	got, _ := feed(a, time.Now(), 12, BitrateSample{Frames: 20, Shed: 10, Bytes: 118_750})
	if got != 0 {
		t.Fatalf("restarted the encoder to move 1000 kbps -> %d bps", got)
	}
}

// Shedding does not on its own mean the link is too narrow. Trap 28's staleness
// path and the keyframe latch both shed heavily on a link with bandwidth to
// spare, and re-encoding smaller fixes neither while costing a restart.
func TestAdapterHoldsWhenTheLinkCarriedEverythingAsked(t *testing.T) {
	a := NewBitrateAdapter(kbps(1000))
	// Shedding hard, but 200_000 bytes/s is 1600 kbps -- more than the encoder
	// was asked to produce, so bandwidth is not what is missing.
	got, _ := feed(a, time.Now(), 12, BitrateSample{Frames: 20, Shed: 15, Bytes: 200_000})
	if got != 0 {
		t.Fatalf("dropped to %d bps on a link carrying more than the encoder produced", got)
	}
}

// Shedding is not on its own evidence of a narrow link. If the wire carried
// everything the encoder produced, the loss is jitter, a stalled write or a
// keyframe wait, and re-encoding smaller fixes none of them while still
// costing a restart.
func TestAdapterHoldsWhenTheLinkCarriedEverything(t *testing.T) {
	a := NewBitrateAdapter(kbps(1000))
	// 150_000 bytes/s is 1200 kbps, above the 1000 configured.
	got, _ := feed(a, time.Now(), 30, BitrateSample{Frames: 20, Shed: 12, Bytes: 150_000})
	if got != 0 {
		t.Fatalf("re-encoded to %d bps on a link that carried everything offered", got)
	}
}

// Probing upward early is how a controller turns one good second into a blip.
// The clean run has to last the full window.
func TestAdapterDoesNotProbeUpBeforeTheFullWindow(t *testing.T) {
	a := NewBitrateAdapter(kbps(3000))
	start := time.Now()
	down, now := feed(a, start, 12, BitrateSample{Frames: 20, Shed: 10, Bytes: 31_250})
	if down == 0 {
		t.Fatal("setup did not come down, so there is nothing to probe up from")
	}
	// Clean, but stopping short of settleWindow + upWindow.
	short := int(settleWindow.Seconds()) + int(upWindow.Seconds()) - 10
	got, _ := feed(a, now, short, BitrateSample{Frames: 60, Shed: 0, Bytes: 40_000})
	if got != 0 {
		t.Fatalf("probed up to %d bps before the window elapsed", got)
	}
}

// Every declined interval must name the branch it came out of. This is the
// whole point of the gate: the controller ran for months against real
// congestion without anyone being able to say whether it had evaluated the
// window and declined, or never reached the window at all. Those two produce
// identical counters everywhere else.
func TestEveryDecisionNamesItsGate(t *testing.T) {
	// One 10s window of heavy shedding on a link carrying 250 kbps, against a
	// 3000 kbps encoder -- the trace case the thresholds were derived from.
	cases := []struct {
		name  string
		build func() (*BitrateAdapter, time.Time, int, BitrateSample)
		want  BitrateGate
	}{
		{
			name: "first sample anchors the window",
			build: func() (*BitrateAdapter, time.Time, int, BitrateSample) {
				return NewBitrateAdapter(kbps(3000)), time.Now(), 1,
					BitrateSample{Frames: 20, Shed: 10, Bytes: 31_250}
			},
			want: GateAnchor,
		},
		{
			name: "a window shorter than downWindow",
			build: func() (*BitrateAdapter, time.Time, int, BitrateSample) {
				return NewBitrateAdapter(kbps(3000)), time.Now(), 3,
					BitrateSample{Frames: 20, Shed: 10, Bytes: 31_250}
			},
			want: GateWindow,
		},
		{
			name: "a full window the encoder produced nothing in",
			build: func() (*BitrateAdapter, time.Time, int, BitrateSample) {
				return NewBitrateAdapter(kbps(3000)), time.Now(), 11,
					BitrateSample{Frames: 0, Shed: 0, Bytes: 0}
			},
			want: GateNoFrames,
		},
		{
			name: "shedding on a link that carried everything asked",
			build: func() (*BitrateAdapter, time.Time, int, BitrateSample) {
				// 200_000 B/s is 1600 kbps, above the 1000 configured.
				return NewBitrateAdapter(kbps(1000)), time.Now(), 11,
					BitrateSample{Frames: 20, Shed: 15, Bytes: 200_000}
			},
			want: GateLinkCarried,
		},
		{
			name: "a move too small to be worth the restart",
			build: func() (*BitrateAdapter, time.Time, int, BitrateSample) {
				// 118_750 B/s is 950 kbps, within 5% of the 1000 configured.
				return NewBitrateAdapter(kbps(1000)), time.Now(), 11,
					BitrateSample{Frames: 20, Shed: 10, Bytes: 118_750}
			},
			want: GateTooSmall,
		},
		{
			name: "sustained shedding on a link that could not carry it",
			build: func() (*BitrateAdapter, time.Time, int, BitrateSample) {
				return NewBitrateAdapter(kbps(3000)), time.Now(), 11,
					BitrateSample{Frames: 20, Shed: 10, Bytes: 31_250}
			},
			want: GateDown,
		},
		{
			name: "a link carrying a fraction of the bitrate, viewer awaiting keyframes",
			build: func() (*BitrateAdapter, time.Time, int, BitrateSample) {
				// 450 kbps of 4000, shedFrac 0.07, awaitFrac 0.33.
				return NewBitrateAdapter(kbps(4000)), time.Now(), 11,
					BitrateSample{Frames: 15, Shed: 1, AwaitKey: 5, Bytes: 56_250}
			},
			want: GateUndercarried,
		},
		{
			name: "a clean window that has not lasted upWindow",
			build: func() (*BitrateAdapter, time.Time, int, BitrateSample) {
				return NewBitrateAdapter(kbps(3000)), time.Now(), 11,
					BitrateSample{Frames: 60, Shed: 0, Bytes: 375_000}
			},
			want: GateUpWindow,
		},
		{
			name: "neither bad enough to act on nor clean enough to recover",
			build: func() (*BitrateAdapter, time.Time, int, BitrateSample) {
				// shedFrac 0.10: above upShedFraction, below downShedFraction.
				return NewBitrateAdapter(kbps(3000)), time.Now(),
					int(upWindow.Seconds()) + 1,
					BitrateSample{Frames: 20, Shed: 2, Bytes: 31_250}
			},
			want: GateInconclusive,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, start, secs, sample := tc.build()
			now := start
			var last BitrateReport
			for i := 0; i < secs; i++ {
				now = now.Add(time.Second)
				_, last = a.Observe(now, sample)
			}
			if last.Gate != tc.want {
				t.Fatalf("gate %q, want %q", last.Gate, tc.want)
			}
		})
	}
}

// A report has to carry the numbers the decision was made from, not the ones
// left behind after the window was reset. Several branches reset before
// returning, so a caller reading the adapter afterwards sees zeroes -- which
// is exactly the "nothing happened" reading the gate exists to disprove.
func TestReportCarriesTheEvidenceTheDecisionUsed(t *testing.T) {
	a := NewBitrateAdapter(kbps(3000))
	now := time.Now()
	var rep BitrateReport
	// 31_250 B/s == 250 kbps delivered, half the frames shed. Eleven ticks,
	// because the window anchors on the first and the decision lands on the
	// one where elapsed first reaches downWindow.
	for i := 0; i < 11; i++ {
		now = now.Add(time.Second)
		_, rep = a.Observe(now, BitrateSample{Frames: 20, Shed: 10, Bytes: 31_250})
	}
	if rep.Gate != GateDown {
		t.Fatalf("gate %q, want %q", rep.Gate, GateDown)
	}
	if rep.Frames == 0 || rep.Shed == 0 || rep.Bytes == 0 {
		t.Fatalf("report was emptied by the reset: frames=%d shed=%d bytes=%d",
			rep.Frames, rep.Shed, rep.Bytes)
	}
	if rep.ShedFrac < 0.49 || rep.ShedFrac > 0.51 {
		t.Fatalf("shedFrac %.3f, want ~0.5", rep.ShedFrac)
	}
	// The goodput is what the decision was, so it has to agree with it.
	if rep.To != kbps(250) {
		t.Fatalf("to %d bps, want the measured 250 kbps", rep.To)
	}
	if rep.From != kbps(3000) {
		t.Fatalf("from %d bps, want the configured 3000 kbps", rep.From)
	}
}

// An idle stretch inside the window must not be read as a slow link.
//
// From a live trace, same window, 49 seconds apart:
//
//	20:43:46  upWindow  windowMs 10999  frames 124  goodput 981,995
//	20:44:35  upWindow  windowMs 59999  frames 158  goodput 194,172
//
// 34 frames arrived in those 49 seconds -- an idle panel at its 10Hz floor,
// trap 20 -- and the measured goodput fell 5.06x with nothing about the link
// changing. Goodput divided by the whole wall-clock window, so every idle
// second counted as a second the link carried nothing, and bitrate.go used
// that figure directly as the new encoder bitrate. A session that idles and
// then sheds would therefore relaunch at the floor on a link that had just
// demonstrably carried megabits.
func TestAnIdleStretchDoesNotDepressTheMeasuredGoodput(t *testing.T) {
	const configured = 8_000_000
	a := NewBitrateAdapter(configured)
	start := time.Now()

	a.Observe(start, BitrateSample{})

	// Two seconds of motion carrying 5 Mbps, shedding a third of it.
	for i := 1; i <= 2; i++ {
		a.Observe(start.Add(time.Duration(i)*time.Second), BitrateSample{
			Frames: 60, Shed: 20, Bytes: 625_000,
		})
	}
	// Nine seconds of an idle screen: nothing produced, nothing carried.
	var next int
	var rep BitrateReport
	for i := 3; i <= 10; i++ {
		next, rep = a.Observe(start.Add(time.Duration(i)*time.Second), BitrateSample{})
	}

	if rep.Gate != GateDown {
		t.Fatalf("gate = %q, want %q (shedFrac %.2f over %dms)",
			rep.Gate, GateDown, rep.ShedFrac, rep.WindowMs)
	}
	// 1,250,000 bytes over the 2s that produced them is 5 Mbps. Over the
	// 11s window it reads 909 kbps, which is the bug.
	if next < 4_000_000 {
		t.Errorf("relaunched at %d bps; the link carried 5 Mbps during the "+
			"2s it was asked to carry anything", next)
	}
	if rep.GoodputBps < 4_000_000 {
		t.Errorf("reported goodput %d bps, want ~5 Mbps", rep.GoodputBps)
	}
}

// A viewer stuck awaiting a keyframe for a whole window is a starved link, and
// the controller has to act on it even though trap 27 excluded keyframe waits
// from the shed fraction.
//
// From a shaped run at 400 kbps against emulator-5554, 70 seconds:
//
//	framesOut     997 -> 4062   3065 produced
//	sent          973 -> 1102    129 delivered
//	shedAwaitKey   13 -> 2678   2665 suppressed
//	shedStale       1 ->  119    118
//	shedPacer       1 ->   13     12
//
// shedFrac was (12+118)/3065 = 0.04, under the 0.20 that would act, so every
// window returned inconclusive while the viewer ran at 1.8fps out of 60. The
// loop is self-sustaining: a stale frame latches needsKey, the keyframe that
// would clear it is the largest frame there is, it cannot land on a link this
// slow, and everything behind it is suppressed.
//
// Excluding keyframe waits from the shed fraction stays right -- one drop
// causes many waits, and trap 27 split the counters for that reason. What was
// missing is that a wait sustained across the whole window is not the echo of
// one drop, and it is the one state where the encoder is provably too big.
func TestAViewerStuckAwaitingAKeyframeIsTreatedAsAStarvedLink(t *testing.T) {
	const configured = 4_000_000
	a := NewBitrateAdapter(configured)
	start := time.Now()
	a.Observe(start, BitrateSample{})

	// The measured shape: 60 frames produced a second, nearly all suppressed,
	// a shed fraction well under the bar, and about 125 bytes a second of
	// parameter sets getting through -- every actual picture was reset in
	// flight. Bytes is what the link carried, so it is that trickle and not
	// the 31 kB a second the transport was handed.
	var next int
	var rep BitrateReport
	for i := 1; i <= 10; i++ {
		next, rep = a.Observe(start.Add(time.Duration(i)*time.Second), BitrateSample{
			Frames: 60, Shed: 2, AwaitKey: 52, Bytes: 125,
		})
	}

	if rep.ShedFrac >= downShedFraction {
		t.Fatalf("shedFrac %.2f is above the bar, so this is not testing the "+
			"gap it is meant to", rep.ShedFrac)
	}
	if rep.Gate != GateStarved {
		t.Fatalf("gate = %q, want %q (awaitFrac %.2f over %dms)",
			rep.Gate, GateStarved, rep.AwaitFrac, rep.WindowMs)
	}
	// One step to the floor, not four. Halving took 22s a step and spent 85 of
	// 90 impaired seconds still oversubscribed; a link carrying 1 kbps is not
	// a near miss on 4 Mbps and does not need to be approached.
	if next != minAdaptiveBitRate {
		t.Errorf("relaunched at %d, want the floor %d", next, minAdaptiveBitRate)
	}
}

// Goodput is measured over the intervals the encoder produced in, so a short
// window holding one large frame can read above the rate the encoder is set
// to. Aiming at it directly would turn a starved window into an increase, which
// is the one direction this branch must never move.
func TestAStarvedWindowNeverRaisesTheBitrate(t *testing.T) {
	const configured = 4_000_000
	a := NewBitrateAdapter(configured)
	start := time.Now()
	a.Observe(start, BitrateSample{})

	var next int
	var rep BitrateReport
	for i := 1; i <= 10; i++ {
		next, rep = a.Observe(start.Add(time.Duration(i)*time.Second), BitrateSample{
			Frames: 60, Shed: 2, AwaitKey: 52, Bytes: 4_000_000,
		})
	}

	if rep.Gate != GateStarved {
		t.Fatalf("gate = %q, want %q", rep.Gate, GateStarved)
	}
	if got := rep.GoodputBps; got <= configured {
		t.Fatalf("goodput %d does not exceed the configured %d, so this is not "+
			"testing the cap", got, configured)
	}
	if next != configured/2 {
		t.Errorf("relaunched at %d, want the halving cap %d", next, configured/2)
	}
}

// The counterweight: keyframe waits must not act on their own when the window
// is mostly healthy, or trap 27's objection stands and one drop argues sixteen
// times over.
func TestABriefKeyframeWaitDoesNotChangeTheEncoder(t *testing.T) {
	a := NewBitrateAdapter(4_000_000)
	start := time.Now()
	a.Observe(start, BitrateSample{})

	var next int
	for i := 1; i <= 10; i++ {
		s := BitrateSample{Frames: 60, Shed: 1, Bytes: 500_000}
		if i == 3 {
			// One drop, and the sixteen suppressed frames behind it.
			s.AwaitKey = 16
		}
		next, _ = a.Observe(start.Add(time.Duration(i)*time.Second), s)
	}
	if next != 0 {
		t.Errorf("changed the encoder to %d on one drop's worth of waiting", next)
	}
}

// starvedSample is the 400 kbps shaped-link shape the starvation tests use:
// nearly every frame suppressed awaiting a keyframe, and a trickle of
// parameter sets getting through.
var starvedSample = BitrateSample{Frames: 60, Shed: 2, AwaitKey: 52, Bytes: 125}

// cleanSample is a link carrying 3 Mbps with nothing shed, clean at any rate
// up to the 4 Mbps default.
var cleanSample = BitrateSample{Frames: 60, Shed: 0, Bytes: 375_000}

// A live change disturbs nothing, so the step up follows the drop by the live
// windows rather than a relaunch's. The same run on a relaunch adapter must
// still be waiting, or the test is not showing a difference.
func TestALiveAdapterProbesUpSoonAfterADrop(t *testing.T) {
	span := int((liveSettleWindow + liveUpWindow).Seconds()) + 2
	for _, tc := range []struct {
		name   string
		a      *BitrateAdapter
		wantUp bool
	}{
		{"live", NewLiveBitrateAdapter(4_000_000), true},
		{"relaunch", NewBitrateAdapter(4_000_000), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			down, now := feed(tc.a, time.Now(), 11, starvedSample)
			if down != minAdaptiveBitRate {
				t.Fatalf("setup came down to %d, want the floor %d", down, minAdaptiveBitRate)
			}
			up, _ := feed(tc.a, now, span, cleanSample)
			if gotUp := up > down; gotUp != tc.wantUp {
				t.Fatalf("%ds clean after the drop: %d -> %d, want a step up %v",
					span, down, up, tc.wantUp)
			}
		})
	}
}

// The measured case the relaunch windows left stranded: a 400 kbps link takes
// the 4 Mbps default to the floor, then clears. Seven steps of half get back
// to 4 Mbps, and the relaunch cap of six stopped short of that for the rest of
// the session. The live adapter must reach the ceiling, and within about two
// minutes rather than the eight a relaunch's windows took.
func TestALiveAdapterRecoversFromTheFloorToTheCeiling(t *testing.T) {
	const configured = 4_000_000
	a := NewLiveBitrateAdapter(configured)
	down, now := feed(a, time.Now(), 11, starvedSample)
	if down != minAdaptiveBitRate {
		t.Fatalf("setup came down to %d, want the floor %d", down, minAdaptiveBitRate)
	}
	start := now
	var reached time.Duration
	for i := 0; i < 300 && reached == 0; i++ {
		now = now.Add(time.Second)
		a.Observe(now, cleanSample)
		if a.Current() == configured {
			reached = now.Sub(start)
		}
	}
	if reached == 0 {
		t.Fatalf("stopped at %d bps after 300s clean, want the configured %d "+
			"(%d changes)", a.Current(), configured, a.changes)
	}
	if a.changes <= maxAdaptiveChanges {
		t.Fatalf("recovered in %d changes, within the relaunch cap of %d, so "+
			"this is not testing the cap", a.changes, maxAdaptiveChanges)
	}
	if reached > 2*time.Minute {
		t.Fatalf("took %v from the floor to the ceiling, want under two minutes", reached)
	}
}

// Shortening the windows must not shorten how much evidence a decision needs.
// downWindow is shared, because telling the link from a burst takes as long
// whichever way the change is applied, and a step up still needs a longer
// clean run than a step down needs a bad one.
func TestALiveAdapterStillNeedsTheFullWindowToAct(t *testing.T) {
	a := NewLiveBitrateAdapter(3_000_000)
	// Nine ticks after the anchor: one second short of downWindow.
	got, now := feed(a, time.Now(), int(downWindow.Seconds()), BitrateSample{Frames: 20, Shed: 10, Bytes: 31_250})
	if got != 0 {
		t.Fatalf("a live adapter came down to %d bps before downWindow", got)
	}
	if liveUpWindow <= downWindow {
		t.Fatalf("liveUpWindow %v is not longer than downWindow %v", liveUpWindow, downWindow)
	}

	b := NewLiveBitrateAdapter(3_000_000)
	down, now := feed(b, now, 11, BitrateSample{Frames: 20, Shed: 10, Bytes: 31_250})
	if down == 0 {
		t.Fatal("setup did not come down")
	}
	short := int((liveSettleWindow + liveUpWindow).Seconds()) - 2
	if up, _ := feed(b, now, short, cleanSample); up != 0 {
		t.Fatalf("probed up to %d bps before liveUpWindow elapsed", up)
	}
}

// A live change settles too, just not for as long: the interval the change
// landed in mixes bytes from both rates, and reading it as the new rate's
// first measurement would misjudge it.
func TestALiveAdapterSettlesAfterAChange(t *testing.T) {
	a := NewLiveBitrateAdapter(3_000_000)
	first, now := feed(a, time.Now(), 11, BitrateSample{Frames: 20, Shed: 10, Bytes: 125_000})
	if first == 0 {
		t.Fatal("no first change")
	}
	now = now.Add(time.Second)
	if _, rep := a.Observe(now, starvedSample); rep.Gate != GateSettling {
		t.Fatalf("one second after a live change, gate %q, want %q", rep.Gate, GateSettling)
	}
	now = now.Add(liveSettleWindow)
	if _, rep := a.Observe(now, starvedSample); rep.Gate == GateSettling {
		t.Fatalf("still settling %v after a live change", liveSettleWindow+time.Second)
	}
}

// A live change costs no restart, but a link on the threshold still hunts, and
// each cycle of that is a step up followed by a window of shedding. The live
// cap is larger, and within its period it is still a cap.
func TestALiveAdapterStopsChangingAfterItsCap(t *testing.T) {
	a := NewLiveBitrateAdapter(kbps(3000))
	start := time.Now()
	now := start
	changes := 0
	for now.Sub(start) < liveCapPeriod-time.Minute {
		got, next := feed(a, now, 12, BitrateSample{Frames: 20, Shed: 10, Bytes: 31_250})
		now = next
		if got != 0 {
			changes++
		}
		got, next = feed(a, now, int(liveUpWindow.Seconds())+4, cleanSample)
		now = next
		if got != 0 {
			changes++
		}
	}
	if changes <= maxAdaptiveChanges {
		t.Fatalf("made %d changes, no more than the relaunch cap of %d", changes, maxAdaptiveChanges)
	}
	if changes > maxLiveAdaptiveChanges {
		t.Fatalf("made %d changes inside %v, live cap is %d", changes, liveCapPeriod, maxLiveAdaptiveChanges)
	}
}

// A simulator session can outlast any number of congestion episodes. Once the
// cap's period has passed, the changes that spent it no longer count, so an
// episode an hour later is still met.
func TestALiveAdapterCapRollsOffWithItsPeriod(t *testing.T) {
	a := NewLiveBitrateAdapter(kbps(3000))
	now := time.Now()
	for a.recentChanges(now) < maxLiveAdaptiveChanges {
		_, now = feed(a, now, 12, BitrateSample{Frames: 20, Shed: 10, Bytes: 31_250})
		_, now = feed(a, now, int(liveUpWindow.Seconds())+4, cleanSample)
	}
	spent := a.changes

	// Still capped a moment later, whatever the link does.
	if _, rep := a.Observe(now.Add(time.Second), starvedSample); rep.Gate != GateCapped {
		t.Fatalf("gate %q with the cap spent, want %q", rep.Gate, GateCapped)
	}

	// An hour on, a starved link is acted on again.
	later := now.Add(time.Hour)
	a.Reset(later)
	got, _ := feed(a, later, 11, starvedSample)
	if got == 0 {
		t.Fatalf("no change an hour after the cap was spent (%d changes, %d recent)",
			a.changes, a.recentChanges(later))
	}
	if a.changes != spent+1 {
		t.Fatalf("changes %d, want %d: the session total keeps counting", a.changes, spent+1)
	}
}

// A relaunched encoder keeps its lifetime cap: every change there is a restart
// the viewer sees, and nothing about that improves with time.
func TestARelaunchAdapterCapNeverRollsOff(t *testing.T) {
	a := NewBitrateAdapter(kbps(3000))
	now := time.Now()
	for a.changes < maxAdaptiveChanges {
		_, now = feed(a, now, 12, BitrateSample{Frames: 20, Shed: 10, Bytes: 31_250})
		_, now = feed(a, now, int(upWindow.Seconds())+2, cleanSample)
	}
	later := now.Add(24 * time.Hour)
	a.Reset(later)
	if got, _ := feed(a, later, 11, starvedSample); got != 0 {
		t.Fatalf("changed to %d bps a day after spending the relaunch cap", got)
	}
}

// The gap between the two step-down gates, as two cellular sessions read it on
// 20 and 21 October. Pixel_10 over DERP: the encoder produced about 14 frames
// a second, a third of them were suppressed awaiting a keyframe, almost none
// were shed by the pacer, and the link carried 340-667 kbps against 4 Mbps.
// shedFrac 0.008-0.079 is under downShedFraction and awaitFrac 0.28-0.44 under
// starvedAwaitFraction, so every window fell through to upWindow and then
// inconclusive, the encoder stayed at 4 Mbps, and the viewer decoded 0-9fps
// until the page was reloaded -- and after the reload as well.
func TestALinkCarryingAFractionOfTheBitrateStepsDown(t *testing.T) {
	cases := []struct {
		name   string
		build  func() *BitrateAdapter
		sample BitrateSample
		want   int
	}{
		{
			name:  "Pixel_10 on cellular, relaunch",
			build: func() *BitrateAdapter { return NewBitrateAdapter(kbps(4000)) },
			// 56_250 B/s is 450 kbps.
			sample: BitrateSample{Frames: 15, Shed: 1, AwaitKey: 5, Bytes: 56_250},
			want:   kbps(450),
		},
		{
			name:  "a simulator on cellular, live",
			build: func() *BitrateAdapter { return NewLiveBitrateAdapter(kbps(4000)) },
			// 125_000 B/s is 1000 kbps; shed 3%, awaitFrac 0.40.
			sample: BitrateSample{Frames: 60, Shed: 2, AwaitKey: 24, Bytes: 125_000},
			want:   kbps(1000),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.build()
			now := time.Now()
			var next int
			var rep BitrateReport
			for i := 0; i <= int(downWindow.Seconds()); i++ {
				now = now.Add(time.Second)
				next, rep = a.Observe(now, tc.sample)
			}
			if rep.ShedFrac >= downShedFraction || rep.AwaitFrac >= starvedAwaitFraction {
				t.Fatalf("shedFrac %.2f awaitFrac %.2f reach an existing gate, so "+
					"this is not testing the gap", rep.ShedFrac, rep.AwaitFrac)
			}
			if rep.Gate != GateUndercarried {
				t.Fatalf("gate %q, want %q", rep.Gate, GateUndercarried)
			}
			if lo, hi := tc.want*9/10, tc.want*11/10; next < lo || next > hi {
				t.Errorf("relaunched at %d, want about the goodput %d", next, tc.want)
			}
		})
	}
}

// After a step down to goodput the pacer can go clean while the viewer is
// still waiting on keyframes a third of the time. That is not recovery, and
// probing up from it puts back the bitrate that starved the link.
func TestNoStepUpWhileTheViewerWaitsOnKeyframes(t *testing.T) {
	a := NewLiveBitrateAdapter(kbps(4000))
	a.current = kbps(1000)
	// 1000 kbps carried, nothing shed, awaitFrac 0.15: above upShedFraction.
	got, _ := feed(a, time.Now(), 120, BitrateSample{Frames: 60, Shed: 0, AwaitKey: 9, Bytes: 125_000})
	if got != 0 {
		t.Errorf("stepped up to %d while the viewer waited on keyframes", got)
	}
}

// The Flipcash simulator on 30 October: a static chat at 2fps, then one
// 951ms round trip and two seconds of frames suppressed awaiting a keyframe.
// The window read awaitFrac 0.62 over 92 frames, and the starved gate cut 4
// Mbps to 256 kbps -- what a static screen produces, not what the link can
// carry -- and the size ladder to 248x544. The waits filled 2 of 11 seconds;
// starvation fills the window.
func TestABurstOfKeyframeWaitsOnAnIdleScreenIsNotStarvation(t *testing.T) {
	a := NewLiveBitrateAdapter(4_000_000)
	start := time.Now()
	a.Observe(start, BitrateSample{})
	idle := BitrateSample{Frames: 2, Bytes: 32_000}
	samples := []BitrateSample{idle, idle, idle, idle, idle, idle, idle, idle, idle,
		{Frames: 15, Shed: 1, AwaitKey: 11, Bytes: 32_000},
		{Frames: 58, Shed: 3, AwaitKey: 46, Bytes: 32_000},
	}
	for i, s := range samples {
		// The trace judged the window at windowMs 10999: the samples ran a
		// little under a second apart, so the tenth landed just short of
		// downWindow and the eleventh decided.
		at := time.Duration(i+1) * 999 * time.Millisecond
		if i == len(samples)-1 {
			at = 10999 * time.Millisecond
		}
		next, rep := a.Observe(start.Add(at), s)
		if next != 0 {
			t.Fatalf("second %d: %s changed the encoder to %d (awaitFrac %.2f over %d frames)",
				i+1, rep.Gate, next, rep.AwaitFrac, rep.Frames)
		}
	}
}
