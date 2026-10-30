package target

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// A drop pastes with Cmd+V when it can, so no edit menu comes up. Cmd+V
// from an app with no pasteboard grant makes pasted ask "Allow Paste"
// through a notification SpringBoard refuses on a simulator, and nothing is
// pasted. pasted then records the app as limited (auth 3). With
// kTCCServicePasteboard granted (auth 2) in the simulator's TCC.db, the same
// Cmd+V pastes at once and iOS shows only its "<app> pasted from
// dtpasteboardd" banner. Measured in Flipcash on an iPhone 17 simulator,
// iOS 27.1: the composer gained the image chip, and an update to the row
// took effect on the next Cmd+V without restarting tccd. simctl privacy has
// no pasteboard service, so the row is written directly.

const (
	// hidLeftGUI and hidV are the HID usages for Cmd and V.
	hidLeftGUI = 227
	hidV       = 25
	// keyPasteGap is between the key events, so the app sees Cmd held.
	keyPasteGap = 50 * time.Millisecond
)

// tccPasteGrantSQL is the statement that lets bundle paste without asking.
// It replaces the limited row pasted writes after a refused prompt.
// bundleIDPattern keeps the ID safe to put in SQL.
func tccPasteGrantSQL(bundle string) (string, error) {
	if !bundleIDPattern.MatchString(bundle) {
		return "", fmt.Errorf("bundle ID %q is not one TCC can name", bundle)
	}
	return "insert or replace into access " +
		"(service, client, client_type, auth_value, auth_reason, auth_version, flags) " +
		"values ('kTCCServicePasteboard', '" + bundle + "', 0, 2, 2, 1, 0);", nil
}

// grantPaste writes the pasteboard grant for bundle into the simulator's
// TCC.db.
func (s *Simulator) grantPaste(ctx context.Context, bundle string) error {
	stmt, err := tccPasteGrantSQL(bundle)
	if err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, "xcrun", "simctl", "getenv", s.UDID(), "HOME").Output()
	if err != nil {
		return fmt.Errorf("simulator home: %w", err)
	}
	db := filepath.Join(strings.TrimSpace(string(out)), "Library", "TCC", "TCC.db")
	if out, err := exec.CommandContext(ctx, "/usr/bin/sqlite3", db, stmt).CombinedOutput(); err != nil {
		return fmt.Errorf("grant paste to %s: %v: %s", bundle, err, strings.TrimSpace(string(out)))
	}
	return nil
}

var (
	// errNoFrontApp is a key paste with no app in front to grant.
	errNoFrontApp = errors.New("no app in front")
	// errNotFocusedHere is a key paste where the tap focused no field
	// holding the drop point.
	errNotFocusedHere = errors.New("no focused field at the drop point")
)

// keyPaste grants the app in front the pasteboard and sends Cmd+V to the
// field at x, y, which the caller has focused. It reads the field before
// and after, as tapPaste does; ui is the read before when the caller has
// one from after the focus, else nil. Only pasteTook is a paste: a field that gives
// nothing to compare is left to the edit menu, which offers no Paste where
// no image can go.
func (s *Simulator) keyPaste(ctx context.Context, ui *UI, x, y int) (pasteVerdict, error) {
	bundle := s.frontApp(ctx)
	if bundle == "" {
		return pasteUnknown, errNoFrontApp
	}
	if err := s.grantPaste(ctx, bundle); err != nil {
		return pasteUnknown, err
	}
	if ui == nil {
		var err error
		if ui, err = s.Inspect(ctx); err != nil {
			return pasteUnknown, err
		}
	}
	// Cmd+V goes to whatever has focus, wherever the drop was. A drop
	// beside a focused field must not paste into it.
	if !focusedAt(ui, x, y) {
		return pasteUnknown, errNotFocusedHere
	}
	before := readField(ui, x, y)
	kctx, err := s.checkHub(ctx)
	if err != nil {
		return pasteUnknown, err
	}
	// Cmd is let go even when V failed, so it is not left held.
	if _, err := s.input().do(kctx, 0, "keydown %d", hidLeftGUI); err != nil {
		return pasteUnknown, err
	}
	_ = sleepCtx(kctx, keyPasteGap)
	_, err = s.input().do(kctx, 0, "key %d", hidV)
	_ = sleepCtx(kctx, keyPasteGap)
	if _, uerr := s.input().do(kctx, 0, "keyup %d", hidLeftGUI); err == nil {
		err = uerr
	}
	if err != nil {
		return pasteUnknown, err
	}
	return s.awaitPasted(ctx, before, x, y)
}
