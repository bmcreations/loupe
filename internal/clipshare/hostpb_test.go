package clipshare

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeSimcap writes a shell script standing in for simcap. -caps prints
// caps. -hostpasteboard records its pid, prints each of out, then appends
// every stdin line to dir/in until stdin closes.
func fakeSimcap(t *testing.T, caps string, out ...string) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "out"), []byte(strings.Join(out, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
d=%q
if [ "$1" = "-caps" ]; then printf '%%s\n' %s; exit 0; fi
if [ "$1" != "-hostpasteboard" ]; then exit 2; fi
echo $$ > "$d/pid"
cat "$d/out"
while read -r line; do echo "$line" >> "$d/in"; done
`, dir, caps)
	bin = filepath.Join(dir, "simcap")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

type traces struct {
	mu   sync.Mutex
	recs []map[string]any
}

func (tr *traces) record(r map[string]any) {
	tr.mu.Lock()
	tr.recs = append(tr.recs, r)
	tr.mu.Unlock()
}

func (tr *traces) all() []map[string]any {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]map[string]any(nil), tr.recs...)
}

func runHost(t *testing.T, s *Service, bin string) *traces {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	tr := &traces{}
	go func() {
		defer close(done)
		RunHostPasteboard(ctx, s, bin, tr.record)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return tr
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("never: %s", what)
}

func readPid(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

func TestHostPasteboardPublishes(t *testing.T) {
	bin, _ := fakeSimcap(t, "video hostpasteboard", "text "+b64("hi"))
	s := enabled(t)
	tr := runHost(t, s, bin)
	waitFor(t, "mac copy published", func() bool { cur, ok := s.Current(); return ok && cur.Text == "hi" })
	if cur, _ := s.Current(); cur.Origin != "mac" {
		t.Fatalf("origin = %q, want mac", cur.Origin)
	}
	for _, r := range tr.all() {
		for _, v := range r {
			if v == "hi" {
				t.Fatalf("the text reached the trace: %v", r)
			}
		}
	}
}

// A password manager's copy is `confidential`, with no text, and never
// becomes a shared value.
func TestHostPasteboardSkipsConfidential(t *testing.T) {
	bin, _ := fakeSimcap(t, "hostpasteboard", "confidential", "text "+b64("after"))
	s := enabled(t)
	runHost(t, s, bin)
	waitFor(t, "the later copy published", func() bool { cur, ok := s.Current(); return ok && cur.Text == "after" })
	if cur, _ := s.Current(); cur.Seq != 1 {
		t.Fatalf("seq = %d, want 1: the confidential copy was published", cur.Seq)
	}
}

func TestHostPasteboardWritesShared(t *testing.T) {
	bin, dir := fakeSimcap(t, "hostpasteboard", "text "+b64("from mac"))
	s := enabled(t)
	runHost(t, s, bin)
	waitFor(t, "mac copy published", func() bool { _, ok := s.Current(); return ok })
	s.Publish(Value{Text: "from a", Origin: "target:a"})
	in := func() string { b, _ := os.ReadFile(filepath.Join(dir, "in")); return string(b) }
	waitFor(t, "set written", func() bool { return strings.Contains(in(), "set "+b64("from a")) })
	if strings.Contains(in(), b64("from mac")) {
		t.Fatalf("the Mac's own copy was written back: %q", in())
	}
}

func TestHostPasteboardStopsWhenDisabled(t *testing.T) {
	bin, dir := fakeSimcap(t, "hostpasteboard")
	s := enabled(t)
	runHost(t, s, bin)
	waitFor(t, "helper started", func() bool { return readPid(dir) != 0 })
	pid := readPid(dir)
	s.SetEnabled(false)
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
	}
	t.Fatalf("helper %d still running 2s after sharing was turned off", pid)
}

func TestHostPasteboardStartsWhenEnabled(t *testing.T) {
	bin, dir := fakeSimcap(t, "hostpasteboard")
	s := New()
	runHost(t, s, bin)
	time.Sleep(50 * time.Millisecond)
	if readPid(dir) != 0 {
		t.Fatal("helper started while sharing was off")
	}
	s.SetEnabled(true)
	waitFor(t, "helper started", func() bool { return readPid(dir) != 0 })
}

func TestHostPasteboardNoCap(t *testing.T) {
	bin, dir := fakeSimcap(t, "video input pasteboard")
	s := enabled(t)
	runHost(t, s, bin)
	time.Sleep(200 * time.Millisecond)
	if readPid(dir) != 0 {
		t.Fatal("started -hostpasteboard on a simcap without it")
	}
}

func TestHostPasteboardRestartsAfterExit(t *testing.T) {
	old := hostPasteboardRestart
	hostPasteboardRestart = 20 * time.Millisecond
	t.Cleanup(func() { hostPasteboardRestart = old })
	bin, dir := fakeSimcap(t, "hostpasteboard")
	s := enabled(t)
	runHost(t, s, bin)
	waitFor(t, "helper started", func() bool { return readPid(dir) != 0 })
	first := readPid(dir)
	_ = syscall.Kill(first, syscall.SIGKILL)
	waitFor(t, "helper restarted", func() bool { p := readPid(dir); return p != 0 && p != first })
}
