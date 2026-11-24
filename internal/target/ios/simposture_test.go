package ios

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// fakeFoldingSimcap answers -input like a simulator with `panels` panels,
// whose lit panel follows the hinge the way the Duo's did at the ends: 0
// folds it, 100 or more opens it, and anything between leaves it lit as it
// was. Every hinge line it is sent is appended to $dir/hinges.
func fakeFoldingSimcap(t *testing.T, panels string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "simcap")
	script := `#!/bin/sh
p=opened
while read -r line; do
  case "$line" in
    route) echo "ok hub" ;;
    panels) echo "ok ` + panels + `" ;;
    posture) if [ ` + panels + ` -ge 2 ]; then echo "ok $p"; else echo "ok none"; fi ;;
    hinge\ *) n=${line#hinge }; echo "$n" >> "$(dirname "$0")/hinges"
      if [ "$n" -eq 0 ]; then p=closed; elif [ "$n" -ge 100 ]; then p=opened; fi; echo ok ;;
    *) echo "err unknown" ;;
  esac
done
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestDuoOffersThreePosturesAndFolds(t *testing.T) {
	s := NewSimulator(SimInfo{UDID: "u", Name: "loupe-duo", State: "Booted"}, fakeFoldingSimcap(t, "2"))
	defer s.input().close()
	ctx := t.Context()

	got, err := s.Settings(ctx)
	if err != nil || len(got) != 2 || got[0].ID != "posture" || got[0].Value != "opened" ||
		!slices.Equal(got[0].Options, []string{"closed", "halfOpened", "opened"}) || !got[0].Supported {
		t.Fatalf("Settings = %+v, %v", got, err)
	}
	// Before any write the angle is the one the lit panel's posture sends.
	if h := got[1]; h.ID != target.HingeAngleID || h.Value != 180.0 || *h.Min != 0 || *h.Max != 180 || *h.Step != 1 {
		t.Fatalf("hingeAngle = %+v", h)
	}
	if failed, _, err := s.ApplySettings(ctx, map[string]any{"posture": "closed"}); err != nil || failed != nil {
		t.Fatalf("fold: failed %v, %v", failed, err)
	}
	if got, _ := s.Settings(ctx); got[0].Value != "closed" || got[1].Value != 0.0 {
		t.Fatalf("after folding, posture = %v, hingeAngle = %v", got[0].Value, got[1].Value)
	}
	if failed, _, err := s.ApplySettings(ctx, map[string]any{"posture": "halfOpened"}); err != nil || failed != nil {
		t.Fatalf("half open: failed %v, %v", failed, err)
	}
	if got, _ := s.Settings(ctx); got[0].Value != "halfOpened" || got[1].Value != 120.0 {
		t.Fatalf("after half opening, posture = %v, hingeAngle = %v", got[0].Value, got[1].Value)
	}
	if failed, _, err := s.ApplySettings(ctx, map[string]any{"posture": "opened"}); err != nil || failed != nil {
		t.Fatalf("open: failed %v, %v", failed, err)
	}
	if got, _ := s.Settings(ctx); got[0].Value != "opened" || got[1].Value != 180.0 {
		t.Fatalf("after opening, posture = %v, hingeAngle = %v", got[0].Value, got[1].Value)
	}
}

// A hinge write sends the angle and returns without waiting on the panel;
// the angle reads back as sent, and posture follows where the panel does.
func TestDuoHingeAngleSendsTheAngleAndReadsItBack(t *testing.T) {
	bin := fakeFoldingSimcap(t, "2")
	s := NewSimulator(SimInfo{UDID: "u", Name: "loupe-duo", State: "Booted"}, bin)
	defer s.input().close()
	ctx := t.Context()

	for _, deg := range []float64{0, 45, 120} {
		if failed, _, err := s.ApplySettings(ctx, map[string]any{target.HingeAngleID: deg}); err != nil || failed != nil {
			t.Fatalf("hinge %v: failed %v, %v", deg, failed, err)
		}
	}
	got, _ := s.Settings(ctx)
	if got[0].Value != "halfOpened" || got[1].Value != 120.0 {
		t.Fatalf("posture %v, hingeAngle %v", got[0].Value, got[1].Value)
	}
	sent, err := os.ReadFile(filepath.Join(filepath.Dir(bin), "hinges"))
	if err != nil || string(sent) != "0\n45\n120\n" {
		t.Fatalf("simcap was sent %q, %v", sent, err)
	}
}

func TestDuoRefusesABadHingeAngleBeforeSendingAnything(t *testing.T) {
	bin := fakeFoldingSimcap(t, "2")
	s := NewSimulator(SimInfo{UDID: "u", Name: "loupe-duo", State: "Booted"}, bin)
	defer s.input().close()
	for name, changes := range map[string]map[string]any{
		"a fraction":     {target.HingeAngleID: 12.5},
		"too far":        {target.HingeAngleID: 200.0},
		"with a posture": {target.HingeAngleID: 90.0, "posture": "closed"},
	} {
		if _, _, err := s.ApplySettings(t.Context(), changes); !errors.Is(err, target.ErrInvalidSetting) {
			t.Errorf("%s = %v, want ErrInvalidSetting", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(bin), "hinges")); err == nil {
		t.Fatal("simcap was sent a hinge")
	}
}

func TestHingeReadback(t *testing.T) {
	now := time.Now()
	old := now.Add(-2 * simPostureSettle)
	cases := []struct {
		name    string
		posture string
		deg     int
		at      time.Time
		sent    bool
		want    float64
		ok      bool
	}{
		{"nothing sent, opened", "opened", 0, time.Time{}, false, 180, true},
		{"nothing sent, closed", "closed", 0, time.Time{}, false, 0, true},
		{"nothing sent, no panel yet", "", 0, time.Time{}, false, 0, false},
		{"sent, panel agrees", "opened", 120, old, true, 120, true},
		// The Duo's panel lags and has hysteresis: closed at 90 is normal.
		{"sent, ambiguous band", "closed", 90, old, true, 90, true},
		{"sent, panel still moving", "closed", 180, now, true, 180, true},
		{"folded elsewhere", "closed", 180, old, true, 0, true},
		{"opened elsewhere", "opened", 0, old, true, 180, true},
		// The ends are inclusive: 135 closed and 5 opened contradict, one
		// degree inside does not.
		{"closed at 134", "closed", 134, old, true, 134, true},
		{"closed at 135", "closed", 135, old, true, 0, true},
		{"opened at 6", "opened", 6, old, true, 6, true},
		{"opened at 5", "opened", 5, old, true, 180, true},
		// The settle window is open-ended: at exactly simPostureSettle the
		// panel is trusted.
		{"folded elsewhere, at the settle", "closed", 180, now.Add(-simPostureSettle), true, 0, true},
	}
	for _, c := range cases {
		got, ok := hingeReadback(c.posture, c.deg, c.at, c.sent, now)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: got %v, %v; want %v, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestOnePanelSimulatorHasNoSettings(t *testing.T) {
	s := NewSimulator(SimInfo{UDID: "u", Name: "loupe-probe", State: "Booted"}, fakeFoldingSimcap(t, "1"))
	defer s.input().close()
	if s.HasSettings(t.Context()) {
		t.Fatal("an iPhone 17 claimed device settings")
	}
	if _, _, err := s.ApplySettings(t.Context(), map[string]any{"posture": "closed"}); !errors.Is(err, target.ErrInvalidSetting) {
		t.Fatalf("fold on one panel = %v, want ErrInvalidSetting", err)
	}
}

func TestDuoRefusesABadPostureBeforeSendingAnything(t *testing.T) {
	s := NewSimulator(SimInfo{UDID: "u", Name: "loupe-duo", State: "Booted"}, fakeFoldingSimcap(t, "2"))
	defer s.input().close()
	for _, v := range []any{"halfOpen", 180, "darkMode"} {
		if _, _, err := s.ApplySettings(t.Context(), map[string]any{"posture": v}); !errors.Is(err, target.ErrInvalidSetting) {
			t.Errorf("posture %v = %v, want ErrInvalidSetting", v, err)
		}
	}
	if _, _, err := s.ApplySettings(t.Context(), map[string]any{"darkMode": true}); !errors.Is(err, target.ErrInvalidSetting) {
		t.Errorf("darkMode = %v, want ErrInvalidSetting", err)
	}
}

func TestShutDownSimulatorHasNoSettings(t *testing.T) {
	s := NewSimulator(SimInfo{UDID: "u", Name: "loupe-duo", State: "Shutdown"}, fakeFoldingSimcap(t, "2"))
	if s.HasSettings(t.Context()) {
		t.Fatal("a shut-down simulator claimed device settings")
	}
}

// A Duo posture write and a slider angle that arrives during it go out in
// order: the posture's hinge and settle wait first, then the angle. The
// fake's panel lags a fold by one read, so the posture polls at least
// twice, and an angle sent outside the coalescer would reach simcap
// between the polls and open the panel under the posture's settle wait.
func TestDuoHingeAngleWaitsForAPostureInFlight(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "simcap")
	script := `#!/bin/sh
