package ios

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

func TestSimLogArgs(t *testing.T) {
	since := time.Date(2026, 10, 22, 9, 5, 7, 900e6, time.FixedZone("EDT", -4*3600))
	for _, c := range []struct {
		name   string
		q      target.LogcatQuery
		pid    int
		window time.Duration
		want   []string
	}{
		{"dump", target.LogcatQuery{Lines: 10}, 0, time.Minute,
			[]string{"show", "--style", "compact", "--last", "60s", "--info"}},
		{"since", target.LogcatQuery{Since: since}, 0, 0,
			[]string{"show", "--style", "compact", "--start", "2026-10-22 09:05:07-0400", "--info"}},
		{"follow", target.LogcatQuery{Follow: true, Lines: 10}, 0, 0,
			[]string{"stream", "--style", "compact", "--info"}},
		{"pid", target.LogcatQuery{Lines: 10}, 959, time.Hour,
			[]string{"show", "--style", "compact", "--last", "3600s", "--info", "--predicate", "processID == 959"}},
		{"ndjson", target.LogcatQuery{Follow: true, Format: LogFormatNDJSON}, 0, 0,
			[]string{"stream", "--style", "ndjson", "--info"}},
	} {
		if got := simLogArgs(c.q, c.pid, c.window); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %q; want %q", c.name, got, c.want)
		}
	}
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSimLogFilterDropsTheHeaderOnlyFirst(t *testing.T) {
	in := "Timestamp               Ty Process[PID:TID]\n" +
		"2026-10-22 09:00:00.000 Df a[1:2] one\n" +
		"Timestamp is a word a message may start with\n"
	got := readAll(t, newSimLogFilter(io.NopCloser(strings.NewReader(in)), ""))
	want := "2026-10-22 09:00:00.000 Df a[1:2] one\nTimestamp is a word a message may start with\n"
	if got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}

func TestSimLogFilterDropsTheNDJSONTrailer(t *testing.T) {
	in := `{"eventMessage":"one"}` + "\n" + `{"count":1,"finished":1}` + "\n"
	got := readAll(t, newSimLogFilter(io.NopCloser(strings.NewReader(in)), LogFormatNDJSON))
	if got != `{"eventMessage":"one"}`+"\n" {
		t.Errorf("got %q", got)
	}
}

func TestSimLogFilterKeepsAnUnterminatedLastLine(t *testing.T) {
	got := readAll(t, newSimLogFilter(io.NopCloser(strings.NewReader("Timestamp x\na\nb")), ""))
	if got != "a\nb" {
		t.Errorf("got %q", got)
	}
}

func TestPidForApp(t *testing.T) {
	list := "PID\tStatus\tLabel\n" +
		"-\t0\tUIKitApplication:com.apple.mobilesafari[9a1c][rb-legacy]\n" +
		"4211\t0\tUIKitApplication:com.apple.Preferences[2f3e][rb-legacy]\n" +
		"4300\t0\tcom.apple.Preferences.extension\n"
	if got := pidForApp(list, "com.apple.Preferences"); got != 4211 {
		t.Errorf("Preferences = %d; want 4211", got)
	}
	if got := pidForApp(list, "com.apple.mobilesafari"); got != 0 {
		t.Errorf("a listed app with no pid = %d; want 0", got)
	}
	if got := pidForApp(list, "com.apple"); got != 0 {
		t.Errorf("a prefix of a bundle = %d; want 0", got)
	}
}

func TestLastFilteredKeepsTheLastN(t *testing.T) {
	got, err := lastFiltered(io.NopCloser(strings.NewReader("a\nb\nc\nd\ne\n")), 2)
	if err != nil || !reflect.DeepEqual(got, []string{"d", "e"}) {
		t.Errorf("got %q, %v", got, err)
	}
	got, _ = lastFiltered(io.NopCloser(strings.NewReader("a\n")), 3)
	if !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("short: got %q", got)
	}
}

