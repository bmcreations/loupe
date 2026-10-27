package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/record"
	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
	"github.com/bmcreations/loupe/internal/trace"
)

// Parameter sets from a one-frame libx264 baseline encode of testsrc at
// 320x240 (copied from internal/record's tests).
var (
	recSPS = []byte{0x67, 0x42, 0xc0, 0x15, 0xd9, 0x01, 0x41, 0xfb, 0x01, 0x10, 0x00, 0x00, 0x03, 0x00, 0x10, 0x00, 0x00, 0x07, 0x80, 0xf1, 0x62, 0xe4, 0x80}
	recPPS = []byte{0x68, 0xcb, 0x83, 0xcb, 0x20}
)

func recAnnexB(nals ...[]byte) []byte {
	var b []byte
	for _, n := range nals {
		b = append(b, 0, 0, 0, 1)
		b = append(b, n...)
	}
	return b
}

// recordTarget is stubTarget that keeps the last stream it opened, so a test
// can send frames on it.
type recordTarget struct {
	mu   sync.Mutex
	last *stubStream
}

func (t *recordTarget) ID() string        { return "rec" }
func (t *recordTarget) Kind() target.Kind { return target.KindSynthetic }
func (t *recordTarget) Label() string     { return "rec" }
func (t *recordTarget) Open(ctx context.Context, opts target.Options) (target.Stream, error) {
	s := &stubStream{frames: make(chan target.Frame)}
	t.mu.Lock()
	t.last = s
	t.mu.Unlock()
	return s, nil
}

func (t *recordTarget) stream() *stubStream {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.last
}

// feed sends a config and n frames, the first a keyframe, 16ms apart.
func (t *recordTarget) feed(tb testing.TB, n int) {
	tb.Helper()
	s := t.stream()
	if s == nil {
		tb.Fatal("no stream opened")
	}
	send := func(f target.Frame) {
		select {
		case s.frames <- f:
		case <-time.After(2 * time.Second):
			tb.Fatal("stream not read")
		}
	}
	send(target.Frame{Config: true, Data: recAnnexB(recSPS, recPPS)})
	for i := 0; i < n; i++ {
		f := target.Frame{PTS: time.Duration(i) * 16 * time.Millisecond, Data: recAnnexB([]byte{0x41, 0xBB, 0xBB})}
		if i == 0 {
			f.Key = true
			f.Data = recAnnexB([]byte{0x65, 0xAA, 0xAA, 0xAA})
		}
		send(f)
	}
}

func newRecordServer(t *testing.T, tw *trace.Writer) (*Server, *recordTarget, *record.Store) {
	t.Helper()
	m := NewManager(target.Options{})
	m.SetTrace(tw)
	tgt := &recordTarget{}
	m.Register(tgt)
	st, err := record.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{mgr: m, trace: tw}
	s.SetRecordings(st)
	t.Cleanup(func() {
		for _, r := range st.List() {
			if r.InProgress {
				_, _ = st.Stop(r.ID)
			}
		}
		m.Shutdown()
	})
	return s, tgt, st
}

