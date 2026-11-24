package android

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/target/ios"

	"github.com/bmcreations/loupe/internal/target"
)

func TestADBLogcatRefusesAFormat(t *testing.T) {
	tg := &Device{info: DeviceInfo{Serial: "s", State: "device"}}
	_, err := tg.Logcat(context.Background(), target.LogcatQuery{Format: ios.LogFormatNDJSON})
	if !errors.Is(err, ios.ErrLogQueryUnsupported) {
		t.Errorf("err = %v; want ErrLogQueryUnsupported", err)
	}
}

func TestParseThreadtime(t *testing.T) {
	for _, c := range []struct {
		line string
		tags []string
		msg  string
	}{
		{"11-24 07:50:01.123  1234  1250 I ReactNativeJS: 'porchlight-js', 'sent'", []string{"ReactNativeJS"}, "'porchlight-js', 'sent'"},
		// threadtime pads a short tag to eight columns before the colon.
		{"11-24 07:50:01.123  1234  1250 I flutter : porchlight: tapped", []string{"flutter"}, "porchlight: tapped"},
		{"11-24 07:50:01.123   812   812 W ActivityManager: Slow operation: 52ms", []string{"ActivityManager"}, "Slow operation: 52ms"},
		{"11-24 07:50:01.123   812   812 D tag     : ", []string{"tag"}, ""},
		{"--------- beginning of main", nil, ""},
		{"", nil, ""},
	} {
		got := parseThreadtime([]byte(c.line))
		if !reflect.DeepEqual(got.Tags, c.tags) || string(got.Message) != c.msg || got.Continues {
			t.Errorf("parseThreadtime(%q) = %q %q %v; want %q %q", c.line, got.Tags, got.Message, got.Continues, c.tags, c.msg)
		}
	}
}

const threadtimeSample = "--------- beginning of main\n" +
	"11-24 07:50:01.100  4242  4242 I ReactNativeJS: 'porchlight-js', 'sent'\n" +
	"11-24 07:50:01.200  4242  4260 I flutter : porchlight tapped\n" +
	"11-24 07:50:01.300  4242  4242 I ReactNativeJS: 'other'\n" +
	"11-24 07:50:01.400   812   812 I ActivityManager: PORCHLIGHT-JS started\n"

// A filter is applied to what logcat prints, so the request's tag and text
// never reach the device's shell, and a dump reads the whole buffer (-d)
// rather than the last lines of it, which could hold no match.
func TestLogcatTagAndTextFilterOnTheHost(t *testing.T) {
	adb, argv := fakeADB(t, "cat <<'EOF'\n"+threadtimeSample+"EOF")
	tgt := NewDevice(ADB{Path: adb}, DeviceInfo{Serial: "s", State: "device"}, "", "")
	for _, c := range []struct {
		q    target.LogcatQuery
		want string
	}{
		{target.LogcatQuery{Tag: "reactnativejs", Lines: 1}, "'porchlight-js', 'sent'|'other'"},
		{target.LogcatQuery{Tag: "flutter", Lines: 1}, "porchlight tapped"},
		{target.LogcatQuery{Text: "Porchlight-JS", Lines: 1}, "'porchlight-js', 'sent'|PORCHLIGHT-JS started"},
		{target.LogcatQuery{Tag: "ReactNativeJS", Text: "other", Lines: 1}, "'other'"},
	} {
		lc, err := tgt.Logcat(context.Background(), c.q)
		if err != nil {
			t.Fatalf("%+v: %v", c.q, err)
		}
		body, _ := io.ReadAll(lc)
		lc.Close()
		var msgs []string
		for _, l := range strings.Split(strings.TrimSpace(string(body)), "\n") {
			msgs = append(msgs, string(parseThreadtime([]byte(l)).Message))
		}
		if got := strings.Join(msgs, "|"); got != c.want {
			t.Errorf("%+v: kept %q; want %q", c.q, got, c.want)
		}
	}
	for _, a := range readArgv(t, argv) {
		if a != "-s s exec-out logcat -v threadtime -d" {
			t.Errorf("adb argv %q; want a whole-buffer dump with nothing from the request", a)
		}
	}
}