// fakeSimLog stands in for simctl, answering each --last window with the
// lines given for it.
func fakeSimLog(t *testing.T, byWindow map[string]string) *[]string {
	t.Helper()
	var asked []string
	old := simLogStart
	simLogStart = func(ctx context.Context, udid string, args []string) (io.ReadCloser, error) {
		w := ""
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--last" {
				w = args[i+1]
			}
		}
		asked = append(asked, w)
		return io.NopCloser(strings.NewReader("Timestamp header\n" + byWindow[w])), nil
	}
	t.Cleanup(func() { simLogStart = old })
	return &asked
}

func TestSimLogTailWidensUntilItHasEnough(t *testing.T) {
	asked := fakeSimLog(t, map[string]string{"60s": "a\n", "600s": "a\nb\nc\n", "3600s": "x\n"})
	got, _, err := simLogTail(context.Background(), "u", target.LogcatQuery{Lines: 2}, 0)
	if err != nil || string(got) != "b\nc\n" {
		t.Errorf("got %q, %v", got, err)
	}
	if !reflect.DeepEqual(*asked, []string{"60s", "600s"}) {
		t.Errorf("windows = %q; want it to stop at the one with enough", *asked)
	}
}

func TestSimLogTailReturnsTheWidestWhenNoneHasEnough(t *testing.T) {
	fakeSimLog(t, map[string]string{"60s": "a\n", "600s": "a\nb\n", "3600s": "a\nb\nc\n"})
	got, cut, err := simLogTail(context.Background(), "u", target.LogcatQuery{Lines: 10}, 0)
	if err != nil || string(got) != "a\nb\nc\n" || cut != "" {
		t.Errorf("got %q, cut %q, %v", got, cut, err)
	}
}

