package record

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrBusy       = errors.New("record: target already recording")
	ErrNotFound   = errors.New("record: no such recording")
	ErrInProgress = errors.New("record: recording in progress")
	ErrClosed     = errors.New("record: store is shutting down")
)

var idPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

// Store keeps recordings in one directory: <id>.mp4 beside <id>.json, the
// json written only once the file is closed. Finished recordings are read
// from the directory on every call, so the directory is the record; only
// recordings in progress are held in memory.
type Store struct {
	dir    string
	cap    int64         // total bytes kept, finished and in progress
	maxAge time.Duration // a finished recording older than this goes at startup
	limit  time.Duration // a recording ends as limit after this long
	create func(string) (sampleWriter, error)

	mu     sync.Mutex
	active map[string]*Recorder // by target
	byID   map[string]*Recorder
	wg     sync.WaitGroup
	closed bool // set by Wait; Start after it is refused, so wg.Add never races wg.Wait
}

// NewStore creates dir if needed and deletes recordings that were never
// closed (an .mp4 with no .json) or ended more than maxAge ago, and any
// metadata temp file a crash left.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{
		dir:    dir,
		cap:    1 << 30,
		maxAge: 7 * 24 * time.Hour,
		limit:  10 * time.Minute,
		create: createMP4,
		active: map[string]*Recorder{},
		byID:   map[string]*Recorder{},
	}
	if err := s.sweep(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) sweep() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-s.maxAge)
	for _, e := range entries {
		name := e.Name()
		switch {
		case strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".json.tmp") &&
			idPattern.MatchString(strings.TrimSuffix(strings.TrimPrefix(name, "."), ".json.tmp")):
			os.Remove(filepath.Join(s.dir, name))
		case strings.HasSuffix(name, ".mp4") && idPattern.MatchString(strings.TrimSuffix(name, ".mp4")):
			if _, err := os.Stat(s.jsonPath(strings.TrimSuffix(name, ".mp4"))); errors.Is(err, os.ErrNotExist) {
				os.Remove(filepath.Join(s.dir, name))
			}
		case strings.HasSuffix(name, ".json") && idPattern.MatchString(strings.TrimSuffix(name, ".json")):
			id := strings.TrimSuffix(name, ".json")
			m, err := s.readMeta(id)
			if err != nil || m.Ended == nil || m.Ended.Before(cutoff) {
				os.Remove(s.mp4Path(id))
				os.Remove(s.jsonPath(id))
			}
		}
	}
	return nil
}

func (s *Store) mp4Path(id string) string  { return filepath.Join(s.dir, id+".mp4") }
func (s *Store) jsonPath(id string) string { return filepath.Join(s.dir, id+".json") }

func (s *Store) readMeta(id string) (Meta, error) {
	b, err := os.ReadFile(s.jsonPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return Meta{}, ErrNotFound
	}
	if err != nil {
		return Meta{}, err
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return Meta{}, err
	}
	return m, nil
}

// finished reads every finished recording's metadata from the directory,
// leaving out any id still held by a recorder.
func (s *Store) finished() []Meta {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var out []Meta
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !idPattern.MatchString(id) {
			continue
		}
		if _, live := s.byID[id]; live {
			continue
		}
		if m, err := s.readMeta(id); err == nil {
			out = append(out, m)
		}
	}
	return out
}

func newID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Start begins recording src for targetID. onEnd runs once, on the
// recorder's goroutine, after the file and its metadata are written.
func (s *Store) Start(src Source, targetID string, onEnd func(Meta)) (Meta, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Meta{}, ErrClosed
	}
	if _, busy := s.active[targetID]; busy {
		s.mu.Unlock()
		return Meta{}, ErrBusy
	}
	var id string
	for {
		var err error
		if id, err = newID(); err != nil {
			s.mu.Unlock()
			return Meta{}, err
		}
		_, live := s.byID[id]
		_, statErr := os.Stat(s.mp4Path(id))
		if !live && errors.Is(statErr, os.ErrNotExist) {
			break
		}
	}
	r := &Recorder{
		store:    s,
		src:      src,
		path:     s.mp4Path(id),
		limit:    s.limit,
		maxBytes: s.cap,
		onEnd:    onEnd,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		meta: Meta{
			ID:             id,
			Target:         targetID,
			Started:        time.Now().UTC(),
			BitrateChanges: []BitrateChange{},
			InProgress:     true,
		},
	}
	// Reserve the target before creating anything, so a second Start is
	// refused while this one opens its file.
	s.active[targetID] = r
	s.byID[id] = r
	s.wg.Add(1)
	s.mu.Unlock()

	w, err := s.create(r.path)
	if err != nil {
		os.Remove(r.path)
		s.ended(r)
		close(r.done)
		s.wg.Done()
		return Meta{}, err
	}
	r.w = w
	r.tap = src.AddTap(tapFrames)
	// The tap replays the last config, but the next keyframe could be a
	// second away; asking for one shortens the skipped lead-in. A failure
	// only means waiting for the encoder's own.
	_ = src.RequestKeyframe()
	first := r.snapshot()
	go r.run()
	s.enforce()
	return first, nil
}

