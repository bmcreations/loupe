package ios

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/bmcreations/loupe/internal/target"

	"github.com/bmcreations/loupe/internal/usbmux"
)

var _ target.Logcatter = (*Device)(nil)

// syslogOpen opens the phone's syslog_relay. A variable so tests can hand
// in a pipe.
var syslogOpen = func(ctx context.Context, udid string) (net.Conn, error) {
	return usbmux.Syslog(ctx, udid)
}

// A read without follow stops at the first quiet gap in the backlog the
// phone sends on connect, or at the cap if it never goes quiet. The backlog
// arrived in under a second on bmc-iphone, 64KB of it.
var (
	syslogQuiet = 500 * time.Millisecond
	syslogCap   = 3 * time.Second
)

// Logcat reads the phone's syslog: every process's lines, as text, through
// lockdownd's syslog_relay over a cable or Wi-Fi. It has no history to
// query by time and no structured form, so Since and Format are refused, and
// a package has no route to its pid here, so only PID, Tag and Text narrow
// it.
func (d *Device) Logcat(ctx context.Context, q target.LogcatQuery) (*target.Logcat, error) {
	switch {
	case q.Format != "":
		return nil, fmt.Errorf("%w: a phone's log is text only", ErrLogQueryUnsupported)
	case !q.Since.IsZero():
		return nil, fmt.Errorf("%w: a phone's log starts from the lines it holds now; since is not available", ErrLogQueryUnsupported)
	case q.Package != "" || q.Foreground:
		return nil, fmt.Errorf("%w: on a phone, narrow the log by pid", ErrLogQueryUnsupported)
	}
	info := d.Info()
	if !info.Reachable() {
		return nil, fmt.Errorf("%s is not connected", info.Name)
	}
	c, err := syslogOpen(ctx, info.UDID)
	if err != nil {
		return nil, fmt.Errorf("syslog: %w", err)
	}
	if q.Follow {
		stop := context.AfterFunc(ctx, func() { c.Close() })
		r := &syslogReader{src: c, stop: stop, f: newSyslogFilter(q)}
		return &target.Logcat{PID: q.PID, ReadCloser: r}, nil
	}
	defer c.Close()
	lines, err := syslogDump(c, newSyslogFilter(q), q.Lines)
	if err != nil {
		return nil, err
	}
	return &target.Logcat{PID: q.PID, ReadCloser: io.NopCloser(bytes.NewReader(target.JoinLines(lines)))}, nil
}

// syslogDump reads until the stream is quiet or the cap runs out, keeping
// the last n lines that pass f.
func syslogDump(c net.Conn, f *syslogFilter, n int) ([]string, error) {
	var kept []string
	keep := func(line []byte) {
		for _, l := range f.line(line) {
			kept = append(kept, string(l))
		}
		if n > 0 && len(kept) > 2*n {
			kept = append(kept[:0], kept[len(kept)-n:]...)
		}
	}
	end := time.Now().Add(syslogCap)
	buf := make([]byte, 32<<10)
	var part []byte
	for {
		dl := time.Now().Add(syslogQuiet)
		if dl.After(end) {
			dl = end
		}
		c.SetReadDeadline(dl)
		k, err := c.Read(buf)
		part = append(part, buf[:k]...)
		for {
			i := bytes.IndexByte(part, '\n')
			if i < 0 {
				break
			}
			keep(part[:i])
			part = part[i+1:]
		}
		if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("syslog: %w", err)
		}
	}
	if n > 0 && len(kept) > n {
		kept = kept[len(kept)-n:]
	}
	return kept, nil
}

// syslogReader is a follow: lines as they arrive, decoded and filtered.
type syslogReader struct {
	src  net.Conn
	stop func() bool
	f    *syslogFilter
	in   []byte
	out  []byte
	buf  [32 << 10]byte
}

func (r *syslogReader) Read(p []byte) (int, error) {
	for len(r.out) == 0 {
		k, err := r.src.Read(r.buf[:])
		r.in = append(r.in, r.buf[:k]...)
		for {
			i := bytes.IndexByte(r.in, '\n')
			if i < 0 {
				break
			}
			for _, l := range r.f.line(r.in[:i]) {
				r.out = append(append(r.out, l...), '\n')
			}
			r.in = r.in[i+1:]
		}
		if err != nil && len(r.out) == 0 {
			return 0, err
		}
		if err != nil {
			break
		}
	}
	n := copy(p, r.out)
	r.out = r.out[n:]
	return n, nil
}

