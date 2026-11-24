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

// btErrorRE is the sentence bundletool prints for any exception it catches,
// its own or the JDK's, such as "[BT:1.17.2] Error: The archive doesn't seem
// to be an App Bundle". The stack trace follows it, and JVM warnings come
// before it.
var btErrorRE = regexp.MustCompile(`(?m)^\[BT:[^\]]*\] Error: (.+)$`)

// btExceptionRE is a stack trace line naming an exception: the trace's first
// line and each "Caused by:". Frames start with a tab and never match.
var btExceptionRE = regexp.MustCompile(`(?m)^(?:Caused by: )?((?:[A-Za-z_$][\w$]*\.)+[A-Za-z_$][\w$]*)(?::|$)`)

// btUnsignedRE is the warning build-apks prints, before it exits 0, when it
// found no keystore to sign the APKs with.
var btUnsignedRE = regexp.MustCompile(`(?m)^WARNING: The APKs won't be signed`)

// BundleAPKs turns the App Bundle at aab into the APKs this device takes,
// the way Play would: bundletool reads the device's ABIs, density, locales
// and SDK level off it through loupe's own adb, builds the APK set for that
// spec, and extracts the base and the config splits it needs. The APKs are
// written under dir and returned base first, ready for Install.
//
// bundletool signs what it builds with ~/.android/debug.keystore, the key
// Gradle debug builds use, not with the bundle's upload key. Without that
// file it builds unsigned APKs that no device installs, so BundleAPKs stops
// there with ErrNoDebugKeystore.
//
// bundletool is the command that runs it: a wrapper script, or java -jar
// and the jar. Every argument is a host path or this device's serial, passed
// to exec, so nothing here reaches the device's shell. Its JVM keeps its
// temp files under dir. A bundle bundletool will not build is an
// *InstallError with BundletoolCode; failing to read the device's spec is
// not, since that is the device or adb, and neither is an I/O error on the
// Mac while bundletool runs.
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
	// build-apks makes a directory under java.io.tmpdir and removes it only
	// when it exits on its own; a dropped request kills it. Under dir, the
	// caller's removing dir takes that directory too. The JVM fails if the
	// directory is missing, so it is made first.
	tmp := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return nil, fmt.Errorf("bundletool temp dir: %w", err)
	}
	env := append(os.Environ(), "JAVA_TOOL_OPTIONS="+javaToolOptions(os.Getenv("JAVA_TOOL_OPTIONS"), tmp))
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, bundletool[0], append(bundletool[1:len(bundletool):len(bundletool)], args...)...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	spec := filepath.Join(dir, "device.json")
	if out, err := run("get-device-spec", "--adb="+adb, "--device-id="+t.info.Serial, "--output="+spec); err != nil {
		return nil, fmt.Errorf("bundletool get-device-spec: %w: %s", err, bundletoolMessage(out))
	}
	set := filepath.Join(dir, "bundle.apks")
	out, err := run("build-apks", "--bundle="+aab, "--output="+set, "--device-spec="+spec)
	if err != nil {
		return nil, bundletoolFailed("build-apks", out, err)
	}
	if btUnsignedRE.MatchString(out) {
		return nil, ErrNoDebugKeystore
	}
	outDir := filepath.Join(dir, "apks")
	if out, err := run("extract-apks", "--apks="+set, "--device-spec="+spec, "--output-dir="+outDir); err != nil {
		return nil, bundletoolFailed("extract-apks", out, err)
	}
	return extractedAPKs(outDir)
}

// javaToolOptions is JAVA_TOOL_OPTIONS with java.io.tmpdir set to tmp,
// after the options already there so that it wins over any tmpdir among
// them. The JVM splits the variable at spaces, and quotes keep a path with
// one whole.
func javaToolOptions(existing, tmp string) string {
	opt := "-Djava.io.tmpdir=" + tmp
	if strings.ContainsAny(tmp, " \t\n") {
		opt = `-Djava.io.tmpdir="` + tmp + `"`
	}
	return strings.TrimSpace(existing + " " + opt)
}

// bundletoolFailed is a failed build-apks or extract-apks. bundletool's
// error line with a bundle exception under it means it judged the bundle
// and refused it, which is the upload's fault and so an InstallError.
// Anything else is not: an I/O error on the Mac, or the JVM dying before
// bundletool started.
func bundletoolFailed(step, out string, err error) error {
	if m := btErrorRE.FindStringSubmatchIndex(out); m != nil && !hostFault(out[m[1]:]) {
		return &target.InstallError{Code: BundletoolCode, Reason: strings.TrimSpace(out[m[2]:m[3]]), Output: out}
	}
	return fmt.Errorf("bundletool %s: %w: %s", step, err, bundletoolMessage(out))
}

