package target

import (
	"testing"
)

// TestContentRotationFor pins the direction against the simulator's: a device
// turned counter-clockwise (1) showing an upright picture needs the same
// three clockwise quarter turns simContentRotation(1) gives.
func TestContentRotationFor(t *testing.T) {
	if got, want := ContentRotationFor(1, 0), simContentRotation(1); got != want {
		t.Fatalf("contentRotationFor(1, 0) = %d; want %d, the simulator's", got, want)
	}
	for held := 0; held < 4; held++ {
		if got := ContentRotationFor(held, held); got != 0 {
			t.Errorf("contentRotationFor(%d, %d) = %d; want 0, the display followed", held, held, got)
		}
	}
	if got := ContentRotationFor(0, 1); got != 1 {
		t.Errorf("contentRotationFor(0, 1) = %d; want 1, a landscape-locked app on an upright phone", got)
	}
}