p=opened
while read -r line; do
  case "$line" in
    route) echo "ok hub" ;;
    panels) echo "ok 2" ;;
    posture) if [ "$lag" = 1 ]; then lag=0; echo "ok opened"; else echo "ok $p"; fi ;;
    hinge\ *) n=${line#hinge }; echo "$n" >> "$(dirname "$0")/hinges"
      if [ "$n" -eq 0 ]; then p=closed; lag=1; elif [ "$n" -ge 100 ]; then p=opened; fi; echo ok ;;
    *) echo "err unknown" ;;
  esac
done
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s := NewSimulator(SimInfo{UDID: "u", Name: "loupe-duo", State: "Booted"}, bin)
	defer s.input().close()
	ctx := t.Context()
	if _, err := s.Settings(ctx); err != nil {
		t.Fatal(err)
	}

	posture := make(chan error, 1)
	go func() {
		_, _, err := s.ApplySettings(ctx, map[string]any{"posture": "closed"})
		posture <- err
	}()
	hinges := filepath.Join(dir, "hinges")
	deadline := time.Now().Add(3 * time.Second)
	for b, _ := os.ReadFile(hinges); string(b) != "0\n"; b, _ = os.ReadFile(hinges) {
		if time.Now().After(deadline) {
			t.Fatalf("the posture's hinge never went out: %q", b)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if failed, _, err := s.ApplySettings(ctx, map[string]any{target.HingeAngleID: 120.0}); err != nil || failed != nil {
		t.Fatalf("hingeAngle: failed %v, %v", failed, err)
	}
	if err := <-posture; err != nil {
		t.Fatalf("posture: %v", err)
	}
	if b, _ := os.ReadFile(hinges); string(b) != "0\n120\n" {
		t.Fatalf("simcap was sent %q", b)
	}
	if got, _ := s.Settings(ctx); got[0].Value != "halfOpened" || got[1].Value != 120.0 {
		t.Fatalf("posture %v, hingeAngle %v", got[0].Value, got[1].Value)
	}
}

// A posture PUT replaced in the queue by a newer angle is never sent, so it
// waits for that angle's write and answers with its error, not a 200.
func TestDuoReplacedPostureAnswersWithTheWriteThatRan(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "simcap")
	// hinge 0 holds the queue for half a second; hinge 99 fails.
	script := `#!/bin/sh
p=opened
while read -r line; do
  case "$line" in
    route) echo "ok hub" ;;
    panels) echo "ok 2" ;;
    posture) echo "ok $p" ;;
    hinge\ *) n=${line#hinge }; echo "$n" >> "$(dirname "$0")/hinges"
      if [ "$n" -eq 0 ]; then sleep 0.5; p=closed; echo ok
      elif [ "$n" -eq 99 ]; then echo "err stuck"
      else p=opened; echo ok; fi ;;
    *) echo "err unknown" ;;
  esac
done
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s := NewSimulator(SimInfo{UDID: "u", Name: "loupe-duo", State: "Booted"}, bin)
	defer s.input().close()
	ctx := t.Context()
	if _, err := s.Settings(ctx); err != nil {
		t.Fatal(err)
	}
	type result struct {
		failed []string
		err    error
	}
	put := func(changes map[string]any) chan result {
		ch := make(chan result, 1)
		go func() { f, _, err := s.ApplySettings(ctx, changes); ch <- result{f, err} }()
		return ch
	}

	closed := put(map[string]any{"posture": "closed"})
	hinges := filepath.Join(dir, "hinges")
	deadline := time.Now().Add(3 * time.Second)
	for b, _ := os.ReadFile(hinges); string(b) != "0\n"; b, _ = os.ReadFile(hinges) {
		if time.Now().After(deadline) {
			t.Fatalf("the first posture's hinge never went out: %q", b)
		}
		time.Sleep(5 * time.Millisecond)
	}
	opened := put(map[string]any{"posture": "opened"})
	waitHingeQueued(t, &s.hinge, simHingeDegrees["opened"])
	angle := put(map[string]any{target.HingeAngleID: 99.0})
	waitHingeQueued(t, &s.hinge, 99)

	if r := <-closed; r.err != nil {
		t.Fatalf("closed: %v", r.err)
	}
	if r := <-angle; r.err == nil || !slices.Equal(r.failed, []string{target.HingeAngleID}) {
		t.Fatalf("hingeAngle = %+v, want it failed", r)
	}
	if r := <-opened; r.err == nil || !strings.Contains(r.err.Error(), "stuck") || !slices.Equal(r.failed, []string{"posture"}) {
		t.Fatalf("replaced posture = %+v, want failed [posture] with the angle's error", r)
	}
	if b, _ := os.ReadFile(hinges); string(b) != "0\n99\n" {
		t.Fatalf("simcap was sent %q, want the replaced posture's 180 left out", b)
	}
}

// A reboot forgets the last angle sent, and so does a panel that says the
// hinge was moved elsewhere: kept, the stale angle would come back once the
// panel stopped contradicting it.
func TestDuoForgetsAStaleHingeAngle(t *testing.T) {
	t.Run("on shutdown", func(t *testing.T) {
		s := NewSimulator(SimInfo{UDID: "u", Name: "loupe-duo", State: "Booted"}, "")
		s.hingeSent.record(120)
		s.Update(SimInfo{UDID: "u", Name: "loupe-duo", State: "Shutdown"})
		if _, _, ok := s.hingeSent.last(); ok {
			t.Fatal("the angle from before the shutdown is still kept")
		}
	})
	t.Run("on a contradiction", func(t *testing.T) {
		s := NewSimulator(SimInfo{UDID: "u", Name: "loupe-duo", State: "Booted"}, fakeFoldingSimcap(t, "2"))
		defer s.input().close()
		s.hingeSent.mu.Lock()
		s.hingeSent.set, s.hingeSent.deg, s.hingeSent.at = true, 3, time.Now().Add(-2*simPostureSettle)
		s.hingeSent.mu.Unlock()
		got, err := s.Settings(t.Context())
		if err != nil || got[1].Value != 180.0 {
			t.Fatalf("Settings = %+v, %v; want the opened panel's 180", got, err)
		}
		if _, _, ok := s.hingeSent.last(); ok {
			t.Fatal("the contradicted angle is still kept")
		}
	})
}

// The lit panel says closed or open; an open one is halfOpened while the
// hinge angle is short of flat.
func TestSimReportedPosture(t *testing.T) {
	for _, c := range []struct {
		lit   string
		deg   float64
		known bool
		want  string
	}{
		{"opened", 180, true, "opened"},
		{"opened", 160, true, "opened"},
		{"opened", 159, true, "halfOpened"},
		{"opened", 120, true, "halfOpened"},
		{"opened", 30, true, "halfOpened"},
		{"opened", 120, false, "opened"},
		{"closed", 60, true, "closed"},
		{"", 120, true, ""},
	} {
		if got := simReportedPosture(c.lit, c.deg, c.known); got != c.want {
			t.Errorf("simReportedPosture(%q, %v, %v) = %q, want %q", c.lit, c.deg, c.known, got, c.want)
		}
	}
}

// Taking the hinge off flat lit the Duo's cover for about a second before
// the inner panel came back, even for a sweep to 120. While a write of 90 or
// more settles, a cover reading is that blip, not a fold; once settled, or
// for a smaller angle, the panel stands.
func TestSimSettlingPanel(t *testing.T) {
	at := time.Unix(1000, 0)
	for _, c := range []struct {
		lit   string
		deg   int
		sent  bool
		after time.Duration
		want  string
	}{
		{"closed", 120, true, time.Second, "opened"},
		{"closed", 90, true, time.Second, "opened"},
		{"closed", 180, true, time.Second, "opened"},
		{"closed", 120, true, simPostureSettle, "closed"},
		{"closed", 60, true, time.Second, "closed"},
		{"closed", 120, false, time.Second, "closed"},
		{"opened", 0, true, time.Second, "opened"},
		{"", 120, true, time.Second, ""},
	} {
		if got := simSettlingPanel(c.lit, c.deg, at, c.sent, at.Add(c.after)); got != c.want {
			t.Errorf("simSettlingPanel(%q, %d, sent %v, +%v) = %q, want %q", c.lit, c.deg, c.sent, c.after, got, c.want)
		}
	}
}

func waitHingeQueued(t *testing.T, c *target.HingeCoalescer, deg int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if q, ok := c.Queued(); ok && q == deg {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%d never queued", deg)
}
