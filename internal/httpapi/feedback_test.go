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
	"time"

	"github.com/bmcreations/loupe/internal/config"
	"github.com/bmcreations/loupe/internal/dest"
	"github.com/bmcreations/loupe/internal/t3"
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

func TestFeedbackTargetRefusalNamesTheExpectedForm(t *testing.T) {
	s, _ := feedbackServer(t, nil)
	body := `{"title":"x","problem":"y","proposedFix":"z","target":"android (USB physical device)"}`
	w := serveOn(s, http.MethodPost, "/api/feedback", body, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	var got struct{ Error string }
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"target id", "avd:<name>", "sim:<udid>", "serial"} {
		if !strings.Contains(got.Error, want) {
			t.Errorf("error %q does not name %q", got.Error, want)
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

// withSettings gives s a settings store holding cfg.
func withSettings(s *Server, dir string, cfg config.Config) {
	s.cfg = NewConfigStore(filepath.Join(dir, "config.json"), cfg, nil, cfg, func(config.Config) {})
}

// feedbackT3 is a feedback server whose destinations.json sends reports to
// a T3 project, with the T3 CLI a script that records its arguments.
func feedbackT3(t *testing.T, project string) (*Server, string) {
	t.Helper()
	s, dir := feedbackServer(t, &dest.File{Feedback: &dest.FeedbackTo{T3Project: project}})
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + dir + "/calls\ncat > " + dir + "/sent\necho '{\"threadId\":\"9cfd27bc-1c9e-46f4-be88-3144e57d7707\"}'\n"
	p := filepath.Join(dir, "t3")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s.findT3 = func() (t3.CLI, error) { return t3.CLI{Path: p}, nil }
	cfg := config.Default()
	cfg.T3Projects = "/work/loupe"
	withSettings(s, dir, cfg)
	return s, dir
}

func TestFeedbackToT3StartsAFixThreadInAWorktree(t *testing.T) {
	s, dir := feedbackT3(t, "/work/loupe")
	fb := postFeedback(t, s, feedbackBody)
	s.feedback.sending.Wait()
	got, _ := readFeedback(fb.Dir)
	if got.Delivery == nil || got.Delivery.State != "sent" || got.Delivery.Thread != "9cfd27bc-1c9e-46f4-be88-3144e57d7707" {
		t.Fatalf("delivery = %+v", got.Delivery)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	args := strings.Split(strings.TrimSpace(string(calls)), "\n")
	want := []string{"thread", "start", "--json", "--title", "Loupe feedback: tap by --text misses a node under a scroll view", "--worktree", "main", "/work/loupe", "-"}
	if strings.Join(args, "|") != strings.Join(want, "|") {
		t.Fatalf("t3 args = %q, want %q", args, want)
	}
	sent, _ := os.ReadFile(filepath.Join(dir, "sent"))
	msg := string(sent)
	if !strings.HasPrefix(msg, "Feedback on loupe from claude") {
		t.Errorf("message must open with the server's words:\n%s", msg)
	}
	for _, want := range []string{"## Proposed fix\n\nsearch the whole tree", "written by an agent, not by the person", "Do not run make build"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
}

func TestFeedbackToT3IsRefusedOutsideT3Projects(t *testing.T) {
	s, dir := feedbackT3(t, "/work/other")
	fb := postFeedback(t, s, feedbackBody)
	s.feedback.sending.Wait()
	got, _ := readFeedback(fb.Dir)
	if got.Delivery == nil || got.Delivery.State != "failed" || !strings.Contains(got.Delivery.Error, "not in the t3Projects setting") {
		t.Fatalf("delivery = %+v", got.Delivery)
	}
	if _, err := os.Stat(filepath.Join(dir, "calls")); err == nil {
		t.Fatal("t3 ran for a project outside the setting")
	}
}

// A fix thread that drives loupe can file a report of its own, which would
// start another thread: reports start at most feedbackThreadsPerHour.
func TestFeedbackToT3StartsAFewThreadsAnHour(t *testing.T) {
	s, _ := feedbackT3(t, "/work/loupe")
	var last Feedback
	for range feedbackThreadsPerHour + 1 {
		last = postFeedback(t, s, feedbackBody)
		s.feedback.sending.Wait()
	}
	got, _ := readFeedback(last.Dir)
	if got.Delivery == nil || got.Delivery.State != "failed" || !strings.Contains(got.Delivery.Error, "in the last hour") {
		t.Fatalf("delivery past the cap = %+v", got.Delivery)
	}
	if !s.takeFeedbackThread(time.Now().Add(time.Hour + time.Minute)) {
		t.Fatal("cap did not lift after an hour")
	}
}

func TestFeedbackStaysLocalWithTheRelayOff(t *testing.T) {
	hit := false
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer relay.Close()
	s, dir := feedbackServer(t, nil)
	s.feedback.relay = relay.URL
	withSettings(s, dir, config.Default())
	fb := postFeedback(t, s, feedbackBody)
	s.feedback.sending.Wait()
	if fb.Delivery != nil || hit {
		t.Fatalf("delivery = %+v, relay hit = %v", fb.Delivery, hit)
	}
}

func TestFeedbackRelaySendsTheWordsAndNotTheDevice(t *testing.T) {
	var body []byte
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"issue":"https://github.com/bmcreations/loupe/issues/12"}`))
	}))
	defer relay.Close()
	s, dir := feedbackServer(t, nil)
	s.feedback.relay = relay.URL
	cfg := config.Default()
	cfg.FeedbackRelay = true
	withSettings(s, dir, cfg)
	fb := postFeedback(t, s, feedbackBody)
	if fb.Delivery == nil || fb.Delivery.State != "sending" {
		t.Fatalf("delivery = %+v", fb.Delivery)
	}
	s.feedback.sending.Wait()
	got, _ := readFeedback(fb.Dir)
	if got.Delivery == nil || got.Delivery.State != "sent" || got.Delivery.Output != "https://github.com/bmcreations/loupe/issues/12" {
		t.Fatalf("delivery = %+v", got.Delivery)
	}
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("relay body %s: %v", body, err)
	}
	if sent["title"] != "tap by --text misses a node under a scroll view" || sent["proposedFix"] != "search the whole tree" || sent["version"] != "v9" {
		t.Errorf("relay body = %s", body)
	}
	// The agent's own words may name a device; loupe adds none of its own.
	for _, k := range []string{"target", "agent", "session", "dir", "id", "t"} {
		if _, ok := sent[k]; ok {
			t.Errorf("relay body carries %q: %s", k, body)
		}
	}
	if strings.Contains(string(body), dir) {
		t.Errorf("relay body carries the local path: %s", body)
	}
}

func TestFeedbackRelayErrorIsRecorded(t *testing.T) {
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer relay.Close()
	s, dir := feedbackServer(t, nil)
	s.feedback.relay = relay.URL
	cfg := config.Default()
	cfg.FeedbackRelay = true
	withSettings(s, dir, cfg)
	fb := postFeedback(t, s, feedbackBody)
	s.feedback.sending.Wait()
	got, _ := readFeedback(fb.Dir)
	if got.Delivery == nil || got.Delivery.State != "failed" || !strings.Contains(got.Delivery.Error, "429") || got.Delivery.Output != "slow down" {
		t.Fatalf("delivery = %+v", got.Delivery)
	}
}
