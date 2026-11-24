package ios

import (
	"os/exec"
	"testing"
)

func TestAppContainerMissingOnlyOnExitStatusTwo(t *testing.T) {
	run := func(code string) error { return exec.Command("sh", "-c", "exit "+code).Run() }
	if !appContainerMissing(run("2")) {
		t.Error("exit 2 is the not-installed answer")
	}
	for _, code := range []string{"0", "1", "149"} {
		if appContainerMissing(run(code)) {
			t.Errorf("exit %s read as not installed", code)
		}
	}
	if appContainerMissing(exec.Command("/nonexistent/xcrun").Run()) {
		t.Error("a binary that never ran read as not installed")
	}
}
