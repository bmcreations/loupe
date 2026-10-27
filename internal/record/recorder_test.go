package record

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/mp4"
	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
)

// Parameter sets from a one-frame libx264 baseline encode of testsrc at
// 320x240, and the same SPS at 240x320 (copied from internal/mp4's tests).
var (
	sps320x240 = []byte{0x67, 0x42, 0xc0, 0x15, 0xd9, 0x01, 0x41, 0xfb, 0x01, 0x10, 0x00, 0x00, 0x03, 0x00, 0x10, 0x00, 0x00, 0x07, 0x80, 0xf1, 0x62, 0xe4, 0x80}
	spsTall    = []byte{0x67, 0x42, 0xc0, 0x15, 0xd9, 0x03, 0xc2, 0x9b, 0x01, 0x10, 0x00, 0x00, 0x03, 0x00, 0x10, 0x00, 0x00, 0x07, 0x80, 0xf1, 0x62, 0xe4, 0x80}
	pps        = []byte{0x68, 0xcb, 0x83, 0xcb, 0x20}
)

func annexB(nals ...[]byte) []byte {
	var b []byte
	for _, n := range nals {
		b = append(b, 0, 0, 0, 1)
		b = append(b, n...)
	}
	return b
}

func cfgFrame(sps []byte) target.Frame {
	return target.Frame{Config: true, Data: annexB(sps, pps)}
}

func keyFrame(pts time.Duration) target.Frame {
	return target.Frame{Key: true, PTS: pts, Data: annexB([]byte{0x65, 0xAA, 0xAA, 0xAA})}
}

func interFrame(pts time.Duration) target.Frame {
	return target.Frame{PTS: pts, Data: annexB([]byte{0x41, 0xBB, 0xBB})}
}

// fakeSource is a session with no stream: frames go in through send.
type fakeSource struct {
	size      int // tap capacity; 0 means what AddTap asks for
	tap       *session.Tap
	send      func(target.Frame)
	keyframes atomic.Int32
	changes   []session.BitrateChange
	done      chan struct{}
	ready     chan struct{} // closed once AddTap has run
	once      sync.Once
}

func newFakeSource() *fakeSource {
	return &fakeSource{done: make(chan struct{}), ready: make(chan struct{})}
}

func (f *fakeSource) AddTap(n int) *session.Tap {
	if f.size > 0 {
		n = f.size
	}
	f.tap, f.send = session.NewTap(n)
	f.once.Do(func() { close(f.ready) })
	return f.tap
}

func (f *fakeSource) RequestKeyframe() error {
	f.keyframes.Add(1)
	return nil
}

func (f *fakeSource) BitrateChanges() []session.BitrateChange { return f.changes }
func (f *fakeSource) Done() <-chan struct{}                   { return f.done }

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	must(t, err)
	// Registered first, so it runs after every start's Stop.
	t.Cleanup(func() {
		if !s.Wait(5 * time.Second) {
			t.Error("recorders still running at cleanup")
		}
	})
	return s
}

// start begins a recording and returns its first Meta and a channel that
// receives the one onEnd call.
func start(t *testing.T, s *Store, src Source, targetID string) (Meta, <-chan Meta) {
	t.Helper()
	ended := make(chan Meta, 1)
	m, err := s.Start(src, targetID, func(m Meta) { ended <- m })
	must(t, err)
	t.Cleanup(func() { s.Stop(m.ID) })
	return m, ended
}

func waitEnd(t *testing.T, ended <-chan Meta) Meta {
	t.Helper()
	select {
	case m := <-ended:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("recording never ended")
		return Meta{}
	}
}

func samples(t *testing.T, s *Store, id string) mp4.Samples {
	t.Helper()
	path, err := s.Path(id)
	must(t, err)
	got, err := mp4.ReadSamples(path)
	must(t, err)
	return got
}

func TestRecordingOpensOnTheFirstKeyframe(t *testing.T) {
	s := newStore(t)
	src := newFakeSource()
	m, _ := start(t, s, src, "t1")
	src.send(cfgFrame(sps320x240))
	src.send(interFrame(0))
	src.send(keyFrame(16 * time.Millisecond))
	src.send(interFrame(33 * time.Millisecond))
	src.send(interFrame(50 * time.Millisecond))
	end, err := s.Stop(m.ID)
	must(t, err)

	got := samples(t, s, m.ID)
	if len(got.Durations) != 3 {
		t.Fatalf("%d samples, want 3", len(got.Durations))
	}
	if len(got.Keys) != 1 || got.Keys[0] != 1 {
		t.Fatalf("key samples %v, want [1]", got.Keys)
	}
	if end.Frames != 3 || end.Reason != "stopped" || end.InProgress || end.Ended == nil {
		t.Fatalf("meta %+v", end)
	}
	if end.Width != 320 || end.Height != 240 {
		t.Fatalf("size %dx%d", end.Width, end.Height)
	}
	fi, err := os.Stat(filepath.Join(s.dir, m.ID+".mp4"))
	must(t, err)
	if end.Bytes != fi.Size() {
		t.Fatalf("bytes %d, file %d", end.Bytes, fi.Size())
	}
	if n := src.keyframes.Load(); n != 1 {
		t.Fatalf("RequestKeyframe called %d times, want 1", n)
	}
}

