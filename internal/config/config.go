// Package config is the persisted server configuration and the rules for
// changing it.
//
// The daemon owns this file and is its only writer. Settings reach it over the
// authed HTTP API rather than by a second process editing the file, so there
// is no two-writer race and one code path serves a local instance and a remote
// one alike.
//
// Every field declares whether it applies live or needs a restart. That is not
// decoration: a settings UI that cannot tell the difference reports success on
// a change the server has not made.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/bmcreations/loupe/internal/scrcpy"
)

// DefaultMaxBufferMs is the jitter-buffer ceiling a client uses when nothing
// overrides it. 50ms absorbs the clumping a LAN produces without being felt on
// a tap; a cellular link needs several times more, which is why this is a
// setting rather than a constant in the client.
const DefaultMaxBufferMs = 50

// DefaultHumanHoldMs is how long a viewer's last input holds a target against
// agents' /api/input. 5s covers the gap between taps of someone using the
// device; someone reading without touching holds nothing, and the viewer's
// banner shows them an agent starting so they can touch to stop it.
const DefaultHumanHoldMs = 5000

// DefaultEmulatorPauseSec is how long a headless emulator goes unused before
// it is paused. Five minutes is longer than the gap between an agent's steps
// and short enough that an emulator left running overnight stops costing CPU.
const DefaultEmulatorPauseSec = 300

// DefaultEmulatorShutdownSec is how long a headless emulator goes unused
// before it is shut down. A paused emulator still holds about 2.9GB, and
// half an hour idle is long past an agent's pause between steps; the next
// open boots from loupe-warm in about 2s.
const DefaultEmulatorShutdownSec = 1800

// Config is the whole persisted configuration. Every field carries a json tag
// that is also its name in the API and in Fields.
//
// Auth is deliberately absent. The token lives in its own file with its own
// permissions, and writing it into a config the app reads back would widen its
// exposure for no gain.
type Config struct {
	MaxSize             int    `json:"maxSize"`
	MaxFPS              int    `json:"maxFps"`
	BitRate             int    `json:"bitRate"`
	VideoCodec          string `json:"videoCodec"`
	KeyframeIntervalSec int    `json:"keyframeIntervalSec"`
	MaxBufferMs         int    `json:"maxBufferMs"`
	HumanHoldMs         int    `json:"humanHoldMs"`
	AdaptiveBitRate     bool   `json:"adaptiveBitRate"`
	EmulatorWindow      bool   `json:"emulatorWindow"`
	EmulatorPauseSec    int    `json:"emulatorPauseSec"`
	EmulatorShutdownSec int    `json:"emulatorShutdownSec"`
	EmulatorKeepState   bool   `json:"emulatorKeepState"`
	SimulatorSlim       bool   `json:"simulatorSlim"`
	Addr                string `json:"addr"`
	TLSCert             string `json:"tlsCert"`
	TLSKey              string `json:"tlsKey"`
	TracePath           string `json:"tracePath"`
}

// Default is the configuration a first run gets. These match the flag defaults
// in cmd/loupe, which remain the fallback when no file exists.
func Default() Config {
	return Config{
		MaxSize:             1080,
		MaxFPS:              60,
		BitRate:             4_000_000,
		VideoCodec:          scrcpy.DefaultVideoCodec,
		KeyframeIntervalSec: 0,
		MaxBufferMs:         DefaultMaxBufferMs,
		HumanHoldMs:         DefaultHumanHoldMs,
		AdaptiveBitRate:     true,
		EmulatorPauseSec:    DefaultEmulatorPauseSec,
		EmulatorShutdownSec: DefaultEmulatorShutdownSec,
		Addr:                ":8080",
	}
}

// Apply says when a change to a field takes effect.
type Apply string

const (
	// AppliesNextSession takes effect when a session next opens, not on the
	// one already running: stream options are snapshotted at acquire and a
	// live viewer keeps what it started with until the session is released.
	// Named for that gap. It was "live", which read as "immediately" and made
	// a UI report success while the person watching saw nothing change.
	AppliesNextSession Apply = "nextSession"
	// AppliesRestart needs the process restarted. Listener and TLS material are
	// bound once at startup.
	AppliesRestart Apply = "restart"
	// AppliesNextBoot takes effect the next time this server boots an AVD.
	// An emulator already running keeps the window it started with: nothing
	// on the console shows a hidden window or opens one, so changing it means
	// shutting the emulator down.
	AppliesNextBoot Apply = "nextBoot"
	// AppliesNow is read each time it is used, so a change reaches what is
	// already running.
	AppliesNow Apply = "now"
)

// Field describes one setting well enough for a UI to render it and for the
// API to explain a rejection.
type Field struct {
	Name    string `json:"name"`
	Applies Apply  `json:"applies"`
	Doc     string `json:"doc"`
	// Flag is the command-line flag that pins this field. A flag given
	// explicitly outranks the file, and the API reports that rather than
	// accepting a write that would never take effect.
	Flag string `json:"flag"`
}

