package ios

import (
	"testing"
)

func TestSimctlLaunchPid(t *testing.T) {
	for out, want := range map[string]int{
		"dev.bmcreations.loupe.camtest: 34371\n": 34371,
		"com.apple.Preferences: 812":             812,
		"An error was encountered":               0,
		"":                                       0,
	} {
		if got := simctlLaunchPid(out); got != want {
			t.Errorf("%q: %d; want %d", out, got, want)
		}
	}
}
