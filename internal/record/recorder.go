// Package record writes a live session's frames to MP4 files and keeps a
// capped store of them.
//
// A recording reads the session through a tap, not as a viewer, so it is not
// paced, shed, counted by the bitrate controller or part of arbitration. It
// holds nothing on the session itself: keeping the session open while a
// recording runs is the caller's job.
package record

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/h264"
	"github.com/bmcreations/loupe/internal/mp4"
	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
)

// Source is the part of a session a recording reads. *session.Session
// satisfies it.
type Source interface {
	AddTap(n int) *session.Tap
	RequestKeyframe() error
	BitrateChanges() []session.BitrateChange
	Done() <-chan struct{}
}

var _ Source = (*session.Session)(nil)

// tapFrames is the tap's capacity: about 8s at 60fps. A tap that fills ends
// the recording as fellBehind rather than leave a hole in the file.
const tapFrames = 512

// Meta describes one recording. It is written beside the file as <id>.json
// when the recording ends.
type Meta struct {
	ID      string     `json:"id"`
	Target  string     `json:"target"`
	Started time.Time  `json:"started"`
	Ended   *time.Time `json:"ended,omitempty"`
	// Reason is why it ended: stopped, limit, resized, fellBehind,
	// sessionEnded or writeError.
	Reason string `json:"reason,omitempty"`
	// Error is set with writeError.
	Error  string `json:"error,omitempty"`
	Frames int    `json:"frames"`
	// Bytes is the file's size once ended, and the sample bytes written so far
	// while in progress.
	Bytes int64 `json:"bytes"`
	// DurationMs is the file's duration: the last sample's PTS plus its
	// duration, which repeats the gap before it.
	DurationMs int64 `json:"durationMs"`
	// FirstKeyMs is how long after Started the opening keyframe arrived, by
	// the host clock. Nothing before it is in the file.
	FirstKeyMs     int64           `json:"firstKeyMs"`
	Width          int             `json:"width,omitempty"`
	Height         int             `json:"height,omitempty"`
	BitrateChanges []BitrateChange `json:"bitrateChanges"`
	InProgress     bool            `json:"inProgress"`
}

// BitrateChange is one encoder bitrate change during the recording.
type BitrateChange struct {
	AtMs int64 `json:"atMs"` // from Started
	From int   `json:"from"`
	To   int   `json:"to"`
}

// sampleWriter is the part of *mp4.Writer a recorder uses, so a test can
// put a failing one in its place.
type sampleWriter interface {
	SetConfig(sps, pps []byte) error
	WriteSample(au []byte, pts time.Duration, key bool) error
	Close() error
}

func createMP4(path string) (sampleWriter, error) { return mp4.Create(path) }

// Recorder writes one recording. Its goroutine owns the writer and the tap.
type Recorder struct {
	store *Store
	src   Source
	tap   *session.Tap
	w     sampleWriter
	path  string
	limit time.Duration
	// maxBytes is the store's cap, which is also the most one recording may
	// write: past it the recording ends as limit.
	maxBytes int64
	onEnd    func(Meta)

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{} // closed after finish, including onEnd

	// Owned by run's goroutine.
	haveConfig bool
	opened     bool
	base       time.Duration // PTS of the opening keyframe
	last       time.Duration // relative PTS of the last sample
	gap        time.Duration // the gap before the last sample

	mu   sync.Mutex
	meta Meta
}

func (r *Recorder) snapshot() Meta {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.meta
	m.BitrateChanges = append([]BitrateChange{}, r.meta.BitrateChanges...)
	return m
}

func (r *Recorder) requestStop() {
	r.stopOnce.Do(func() { close(r.stop) })
}

func (r *Recorder) run() {
	defer r.store.wg.Done()
	defer close(r.done)
	limit := time.NewTimer(r.limit)
	defer limit.Stop()
	for {
		select {
		case f := <-r.tap.C:
			if err := r.handle(f); err != nil {
				r.finish(reasonFor(err), err)
				return
			}
			if r.overCap() {
				r.finish("limit", nil)
				return
			}
		case <-r.tap.Dropped:
			r.end("fellBehind")
			return
		case <-r.stop:
			r.end("stopped")
			return
		case <-limit.C:
			r.end("limit")
			return
		case <-r.src.Done():
			r.end("sessionEnded")
			return
		}
	}
}