func TestSimLogTailStopsWideningPastTheDeadline(t *testing.T) {
	asked := fakeSimLog(t, map[string]string{"60s": "a\n"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond)
	got, cut, err := simLogTail(ctx, "u", target.LogcatQuery{Lines: 10}, 0)
	if err != nil || string(got) != "a\n" || cut != target.LogCutTime {
		t.Errorf("got %q, cut %q, %v", got, cut, err)
	}
	if len(*asked) != 1 {
		t.Errorf("windows = %q; want it to stop after the first", *asked)
	}
}

// slowOpen is a log that waits d before its first byte, as log show does
// while it opens the store, and then sends the rest at once.
type slowOpen struct {
	d      time.Duration
	opened bool
	io.Reader
}

func (s *slowOpen) Read(p []byte) (int, error) {
	if !s.opened {
		s.opened = true
		time.Sleep(s.d)
	}
	return s.Reader.Read(p)
}

// Every window pays log show's open cost once, so it is not scaled with the
// window. On a simulator a minute filtered to one app took 0.98s, 0.97s of
// it before the first byte, and ten times that stopped a since=60m dump at
// the minute while the ten minutes took 0.92s.
func TestSimLogTailDoesNotScaleTheOpenCost(t *testing.T) {
	asked := fakeSimLog(t, map[string]string{"60s": "a\n", "600s": "a\nb\n"})
	fast := simLogStart // fakeSimLog's cleanup puts the real one back
	simLogStart = func(ctx context.Context, udid string, args []string) (io.ReadCloser, error) {
		r, err := fast(ctx, udid, args)
		return io.NopCloser(&slowOpen{d: 150 * time.Millisecond, Reader: r}), err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, cut, err := simLogTail(ctx, "u", target.LogcatQuery{Lines: 2}, 0)
	if err != nil || string(got) != "a\nb\n" || cut != "" {
		t.Errorf("got %q, cut %q, %v; want both lines from the ten minutes", got, cut, err)
	}
	if !reflect.DeepEqual(*asked, []string{"60s", "600s"}) {
		t.Errorf("windows = %q; want the minute, then ten", *asked)
	}
}

func TestSimLogGuess(t *testing.T) {
	ms := time.Millisecond
	// Unfiltered on the same simulator: the minute's first byte at 0.94s,
	// its end at 1.28s; the ten minutes took 4.12s.
	if g := simLogGuess(940*ms, 1280*ms, time.Minute, 10*time.Minute); g < 3*time.Second || g > 6*time.Second {
		t.Errorf("unfiltered guess = %v; want near the 4.12s it took", g)
	}
	// No byte at all: the whole time was the open.
	if g := simLogGuess(0, 900*ms, time.Minute, 10*time.Minute); g != 900*ms {
		t.Errorf("empty guess = %v; want the open cost alone", g)
	}
}

// fakeSimLogSince stands in for simctl on a since dump, answering a --last
// window by its length and the --start one as "start".
func fakeSimLogSince(t *testing.T, byWindow map[string]string) *[]string {
	t.Helper()
	var asked []string
	old := simLogStart
	simLogStart = func(ctx context.Context, udid string, args []string) (io.ReadCloser, error) {
		w := ""
		for i, a := range args {
			if a == "--last" && i+1 < len(args) {
				w = args[i+1]
			} else if a == "--start" {
				w = "start"
			}
		}
		asked = append(asked, w)
		return io.NopCloser(strings.NewReader("Timestamp header\n" + byWindow[w])), nil
	}
	t.Cleanup(func() { simLogStart = old })
	return &asked
}

// A since far back is read newest first, so a busy log answers from its
// last minute rather than streaming the whole window from its start.
func TestSimLogSinceReadsTheNewestFirst(t *testing.T) {
	asked := fakeSimLogSince(t, map[string]string{"60s": "a\nb\nc\n", "start": "old\n"})
	q := target.LogcatQuery{Lines: 2, Since: time.Now().Add(-time.Hour)}
	got, cut, err := simLogTail(context.Background(), "u", q, 0)
	if err != nil || string(got) != "b\nc\n" || cut != "" {
		t.Errorf("got %q, cut %q, %v", got, cut, err)
	}
	if !reflect.DeepEqual(*asked, []string{"60s"}) {
		t.Errorf("windows = %q; want only the last minute", *asked)
	}
}

// The windows stop at since: none longer than it is tried, and since
// itself is the last.
func TestSimLogSinceEndsAtSince(t *testing.T) {
	asked := fakeSimLogSince(t, map[string]string{"60s": "b\n", "start": "a\nb\n", "600s": "x\ny\nz\n"})
	q := target.LogcatQuery{Lines: 10, Since: time.Now().Add(-5 * time.Minute)}
	got, cut, err := simLogTail(context.Background(), "u", q, 0)
	if err != nil || string(got) != "a\nb\n" || cut != "" {
		t.Errorf("got %q, cut %q, %v", got, cut, err)
	}
	if !reflect.DeepEqual(*asked, []string{"60s", "start"}) {
		t.Errorf("windows = %q; want the minute, then since", *asked)
	}
}

func TestSimLogSinceSaysWhenTheDeadlineCutIt(t *testing.T) {
	asked := fakeSimLogSince(t, map[string]string{"60s": "a\n"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond)
	q := target.LogcatQuery{Lines: 10, Since: time.Now().Add(-time.Hour)}
	got, cut, err := simLogTail(ctx, "u", q, 0)
	if err != nil || string(got) != "a\n" || cut != target.LogCutTime {
		t.Errorf("got %q, cut %q, %v; want the minute, cut by time", got, cut, err)
	}
	if len(*asked) != 1 {
		t.Errorf("windows = %q; want it to stop after the first", *asked)
	}
}

func TestParseCompact(t *testing.T) {
	rn := []string{"com.facebook.react.log", "com.facebook.react.log:javascript"}
	for _, c := range []struct {
		line string
		tags []string
		msg  string
		cont bool
	}{
		{`2026-11-24 07:45:56.764 Df locationd[33406:2f24689] [com.apple.locationd.Utility:QA] {"msg":"x"}`,
			[]string{"com.apple.locationd.Utility", "com.apple.locationd.Utility:QA"}, `{"msg":"x"}`, false},
		// A category may hold spaces.
		{"2026-11-24 07:45:59.281 E  SpringBoard[33390:29d7943] [com.apple.coregraphics:Unknown process name] CGAffineTransformInvert: singular matrix.",
			[]string{"com.apple.coregraphics", "com.apple.coregraphics:Unknown process name"}, "CGAffineTransformInvert: singular matrix.", false},
		// launchd's subsystems hold brackets and spaces, and no category.
		{"2026-11-24 07:45:59.814 Df launchd_sim[33370:2f30703] [user/501/com.apple.managedappdistributiond [23706]:] xpcproxy_sim spawned with pid 23706",
			[]string{"user/501/com.apple.managedappdistributiond [23706]"}, "xpcproxy_sim spawned with pid 23706", false},
		// With no subsystem the sender's image is printed instead.
		{"2026-11-24 07:45:59.978 F  backboardd[33403:29d7a27] (IOSurface) IOSurfaceQueryTransactionList got non-success",
			[]string{"IOSurface"}, "IOSurfaceQueryTransactionList got non-success", false},
		{"2026-11-24 08:00:00.000 Df Runner[4300:1a2b] (Flutter) flutter: porchlight tapped",
			[]string{"Flutter"}, "flutter: porchlight tapped", false},
		// A process name may hold a space, and a message may start with a
		// bracket of its own.
		{"2026-11-24 08:00:00.000 I  Expo Go[4211:1a2b] [com.facebook.react.log:javascript] [porchlight] sent",
			rn, "[porchlight] sent", false},
		{"2026-11-24 08:00:00.000 I  Expo Go[4211:1a2b] [com.facebook.react.log:javascript]", rn, "", false},
		// With nothing between the process and the message, the sender is
		// the process's own binary, and its name is the tag.
		{"2026-11-24 07:47:45.253 Df launchd_sim[37327:2f34f37] No spawn timestamp found; service=com.apple.managedappdistributiond, pid=35531",
			[]string{"launchd_sim"}, "No spawn timestamp found; service=com.apple.managedappdistributiond, pid=35531", false},
		{"2026-11-24 08:00:00.000 Df Expo Go[4211:1a2b] PORCHLIGHT-JS said", []string{"Expo Go"}, "PORCHLIGHT-JS said", false},
		{"2026-11-24 08:00:00.000 Df launchd_sim[1:2]", []string{"launchd_sim"}, "", false},
		// The process's own message may start with a bracket or a
		// parenthesis that is not a subsystem or a sender.
		{"2026-11-24 08:00:00.000 Df Porchlight[500:2b] [porchlight] tapped", []string{"Porchlight"}, "[porchlight] tapped", false},
		{"2026-11-24 08:00:00.000 Df Porchlight[500:2b] [porchlight] door: front] ok", []string{"Porchlight"}, "[porchlight] door: front] ok", false},
		{"2026-11-24 08:00:00.000 Df Porchlight[500:2b] (unclosed", []string{"Porchlight"}, "(unclosed", false},
		// A continuation's message is the whole line.
		{"}, with animation: true", nil, "}, with animation: true", true},
		{"    \"FLAGS\" = 5;", nil, "    \"FLAGS\" = 5;", true},
	} {
		got := parseCompact([]byte(c.line))
		if !reflect.DeepEqual(got.Tags, c.tags) || string(got.Message) != c.msg || got.Continues != c.cont {
			t.Errorf("parseCompact(%q) = %q %q %v; want %q %q %v", c.line, got.Tags, got.Message, got.Continues, c.tags, c.msg, c.cont)
		}
	}
}

func TestParseNDJSON(t *testing.T) {
	for _, c := range []struct {
		line string
		tags []string
		msg  string
	}{
		{`{"subsystem":"com.facebook.react.log","category":"javascript","senderImagePath":"/x/Expo Go","eventMessage":"sent"}`,
			[]string{"com.facebook.react.log", "com.facebook.react.log:javascript"}, "sent"},
		{`{"subsystem":"","category":"","senderImagePath":"/a/Flutter.framework/Flutter","eventMessage":"flutter: x"}`,
			[]string{"Flutter"}, "flutter: x"},
		{`{"subsystem":"user/501/x [1]","category":"","senderImagePath":"/usr/libexec/launchd_sim","eventMessage":"m"}`,
			[]string{"user/501/x [1]"}, "m"},
		{`not json`, nil, ""},
	} {
		got := parseNDJSON([]byte(c.line))
		if !reflect.DeepEqual(got.Tags, c.tags) || string(got.Message) != c.msg || got.Continues {
			t.Errorf("parseNDJSON(%q) = %q %q %v; want %q %q", c.line, got.Tags, got.Message, got.Continues, c.tags, c.msg)
		}
	}
}

const (
	compactRN    = "2026-11-24 08:00:00.000 I  Expo Go[4211:1a2b] [com.facebook.react.log:javascript] porchlight-js sent {\n"
	compactCont  = "  \"n\" = 1;\n"
	compactOther = "2026-11-24 08:00:01.000 Df locationd[1:2] [com.apple.locationd:QA] tick {\n"
	compactSaid  = "2026-11-24 08:00:02.000 Df Expo Go[4211:1a2b] PORCHLIGHT-JS said\n"
)

func TestSimLogReaderFiltersByTagAndText(t *testing.T) {
	in := "Timestamp               Ty Process[PID:TID]\n" + compactRN + compactCont + compactOther + compactCont + compactSaid
	for _, c := range []struct {
		q    target.LogcatQuery
		want string
	}{
		{target.LogcatQuery{Tag: "com.facebook.react.log"}, compactRN + compactCont},
		{target.LogcatQuery{Tag: "COM.FACEBOOK.REACT.LOG:javascript"}, compactRN + compactCont},
		{target.LogcatQuery{Tag: "expo go"}, compactSaid},
		{target.LogcatQuery{Text: "porchlight-js"}, compactRN + compactCont + compactSaid},
		// A match in a continuation keeps its whole entry.
		{target.LogcatQuery{Text: `"n" = 1`}, compactRN + compactCont + compactOther + compactCont},
		{target.LogcatQuery{Tag: "com.apple.locationd", Text: `"N" = 1`}, compactOther + compactCont},
		{target.LogcatQuery{}, in[len("Timestamp               Ty Process[PID:TID]\n"):]},
	} {
		if got := readAll(t, simLogReader(io.NopCloser(strings.NewReader(in)), c.q)); got != c.want {
			t.Errorf("%+v: got %q; want %q", c.q, got, c.want)
		}
	}
}

// Text looks in eventMessage alone, so a word that is also a key or a field
// value does not keep the line.
func TestSimLogReaderNDJSONTextIsTheMessageOnly(t *testing.T) {
	in := `{"subsystem":"porchlight","eventMessage":"tick"}` + "\n" +
		`{"subsystem":"x","eventMessage":"Porchlight sent"}` + "\n" +
		`{"count":2,"finished":1}` + "\n"
	got := readAll(t, simLogReader(io.NopCloser(strings.NewReader(in)), target.LogcatQuery{Format: LogFormatNDJSON, Text: "porchlight"}))
	if got != `{"subsystem":"x","eventMessage":"Porchlight sent"}`+"\n" {
		t.Errorf("got %q", got)
	}
}

// The same entries in both styles, as log show prints them: launchd_sim's
// own message with no subsystem, CoreSpotlight's in Preferences, an app's
// own message that starts with a bracket, and an app's subsystem with a
// message over three lines.
const (
	bothCompact = "Timestamp               Ty Process[PID:TID]\n" +
		"2026-11-24 07:47:45.253 Df launchd_sim[37327:2f34f37] No spawn timestamp found; service=com.apple.managedappdistributiond, pid=35531\n" +
		"2026-11-24 07:47:45.300 Df Preferences[400:1a] (CoreSpotlight) indexing {\n" +
		"    kind = _NSSwiftCalendar: gregorian;\n" +
		"}\n" +
		"2026-11-24 07:47:45.400 Df Porchlight[500:2b] [porchlight] tapped\n" +
		"2026-11-24 07:47:45.500 I  Porchlight[500:2b] [dev.porchlight:ui] sent {\n" +
		"    door = front;\n" +
		"}\n"
	bothNDJSON = `{"subsystem":"","category":"","processImagePath":"/usr/libexec/launchd_sim","senderImagePath":"/usr/libexec/launchd_sim","eventMessage":"No spawn timestamp found; service=com.apple.managedappdistributiond, pid=35531"}` + "\n" +
		`{"subsystem":"","category":"","processImagePath":"/x/Preferences","senderImagePath":"/x/CoreSpotlight","eventMessage":"indexing {\n    kind = _NSSwiftCalendar: gregorian;\n}"}` + "\n" +
		`{"subsystem":"","category":"","processImagePath":"/x/Porchlight.app/Porchlight","senderImagePath":"/x/Porchlight.app/Porchlight","eventMessage":"[porchlight] tapped"}` + "\n" +
		`{"subsystem":"dev.porchlight","category":"ui","processImagePath":"/x/Porchlight.app/Porchlight","senderImagePath":"/x/Porchlight.app/Porchlight","eventMessage":"sent {\n    door = front;\n}"}` + "\n" +
		`{"count":4,"finished":1}` + "\n"
)

// entries counts the entries in a filtered read: its timestamped lines in
// compact, its lines in ndjson.
func entries(out, format string) int {
	n := 0
	for _, l := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if l != "" && (format == LogFormatNDJSON || compactStamp([]byte(l))) {
			n++
		}
	}
	return n
}

// A query keeps the same entries whichever style it reads.
func TestSimLogReaderKeepsTheSameEntriesInBothStyles(t *testing.T) {
	for _, c := range []struct {
		q    target.LogcatQuery
		want int
	}{
		{target.LogcatQuery{Tag: "launchd_sim"}, 1},
		{target.LogcatQuery{Tag: "porchlight"}, 1},
		{target.LogcatQuery{Tag: "CoreSpotlight"}, 1},
		{target.LogcatQuery{Tag: "dev.porchlight:ui"}, 1},
		{target.LogcatQuery{Text: "_NSSwiftCalendar:"}, 1},
		{target.LogcatQuery{Text: "door = front"}, 1},
		{target.LogcatQuery{Tag: "dev.porchlight", Text: "front"}, 1},
		{target.LogcatQuery{Text: "tapped"}, 1},
		{target.LogcatQuery{Text: "spawn"}, 1},
		{target.LogcatQuery{Text: "porchlight"}, 1},
		{target.LogcatQuery{Tag: "launchd_sim", Text: "front"}, 0},
	} {
		compact := readAll(t, simLogReader(io.NopCloser(strings.NewReader(bothCompact)), c.q))
		nq := c.q
		nq.Format = LogFormatNDJSON
		ndjson := readAll(t, simLogReader(io.NopCloser(strings.NewReader(bothNDJSON)), nq))
		if got := entries(compact, ""); got != c.want {
			t.Errorf("compact %+v kept %d entries; want %d:\n%s", c.q, got, c.want, compact)
		}
		if got := entries(ndjson, LogFormatNDJSON); got != c.want {
			t.Errorf("ndjson %+v kept %d entries; want %d:\n%s", c.q, got, c.want, ndjson)
		}
	}
	// A kept compact entry keeps every line it printed.
	got := readAll(t, simLogReader(io.NopCloser(strings.NewReader(bothCompact)), target.LogcatQuery{Text: "_NSSwiftCalendar:"}))
	if want := "2026-11-24 07:47:45.300 Df Preferences[400:1a] (CoreSpotlight) indexing {\n    kind = _NSSwiftCalendar: gregorian;\n}\n"; got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}

// cutSimLog stands in for simctl: each "style window" in byWindow, such as
// "ndjson 60s", prints its entries as log show would and ends, and any
// other blocks until ctx ends and then fails as a killed log show does.
func cutSimLog(t *testing.T, byWindow map[string]string) {
	t.Helper()
	old := simLogStart
	simLogStart = func(ctx context.Context, udid string, args []string) (io.ReadCloser, error) {
		style, w := args[2], ""
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--last" {
				w = args[i+1]
			}
		}
		lines, ok := byWindow[style+" "+w]
		if !ok {
			return &killedLog{ctx: ctx}, nil
		}
		if style == LogFormatNDJSON {
			return io.NopCloser(strings.NewReader(lines + `{"count":1,"finished":1}` + "\n")), nil
		}
		return io.NopCloser(strings.NewReader("Timestamp header\n" + lines)), nil
	}
	t.Cleanup(func() { simLogStart = old })
}

