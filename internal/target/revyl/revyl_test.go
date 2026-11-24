package revyl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// fakeRevyl is a revyl stand-in: a shell script that logs its argv and
// prints testdata/revyl/<cmd>-<sub>.json. The JSON is shaped from revyl-cli
// v0.2.27's structs (see revyl.go for the file paths), not recorded from a
// live account. A <cmd>-<sub>.json.first file is printed once, before the
// plain one, so a test can show a session changing between two reads.
type fakeRevyl struct {
	t   *testing.T
	dir string
	cli CLI
}

const fakeRevylScript = `#!/bin/sh
dir="$(dirname "$0")"
# Everything else is written before argv.log, which tests poll.
env | grep -E '^(REVYL_|NO_COLOR)' | sort > "$dir/env.log"
if [ -n "${LOUPE_REVYL_TEXT-}" ]; then printf '%s\n' "$LOUPE_REVYL_TEXT" >> "$dir/text.log"; fi
printf '%s\037' "$@" >> "$dir/argv.log"
printf '\n' >> "$dir/argv.log"
[ -f "$dir/slow" ] && [ "$1 $2" = "device tap" ] && sleep "$(cat "$dir/slow")"
f="$dir/$1-$2.json"
if [ -f "$dir/$1-$2.fail" ]; then cat "$dir/$1-$2.fail" >&2; exit 1; fi
if [ -f "$f.first" ]; then cat "$f.first"; rm "$f.first"; exit 0; fi
[ -f "$f" ] && cat "$f"
exit 0
`

func newFakeRevyl(t *testing.T) *fakeRevyl {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "revyl")
	if err := os.WriteFile(bin, []byte(fakeRevylScript), 0o755); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	return &fakeRevyl{t: t, dir: dir, cli: CLI{Path: bin, Dir: work}}
}

// respond makes `revyl <cmd> <sub>` print testdata/revyl/<fixture>.json,
// with {{WHEP}} replaced.
func (f *fakeRevyl) respond(cmdSub, fixture, whep string) {
	f.t.Helper()
	f.respondAs(cmdSub+".json", fixture, whep)
}

func (f *fakeRevyl) respondAs(name, fixture, whep string) {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "revyl", fixture+".json"))
	if err != nil {
		f.t.Fatal(err)
	}
	b = []byte(strings.ReplaceAll(string(b), "{{WHEP}}", whep))
	if err := os.WriteFile(filepath.Join(f.dir, name), b, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// calls returns each invocation's argv.
func (f *fakeRevyl) calls() [][]string {
	b, _ := os.ReadFile(filepath.Join(f.dir, "argv.log"))
	var out [][]string
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		out = append(out, strings.Split(strings.TrimSuffix(line, "\x1f"), "\x1f"))
	}
	return out
}

