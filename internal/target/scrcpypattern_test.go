package target

import (
	"regexp"
	"testing"
)

// TestScrcpyPatternSkipsItsOwnShell pins the pgrep pattern: adb shell runs
// pgrep inside `sh -c "pgrep -f '<pattern>'"`, so a pattern that matches its
// own text finds that sh every time and every save reported a lingering
// server. The pattern must match loupe's server and nothing else.
func TestScrcpyPatternSkipsItsOwnShell(t *testing.T) {
	pat := scrcpyPattern("3.3.4")
	re := regexp.MustCompile(pat)
	if !re.MatchString("app_process / com.genymobile.scrcpy.Server 3.3.4 tunnel_forward=true") {
		t.Errorf("%q misses loupe's server", pat)
	}
	if re.MatchString("sh -c pgrep -f " + shellQuote(pat)) {
		t.Errorf("%q matches the shell running pgrep", pat)
	}
	if re.MatchString("app_process / com.genymobile.scrcpy.Server 1.19-ws8 web ERROR 8886 true") {
		t.Errorf("%q matches ws-scrcpy's server", pat)
	}
}
