package target

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// A simulator turned counter-clockwise (landscapeLeft, Rotator 1) draws its
// content turned clockwise inside the portrait picture, so the client turns
// the picture back by three clockwise quarters to stand it up.
func TestSimContentRotationTurnsThePictureBackClockwise(t *testing.T) {
	for to, want := range []int{0, 3, 2, 1} {
		if got := simContentRotation(to); got != want {
			t.Errorf("simContentRotation(%d) = %d, want %d", to, got, want)
		}
	}
}

func TestSimulatorContentRotationFollowsRotate(t *testing.T) {
	bin, _ := statefulSimcap(t)
	in := &simInput{bin: bin, udid: "u"}
	defer in.close()
	s := &Simulator{}
	s.inputOnce.Do(func() { s.in = in })
	s.booted.Store(true)
	ctx := context.Background()

	if got := s.contentRotation(ctx); got != 0 {
		t.Fatalf("contentRotation of a fresh simulator = %d, want 0", got)
	}
	if err := s.Rotate(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got := s.contentRotation(ctx); got != 3 {
		t.Fatalf("contentRotation after Rotate(1) = %d, want 3", got)
	}
	if err := s.Rotate(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if got := s.contentRotation(ctx); got != 0 {
		t.Fatalf("contentRotation after Rotate(0) = %d, want 0", got)
	}
}

func TestSimulatorContentRotationIsZeroWhenShutDown(t *testing.T) {
	if got := (&Simulator{}).contentRotation(context.Background()); got != 0 {
		t.Fatalf("contentRotation of a shut-down simulator = %d, want 0", got)
	}
}

// An upright frame marshals exactly as it did before contentRotation
// existed, so an older client reads the same JSON.
func TestDeviceFrameOmitsAnUprightContentRotation(t *testing.T) {
	b, err := json.Marshal(&DeviceFrame{Width: 10, Height: 20})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "contentRotation") {
		t.Errorf("upright frame marshalled %s", b)
	}
	b, _ = json.Marshal(&DeviceFrame{Width: 10, Height: 20, ContentRotation: 3})
	if !strings.Contains(string(b), `"contentRotation":3`) {
		t.Errorf("turned frame marshalled %s", b)
	}
}

// A fresh helper's first `orient` waits for simcap to open the orientation
// capability, which takes longer than a plain command's budget.
func TestSimulatorRotateWaitsForTheOrientationOpen(t *testing.T) {
	old := simOrientOpenBudget
	simOrientOpenBudget = 2 * time.Second
	defer func() { simOrientOpenBudget = old }()

	bin := filepath.Join(t.TempDir(), "simcap")
	script := `#!/bin/sh
opened=
while read -r line; do
  if [ "$line" = route ]; then echo "ok hub"; continue; fi
  case "$line" in
    "orient "*) [ -z "$opened" ] && sleep 0.5 && opened=1; echo "ok landscapeLeft unlocked" ;;
    *) echo ok ;;
  esac
done
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	in := &simInput{bin: bin, udid: "u", budget: 100 * time.Millisecond}
	defer in.close()
	s := &Simulator{}
	s.inputOnce.Do(func() { s.in = in })
	s.booted.Store(true)

	if err := s.Rotate(context.Background(), 1); err != nil {
		t.Fatalf("Rotate with a 0.5s orientation open: %v", err)
	}
}
