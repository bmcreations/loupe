package target

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"unicode/utf8"
)

// LogLine is what a target's parser reads out of one line of its log, for
// LogFilter to match a query's tag and text against.
type LogLine struct {
	// Tags are the names a query's tag may match: logcat's tag, or a
	// unified log entry's subsystem, subsystem:category or sender.
	Tags []string
	// Message is the entry's text, without the header in front of it.
	Message []byte
	// Continues marks a line with no header of its own, the rest of a
	// message that ran over several lines. It goes wherever its entry went.
	Continues bool
}

// LogFilter keeps the lines of a log that match a query's Tag and Text.
// It runs on the host, over what the device printed, so neither value is
// ever handed to a device's shell or to a log predicate.
//
// A Tag matches one of a line's tags exactly, ignoring case. A Text matches
// anywhere in the message, ignoring case. With both, a line must match both.
// A filter keeps state between lines, so each log read needs its own.
type LogFilter struct {
	tag      string
	text     []byte
	parse    func([]byte) LogLine
	lastKept bool
}

// NewLogFilter is a filter for q's Tag and Text, reading each line with
// parse. It is nil when q has neither, and a nil filter keeps every line.
func NewLogFilter(q LogcatQuery, parse func([]byte) LogLine) *LogFilter {
	if q.Tag == "" && q.Text == "" {
		return nil
	}
	return &LogFilter{tag: q.Tag, text: []byte(q.Text), parse: parse}
}

// Filtered reports whether q asks for a tag or text filter.
func (q LogcatQuery) Filtered() bool { return q.Tag != "" || q.Text != "" }

// Keep reports whether line passes the filter. line may still end in its
// newline.
func (f *LogFilter) Keep(line []byte) bool {
	if f == nil {
		return true
	}
	l := f.parse(bytes.TrimRight(line, "\r\n"))
	if l.Continues {
		return f.lastKept
	}
	f.lastKept = f.match(l)
	return f.lastKept
}

func (f *LogFilter) match(l LogLine) bool {
	if f.tag != "" {
		found := false
		for _, t := range l.Tags {
			if strings.EqualFold(t, f.tag) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return len(f.text) == 0 || containsFold(l.Message, f.text)
}

// containsFold reports whether sub is in s, ignoring case, without the
// copy a lowercased line would cost on every line of a long log.
func containsFold(s, sub []byte) bool {
	if len(sub) > len(s) {
		return false
	}
	first, _ := utf8.DecodeRune(sub)
	for i := 0; i+len(sub) <= len(s); i++ {
		if first < utf8.RuneSelf && s[i] != byte(first) && s[i]^0x20 != byte(first) {
			continue
		}
		if bytes.EqualFold(s[i:i+len(sub)], sub) {
			return true
		}
	}
	return false
}

// FilterLog wraps r so reads return only the lines f keeps, each as it
// arrives, which a follow needs. With a nil filter it is r itself.
func FilterLog(r io.ReadCloser, f *LogFilter) io.ReadCloser {
	if f == nil {
		return r
	}
	return &filteredLog{src: r, br: bufio.NewReaderSize(r, 64<<10), f: f}
}

type filteredLog struct {
	src  io.ReadCloser
	br   *bufio.Reader
	f    *LogFilter
	pend []byte
}

func (r *filteredLog) Read(p []byte) (int, error) {
	for len(r.pend) == 0 {
		line, err := r.br.ReadBytes('\n')
		if len(line) > 0 && r.f.Keep(line) {
			r.pend = line
		}
		if err != nil {
			if len(r.pend) > 0 {
				break
			}
			return 0, err
		}
	}
	n := copy(p, r.pend)
	r.pend = r.pend[n:]
	return n, nil
}

func (r *filteredLog) Close() error { return r.src.Close() }
