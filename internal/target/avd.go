package target

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// AVD is one installed Android Virtual Device, read from disk without booting
// it. It is enough to list the AVD and describe it; only booting needs the SDK.
type AVD struct {
	// Name is what `emulator -avd` takes and what ro.boot.qemu.avd_name reports
	// once it runs, which is how a running emulator is mapped back to it.
	Name string
	// Dir is the .avd directory holding config.ini.
	Dir string
	// DisplayName is avd.ini.displayname, or Name with underscores as spaces.
	DisplayName string
	// Device is hw.device.name, such as "pixel_10" or "pixel_tablet".
	Device   string
	APILevel int
	// Hinge is hw.sensor.hinge, which a foldable's device definition sets.
	Hinge bool
}

// Description is what the AVD's config says about the device it emulates.
// OSVersion is left out: config.ini records the API level and not the release
// name, and the release is read from getprop once the AVD boots rather than
// guessed from a table here.
func (a AVD) Description() Description {
	form := formFromDevice(a.Device)
	if a.Hinge && form != "" {
		form = "foldable"
	}
	return Description{OS: "android", APILevel: a.APILevel, Form: form, Transport: "emulator"}
}

// formFromDevice classifies an SDK device definition name. The SDK has no
// field for this, so it goes by the naming the device definitions use:
// "pixel_tablet", "7.6in Foldable", "tv_1080p", "wearos_large_round",
// "automotive_1024p_landscape". A television or a watch is omitted rather than
// called a phone, the same rule formFrom applies to ro.build.characteristics.
func formFromDevice(device string) string {
	d := strings.ToLower(device)
	switch {
	case strings.Contains(d, "tv"), strings.Contains(d, "wear"), strings.Contains(d, "automotive"):
		return ""
	case strings.Contains(d, "fold"):
		// pixel_fold, pixel_9_pro_fold: a foldable even where config.ini
		// carries no hw.sensor.hinge.
		return "foldable"
	case strings.Contains(d, "tablet"):
		return "tablet"
	default:
		return "phone"
	}
}

// AVDHome is where AVDs are installed: ANDROID_AVD_HOME, then
// $ANDROID_USER_HOME/avd, then ~/.android/avd, the order the emulator itself
// uses.
func AVDHome() string {
	if d := os.Getenv("ANDROID_AVD_HOME"); d != "" {
		return d
	}
	return CLIAVDHome()
}

// CLIAVDHome is where the android CLI creates an AVD: $ANDROID_USER_HOME/avd,
// then ~/.android/avd. It does not read ANDROID_AVD_HOME. With that set to a
// scratch directory and ANDROID_USER_HOME to another, `android emulator
// create` wrote the AVD under the second (CLI 1.0.16500706).
func CLIAVDHome() string {
	if d := os.Getenv("ANDROID_USER_HOME"); d != "" {
		return filepath.Join(d, "avd")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".android", "avd")
}

// ScanAVDs lists the AVDs installed under home, sorted by name. It reads
// `<name>.ini` for the path to each AVD and that directory's config.ini for the
// rest.
//
// This reads files rather than asking a tool. `avdmanager list avd` is the
// obvious tool and it is wrong here: its device table predates the Pixel 10,
// so it reports Pixel_10 as "could not be loaded" and lists only the other
// AVD. `emulator -list-avds` is correct, but needs the SDK found first and a
// subprocess every scan.
//
// An AVD whose config cannot be read is still listed, by name alone: it may
// well boot, and leaving it out would hide it with no explanation.
func ScanAVDs(home string) ([]AVD, error) {
	inis, err := filepath.Glob(filepath.Join(home, "*.ini"))
	if err != nil {
		return nil, err
	}
	var out []AVD
	for _, ini := range inis {
		name := strings.TrimSuffix(filepath.Base(ini), ".ini")
		a := AVD{Name: name, Dir: filepath.Join(home, name+".avd")}
		if kv, err := readINI(ini); err == nil && kv["path"] != "" {
			a.Dir = kv["path"]
		}
		if _, err := os.Stat(a.Dir); err != nil {
			// A <name>.ini whose directory is gone is what a half-deleted AVD
			// leaves behind; the emulator cannot boot it either.
			continue
		}
		if kv, err := readINI(filepath.Join(a.Dir, "config.ini")); err == nil {
			a.DisplayName = kv["avd.ini.displayname"]
			a.Device = kv["hw.device.name"]
			a.Hinge = kv["hw.sensor.hinge"] == "yes"
			a.APILevel = apiLevelFrom(kv["target"], kv["image.sysdir.1"])
		}
		if a.DisplayName == "" {
			a.DisplayName = strings.ReplaceAll(name, "_", " ")
		}
		out = append(out, a)
	}
	slices.SortFunc(out, func(x, y AVD) int { return strings.Compare(x.Name, y.Name) })
	return out, nil
}

// apiLevelFrom reads the API level out of config.ini's `target`
// ("android-37.0", "android-36") or, failing that, the system image path
// ("system-images/android-36/google_apis_playstore/arm64-v8a/"). A preview
// target such as "android-Baklava" has no number, and 0 omits it.
func apiLevelFrom(target, sysdir string) int {
	for _, s := range []string{target, sysdir} {
		for _, part := range strings.Split(s, "/") {
			rest, ok := strings.CutPrefix(part, "android-")
			if !ok {
				continue
			}
			rest, _, _ = strings.Cut(rest, ".")
			if n, err := strconv.Atoi(rest); err == nil {
				return n
			}
		}
	}
	return 0
}

// readINI reads the emulator's key=value files. Both spacings occur in the
// wild: Android Studio writes `key=value` and avdmanager writes
// `key = value`, and this machine has one AVD of each.
func readINI(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	defer f.Close()
	kv := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return kv, sc.Err()
}

// RepairAVDTarget fixes an AVD whose <name>.ini says `target=android-0` (or
// has no numeric target) by reading the API level from config.ini's system
// image path. It returns the target it wrote, or "" when nothing changed.
func RepairAVDTarget(dir string) (string, error) {
	if dir == "" {
		return "", nil
	}
	ini := strings.TrimSuffix(filepath.Clean(dir), ".avd") + ".ini"
	kv, err := readINI(ini)
	if err != nil {
		return "", err
	}
	if apiLevelFrom(kv["target"], "") > 0 {
		return "", nil
	}
	cfg, err := readINI(filepath.Join(dir, "config.ini"))
	if err != nil {
		return "", err
	}
	n := apiLevelFrom("", cfg["image.sysdir.1"])
	if n == 0 {
		return "", nil
	}
	raw, err := os.ReadFile(ini)
	if err != nil {
		return "", err
	}
	want := fmt.Sprintf("android-%d", n)
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	found := false
	for i, l := range lines {
		if k, _, ok := strings.Cut(l, "="); ok && strings.TrimSpace(k) == "target" {
			lines[i], found = "target="+want, true
		}
	}
	if !found {
		lines = append(lines, "target="+want)
	}
	return want, writeFileAtomic(ini, []byte(strings.Join(lines, "\n")+"\n"))
}
