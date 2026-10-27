package target

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// fakeFoldingSimcap answers -input like a simulator with `panels` panels,
// whose lit panel follows the last hinge angle it was sent.
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
    "hinge 0") p=closed; echo ok ;;
    "hinge 180") p=opened; echo ok ;;
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
	if err != nil || len(got) != 1 || got[0].ID != "posture" || got[0].Value != "opened" ||
		!slices.Equal(got[0].Options, []string{"closed", "opened"}) || !got[0].Supported {
		t.Fatalf("Settings = %+v, %v", got, err)
	}
	if failed, _, err := s.ApplySettings(ctx, map[string]any{"posture": "closed"}); err != nil || failed != nil {
		t.Fatalf("fold: failed %v, %v", failed, err)
	}
	if got, _ := s.Settings(ctx); got[0].Value != "closed" {
		t.Fatalf("after folding, posture = %v", got[0].Value)
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
