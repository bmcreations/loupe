package session

import (
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// The 13:23 trace on 5G (phone, sim:77763E2D): round trip flat at 100-130ms
// against an ~82ms base, the link carrying 2-3 Mbps of a 4 Mbps encoder, and
// health reading "delivered 23 of 60", "30 of 58" and "23 of 58" second after
// second. Each of those seconds held one or two stale sheds and the 20-33
// keyframe-wait frames behind them.
const lossBase = 82 * time.Millisecond

// lossy is the 13:23:26 second: 60 produced, 23 decoded, 24 sent whole, two
// stale sheds. 300 kB/s is the 2.4 Mbps the link carried.
func lossy() BitrateSample {
	return BitrateSample{
		Frames: 58, Bytes: 300_000, RTT: 101 * time.Millisecond, BaseRTT: lossBase,
		Delivery: DeliveryReading{Valid: true, Decoded: true, Produced: 60, Delivered: 23, Allowance: 2, Sent: 24, Events: 2},
	}
}

func observeAll(a *BitrateAdapter, now time.Time, ss ...BitrateSample) (int, BitrateReport, time.Time) {
	var got int
	var rep BitrateReport
	for _, s := range ss {
		now = now.Add(time.Second)
		n, r := a.Observe(now, s)
		if n != 0 {
			got = n
		}
		rep = r
	}
	return got, rep, now
}

func TestSustainedLossWithFlatRTTCutsToTheCarriedRate(t *testing.T) {
	a := NewLiveBitrateAdapter(4_000_000)
	now := time.Now()
	a.Observe(now, lossy()) // anchor
	got, rep, _ := observeAll(a, now, lossy(), lossy())
	if rep.Gate != GateLoss {
		t.Fatalf("gate %q, want %q", rep.Gate, GateLoss)
	}
	// 0.8 of the 2.4 Mbps the two lossy seconds carried.
	if got < 1_800_000 || got > 2_050_000 {
		t.Errorf("cut to %d, want about 1.92 Mbps", got)
	}
	if a.changes != 0 {
		t.Errorf("a loss cut counted against the change cap: %d", a.changes)
	}
	if rep.Detail["deliveredFrac"] == nil || rep.Detail["runBytes"] != uint64(600_000) {
		t.Errorf("detail %v lacks the cut's inputs", rep.Detail)
	}
	if !a.afterDelay {
		t.Error("recovery after a loss cut does not wait for delivery")
	}
}

func TestOneSecondOfLossDoesNotCut(t *testing.T) {
	a := NewLiveBitrateAdapter(4_000_000)
	now := time.Now()
	a.Observe(now, lossy())
	healthy := lossy()
	healthy.Delivery = DeliveryReading{Valid: true, Decoded: true, Produced: 58, Delivered: 58, Sent: 58}
	if got, _, _ := observeAll(a, now, lossy(), healthy, lossy(), healthy); got != 0 {
		t.Fatalf("cut to %d on lossy seconds that never ran two in a row", got)
	}
}

// 13:24:29: one abandon, and the next second is only the keyframe wait behind
// it. The wait frames were never sent and carry no new event.
func TestOneAbandonAndItsKeyframeWaitIsNotTwoLosses(t *testing.T) {
	a := NewLiveBitrateAdapter(4_000_000)
	now := time.Now()
	abandon := BitrateSample{Frames: 34, Bytes: 100_000, RTT: 133 * time.Millisecond, BaseRTT: lossBase,
		Delivery: DeliveryReading{Valid: true, Decoded: true, Produced: 34, Delivered: 10, Allowance: 2, Sent: 3, Events: 1, Abandons: 1}}
	wait := BitrateSample{Frames: 58, Bytes: 60_000, RTT: 127 * time.Millisecond, BaseRTT: lossBase,
		Delivery: DeliveryReading{Valid: true, Decoded: true, Produced: 58, Delivered: 30, Allowance: 2, Sent: 31, Events: 0}}
	a.Observe(now, wait)
	if got, rep, _ := observeAll(a, now, abandon, wait); got != 0 {
		t.Fatalf("cut to %d (%s): the wait behind one abandon counted as a second loss", got, rep.Gate)
	}
}

// A long wait with no new stale shed or abandon is the starved and
// undercarried gates' to judge over their window, not this one's.
func TestAKeyframeWaitAloneIsNotLoss(t *testing.T) {
	a := NewLiveBitrateAdapter(4_000_000)
	now := time.Now()
	wait := BitrateSample{Frames: 58, Bytes: 40_000, RTT: 101 * time.Millisecond, BaseRTT: lossBase,
		Delivery: DeliveryReading{Valid: true, Decoded: true, Produced: 60, Delivered: 15, Allowance: 2, Sent: 16}}
	a.Observe(now, wait)
	for i := 0; i < 4; i++ {
		var n int
		var rep BitrateReport
		n, rep, now = observeAll(a, now, wait)
		if rep.Gate == GateLoss {
			t.Fatalf("second %d: loss cut to %d from a wait with no loss event", i, n)
		}
	}
}

// What the client failed to receive of what was sent, with no stale shed in
// the second: 55 sent whole, 20 decoded.
func TestFramesSentButNotReceivedCut(t *testing.T) {
	a := NewLiveBitrateAdapter(4_000_000)
	now := time.Now()
	s := lossy()
	s.Delivery = DeliveryReading{Valid: true, Decoded: true, Produced: 60, Delivered: 20, Allowance: 2, Sent: 55}
	a.Observe(now, s)
	if _, rep, _ := observeAll(a, now, s, s); rep.Gate != GateLoss {
		t.Fatalf("gate %q, want %q", rep.Gate, GateLoss)
	}
}

func TestLossOnAQueueingLinkIsLeftToTheDelayGate(t *testing.T) {
	a := NewLiveBitrateAdapter(4_000_000)
	now := time.Now()
	s := lossy()
	s.RTT = 400 * time.Millisecond
	a.Observe(now, s)
	got, rep, _ := observeAll(a, now, s, s)
	if got == 0 || rep.Gate != GateDelay {
		t.Fatalf("gate %q cut %d, want the delay gate to take a queueing link", rep.Gate, got)
	}
}

func TestLossOnAnIdleScreenDoesNotCut(t *testing.T) {
	a := NewLiveBitrateAdapter(4_000_000)
	now := time.Now()
	s := lossy()
	s.Frames = 2
	a.Observe(now, s)
	if got, _, _ := observeAll(a, now, s, s, s, s); got != 0 {
		t.Fatalf("cut to %d on an idle screen", got)
	}
}

func TestAMissingReportNeitherExtendsNorEndsTheRun(t *testing.T) {
	a := NewLiveBitrateAdapter(4_000_000)
	now := time.Now()
	none := lossy()
	none.Delivery = DeliveryReading{}
	a.Observe(now, lossy())
	if got, _, _ := observeAll(a, now, lossy(), none, lossy()); got == 0 {
		t.Fatal("a second with no client report ended the run")
	}
}

func TestLossCutsAreRateLimitedLikeDelayCuts(t *testing.T) {
	a := NewBitrateAdapter(4_000_000) // relaunch: 3 per 10 minutes
	now := time.Now()
	a.Observe(now, lossy())
	for i := 0; i < a.windows.maxDelayCuts; i++ {
		a.lossAt = append(a.lossAt, now)
	}
	got, _, now := observeAll(a, now, lossy(), lossy())
	if got != 0 {
		t.Fatalf("cut to %d past the limit", got)
	}
	a.lossAt = nil
	if got, _, _ := observeAll(a, now, lossy(), lossy()); got == 0 {
		t.Fatal("no cut with the limit clear")
	}
}

func TestALossCutDoesNotUseUpTheDelayLimit(t *testing.T) {
	a := NewLiveBitrateAdapter(4_000_000)
	now := time.Now()
	a.Observe(now, lossy())
	observeAll(a, now, lossy(), lossy())
	if len(a.lossAt) != 1 || len(a.delayAt) != 0 {
		t.Fatalf("lossAt %d delayAt %d, want 1 and 0", len(a.lossAt), len(a.delayAt))
	}
}

// Undercarried gate: no resize until the bitrate is at its floor.

func undercarried(bytes uint64) BitrateSample {
	return BitrateSample{Frames: 15, AwaitKey: 5, Bytes: bytes}
}

func TestUndercarriedAtTheFloorStepsTheSizeOnlyOnTheSecondWindow(t *testing.T) {
	a := NewLiveBitrateAdapter(4_000_000)
	a.current = minAdaptiveBitRate
	now := time.Now()
	a.Observe(now, undercarried(10_000))
	first, rep1, now := observeAll(a, now, repeat(undercarried(10_000), int(downWindow.Seconds()))...)
	if first != 0 || rep1.Gate != GateTooSmall {
		t.Fatalf("first window at the floor: next %d gate %q, want no change (tooSmall)", first, rep1.Gate)
	}
	second, rep2, _ := observeAll(a, now, repeat(undercarried(10_000), int(downWindow.Seconds()))...)
	if second != minAdaptiveBitRate || rep2.Gate != GateUndercarried || rep2.From != rep2.To {
		t.Fatalf("second window: next %d gate %q from %d to %d, want a size-only step at the floor",
			second, rep2.Gate, rep2.From, rep2.To)
	}
}

func TestARelaunchAdapterNeverAsksForASizeOnlyStep(t *testing.T) {
	a := NewBitrateAdapter(4_000_000)
	a.current = minAdaptiveBitRate
	now := time.Now()
	a.Observe(now, undercarried(10_000))
	for i := 0; i < 3; i++ {
		var got int
		got, _, now = observeAll(a, now, repeat(undercarried(10_000), int(downWindow.Seconds()))...)
		if got != 0 {
			t.Fatalf("relaunch adapter asked for %d at the floor", got)
		}
	}
}

func repeat(s BitrateSample, n int) []BitrateSample {
	out := make([]BitrateSample, n)
	for i := range out {
		out[i] = s
	}
	return out
}

func TestDeliveryReadingsAreReadOnceAndTheWorstViewerWins(t *testing.T) {
	st := &liveStream{swapStream: newSwapStream(), live: true}
	s, _ := openLive(t, target.KindSimulator, st)
	a, _ := s.AddViewer()
	b, _ := s.AddViewer()
	good := DeliveryReading{Valid: true, Decoded: true, Produced: 60, Delivered: 58, Sent: 60}
	bad := DeliveryReading{Valid: true, Decoded: true, Produced: 60, Delivered: 20, Sent: 60, Events: 1}
	s.NoteDelivery(a, good)
	s.NoteDelivery(b, bad)
	if got := s.takeDelivery(); got.Delivered != 20 {
		t.Fatalf("took %+v, want the worse viewer's reading", got)
	}
	if got := s.takeDelivery(); got.Valid {
		t.Fatalf("the same readings were taken twice: %+v", got)
	}
}

// The 5G trace phone that decoded 0 frames for its first 6s (decFps 0,
// decodeErrors 0) while receiving 2.4 Mbps read deliveredFrac 0.142 and was cut
// 4M -> 1.94M. Until the client has decoded a frame, a delivered count of 0 is
// a decoder that has not started.
func TestLossGateWaitsForTheFirstDecodedFrame(t *testing.T) {
	cases := []struct {
		name    string
		decoded bool
		wantCut bool
	}{
		{"nothing decoded yet", false, false},
		{"a frame has been decoded", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := lossy()
			s.Delivery.Decoded = tc.decoded
			s.Delivery.Delivered = 8 // 0.142 of 60 with the allowance
			a := NewLiveBitrateAdapter(4_000_000)
			now := time.Now()
			a.Observe(now, s)
			got, rep, _ := observeAll(a, now, s, s, s)
			if cut := got != 0; cut != tc.wantCut {
				t.Fatalf("cut to %d (%s), want cut=%v", got, rep.Gate, tc.wantCut)
			}
		})
	}
}