// end finishes for a reason that is not a write failure. Frames already in
// the tap arrived before the reason did, so they are written first: select
// picks among ready cases at random, and a stop must not lose them. The tap
// is removed first so the drain ends.
func (r *Recorder) end(reason string) {
	r.tap.Remove()
	for {
		select {
		case f := <-r.tap.C:
			if err := r.handle(f); err != nil {
				r.finish(reasonFor(err), err)
				return
			}
			continue
		default:
		}
		break
	}
	r.finish(reason, nil)
}

func (r *Recorder) overCap() bool {
	if r.maxBytes <= 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.meta.Bytes >= r.maxBytes
}

func reasonFor(err error) string {
	if errors.Is(err, mp4.ErrResized) {
		return "resized"
	}
	return "writeError"
}

func (r *Recorder) handle(f target.Frame) error {
	if f.Config {
		var sps, pps []byte
		h264.SplitAnnexB(f.Data, func(nal []byte) {
			switch h264.Type(nal) {
			case h264.NALSPS:
				sps = nal
			case h264.NALPPS:
				pps = nal
			}
		})
		if sps == nil || pps == nil {
			return nil
		}
		if err := r.w.SetConfig(sps, pps); err != nil {
			return err
		}
		if !r.haveConfig {
			r.haveConfig = true
			if w, h, err := h264.SPSDimensions(sps); err == nil {
				r.mu.Lock()
				r.meta.Width, r.meta.Height = w, h
				r.mu.Unlock()
			}
		}
		return nil
	}
	if !r.opened {
		// A tap can start mid-GOP, and a sample before the parameter sets
		// cannot be written, so the file opens on the first key after a
		// config.
		if !r.haveConfig || !f.Key {
			return nil
		}
		r.opened = true
		r.base = f.PTS
		r.mu.Lock()
		r.meta.FirstKeyMs = time.Since(r.meta.Started).Milliseconds()
		r.mu.Unlock()
	}
	pts := f.PTS - r.base
	if err := r.w.WriteSample(f.Data, pts, f.Key); err != nil {
		return err
	}
	r.mu.Lock()
	if r.meta.Frames > 0 {
		r.gap = pts - r.last
	}
	r.last = pts
	r.meta.Frames++
	r.meta.Bytes += int64(len(f.Data))
	r.mu.Unlock()
	return nil
}

// finish closes the file, writes its metadata and releases the target. The
// tap is removed on every path.
func (r *Recorder) finish(reason string, cause error) {
	r.tap.Remove()
	closeErr := r.w.Close()
	if cause == nil && closeErr != nil {
		reason, cause = "writeError", closeErr
	}
	ended := time.Now().UTC()

	r.mu.Lock()
	m := &r.meta
	m.Ended = &ended
	m.Reason = reason
	if cause != nil && reason == "writeError" {
		m.Error = cause.Error()
	}
	m.InProgress = false
	if fi, err := os.Stat(r.path); err == nil {
		m.Bytes = fi.Size()
	}
	switch {
	case m.Frames == 1:
		m.DurationMs = (time.Second / 60).Milliseconds()
	case m.Frames > 1:
		m.DurationMs = (r.last + max(r.gap, 0)).Milliseconds()
	}
	m.BitrateChanges = []BitrateChange{}
	for _, c := range r.src.BitrateChanges() {
		if c.At.Before(m.Started) {
			continue
		}
		m.BitrateChanges = append(m.BitrateChanges, BitrateChange{
			AtMs: c.At.Sub(m.Started).Milliseconds(), From: c.From, To: c.To,
		})
	}
	final := *m
	r.mu.Unlock()

	// A failed metadata write leaves an .mp4 with no .json, which the next
	// startup deletes as unclosed, and Get and Path answer ErrNotFound for
	// it. The caller still gets the Meta, now saying why.
	if err := writeMeta(r.store.dir, final); err != nil {
		msg := "write metadata: " + err.Error()
		if final.Error != "" {
			msg = final.Error + "; " + msg
		}
		final.Reason, final.Error = "writeError", msg
		r.mu.Lock()
		r.meta.Reason, r.meta.Error = final.Reason, final.Error
		r.mu.Unlock()
	}
	r.store.ended(r)
	if r.onEnd != nil {
		r.onEnd(final)
	}
}

// writeMeta writes <id>.json through a temp file and a rename, so a reader
// never sees half of it.
func writeMeta(dir string, m Meta) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+m.ID+".json.tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if serr := f.Sync(); err == nil {
		err = serr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(dir, m.ID+".json"))
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}