func TestPTSIsRelativeToTheOpeningKeyframe(t *testing.T) {
	s := newStore(t)
	src := newFakeSource()
	m, _ := start(t, s, src, "t1")
	src.send(cfgFrame(sps320x240))
	src.send(keyFrame(5 * time.Second))
	src.send(interFrame(5*time.Second + 16700*time.Microsecond))
	end, err := s.Stop(m.ID)
	must(t, err)
	got := samples(t, s, m.ID)
	if len(got.Durations) != 2 || got.Durations[0] != 1503 || got.Durations[1] != 1503 {
		t.Fatalf("durations %v, want [1503 1503]", got.Durations)
	}
	if end.DurationMs != 33 {
		t.Fatalf("durationMs %d, want 33", end.DurationMs)
	}
	if end.FirstKeyMs < 0 || end.FirstKeyMs > 5000 {
		t.Fatalf("firstKeyMs %d", end.FirstKeyMs)
	}
}

func TestDroppedEndsAsFellBehind(t *testing.T) {
	s := newStore(t)
	src := newFakeSource()
	src.size = 3
	m, ended := start(t, s, src, "t1")
	// Nothing reads the tap until the recorder's goroutine runs, so fill it
	// before it can: three fit, the fourth overflows.
	<-src.ready
	src.send(cfgFrame(sps320x240))
	src.send(keyFrame(0))
	src.send(interFrame(16 * time.Millisecond))
	for i := 0; ; i++ {
		src.send(interFrame(time.Duration(i+2) * 16 * time.Millisecond))
		select {
		case <-src.tap.Dropped:
		default:
			if i < 100000 {
				continue
			}
			t.Fatal("tap never overflowed")
		}
		break
	}
	end := waitEnd(t, ended)
	if end.Reason != "fellBehind" || end.Error != "" {
		t.Fatalf("reason %q error %q, want fellBehind", end.Reason, end.Error)
	}
	got := samples(t, s, m.ID)
	if len(got.Durations) == 0 || len(got.Durations) != end.Frames {
		t.Fatalf("%d samples, meta says %d", len(got.Durations), end.Frames)
	}
}

func TestResizeEndsAsResized(t *testing.T) {
	s := newStore(t)
	src := newFakeSource()
	m, ended := start(t, s, src, "t1")
	src.send(cfgFrame(sps320x240))
	src.send(keyFrame(0))
	src.send(interFrame(16 * time.Millisecond))
	src.send(cfgFrame(spsTall))
	src.send(keyFrame(33 * time.Millisecond))
	end := waitEnd(t, ended)
	if end.Reason != "resized" || end.Error != "" {
		t.Fatalf("reason %q error %q, want resized", end.Reason, end.Error)
	}
	got := samples(t, s, m.ID)
	if len(got.Durations) != 2 || got.Width != 320 || got.Height != 240 {
		t.Fatalf("samples %+v", got)
	}
}

func TestSessionDoneEndsAsSessionEnded(t *testing.T) {
	s := newStore(t)
	src := newFakeSource()
	m, ended := start(t, s, src, "t1")
	src.send(cfgFrame(sps320x240))
	src.send(keyFrame(0))
	close(src.done)
	end := waitEnd(t, ended)
	if end.Reason != "sessionEnded" {
		t.Fatalf("reason %q, want sessionEnded", end.Reason)
	}
	if got := samples(t, s, m.ID); len(got.Durations) != 1 {
		t.Fatalf("%d samples, want 1", len(got.Durations))
	}
	if _, ok := s.ActiveFor("t1"); ok {
		t.Fatal("still active after its end")
	}
}

func TestLimitEndsAsLimit(t *testing.T) {
	s := newStore(t)
	s.limit = 50 * time.Millisecond
	src := newFakeSource()
	_, ended := start(t, s, src, "t1")
	if end := waitEnd(t, ended); end.Reason != "limit" {
		t.Fatalf("reason %q, want limit", end.Reason)
	}
}

