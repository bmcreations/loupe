package android

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

// writeZip writes a zip holding one empty entry per name.
func writeZip(t *testing.T, path string, names ...string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, n := range names {
		if _, err := zw.Create(n); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// An .aab and an .apk are both zips, which is how a bundle got past the
// zip-signature check and reached adb as 0.apk. BundleConfig.pb at the root
// is what bundletool itself requires of a bundle.
func TestIsBundle(t *testing.T) {
	dir := t.TempDir()
	aab := filepath.Join(dir, "app.aab")
	writeZip(t, aab, "BundleConfig.pb", "base/manifest/AndroidManifest.xml", "base/dex/classes.dex")
	apk := filepath.Join(dir, "app.apk")
	writeZip(t, apk, "AndroidManifest.xml", "classes.dex", "resources.arsc")
	nested := filepath.Join(dir, "nested.apk")
	writeZip(t, nested, "AndroidManifest.xml", "assets/BundleConfig.pb")
	notZip := filepath.Join(dir, "0.apk")
	if err := os.WriteFile(notZip, []byte("PK\x03\x04rest of the zip"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		path string
		want bool
	}{
		{aab, true},
		{apk, false},
		{nested, false},
		{notZip, false},
		{filepath.Join(dir, "missing"), false},
	} {
		if got := IsBundle(c.path); got != c.want {
			t.Errorf("IsBundle(%s) = %v; want %v", filepath.Base(c.path), got, c.want)
		}
	}
}

// fakeBundletool writes a bundletool stand-in that records its argv, one
// call per line, and writes what each subcommand would: the device spec, the
// APK set, and the extracted splits.
func fakeBundletool(t *testing.T, extra string) (path, argvFile string) {
	t.Helper()
	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv")
	path = filepath.Join(dir, "bundletool")
	script := `#!/bin/sh
printf '%s\n' "$*" >> '` + argvFile + `'
echo "WARNING: A terminally deprecated method in sun.misc.Unsafe has been called" >&2
` + extra + `
cmd=$1
shift
for a in "$@"; do
  case $a in
    --output=*) out=${a#--output=} ;;
    --output-dir=*) outdir=${a#--output-dir=} ;;
  esac
done
case $cmd in
  get-device-spec) echo '{"supportedAbis":["arm64-v8a"],"sdkVersion":37}' > "$out" ;;
  build-apks) echo apkset > "$out" ;;
  extract-apks)
    mkdir -p "$outdir"
    for n in base-xxhdpi base-arm64_v8a base-master base-en; do echo PK > "$outdir/$n.apk"; done
    echo "The APKs have been extracted in the directory: $outdir" ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, argvFile
}

// The bundle is built for this device and no other: bundletool reads the
// spec off this serial through loupe's own adb, builds the set for that
// spec, and extracts what Play would install. The base comes first, so aapt2
// reads the package from the base rather than a config split.
func TestBundleAPKsBuildsTheSetForThisDevice(t *testing.T) {
	bt, argv := fakeBundletool(t, "")
	adb, _ := fakeADB(t, `echo Success`)
	tgt := NewDevice(ADB{Path: adb}, DeviceInfo{Serial: "emulator-5554", State: "device"}, "", "")
	dir := t.TempDir()
	aab := filepath.Join(dir, "0.aab")

	apks, err := tgt.BundleAPKs(context.Background(), []string{bt}, aab, dir)
	if err != nil {
		t.Fatalf("BundleAPKs: %v", err)
	}
	var names []string
	for _, p := range apks {
		if filepath.Dir(filepath.Dir(p)) != dir {
			t.Errorf("%s is not under the staging dir %s", p, dir)
		}
		names = append(names, filepath.Base(p))
	}
	if want := "base-master.apk base-arm64_v8a.apk base-en.apk base-xxhdpi.apk"; strings.Join(names, " ") != want {
		t.Errorf("apks = %q; want %q", names, want)
	}

	got := readArgv(t, argv)
	if len(got) != 3 {
		t.Fatalf("bundletool ran %d times: %q; want get-device-spec, build-apks, extract-apks", len(got), got)
	}
	spec := filepath.Join(dir, "device.json")
	set := filepath.Join(dir, "bundle.apks")
	for i, want := range []string{
		"get-device-spec --adb=" + adb + " --device-id=emulator-5554 --output=" + spec,
		"build-apks --bundle=" + aab + " --output=" + set + " --device-spec=" + spec,
		"extract-apks --apks=" + set + " --device-spec=" + spec + " --output-dir=" + filepath.Join(dir, "apks"),
	} {
		if got[i] != want {
			t.Errorf("call %d = %q; want %q", i, got[i], want)
		}
	}
}

// java -jar and the jar come before bundletool's own arguments.
func TestBundleAPKsRunsAJarThroughJava(t *testing.T) {
	java, argv := fakeBundletool(t, "shift 2")
	adb, _ := fakeADB(t, `echo Success`)
	tgt := NewDevice(ADB{Path: adb}, DeviceInfo{Serial: "s", State: "device"}, "", "")
	dir := t.TempDir()

	if _, err := tgt.BundleAPKs(context.Background(), []string{java, "-jar", "/opt/bundletool-all.jar"}, filepath.Join(dir, "0.aab"), dir); err != nil {
		t.Fatalf("BundleAPKs: %v", err)
	}
	if got := readArgv(t, argv); !strings.HasPrefix(got[0], "-jar /opt/bundletool-all.jar get-device-spec ") {
		t.Errorf("first call = %q; want java -jar <jar> get-device-spec", got[0])
	}
}

// A bundle bundletool will not build is the upload's fault, so it is an
// InstallError (the handler's 422) carrying bundletool's own sentence rather
// than the JVM warnings or the stack trace around it.
func TestBundleAPKsBadBundleIsAnInstallError(t *testing.T) {
	bt, _ := fakeBundletool(t, `if [ "$1" = build-apks ]; then
  echo "[BT:1.17.2] Error: The archive doesn't seem to be an App Bundle, it is missing required file 'BundleConfig.pb'."
  echo "com.android.tools.build.bundletool.model.exceptions.InvalidBundleException: The archive doesn't seem to be an App Bundle"
  echo "	at com.android.tools.build.bundletool.validation.ValidatorRunner.validateBundleZipFile(ValidatorRunner.java:43)"
  exit 1
fi`)
	adb, _ := fakeADB(t, `echo Success`)
	tgt := NewDevice(ADB{Path: adb}, DeviceInfo{Serial: "s", State: "device"}, "", "")
	dir := t.TempDir()

	_, err := tgt.BundleAPKs(context.Background(), []string{bt}, filepath.Join(dir, "0.aab"), dir)
	refused, ok := target.IsInstallRefused(err)
	if !ok {
		t.Fatalf("err = %v; want an *InstallError", err)
	}
	if refused.Code != BundletoolCode {
		t.Errorf("Code = %q; want %q", refused.Code, BundletoolCode)
	}
	if want := "The archive doesn't seem to be an App Bundle, it is missing required file 'BundleConfig.pb'."; refused.Reason != want {
		t.Errorf("Reason = %q; want %q", refused.Reason, want)
	}
	if !strings.Contains(refused.Output, "InvalidBundleException") {
		t.Errorf("Output = %q; want bundletool's whole answer kept", refused.Output)
	}
}

// bundletool prints the same "[BT:…] Error:" line for every exception it
// catches, its own and the JDK's, so the line alone does not say whose fault
// it was. The exception chain under it does. These are bundletool 1.17.2's
// answers, the stack frames cut.
func TestBundleAPKsTellsABadBundleFromAHostFailure(t *testing.T) {
	for _, c := range []struct {
		name   string
		out    string
		refuse bool
	}{
		{"not a bundle", `[BT:1.17.2] Error: The archive doesn't seem to be an App Bundle, it is missing required file 'BundleConfig.pb'.
com.android.tools.build.bundletool.model.exceptions.InvalidBundleException: The archive doesn't seem to be an App Bundle, it is missing required file 'BundleConfig.pb'.`, true},
		{"entry named ..", `[BT:1.17.2] Error: Name '..' is not supported inside path.
java.lang.IllegalArgumentException: Name '..' is not supported inside path.`, true},
		// A corrupt manifest is the bundle's, though the JDK wraps it in an
		// UncheckedIOException like a host I/O error.
		{"corrupt manifest", `[BT:1.17.2] Error: com.google.protobuf.InvalidProtocolBufferException: Protocol message had invalid UTF-8.
java.io.UncheckedIOException: com.google.protobuf.InvalidProtocolBufferException: Protocol message had invalid UTF-8.
Caused by: com.google.protobuf.InvalidProtocolBufferException: Protocol message had invalid UTF-8.`, true},
		// An entry whose deflate stream ends before its compressed size does
		// is the bundle's damage: the upload is all on disk before bundletool
		// runs. bundletool 1.18.3 printed these for BundleConfig.pb and for a
		// dex cut that way.
		{"config ends early", `[BT:1.18.3] Error: Error reading file 'BundleConfig.pb'.
java.io.UncheckedIOException: Error reading file 'BundleConfig.pb'.
Caused by: java.io.EOFException: Unexpected end of ZLIB input stream`, true},
		{"dex ends early", `[BT:1.18.3] Error: java.io.EOFException: Unexpected end of ZLIB input stream
com.google.common.util.concurrent.UncheckedExecutionException: java.io.EOFException: Unexpected end of ZLIB input stream
Caused by: java.io.EOFException: Unexpected end of ZLIB input stream`, true},
		{"temp dir missing", `[BT:1.17.2] Error: java.nio.file.NoSuchFileException: /tmp/gone/AutoValue_BuildApksCommand213534187188440284
java.io.UncheckedIOException: java.nio.file.NoSuchFileException: /tmp/gone/AutoValue_BuildApksCommand213534187188440284
Caused by: java.nio.file.NoSuchFileException: /tmp/gone/AutoValue_BuildApksCommand213534187188440284`, false},
		{"disk full", `[BT:1.17.2] Error: No space left on device
java.io.IOException: No space left on device`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			script := `if [ "$1" = build-apks ]; then
  cat <<'EOF'
` + c.out + `
	at com.android.tools.build.bundletool.commands.BuildApksCommand.execute(BuildApksCommand.java:1)
EOF
  exit 1
fi`
			bt, _ := fakeBundletool(t, script)
			adb, _ := fakeADB(t, `echo Success`)
			tgt := NewDevice(ADB{Path: adb}, DeviceInfo{Serial: "s", State: "device"}, "", "")
			dir := t.TempDir()

			_, err := tgt.BundleAPKs(context.Background(), []string{bt}, filepath.Join(dir, "0.aab"), dir)
			if err == nil {
				t.Fatal("BundleAPKs succeeded")
			}
			first := strings.SplitN(c.out, "\n", 2)[0]
			_, sentence, _ := strings.Cut(first, "] Error: ")
			refused, ok := target.IsInstallRefused(err)
			if ok != c.refuse {
				t.Fatalf("err = %v; refusal = %v, want %v", err, ok, c.refuse)
			}
			if ok && refused.Reason != sentence {
				t.Errorf("Reason = %q; want %q", refused.Reason, sentence)
			}
			if !ok && !strings.Contains(err.Error(), sentence) {
				t.Errorf("err = %v; want bundletool's sentence in it", err)
			}
		})
	}
}

// build-apks makes a directory under java.io.tmpdir and removes it only when
// it exits on its own. A dropped request kills it, so java.io.tmpdir goes
// under dir, which the handler removes however the request ends. It goes in
// JAVA_TOOL_OPTIONS, which reaches the JVM behind a wrapper script too, after
// whatever was set there already so that it wins.
func TestBundleAPKsPutsJavasTempDirUnderDir(t *testing.T) {
	for _, c := range []struct {
		name, existing, sub string
		quoted              bool
	}{
		{"nothing set", "", "", false},
		{"options already set", "-Xmx2g", "", false},
		{"dir with a space", "", "with space", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("JAVA_TOOL_OPTIONS", c.existing)
			record := filepath.Join(t.TempDir(), "env")
			bt, _ := fakeBundletool(t, `printf '%s\n' "$JAVA_TOOL_OPTIONS" >> '`+record+`'
tmp=${JAVA_TOOL_OPTIONS##*-Djava.io.tmpdir=}
tmp=${tmp#\"}
tmp=${tmp%\"}
[ -d "$tmp" ] && echo "$1 has it" >> '`+record+`'`)
			adb, _ := fakeADB(t, `echo Success`)
			tgt := NewDevice(ADB{Path: adb}, DeviceInfo{Serial: "s", State: "device"}, "", "")
			dir := filepath.Join(t.TempDir(), c.sub)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}

			if _, err := tgt.BundleAPKs(context.Background(), []string{bt}, filepath.Join(dir, "0.aab"), dir); err != nil {
				t.Fatalf("BundleAPKs: %v", err)
			}
			tmp := filepath.Join(dir, "tmp")
			opt := "-Djava.io.tmpdir=" + tmp
			if c.quoted {
				opt = `-Djava.io.tmpdir="` + tmp + `"`
			}
			want := strings.TrimSpace(c.existing + " " + opt)
			b, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			for i, step := range []string{"get-device-spec", "build-apks", "extract-apks"} {
				if 2*i+1 >= len(lines) {
					t.Fatalf("%s: no record: %q", step, lines)
				}
				if lines[2*i] != want {
					t.Errorf("%s: JAVA_TOOL_OPTIONS = %q; want %q", step, lines[2*i], want)
				}
				if lines[2*i+1] != step+" has it" {
					t.Errorf("%s: java.io.tmpdir did not exist when it ran: %q", step, lines)
				}
			}
		})
	}
}