const ndjsonOther = `{"subsystem":"com.apple.locationd","category":"QA","processImagePath":"/usr/libexec/locationd","senderImagePath":"/usr/libexec/locationd","eventMessage":"tick"}` + "\n"

type killedLog struct{ ctx context.Context }

func (k *killedLog) Read([]byte) (int, error) {
	<-k.ctx.Done()
	return 0, errors.New("simctl spawn log show: signal: terminated")
}

func (k *killedLog) Close() error { return nil }

// A window that finished with no match is an answer: the dump is empty, not
// the error of a wider window that the deadline cut.
func TestSimLogTailWithNoMatchBeforeTheDeadlineIsEmpty(t *testing.T) {
	cutSimLog(t, map[string]string{"compact 60s": compactOther + compactOther, "ndjson 60s": ndjsonOther + ndjsonOther})
	for _, q := range []target.LogcatQuery{
		{Lines: 10, Tag: "nothing.matches"},
		{Lines: 10, Text: "nothing matches"},
		{Lines: 10, Tag: "nothing.matches", Format: LogFormatNDJSON},
		{Lines: 10, Text: "nothing matches", Format: LogFormatNDJSON},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		got, cut, err := simLogTail(ctx, "u", q, 0)
		cancel()
		if err != nil || len(got) != 0 {
			t.Errorf("%+v: got %q, %v; want nothing and no error", q, got, err)
		}
		// The deadline stopped it short of the lines asked for.
		if cut != target.LogCutTime {
			t.Errorf("%+v: cut = %q; want %q", q, cut, target.LogCutTime)
		}
	}
}

