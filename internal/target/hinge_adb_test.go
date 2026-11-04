package target

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// foldADB is an adb stand-in for an emulator: print-states answers states,
// and the console answers reply to `sensor set` ("" is OK). Every argv is
// recorded, so a test can see what reached the console.
func foldADB(t *testing.T, states, reply string) (*ADBTarget, func() []string) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("states", states)
	if reply == "" {
		reply = "OK\n"
	}
	write("reply", reply)
	path, argv := fakeADB(t, `d='`+dir+`'
case "$*" in
  *print-states) cat "$d/states" ;;
  *print-state) echo 1 ;;
  *"emu sensor set"*) cat "$d/reply" ;;
  *"emu sensor get"*) printf 'hinge-angle0 = 90\nOK\n' ;;
esac
`)
	tgt := NewADBTarget(ADB{Path: path}, DeviceInfo{Serial: "emulator-5554", State: "device", Emulator: true}, "", "")
	return tgt, func() []string {
		if _, err := os.Stat(argv); err != nil {
			return nil
		}
		return readArgv(t, argv)
	}
}

func emuLines(argv []string) []string {
	var out []string
	for _, l := range argv {
		if strings.Contains(l, " emu ") {
			out = append(out, l)
		}
	}
	return out
}

// A flat AVD has no hingeAngle row, so a write to it is refused before the
// console hears of it, on the fast path and in a mixed PUT alike.
func TestHingeAngleRefusedOnAFlatEmulator(t *testing.T) {
	for name, changes := range map[string]map[string]any{
		"alone":         {hingeAngleID: 90.0},
		"with darkMode": {hingeAngleID: 90.0, "darkMode": true},
	} {
		t.Run(name, func(t *testing.T) {
			tgt, argv := foldADB(t, flatStates, "")
			_, _, err := tgt.ApplySettings(context.Background(), changes)
			if !errors.Is(err, ErrInvalidSetting) {
				t.Fatalf("err = %v, want ErrInvalidSetting", err)
			}
			if got := emuLines(argv()); len(got) != 0 {
				t.Fatalf("console was sent %v", got)
			}
		})
	}
}

// A foldable answer is kept, so a slider pays the print-states read once.
func TestHingeAngleFoldCheckIsCached(t *testing.T) {
	tgt, argv := foldADB(t, foldStates, "")
	for _, deg := range []float64{90, 91} {
		if _, _, err := tgt.ApplySettings(context.Background(), map[string]any{hingeAngleID: deg}); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for _, l := range argv() {
		if strings.HasSuffix(l, "print-states") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("print-states read %d times, want 1", n)
	}
}

// The fast path a slider hits: one console write per angle, a bad value
// refused with no console traffic, and a console refusal reported as the
// setting that failed.
func TestApplyHingeAngleFastPath(t *testing.T) {
	t.Run("sends the angle", func(t *testing.T) {
		tgt, argv := foldADB(t, foldStates, "")
		failed, notRestored, err := tgt.ApplySettings(context.Background(), map[string]any{hingeAngleID: 90.0})
		if err != nil || failed != nil || notRestored != nil {
			t.Fatalf("failed %v, notRestored %v, err %v", failed, notRestored, err)
		}
		got := emuLines(argv())
		if len(got) != 1 || !strings.HasSuffix(got[0], " emu sensor set hinge-angle0 90") {
			t.Fatalf("console was sent %v", got)
		}
	})
	t.Run("a fraction is 400", func(t *testing.T) {
		tgt, argv := foldADB(t, foldStates, "")
		_, _, err := tgt.ApplySettings(context.Background(), map[string]any{hingeAngleID: 90.5})
		if !errors.Is(err, ErrInvalidSetting) {
			t.Fatalf("err = %v, want ErrInvalidSetting", err)
		}
		if a := argv(); len(a) != 0 {
			t.Fatalf("adb was run: %v", a)
		}
	})
	t.Run("a KO reply fails hingeAngle", func(t *testing.T) {
		tgt, _ := foldADB(t, foldStates, "KO: something\n")
		failed, _, err := tgt.ApplySettings(context.Background(), map[string]any{hingeAngleID: 90.0})
		if err == nil || errors.Is(err, ErrInvalidSetting) || len(failed) != 1 || failed[0] != hingeAngleID {
			t.Fatalf("failed %v, err %v", failed, err)
		}
	})
}
