package target

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var _ Rotator = (*Simulator)(nil)

func TestSimOrientationNameMapsQuarterTurns(t *testing.T) {
	want := []string{"portrait", "landscapeLeft", "portraitUpsideDown", "landscapeRight"}
	for to, name := range want {
		got, err := simOrientationName(to)
		if err != nil || got != name {
			t.Errorf("simOrientationName(%d) = %q, %v; want %q", to, got, err, name)
		}
		if back := simRotationFromName(name); back != to {
			t.Errorf("simRotationFromName(%q) = %d, want %d", name, back, to)
		}
	}
	for _, bad := range []int{-1, 4} {
		if _, err := simOrientationName(bad); err == nil {
			t.Errorf("simOrientationName(%d) accepted an out-of-range turn", bad)
		}
	}
}

func TestSimRotationFromNameTreatsFlatAndUnknownAsPortrait(t *testing.T) {
	for _, n := range []string{"unknown", "faceUp", "faceDown", ""} {
		if got := simRotationFromName(n); got != 0 {
			t.Errorf("simRotationFromName(%q) = %d, want 0", n, got)
		}
	}
}

// statefulSimcap answers `orient set NAME` and `orient get` from a file, so
// Rotate followed by Rotation reads back what was set.
func statefulSimcap(t *testing.T) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "commands")
	state := filepath.Join(dir, "state")
	bin = filepath.Join(dir, "simcap")
	script := `#!/bin/sh
echo unknown > "` + state + `"
while read -r line; do
  if [ "$line" = route ]; then echo "ok hub"; continue; fi
  echo "$line" >> "` + logPath + `"
  case "$line" in
    "orient set "*) echo "${line#orient set }" > "` + state + `"; echo "ok $(cat "` + state + `") unlocked" ;;
    "orient get") echo "ok $(cat "` + state + `") unlocked" ;;
    *) echo ok ;;
  esac
done
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logPath
}

func TestSimulatorRotateSetsTheOrientationAndRotationReadsItBack(t *testing.T) {
	bin, logPath := statefulSimcap(t)
	in := &simInput{bin: bin, udid: "u"}
	defer in.close()
	s := &Simulator{}
	s.inputOnce.Do(func() { s.in = in })
	s.booted.Store(true)
	ctx := context.Background()

	if got, err := s.Rotation(ctx); err != nil || got != 0 {
		t.Fatalf("Rotation before = %d, %v; want 0", got, err)
	}
	if err := s.Rotate(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Rotation(ctx); err != nil || got != 1 {
		t.Fatalf("Rotation after Rotate(1) = %d, %v; want 1", got, err)
	}
	if err := s.Rotate(ctx, 4); err == nil {
		t.Fatal("Rotate(4) was accepted")
	}
	got := strings.Join(commands(t, logPath), " ")
	if want := "orient_get orient_set_landscapeLeft orient_get"; got != want {
		t.Errorf("sent %q, want %q", got, want)
	}
}

func TestSimulatorRotateNeedsABootedSimulator(t *testing.T) {
	if err := (&Simulator{}).Rotate(context.Background(), 1); err == nil {
		t.Fatal("Rotate on a shut-down simulator succeeded")
	}
}
