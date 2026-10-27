package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type buildResult struct {
	notice map[string]any
	output string // stderr, and stdout without the notice line
}

// runBuild spawns the project's build command and reads the notice from the
// last line of its stdout.
func (s *Server) runBuild(ctx context.Context, seq int64) (*buildResult, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.BuildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", s.cfg.BuildCmd)
	cmd.Dir = s.cfg.BuildDir
	cmd.Env = append(os.Environ(), "HOTSWAP_SEQ="+strconv.FormatInt(seq, 10), "HOTSWAP_PATCHES="+s.patchesDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	last := ""
	if len(lines) > 0 {
		last = strings.TrimSpace(lines[len(lines)-1])
		lines = lines[:len(lines)-1]
	}
	out := tail(strings.TrimSpace(strings.Join(lines, "\n")+"\n"+stderr.String()), 4000)
	if ctx.Err() == context.DeadlineExceeded {
		return &buildResult{output: out}, fmt.Errorf("build timed out after %s", s.cfg.BuildTimeout)
	}
	if err != nil {
		return &buildResult{output: out}, fmt.Errorf("build failed: %v", err)
	}
	n := map[string]any{}
	if json.Unmarshal([]byte(last), &n) != nil {
		return &buildResult{output: out}, fmt.Errorf("build printed no notice JSON on its last stdout line")
	}
	delete(n, "seq")
	return &buildResult{notice: n, output: out}, nil
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	if target == "" {
		writeJSON(w, 400, map[string]any{"ok": false, "code": "badRequest", "error": "target is required"})
		return
	}
	if !s.applyMu.TryLock() {
		writeJSON(w, 409, map[string]any{"ok": false, "code": "busy", "error": "an apply is already running"})
		return
	}
	defer s.applyMu.Unlock()
	start := time.Now()

	// Before the build: a build for an app that is not listening is wasted.
	if !s.polling() {
		writeJSON(w, 409, map[string]any{"ok": false, "code": "appNotPolling",
			"error": fmt.Sprintf("the app has not polled /next in the last %s; launch the debug build with its hotswap base pointing at this server", s.cfg.PollWindow)})
		return
	}

	// The screen before the patch, taken while the build runs, so the response can say whether
	// the patch changed anything. A dylib can load and a redraw finish with the old body still on
	// screen, when the view it replaces is not re-evaluated.
	before := make(chan []byte, 1)
	go func() {
		b, _ := s.loupe.screenshot(target, r.Header)
		before <- b
	}()

	seq := s.nextSeq()
	br, err := s.runBuild(r.Context(), seq)
	buildMs := time.Since(start).Milliseconds()
	if err != nil {
		writeJSON(w, 422, map[string]any{"ok": false, "code": "buildFailed", "error": err.Error(), "output": br.output, "buildMs": buildMs, "seq": seq})
		return
	}

	reports, done := s.expectReport(seq)
	defer done()
	notice := br.notice
	notice["seq"] = float64(seq)
	s.publish(notice)
	published := time.Now()

	var rep map[string]any
	select {
	case rep = <-reports:
	case <-time.After(s.cfg.ReportTimeout):
		writeJSON(w, 504, map[string]any{"ok": false, "code": "noReport", "seq": seq, "buildMs": buildMs,
			"error": fmt.Sprintf("the app sent no patch report for seq %d in %s", seq, s.cfg.ReportTimeout)})
		return
	case <-r.Context().Done():
		return
	}
	applyMs := time.Since(published).Milliseconds()
	if ok, _ := rep["ok"].(bool); !ok {
		writeJSON(w, 409, map[string]any{"ok": false, "code": "needsRebuild", "seq": seq, "buildMs": buildMs,
			"error": rep["error"], "report": rep})
		return
	}

	// The app reports once it has drawn the patch, which leaves it ahead of the screen by
	// at most a frame or two; shots until two in a row match cover that and an app that cannot tell.
	// The tree is read after, so it describes the settled screen.
	sh := s.settle(target, r.Header)
	var tr struct {
		raw json.RawMessage
		err error
	}
	tr.raw, tr.err = s.loupe.ui(target, r.Header)

	resp := map[string]any{"ok": true, "target": target, "seq": seq, "buildMs": buildMs, "applyMs": applyMs,
		"report": rep, "build": br.notice, "timings": timings(rep),
		"settle": map[string]any{"stable": sh.stable, "shots": sh.shots, "settleMs": sh.ms}}
	if changed, ok := pixelsChanged(<-before, sh.b); ok {
		resp["changed"] = changed
	}
	var errs []string
	if sh.err != nil {
		errs = append(errs, "screenshot: "+sh.err.Error())
	} else {
		name := fmt.Sprintf("%d-%s.png", seq, unsafeName.ReplaceAllString(target, "_"))
		path, _ := filepath.Abs(filepath.Join(s.shotsDir, name))
		if err := os.WriteFile(path, sh.b, 0o644); err != nil {
			errs = append(errs, "screenshot: "+err.Error())
		} else {
			resp["screenshot"] = map[string]any{"path": path, "url": "http://" + r.Host + "/shots/" + name, "bytes": len(sh.b)}
		}
	}
	if tr.err != nil {
		errs = append(errs, "ui: "+tr.err.Error())
	} else if tr.raw != nil {
		resp["ui"] = tr.raw
	}
	if len(errs) > 0 {
		// The patch is on the device; only the readback failed.
		resp["ok"] = false
		resp["code"] = "loupeError"
		resp["error"] = strings.Join(errs, "; ")
	}
	resp["totalMs"] = time.Since(start).Milliseconds()
	code := 200
	if len(errs) > 0 {
		code = 502
	}
	writeJSON(w, code, resp)
}

// timings picks the numeric fields an app reports about the patch.
func timings(rep map[string]any) map[string]any {
	t := map[string]any{}
	for _, k := range []string{"downloadMs", "redefineMs", "recomposeMs", "loadMs", "shownMs", "bytes", "classes"} {
		if v, ok := rep[k]; ok {
			t[k] = v
		}
	}
	return t
}

type settled struct {
	b      []byte
	err    error
	stable bool
	shots  int
	ms     int64
}

// settle takes screenshots until two in a row have the same pixels or SettleTimeout passes,
// and returns the last. A SettleTimeout of 0 takes one shot and trusts the app's report.
// Pixels, not PNG bytes: an encoder may write the same picture differently.
func (s *Server) settle(target string, from http.Header) settled {
	start := time.Now()
	var out settled
	var prev image.Image
	for {
		b, err := s.loupe.screenshot(target, from)
		out.shots++
		if err != nil {
			out.err = err
			break
		}
		out.b = b
		if s.cfg.SettleTimeout <= 0 {
			break
		}
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil {
			break
		}
		if prev != nil && samePixels(prev, img) {
			out.stable = true
			break
		}
		prev = img
		if time.Since(start) >= s.cfg.SettleTimeout {
			break
		}
	}
	out.ms = time.Since(start).Milliseconds()
	return out
}

// pixelsChanged reports whether two screenshots differ, and false for ok when either is missing
// or does not decode. The status bar counts: a clock that ticks between them reads as a change.
func pixelsChanged(a, b []byte) (changed, ok bool) {
	if a == nil || b == nil {
		return false, false
	}
	ia, err := png.Decode(bytes.NewReader(a))
	if err != nil {
		return false, false
	}
	ib, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return false, false
	}
	return !samePixels(ia, ib), true
}

func samePixels(a, b image.Image) bool {
	if a.Bounds() != b.Bounds() {
		return false
	}
	r := a.Bounds()
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if a.At(x, y) != b.At(x, y) {
				return false
			}
		}
	}
	return true
}