// hostFault reports whether the stack trace bundletool printed under its
// error line is the Mac's fault rather than the bundle's: some exception in
// its chain is the JDK's I/O, in java.io or java.nio, as a missing temp dir
// (java.nio.file.NoSuchFileException) or a full disk (java.io.IOException)
// is. Two of them do not count. java.io.UncheckedIOException alone: the JDK
// wraps a bundle's corrupt protobuf in one too, and then its cause is
// protobuf's. java.io.EOFException: the upload is all on disk before
// bundletool runs, so a stream ending early is a zip entry whose deflate
// stream is cut short, the bundle's damage.
func hostFault(trace string) bool {
	for _, m := range btExceptionRE.FindAllStringSubmatch(trace, -1) {
		switch c := m[1]; {
		case c == "java.io.UncheckedIOException", c == "java.io.EOFException":
		case strings.HasPrefix(c, "java.io."), strings.HasPrefix(c, "java.nio."):
			return true
		}
	}
	return false
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

// ErrNoJava is FindBundletool finding bundletool only as a jar, with no
// Java runtime to run it.
var ErrNoJava = errors.New("bundletool was found as a jar, but no Java runtime to run it: install one with `brew install openjdk`, or set JAVA_HOME for loupe")

// ErrNoDebugKeystore is BundleAPKs finding that bundletool had no debug
// keystore to sign with, so the APKs it built would be refused by any
// device. Its text says how to make one.
var ErrNoDebugKeystore = errors.New("bundletool found no ~/.android/debug.keystore to sign the APKs it builds from an .aab, and a device installs no unsigned APK: run any Gradle debug build once on the Mac running loupe to create that file, then install again")

// FindBundletool is the command that runs bundletool: an explicit
// -bundletool path, else bundletool on PATH, then Homebrew's two prefixes
// for a loupe started with a PATH that lacks them (launchd's plist puts both
// first). A path that is a jar runs through java -jar. It is looked up on
// each install, so installing bundletool needs no restart of loupe.
func FindBundletool(explicit string) ([]string, error) {
	var places []string
	if p, err := exec.LookPath("bundletool"); err == nil {
		places = append(places, p)
	}
	places = append(places, "/opt/homebrew/bin/bundletool", "/usr/local/bin/bundletool")
	return findBundletool(explicit, places, func() []string { return javaCandidates(javaHome, javaOnPath) })
}

// findBundletool is FindBundletool with its places and Java runtimes given.
// An explicit path is the only one tried, so a wrong -bundletool fails
// rather than quietly running some other copy.
func findBundletool(explicit string, places []string, javas func() []string) ([]string, error) {
	if explicit != "" {
		places = []string{explicit}
	}
	missing := ErrNoBundletool
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
			// A wrapper further on may bring its own Java.
			missing = ErrNoJava
			continue
		}
		if fi.Mode()&0o111 != 0 {
			return []string{p}, nil
		}
	}
	return nil, missing
}

// stubJava is the java macOS ships, a stub that fails when no JDK is
// registered. When one is, javaHome finds it.
const stubJava = "/usr/bin/java"

// javaCandidates are the Java runtimes to run a bundletool jar with, in
// order: JAVA_HOME, the JDK macOS has registered, the java on PATH unless it
// is the stub, Homebrew's openjdk and Android Studio's bundled runtime.
func javaCandidates(javaHome, onPath func() string) []string {
	var out []string
	if h := os.Getenv("JAVA_HOME"); h != "" {
		out = append(out, filepath.Join(h, "bin", "java"))
	}
	if h := javaHome(); h != "" {
		out = append(out, filepath.Join(h, "bin", "java"))
	}
	if p := onPath(); p != "" && p != stubJava {
		out = append(out, p)
	}
	return append(out,
		"/opt/homebrew/opt/openjdk/bin/java",
		"/usr/local/opt/openjdk/bin/java",
		"/Applications/Android Studio.app/Contents/jbr/Contents/Home/bin/java",
	)
}

// javaOnPath is the java PATH finds, or "" when there is none or it is the
// stub, through a symlink or not.
func javaOnPath() string {
	p, err := exec.LookPath("java")
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil && r == stubJava {
		return ""
	}
	return p
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
