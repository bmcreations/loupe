package scrcpy

import (
	"slices"
	"strings"
	"testing"
)

// optionValue pulls one key=value out of the launch options.
func optionValue(t *testing.T, opts []string, key string) string {
	t.Helper()
	for _, o := range opts {
		if k, v, ok := strings.Cut(o, "="); ok && k == key {
			return v
		}
	}
	t.Fatalf("no %q among the launch options: %v", key, opts)
	return ""
}

// TestServerOptionsBoundTheGOP is the regression test for the whole trap
// 15/19/22 family. scrcpy emits no keyframes on a timer -- an idle emulator
// produced exactly one in six seconds, the initial one -- so with an unbounded
// GOP every keyframe latch in loupe is permanent rather than a stall. Asking
// the encoder for one per second turns a freeze into a one-second gap even
// when RequestKeyframe never gets through.
func TestServerOptionsBoundTheGOP(t *testing.T) {
	opts := serverOptions(Config{}.withDefaults(), "deadbeef")
	if got, want := optionValue(t, opts, "video_codec_options"), "i-frame-interval:int=1"; got != want {
		t.Errorf("video_codec_options = %q, want %q", got, want)
	}
}

func TestServerOptionsCarryAnExplicitKeyframeInterval(t *testing.T) {
	opts := serverOptions(Config{KeyframeIntervalSec: 4}.withDefaults(), "deadbeef")
	if got, want := optionValue(t, opts, "video_codec_options"), "i-frame-interval:int=4"; got != want {
		t.Errorf("video_codec_options = %q, want %q", got, want)
	}
}

// TestServerOptionsDefaultToH264 keeps the default on the codec the client is
// known to decode. H265 is selectable but unproven on Safari's WebCodecs here.
func TestServerOptionsDefaultToH264(t *testing.T) {
	if got := optionValue(t, serverOptions(Config{}.withDefaults(), "deadbeef"), "video_codec"); got != "h264" {
		t.Errorf("video_codec = %q, want h264", got)
	}
	opts := serverOptions(Config{VideoCodec: "h265"}.withDefaults(), "deadbeef")
	if got := optionValue(t, opts, "video_codec"); got != "h265" {
		t.Errorf("video_codec = %q, want h265", got)
	}
}

// TestOptionNamesCoversEveryOptionSent guards the indirection itself: the jar
// test asserts OptionNames against the dex, so a name that never reaches
// OptionNames is a name nothing checks.
func TestOptionNamesCoversEveryOptionSent(t *testing.T) {
	names := OptionNames()
	for _, o := range serverOptions(Config{}.withDefaults(), "deadbeef") {
		k, _, _ := strings.Cut(o, "=")
		if !slices.Contains(names, k) {
			t.Errorf("option %q is sent but missing from OptionNames()", k)
		}
	}
}