// With no debug keystore, build-apks exits 0 having built APKs that no
// device installs. That is the Mac's to fix, not the bundle's, so it stops
// there with ErrNoDebugKeystore and nothing reaches the device.
func TestBundleAPKsWithNoDebugKeystoreStopsBeforeTheDevice(t *testing.T) {
	bt, argv := fakeBundletool(t, `if [ "$1" = build-apks ]; then
  echo "WARNING: The APKs won't be signed and thus not installable unless you also pass a keystore via the flag --ks. See the command help for more information."
fi`)
	adb, _ := fakeADB(t, `echo Success`)
	tgt := NewDevice(ADB{Path: adb}, DeviceInfo{Serial: "s", State: "device"}, "", "")
	dir := t.TempDir()

	_, err := tgt.BundleAPKs(context.Background(), []string{bt}, filepath.Join(dir, "0.aab"), dir)
	if !errors.Is(err, ErrNoDebugKeystore) {
		t.Fatalf("err = %v; want ErrNoDebugKeystore", err)
	}
	if _, ok := target.IsInstallRefused(err); ok {
		t.Errorf("err = %v is a refusal of the bundle; want the Mac's", err)
	}
	if got := readArgv(t, argv); len(got) != 2 {
		t.Errorf("bundletool ran %q; want it to stop after build-apks", got)
	}
}