func (r *syslogReader) Close() error {
	r.stop()
	return r.src.Close()
}

// syslogHeader finds the pid in a syslog_relay line, which reads
// "Oct 22 20:20:38 bmc-iphone wifid(WiFiPolicy)[56] <Notice>: msg".
var syslogHeader = regexp.MustCompile(`\[(\d+)\] <[A-Za-z]+>:`)

// syslogFilter turns one raw line into the lines to serve now, none when
// it is dropped or held. A line with no header continues the message above
// it, a multi-line dictionary for one, so it goes wherever that message
// went. The tag and text are matched after unvis, against the lines as they
// are served, and a text found in any line of a message keeps all of it.
type syslogFilter struct {
	pid      int
	lastKept bool
	tags     *target.LogFilter
}

func newSyslogFilter(q target.LogcatQuery) *syslogFilter {
	return &syslogFilter{pid: q.PID, tags: target.NewLogFilter(q, parseSyslog)}
}

func (f *syslogFilter) line(raw []byte) [][]byte {
	raw = bytes.Trim(raw, "\x00\r")
	if len(raw) == 0 {
		return nil
	}
	if f.pid > 0 {
		if m := syslogHeader.FindSubmatch(raw); m != nil {
			p, _ := strconv.Atoi(string(m[1]))
			f.lastKept = p == f.pid
		}
		if !f.lastKept {
			return nil
		}
	}
	return f.tags.Keep(unvis(raw))
}

// unvis undoes the vis(3) encoding syslog_relay applies to every byte that
// is not printable ASCII: \M-c is c|0x80, \M^c is a control byte with the
// high bit, \^c a control byte, and \ooo an octal byte. Anything else is
// left as it came, so a backslash in a message survives.
func unvis(b []byte) []byte {
	if bytes.IndexByte(b, '\\') < 0 {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c != '\\' || i+1 >= len(b) {
			out = append(out, c)
			continue
		}
		rest := b[i+1:]
		switch {
		case len(rest) >= 3 && rest[0] == 'M' && rest[1] == '-':
			out = append(out, rest[2]|0x80)
			i += 3
		case len(rest) >= 3 && rest[0] == 'M' && rest[1] == '^':
			out = append(out, ctrl(rest[2])|0x80)
			i += 3
		case len(rest) >= 2 && rest[0] == '^':
			out = append(out, ctrl(rest[1]))
			i += 2
		case len(rest) >= 3 && isOctal(rest[0]) && isOctal(rest[1]) && isOctal(rest[2]):
			out = append(out, (rest[0]-'0')<<6|(rest[1]-'0')<<3|(rest[2]-'0'))
			i += 3
		default:
			out = append(out, c)
		}
	}
	return out
}

func ctrl(c byte) byte {
	if c == '?' {
		return 0x7f
	}
	return c & 0x1f
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

// syslogLine reads the header of a syslog_relay line: the process, with
// its sender in parentheses when the line names one, then the message.
var syslogLine = regexp.MustCompile(`^[A-Z][a-z]{2} [ \d]\d \d\d:\d\d:\d\d \S+ (.+?)\[\d+\] <[A-Za-z]+>: ?`)

// parseSyslog reads a decoded syslog_relay line for LogFilter. Its tag is
// the sender when the line names one, "wifid(WiFiPolicy)" is WiFiPolicy,
// and the process otherwise. A line with no header continues the one above.
func parseSyslog(line []byte) target.LogLine {
	if !syslogHeader.Match(line) {
		return target.LogLine{Continues: true, Message: line}
	}
	m := syslogLine.FindSubmatchIndex(line)
	if m == nil {
		return target.LogLine{}
	}
	name := line[m[2]:m[3]]
	if open := bytes.IndexByte(name, '('); open >= 0 && name[len(name)-1] == ')' {
		name = name[open+1 : len(name)-1]
	}
	return target.LogLine{Tags: []string{string(name)}, Message: line[m[1]:]}
}
