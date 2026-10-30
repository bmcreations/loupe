package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

// dropTarget is an ImageDropper that keeps what it was handed.
type dropTarget struct {
	stubTarget
	err     error
	canErr  error
	calls   int
	gotBody []byte
	gotExt  string
	gotX    int
	gotY    int
}

func (f *dropTarget) CanDropImage(context.Context) error { return f.canErr }

func (f *dropTarget) DropImage(ctx context.Context, path string, x, y int) (target.DropResult, error) {
	f.calls++
	b, err := os.ReadFile(path)
	if err != nil {
		return target.DropResult{}, err
	}
	f.gotBody, f.gotExt, f.gotX, f.gotY = b, filepath.Ext(path), x, y
	return target.DropResult{}, f.err
}

func TestDropPastesTheImageAtThePoint(t *testing.T) {
	f := &dropTarget{}
	w := serveAgent(t, f, http.MethodPost, "/api/drop?target=stub&x=585&y=885", strings.NewReader(png))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s; want 200", w.Code, w.Body)
	}
	if string(f.gotBody) != png || f.gotExt != ".png" || f.gotX != 585 || f.gotY != 885 {
		t.Errorf("target got %q %s at %d,%d; want the body as .png at 585,885", f.gotBody, f.gotExt, f.gotX, f.gotY)
	}
	var resp DropResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || !resp.OK || resp.Target != "stub" {
		t.Errorf("response %s; want ok for stub", w.Body)
	}
}

func TestDropRefusesWhatIsNotAnImage(t *testing.T) {
	for name, body := range map[string]string{
		"apk":   apk,
		"video": "\x00\x00\x00\x18ftypmp42 rest of the movie",
		"empty": "",
	} {
		f := &dropTarget{}
		w := serveAgent(t, f, http.MethodPost, "/api/drop?target=stub&x=1&y=1", strings.NewReader(body))
		if w.Code != http.StatusBadRequest || f.calls != 0 {
			t.Errorf("%s: status %d, target called %d times; want 400 and no call", name, w.Code, f.calls)
		}
	}
}

func TestDropNeedsBothCoordinates(t *testing.T) {
	for _, q := range []string{"", "&x=1", "&y=1", "&x=a&y=1", "&x=1&y=-2"} {
		f := &dropTarget{}
		w := serveAgent(t, f, http.MethodPost, "/api/drop?target=stub"+q, strings.NewReader(png))
		if w.Code != http.StatusBadRequest || f.calls != 0 {
			t.Errorf("%q: status %d, target called %d times; want 400 and no call", q, w.Code, f.calls)
		}
	}
}

func TestDropSizeLimit(t *testing.T) {
	defer func(n int64) { maxDropBytes = n }(maxDropBytes)
	maxDropBytes = 64
	big := png + strings.Repeat("x", 100)

	f := &dropTarget{}
	w := serveAgent(t, f, http.MethodPost, "/api/drop?target=stub&x=1&y=1", strings.NewReader(big))
	if w.Code != http.StatusRequestEntityTooLarge || f.calls != 0 {
		t.Errorf("status %d, target called %d times; want 413 and no call", w.Code, f.calls)
	}

	// Without a Content-Length the limit is found while staging.
	f = &dropTarget{}
	w = serveAgent(t, f, http.MethodPost, "/api/drop?target=stub&x=1&y=1", struct{ *strings.Reader }{strings.NewReader(big)})
	if w.Code != http.StatusRequestEntityTooLarge || f.calls != 0 {
		t.Errorf("chunked: status %d, target called %d times; want 413 and no call", w.Code, f.calls)
	}
}

func TestDropIs501OffASimulator(t *testing.T) {
	w := serveAgent(t, &stubTarget{}, http.MethodPost, "/api/drop?target=stub&x=1&y=1", strings.NewReader(png))
	if w.Code != http.StatusNotImplemented || errBody(t, w).Code != ErrCodeUnsupported {
		t.Errorf("status %d %s; want 501 unsupported", w.Code, w.Body)
	}
}

