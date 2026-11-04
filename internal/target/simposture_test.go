package target

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
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

func TestDuoOffersClosedAndOpenedAndFolds(t *testing.T) {
	s := NewSimulator(SimInfo{UDID: "u", Name: "loupe-duo", State: "Booted"}, fakeFoldingSimcap(t, "2"))
	defer s.input().close()
	ctx := t.Context()

	got, err := s.Settings(ctx)
	if err != nil || len(got) != 2 || got[0].ID != "posture" || got[0].Value != "opened" ||
		!slices.Equal(got[0].Options, []string{"closed", "opened"}) || !got[0].Supported {
		t.Fatalf("Settings = %+v, %v", got, err)
	}
	// Before any write the angle is the one the lit panel's posture sends.
	if h := got[1]; h.ID != hingeAngleID || h.Value != 180.0 || *h.Min != 0 || *h.Max != 180 || *h.Step != 1 {
		t.Fatalf("hingeAngle = %+v", h)
	}
	if failed, _, err := s.ApplySettings(ctx, map[string]any{"posture": "closed"}); err != nil || failed != nil {
		t.Fatalf("fold: failed %v, %v", failed, err)
	}
	if got, _ := s.Settings(ctx); got[0].Value != "closed" || got[1].Value != 0.0 {
		t.Fatalf("after folding, posture = %v, hingeAngle = %v", got[0].Value, got[1].Value)
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
		if failed, _, err := s.ApplySettings(ctx, map[string]any{hingeAngleID: deg}); err != nil || failed != nil {
			t.Fatalf("hinge %v: failed %v, %v", deg, failed, err)
		}
	}
	got, _ := s.Settings(ctx)
	if got[0].Value != "opened" || got[1].Value != 120.0 {
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
		"a fraction":     {hingeAngleID: 12.5},
		"too far":        {hingeAngleID: 200.0},
		"with a posture": {hingeAngleID: 90.0, "posture": "closed"},
	} {
		if _, _, err := s.ApplySettings(t.Context(), changes); !errors.Is(err, ErrInvalidSetting) {
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
	if _, _, err := s.ApplySettings(t.Context(), map[string]any{"posture": "closed"}); !errors.Is(err, ErrInvalidSetting) {
		t.Fatalf("fold on one panel = %v, want ErrInvalidSetting", err)
	}
}

func TestDuoRefusesHalfOpenBeforeSendingAnything(t *testing.T) {
	s := NewSimulator(SimInfo{UDID: "u", Name: "loupe-duo", State: "Booted"}, fakeFoldingSimcap(t, "2"))
	defer s.input().close()
	for _, v := range []any{"halfOpened", 180, "darkMode"} {
		if _, _, err := s.ApplySettings(t.Context(), map[string]any{"posture": v}); !errors.Is(err, ErrInvalidSetting) {
			t.Errorf("posture %v = %v, want ErrInvalidSetting", v, err)
		}
	}
	if _, _, err := s.ApplySettings(t.Context(), map[string]any{"darkMode": true}); !errors.Is(err, ErrInvalidSetting) {
		t.Errorf("darkMode = %v, want ErrInvalidSetting", err)
	}
}

func TestShutDownSimulatorHasNoSettings(t *testing.T) {
	s := NewSimulator(SimInfo{UDID: "u", Name: "loupe-duo", State: "Shutdown"}, fakeFoldingSimcap(t, "2"))
	if s.HasSettings(t.Context()) {
		t.Fatal("a shut-down simulator claimed device settings")
	}
}
