package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsAreUsable(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatalf("default config does not validate: %v", err)
	}
	if c.MaxFPS != 60 || c.VideoCodec != "h264" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	c, existed, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if existed {
		t.Fatal("reported a file that is not there")
	}
	if c != Default() {
		t.Fatalf("missing file should give defaults, got %+v", c)
	}
}

// A config written by an older build must not reset every field it omits.
// Round-tripping through Default() is what makes a partial file safe.
func TestLoadFillsOmittedFieldsFromDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"maxFps":30}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, existed, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !existed {
		t.Fatal("existing file reported as missing")
	}
	if c.MaxFPS != 30 {
		t.Fatalf("MaxFPS = %d, want 30", c.MaxFPS)
	}
	if c.BitRate != Default().BitRate || c.VideoCodec != Default().VideoCodec {
		t.Fatalf("omitted fields were not defaulted: %+v", c)
	}
}

func TestSaveCreatesTheDirectoryAndRoundTrips(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nested", "deeper", "config.json")
	c := Default()
	c.MaxFPS = 24
	c.Addr = ":19000"
	if err := Save(p, c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, existed, err := Load(p)
	if err != nil || !existed {
		t.Fatalf("Load after Save: %v existed=%v", err, existed)
	}
	if got != c {
		t.Fatalf("round trip lost data:\n got %+v\nwant %+v", got, c)
	}
}

// The daemon is the only writer, but it can be killed mid-write. A reader must
// never see a half-written file, which means the last step has to be a rename
// onto the path rather than a write into it. A rename swaps the inode; an
// in-place write keeps it, so the inode is what tells the two apart.
func TestSaveReplacesTheFileByRenameRatherThanWritingInPlace(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := Save(p, Default()); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}

	c := Default()
	c.MaxFPS = 30
	if err := Save(p, c); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("Save wrote in place; a crash mid-write would leave a partial config")
	}

	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("Save left temporary files behind: %v", names)
	}
}

func TestSaveRefusesAnInvalidConfig(t *testing.T) {
	c := Default()
	c.MaxFPS = 9999
	err := Save(filepath.Join(t.TempDir(), "config.json"), c)
	if err == nil {
		t.Fatal("Save accepted an invalid config")
	}
}

func TestValidateRejectsOutOfRange(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"fps too high":      func(c *Config) { c.MaxFPS = 500 },
		"fps zero":          func(c *Config) { c.MaxFPS = 0 },
		"negative size":     func(c *Config) { c.MaxSize = -1 },
		"tiny size":         func(c *Config) { c.MaxSize = 10 },
		"bitrate zero":      func(c *Config) { c.BitRate = 0 },
		"unknown codec":     func(c *Config) { c.VideoCodec = "vp9" },
		"keyframe negative": func(c *Config) { c.KeyframeIntervalSec = -1 },
		"keyframe too long": func(c *Config) { c.KeyframeIntervalSec = 600 },
		"empty addr":        func(c *Config) { c.Addr = "" },
	} {
		c := Default()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, c)
		}
	}
}

// MaxSize 0 means the target's native resolution and must stay legal.
func TestValidateAllowsZeroMaxSize(t *testing.T) {
	c := Default()
	c.MaxSize = 0
	if err := c.Validate(); err != nil {
		t.Fatalf("MaxSize 0 should mean native resolution: %v", err)
	}
}

// Every field has to be reachable by name, or the API cannot report which
// setting a caller got wrong and the app cannot render a form.
func TestFieldsCoverEveryConfigField(t *testing.T) {
	b, err := json.Marshal(Default())
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	described := map[string]bool{}
	for _, f := range Fields() {
		described[f.Name] = true
		if f.Applies != AppliesNextSession && f.Applies != AppliesRestart && f.Applies != AppliesNextBoot && f.Applies != AppliesNow {
			t.Errorf("field %q has no apply class", f.Name)
		}
		if strings.TrimSpace(f.Doc) == "" {
			t.Errorf("field %q has no doc string", f.Name)
		}
	}
	for name := range raw {
		if !described[name] {
			t.Errorf("config field %q is not described by Fields()", name)
		}
	}
	for name := range described {
		if _, ok := raw[name]; !ok {
			t.Errorf("Fields() describes %q, which is not a config field", name)
		}
	}
}

// A setting that needs a restart and one that does not must be distinguishable,
// or the app reports success on a change that did not take.
func TestApplyClassesAreWhatTheServerCanActuallyDo(t *testing.T) {
	want := map[string]Apply{
		"maxSize":             AppliesNextSession,
		"maxFps":              AppliesNextSession,
		"bitRate":             AppliesNextSession,
		"videoCodec":          AppliesNextSession,
		"keyframeIntervalSec": AppliesNextSession,
		"addr":                AppliesRestart,
		"tlsCert":             AppliesRestart,
		"tlsKey":              AppliesRestart,
		"tracePath":           AppliesRestart,
		"simulatorSlim":       AppliesNow,
		"sharedClipboard":     AppliesNow,
	}
	for _, f := range Fields() {
		if w, ok := want[f.Name]; ok && f.Applies != w {
			t.Errorf("field %q applies %q, want %q", f.Name, f.Applies, w)
		}
	}
}

