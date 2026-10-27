package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/config"
	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
)

// newTestStore builds a store over a throwaway file.
func newTestStore(t *testing.T, pinned ...string) (*ConfigStore, *[]config.Config) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	pin := make(map[string]bool, len(pinned))
	for _, p := range pinned {
		pin[p] = true
	}
	var mu sync.Mutex
	applied := []config.Config{}
	st := NewConfigStore(path, config.Default(), pin, config.Default(), func(c config.Config) {
		mu.Lock()
		defer mu.Unlock()
		applied = append(applied, c)
	})
	return st, &applied
}

// putConfig sends a patch and fails the test if it is rejected.
func putConfig(t *testing.T, st *ConfigStore, body string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	rec := serveConfig(t, st, http.MethodPut, body)
	if rec.Code != http.StatusOK {
		return rec, fmt.Errorf("status %d: %s", rec.Code, rec.Body.String())
	}
	return rec, nil
}

func serveConfig(t *testing.T, st *ConfigStore, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	s := &Server{cfg: st}
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/api/config", nil)
	} else {
		r = httptest.NewRequest(method, "/api/config", strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	s.handleConfig(w, r)
	return w
}

func TestConfigGetReportsSourceAndApplies(t *testing.T) {
	st, _ := newTestStore(t, "maxFps")

	w := serveConfig(t, st, http.MethodGet, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var got ConfigResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	byName := map[string]configField{}
	for _, f := range got.Fields {
		byName[f.Name] = f
	}

	// A flag-pinned setting must say so, or the app will write it to the file
	// and report success while the running server keeps ignoring it.
	if f := byName["maxFps"]; f.Source != "flag" || !f.Pinned {
		t.Errorf("maxFps source = %q pinned = %v, want flag/true", f.Source, f.Pinned)
	}
	if f := byName["maxSize"]; f.Source != "default" || f.Pinned {
		t.Errorf("maxSize source = %q pinned = %v, want default/false", f.Source, f.Pinned)
	}
	if f := byName["maxFps"]; f.Applies != config.AppliesNextSession {
		t.Errorf("maxFps applies = %q, want live", f.Applies)
	}
	if f := byName["addr"]; f.Applies != config.AppliesRestart {
		t.Errorf("addr applies = %q, want restart", f.Applies)
	}
	if got.Path == "" {
		t.Error("path is empty; the UI has nothing to show the user")
	}
}

func TestConfigPutPersistsAndAppliesNextSession(t *testing.T) {
	st, applied := newTestStore(t)

	w := serveConfig(t, st, http.MethodPut, `{"maxSize":720}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", w.Code, w.Body.String())
	}

	var got PutResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Changed) != 1 || got.Changed[0] != "maxSize" {
		t.Errorf("changed = %v, want [maxSize]", got.Changed)
	}
	if len(got.RestartRequired) != 0 {
		t.Errorf("restartRequired = %v, want none", got.RestartRequired)
	}
	if len(*applied) != 1 || (*applied)[0].MaxSize != 720 {
		t.Errorf("applied = %+v, want one call with MaxSize 720", *applied)
	}

	// It has to survive a restart, which means it has to be on disk.
	on, _, err := config.Load(st.Path())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if on.MaxSize != 720 {
		t.Errorf("persisted MaxSize = %d, want 720", on.MaxSize)
	}
}

func TestConfigPutReportsRestartRequired(t *testing.T) {
	st, _ := newTestStore(t)

	w := serveConfig(t, st, http.MethodPut, `{"addr":":9999"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", w.Code, w.Body.String())
	}
	var got PutResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.RestartRequired) != 1 || got.RestartRequired[0] != "addr" {
		t.Errorf("restartRequired = %v, want [addr]", got.RestartRequired)
	}
}

func TestConfigPutReportsFlagPinnedSettings(t *testing.T) {
	st, _ := newTestStore(t, "maxFps")

	w := serveConfig(t, st, http.MethodPut, `{"maxFps":30}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", w.Code, w.Body.String())
	}
	var got PutResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)

	// Written to the file, but a command-line flag still wins at runtime. If
	// this is silent the UI shows 30 and the device keeps running at 60.
	if len(got.Pinned) != 1 || got.Pinned[0] != "maxFps" {
		t.Errorf("pinned = %v, want [maxFps]", got.Pinned)
	}
}

func TestConfigPutRejectsUnknownAndInvalid(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"unknown key", `{"maxFsp":30}`},
		{"out of range", `{"maxFps":900}`},
		{"bad codec", `{"videoCodec":"vp9"}`},
		{"malformed", `{`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, applied := newTestStore(t)
			w := serveConfig(t, st, http.MethodPut, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
			if len(*applied) != 0 {
				t.Errorf("applied %d changes on a rejected request", len(*applied))
			}
			if _, err := os.Stat(st.Path()); err == nil {
				t.Error("wrote the config file on a rejected request")
			}
		})
	}
}

func TestConfigRejectsOtherMethods(t *testing.T) {
	st, _ := newTestStore(t)
	w := serveConfig(t, st, http.MethodPost, `{}`)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestManagerOptionsAreReplaceable(t *testing.T) {
	m := NewManager(target.Options{MaxFPS: 60})
	if got := m.Options().MaxFPS; got != 60 {
		t.Fatalf("MaxFPS = %d, want 60", got)
	}
	m.SetOptions(target.Options{MaxFPS: 30})
	if got := m.Options().MaxFPS; got != 30 {
		t.Errorf("MaxFPS = %d, want 30", got)
	}
}

// stubTarget opens instantly. Each stream gets its own frame channel, closed
// on Close, because Acquire's optimistic-open race means most of these streams
// are closed immediately and a pump blocked on a channel that never closes
// would deadlock the teardown.
type stubTarget struct{}

func (s *stubTarget) ID() string        { return "stub" }
func (s *stubTarget) Kind() target.Kind { return target.KindSynthetic }
func (s *stubTarget) Label() string     { return "stub" }
func (s *stubTarget) Open(ctx context.Context, opts target.Options) (target.Stream, error) {
	return &stubStream{frames: make(chan target.Frame)}, nil
}

type stubStream struct {
	frames chan target.Frame
	once   sync.Once
}

func (s *stubStream) Meta() target.Meta           { return target.Meta{Width: 100, Height: 100} }
func (s *stubStream) Frames() <-chan target.Frame { return s.frames }
func (s *stubStream) Touch(target.Touch) error    { return target.ErrNoControl }
func (s *stubStream) Scroll(target.Scroll) error  { return target.ErrNoControl }
func (s *stubStream) Key(target.Key) error        { return target.ErrNoControl }
func (s *stubStream) RequestKeyframe() error      { return nil }
func (s *stubStream) Err() error                  { return nil }
func (s *stubStream) Close() error                { s.once.Do(func() { close(s.frames) }); return nil }

// TestSetOptionsRacesWithAcquire is the test that matters for the lock, and it
// only means anything under -race. Acquire deliberately drops the mutex before
// opening a session, because launching scrcpy takes hundreds of milliseconds
// and must not block the targets listing. That makes the options it hands to
// session.Open a read that has to happen before the unlock, and no
// single-goroutine test can tell the difference.
func TestSetOptionsRacesWithAcquire(t *testing.T) {
	m := NewManager(target.Options{MaxFPS: 60})
	m.Register(&stubTarget{})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			m.SetOptions(target.Options{MaxFPS: 30 + i})
		}(i)
		go func() {
			defer wg.Done()
			s, err := m.Acquire(context.Background(), "stub")
			if err == nil {
				_ = s.Close()
			}
		}()
	}
	wg.Wait()
}

func TestPinnedFieldReportsTheValueActuallyInForce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// The file says 720. The command line said 1080 and wins for the life of
	// the process. A UI that renders the file value shows a number the server
	// is not using, which is the silent failure this whole path exists to stop.
	onDisk := config.Default()
	onDisk.MaxSize = 720
	if err := config.Save(path, onDisk); err != nil {
		t.Fatal(err)
	}
	inForce := config.Default()
	inForce.MaxSize = 1080

	st := NewConfigStore(path, onDisk, map[string]bool{"maxSize": true}, inForce, func(config.Config) {})
	rec := serveConfig(t, st, http.MethodGet, "")

	var got ConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, f := range got.Fields {
		if f.Name != "maxSize" {
			continue
		}
		if f.Value != float64(1080) {
			t.Errorf("value = %v, want 1080 (the flag is in force)", f.Value)
		}
		if f.FileValue != float64(720) {
			t.Errorf("fileValue = %v, want 720 (what is persisted)", f.FileValue)
		}
		return
	}
	t.Fatal("maxSize missing from the response")
}

func TestUnpinnedFieldReportsTheSameValueTwice(t *testing.T) {
	st, _ := newTestStore(t)
	if _, err := putConfig(t, st, `{"maxFps":30}`); err != nil {
		t.Fatal(err)
	}

	rec := serveConfig(t, st, http.MethodGet, "")
	var got ConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, f := range got.Fields {
		if f.Name != "maxFps" {
			continue
		}
		// Nothing pins it, so what is on disk is what is in force.
		if f.Value != float64(30) || f.FileValue != float64(30) {
			t.Errorf("value = %v, fileValue = %v, want both 30", f.Value, f.FileValue)
		}
		return
	}
	t.Fatal("maxFps missing from the response")
}

// The setting is useless unless it reaches the session that will report it to
// the browser. Acquire opens outside the lock, so the value has to be read
// before the unlock and applied before the session is published -- a viewer
// that joins first caches init without it and never learns the ceiling.
func TestManagerAppliesTheClientBufferToNewSessions(t *testing.T) {
	m := NewManager(target.Options{MaxFPS: 60})
	m.Register(&stubTarget{})
	m.SetClientBuffer(275)

	s, err := m.Acquire(context.Background(), "stub")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer s.Close()

	_, out := s.AddViewer()
	select {
	case msg := <-out:
		var got session.InitPayload
		if err := json.Unmarshal(msg.Data[1:], &got); err != nil {
			t.Fatalf("init is not InitPayload JSON: %v", err)
		}
		if got.MaxBufferMs != 275 {
			t.Errorf("init maxBufferMs = %d, want 275", got.MaxBufferMs)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no init message")
	}
}

// TestConfigPutWithNoActualChangeReturnsAnEmptyArrayNotNull pins the
// null-array fix from the apischema review: a patch that restates the
// current value changes nothing, config.Merge's changedFields returns nil,
// and PutResponse.Changed carries no omitempty -- docs/api.schema.json
// promises an array. Encoding the real handler output and decoding it back
// is what catches a regression that reintroduces the bare
// `Changed: changed` assignment.
func TestConfigPutWithNoActualChangeReturnsAnEmptyArrayNotNull(t *testing.T) {
	st, _ := newTestStore(t)

	// 1080 is config.Default()'s own MaxSize, so this patch changes nothing.
	w := serveConfig(t, st, http.MethodPut, `{"maxSize":1080}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"changed":[]`) {
		t.Fatalf("body = %s, want a literal \"changed\":[] rather than null", body)
	}
	var resp PutResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Changed == nil {
		t.Fatalf("Changed decoded as nil despite the wire promising a non-null array")
	}
	if len(resp.Changed) != 0 {
		t.Fatalf("Changed = %v, want empty", resp.Changed)
	}
}
