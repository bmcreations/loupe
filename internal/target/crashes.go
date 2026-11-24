package target

import (
	"context"
	"errors"
	"regexp"
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
func ValidCrashID(s string) bool { return crashIDRE.MatchString(s) || DropboxIDRE.MatchString(s) }
