package ios

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// simLogErrorPredicate is what the error scan reads of a simulator's
// unified log: React Native's JS console at error and fault, which is where
// ExceptionsManager reports a thrown Error, and all of Flutter's engine,
// which logs at the default level, Dart's errors included. It is a
// constant, and spawn passes it to log as one argument.
const simLogErrorPredicate = `(subsystem == "com.facebook.react.log" AND category == "javascript" AND (messageType == error OR messageType == fault)) OR senderImagePath ENDSWITH "/Flutter.framework/Flutter"`

// simLogErrorWindow is how far back a scan with no since reads. The
// unified log keeps days, and a listing with no since is about now.
const simLogErrorWindow = time.Hour

// simLogErrorTimeout bounds a scan, so a busy simulator's log cannot hold
// the crash listing open. The predicate is applied inside log: an hour on a
// simulator running a Flutter app and a React Native one read in 0.86 to
// 0.94 s. A var so tests can shorten it.
var simLogErrorTimeout = 10 * time.Second

// simLogTimeLayout is ndjson's timestamp, "2026-11-24 07:50:29.597175-0500".
const simLogTimeLayout = "2006-01-02 15:04:05.000000-0700"

func simLogErrorArgs(since time.Time) []string {
	args := []string{"show", "--style", LogFormatNDJSON}
	if since.IsZero() {
		args = append(args, "--last", fmt.Sprintf("%ds", int(simLogErrorWindow/time.Second)))
	} else {
		// Rounded down to the second, as Logcat's is; the listing drops
		// what came before since.
		args = append(args, "--start", since.Format("2006-01-02 15:04:05-0700"))
	}
	return append(args, "--predicate", simLogErrorPredicate)
}

// simLogEvent is the part of an ndjson entry the scan reads.
type simLogEvent struct {
	Timestamp        string `json:"timestamp"`
	MessageType      string `json:"messageType"`
	Subsystem        string `json:"subsystem"`
	SenderImagePath  string `json:"senderImagePath"`
	ProcessImagePath string `json:"processImagePath"`
	ProcessID        int    `json:"processID"`
	EventMessage     string `json:"eventMessage"`
}

// simLogEntry is a log entry and the executable that wrote it.
type simLogEntry struct {
	target.LogEntry
	Image string
}

// parseSimLogErrors reads log show's ndjson into log entries. A message
// keeps its lines, so a Dart stack is one entry. Flutter's print and
// debugPrint come with "flutter: " ahead of the text, which Android's tag
// carries instead, and it is dropped so both read alike.
func parseSimLogErrors(r io.Reader) ([]simLogEntry, error) {
	var entries []simLogEntry
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev simLogEvent
		if json.Unmarshal(line, &ev) != nil || ev.Timestamp == "" {
			// The closing {"count":N,"finished":1} has no timestamp.
			continue
		}
		at, err := time.Parse(simLogTimeLayout, ev.Timestamp)
		if err != nil {
			continue
		}
		e := target.LogEntry{
			Time:    at.UTC(),
			PID:     ev.ProcessID,
			Process: path.Base(ev.ProcessImagePath),
			Error:   ev.MessageType == "Error" || ev.MessageType == "Fault",
			Text:    ev.EventMessage,
		}
		switch {
		case ev.Subsystem == "com.facebook.react.log":
			e.Runtime = target.LogReactNative
		case strings.HasSuffix(ev.SenderImagePath, "/Flutter.framework/Flutter"):
			e.Runtime = target.LogFlutter
			e.Text = strings.TrimPrefix(e.Text, "flutter: ")
		default:
			continue
		}
		e.Raw = fmt.Sprintf("%s %s[%d] %s", ev.Timestamp, e.Process, e.PID, ev.EventMessage)
		entries = append(entries, simLogEntry{LogEntry: e, Image: ev.ProcessImagePath})
	}
	return entries, sc.Err()
}

// simAppList is launchctl's list inside the simulator. A var so tests can
// stand in for simctl.
var simAppList = func(ctx context.Context, s *Simulator) (string, error) { return s.launchctlList(ctx) }

// logErrors scans the simulator's log for JS and Dart errors since since,
// or in the last hour.
func (s *Simulator) logErrors(ctx context.Context, since time.Time) ([]target.LogError, error) {
	ctx, cancel := context.WithTimeout(ctx, simLogErrorTimeout)
	defer cancel()
	r, err := simLogStart(ctx, s.UDID(), simLogErrorArgs(since))
	if err != nil {
		return nil, fmt.Errorf("simulator log: %w", err)
	}
	parsed, err := parseSimLogErrors(r)
	if cerr := r.Close(); err == nil {
		err = cerr
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("reading the simulator log for JS and Dart errors took over %s; pass a later since", simLogErrorTimeout)
	}
	if err != nil {
		return nil, fmt.Errorf("simulator log: %w", err)
	}
	entries := make([]target.LogEntry, len(parsed))
	// An error's time and pid are its first entry's, which names its
	// executable.
	type at struct {
		micros int64
		pid    int
	}
	images := map[at]string{}
	for i, e := range parsed {
		entries[i] = e.LogEntry
		images[at{e.Time.UnixMicro(), e.PID}] = e.Image
	}
	errs := target.ScanLogErrors(entries)
	bundles := map[string]string{}
	var list *string
	for i := range errs {
		// The executable's app names its bundle after it quits, for as
		// long as it is installed.
		if app := path.Dir(images[at{errs[i].Time.UnixMicro(), errs[i].PID}]); strings.HasSuffix(app, ".app") {
			id, ok := bundles[app]
			if !ok {
				id, _ = appBundleID(ctx, app)
				bundles[app] = id
			}
			if id != "" {
				errs[i].BundleID = id
				continue
			}
		}
		// An app deleted since: launchctl names it while it runs.
		if list == nil {
			l, _ := simAppList(ctx, s)
			list = &l
		}
		errs[i].BundleID = appForPid(*list, strconv.Itoa(errs[i].PID))
	}
	return errs, nil
}

// logErrorReports is logErrors as crash rows. A simulator that is not
// booted has no log to read, and lists none.
func (s *Simulator) logErrorReports(ctx context.Context, since time.Time) ([]target.CrashReport, error) {
	if !s.booted.Load() {
		return nil, nil
	}
	errs, err := s.logErrors(ctx, since)
	if err != nil {
		return nil, err
	}
	rows := make([]target.CrashReport, len(errs))
	for i, e := range errs {
		rows[i] = e.Report()
	}
	return rows, nil
}

// logErrorText is one log error's entries as the log printed them, found
// again by the time its id names.
func (s *Simulator) logErrorText(ctx context.Context, id string, at time.Time) ([]byte, error) {
	if err := s.requireBooted(); err != nil {
		return nil, err
	}
	errs, err := s.logErrors(ctx, at)
	if err != nil {
		return nil, err
	}
	for _, e := range errs {
		if e.ID() == id {
			return []byte(e.Text + "\n"), nil
		}
	}
	return nil, target.ErrCrashNotFound
}
