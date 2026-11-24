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
	// Message is the entry's text on this line: what follows the header,
	// or the whole of a line that continues an entry.
	Message []byte
	// Continues marks a line with no header of its own, the rest of a
	// message that ran over several lines. It goes wherever its entry went.
	Continues bool
}

// LogFilter keeps the entries of a log that match a query's Tag and Text.
// It runs on the host, over what the device printed, so neither value is
// ever handed to a device's shell or to a log predicate.
//
// A Tag matches one of the tags on an entry's first line exactly, ignoring
// case. A Text matches anywhere in the message, on any of the entry's
// lines, ignoring case. With both, an entry must match both. An entry is
// kept or dropped whole: while the tag matches and the text has not turned
// up yet, its lines are held, and they are served the moment a line holds
// the text, or dropped when the next entry starts. A filter keeps state
// between lines, so each log read needs its own.
type LogFilter struct {
	tag   string
	text  []byte
	parse func([]byte) LogLine
	state entryState
	held  [][]byte
	size  int
	out   [][]byte
}

type entryState int

const (
	entryDropped entryState = iota // also before the first entry
	entryKept
	entryHeld // the tag matched and the text is yet to
)

// NewLogFilter is a filter for q's Tag and Text, reading each line with
// parse. It is nil when q has neither, and a nil filter keeps every line.
func NewLogFilter(q LogcatQuery, parse func([]byte) LogLine) *LogFilter {
	if q.Tag == "" && q.Text == "" {
		return nil
	}
	return &LogFilter{tag: q.Tag, text: []byte(q.Text), parse: parse}
}

// maxHeldEntry bounds the bytes of one entry a text filter holds back
// while it waits for a line that holds the text. A var so tests can lower
// it.
var maxHeldEntry = 1 << 20

// Filtered reports whether q asks for a tag or text filter.
func (q LogcatQuery) Filtered() bool { return q.Tag != "" || q.Text != "" }

// Keep passes line through the filter and returns the lines to serve now,
// in order: none, line alone, or the held lines of line's entry and then
// line, when line is the first of the entry to hold the text. line may
// still end in its newline, and each line comes back as it went in. The
// result is good until the next call.
func (f *LogFilter) Keep(line []byte) [][]byte {
	if f == nil {
		return [][]byte{line}
	}
	f.out = f.out[:0]
	l := f.parse(bytes.TrimRight(line, "\r\n"))
	if !l.Continues {
		f.held, f.size = f.held[:0], 0
		switch {
		case !f.tagMatches(l.Tags):
			f.state = entryDropped
		case f.textIn(l.Message):
			f.state = entryKept
		default:
			f.state = entryHeld
		}
	} else if f.state == entryHeld && f.textIn(l.Message) {
		f.state = entryKept
	}
	switch f.state {
	case entryKept:
		f.out = append(append(f.out, f.held...), line)
		f.held, f.size = f.held[:0], 0
	case entryHeld:
		if f.size += len(line); f.size > maxHeldEntry {
			// Too long to hold: dropped whole, not served from the middle.
			f.state, f.held, f.size = entryDropped, nil, 0
		} else {
			f.held = append(f.held, bytes.Clone(line))
		}
	}
	return f.out
}

func (f *LogFilter) tagMatches(tags []string) bool {
	if f.tag == "" {
		return true
	}
	for _, t := range tags {
		if strings.EqualFold(t, f.tag) {
			return true
		}
	}
	return false
}

func (f *LogFilter) textIn(msg []byte) bool {
	return len(f.text) == 0 || containsFold(msg, f.text)
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

// FilterLog wraps r so reads return only the lines f keeps, each as soon
// as f lets it go, which a follow needs. With a nil filter it is r itself.
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
		if len(line) > 0 {
			for _, l := range r.f.Keep(line) {
				r.pend = append(r.pend, l...)
			}
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
