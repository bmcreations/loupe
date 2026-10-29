package clipshare

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// MaxBytes bounds a shared value, the same 64 KiB /api/clipboard takes.
const MaxBytes = 64 << 10

// hostPasteboardRestart is how long a helper that exited waits before the
// next start. A var so tests need not wait it out.
var hostPasteboardRestart = 2 * time.Second

// RunHostPasteboard mirrors this Mac's general pasteboard into s while s is
// enabled, through `bin -hostpasteboard` (simcap's `hostpasteboard.swift`).
// A copy on the Mac is published as origin mac, and every other shared value
// is set on the Mac. Turning sharing off stops the helper; one that exits on
// its own is started again after a pause. A simcap whose -caps lacks
// hostpasteboard is never started. It returns when ctx ends.
//
// While the helper is available s reports this Mac's HostID (MirroredHost),
// so a client can tell this server's Mac from its own.
//
// trace gets lengths, never text.
func RunHostPasteboard(ctx context.Context, s *Service, bin string, trace func(map[string]any)) {
	if trace == nil {
		trace = func(map[string]any) {}
	}
	id := HostID(ctx)
	defer s.setMirroredHost("")
	// Checked before sharing is on too, so a client that turns it on and
	// asks at once is already told.
	s.setMirroredHost(hostIfAvailable(ctx, bin, id))
	for ctx.Err() == nil {
		if !waitEnabled(ctx, s, true) {
			return
		}
		// Whether the helper runs is the helper's question, not the id's:
		// the id is empty when this Mac's UUID cannot be read, and the
		// helper still runs then.
		available := hasHostPasteboard(ctx, bin)
		if available {
			s.setMirroredHost(id)
		} else {
			s.setMirroredHost("")
		}
		if !available {
			trace(map[string]any{"kind": "clipboard", "from": "mac", "error": bin + " has no hostpasteboard mode (make simcap)"})
			if !waitEnabled(ctx, s, false) {
				return
			}
			continue
		}
		exited := runHostPasteboardOnce(ctx, s, bin, trace)
		if !exited {
			continue
		}
		select {
		case <-time.After(hostPasteboardRestart):
		case <-ctx.Done():
			return
		}
	}
}

func hostIfAvailable(ctx context.Context, bin, id string) string {
	if id != "" && hasHostPasteboard(ctx, bin) {
		return id
	}
	return ""
}

// HostID names this Mac without revealing its hardware UUID: the SHA-256 of
// it under a fixed prefix, hex. Handset computes the same, so it can tell a
// server that mirrors its own Mac's pasteboard from one that mirrors another
// Mac's. It is empty when the UUID cannot be read.
func HostID(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "IOPlatformUUID") {
			continue
		}
		if f := strings.Split(line, `"`); len(f) >= 5 {
			sum := sha256.Sum256([]byte("loupe-host:" + f[3]))
			return hex.EncodeToString(sum[:])
		}
	}
	return ""
}

// waitEnabled blocks until s.Enabled() is on, and reports false if ctx ended
// first.
func waitEnabled(ctx context.Context, s *Service, on bool) bool {
	for {
		ch := s.changes()
		if s.Enabled() == on {
			return true
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
}

func hasHostPasteboard(ctx context.Context, bin string) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "-caps").Output()
	if err != nil {
		return false
	}
	for _, c := range strings.Fields(string(out)) {
		if c == "hostpasteboard" {
			return true
		}
	}
	return false
}

// runHostPasteboardOnce runs one helper until it exits, sharing is turned
// off, or ctx ends. It reports whether the helper exited on its own.
func runHostPasteboardOnce(ctx context.Context, s *Service, bin string, trace func(map[string]any)) bool {
	cmd := exec.Command(bin, "-hostpasteboard")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		trace(map[string]any{"kind": "clipboard", "from": "mac", "error": err.Error()})
		return true
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		trace(map[string]any{"kind": "clipboard", "from": "mac", "error": err.Error()})
		return true
	}
	if err := cmd.Start(); err != nil {
		trace(map[string]any{"kind": "clipboard", "from": "mac", "error": err.Error()})
		return true
	}

	var inMu sync.Mutex
	cancelSub := s.Subscribe(func(v Value) {
		if v.Origin == "mac" {
			return
		}
		inMu.Lock()
		_, err := io.WriteString(stdin, "set "+base64.StdEncoding.EncodeToString([]byte(v.Text))+"\n")
		inMu.Unlock()
		rec := map[string]any{"kind": "clipboard", "target": "mac", "from": "shared"}
		if err != nil {
			rec["error"] = err.Error()
		} else {
			rec["textLen"] = utf8.RuneCountInString(v.Text)
		}
		trace(rec)
	})
	defer cancelSub()

	exited := make(chan struct{})
	go func() {
		defer close(exited)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if line == "confidential" {
				// A password manager's copy: it stays on this Mac.
				trace(map[string]any{"kind": "clipboard", "from": "mac", "confidential": true})
				continue
			}
			rest, ok := strings.CutPrefix(line, "text ")
			if !ok {
				continue
			}
			b, err := base64.StdEncoding.DecodeString(rest)
			if err != nil || !utf8.Valid(b) || len(b) == 0 {
				continue
			}
			if len(b) > MaxBytes {
				trace(map[string]any{"kind": "clipboard", "from": "mac", "error": "copy over 64 KiB not shared", "bytes": len(b)})
				continue
			}
			if v, ok := s.Publish(Value{Text: string(b), Origin: "mac"}); ok {
				trace(map[string]any{"kind": "clipboard", "from": "mac", "textLen": utf8.RuneCount(b), "seq": v.Seq})
			}
		}
		_ = cmd.Wait()
	}()

	stop := func() {
		// Closing stdin is how simcap is told to exit, as simclipboard.go's
		// stop does.
		inMu.Lock()
		_ = stdin.Close()
		inMu.Unlock()
		select {
		case <-exited:
		case <-time.After(time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	}
	for {
		ch := s.changes()
		if !s.Enabled() {
			stop()
			return false
		}
		select {
		case <-exited:
			return true
		case <-ctx.Done():
			stop()
			return false
		case <-ch:
		}
	}
}
