package android

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/bmcreations/loupe/internal/target"
)

var (
	_ target.BundleInstaller = (*Device)(nil)
	_ target.BundleInstaller = (*Emulator)(nil)
)

// IsBundle reports whether the file at path is an Android App Bundle (.aab)
// rather than an APK. Both are zips, so the zip signature that gets an
// upload staged cannot tell them apart; BundleConfig.pb at the root is the
// file bundletool itself requires of a bundle, and an APK never has one.
func IsBundle(path string) bool {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return false
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name == "BundleConfig.pb" {
			return true
		}
	}
	return false
}

// BundletoolCode is InstallError.Code when bundletool refused to turn an
// upload into APKs, so the package manager was never asked.
const BundletoolCode = "BUNDLETOOL_FAILED"

// btErrorRE is the sentence bundletool prints for an error it raised itself,
// such as "[BT:1.17.2] Error: The archive doesn't seem to be an App Bundle".
// A Java stack trace follows it, and JVM warnings come before it.
var btErrorRE = regexp.MustCompile(`(?m)^\[BT:[^\]]*\] Error: (.+)$`)

// BundleAPKs turns the App Bundle at aab into the APKs this device takes,
// the way Play would: bundletool reads the device's ABIs, density, locales
// and SDK level off it through loupe's own adb, builds the APK set for that
// spec, and extracts the base and the config splits it needs. The APKs are
// written under dir and returned base first, ready for Install.
//
// bundletool signs what it builds with ~/.android/debug.keystore, the key
// Gradle debug builds use, not with the bundle's upload key; without that
// file it leaves the APKs unsigned and the package manager refuses them with
// INSTALL_PARSE_FAILED_NO_CERTIFICATES.
//
// bundletool is the command that runs it: a wrapper script, or java -jar
// and the jar. Every argument is a host path or this device's serial, passed
// to exec, so nothing here reaches the device's shell. A bundle bundletool
// will not build is an *InstallError with BundletoolCode; failing to read the
// device's spec is not, since that is the device or adb.
func (t *Device) BundleAPKs(ctx context.Context, bundletool []string, aab, dir string) ([]string, error) {
	if len(bundletool) == 0 {
		return nil, ErrNoBundletool
	}
	if !t.info.Ready() {
		return nil, fmt.Errorf("%w: device %s is in state %q", target.ErrDeviceUnreachable, t.info.Serial, t.info.State)
	}
	// bundletool runs adb itself and wants it as a path it can stat, not a
	// name to look up.
	adb, err := exec.LookPath(t.adb.bin())
	if err != nil {
		return nil, fmt.Errorf("bundletool needs adb: %w", err)
	}
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, bundletool[0], append(bundletool[1:len(bundletool):len(bundletool)], args...)...)
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	spec := filepath.Join(dir, "device.json")
	if out, err := run("get-device-spec", "--adb="+adb, "--device-id="+t.info.Serial, "--output="+spec); err != nil {
		return nil, fmt.Errorf("bundletool get-device-spec: %w: %s", err, bundletoolMessage(out))
	}
	set := filepath.Join(dir, "bundle.apks")
	if out, err := run("build-apks", "--bundle="+aab, "--output="+set, "--device-spec="+spec); err != nil {
		return nil, bundletoolFailed("build-apks", out, err)
	}
	outDir := filepath.Join(dir, "apks")
	if out, err := run("extract-apks", "--apks="+set, "--device-spec="+spec, "--output-dir="+outDir); err != nil {
		return nil, bundletoolFailed("extract-apks", out, err)
	}
	return extractedAPKs(outDir)
}

// bundletoolFailed is a failed build-apks or extract-apks. bundletool's own
// error line means it judged the bundle and refused it, which is the
// upload's fault and so an InstallError; anything else, such as the JVM
// dying, is not.
func bundletoolFailed(step, out string, err error) error {
	if m := btErrorRE.FindStringSubmatch(out); m != nil {
		return &target.InstallError{Code: BundletoolCode, Reason: strings.TrimSpace(m[1]), Output: out}
	}
	return fmt.Errorf("bundletool %s: %w: %s", step, err, bundletoolMessage(out))
}

// bundletoolMessage is the line of bundletool's output worth quoting: its
// own error sentence when there is one, else the last line, which for a JVM
// that never got as far as bundletool is the JVM's.
func bundletoolMessage(out string) string {
	if m := btErrorRE.FindStringSubmatch(out); m != nil {
		return strings.TrimSpace(m[1])
	}
	return target.LastLine(out)
}

