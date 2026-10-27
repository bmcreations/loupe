package target

import (
	"os"
	"path/filepath"
)

// SDKRoot finds the Android SDK: the explicit path if one was given, then
// ANDROID_HOME, then ANDROID_SDK_ROOT, then Android Studio's default install
// on macOS. It returns "" if none of them is a directory.
//
// Both variables are unset on the machine this runs on, and launchd's PATH
// does not reach the SDK, which is why a bare "emulator" could never start an
// AVD under the supervisor. Nothing here derives the SDK from adb: the adb on
// PATH is Homebrew's platform-tools cask, whose parent is not an SDK.
func SDKRoot(explicit string) string {
	home, _ := os.UserHomeDir()
	candidates := []string{
		explicit,
		os.Getenv("ANDROID_HOME"),
		os.Getenv("ANDROID_SDK_ROOT"),
	}
	if home != "" {
		candidates = append(candidates, filepath.Join(home, "Library", "Android", "sdk"))
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if fi, err := os.Stat(c); err == nil && fi.IsDir() {
			return c
		}
	}
	return ""
}

// EmulatorBinary is the emulator inside an SDK, or plain "emulator" when no
// SDK was found, leaving PATH as the last resort it always was.
func EmulatorBinary(sdk string) string {
	if sdk == "" {
		return "emulator"
	}
	p := filepath.Join(sdk, "emulator", "emulator")
	if _, err := os.Stat(p); err != nil {
		return "emulator"
	}
	return p
}

// AndroidCLI finds Google's `android` command-line tool, which creates an AVD
// from a profile. The explicit path wins, then the two places its installer
// and Homebrew put it, since launchd's PATH reaches neither. It returns ""
// when none exists, and creation is then not offered.
func AndroidCLI(explicit string) string {
	for _, c := range []string{explicit, "/usr/local/bin/android", "/opt/homebrew/bin/android"} {
		if c == "" {
			continue
		}
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return ""
}
