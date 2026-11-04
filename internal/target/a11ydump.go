package target

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/bmcreations/loupe/internal/a11ydump"
)

// a11yDumpEnabled reports whether Android trees come from loupe's own dump
// (internal/a11ydump) rather than `uiautomator dump`. LOUPE_A11Y_DUMP=0
// turns it off.
func a11yDumpEnabled() bool { return os.Getenv("LOUPE_A11Y_DUMP") != "0" }

const a11yDeviceDir = "/data/local/tmp"

// a11yDumpTimeout bounds one run of the dex. A dump takes ~250ms; past this
// the helper is stuck (a waking device, a starved VM) and uiautomator, which
// costs about this much anyway, is the better answer. A var for tests.
var a11yDumpTimeout = 3 * time.Second

// inspectA11y runs the a11ydump dex: uiautomator's XML in ~250ms instead of
// ~2s, plus each scrollable node's scroll-backward and scroll-forward.
// Called under inspectMu.
func (t *ADBTarget) inspectA11y(ctx context.Context) (*UI, error) {
	remote := a11yDeviceDir + "/" + a11ydump.DeviceName
	if !t.a11yPushed {
		if err := t.pushA11yDex(ctx, remote); err != nil {
			return nil, err
		}
		t.a11yPushed = true
	}
	runCtx, cancel := context.WithTimeout(ctx, a11yDumpTimeout)
	defer cancel()
	raw, err := t.adb.ExecOut(runCtx, t.info.Serial, "CLASSPATH="+remote, "app_process", "/", a11ydump.Class)
	if err != nil && runCtx.Err() != nil && ctx.Err() == nil {
		// Killing adb leaves the helper running on the device, holding the
		// one UiAutomation slot the fallback needs.
		_, _ = t.adb.Shell(ctx, t.info.Serial, "pkill", "-f", a11ydump.Class)
		return nil, fmt.Errorf("a11y dump: no answer in %s", a11yDumpTimeout)
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == a11ydump.ExitBusy {
			return nil, fmt.Errorf("a11y dump: %w", ErrUIAutomationBusy)
		}
		// A wiped emulator loses the dex; push it again next time.
		t.a11yPushed = false
		return nil, fmt.Errorf("a11y dump: %w", err)
	}
	if strings.TrimSpace(string(raw)) == "busy" {
		return nil, fmt.Errorf("a11y dump: %w", ErrUIAutomationBusy)
	}
	if !strings.HasPrefix(string(raw), "<?xml") {
		t.a11yPushed = false
		return nil, fmt.Errorf("a11y dump: unexpected output %q", truncate(string(raw), 200))
	}
	return ParseUI(raw)
}

// pushA11yDex copies the dex to the device unless this version is there.
func (t *ADBTarget) pushA11yDex(ctx context.Context, remote string) error {
	if out, err := t.adb.Shell(ctx, t.info.Serial, "ls", remote); err == nil && strings.TrimSpace(out) == remote {
		return nil
	}
	f, err := os.CreateTemp("", "loupe-a11y-*.dex")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(a11ydump.Dex()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return t.adb.Push(ctx, t.info.Serial, f.Name(), remote)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
