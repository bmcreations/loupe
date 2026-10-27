package target

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestShellQuoteNeutralisesDeviceShellMetacharacters guards the one place an
// API payload reaches a shell. `adb shell` joins its arguments and runs them
// through sh ON THE DEVICE, so passing args as a slice on the host proves
// nothing. Each case is run through a real sh to check that the quoted form
// evaluates back to the original string and nothing else executes.
func TestShellQuoteNeutralisesDeviceShellMetacharacters(t *testing.T) {
	cases := []string{
		"hello world",
		"it's fine",
		"a; touch /tmp/loupe-pwned",
		"$(echo pwned)",
		"`echo pwned`",
		"a && echo pwned",
		"a | echo pwned",
		"quote\" and 'single'",
		"newline\nsecond",
		"*",
	}
	for _, in := range cases {
		out, err := exec.Command("sh", "-c", "printf %s "+shellQuote(in)).Output()
		if err != nil {
			t.Errorf("%q: sh rejected the quoted form: %v", in, err)
			continue
		}
		if string(out) != in {
			t.Errorf("%q round-tripped as %q", in, string(out))
		}
	}
}

func TestShellQuoteWrapsEvenPlainText(t *testing.T) {
	// A bare word would also survive sh, but leaving it unquoted means the
	// quoting rule depends on the content, and the next caller gets it wrong.
	if got := shellQuote("plain"); !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
		t.Errorf("shellQuote(plain) = %s, want it quoted unconditionally", got)
	}
}

// TestOrientedSizeSwapsAtLandscapeRotations pins the axis swap: a natural,
// unrotated size is relative to however the device is naturally held --
// portrait for a phone, but landscape for a naturally-landscape device like
// pixel_tablet (CLAUDE.md trap 44) -- and rotation 1 or 3 (Android's
// Surface.ROTATION_90/270) means the currently-visible screen is that size
// turned on its side.
func TestOrientedSizeSwapsAtLandscapeRotations(t *testing.T) {
	cases := []struct {
		rotation int
		wantW    int
		wantH    int
	}{
		{0, 1080, 2400},
		{1, 2400, 1080},
		{2, 1080, 2400},
		{3, 2400, 1080},
	}
	for _, c := range cases {
		w, h, err := orientedSize(1080, 2400, c.rotation, nil)
		if err != nil {
			t.Errorf("orientedSize(1080, 2400, %d, nil) returned an error: %v", c.rotation, err)
			continue
		}
		if w != c.wantW || h != c.wantH {
			t.Errorf("orientedSize(1080, 2400, %d, nil) = %d, %d; want %d, %d",
				c.rotation, w, h, c.wantW, c.wantH)
		}
	}
}

// TestOrientedSizeIgnoresAFailedRotationRead pins opus review item 1: a
// failed rotation read must fail ScreenSize, not fall back to the unswapped
// size, because an unswapped landscape tap silently publishes a wrong
// fraction. Renamed from the old "ignores" name, which described the
// fallback this fixes.
func TestOrientedSizeIgnoresAFailedRotationRead(t *testing.T) {
	_, _, err := orientedSize(1080, 2400, 1, errors.New("no rotation reported"))
	if err == nil {
		t.Fatal("orientedSize with a failed rotation read and a non-square size: want an error, got nil")
	}
}

// TestOrientedSizeAcceptsAFailedRotationReadOnASquareScreen covers the one
// case where a failed rotation read is harmless: a square screen reads the
// same either way it is held, so there is nothing for the swap to get wrong.
func TestOrientedSizeAcceptsAFailedRotationReadOnASquareScreen(t *testing.T) {
	w, h, err := orientedSize(1080, 1080, 1, errors.New("no rotation reported"))
	if err != nil {
		t.Fatalf("orientedSize(1080, 1080, ...) with a failed rotation read: %v, want nil (square is unambiguous)", err)
	}
	if w != 1080 || h != 1080 {
		t.Fatalf("orientedSize(1080, 1080, ...) = %d, %d; want 1080, 1080", w, h)
	}
}

