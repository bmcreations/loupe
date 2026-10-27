package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Config is the extension's settings.
type Config struct {
	Addr          string
	LoupeURL      string
	Token         string
	BuildCmd      string
	BuildDir      string
	DataDir       string
	BuildTimeout  time.Duration
	ReportTimeout time.Duration
	PollWindow    time.Duration
	SettleTimeout time.Duration
}

// Server speaks the hotswap protocol the debug apps poll, and runs /apply.
type Server struct {
	cfg        Config
	patchesDir string
	shotsDir   string
	loupe      *loupeClient

	applyMu sync.Mutex // one apply at a time

	mu        sync.Mutex
	counter   int64          // last seq handed out; seeded from Unix time
	latest    map[string]any // the current notice; {"seq":0} until the first publish
	changed   chan struct{}  // closed and replaced on each publish
	inflight  int            // /next requests currently waiting
	lastPoll  time.Time      // when a /next last started or ended
	waiters   map[int64]chan map[string]any
	reportLog *os.File
}

func NewServer(cfg Config) (*Server, error) {
	s := &Server{
		cfg:        cfg,
		patchesDir: filepath.Join(cfg.DataDir, "patches"),
		shotsDir:   filepath.Join(cfg.DataDir, "shots"),
		counter:    time.Now().Unix(),
		latest:     map[string]any{"seq": float64(0)},
		changed:    make(chan struct{}),
		waiters:    map[int64]chan map[string]any{},
	}
	for _, d := range []string{s.patchesDir, s.shotsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(filepath.Join(cfg.DataDir, "reports.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	s.reportLog = f
	s.loupe = newLoupeClient(cfg.LoupeURL, cfg.Token)
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /seq", s.handleSeq)
	mux.HandleFunc("GET /next", s.handleNext)
	mux.HandleFunc("POST /publish", s.handlePublish)
	mux.HandleFunc("POST /report", s.handleReport)
	mux.HandleFunc("POST /apply", s.handleApply)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "polling": s.polling()})
	})
	mux.Handle("GET /patches/", http.StripPrefix("/patches/", http.FileServer(http.Dir(s.patchesDir))))
	mux.Handle("GET /shots/", http.StripPrefix("/shots/", http.FileServer(http.Dir(s.shotsDir))))
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(code)
	w.Write(b)
}

func (s *Server) currentSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counter
}

// handleSeq answers the counter, not the latest notice's seq: the iOS loader
// names modules Live_<seq> and needs them unique per process, and the Android
// loader uses it to skip patches published before it launched.
func (s *Server) handleSeq(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"seq": s.currentSeq()})
}

func noticeSeq(n map[string]any) int64 {
	f, _ := n["seq"].(float64)
	return int64(f)
}

func (s *Server) handleNext(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	s.mu.Lock()
	s.inflight++
	s.lastPoll = time.Now()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inflight--
		s.lastPoll = time.Now()
		s.mu.Unlock()
	}()
	for {
		s.mu.Lock()
		n, ch := s.latest, s.changed
		s.mu.Unlock()
		if noticeSeq(n) > after {
			writeJSON(w, 200, n)
			return
		}
		select {
		case <-ch:
		case <-deadline.C:
			w.WriteHeader(204)
			return
		case <-r.Context().Done():
			return
		}
	}
}

// polling reports whether an app is waiting on /next, or was a moment ago.
// The app's long poll ends every 30s and restarts at once, so the gap between
// two polls is short.
func (s *Server) polling() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight > 0 || (!s.lastPoll.IsZero() && time.Since(s.lastPoll) < s.cfg.PollWindow)
}

func (s *Server) publish(rec map[string]any) {
	rec["publishedAt"] = time.Now().UnixMilli()
	s.mu.Lock()
	if seq := noticeSeq(rec); seq > s.counter {
		s.counter = seq
	}
	s.latest = rec
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	rec := map[string]any{}
	if json.Unmarshal(raw, &rec) != nil || noticeSeq(rec) == 0 {
		writeJSON(w, 400, map[string]any{"ok": false, "code": "badNotice", "error": "body must be a JSON object with a seq"})
		return
	}
	s.publish(rec)
	w.WriteHeader(204)
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	rec := map[string]any{}
	if json.Unmarshal(raw, &rec) != nil {
		rec = map[string]any{"raw": string(raw)}
	}
	rec["receivedAt"] = time.Now().UnixMilli()
	line, _ := json.Marshal(rec)
	s.mu.Lock()
	s.reportLog.Write(append(line, '\n'))
	if ev, _ := rec["event"].(string); ev == "patch" {
		seq := noticeSeq(rec)
		if ch, ok := s.waiters[seq]; ok {
			select {
			case ch <- rec:
			default:
			}
		}
	}
	s.mu.Unlock()
	w.WriteHeader(204)
}

// expectReport registers interest in the patch report for seq. Call it before
// publishing, so a fast app cannot report first.
func (s *Server) expectReport(seq int64) (<-chan map[string]any, func()) {
	ch := make(chan map[string]any, 1)
	s.mu.Lock()
	s.waiters[seq] = ch
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.waiters, seq)
		s.mu.Unlock()
	}
}

func (s *Server) nextSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counter++
	return s.counter
}
