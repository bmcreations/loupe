package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// MaxSequenceSteps bounds one POST /api/sequence. A flow longer than this is
// better split, so a failure partway reports where it stopped soon enough.
const MaxSequenceSteps = 50

// SequenceRequest is POST /api/sequence's body: the steps to run in order,
// each exactly what POST /api/input would take.
type SequenceRequest struct {
	Steps []InputRequest `json:"steps"`
}

// SequenceStep is one step's answer: its POST /api/input status and body.
type SequenceStep struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body,omitempty"`
}

// SequenceResponse answers POST /api/sequence. OK is true when every step
// returned 200. Otherwise Failed is the index of the step that did not, the
// response carries that step's status, Steps stops at it, and Error and Code
// repeat its own, so a caller that keys on a code (humanHeld, claimedBy)
// reads it where it would for a single input.
type SequenceResponse struct {
	OK     bool           `json:"ok"`
	Failed *int           `json:"failed,omitempty"`
	Error  string         `json:"error,omitempty"`
	Code   string         `json:"code,omitempty"`
	Steps  []SequenceStep `json:"steps"`
}

// handleSequence runs several inputs in one request, so an agent pays one
// round trip for a flow instead of one per step. Every step goes through
// handleInput itself, so each is validated, arbitrated against claims and
// the human hold, logged and traced exactly as if it had been sent alone: a
// person who takes the device back partway stops the sequence at the next
// step with that step's 409.
func (s *Server) handleSequence(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.agentTarget(w, r); !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	// The steps are kept raw: handleInput checks each body's fields itself,
	// and a decoded InputRequest cannot tell an omitted x from x:0.
	var raw struct {
		Steps []json.RawMessage `json:"steps"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	if len(raw.Steps) == 0 || len(raw.Steps) > MaxSequenceSteps {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: fmt.Sprintf("sequence needs 1 to %d steps, got %d", MaxSequenceSteps, len(raw.Steps)),
		})
		return
	}
	reqs := make([]InputRequest, len(raw.Steps))
	for i, b := range raw.Steps {
		if err := json.Unmarshal(b, &reqs[i]); err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("step %d: %v", i, err)})
			return
		}
	}

	resp := SequenceResponse{OK: true, Steps: make([]SequenceStep, 0, len(reqs))}
	for i, req := range reqs {
		if r.Context().Err() != nil {
			return
		}
		// An agent sending one tap sends a move first so its cursor is on
		// the spot as the touch lands; between steps here nobody else would.
		// A match tap places its own cursor once it knows where.
		if req.Match == nil && pointsFirst(req.Action) && validateInputRequest(raw.Steps[i], req.Action) == nil {
			s.runStep(r, []byte(fmt.Sprintf(`{"action":"move","x":%d,"y":%d}`, req.X, req.Y)))
		}
		status, out := s.runStep(r, raw.Steps[i])
		resp.Steps = append(resp.Steps, SequenceStep{Status: status, Body: out})
		if status != http.StatusOK {
			failed := i
			resp.OK, resp.Failed = false, &failed
			var e ErrorResponse
			_ = json.Unmarshal(out, &e)
			resp.Error, resp.Code = fmt.Sprintf("step %d: %s", i, e.Error), e.Code
			writeJSON(w, status, resp)
			return
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// pointsFirst reports whether a step touches at x, y, so the cursor should
// be sent there before it.
func pointsFirst(action string) bool {
	return action == "tap" || action == "swipe"
}

// runStep sends one step's body through handleInput with the sequence's own
// query and headers, and returns what it answered.
func (s *Server) runStep(r *http.Request, body []byte) (int, json.RawMessage) {
	sr := r.Clone(r.Context())
	sr.Body = io.NopCloser(bytes.NewReader(body))
	sr.ContentLength = int64(len(body))
	rec := &stepRecorder{header: http.Header{}, code: http.StatusOK}
	s.handleInput(rec, sr)
	out := bytes.TrimSpace(rec.body.Bytes())
	if !json.Valid(out) {
		// http.Error answers in plain text; wrap it so the step stays JSON.
		out, _ = json.Marshal(ErrorResponse{Error: string(out)})
	}
	return rec.code, out
}

// stepRecorder keeps what handleInput writes for one step.
type stepRecorder struct {
	header http.Header
	body   bytes.Buffer
	code   int
	wrote  bool
}

func (w *stepRecorder) Header() http.Header { return w.header }

func (w *stepRecorder) WriteHeader(code int) {
	if !w.wrote {
		w.code, w.wrote = code, true
	}
}

func (w *stepRecorder) Write(b []byte) (int, error) {
	w.wrote = true
	return w.body.Write(b)
}