// extractedAPKs lists what extract-apks wrote, base-master.apk first so the
// package name aapt2 reads off the first APK is the base's, then the rest in
// name order.
func extractedAPKs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("bundletool extract-apks: %w", err)
	}
	var apks []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".apk") {
			apks = append(apks, filepath.Join(dir, e.Name()))
		}
	}
	if len(apks) == 0 {
		return nil, errors.New("bundletool extract-apks wrote no APKs for this device")
	}
	sort.SliceStable(apks, func(i, j int) bool {
		bi, bj := filepath.Base(apks[i]) == "base-master.apk", filepath.Base(apks[j]) == "base-master.apk"
		if bi != bj {
			return bi
		}
		return apks[i] < apks[j]
	})
	if len(apks) > maxBundleAPKs {
		return nil, fmt.Errorf("bundletool extracted %d APKs for this device; at most %d are installed at once", len(apks), maxBundleAPKs)
	}
	return apks, nil
}

// maxBundleAPKs matches the bound /api/install puts on an upload's splits.
const maxBundleAPKs = 64

// ErrNoBundletool is FindBundletool finding no bundletool. Its text says
// what to install, since that is all a caller can do about it.
var ErrNoBundletool = errors.New("installing an .aab needs bundletool, which turns the bundle into the APKs this device takes, and none was found: install it with `brew install bundletool`, or start loupe with -bundletool set to a bundletool script or bundletool-all.jar")

// ErrNoJava is FindBundletool finding bundletool as a jar with no Java
// runtime to run it.
var ErrNoJava = errors.New("bundletool was found as a jar, but no Java runtime to run it: install one with `brew install openjdk`, or set JAVA_HOME for loupe")

// FindBundletool is the command that runs bundletool: an explicit
// -bundletool path, else bundletool on PATH, then Homebrew's two prefixes,
// since launchd's PATH reaches neither. A path that is a jar runs through
// java -jar. It is looked up on each install, so installing bundletool needs
// no restart of loupe.
func FindBundletool(explicit string) ([]string, error) {
	var places []string
	if p, err := exec.LookPath("bundletool"); err == nil {
		places = append(places, p)
	}
	places = append(places, "/opt/homebrew/bin/bundletool", "/usr/local/bin/bundletool")
	return findBundletool(explicit, places, func() []string { return javaCandidates(javaHome) })
}

// findBundletool is FindBundletool with its places and Java runtimes given.
// An explicit path is the only one tried, so a wrong -bundletool fails
// rather than quietly running some other copy.
func findBundletool(explicit string, places []string, javas func() []string) ([]string, error) {
	if explicit != "" {
		places = []string{explicit}
	}
	for _, p := range places {
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		// Google publishes bundletool as a jar, and one downloaded by hand
		// is not executable, so it is told apart by its zip signature.
		if isZip(p) {
			for _, j := range javas() {
				if isExecutable(j) {
					return []string{j, "-jar", p}, nil
				}
			}
			return nil, ErrNoJava
		}
		if fi.Mode()&0o111 != 0 {
			return []string{p}, nil
		}
	}
	return nil, ErrNoBundletool
}

// javaCandidates are the Java runtimes to run a bundletool jar with, in
// order: JAVA_HOME, the JDK macOS has registered, Homebrew's openjdk and
// Android Studio's bundled runtime. /usr/bin/java is left out: it is a stub
// that fails when no JDK is registered, and when one is, javaHome finds it.
func javaCandidates(javaHome func() string) []string {
	var out []string
	if h := os.Getenv("JAVA_HOME"); h != "" {
		out = append(out, filepath.Join(h, "bin", "java"))
	}
	if h := javaHome(); h != "" {
		out = append(out, filepath.Join(h, "bin", "java"))
	}
	return append(out,
		"/opt/homebrew/opt/openjdk/bin/java",
		"/usr/local/opt/openjdk/bin/java",
		"/Applications/Android Studio.app/Contents/jbr/Contents/Home/bin/java",
	)
}

// javaHome is the JDK /usr/libexec/java_home reports, or "" when it reports
// none, which it does by exiting non-zero.
func javaHome() string {
	out, err := exec.Command("/usr/libexec/java_home").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func isZip(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 4)
	if _, err := f.Read(head); err != nil {
		return false
	}
	return bytes.Equal(head, []byte("PK\x03\x04"))
}

func isExecutable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0
}