func doRecord(s *Server, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func startRecording(t *testing.T, s *Server) string {
	t.Helper()
	w := doRecord(s, http.MethodPost, "/api/record?target=rec", nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	var body RecordStartResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.ID == "" {
		t.Fatalf("start body %q: %v", w.Body.String(), err)
	}
	return body.ID
}

// waitFrames polls until the recorder has written n frames: a frame the
// stream handed over may still be between the pump and the tap.
func waitFrames(t *testing.T, st *record.Store, id string, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m, err := st.Get(id); err == nil && m.Frames >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("recording %s never reached %d frames", id, n)
}

func stopRecording(t *testing.T, s *Server, id string) record.Meta {
	t.Helper()
	w := doRecord(s, http.MethodPost, "/api/record/"+id+"/stop", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("stop: %d %s", w.Code, w.Body.String())
	}
	var m record.Meta
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func errCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var e ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	return e.Code
}

func TestRecordWithoutAStoreIs501(t *testing.T) {
	s := &Server{mgr: NewManager(target.Options{})}
	for _, p := range []string{"/api/record", "/api/record/0123456789ab"} {
		if w := doRecord(s, http.MethodGet, p, nil); w.Code != http.StatusNotImplemented {
			t.Errorf("GET %s = %d; want 501", p, w.Code)
		}
	}
}

func TestRecordStartStopAndFetch(t *testing.T) {
	s, tgt, st := newRecordServer(t, nil)
	id := startRecording(t, s)
	tgt.feed(t, 5)
	waitFrames(t, st, id, 5)
	m := stopRecording(t, s, id)
	if m.Frames != 5 || m.Reason != "stopped" || m.ID != id {
		t.Fatalf("stop meta = %+v; want 5 frames, stopped", m)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/record/"+id+".mp4", nil)
	req.Header.Set("Range", "bytes=0-7")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusPartialContent {
		t.Fatalf("range GET = %d; want 206", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "video/mp4" {
		t.Errorf("Content-Type = %q", ct)
	}
	b := w.Body.Bytes()
	if len(b) != 8 || string(b[4:8]) != "ftyp" || b[0] != 0 || b[1] != 0 || b[2] != 0 || b[3] == 0 {
		t.Fatalf("first 8 bytes = % x; want an ftyp box header", b)
	}

	// Listed, newest first.
	w = doRecord(s, http.MethodGet, "/api/record", nil)
	var list RecordListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list) != 1 || list[0].ID != id {
		t.Fatalf("list = %s (%v)", w.Body.String(), err)
	}
	// And by id.
	w = doRecord(s, http.MethodGet, "/api/record/"+id, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"frames":5`) {
		t.Fatalf("GET meta = %d %s", w.Code, w.Body.String())
	}
}

func TestRecordListIsAnArrayWhenEmpty(t *testing.T) {
	s, _, _ := newRecordServer(t, nil)
	w := doRecord(s, http.MethodGet, "/api/record", nil)
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("empty list = %q; want []", w.Body.String())
	}
}

func TestStopTwiceReturnsTheMetadata(t *testing.T) {
	s, tgt, st := newRecordServer(t, nil)
	id := startRecording(t, s)
	tgt.feed(t, 3)
	waitFrames(t, st, id, 3)
	first := stopRecording(t, s, id)
	second := stopRecording(t, s, id)
	if second.Frames != first.Frames || second.Reason != "stopped" {
		t.Fatalf("second stop = %+v; want %+v", second, first)
	}
}

func TestSecondStartIs409Recording(t *testing.T) {
	s, _, _ := newRecordServer(t, nil)
	id := startRecording(t, s)
	w := doRecord(s, http.MethodPost, "/api/record?target=rec", nil)
	if w.Code != http.StatusConflict || errCode(t, w) != ErrCodeRecording {
		t.Fatalf("second start = %d %s; want 409 recording", w.Code, w.Body.String())
	}
	var e ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.ID != id || !strings.Contains(e.Error, id) {
		t.Fatalf("409 id = %q, error %q; want id %s", e.ID, e.Error, id)
	}
}

func TestRecordStartNeedsATarget(t *testing.T) {
	s, _, _ := newRecordServer(t, nil)
	if w := doRecord(s, http.MethodPost, "/api/record", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("no target = %d; want 400", w.Code)
	}
	if w := doRecord(s, http.MethodPost, "/api/record?target=nope", nil); w.Code < 400 {
		t.Fatalf("unknown target = %d; want an error", w.Code)
	}
}

func TestFetchAndDeleteWhileRecordingAre409(t *testing.T) {
	s, _, _ := newRecordServer(t, nil)
	id := startRecording(t, s)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/record/" + id + ".mp4"},
		{http.MethodDelete, "/api/record/" + id},
		{http.MethodGet, "/api/record/" + id + "/frames?from=0&to=1"},
	} {
		w := doRecord(s, c.method, c.path, nil)
		if w.Code != http.StatusConflict || errCode(t, w) != ErrCodeRecording {
			t.Errorf("%s %s = %d %s; want 409 recording", c.method, c.path, w.Code, w.Body.String())
		}
	}
}

func TestDeleteIs204ThenGetIs404(t *testing.T) {
	s, tgt, st := newRecordServer(t, nil)
	id := startRecording(t, s)
	tgt.feed(t, 2)
	waitFrames(t, st, id, 2)
	stopRecording(t, s, id)
	if w := doRecord(s, http.MethodDelete, "/api/record/"+id, nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s; want 204", w.Code, w.Body.String())
	}
	for _, p := range []string{"/api/record/" + id, "/api/record/" + id + ".mp4"} {
		if w := doRecord(s, http.MethodGet, p, nil); w.Code != http.StatusNotFound {
			t.Errorf("GET %s after delete = %d; want 404", p, w.Code)
		}
	}
}

func TestRecordItemRejectsBadIDsAndShapes(t *testing.T) {
	s, _, _ := newRecordServer(t, nil)
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/api/record/abc.mp4", http.StatusNotFound},
		{http.MethodGet, "/api/record/ZZZZZZZZZZZZ", http.StatusNotFound},
		{http.MethodGet, "/api/record/0123456789ab/what", http.StatusNotFound},
		{http.MethodGet, "/api/record/0123456789ab/stop", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/record/0123456789ab.mp4", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/record/0123456789ab", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/record/0123456789ab/stop", http.StatusNotFound},
	} {
		if w := doRecord(s, c.method, c.path, nil); w.Code != c.want {
			t.Errorf("%s %s = %d; want %d", c.method, c.path, w.Code, c.want)
		}
	}
}

func TestFramesWithoutFFmpegIs501(t *testing.T) {
	t.Setenv("PATH", "")
	s, tgt, st := newRecordServer(t, nil)
	s.ffmpeg = func() (string, error) { return "", record.ErrNoFFmpeg }
	id := startRecording(t, s)
	tgt.feed(t, 3)
	waitFrames(t, st, id, 3)
	stopRecording(t, s, id)
	w := doRecord(s, http.MethodGet, "/api/record/"+id+"/frames?from=0&to=1", nil)
	if w.Code != http.StatusNotImplemented || errCode(t, w) != ErrCodeNoFFmpeg {
		t.Fatalf("frames = %d %s; want 501 noFFmpeg", w.Code, w.Body.String())
	}
}

func TestFramesRangeChecks(t *testing.T) {
	s, tgt, st := newRecordServer(t, nil)
	s.ffmpeg = func() (string, error) { return "/nonexistent/ffmpeg", nil }
	id := startRecording(t, s)
	tgt.feed(t, 3)
	waitFrames(t, st, id, 3)
	stopRecording(t, s, id)
	for _, c := range []struct {
		q    string
		want int
	}{
		{"", http.StatusBadRequest},
		{"from=x&to=1", http.StatusBadRequest},
		{"from=0&to=11s", http.StatusUnprocessableEntity},
		{"from=2&to=1", http.StatusBadRequest},
		{"from=0&to=1&fps=0", http.StatusBadRequest},
		{"from=0&to=1&fps=-5", http.StatusBadRequest},
		{"from=0&to=1&fps=241", http.StatusBadRequest},
		{"from=0&to=1&fps=abc", http.StatusBadRequest},
		{"from=0&to=1&fps=NaN", http.StatusBadRequest},
	} {
		if w := doRecord(s, http.MethodGet, "/api/record/"+id+"/frames?"+c.q, nil); w.Code != c.want {
			t.Errorf("frames?%s = %d %s; want %d", c.q, w.Code, w.Body.String(), c.want)
		}
	}
}

func TestParseSheetTime(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"1.5":   1500 * time.Millisecond,
		"0":     0,
		"250ms": 250 * time.Millisecond,
		"2s":    2 * time.Second,
	} {
		got, err := parseSheetTime(in)
		if err != nil || got != want {
			t.Errorf("parseSheetTime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "x", "-1", "NaN", "Inf"} {
		if _, err := parseSheetTime(in); err == nil {
			t.Errorf("parseSheetTime(%q) took it", in)
		}
	}
}

func TestRecordingSurvivesTheLastViewerLeaving(t *testing.T) {
	saved := idleGrace
	idleGrace = 20 * time.Millisecond
	t.Cleanup(func() { idleGrace = saved })

	s, _, st := newRecordServer(t, nil)
	id := startRecording(t, s)
	sess, ok := s.mgr.Session("rec")
	if !ok {
		t.Fatal("no session")
	}
	s.mgr.Release("rec")
	time.Sleep(60 * time.Millisecond)
	if m, err := st.Get(id); err != nil || !m.InProgress {
		t.Fatalf("recording = %+v, %v; want still in progress", m, err)
	}
	select {
	case <-sess.Done():
		t.Fatal("session closed under a recording")
	default:
	}

	// Stopping releases the hold, so the reaper may now close it.
	stopRecording(t, s, id)
	if sess.Holds() != 0 {
		t.Fatalf("holds after stop = %d; want 0", sess.Holds())
	}
	select {
	case <-sess.Done():
	case <-time.After(time.Second):
		t.Fatal("session never closed after the recording ended")
	}
}

func TestRecordStartIsNotRefusedWhileAPersonHolds(t *testing.T) {
	s, _, _ := newRecordServer(t, nil)
	sess, err := s.mgr.Acquire(t.Context(), "rec")
	if err != nil {
		t.Fatal(err)
	}
	vid, _ := sess.AddViewer()
	_ = sess.ViewerTouch(vid, session.TouchEvent{Action: 0, PointerID: 1})
	if ok, _, _ := sess.AgentInput(""); ok {
		t.Fatal("setup: the person does not hold the target")
	}
	w := doRecord(s, http.MethodPost, "/api/record?target=rec", map[string]string{HeaderAgent: "claude"})
	if w.Code != http.StatusCreated {
		t.Fatalf("start while held = %d %s; want 201", w.Code, w.Body.String())
	}
}

func TestRecordTraceRecordsStartAndEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	tw, err := trace.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Close()
	s, tgt, st := newRecordServer(t, tw)
	id := startRecording(t, s)
	tgt.feed(t, 4)
	waitFrames(t, st, id, 4)
	stopRecording(t, s, id)

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var recs []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r map[string]any
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		if r["kind"] == "record" {
			recs = append(recs, r)
		}
	}
	if len(recs) != 2 {
		t.Fatalf("record traces = %v; want 2", recs)
	}
	start, end := recs[0], recs[1]
	if start["target"] != "rec" || start["id"] != id || start["caller"] != "agent" || start["action"] != "start" {
		t.Errorf("start record = %v", start)
	}
	for _, k := range []string{"reason", "frames", "bytes", "durationMs", "bitrateChanges", "firstKeyMs"} {
		if _, ok := end[k]; !ok {
			t.Errorf("end record lacks %q: %v", k, end)
		}
	}
	if end["reason"] != "stopped" || end["frames"] != float64(4) || end["id"] != id {
		t.Errorf("end record = %v", end)
	}
}