// The cap is also the limit for one recording, checked as samples are
// written, so a high bitrate cannot run one recording far past it.
func TestCapEndsARecordingAsLimit(t *testing.T) {
	s := newStore(t)
	s.cap = 100
	src := newFakeSource()
	_, ended := start(t, s, src, "t1")
	<-src.ready
	src.send(cfgFrame(sps320x240))
	src.send(keyFrame(0))
	for i := 1; i < 200; i++ {
		src.send(interFrame(time.Duration(i) * time.Second / 60))
	}
	end := waitEnd(t, ended)
	if end.Reason != "limit" {
		t.Fatalf("reason %q, want limit", end.Reason)
	}
	if end.Frames >= 200 {
		t.Fatalf("%d frames written, want it stopped near the cap", end.Frames)
	}
}

type failingWriter struct{ closed atomic.Bool }

func (w *failingWriter) SetConfig(sps, pps []byte) error { return nil }
func (w *failingWriter) WriteSample([]byte, time.Duration, bool) error {
	return errors.New("disk full")
}
func (w *failingWriter) Close() error { w.closed.Store(true); return nil }

func TestWriteErrorEndsAsWriteError(t *testing.T) {
	s := newStore(t)
	fw := &failingWriter{}
	s.create = func(path string) (sampleWriter, error) {
		return fw, os.WriteFile(path, nil, 0o644)
	}
	src := newFakeSource()
	_, ended := start(t, s, src, "t1")
	src.send(cfgFrame(sps320x240))
	src.send(keyFrame(0))
	end := waitEnd(t, ended)
	if end.Reason != "writeError" || end.Error != "disk full" {
		t.Fatalf("reason %q error %q, want writeError / disk full", end.Reason, end.Error)
	}
	if !fw.closed.Load() {
		t.Fatal("writer not closed")
	}
	if src.tap != nil {
		// The tap is gone: a send after the end must not reach anything.
		src.send(interFrame(0))
		if len(src.tap.C) != 0 {
			t.Fatal("tap still registered after the end")
		}
	}
}

func TestConfigAfterTheTapStillRecords(t *testing.T) {
	s := newStore(t)
	src := newFakeSource()
	m, _ := start(t, s, src, "t1")
	src.send(interFrame(0))
	src.send(interFrame(16 * time.Millisecond))
	src.send(cfgFrame(sps320x240))
	src.send(keyFrame(33 * time.Millisecond))
	end, err := s.Stop(m.ID)
	must(t, err)
	if got := samples(t, s, m.ID); len(got.Durations) != 1 || end.Frames != 1 {
		t.Fatalf("%d samples, meta %d, want 1", len(got.Durations), end.Frames)
	}
}

// A keyframe before any config cannot be written, so the file opens on the
// first key after one.
func TestKeyBeforeConfigIsSkipped(t *testing.T) {
	s := newStore(t)
	src := newFakeSource()
	m, _ := start(t, s, src, "t1")
	src.send(keyFrame(0))
	src.send(cfgFrame(sps320x240))
	src.send(interFrame(16 * time.Millisecond))
	src.send(keyFrame(33 * time.Millisecond))
	end, err := s.Stop(m.ID)
	must(t, err)
	if end.Reason != "stopped" || end.Frames != 1 {
		t.Fatalf("meta %+v", end)
	}
}

func TestBitrateChangesAreFilteredAndRelative(t *testing.T) {
	s := newStore(t)
	src := newFakeSource()
	m, _ := start(t, s, src, "t1")
	src.changes = []session.BitrateChange{
		{At: m.Started.Add(-time.Second), From: 8000000, To: 4000000},
		{At: m.Started.Add(1500 * time.Millisecond), From: 4000000, To: 2000000},
	}
	end, err := s.Stop(m.ID)
	must(t, err)
	want := []BitrateChange{{AtMs: 1500, From: 4000000, To: 2000000}}
	if len(end.BitrateChanges) != 1 || end.BitrateChanges[0] != want[0] {
		t.Fatalf("changes %+v, want %+v", end.BitrateChanges, want)
	}
}

func TestMetadataWriteFailureIsReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	s := newStore(t)
	s.create = func(path string) (sampleWriter, error) {
		return &failingWriter{}, os.WriteFile(path, nil, 0o644)
	}
	src := newFakeSource()
	m, ended := start(t, s, src, "t1")
	// The file exists; the directory now refuses the metadata temp file.
	must(t, os.Chmod(s.dir, 0o500))
	t.Cleanup(func() { os.Chmod(s.dir, 0o755) })
	close(src.done)
	end := waitEnd(t, ended)
	if end.Reason != "writeError" || !strings.Contains(end.Error, "write metadata") {
		t.Fatalf("reason %q error %q, want writeError naming the metadata write", end.Reason, end.Error)
	}
	if _, err := s.Get(m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get: %v, want ErrNotFound", err)
	}
}