// Fields describes every setting, in the order a settings UI should show them.
// TestFieldsCoverEveryConfigField keeps this in step with the struct.
func Fields() []Field {
	return []Field{
		{"maxSize", AppliesNextSession, "Cap on the longest edge of the video, in pixels. 0 means the device's native resolution.", "max-size"},
		{"maxFps", AppliesNextSession, "Cap on the capture frame rate.", "max-fps"},
		{"bitRate", AppliesNextSession, "Requested video bitrate, in bits per second. Raise it on a LAN, lower it over a relay.", "bitrate"},
		{"videoCodec", AppliesNextSession, "Device-side encoder. Only h264 is known to decode in the browser.", "video-codec"},
		{"keyframeIntervalSec", AppliesNextSession, "Seconds between keyframes, counted at the configured frame rate. Bounds how long a wait-for-keyframe stall can last. 0 uses each target's default: 1 on Android, 5 on a simulator.", "keyframe-interval"},
		{"maxBufferMs", AppliesNextSession, "Ceiling on the client's jitter buffer, in milliseconds. It absorbs clumped arrivals at the cost of that much added latency on every tap. Raise it on a cellular link, lower it on a LAN. 0 draws on arrival.", "max-buffer"},
		{"humanHoldMs", AppliesNextSession, "How long a viewer's last touch, key or scroll keeps agents' /api/input off the device, in milliseconds. A finger still down holds it regardless. An agent refused meanwhile gets 409 humanHeld with the time left.", "human-hold"},
		{"adaptiveBitRate", AppliesNextSession, "Relaunch the encoder at a lower bitrate when the link cannot carry what it is producing, and probe back up once it can. bitRate becomes a ceiling rather than a fixed rate.", "adaptive-bitrate"},
		{"emulatorWindow", AppliesNextBoot, "Open the emulator's own window when this server boots an AVD. Off hides the window and turns off audio, which the stream does not need. An emulator already running keeps what it started with.", "emulator-window"},
		{"emulatorPauseSec", AppliesNow, "Pause a headless emulator that no viewer, recording, log follow or agent call has used for this many seconds, and resume it on the next one. A paused emulator uses about 1% CPU. 0 never pauses. An emulator with its window open is never paused.", "emulator-pause"},
		{"emulatorShutdownSec", AppliesNow, "Shut down a headless emulator that nothing has used for this many seconds, which frees its memory. The next open boots it from its snapshot, so whatever changed since it booted, an installed APK included, is gone unless emulatorKeepState is on. 0 never shuts one down. An emulator with its window open is never shut down.", "emulator-shutdown"},
		{"emulatorKeepState", AppliesNow, "Save an emulator's state into its snapshot before shutting it down, idle or from the picker, so the next open resumes where it left off, installed apps included. Off, every boot returns to the snapshot taken after its first boot. A save takes a second or two and the snapshot is about 2 GB either way. Reset discards a kept state.", "emulator-keep-state"},
		{"simulatorSlim", AppliesNow, "Boot simulators with 168 background daemons disabled, which cut one iPhone 17 Pro simulator from 8.9 GB to 2.8 GB and its first minutes of idle CPU from up to 160% of a core to 2%. Stream frame rate did not change. It takes effect on the next boot loupe performs; a simulator already booted, or booted from Xcode, keeps what it has. iOS 18.5 and newer only. Turning it off restores only the daemons loupe disabled.", "simulator-slim"},
		{"addr", AppliesRestart, "Listen address.", "addr"},
		{"tlsCert", AppliesRestart, "TLS certificate. Serves HTTPS directly rather than behind a proxy.", "tls-cert"},
		{"tlsKey", AppliesRestart, "TLS private key. Required with tlsCert.", "tls-key"},
		{"tracePath", AppliesRestart, "Where to write the per-second NDJSON metrics trace. Empty disables it.", "trace"},
	}
}

// codecs are the encoders scrcpy accepts. h265 and av1 reach the device and
// stream, but the client cannot yet build a codec string for them, so they are
// experiments rather than working choices.
var codecs = map[string]bool{"h264": true, "h265": true, "av1": true}