func TestSkippedLossIsTracedOncePerQuietPeriod(t *testing.T) {
	s := lossy()
	s.Delivery.Decoded = false
	a := NewLiveBitrateAdapter(4_000_000)
	now := time.Now()
	a.Observe(now, s)
	var traced []BitrateReport
	for i := 0; i < 25; i++ {
		now = now.Add(time.Second)
		a.Observe(now, s)
		if r, ok := a.TakeSkip(); ok {
			traced = append(traced, r)
		}
	}
	// Seconds 1, 12 and 23: the first at once, then one per lossSkipEvery.
	if len(traced) != 3 {
		t.Fatalf("%d skip records in 25s, want 3", len(traced))
	}
	if traced[0].Gate != GateLossSkipped || traced[0].Detail["why"] != "noDecodeYet" {
		t.Errorf("record %+v, want gate lossSkipped why noDecodeYet", traced[0])
	}
	// A healthy reading with no decode yet declines nothing and says nothing.
	h := s
	h.Delivery = DeliveryReading{Valid: true, Produced: 5, Delivered: 0, Sent: 5}
	b := NewLiveBitrateAdapter(4_000_000)
	b.Observe(now, h)
	b.Observe(now.Add(time.Second), h)
	if _, ok := b.TakeSkip(); ok {
		t.Error("a skip was traced for a reading that was not lossy")
	}
}

func TestADecodingViewerIsNotHiddenByOneThatHasNotDecoded(t *testing.T) {
	st := &liveStream{swapStream: newSwapStream(), live: true}
	s, _ := openLive(t, target.KindSimulator, st)
	a, _ := s.AddViewer()
	b, _ := s.AddViewer()
	s.NoteDelivery(a, DeliveryReading{Valid: true, Decoded: true, Produced: 60, Delivered: 50, Sent: 60})
	s.NoteDelivery(b, DeliveryReading{Valid: true, Produced: 60, Delivered: 0, Sent: 60})
	if got := s.takeDelivery(); !got.Decoded || got.Delivered != 50 {
		t.Fatalf("took %+v, want the decoding viewer's reading", got)
	}
}
