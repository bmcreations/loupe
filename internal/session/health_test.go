package session

import (
	"strings"
	"testing"
	"time"
)

func TestAnIdlePanelIsHealthy(t *testing.T) {
	v, _ := Health(HealthInput{Produced: 6, Delivered: 6, BaseMs: 12, RttMs: 20, ReportAge: time.Second})
	if v != "good" {
		t.Fatalf("idle panel = %s; want good (trap 20: 6fps is correct when nothing moves)", v)
	}
}

func TestALowDeliveryRatioIsBad(t *testing.T) {
	v, why := Health(HealthInput{Produced: 60, Delivered: 30, BaseMs: 12, RttMs: 20, ReportAge: time.Second})
	if v != "bad" || !strings.Contains(why, "30 of 60") {
		t.Fatalf("got %s %q", v, why)
	}
}

func TestRttIsJudgedAgainstTheBaseline(t *testing.T) {
	// 240ms on a 233ms-baseline path is healthy; that path was trap 5.
	v, _ := Health(HealthInput{Produced: 60, Delivered: 60, BaseMs: 233, RttMs: 240, ReportAge: time.Second})
	if v != "good" {
		t.Fatalf("slow healthy path = %s; want good", v)
	}
	v, _ = Health(HealthInput{Produced: 60, Delivered: 60, BaseMs: 10, RttMs: 140, ReportAge: time.Second})
	if v != "bad" {
		t.Fatalf("130ms over base = %s; want bad", v)
	}
}

// The boundary is inclusive, matching web/health.js's `>= STALE_AFTER_MS`: a
// report exactly 2.5s old reads stale, and one microsecond younger is still
// current.
func TestStaleBoundaryIsGreaterThanOrEqual(t *testing.T) {
	v, _ := Health(HealthInput{Produced: 60, Delivered: 60, BaseMs: 12, RttMs: 20, ReportAge: 2500*time.Millisecond - time.Microsecond})
	if v != "good" {
		t.Fatalf("report age 2500ms-1us = %s; want good", v)
	}
	v, _ = Health(HealthInput{Produced: 60, Delivered: 60, BaseMs: 12, RttMs: 20, ReportAge: 2500 * time.Millisecond})
	if v != "stale" {
		t.Fatalf("report age exactly 2500ms = %s; want stale", v)
	}
}

func TestAnOldReportIsNoReport(t *testing.T) {
	v, why := Health(HealthInput{Produced: 60, Delivered: 60, ReportAge: 3 * time.Second})
	if v != "stale" || why != "no report" {
		t.Fatalf("3s-old report = %s %q; want stale, \"no report\"", v, why)
	}
}

// A stale report wins even when every live input would otherwise read bad --
// trap 26 is that an old reading is no reading, not a stale confirmation of
// whatever it last said.
func TestStaleWinsOverEveryOtherSignal(t *testing.T) {
	v, why := Health(HealthInput{
		Produced: 60, Delivered: 0, ShedRate: 1000, BaseMs: 10, RttMs: 900,
		ReportAge: 3 * time.Second,
	})
	if v != "stale" || why != "no report" {
		t.Fatalf("got %s %q; want stale, \"no report\"", v, why)
	}
}

// Shed rate, pinned from both sides at each boundary named in the plan's
// table (4 good, 5 degraded, 19 degraded, 20 bad).
func TestShedRateBoundaries(t *testing.T) {
	cases := []struct {
		shed uint64
		want string
	}{
		{4, "good"},
		{5, "degraded"},
		{19, "degraded"},
		{20, "bad"},
	}
	for _, c := range cases {
		v, why := Health(HealthInput{Produced: 60, Delivered: 60, ShedRate: c.shed, BaseMs: 12, RttMs: 20, ReportAge: time.Second})
		if v != c.want {
			t.Errorf("shed %d = %s; want %s", c.shed, v, c.want)
		}
		if c.want != "good" && !strings.Contains(why, "shed") {
			t.Errorf("shed %d reason %q does not name shed", c.shed, why)
		}
	}
}

