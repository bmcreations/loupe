package server

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/bmcreations/loupe/internal/scrcpy"
)

// TestJarIsThePinnedArtifact is the tripwire under every scrcpy constant loupe
// hardcodes. Those constants were read out of this jar's dex and nothing at
// runtime re-checks them, so a jar swap has to fail here rather than on a
// device.
//
// The cost of not having this is known. scrcpy 4.1 moves PACKET_FLAG_CONFIG
// from bit 63 to bit 62 and PACKET_FLAG_KEY_FRAME from 62 to 61, which would
// make loupe read a config packet as a keyframe and add 2^61 microseconds to
// every keyframe PTS. Unknown launch options only warn (Ln.w), so it would
// launch cleanly and then misparse in silence.
func TestJarIsThePinnedArtifact(t *testing.T) {
	if got := sum(Jar()); got != SHA256 {
		t.Fatalf("embedded jar changed\n got %s\nwant %s\n\n"+
			"Re-verify against the new dex before repinning: PACKET_FLAG_CONFIG and\n"+
			"PACKET_FLAG_KEY_FRAME in scrcpy/device/Streamer, TYPE_RESET_VIDEO in the\n"+
			"control message types, and every launch option in launch.go.", got, SHA256)
	}
}

// TestVersionMatchesTheJar catches the drift that produces the least
// informative failure in the whole device path: scrcpy-server exits on a
// version mismatch before opening its socket, so loupe sees a connect timeout
// with nothing pointing at the cause.
func TestVersionMatchesTheJar(t *testing.T) {
	dex := classesDex(t)
	if !hasDexString(dex, Version) {
		t.Fatalf("Version = %q but that string is not in the jar's dex", Version)
	}
}

// TestLaunchOptionsExist asserts every option name Launch passes is one this
// jar actually parses. scrcpy warns and continues on an unknown option rather
// than refusing, so a rename lands as changed behaviour and no error: 4.1
// renamed send_codec_meta to send_stream_meta, which would have silently
// dropped the codec metadata prefix that the stream header parser expects.
//
// The names come from scrcpy.OptionNames rather than a copy, so an option
// added to Launch is covered without anyone remembering to add it here.
func TestLaunchOptionsExist(t *testing.T) {
	dex := classesDex(t)
	for _, opt := range scrcpy.OptionNames() {
		if !hasDexString(dex, opt) {
			t.Errorf("launch option %q is not parsed by scrcpy-server %s", opt, Version)
		}
	}
}

// TestCodecOptionKeysExist covers the half of video_codec_options that scrcpy
// does not interpret. The option name is scrcpy's; the keys inside its value
// are MediaFormat's, passed through to the encoder. A key the encoder does not
// know is ignored in the same silence, so an unbounded GOP would come back
// looking exactly like trap 15.
func TestCodecOptionKeysExist(t *testing.T) {
	dex := classesDex(t)
	for _, key := range []string{"i-frame-interval", scrcpy.DefaultVideoCodec} {
		if !hasDexString(dex, key) {
			t.Errorf("%q is not known to scrcpy-server %s", key, Version)
		}
	}
}

func TestMaterializeWritesTheJarAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path, err := Materialize(dir)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read staged jar: %v", err)
	}
	if !bytes.Equal(got, Jar()) {
		t.Fatalf("staged jar differs from the embedded bytes")
	}

	// A second call must not rewrite: repeated device launches should cost a
	// hash, not a write.
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Materialize(dir)
	if err != nil {
		t.Fatalf("second Materialize: %v", err)
	}
	if again != path {
		t.Fatalf("path moved between calls: %q then %q", path, again)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("jar was rewritten despite being present and correct")
	}

	// A corrupted stage must be repaired rather than pushed to a device.
	if err := os.WriteFile(path, []byte("truncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Materialize(dir); err != nil {
		t.Fatalf("third Materialize: %v", err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, Jar()) {
		t.Errorf("a corrupted staged jar was left in place")
	}
}

func TestMaterializeCreatesMissingDirectories(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "cache")
	if _, err := Materialize(dir); err != nil {
		t.Fatalf("Materialize into a missing directory: %v", err)
	}
}

// classesDex pulls classes.dex out of the embedded jar.
func classesDex(t *testing.T) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(Jar()), int64(len(Jar())))
	if err != nil {
		t.Fatalf("open embedded jar: %v", err)
	}
	for _, f := range zr.File {
		if f.Name != "classes.dex" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open classes.dex: %v", err)
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read classes.dex: %v", err)
		}
		return b
	}
	t.Fatal("no classes.dex in the embedded jar")
	return nil
}

// hasDexString reports whether s is in the dex string pool as a whole entry.
//
// Dex stores strings as a ULEB128 length followed by MUTF-8 bytes and a NUL.
// Matching the length and terminator rather than the bare bytes is what keeps
// "video" from matching inside "video_codec". Lengths below 128 encode as a
// single byte, which covers every option name here.
func hasDexString(dex []byte, s string) bool {
	if len(s) == 0 || len(s) >= 128 {
		panic("hasDexString: length outside the single-byte ULEB128 range")
	}
	needle := make([]byte, 0, len(s)+2)
	needle = append(needle, byte(len(s)))
	needle = append(needle, s...)
	needle = append(needle, 0)
	return bytes.Contains(dex, needle)
}