// Validate reports the first thing wrong with c.
func (c Config) Validate() error {
	if c.MaxSize != 0 && (c.MaxSize < 100 || c.MaxSize > 4096) {
		return fmt.Errorf("maxSize %d: want 0 for native, or 100 to 4096", c.MaxSize)
	}
	if c.MaxFPS < 1 || c.MaxFPS > 240 {
		return fmt.Errorf("maxFps %d: want 1 to 240", c.MaxFPS)
	}
	if c.BitRate < 100_000 || c.BitRate > 100_000_000 {
		return fmt.Errorf("bitRate %d: want 100000 to 100000000", c.BitRate)
	}
	if !codecs[c.VideoCodec] {
		return fmt.Errorf("videoCodec %q: want h264, h265 or av1", c.VideoCodec)
	}
	if c.KeyframeIntervalSec < 0 || c.KeyframeIntervalSec > 60 {
		return fmt.Errorf("keyframeIntervalSec %d: want 0 to 60", c.KeyframeIntervalSec)
	}
	if c.MaxBufferMs < 0 || c.MaxBufferMs > 1000 {
		return fmt.Errorf("maxBufferMs %d: want 0 to 1000", c.MaxBufferMs)
	}
	if c.HumanHoldMs < 500 || c.HumanHoldMs > 120_000 {
		return fmt.Errorf("humanHoldMs %d: want 500 to 120000", c.HumanHoldMs)
	}
	if c.EmulatorPauseSec != 0 && (c.EmulatorPauseSec < 30 || c.EmulatorPauseSec > 86_400) {
		return fmt.Errorf("emulatorPauseSec %d: want 0 for never, or 30 to 86400", c.EmulatorPauseSec)
	}
	if c.EmulatorShutdownSec != 0 && (c.EmulatorShutdownSec < 30 || c.EmulatorShutdownSec > 86_400) {
		return fmt.Errorf("emulatorShutdownSec %d: want 0 for never, or 30 to 86400", c.EmulatorShutdownSec)
	}
	if c.Addr == "" {
		return fmt.Errorf("addr: must not be empty")
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return fmt.Errorf("tlsCert and tlsKey: set both or neither")
	}
	return nil
}

// Load reads the config at path. A missing file is not an error: it reports
// defaults and existed=false, which is what a first run sees.
//
// Fields the file omits keep their default rather than their zero value, so a
// file written by an older build does not silently reset everything added
// since.
func Load(path string) (cfg Config, existed bool, err error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Default(), false, nil
	}
	if err != nil {
		return Default(), false, fmt.Errorf("read %s: %w", path, err)
	}
	c := Default()
	if err := json.Unmarshal(b, &c); err != nil {
		return Default(), true, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, true, nil
}

// Save writes c to path atomically, creating the directory if needed.
//
// It validates first. Writing a config the server would refuse to start on
// turns a rejected API call into a broken restart, and the process that does
// that restart is a supervisor with nobody watching.
func Save(path string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename has taken it

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("rename onto %s: %w", path, err)
	}
	return nil
}

// Merge applies a partial JSON object to base and returns the result along with
// the names of the fields it actually changed.
//
// Unknown keys are an error rather than a silent no-op. A misspelled setting
// that returns 200 is the failure this whole design exists to avoid, and a
// typo in a hand-written request looks exactly like one.
func Merge(base Config, patch []byte) (Config, []string, error) {
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(patch))
	if err := dec.Decode(&raw); err != nil {
		return base, nil, fmt.Errorf("parse body: %w", err)
	}

	known := map[string]bool{}
	for _, f := range Fields() {
		known[f.Name] = true
	}
	var unknown []string
	for k := range raw {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return base, nil, fmt.Errorf("unknown settings: %v", unknown)
	}

	// Re-marshal base and overlay the patch, so each key is decoded by the
	// struct's own tags and types rather than by hand.
	cur, err := json.Marshal(base)
	if err != nil {
		return base, nil, err
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(cur, &merged); err != nil {
		return base, nil, err
	}
	for k, v := range raw {
		merged[k] = v
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return base, nil, err
	}
	next := Default()
	if err := json.Unmarshal(out, &next); err != nil {
		return base, nil, fmt.Errorf("apply settings: %w", err)
	}
	if err := next.Validate(); err != nil {
		return base, nil, err
	}
	return next, changedFields(base, next), nil
}

// changedFields names the settings that differ between two configs, by their
// API names.
func changedFields(a, b Config) []string {
	if a == b {
		return nil
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	var ma, mb map[string]json.RawMessage
	_ = json.Unmarshal(ja, &ma)
	_ = json.Unmarshal(jb, &mb)

	var out []string
	for _, f := range Fields() {
		if !bytes.Equal(ma[f.Name], mb[f.Name]) {
			out = append(out, f.Name)
		}
	}
	return out
}

// DefaultPath is where the config lives when nothing overrides it.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config dir: %w", err)
	}
	return filepath.Join(dir, "loupe", "config.json"), nil
}

// Values renders the configuration keyed by the same names Fields uses, so a
// caller can pair a value with its metadata without a switch that has to be
// kept in step with the struct.
//
// It round-trips through JSON deliberately: the struct tags are already the
// single source of truth for those names, and a hand-written map would be a
// second one that drifts the first time a field is added.
func (c Config) Values() map[string]any {
	b, err := json.Marshal(c)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{}
	}
	return m
}

// FieldForFlag maps a command-line flag name to the config field it pins.
func FieldForFlag(flag string) (string, bool) {
	for _, f := range Fields() {
		if f.Flag == flag {
			return f.Name, true
		}
	}
	return "", false
}