// Delivery ratio boundaries: 0.9 itself is still good, just under it is
// degraded; 0.6 itself is still degraded, just under it is bad.
func TestDeliveryRatioBoundaries(t *testing.T) {
	cases := []struct {
		delivered int
		want      string
	}{
		{54, "good"},     // 54/60 = 0.900 exactly
		{53, "degraded"}, // 0.883
		{36, "degraded"}, // 36/60 = 0.600 exactly
		{35, "bad"},      // 0.583
	}
	for _, c := range cases {
		v, _ := Health(HealthInput{Produced: 60, Delivered: c.delivered, BaseMs: 12, RttMs: 20, ReportAge: time.Second})
		if v != c.want {
			t.Errorf("delivered %d/60 = %s; want %s", c.delivered, v, c.want)
		}
	}
}

// Finer-grained than TestDeliveryRatioBoundaries: at coarse denominators
// (produced=60) a threshold that has drifted by a percent or two can still
// land on the same side of every case above. A larger denominator pins the
// exact value of degradedRatio and badRatio, not just their neighbourhood.
func TestDeliveryRatioBoundariesAtFineGrain(t *testing.T) {
	cases := []struct {
		produced, delivered int
		want                string
	}{
		{1000, 900, "good"},     // ratio 0.900 exactly
		{1000, 899, "degraded"}, // 0.899, just under degradedRatio
		{1000, 600, "degraded"}, // ratio 0.600 exactly
		{1000, 599, "bad"},      // 0.599, just under badRatio
	}
	for _, c := range cases {
		v, _ := Health(HealthInput{Produced: c.produced, Delivered: c.delivered, BaseMs: 12, RttMs: 20, ReportAge: time.Second})
		if v != c.want {
			t.Errorf("delivered %d/%d = %s; want %s", c.delivered, c.produced, v, c.want)
		}
	}
}

// Too few frames produced to judge a ratio from, for this one call: trap 20's
// idle panel can legitimately produce a handful of frames a second, and a
// single missed one would otherwise swing the ratio to "bad". This pins the
// pure-function gate in isolation; it is not the whole story for a sustained
// low rate spread across many ticks -- see
// internal/httpapi's TestSustainedLowProducedEventuallyReadsBad, which drives
// healthTracker's window across several ticks and checks that Produced
// accumulates instead of resetting to below the gate every tick.
func TestTooFewFramesToJudgeIsGood(t *testing.T) {
	v, _ := Health(HealthInput{Produced: 4, Delivered: 0, BaseMs: 12, RttMs: 20, ReportAge: time.Second})
	if v != "good" {
		t.Fatalf("produced 4, delivered 0 = %s; want good (too few frames to judge)", v)
	}
}

// The boundary sits at 5, not 4: once there are enough frames to judge, a
// fully-missed second reads as bad.
func TestFiveFramesIsEnoughToJudge(t *testing.T) {
	v, _ := Health(HealthInput{Produced: 5, Delivered: 0, BaseMs: 12, RttMs: 20, ReportAge: time.Second})
	if v != "bad" {
		t.Fatalf("produced 5, delivered 0 = %s; want bad", v)
	}
}

// Rtt excess boundaries, matching web/app.js's retired grade(el.mRtt, ..., 60, 120).
func TestRttExcessBoundaries(t *testing.T) {
	cases := []struct {
		rttMs float64
		want  string
	}{
		{69, "good"},     // 69 - 10 = 59, just under 60
		{70, "degraded"}, // exactly 60 over base
		{129, "degraded"},
		{130, "bad"}, // exactly 120 over base
	}
	for _, c := range cases {
		v, _ := Health(HealthInput{Produced: 60, Delivered: 60, BaseMs: 10, RttMs: c.rttMs, ReportAge: time.Second})
		if v != c.want {
			t.Errorf("rtt %vms over 10ms base = %s; want %s", c.rttMs, v, c.want)
		}
	}
}

// The worst input wins regardless of which one it is: a bad rtt outranks a
// merely-degraded shed rate.
func TestTheWorstInputWins(t *testing.T) {
	v, why := Health(HealthInput{
		Produced: 60, Delivered: 60, ShedRate: 6, // degraded on its own
		BaseMs: 10, RttMs: 200, // bad on its own: 190ms over base
		ReportAge: time.Second,
	})
	if v != "bad" || !strings.Contains(why, "rtt") {
		t.Fatalf("got %s %q; want bad naming rtt", v, why)
	}
}

