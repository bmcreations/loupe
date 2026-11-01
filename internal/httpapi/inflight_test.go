package httpapi

import "testing"

// Entering needKey resets the inter-frame streams already written and leaves
// keyframes and parameter sets alone.
func TestInflightCancelStaleSkipsKeys(t *testing.T) {
	var f inflightFrames
	cancelled := map[string]int{}
	note := func(name string, key bool) { f.note(key, func() { cancelled[name]++ }) }
	note("inter1", false)
	note("key", true)
	note("inter2", false)

	if n := f.CancelStale(); n != 2 {
		t.Fatalf("CancelStale = %d, want 2", n)
	}
	if cancelled["inter1"] != 1 || cancelled["inter2"] != 1 || cancelled["key"] != 0 {
		t.Fatalf("cancelled = %v", cancelled)
	}
	if n := f.CancelStale(); n != 0 {
		t.Fatalf("second CancelStale = %d, want 0", n)
	}
}

func TestInflightIsBounded(t *testing.T) {
	var f inflightFrames
	for i := 0; i < inflightLimit*2; i++ {
		f.note(false, func() {})
	}
	if n := f.CancelStale(); n != inflightLimit {
		t.Fatalf("CancelStale = %d, want %d", n, inflightLimit)
	}
}