func TestDropWithNoPasteIs422(t *testing.T) {
	f := &dropTarget{err: target.ErrNoPaste}
	w := serveAgent(t, f, http.MethodPost, "/api/drop?target=stub&x=1&y=1", strings.NewReader(png))
	if w.Code != http.StatusUnprocessableEntity || errBody(t, w).Code != ErrCodeNoPaste {
		t.Errorf("status %d %s; want 422 noPaste", w.Code, w.Body)
	}
}

func TestDropDeviceFailureIs502(t *testing.T) {
	f := &dropTarget{err: errors.New("set pasteboard: CoreDevice: no answer in 30s")}
	w := serveAgent(t, f, http.MethodPost, "/api/drop?target=stub&x=1&y=1", strings.NewReader(png))
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "no answer") {
		t.Errorf("status %d %s; want 502 naming the cause", w.Code, w.Body)
	}
}

func TestDropWhosePasteDidNotTakeIs422(t *testing.T) {
	f := &dropTarget{err: target.ErrPasteFailed}
	w := serveAgent(t, f, http.MethodPost, "/api/drop?target=stub&x=1&y=1", strings.NewReader(png))
	if w.Code != http.StatusUnprocessableEntity || errBody(t, w).Code != ErrCodePasteFailed {
		t.Errorf("status %d %s; want 422 pasteFailed", w.Code, w.Body)
	}
}

// An old simcap is refused before anything reaches the device, the image
// included.
func TestDropThroughAnOldSimcapIs501BeforeAnyTap(t *testing.T) {
	f := &dropTarget{canErr: target.ErrSimcapOutdated}
	w := serveAgent(t, f, http.MethodPost, "/api/drop?target=stub&x=1&y=1", strings.NewReader(png))
	if w.Code != http.StatusNotImplemented || errBody(t, w).Code != ErrCodeSimcapOutdated || f.calls != 0 {
		t.Errorf("status %d %s, target called %d times; want 501 simcapOutdated and no call", w.Code, w.Body, f.calls)
	}
	if !strings.Contains(w.Body.String(), "make simcap") {
		t.Errorf("body %s; want it to say how to rebuild simcap", w.Body)
	}
}

// dropAgentTarget is a streamable target with a screen size that takes a
// drop, so a session can watch one.
type dropAgentTarget struct {
	*fakeAgentTarget
	err error
}

func (f *dropAgentTarget) CanDropImage(context.Context) error { return nil }
func (f *dropAgentTarget) DropImage(context.Context, string, int, int) (target.DropResult, error) {
	return target.DropResult{}, f.err
}

// Viewers see a drop as they see an agent's tap: one event at the point,
// with the error when it failed.
func TestDropPublishesATapAtThePoint(t *testing.T) {
	for _, c := range []struct {
		name    string
		err     error
		wantErr bool
	}{{"ok", nil, false}, {"failed", target.ErrPasteFailed, true}} {
		m := NewManager(target.Options{})
		m.Register(&dropAgentTarget{fakeAgentTarget: &fakeAgentTarget{id: "dev1", screenW: 1080, screenH: 2400}, err: c.err})
		sess, err := m.Acquire(t.Context(), "dev1")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		s := &Server{mgr: m}
		w := httptest.NewRecorder()
		s.handleDrop(w, httptest.NewRequest(http.MethodPost, "/api/drop?target=dev1&x=540&y=600", strings.NewReader(png)))
		events := sess.EventsAfter(0)
		sess.Close()
		if len(events) != 1 {
			t.Fatalf("%s: events %+v; want one", c.name, events)
		}
		e := events[0]
		if e.Source != "agent" || e.Kind != "tap" || e.From == nil || e.From.X != 0.5 || e.From.Y != 0.25 {
			t.Errorf("%s: event %+v; want an agent tap at 0.5,0.25", c.name, e)
		}
		if (e.Err != "") != c.wantErr {
			t.Errorf("%s: Err %q; want set %v", c.name, e.Err, c.wantErr)
		}
	}
}
