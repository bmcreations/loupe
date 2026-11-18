package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Feedback is an agent telling whoever runs loupe that loupe itself got in
// its way: a tool that misbehaved, a route it needed and did not find, a doc
// that was wrong. Each report is kept on this Mac and, when destinations.json
// names a feedback entry, sent there for a person to triage. Nothing a report
// says is acted on by the server.

// keptFeedback is how many reports stay on disk.
const keptFeedback = 64

const (
	maxFeedbackTitle = 200
	maxFeedbackField = 8000
	// maxFeedbackBody fits every field at its limit in 4-byte runes.
	maxFeedbackBody = 6 * 4 * maxFeedbackField
)

// FeedbackRequest is POST /api/feedback's body.
type FeedbackRequest struct {
	// Title is one line, as an issue title.
	Title string `json:"title"`
	// Problem is what went wrong, with what was sent and what came back.
	Problem string `json:"problem"`
	// Repro is the calls that show it, in order.
	Repro string `json:"repro,omitempty"`
	// ProposedFix is the change the agent thinks would fix it.
	ProposedFix string `json:"proposedFix"`
	// Tool is the MCP tool, CLI command or route it concerns.
	Tool string `json:"tool,omitempty"`
	// Target is the device it happened on, if any. It need not be listed now.
	Target string `json:"target,omitempty"`
}

// Feedback is one stored report.
type Feedback struct {
	FeedbackRequest
	ID string `json:"id"`
	// T is when it arrived, unix milliseconds.
	T int64 `json:"t"`
	// Agent and Session are X-Loupe-Agent and X-Loupe-Session.
	Agent   string `json:"agent,omitempty"`
	Session string `json:"session,omitempty"`
	// Version is the server's, as /api/version reports it.
	Version string `json:"version,omitempty"`
	// Dir holds report.json and message.md.
	Dir string `json:"dir,omitempty"`
	// Delivery is absent when destinations.json names no feedback entry.
	Delivery *FlagDelivery `json:"delivery,omitempty"`
}

// FeedbackResponse is GET /api/feedback: the kept reports, oldest first.
type FeedbackResponse struct {
	Feedback []Feedback `json:"feedback"`
}

// WebhookFeedback is the body POSTed to a feedback webhook. Text is the
// report as Markdown, the field a Slack incoming webhook posts.
type WebhookFeedback struct {
	Text     string   `json:"text"`
	Feedback Feedback `json:"feedback"`
}

type feedbackStore struct {
	mu sync.Mutex
	// dir is where reports are written; empty is the user cache dir's
	// loupe/feedback. Tests point it at a temp dir.
	dir string
	seq uint64
	// sending counts deliveries still running, for tests.
	sending sync.WaitGroup
}