// Reading the device's spec failing is the device or adb, not the bundle, so
// it is not an InstallError.
func TestBundleAPKsDeviceSpecFailureIsNotARefusal(t *testing.T) {
	bt, _ := fakeBundletool(t, `if [ "$1" = get-device-spec ]; then
  echo "[BT:1.17.2] Error: Device not found."
  exit 1
fi`)
	adb, _ := fakeADB(t, `echo Success`)
	tgt := NewDevice(ADB{Path: adb}, DeviceInfo{Serial: "s", State: "device"}, "", "")
	dir := t.TempDir()

	_, err := tgt.BundleAPKs(context.Background(), []string{bt}, filepath.Join(dir, "0.aab"), dir)
	if err == nil {
		t.Fatal("BundleAPKs succeeded")
	}
	if _, ok := target.IsInstallRefused(err); ok {
		t.Errorf("err = %v is a refusal; want a device failure", err)
	}
	if !strings.Contains(err.Error(), "Device not found.") {
		t.Errorf("err = %v; want bundletool's sentence in it", err)
	}
}

func TestBundleAPKsOnADeviceNotReadyRunsNothing(t *testing.T) {
	bt, argv := fakeBundletool(t, "")
	tgt := NewDevice(ADB{Path: "/nonexistent/adb"}, DeviceInfo{Serial: "s", State: "offline"}, "", "")
	dir := t.TempDir()
	if _, err := tgt.BundleAPKs(context.Background(), []string{bt}, filepath.Join(dir, "0.aab"), dir); !errors.Is(err, target.ErrDeviceUnreachable) {
		t.Fatalf("err = %v; want ErrDeviceUnreachable", err)
	}
	if _, err := os.Stat(argv); err == nil {
		t.Error("bundletool ran for a device that is not ready")
	}
}