// The Mac's pasteboard can hold passwords, so nothing shares it until a
// person turns it on.
func TestSharedClipboardIsOffByDefault(t *testing.T) {
	if Default().SharedClipboard {
		t.Fatal("sharedClipboard is on by default")
	}
}

func TestMergeAppliesOnlyPresentKeys(t *testing.T) {
	base := Default()
	base.MaxFPS = 60
	base.BitRate = 4_000_000

	got, changed, err := Merge(base, []byte(`{"maxFps":30}`))
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if got.MaxFPS != 30 {
		t.Fatalf("MaxFPS = %d, want 30", got.MaxFPS)
	}
	if got.BitRate != base.BitRate {
		t.Fatalf("Merge clobbered an absent key: BitRate = %d", got.BitRate)
	}
	if len(changed) != 1 || changed[0] != "maxFps" {
		t.Fatalf("changed = %v, want [maxFps]", changed)
	}
}

func TestMergeReportsNoChangeWhenTheValueIsTheSame(t *testing.T) {
	base := Default()
	_, changed, err := Merge(base, []byte(`{"maxFps":60}`))
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(changed) != 0 {
		t.Fatalf("changed = %v, want none", changed)
	}
}

func TestMergeRejectsUnknownKeys(t *testing.T) {
	_, _, err := Merge(Default(), []byte(`{"maxFsp":30}`))
	if err == nil {
		t.Fatal("Merge accepted a misspelled key")
	}
	if !strings.Contains(err.Error(), "maxFsp") {
		t.Fatalf("error should name the bad key, got %v", err)
	}
}

func TestMergeValidates(t *testing.T) {
	if _, _, err := Merge(Default(), []byte(`{"maxFps":9999}`)); err == nil {
		t.Fatal("Merge accepted an out-of-range value")
	}
}

// The Kotlin client will hardcode these strings, so a rename here is a
// silent break there. "live" was the original value and was wrong: a change
// to a stream option does not reach a session that is already open, so a UI
// that trusted it reported success while the viewer saw no change.
func TestApplyValuesAreStable(t *testing.T) {
	if got := string(AppliesNextSession); got != "nextSession" {
		t.Errorf("AppliesNextSession = %q, want %q", got, "nextSession")
	}
	if got := string(AppliesRestart); got != "restart" {
		t.Errorf("AppliesRestart = %q, want %q", got, "restart")
	}
	if got := string(AppliesNextBoot); got != "nextBoot" {
		t.Errorf("AppliesNextBoot = %q, want %q", got, "nextBoot")
	}
	if got := string(AppliesNow); got != "now" {
		t.Errorf("AppliesNow = %q, want %q", got, "now")
	}
}

// The buffer ceiling is the one setting that trades latency for smoothness, so
// both ends of its range have to be reachable and the nonsense beyond them has
// to be refused. Zero is legal and means draw on arrival.
func TestMaxBufferMsIsValidated(t *testing.T) {
	for _, tc := range []struct {
		ms   int
		want bool
	}{
		{0, true}, {50, true}, {250, true}, {1000, true},
		{-1, false}, {1001, false},
	} {
		c := Default()
		c.MaxBufferMs = tc.ms
		err := c.Validate()
		if got := err == nil; got != tc.want {
			t.Errorf("MaxBufferMs %d: accepted=%v want %v (err=%v)", tc.ms, got, tc.want, err)
		}
	}
}

// A field missing from Fields() is invisible to the settings UI, which is the
// whole reason the table exists.
func TestMaxBufferMsIsListed(t *testing.T) {
	var found bool
	for _, f := range Fields() {
		if f.Name == "maxBufferMs" {
			found = true
			if f.Flag == "" {
				t.Error("maxBufferMs has no flag name")
			}
		}
	}
	if !found {
		t.Fatal("maxBufferMs missing from Fields()")
	}
	if _, ok := Default().Values()["maxBufferMs"]; !ok {
		t.Error("maxBufferMs missing from Values()")
	}
}

func TestHumanHoldMsIsValidated(t *testing.T) {
	for _, tc := range []struct {
		ms   int
		want bool
	}{{499, false}, {500, true}, {DefaultHumanHoldMs, true}, {120_000, true}, {120_001, false}} {
		c := Default()
		c.HumanHoldMs = tc.ms
		if err := c.Validate(); (err == nil) != tc.want {
			t.Errorf("HumanHoldMs %d: accepted=%v want %v (err=%v)", tc.ms, err == nil, tc.want, err)
		}
	}
}
