package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/dest"
	"github.com/bmcreations/loupe/internal/target"
)

const feedbackBody = `{"title":"tap by --text misses a node under a scroll view","problem":"tap returned 404","repro":"loupe tap avd:Pixel_10 --text Save","proposedFix":"search the whole tree","tool":"tap","target":"avd:Pixel_10"}`

// feedbackServer is a server whose reports land in a temp dir, with
// destinations.json set to f when f is not nil.
func feedbackServer(t *testing.T, f *dest.File) (*Server, string) {
	t.Helper()
	s := &Server{mgr: NewManager(target.Options{}), token: "secret", flags: newFlagStore(), version: "v9"}
	dir := t.TempDir()
	s.feedback.dir = filepath.Join(dir, "feedback")
	if f != nil {
		b, _ := json.Marshal(f)
		s.destPath = filepath.Join(dir, "destinations.json")
		if err := os.WriteFile(s.destPath, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return s, dir
}

func postFeedback(t *testing.T, s *Server, body string) Feedback {
	t.Helper()
	w := serveOn(s, http.MethodPost, "/api/feedback", body, map[string]string{HeaderAgent: "claude", HeaderSession: "s1"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var fb Feedback
	if err := json.Unmarshal(w.Body.Bytes(), &fb); err != nil {
		t.Fatal(err)
	}
	return fb
}

func TestFeedbackWithNoDestinationIsKeptAndListed(t *testing.T) {
	s, _ := feedbackServer(t, nil)
	fb := postFeedback(t, s, feedbackBody)
	if fb.Delivery != nil || fb.Agent != "claude" || fb.Session != "s1" || fb.Version != "v9" || fb.Tool != "tap" {
		t.Fatalf("report = %+v", fb)
	}
	postFeedback(t, s, strings.Replace(feedbackBody, "tap by", "second:", 1))

	var list FeedbackResponse
	json.Unmarshal(serveOn(s, http.MethodGet, "/api/feedback", "", nil).Body.Bytes(), &list)
	if len(list.Feedback) != 2 || list.Feedback[0].ID != fb.ID || !strings.HasPrefix(list.Feedback[1].Title, "second:") {
		t.Fatalf("list = %+v", list.Feedback)
	}
	var one Feedback
	json.Unmarshal(serveOn(s, http.MethodGet, "/api/feedback?id="+fb.ID, "", nil).Body.Bytes(), &one)
	if one.ProposedFix != "search the whole tree" {
		t.Fatalf("one = %+v", one)
	}
	if w := serveOn(s, http.MethodGet, "/api/feedback?id=../x", "", nil); w.Code != http.StatusBadRequest {
		t.Errorf("../x: status = %d", w.Code)
	}
	if w := serveOn(s, http.MethodGet, "/api/feedback?id=1-99", "", nil); w.Code != http.StatusNotFound {
		t.Errorf("missing id: status = %d", w.Code)
	}
}

func TestFeedbackIsRefusedWithoutItsRequiredFields(t *testing.T) {
	s, _ := feedbackServer(t, nil)
	for _, body := range []string{
		`{"title":"x","problem":"y"}`,
		`{"problem":"y","proposedFix":"z"}`,
		`{"title":"two\nlines","problem":"y","proposedFix":"z"}`,
		`{"title":"x","problem":"y","proposedFix":"z","target":"-rf"}`,
		`{"title":"x","problem":"y","proposedFix":"z","severity":"high"}`,
		`{"title":"x","problem":"` + strings.Repeat("a", maxFeedbackField+1) + `","proposedFix":"z"}`,
	} {
		if w := serveOn(s, http.MethodPost, "/api/feedback", body, nil); w.Code != http.StatusBadRequest {
			t.Errorf("%.60s: status = %d, want 400", body, w.Code)
		}
	}
}

func TestFeedbackToACommandRunsItWithTheReportOnStdin(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "gh")
	os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+dir+"/args\ncat > "+dir+"/stdin\necho https://github.com/o/r/issues/7\n"), 0o755)
	s, _ := feedbackServer(t, &dest.File{
		Commands: []dest.Command{{Name: "gh", Program: script, Args: []string{"--title-from", "{text}", "--body-file", "{message}"}}},
		Feedback: &dest.FeedbackTo{Command: "gh"},
	})
	fb := postFeedback(t, s, feedbackBody)
	if fb.Delivery == nil || fb.Delivery.State != "sending" {
		t.Fatalf("delivery = %+v", fb.Delivery)
	}
	s.feedback.sending.Wait()
	got, _ := readFeedback(fb.Dir)
	if got.Delivery == nil || got.Delivery.State != "sent" || got.Delivery.Output != "https://github.com/o/r/issues/7" {
		t.Fatalf("delivery = %+v", got.Delivery)
	}
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	msg := string(stdin)
	if !strings.HasPrefix(msg, "Feedback on loupe from claude (loupe v9).") {
		t.Errorf("message must open with the server's words:\n%s", msg)
	}
	for _, want := range []string{"# tap by --text", "## Problem\n\ntap returned 404", "## Repro", "## Proposed fix\n\nsearch the whole tree", "Target: avd:Pixel_10"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if !strings.Contains(string(args), "--body-file\n"+filepath.Join(fb.Dir, "message.md")) {
		t.Errorf("args = %q", args)
	}
	if b, _ := os.ReadFile(filepath.Join(fb.Dir, "message.md")); string(b) != msg {
		t.Errorf("message.md differs from stdin")
	}
}

func TestFeedbackToAWebhookCarriesSlackText(t *testing.T) {
	var got map[string]json.RawMessage
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		io.WriteString(w, "ok")
	}))
	defer hook.Close()
	s, _ := feedbackServer(t, &dest.File{
		Webhooks: []dest.Webhook{{Name: "slack", URL: hook.URL}},
		Feedback: &dest.FeedbackTo{Webhook: "slack"},
	})
	fb := postFeedback(t, s, feedbackBody)
	s.feedback.sending.Wait()
	if d, _ := readFeedback(fb.Dir); d.Delivery == nil || d.Delivery.State != "sent" {
		t.Fatalf("delivery = %+v", d.Delivery)
	}
	var text string
	var sent Feedback
	json.Unmarshal(got["text"], &text)
	json.Unmarshal(got["feedback"], &sent)
	if !strings.Contains(text, "## Proposed fix") || sent.ID != fb.ID || sent.Delivery != nil || sent.Dir != "" {
		t.Fatalf("text %q, feedback %+v", text, sent)
	}
}

func TestFeedbackKeepsTheNewestReports(t *testing.T) {
	s, _ := feedbackServer(t, nil)
	for range keptFeedback + 3 {
		postFeedback(t, s, feedbackBody)
	}
	ents, _ := os.ReadDir(s.feedback.dir)
	if len(ents) != keptFeedback {
		t.Fatalf("kept %d, want %d", len(ents), keptFeedback)
	}
}