func TestFindBundletool(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "bundletool")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec java -jar x.jar \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Google publishes bundletool as a jar, and a downloaded one is not
	// executable. It is told apart by its zip signature, not its name.
	jar := filepath.Join(dir, "bundletool-all.jar")
	writeZip(t, jar, "META-INF/MANIFEST.MF")
	if err := os.Chmod(jar, 0o644); err != nil {
		t.Fatal(err)
	}
	notExec := filepath.Join(dir, "script")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	java := filepath.Join(dir, "java")
	if err := os.WriteFile(java, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing")
	javas := func() []string { return []string{missing, java} }
	noJava := func() []string { return []string{missing} }

	for _, c := range []struct {
		name     string
		explicit string
		places   []string
		javas    func() []string
		want     string
		err      error
	}{
		{"explicit wrapper", wrapper, nil, javas, wrapper, nil},
		{"explicit jar", jar, []string{wrapper}, javas, java + " -jar " + jar, nil},
		{"first place that exists", "", []string{missing, wrapper}, javas, wrapper, nil},
		{"jar in a place", "", []string{jar}, javas, java + " -jar " + jar, nil},
		{"jar with no java", "", []string{jar}, noJava, "", ErrNoJava},
		// A jar nothing can run does not hide a wrapper further on.
		{"jar with no java, then a wrapper", "", []string{jar, wrapper}, noJava, wrapper, nil},
		{"explicit jar with no java", jar, []string{wrapper}, noJava, "", ErrNoJava},
		{"none", "", []string{missing}, javas, "", ErrNoBundletool},
		{"explicit that does not exist", missing, []string{wrapper}, javas, "", ErrNoBundletool},
		{"script that is not executable", "", []string{notExec}, javas, "", ErrNoBundletool},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := findBundletool(c.explicit, c.places, c.javas)
			if !errors.Is(err, c.err) {
				t.Fatalf("err = %v; want %v", err, c.err)
			}
			if strings.Join(got, " ") != c.want {
				t.Errorf("command = %q; want %q", got, c.want)
			}
		})
	}
}

// The macOS /usr/bin/java is a stub that fails when no JDK is registered, so
// a java that resolves is not taken on trust: JAVA_HOME and the registered
// JDK come first, a java on PATH counts only when it is not the stub, and
// the stub never appears.
func TestJavaCandidates(t *testing.T) {
	t.Setenv("JAVA_HOME", "/jdk")
	got := javaCandidates(func() string { return "/registered/jdk" }, func() string { return "/sdkman/java/bin/java" })
	if len(got) < 4 || got[0] != "/jdk/bin/java" || got[1] != "/registered/jdk/bin/java" || got[2] != "/sdkman/java/bin/java" {
		t.Errorf("candidates = %q; want JAVA_HOME's java, then java_home's, then PATH's", got)
	}
	got = javaCandidates(func() string { return "" }, func() string { return "/usr/bin/java" })
	for _, p := range got {
		if p == "/usr/bin/java" {
			t.Errorf("candidates include the /usr/bin/java stub: %q", got)
		}
	}
}
