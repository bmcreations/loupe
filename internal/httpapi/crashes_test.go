package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

const crashID = "6A3F0C2E-1B4D-4E8A-9C71-2D5E8F0A1B3C"

// crashTarget is a Crasher with one report.
type crashTarget struct {
	stubTarget
	gotQ  target.CrashQuery
	gotID string
}

func (f *crashTarget) Crashes(_ context.Context, q target.CrashQuery) ([]target.CrashReport, error) {
	f.gotQ = q
	return []target.CrashReport{{ID: crashID, Kind: "crash", Process: "Acmechat", Summary: "EXC_CRASH (SIGABRT)"}}, nil
}

func (f *crashTarget) Crash(_ context.Context, id string) ([]byte, error) {
	f.gotID = id
	if id != crashID {
		return nil, target.ErrCrashNotFound
	}
	return []byte(`{"bug_type":"309"}` + "\n{}"), nil
}

func TestCrashesListsTheTargetsReports(t *testing.T) {
	f := &crashTarget{}
	before := time.Now()
	w := serveAgent(t, f, http.MethodGet, "/api/crashes?target=stub&limit=5&since=10m", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	var got CrashList
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	if got.Target != "stub" || len(got.Crashes) != 1 || got.Crashes[0].ID != crashID {
		t.Errorf("got %+v", got)
	}
	if f.gotQ.Limit != 5 || f.gotQ.Since.Before(before.Add(-10*time.Minute)) || f.gotQ.Since.After(after.Add(-10*time.Minute)) {
		t.Errorf("query %+v", f.gotQ)
	}
}

func TestCrashesRejectsBadQueries(t *testing.T) {
	for _, q := range []string{"limit=0", "limit=201", "limit=x", "since=yesterday"} {
		w := serveAgent(t, &crashTarget{}, http.MethodGet, "/api/crashes?target=stub&"+q, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d; want 400", q, w.Code)
		}
	}
}

func TestCrashServesTheReportAsText(t *testing.T) {
	w := serveAgent(t, &crashTarget{}, http.MethodGet, "/api/crashes/"+crashID+"?target=stub", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type %q", ct)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("no nosniff")
	}
	if !strings.HasPrefix(w.Body.String(), `{"bug_type":"309"}`) {
		t.Errorf("body %q", w.Body)
	}
}

// Another target's report is ErrCrashNotFound from the target, and so the
// same 404 as one that does not exist.
func TestCrashOfAnotherTargetIs404(t *testing.T) {
	w := serveAgent(t, &crashTarget{}, http.MethodGet, "/api/crashes/11111111-2222-3333-4444-555555555555?target=stub", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("status %d %s; want 404", w.Code, w.Body)
	}
}

// An id that is not a UUID never reaches the target.
func TestCrashRejectsAnIDThatIsNotAUUID(t *testing.T) {
	for _, id := range []string{"..%2F..%2Fetc%2Fpasswd", "Acmechat-2026-10-21-190230", "x"} {
		f := &crashTarget{}
		w := serveAgent(t, f, http.MethodGet, "/api/crashes/"+id+"?target=stub", nil)
		if w.Code != http.StatusBadRequest || f.gotID != "" {
			t.Errorf("%s = %d, reached target %q; want 400", id, w.Code, f.gotID)
		}
	}
}

func TestCrashesAre501WithoutTheCapability(t *testing.T) {
	for _, p := range []string{"/api/crashes?target=stub", "/api/crashes/" + crashID + "?target=stub"} {
		if w := serveAgent(t, &stubTarget{}, http.MethodGet, p, nil); w.Code != http.StatusNotImplemented {
			t.Errorf("%s = %d %s; want 501", p, w.Code, w.Body)
		}
	}
}

func TestCrashesAreBehindAuth(t *testing.T) {
	m := NewManager(target.Options{})
	m.Register(&crashTarget{})
	s := &Server{mgr: m, token: "secret"}
	for _, p := range []string{"/api/crashes?target=stub", "/api/crashes/" + crashID + "?target=stub"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s = %d; want 401", p, w.Code)
		}
	}
}
