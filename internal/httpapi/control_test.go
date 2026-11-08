package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/bmcreations/loupe/internal/session"
)

func postRelease(s *Server, targetID, viewer string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/control/release?target="+targetID, nil)
	if viewer != "" {
		r.Header.Set(HeaderViewer, viewer)
	}
	s.handleControlRelease(w, r)
	return w
}

func TestReleaseHandsTheTargetBackToAWaitingAgent(t *testing.T) {
	s, sess, id := heldSession(t)
	if rec := postInputAs(s, "dev1", `{"action":"tap","x":1,"y":1}`, map[string]string{HeaderAgent: "claude"}); rec.Code != http.StatusConflict {
		t.Fatalf("agent before release: status = %d; want 409", rec.Code)
	}
	rec := postRelease(s, "dev1", strconv.FormatUint(id, 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var st session.ControlState
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Held || st.Agent != session.AgentWaiting {
		t.Fatalf("state = %+v; want free with claude still waiting", st)
	}
	if rec := postInputAs(s, "dev1", `{"action":"tap","x":1,"y":1}`, map[string]string{HeaderAgent: "claude"}); rec.Code != http.StatusOK {
		t.Fatalf("agent after release: status = %d: %s", rec.Code, rec.Body.String())
	}
	if st := sess.ControlState(); st.Agent != session.AgentDriving {
		t.Errorf("control state = %+v; want claude driving", st)
	}
}

func TestReleaseRefusesACallerThatIsNotAViewer(t *testing.T) {
	s, sess, _ := heldSession(t)
	if rec := postRelease(s, "dev1", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("no viewer header: status = %d; want 400", rec.Code)
	}
	if rec := postRelease(s, "dev1", "999"); rec.Code != http.StatusConflict {
		t.Errorf("unattached viewer: status = %d; want 409", rec.Code)
	}
	if rec := postRelease(s, "nope", "1"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown target: status = %d; want 404", rec.Code)
	}
	if st := sess.ControlState(); !st.Held {
		t.Errorf("state = %+v; want still held", st)
	}
}

func TestReleaseByAViewerNotHoldingIsANoOp(t *testing.T) {
	s, sess, holder := heldSession(t)
	other, _ := sess.AddViewer()
	rec := postRelease(s, "dev1", strconv.FormatUint(other, 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if st := sess.ControlState(); !st.Held || st.Holder != holder {
		t.Errorf("state = %+v; want still held by %d", st, holder)
	}
}