// A dump whose first window the deadline cuts has nothing to answer with.
func TestSimLogTailCutInItsFirstWindowFails(t *testing.T) {
	cutSimLog(t, nil)
	for _, q := range []target.LogcatQuery{
		{Lines: 10},
		{Lines: 10, Tag: "nothing.matches"},
		{Lines: 10, Tag: "nothing.matches", Format: LogFormatNDJSON},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, _, err := simLogTail(ctx, "u", q, 0)
		cancel()
		if err == nil {
			t.Errorf("%+v: no error", q)
		}
	}
}

// The window stops widening on matching lines, so a match older than the
// last lines of the window is still found.
func TestSimLogTailCountsOnlyMatchingLines(t *testing.T) {
	asked := fakeSimLog(t, map[string]string{
		"60s":  compactRN + compactOther + compactOther,
		"600s": compactRN + compactRN,
	})
	got, cut, err := simLogTail(context.Background(), "u", target.LogcatQuery{Lines: 1, Tag: "com.facebook.react.log"}, 0)
	if err != nil || string(got) != compactRN || cut != "" {
		t.Errorf("got %q, cut %q, %v", got, cut, err)
	}
	if !reflect.DeepEqual(*asked, []string{"60s"}) {
		t.Errorf("windows = %q; want the first, which holds a match", *asked)
	}
}

func TestValidBundleID(t *testing.T) {
	for _, ok := range []string{"com.apple.Preferences", "dev.bmc-creations.app", "Foo"} {
		if !ValidBundleID(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "-com.a", ".com.a", "com.a;reboot", "com a", "com/a"} {
		if ValidBundleID(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
