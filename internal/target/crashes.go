package target

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Crasher is implemented by targets whose crash reports can be read. It is
// optional the way Logcatter is.
type Crasher interface {
	// Crashes lists the target's crash reports, newest first.
	Crashes(ctx context.Context, q CrashQuery) ([]CrashReport, error)
	// Crash returns one report as it was written. A report that does not
	// exist and one that belongs to another target are both ErrCrashNotFound.
	Crash(ctx context.Context, id string) ([]byte, error)
}

// CrashQuery narrows what Crashes returns.
type CrashQuery struct {
	// Since drops reports of crashes before it. Zero keeps them all.
	Since time.Time
	// Limit is how many of the newest reports to return.
	Limit int
}

// CrashReport is one row of a crash listing: enough to tell crashes apart
// and pick one, without the threads.
type CrashReport struct {
	// ID is what Crash takes: on a simulator the report's incident id, a
	// UUID, and on Android the dropbox tag and the entry's epoch millis,
	// "data_app_crash-1792722095531".
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Time is when the process crashed, which was 22s and 36s before
	// the report was written.
	Time    time.Time `json:"time"`
	Process string    `json:"process"`
	// BundleID is empty for a process without a bundle, such as a daemon.
	// On Android it is the package, Version its versionName and Build its
	// versionCode.
	BundleID string `json:"bundleID,omitempty"`
	Version  string `json:"version,omitempty"`
	Build    string `json:"build,omitempty"`
	PID      int    `json:"pid,omitempty"`
	// Summary is the exception and the termination, e.g.
	// "EXC_CRASH (SIGABRT), Abort trap: 6".
	Summary string `json:"summary"`
}

// ErrCrashNotFound is a crash id this target has no report for.
var ErrCrashNotFound = errors.New("no such crash report")

var crashIDRE = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)

// ValidCrashID reports whether s is shaped like a crash id: a simulator's
// incident id, or an Android dropbox entry's tag and millis.
func ValidCrashID(s string) bool { return crashIDRE.MatchString(s) || dropboxIDRE.MatchString(s) }

// bugTypeCrash is the .ips bug_type of a crash. The directory also holds
// other kinds (226 is a .diag of something else entirely), and only this
// one has been seen from a simulator's process.
const bugTypeCrash = "309"

// DiagnosticReportsDir is where macOS writes crash reports, a simulator's
// processes included: they are processes on the Mac.
func DiagnosticReportsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Logs", "DiagnosticReports")
}

// ipsHeader is the first line of an .ips file, a JSON document of its own.
type ipsHeader struct {
	BugType    string `json:"bug_type"`
	IncidentID string `json:"incident_id"`
	AppVersion string `json:"app_version"`
	Build      string `json:"build_version"`
	BundleID   string `json:"bundleID"`
}

// ipsBody is the part of the second document a listing reads.
type ipsBody struct {
	CaptureTime   string `json:"captureTime"`
	ProcName      string `json:"procName"`
	PID           int    `json:"pid"`
	CoalitionName string `json:"coalitionName"`
	Exception     struct {
		Type   string `json:"type"`
		Signal string `json:"signal"`
	} `json:"exception"`
	Termination struct {
		Indicator string `json:"indicator"`
	} `json:"termination"`
}

// ipsTimeLayout is captureTime's layout, "2026-10-22 19:02:07.9999 -0400".
const ipsTimeLayout = "2006-01-02 15:04:05.999999999 -0700"

// simCoalition is the coalition every process of one simulator runs in.
func simCoalition(udid string) string { return "com.apple.CoreSimulator.SimDevice." + udid }

// reportDirs are the directories a report can be in. macOS moves reports
// into Retired: on 22 October, 235 of the 239 crash reports here were
// there, a simulator's among them.
func reportDirs(dir string) []string { return []string{dir, filepath.Join(dir, "Retired")} }