// ended releases r's target and id, then applies the cap.
func (s *Store) ended(r *Recorder) {
	s.mu.Lock()
	if s.active[r.meta.Target] == r {
		delete(s.active, r.meta.Target)
	}
	if s.byID[r.meta.ID] == r {
		delete(s.byID, r.meta.ID)
	}
	s.mu.Unlock()
	s.enforce()
}

// Stop ends a recording and waits for its file to close. Stopping one that
// already ended returns its metadata.
func (s *Store) Stop(id string) (Meta, error) {
	if !idPattern.MatchString(id) {
		return Meta{}, ErrNotFound
	}
	s.mu.Lock()
	r := s.byID[id]
	s.mu.Unlock()
	if r == nil {
		return s.readMeta(id)
	}
	r.requestStop()
	<-r.done
	m := r.snapshot()
	if m.Ended == nil { // its file could not be created
		return Meta{}, ErrNotFound
	}
	return m, nil
}

// Get returns a recording's metadata, in progress or finished.
func (s *Store) Get(id string) (Meta, error) {
	if !idPattern.MatchString(id) {
		return Meta{}, ErrNotFound
	}
	s.mu.Lock()
	r := s.byID[id]
	s.mu.Unlock()
	if r != nil {
		return r.snapshot(), nil
	}
	return s.readMeta(id)
}

// List returns every recording, newest first, the active ones included.
func (s *Store) List() []Meta {
	s.mu.Lock()
	out := s.finished()
	for _, r := range s.byID {
		out = append(out, r.snapshot())
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out
}

// Path returns a finished recording's file.
func (s *Store) Path(id string) (string, error) {
	if !idPattern.MatchString(id) {
		return "", ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byID[id] != nil {
		return "", ErrInProgress
	}
	if _, err := os.Stat(s.jsonPath(id)); err != nil {
		return "", ErrNotFound
	}
	if _, err := os.Stat(s.mp4Path(id)); err != nil {
		return "", ErrNotFound
	}
	return s.mp4Path(id), nil
}

// Delete removes a finished recording and its metadata.
func (s *Store) Delete(id string) error {
	if !idPattern.MatchString(id) {
		return ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byID[id] != nil {
		return ErrInProgress
	}
	errJSON := os.Remove(s.jsonPath(id))
	errMP4 := os.Remove(s.mp4Path(id))
	if errors.Is(errJSON, os.ErrNotExist) && errors.Is(errMP4, os.ErrNotExist) {
		return ErrNotFound
	}
	if errJSON != nil && !errors.Is(errJSON, os.ErrNotExist) {
		return errJSON
	}
	if errMP4 != nil && !errors.Is(errMP4, os.ErrNotExist) {
		return errMP4
	}
	return nil
}

// ActiveFor returns the recording in progress on a target.
func (s *Store) ActiveFor(targetID string) (Meta, bool) {
	s.mu.Lock()
	r := s.active[targetID]
	s.mu.Unlock()
	if r == nil {
		return Meta{}, false
	}
	return r.snapshot(), true
}

// Wait reports whether every recorder finished within timeout. It does not
// stop them; a caller shutting down stops each first. It also closes the
// store to new recordings: Start answers ErrClosed from here on.
func (s *Store) Wait(timeout time.Duration) bool {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// enforce deletes the oldest finished recordings until the finished files
// and the bytes written so far by every recorder fit under cap. A recording
// in progress is counted and never deleted.
func (s *Store) enforce() {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int64
	for _, r := range s.byID {
		total += r.snapshot().Bytes
	}
	type file struct {
		id      string
		started time.Time
		size    int64
	}
	var files []file
	for _, m := range s.finished() {
		var size int64
		if fi, err := os.Stat(s.mp4Path(m.ID)); err == nil {
			size = fi.Size()
		}
		total += size
		files = append(files, file{m.ID, m.Started, size})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].started.Before(files[j].started) })
	for _, f := range files {
		if total <= s.cap {
			return
		}
		os.Remove(s.mp4Path(f.id))
		os.Remove(s.jsonPath(f.id))
		total -= f.size
	}
}
