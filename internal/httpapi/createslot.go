package httpapi

import (
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// createJob is one device creation, as the target list and the options
// response report it.
type createJob struct {
	// id is the pending row's id: avd:<name> on Android, sim-new:<name> on
	// iOS, where no UDID exists yet.
	id       string
	label    string
	platform string // android or ios
	name     string
	started  time.Time
	// progress words the row's reason while the job runs.
	progress func(elapsed time.Duration) string

	done bool
	err  string
	// createdID is the target the success made, avd:<name> or sim:<udid>.
	createdID string
	// warning is a success that is not quite clean; err stays empty.
	warning string
}

// createSlot holds the one creation allowed at a time, server-wide. The
// /api/avds creator and the /api/devices creator both start jobs here, so
// they cannot run together.
type createSlot struct {
	mu  sync.Mutex
	job *createJob
}

// begin claims the slot, or says a creation is running. A finished job, failed
// or not, is replaced.
func (g *createSlot) begin(j *createJob) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job != nil && !g.job.done {
		return errCreating
	}
	g.job = j
	return nil
}

// finish settles a job. A success stays as the last job, so the options
// response can report it done with the id it made, but it has no row.
func (g *createSlot) finish(j *createJob, failure, createdID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	j.done = true
	j.err = failure
	j.createdID = createdID
}

// warn attaches a warning to a finished job.
func (g *createSlot) warn(j *createJob, w string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	j.warning = w
}

// last is a copy of the current or most recent job.
func (g *createSlot) last() (createJob, bool) {
	if g == nil {
		return createJob{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job == nil {
		return createJob{}, false
	}
	return *g.job, true
}

// row is the creation as a target, or false when nothing is in flight or
// failed. A failed one stays until the next creation, so the person sees why;
// a successful one is the real target by then.
func (g *createSlot) row() (TargetInfo, bool) {
	job, ok := g.last()
	if !ok || (job.done && job.err == "") {
		return TargetInfo{}, false
	}
	desc := target.Description{OS: "android", Transport: "emulator"}
	kind := target.KindEmulator
	if job.platform == "ios" {
		desc = target.Description{OS: "ios"}
		kind = target.KindSimulator
	}
	info := TargetInfo{ID: job.id, Kind: string(kind), Label: job.label, Description: desc}
	if job.done {
		info.State = stateCreateFailed
		info.Reason = "Couldn't create: " + job.err
	} else {
		info.State = stateCreating
		if job.progress != nil {
			info.Reason = job.progress(time.Since(job.started))
		}
	}
	return info, true
}

// withCreation adds the creation's row to a target list, in place of an
// entry with the same id, which a failed creation of an existing name would
// otherwise duplicate.
func (g *createSlot) withCreation(targets []TargetInfo) []TargetInfo {
	if g == nil {
		return targets
	}
	row, ok := g.row()
	if !ok {
		return targets
	}
	for _, t := range targets {
		if t.ID == row.ID {
			return targets
		}
	}
	return append(targets, row)
}

// sharedSlot is the server's slot, adopting mine when it has none yet.
func (s *Server) sharedSlot(mine *createSlot) *createSlot {
	if s.slot == nil {
		if mine == nil {
			mine = &createSlot{}
		}
		s.slot = mine
	}
	return s.slot
}
