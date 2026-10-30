package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postSequence(s *Server, targetID, body string, hdr map[string]string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/sequence?target="+targetID, strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	s.handleSequence(w, r)
	return w
}

func TestASequenceRunsEveryStepInOrder(t *testing.T) {
	s, ft := matchServer(t, signInScreen())
	rec := postSequence(s, "dev1", `{"steps":[
		{"action":"tap","x":10,"y":20},
		{"action":"wait","ms":1},
		{"action":"tap","match":{"text":"Sign in"}}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp SequenceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Failed != nil || len(resp.Steps) != 3 {
		t.Fatalf("response = %s; want ok with three steps", rec.Body.String())
	}
	if len(ft.taps) != 2 || ft.taps[0] != [2]int{10, 20} || ft.taps[1] != [2]int{540, 600} {
		t.Fatalf("taps = %v; want (10,20) then the Sign in center", ft.taps)
	}
	if !strings.Contains(string(resp.Steps[2].Body), `"Sign in"`) {
		t.Fatalf("step 2 body = %s; want the matched node", resp.Steps[2].Body)
	}
}

func TestASequenceStopsAtTheFirstStepThatFails(t *testing.T) {
	s, ft := matchServer(t, signInScreen())
	rec := postSequence(s, "dev1", `{"steps":[
		{"action":"tap","x":10,"y":20},
		{"action":"tap","match":{"text":"Nope"}},
		{"action":"tap","x":30,"y":40}]}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want the failing step's 404: %s", rec.Code, rec.Body.String())
	}
	var resp SequenceResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.OK || resp.Failed == nil || *resp.Failed != 1 || len(resp.Steps) != 2 || resp.Code != ErrCodeNoMatch {
		t.Fatalf("response = %s; want failed at step 1 with two steps reported", rec.Body.String())
	}
	if len(ft.taps) != 1 {
		t.Fatalf("taps = %v; want only the first", ft.taps)
	}
}

func TestASequenceStopsWhenAPersonHoldsTheDevice(t *testing.T) {
	s, _, _ := heldSession(t)
	rec := postSequence(s, "dev1", `{"steps":[{"action":"tap","x":1,"y":1}]}`, map[string]string{HeaderAgent: "claude"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ErrCodeHumanHeld) {
		t.Fatalf("status = %d: %s; want 409 humanHeld", rec.Code, rec.Body.String())
	}
}

func TestASequenceStepIsCheckedLikeAnInput(t *testing.T) {
	s, _ := matchServer(t, signInScreen())
	rec := postSequence(s, "dev1", `{"steps":[{"action":"tap"}]}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 for a tap with neither x,y nor match: %s", rec.Code, rec.Body.String())
	}
}

func TestASequenceIsBounded(t *testing.T) {
	s, _ := matchServer(t, signInScreen())
	if rec := postSequence(s, "dev1", `{"steps":[]}`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty: status = %d; want 400", rec.Code)
	}
	steps := strings.Repeat(`{"action":"wait","ms":0},`, MaxSequenceSteps+1)
	if rec := postSequence(s, "dev1", `{"steps":[`+strings.TrimSuffix(steps, ",")+`]}`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("too many: status = %d; want 400", rec.Code)
	}
}