// writeFakeADB writes an executable shell script standing in for the adb
// binary. It answers `shell dumpsys window displays` with $FAKE_ADB_DUMP, or
// fails the call when $FAKE_ADB_FAIL is set -- both read from the test's own
// environment via t.Setenv, since exec.Command inherits the parent process
// environment by default.
func writeFakeADB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-adb")
	script := "#!/bin/sh\n" +
		"if [ -n \"$FAKE_ADB_FAIL\" ]; then\n" +
		"  echo \"$FAKE_ADB_FAIL\" >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"printf '%s\\n' \"$FAKE_ADB_DUMP\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake adb: %v", err)
	}
	return path
}

// fakeDumpsysWindowDisplays builds a dumpsys window displays dump in trap
// 44's shape: the decoy 1dpi display (mDisplayId=1084) prints first, with
// its own init= that a parser reading the first match would misread, and the
// real display 0 -- the one ScreenGeometry has to isolate -- comes second.
func fakeDumpsysWindowDisplays(naturalWH, curWH, rotation string) string {
	return fmt.Sprintf(`  Display: mDisplayId=1084
    init=1080x480 1dpi mMinSizeOfResizeableTaskDp=220 cur=1080x480 app=1080x480 rng=480x480-1080x1080
  overrideConfig={0.0 ?mcc0mnc ?localeList ?layoutDir sw76800dp w172800dp h76800dp 1dpi xlrg long compactNeeded land ?uimode ?night finger qwerty/v/v dpad/v winConfig={ mBounds=Rect(0, 0 - 1080, 480) mDisplayRotation=ROTATION_0 mRotation=ROTATION_0} ?fontWeightAdjustment}
    mLandscapeRotation=ROTATION_90 mSeascapeRotation=ROTATION_270
    mPortraitRotation=ROTATION_0 mUpsideDownRotation=ROTATION_180
  Display: mDisplayId=0 (organized)
    init=%s 420dpi mMinSizeOfResizeableTaskDp=220 cur=%s
  overrideConfig={1.0 310mcc260mnc [en_US] ldltr sw411dp w923dp h411dp 420dpi nrml long compactNeeded land finger qwerty/v/v dpad/v winConfig={ mBounds=Rect(0, 0 - 2424, 1080) mDisplayRotation=%s mWindowingMode=fullscreen mActivityType=undefined mAlwaysOnTop=undefined mRotation=%s} s.1075 fontWeightAdjustment=0}
    mSupportAutoRotation=true`, naturalWH, curWH, rotation, rotation)
}

// TestADBTargetScreenSize covers opus review item 2: ADBTarget.ScreenSize
// against a fake adb across rotations 0, 1 and 3, and a failing dumpsys. It
// is what kills "skip orientedSize", "hard-code rotation 0" and "swallow the
// size error" as mutations, alongside the compile-time ScreenSizer assertion
// above for "ADBTarget no longer implements the interface".
func TestADBTargetScreenSize(t *testing.T) {
	script := writeFakeADB(t)
	tgt := NewADBTarget(ADB{Path: script}, DeviceInfo{Serial: "fake-1", State: "device"}, "", "")

	cases := []struct {
		name         string
		dump         string
		fail         bool
		wantW, wantH int
	}{
		{
			name:  "rotation0",
			dump:  fakeDumpsysWindowDisplays("1080x2424", "1080x2424", "ROTATION_0"),
			wantW: 1080, wantH: 2424,
		},
		{
			name:  "rotation1",
			dump:  fakeDumpsysWindowDisplays("1080x2424", "2424x1080", "ROTATION_90"),
			wantW: 2424, wantH: 1080,
		},
		{
			name:  "rotation3",
			dump:  fakeDumpsysWindowDisplays("1080x2424", "2424x1080", "ROTATION_270"),
			wantW: 2424, wantH: 1080,
		},
		{
			name: "dumpsysFails",
			fail: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("FAKE_ADB_DUMP", c.dump)
			if c.fail {
				t.Setenv("FAKE_ADB_FAIL", "device offline")
			} else {
				t.Setenv("FAKE_ADB_FAIL", "")
			}

			w, h, err := tgt.ScreenSize(t.Context())
			if c.fail {
				if err == nil {
					t.Fatalf("ScreenSize() = %d, %d, nil; want an error when dumpsys fails", w, h)
				}
				return
			}
			if err != nil {
				t.Fatalf("ScreenSize(): %v", err)
			}
			if w != c.wantW || h != c.wantH {
				t.Fatalf("ScreenSize() = %d, %d; want %d, %d", w, h, c.wantW, c.wantH)
			}
		})
	}
}