// waitCalls waits for n invocations, since input runs on a worker.
func (f *fakeRevyl) waitCalls(n int) [][]string {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		c := f.calls()
		if len(c) >= n {
			return c
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("got %d revyl calls, want %d: %q", len(c), n, c)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *fakeRevyl) file(name string) string {
	b, _ := os.ReadFile(filepath.Join(f.dir, name))
	return string(b)
}

const testSession = "5b0c9f7e-2d55-4c4b-9a43-0d6f3f1f2a10"

func TestRevylCLIReadsTheSourcesJSONShapes(t *testing.T) {
	f := newFakeRevyl(t)
	ctx := context.Background()

	f.respond("device-start", "device-start", "https://whep.revyl.invalid/s/abc")
	s, err := f.cli.Start(ctx, Start{Platform: "android", Model: "Pixel 7", OSVersion: "Android 14", IdleTimeout: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if s.SessionID != testSession || s.Platform != "android" || s.ScreenWidth != 720 || s.ScreenHeight != 404 ||
		!s.Ready() || s.IdleTimeout != 15*time.Minute {
		t.Fatalf("Start = %+v", s)
	}
	want := []string{"device", "start", "--platform", "android", "--json", "--open=false", "--computer", "none",
		"--device-model", "Pixel 7", "--os-version", "Android 14", "--timeout", "600"}
	if got := f.calls()[0]; !reflect.DeepEqual(got, want) {
		t.Fatalf("start argv = %q, want %q", got, want)
	}

	f.respond("device-info", "device-info-starting", "")
	s, ok, err := f.cli.Info(ctx, testSession)
	if err != nil || !ok || s.Ready() {
		t.Fatalf("Info(starting) = %+v ok %v err %v, want a session not ready yet", s, ok, err)
	}
	f.respond("device-info", "device-info-ended", "")
	if _, ok, err := f.cli.Info(ctx, testSession); err != nil || ok {
		t.Fatalf("Info(ended) ok %v err %v, want not ok", ok, err)
	}

	f.respond("device-targets", "device-targets", "")
	tg, err := f.cli.Targets(ctx)
	if err != nil || len(tg["android"]) != 2 || tg["android"][0] != (DeviceTarget{Model: "Pixel 7", Runtime: "Android 14"}) ||
		tg["ios"][0].Model != "iPhone 16" {
		t.Fatalf("Targets = %+v err %v", tg, err)
	}

	f.respond("device-list", "device-list", "")
	ls, err := f.cli.List(ctx)
	if err != nil || len(ls) != 2 || ls[1].Status != "queued" || ls[1].Platform != "ios" {
		t.Fatalf("List = %+v err %v", ls, err)
	}

	f.respond("device-stop", "device-stop", "")
	if err := f.cli.Stop(ctx, testSession); err != nil {
		t.Fatal(err)
	}

	f.respond("auth-status", "auth-status", "")
	a, err := f.cli.AuthStatus(ctx)
	if err != nil || !a.Authenticated || a.Email != "dev@example.invalid" || a.AuthMethod != "browser" {
		t.Fatalf("AuthStatus = %+v err %v", a, err)
	}
	f.respond("auth-status", "auth-status-signed-out", "")
	if a, err := f.cli.AuthStatus(ctx); err != nil || a.Authenticated {
		t.Fatalf("AuthStatus(signed out) = %+v err %v", a, err)
	}

	// The CLI's analytics and update check are off for every run.
	env := f.file("env.log")
	for _, kv := range []string{"REVYL_TELEMETRY_DISABLED=1", "REVYL_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1"} {
		if !strings.Contains(env, kv) {
			t.Fatalf("env %q lacks %s", env, kv)
		}
	}

	// A failure names the command and the CLI's message, not the session ID.
	if err := os.WriteFile(filepath.Join(f.dir, "device-stop.fail"), []byte("Error: session not found\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = f.cli.Stop(ctx, testSession)
	if err == nil || err.Error() != "revyl device stop: Error: session not found" {
		t.Fatalf("Stop error = %v", err)
	}
}

// fixedClock steps by the given durations, one per call.
type fixedClock struct{ t time.Time }

func (c *fixedClock) now() time.Time { return c.t }

func gestures(t *testing.T, f *fakeRevyl, platform string, screenW, screenH int) (*revylInput, *fixedClock) {
	t.Helper()
	in := newRevylInput(f.cli, nil, Session{SessionID: testSession, Platform: platform, ScreenWidth: screenW, ScreenHeight: screenH}, nil)
	t.Cleanup(in.close)
	clk := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	in.now = clk.now
	return in, clk
}

func press(t *testing.T, in *revylInput, m target.Meta, clk *fixedClock, held time.Duration, pts ...[2]int32) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(in.touch(target.Touch{Action: target.TouchDown, PointerID: 1, X: pts[0][0], Y: pts[0][1], Pressure: 1}, m))
	for _, p := range pts[1:] {
		must(in.touch(target.Touch{Action: target.TouchMove, PointerID: 1, X: p[0], Y: p[1], Pressure: 1}, m))
	}
	clk.t = clk.t.Add(held)
	last := pts[len(pts)-1]
	must(in.touch(target.Touch{Action: target.TouchUp, PointerID: 1, X: last[0], Y: last[1]}, m))
}

func argv(args ...string) []string { return append(args, "--session-id", testSession) }

func TestRevylGesturesInAndroidPixels(t *testing.T) {
	f := newFakeRevyl(t)
	in, clk := gestures(t, f, "android", 1080, 2400)
	// The stream is half the screen's size, as when MaxSize scaled it.
	m := target.Meta{Width: 540, Height: 1200}

	press(t, in, m, clk, 80*time.Millisecond, [2]int32{270, 600}, [2]int32{272, 601})
	press(t, in, m, clk, 300*time.Millisecond, [2]int32{100, 1000}, [2]int32{100, 700}, [2]int32{100, 200})
	press(t, in, m, clk, 900*time.Millisecond, [2]int32{50, 60})
	// A second finger while the first is down is ignored.
	_ = in.touch(target.Touch{Action: target.TouchDown, PointerID: 1, X: 10, Y: 10}, m)
	_ = in.touch(target.Touch{Action: target.TouchDown, PointerID: 2, X: 500, Y: 500}, m)
	_ = in.touch(target.Touch{Action: target.TouchUp, PointerID: 2, X: 500, Y: 500}, m)
	_ = in.touch(target.Touch{Action: target.TouchCancel, PointerID: 1}, m)

	for _, k := range []int32{target.KeycodeHome, target.KeycodeBack, keycodeEnter, keycodeDel} {
		if err := in.key(target.Key{Action: target.KeyDown, Keycode: k}); err != nil {
			t.Fatalf("key %d: %v", k, err)
		}
		if err := in.key(target.Key{Action: target.KeyUp, Keycode: k}); err != nil {
			t.Fatalf("key up %d: %v", k, err)
		}
	}
	if err := in.key(target.Key{Action: target.KeyDown, Keycode: target.KeycodePower}); !errors.Is(err, target.ErrNoControl) {
		t.Fatalf("power key = %v, want ErrNoControl", err)
	}
	if err := in.text("hello wörld"); err != nil {
		t.Fatal(err)
	}

	got := f.waitCalls(8)
	want := [][]string{
		argv("device", "tap", "--x", "540", "--y", "1200"),
		argv("device", "drag", "--start-x", "200", "--start-y", "2000", "--end-x", "200", "--end-y", "400"),
		argv("device", "long-press", "--x", "100", "--y", "120", "--duration", "900"),
		argv("device", "home"),
		argv("device", "back"),
		argv("device", "key", "--key", "ENTER"),
		argv("device", "key", "--key", "BACKSPACE"),
		// Typing goes where the last tap or long-press landed.
		argv("device", "type", "--text-env", "LOUPE_REVYL_TEXT", "--clear-first=false", "--x", "100", "--y", "120"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv:\n got %q\nwant %q", got, want)
	}
	// The text went through the environment, never argv.
	if f.file("text.log") != "hello wörld\n" {
		t.Fatalf("typed text = %q", f.file("text.log"))
	}
	for _, c := range got {
		if strings.Contains(strings.Join(c, " "), "hello") {
			t.Fatalf("text in argv: %q", c)
		}
	}
	if err := in.err(); err != nil {
		t.Fatalf("last gesture error: %v", err)
	}
}

func TestRevylGesturesInIOSPoints(t *testing.T) {
	f := newFakeRevyl(t)
	// iPhone 16 Pro: 393x852 points, streamed at its 1179x2556 pixels (3x).
	in, clk := gestures(t, f, "ios", 393, 852)
	m := target.Meta{Width: 1179, Height: 2556}

	press(t, in, m, clk, 50*time.Millisecond, [2]int32{589, 1278})
	press(t, in, m, clk, 200*time.Millisecond, [2]int32{589, 2400}, [2]int32{589, 1200})
	if err := in.key(target.Key{Action: target.KeyDown, Keycode: target.KeycodeBack}); !errors.Is(err, target.ErrNoControl) {
		t.Fatalf("iOS back = %v, want ErrNoControl (back is Android's)", err)
	}
	if err := in.key(target.Key{Action: target.KeyDown, Keycode: target.KeycodeHome}); err != nil {
		t.Fatal(err)
	}
	got := f.waitCalls(3)
	want := [][]string{
		argv("device", "tap", "--x", "196", "--y", "426"),
		argv("device", "drag", "--start-x", "196", "--start-y", "800", "--end-x", "196", "--end-y", "400"),
		argv("device", "home"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv:\n got %q\nwant %q", got, want)
	}

	// The stream turned to landscape: the point space turns with it.
	land := target.Meta{Width: 2556, Height: 1179}
	press(t, in, land, clk, 50*time.Millisecond, [2]int32{2556 - 1, 1179 - 1})
	if got := f.waitCalls(4)[3]; !reflect.DeepEqual(got, argv("device", "tap", "--x", "851", "--y", "392")) {
		t.Fatalf("landscape tap = %q", got)
	}

	// Without a size from Revyl, iOS input refuses rather than guessing a
	// scale factor.
	blind, clk2 := gestures(t, f, "ios", 0, 0)
	_ = blind.touch(target.Touch{Action: target.TouchDown, PointerID: 1, X: 10, Y: 10}, m)
	clk2.t = clk2.t.Add(10 * time.Millisecond)
	if err := blind.touch(target.Touch{Action: target.TouchUp, PointerID: 1, X: 10, Y: 10}, m); !errors.Is(err, errRevylNoPoints) {
		t.Fatalf("iOS tap without a screen size = %v, want errRevylNoPoints", err)
	}
	// Typing before any tap has nowhere to go.
	if err := blind.text("x"); err == nil {
		t.Fatal("text before a tap succeeded")
	}
}

func TestRevylInputReturnsBeforeTheCLIDoes(t *testing.T) {
	f := newFakeRevyl(t)
	if err := os.WriteFile(filepath.Join(f.dir, "slow"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	in, clk := gestures(t, f, "android", 1080, 2400)
	m := target.Meta{Width: 1080, Height: 2400}
	start := time.Now()
	press(t, in, m, clk, 10*time.Millisecond, [2]int32{10, 10})
	press(t, in, m, clk, 10*time.Millisecond, [2]int32{20, 20})
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Fatalf("two taps took %v to queue; input must not wait for the CLI", d)
	}
	got := f.waitCalls(2)
	if got[0][3] != "10" || got[1][3] != "20" {
		t.Fatalf("taps ran out of order: %q", got)
	}
}

func TestRevylTargetOpensWHEPAndMapsTouches(t *testing.T) {
	w := newFakeWHEP(t)
	f := newFakeRevyl(t)
	whep := w.srv.URL + "/whep/endpoint"
	// The first read is a device still starting, with no stream yet.
	f.respondAs("device-info.json.first", "device-info-starting", "")
	f.respond("device-info", "device-info", whep)

	tgt := &Target{
		CLI: f.cli, Model: "Pixel 7", OSVersion: "Android 14",
		Session:  Session{SessionID: testSession, Platform: "android"},
		settings: loopbackSettings(), poll: 20 * time.Millisecond,
	}
	if tgt.ID() != "revyl:"+testSession || tgt.Kind() != target.KindRevyl || tgt.Label() != "Revyl Pixel 7" {
		t.Fatalf("ID %q Kind %q Label %q", tgt.ID(), tgt.Kind(), tgt.Label())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := tgt.Open(ctx, target.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if m := st.Meta(); m.Width != 360 || m.Height != 202 {
		t.Fatalf("Meta = %+v, want the SPS's 360x202", m)
	}
	if _, ok := st.(target.TextTyper); !ok {
		t.Fatal("stream does not type text")
	}

	// The fixture's screen is 720x404 pixels, twice the stream.
	if err := st.Touch(target.Touch{Action: target.TouchDown, PointerID: 7, X: 180, Y: 101, Pressure: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.Touch(target.Touch{Action: target.TouchUp, PointerID: 7, X: 180, Y: 101}); err != nil {
		t.Fatal(err)
	}
	calls := f.waitCalls(3)
	if got := calls[2]; !reflect.DeepEqual(got, argv("device", "tap", "--x", "360", "--y", "202")) {
		t.Fatalf("tap argv = %q (all %q)", got, calls)
	}
	if err := st.Scroll(target.Scroll{X: 1, Y: 1, VScroll: 1}); !errors.Is(err, target.ErrNoControl) {
		t.Fatalf("Scroll = %v, want ErrNoControl", err)
	}
}