// parseIPS reads a report's header and body. ok is false for anything that
// is not a crash report of the coalition given; an empty coalition takes
// any, for a directory that holds one device's reports only.
func parseIPS(raw []byte, coalition string) (h ipsHeader, b ipsBody, ok bool) {
	line, rest, found := bytes.Cut(raw, []byte("\n"))
	if !found || json.Unmarshal(line, &h) != nil || h.BugType != bugTypeCrash || !ValidCrashID(h.IncidentID) {
		return h, b, false
	}
	if json.Unmarshal(rest, &b) != nil || (coalition != "" && b.CoalitionName != coalition) {
		return h, b, false
	}
	return h, b, true
}

func (b ipsBody) summary() string {
	var parts []string
	if e := b.Exception.Type; e != "" {
		if b.Exception.Signal != "" {
			e += " (" + b.Exception.Signal + ")"
		}
		parts = append(parts, e)
	}
	if b.Termination.Indicator != "" {
		parts = append(parts, b.Termination.Indicator)
	}
	return strings.Join(parts, ", ")
}

// listCrashes reads the crash reports in dir and its Retired directory
// whose processes ran in coalition.
//
// A file older than Since is skipped by its mtime before it is read, which
// is safe because the report is written after the crash. The coalition is
// matched as bytes before anything is decoded, since most of the directory
// is the Mac's own processes and other simulators'.
func listCrashes(ctx context.Context, dir, coalition string, q CrashQuery) ([]CrashReport, error) {
	needle := []byte(coalition)
	var out []CrashReport
	for _, d := range reportDirs(dir) {
		entries, err := os.ReadDir(d)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".ips") {
				continue
			}
			if !q.Since.IsZero() {
				if fi, err := e.Info(); err != nil || fi.ModTime().Before(q.Since) {
					continue
				}
			}
			raw, err := os.ReadFile(filepath.Join(d, e.Name()))
			if err != nil || (len(needle) > 0 && !bytes.Contains(raw, needle)) {
				continue
			}
			h, b, ok := parseIPS(raw, coalition)
			if !ok {
				continue
			}
			at, err := time.Parse(ipsTimeLayout, b.CaptureTime)
			if err != nil || (!q.Since.IsZero() && at.Before(q.Since)) {
				continue
			}
			out = append(out, CrashReport{
				ID: strings.ToUpper(h.IncidentID), Kind: "crash", Time: at,
				Process: b.ProcName, BundleID: h.BundleID,
				Version: h.AppVersion, Build: h.Build, PID: b.PID,
				Summary: b.summary(),
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// readCrash returns the report with incident id, if its process ran in
// coalition. Only the first line of each file is read until the id
// matches, and the coalition is checked on the file before it is returned,
// because the directory holds every simulator's reports and the Mac's.
func readCrash(dir, coalition, id string) ([]byte, error) {
	if !ValidCrashID(id) {
		return nil, fmt.Errorf("%w: %q is not a crash id", ErrCrashNotFound, id)
	}
	for _, d := range reportDirs(dir) {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".ips") {
				continue
			}
			path := filepath.Join(d, e.Name())
			var h ipsHeader
			if json.Unmarshal(firstLine(path), &h) != nil || !strings.EqualFold(h.IncidentID, id) {
				continue
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			if _, _, ok := parseIPS(raw, coalition); !ok {
				return nil, ErrCrashNotFound
			}
			return raw, nil
		}
	}
	return nil, ErrCrashNotFound
}

// firstLine is a file's first line, at most 64KB of it.
func firstLine(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4096), 64<<10)
	if !sc.Scan() {
		return nil
	}
	return sc.Bytes()
}

// Crashes lists the simulator's crash reports. They are files on the Mac,
// so a shut-down simulator's are listed without booting it.
func (s *Simulator) Crashes(ctx context.Context, q CrashQuery) ([]CrashReport, error) {
	return listCrashes(ctx, DiagnosticReportsDir(), simCoalition(s.UDID()), q)
}

// Crash returns one of the simulator's crash reports as macOS wrote it.
func (s *Simulator) Crash(_ context.Context, id string) ([]byte, error) {
	return readCrash(DiagnosticReportsDir(), simCoalition(s.UDID()), id)
}
