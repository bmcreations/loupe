package target

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

// parseColon reads "tag: message" lines; a line that starts with a space
// continues the one above.
func parseColon(line []byte) LogLine {
	if bytes.HasPrefix(line, []byte(" ")) {
		return LogLine{Continues: true, Message: line}
	}
	tag, msg, ok := bytes.Cut(line, []byte(": "))
	if !ok {
		return LogLine{}
	}
	return LogLine{Tags: []string{string(tag)}, Message: msg}
}

func keptLines(f *LogFilter, lines ...string) []string {
	var kept []string
	for _, l := range lines {
		for _, k := range f.Keep([]byte(l)) {
			kept = append(kept, string(k))
		}
	}
	return kept
}

func TestNewLogFilterIsNilWithoutATagOrText(t *testing.T) {
	if f := NewLogFilter(LogcatQuery{Lines: 10, PID: 3}, parseColon); f != nil {
		t.Errorf("filter %+v for a query with neither", f)
	}
	var f *LogFilter
	if got := f.Keep([]byte("anything")); len(got) != 1 || string(got[0]) != "anything" {
		t.Error("a nil filter dropped a line")
	}
}

func TestLogFilterTagIsExactAndIgnoresCase(t *testing.T) {
	f := NewLogFilter(LogcatQuery{Tag: "flutter"}, parseColon)
	got := keptLines(f, "flutter: one", "Flutter: two", "flutterx: three", "flu: four", "no tag here")
	if strings.Join(got, "|") != "flutter: one|Flutter: two" {
		t.Errorf("kept %q", got)
	}
}

func TestLogFilterTagMatchesAnyOfALinesTags(t *testing.T) {
	parse := func(line []byte) LogLine {
		return LogLine{Tags: []string{"com.facebook.react.log", "com.facebook.react.log:javascript"}, Message: line}
	}
	for _, tag := range []string{"com.facebook.react.log", "com.facebook.react.log:javascript"} {
		if len(NewLogFilter(LogcatQuery{Tag: tag}, parse).Keep([]byte("x"))) != 1 {
			t.Errorf("tag %q dropped the line", tag)
		}
	}
	if len(NewLogFilter(LogcatQuery{Tag: "javascript"}, parse).Keep([]byte("x"))) != 0 {
		t.Error("a category alone matched")
	}
}

// Text looks in the message only, so a word in the tag or the header does
// not keep every line that carries it.
func TestLogFilterTextSearchesTheMessageIgnoringCase(t *testing.T) {
	f := NewLogFilter(LogcatQuery{Text: "Porchlight-JS"}, parseColon)
	got := keptLines(f, "ReactNativeJS: porchlight-js: sent", "porchlight-js: unrelated", "x: PORCHLIGHT-JS done")
	if strings.Join(got, "|") != "ReactNativeJS: porchlight-js: sent|x: PORCHLIGHT-JS done" {
		t.Errorf("kept %q", got)
	}
}

func TestLogFilterTagAndTextMustBothMatch(t *testing.T) {
	f := NewLogFilter(LogcatQuery{Tag: "a", Text: "yes"}, parseColon)
	got := keptLines(f, "a: yes", "a: no", "b: yes")
	if strings.Join(got, "|") != "a: yes" {
		t.Errorf("kept %q", got)
	}
}

func TestLogFilterContinuationGoesWithItsEntry(t *testing.T) {
	f := NewLogFilter(LogcatQuery{Tag: "a"}, parseColon)
	got := keptLines(f, " before any entry", "a: {", " kept", "b: {", " dropped", "a: x")
	if strings.Join(got, "|") != "a: {| kept|a: x" {
		t.Errorf("kept %q", got)
	}
}

func filterAll(t *testing.T, q LogcatQuery, in string) string {
	t.Helper()
	b, err := io.ReadAll(FilterLog(io.NopCloser(strings.NewReader(in)), NewLogFilter(q, parseColon)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Text is looked for in every line of an entry, and the entry is kept or
// dropped whole, so a match after its first line keeps the lines before it.
func TestLogFilterTextMatchesAnyLineOfItsEntry(t *testing.T) {
	in := "a: {\n x\n match\n y\na: no\n z\nb: match\n"
	if got, want := filterAll(t, LogcatQuery{Text: "MATCH"}, in), "a: {\n x\n match\n y\nb: match\n"; got != want {
		t.Errorf("got %q; want %q", got, want)
	}
	// The tag is the header's, so a match in another tag's entry is not one.
	if got, want := filterAll(t, LogcatQuery{Tag: "b", Text: "x"}, in), ""; got != want {
		t.Errorf("tag b: got %q; want %q", got, want)
	}
}

// An entry is held only until the line that holds the text arrives, so a
// follow gets it then, without waiting for the next entry.
func TestFilterLogReleasesAHeldEntryOnItsMatch(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	r := FilterLog(pr, NewLogFilter(LogcatQuery{Text: "match"}, parseColon))
	go pw.Write([]byte("a: {\n x\n match\n"))
	const want = "a: {\n x\n match\n"
	done := make(chan string, 1)
	go func() {
		var got []byte
		buf := make([]byte, 64)
		for len(got) < len(want) {
			n, err := r.Read(buf)
			got = append(got, buf[:n]...)
			if err != nil {
				break
			}
		}
		done <- string(got)
	}()
	select {
	case got := <-done:
		if got != want {
			t.Errorf("got %q; want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Error("the entry was not served when its match arrived")
	}
}

// An entry too long to hold is dropped whole rather than served from the
// middle.
func TestLogFilterDropsAnEntryTooLongToHold(t *testing.T) {
	old := maxHeldEntry
	maxHeldEntry = 16
	t.Cleanup(func() { maxHeldEntry = old })
	in := "a: {\n 0123456789\n 0123456789\n match\nb: match\n"
	if got, want := filterAll(t, LogcatQuery{Text: "match"}, in), "b: match\n"; got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}

func TestFilterLogStreamsTheKeptLines(t *testing.T) {
	in := "a: one\r\nb: two\na: three"
	r := FilterLog(io.NopCloser(strings.NewReader(in)), NewLogFilter(LogcatQuery{Tag: "a"}, parseColon))
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "a: one\r\na: three" {
		t.Errorf("got %q", b)
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
}

func TestFilterLogWithoutAFilterIsTheSameReader(t *testing.T) {
	src := io.NopCloser(strings.NewReader("x"))
	if FilterLog(src, nil) != src {
		t.Error("FilterLog wrapped a reader it had nothing to filter on")
	}
}
