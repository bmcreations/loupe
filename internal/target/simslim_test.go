package target

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stockStore is the shape launchd_sim writes for itself: explicit false
// entries, which a rewrite has to keep.
const stockStore = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>com.apple.nanonewscd</key>
	<false/>
	<key>com.apple.example.userset</key>
	<true/>
</dict>
</plist>
`

func useSlimRoot(t *testing.T) string {
	t.Helper()
	was := simSlimRoot
	simSlimRoot = t.TempDir()
	t.Cleanup(func() { simSlimRoot = was })
	return simSlimRoot
}

func writeStore(t *testing.T, udid, body string) string {
	t.Helper()
	dir := simSlimDir(udid)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "disabled.plist")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func readStore(t *testing.T, path string) map[string]bool {
	t.Helper()
	got, err := readSlimStore(path)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestSlimStoreRoundTrips(t *testing.T) {
	entries, err := parseSlimStore(strings.NewReader(stockStore))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries["com.apple.nanonewscd"] || !entries["com.apple.example.userset"] {
		t.Fatalf("parsed %v", entries)
	}
	enc := encodeSlimStore(entries)
	again, err := parseSlimStore(bytes.NewReader(enc))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encodeSlimStore(again), enc) {
		t.Fatalf("re-encoding changed the bytes:\n%s", enc)
	}
}

func TestSlimPersistsFromIOS185(t *testing.T) {
	for v, want := range map[string]bool{
		"17.5": false, "18.3": false, "18.5": true, "18.6.1": true, "26.0": true, "27.0": true,
		"": false, "x.y": false, "27": false,
	} {
		if got := slimPersists(v); got != want {
			t.Errorf("slimPersists(%q) = %v, want %v", v, got, want)
		}
	}
}

// With slim off loupe must not create or rewrite a store it never slimmed.
func TestSetSlimOffWritesNothing(t *testing.T) {
	useSlimRoot(t)
	if got, err := setSlim("NEVER", false); err != nil || got != nil {
		t.Fatalf("setSlim off = %v, %v", got, err)
	}
	if _, err := os.Stat(simSlimDir("NEVER")); !os.IsNotExist(err) {
		t.Fatalf("store dir created: %v", err)
	}

	p := writeStore(t, "STOCK", stockStore)
	if _, err := setSlim("STOCK", false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != stockStore {
		t.Fatalf("stock store rewritten:\n%s", b)
	}
}

func TestSetSlimOnThenOffRestoresOnlyItsOwn(t *testing.T) {
	useSlimRoot(t)
	p := writeStore(t, "U", stockStore)

	wanted, err := setSlim("U", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(wanted) != len(simSlimLabels) {
		t.Fatalf("wanted %d labels, want %d", len(wanted), len(simSlimLabels))
	}
	on := readStore(t, p)
	for _, l := range simSlimLabels {
		if !on[l] {
			t.Fatalf("%s not disabled", l)
		}
	}
	if !on["com.apple.example.userset"] {
		t.Fatal("lost an override loupe did not set")
	}

	if wanted, err := setSlim("U", false); err != nil || wanted != nil {
		t.Fatalf("setSlim off = %v, %v", wanted, err)
	}
	off := readStore(t, p)
	for _, l := range simSlimLabels {
		if off[l] {
			t.Fatalf("%s still disabled", l)
		}
	}
	if !off["com.apple.example.userset"] {
		t.Fatal("turning slim off enabled an override loupe did not set")
	}
	if _, err := os.Stat(filepath.Join(simSlimDir("U"), simSlimMarker)); !os.IsNotExist(err) {
		t.Fatalf("marker left behind: %v", err)
	}
}

// A label someone else had already disabled stays disabled when slim goes off.
func TestSetSlimOffKeepsPreexistingDisables(t *testing.T) {
	useSlimRoot(t)
	pre := simSlimLabels[0]
	p := writeStore(t, "P", strings.Replace(stockStore, "com.apple.example.userset", pre, 1))
	if _, err := setSlim("P", true); err != nil {
		t.Fatal(err)
	}
	if _, err := setSlim("P", false); err != nil {
		t.Fatal(err)
	}
	if !readStore(t, p)[pre] {
		t.Fatalf("%s was disabled before loupe and is now enabled", pre)
	}
}

func TestParseDisabled(t *testing.T) {
	out := "disabled services = {\n\t\"com.apple.a\" => disabled\n\t\"com.apple.b\" => enabled\n}\n"
	got := parseDisabled(out)
	if !got["com.apple.a"] || got["com.apple.b"] || len(got) != 1 {
		t.Fatalf("parseDisabled = %v", got)
	}
}