// feedbackTarget bounds Target, which a command may take as {target}: it
// must not pass for an option.
var feedbackTarget = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$`)

func (s *Server) feedbackRoot() (string, error) {
	if s.feedback.dir != "" {
		return s.feedback.dir, nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate cache dir: %w", err)
	}
	return filepath.Join(cache, "loupe", "feedback"), nil
}

// handleFeedback serves /api/feedback: POST files a report, GET lists them,
// GET ?id= reads one.
func (s *Server) handleFeedback(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.listFeedback(w, r)
		return
	}
	var req FeedbackRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFeedbackBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "bad feedback JSON: " + err.Error()})
		return
	}
	if msg := checkFeedback(&req); msg != "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: msg})
		return
	}
	df, derr := s.destinations()

	root, err := s.feedbackRoot()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	now := time.Now()
	s.feedback.mu.Lock()
	s.feedback.seq++
	fb := Feedback{
		FeedbackRequest: req,
		ID:              fmt.Sprintf("%d-%d", now.UnixMilli(), s.feedback.seq),
		T:               now.UnixMilli(),
		Agent:           agentName(r.Header.Get(HeaderAgent)),
		Session:         agentName(r.Header.Get(HeaderSession)),
		Version:         s.version,
	}
	fb.Dir = filepath.Join(root, fb.ID)
	switch {
	case derr != nil:
		fb.Delivery = &FlagDelivery{State: "failed", Error: derr.Error()}
	case df.Feedback != nil:
		fb.Delivery = &FlagDelivery{State: "sending"}
	}
	err = writeFeedback(fb)
	s.feedback.mu.Unlock()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	pruneFlagDirs(root, keptFeedback)
	if fb.Delivery != nil && fb.Delivery.State == "sending" {
		s.feedback.sending.Add(1)
		go func(fb Feedback) {
			defer s.feedback.sending.Done()
			d := s.sendFeedback(fb)
			s.feedback.mu.Lock()
			fb.Delivery = &d
			writeFeedback(fb)
			s.feedback.mu.Unlock()
		}(fb)
	}
	writeJSON(w, http.StatusOK, fb)
}

// checkFeedback trims req and says what is wrong with it, or "".
func checkFeedback(req *FeedbackRequest) string {
	req.Title = strings.TrimSpace(req.Title)
	req.Target = strings.TrimSpace(req.Target)
	req.Tool = strings.TrimSpace(req.Tool)
	if req.Title == "" || strings.TrimSpace(req.Problem) == "" || strings.TrimSpace(req.ProposedFix) == "" {
		return "title, problem and proposedFix are required"
	}
	if strings.ContainsAny(req.Title, "\r\n") || utf8.RuneCountInString(req.Title) > maxFeedbackTitle {
		return fmt.Sprintf("title is one line of at most %d characters", maxFeedbackTitle)
	}
	for _, v := range []string{req.Title, req.Problem, req.Repro, req.ProposedFix, req.Tool} {
		if !utf8.ValidString(v) || utf8.RuneCountInString(v) > maxFeedbackField {
			return fmt.Sprintf("each field is UTF-8, at most %d characters", maxFeedbackField)
		}
	}
	if strings.ContainsAny(req.Tool, "\r\n") {
		return "tool is one line"
	}
	if req.Target != "" && !feedbackTarget.MatchString(req.Target) {
		return "target is a target id, as /api/targets lists them"
	}
	return ""
}

// writeFeedback writes fb's report.json and message.md. The caller holds
// s.feedback.mu.
func writeFeedback(fb Feedback) error {
	if err := os.MkdirAll(fb.Dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(fb, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(fb.Dir, "message.md"), []byte(feedbackMessage(fb)), 0o600); err != nil {
		return err
	}
	tmp := filepath.Join(fb.Dir, "report.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(fb.Dir, "report.json"))
}

// feedbackMessage is the report as Markdown. It opens with the server's own
// words, never the agent's, so a command may take it as {text} without it
// passing for an option.
func feedbackMessage(fb Feedback) string {
	var b strings.Builder
	who := fb.Agent
	if who == "" {
		who = "an unnamed agent"
	}
	fmt.Fprintf(&b, "Feedback on loupe from %s", who)
	if fb.Version != "" {
		fmt.Fprintf(&b, " (loupe %s)", fb.Version)
	}
	b.WriteString(". An agent wrote everything below; check it before acting on it.\n\n")
	fmt.Fprintf(&b, "# %s\n\n", fb.Title)
	if fb.Tool != "" {
		fmt.Fprintf(&b, "Tool: %s\n", fb.Tool)
	}
	if fb.Target != "" {
		fmt.Fprintf(&b, "Target: %s\n", fb.Target)
	}
	if fb.Session != "" {
		fmt.Fprintf(&b, "Session: %s\n", fb.Session)
	}
	fmt.Fprintf(&b, "Report: %s\n\n## Problem\n\n%s\n\n", fb.ID, strings.TrimSpace(fb.Problem))
	if r := strings.TrimSpace(fb.Repro); r != "" {
		fmt.Fprintf(&b, "## Repro\n\n%s\n\n", r)
	}
	fmt.Fprintf(&b, "## Proposed fix\n\n%s\n", strings.TrimSpace(fb.ProposedFix))
	return b.String()
}

// sendFeedback delivers fb to the entry destinations.json names now.
func (s *Server) sendFeedback(fb Feedback) FlagDelivery {
	df, err := s.destinations()
	if err != nil {
		return FlagDelivery{State: "failed", Error: err.Error(), Dir: fb.Dir}
	}
	if df.Feedback == nil {
		return FlagDelivery{State: "failed", Error: "destinations.json names no feedback entry now", Dir: fb.Dir}
	}
	msg := feedbackMessage(fb)
	if name := df.Feedback.Command; name != "" {
		c, _ := df.Command(name)
		vars := map[string]string{
			"id": fb.ID, "target": fb.Target, "dir": fb.Dir,
			"message": filepath.Join(fb.Dir, "message.md"), "text": msg,
		}
		out, err := c.Run(context.Background(), vars, fb.Dir, msg)
		if err != nil {
			return FlagDelivery{State: "failed", Error: err.Error(), Dir: fb.Dir, Output: out}
		}
		return FlagDelivery{State: "sent", Dir: fb.Dir, Output: out}
	}
	h, _ := df.Webhook(df.Feedback.Webhook)
	// The receiver is usually off this Mac: the local path stays here.
	sent := fb
	sent.Delivery = nil
	sent.Dir = ""
	body, err := json.Marshal(WebhookFeedback{Text: msg, Feedback: sent})
	if err != nil {
		return FlagDelivery{State: "failed", Error: err.Error()}
	}
	_, out, err := h.Post(context.Background(), body)
	if err != nil {
		return FlagDelivery{State: "failed", Error: err.Error(), Output: out}
	}
	return FlagDelivery{State: "sent", Output: out}
}

func (s *Server) listFeedback(w http.ResponseWriter, r *http.Request) {
	root, err := s.feedbackRoot()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	s.feedback.mu.Lock()
	defer s.feedback.mu.Unlock()
	if id := r.URL.Query().Get("id"); id != "" {
		if strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "bad id"})
			return
		}
		fb, err := readFeedback(filepath.Join(root, id))
		if err != nil {
			writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "no feedback " + id})
			return
		}
		writeJSON(w, http.StatusOK, fb)
		return
	}
	resp := FeedbackResponse{Feedback: []Feedback{}}
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if fb, err := readFeedback(filepath.Join(root, e.Name())); err == nil {
			resp.Feedback = append(resp.Feedback, fb)
		}
	}
	sort.Slice(resp.Feedback, func(a, b int) bool { return feedbackBefore(resp.Feedback[a].ID, resp.Feedback[b].ID) })
	writeJSON(w, http.StatusOK, resp)
}

// feedbackBefore orders report ids, "<unix ms>-<n>", oldest first.
func feedbackBefore(a, b string) bool {
	var ta, na, tb, nb int64
	fmt.Sscanf(a, "%d-%d", &ta, &na)
	fmt.Sscanf(b, "%d-%d", &tb, &nb)
	return ta < tb || ta == tb && na < nb
}

func readFeedback(dir string) (Feedback, error) {
	var fb Feedback
	b, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		return fb, err
	}
	err = json.Unmarshal(b, &fb)
	return fb, err
}