// Among inputs that tie in severity, the fixed evaluation order in Health's
// doc comment -- delivery ratio, then shed rate, then rtt -- decides which
// reason is reported. This is a design choice (the plan does not pin a tie
// order), but it has to be deterministic and explainable, so it is pinned
// here: reversing precedence between any two of the three fails this test.
func TestTiedSeverityNamesTheFirstCheckedInput(t *testing.T) {
	// Delivery ratio degraded (0.883) and shed rate degraded (6) at once:
	// delivery wins because it is checked first.
	v, why := Health(HealthInput{Produced: 60, Delivered: 53, ShedRate: 6, BaseMs: 12, RttMs: 20, ReportAge: time.Second})
	if v != "degraded" || !strings.Contains(why, "delivered") {
		t.Fatalf("got %s %q; want degraded naming delivered", v, why)
	}

	// Shed rate degraded (6) and rtt degraded (70ms over a 10ms base) at
	// once, with delivery healthy: shed wins because it is checked before
	// rtt.
	v, why = Health(HealthInput{Produced: 60, Delivered: 60, ShedRate: 6, BaseMs: 10, RttMs: 80, ReportAge: time.Second})
	if v != "degraded" || !strings.Contains(why, "shed") {
		t.Fatalf("got %s %q; want degraded naming shed", v, why)
	}
}

// Delivered is read from the client's own decoded-frame report, never from
// bytes the transport wrote -- trap 35's fix one layer up. This does not
// re-test wtConn (that is viewer_test.go's job); it pins that Health's
// Delivered field means "the client decoded these" by construction: nothing
// in Health can distinguish a frame that was sent from one that was
// delivered, which is exactly why the wiring in viewer.go must pass decFps
// and never sess-side sent-byte counters as Delivered.
// The rtt check is skipped entirely while BaseMs is 0 -- a fresh viewer's
// pacer that has not sampled a single round trip yet, not a zero-latency
// path. Without the guard this reads "bad" naming rtt; a viewer that has
// never had a chance to prove its path is bad should not be judged on it.
func TestRttCheckSkippedWhenBaseIsZero(t *testing.T) {
	v, why := Health(HealthInput{Produced: 60, Delivered: 60, BaseMs: 0, RttMs: 900, ReportAge: time.Second})
	if v != "good" {
		t.Fatalf("rtt 900ms over a 0ms base = %s %q; want good (base not yet learnt)", v, why)
	}
}

func TestDeliveredIsJudgedAsReportedNotAsSent(t *testing.T) {
	// A viewer that received nothing usable -- as in trap 35, where the
	// transport "sent" two 46-byte parameter sets a second and no pictures --
	// reports Delivered: 0 regardless of how many bytes went out. Health has
	// no bytes-sent input at all, so there is nothing here for an abandoned
	// frame to inflate.
	v, why := Health(HealthInput{Produced: 45, Delivered: 0, BaseMs: 300, RttMs: 320, ReportAge: time.Second})
	if v != "bad" || !strings.Contains(why, "0 of 45") {
		t.Fatalf("got %s %q; want bad naming \"0 of 45\"", v, why)
	}
}

// Allowance lifts the judged ratio, and only that: the reason still names the
// frames the client decoded. 50 of 60 is 0.83. Forgiving 3 judges it as 53/60,
// still degraded, and the reason must say 50 rather than 53. Forgiving 4
// judges it as 54/60, exactly the 0.9 boundary, which is good.
func TestAllowanceLiftsTheRatioButNotTheReportedCount(t *testing.T) {
	v, why := Health(HealthInput{Produced: 60, Delivered: 50, Allowance: 3, BaseMs: 12, RttMs: 20, ReportAge: time.Second})
	if v != "degraded" || !strings.Contains(why, "delivered 50 of 60") {
		t.Fatalf("got %s %q; want degraded naming \"delivered 50 of 60\"", v, why)
	}
	v, why = Health(HealthInput{Produced: 60, Delivered: 50, Allowance: 4, BaseMs: 12, RttMs: 20, ReportAge: time.Second})
	if v != "good" {
		t.Fatalf("50 delivered with 4 forgiven = %s %q; want good", v, why)
	}
}

func TestDecodeErrorsAreBadWhateverElseSays(t *testing.T) {
	v, why := Health(HealthInput{Produced: 2, Delivered: 0, BaseMs: 20, RttMs: 20, DecodeErrors: 2})
	if v != "bad" || why != "decoder rejected 2 frames" {
		t.Fatalf("got %q %q", v, why)
	}
}
