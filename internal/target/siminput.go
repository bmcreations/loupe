package target

import (
	"context"
	"time"
)

// Reclaimer is implemented by a target whose input something else can take
// away and this server can take back. /api/input's "reclaim" action calls it.
type Reclaimer interface {
	Reclaim(ctx context.Context) error
	// LastReclaim reports the most recent reclaim that restarted SpringBoard,
	// so a caller can tell a press that paid for one from a slow press.
	LastReclaim() (ReclaimRecord, bool)
}

// ReclaimRecord is one reclaim: when it finished, how long it took, and the
// app it reopened, "" when the Home Screen was in front.
type ReclaimRecord struct {
	At       time.Time
	Took     time.Duration
	Reopened string
}
