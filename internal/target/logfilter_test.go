package target

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// parseColon reads "tag: message" lines; a line that starts with a space
// continues the one above.
func parseColon(line []byte) LogLine {
	if bytes.HasPrefix(line, []byte(" ")) {
		return LogLine{Continues: true}
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
		if f.Keep([]byte(l)) {
			kept = append(kept, l)
		}
	}
	return kept
}

func TestNewLogFilterIsNilWithoutATagOrText(t *testing.T) {
	if f := NewLogFilter(LogcatQuery{Lines: 10, PID: 3}, parseColon); f != nil {
		t.Errorf("filter %+v for a query with neither", f)
	}
	var f *LogFilter
	if !f.Keep([]byte("anything")) {
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
		if !NewLogFilter(LogcatQuery{Tag: tag}, parse).Keep([]byte("x")) {
			t.Errorf("tag %q dropped the line", tag)
		}
	}
	if NewLogFilter(LogcatQuery{Tag: "javascript"}, parse).Keep([]byte("x")) {
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
