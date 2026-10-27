package target

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// A simulator's pasteboard is reached through CoreDevice by simcap, the
// route Device Hub syncs the Mac's clipboard through; `simctl pbcopy` exits
// 0 on Xcode 27 and sets nothing. See simcap's `pasteboard.swift`.

// ReadClipboard implements Clipboarder through the input process.
func (s *Simulator) ReadClipboard(ctx context.Context) (string, error) {
	if err := s.requireBooted(); err != nil {
		return "", err
	}
	b64, err := s.input().do(ctx, 0, "pbget")
	if err != nil {
		return "", fmt.Errorf("read pasteboard: %w", err)
	}
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("read pasteboard: %w", err)
	}
	return string(b), nil
}

// WriteClipboard implements Clipboarder. Paste types the text rather than
// pressing Cmd+V: iOS asks the user before an app reads a pasteboard another
// process set, and Home Screen Search refused a Cmd+V with "Paste requires
// user authorization" on loupe-probe. Typing needs no permission, and
// TypeText fails before sending anything when a character has no key.
func (s *Simulator) WriteClipboard(ctx context.Context, text string, paste bool) error {
	if err := s.requireBooted(); err != nil {
		return err
	}
	s.pbSet.Store(&text)
	if _, err := s.input().do(ctx, 0, "pbset %s", base64.StdEncoding.EncodeToString([]byte(text))); err != nil {
		return fmt.Errorf("set pasteboard: %w", err)
	}
	if paste {
		return s.TypeText(ctx, text)
	}
	return nil
}

// echoOfSet reports whether a change is the text a viewer just set, and
// forgets it either way: only the change right after a set can be its echo.
func (s *Simulator) echoOfSet(text string) bool {
	p := s.pbSet.Swap(nil)
	return p != nil && *p == text
}

// ClipboardChanges implements ClipboardWatcher. A build without the watcher
// never reports a change.
func (st *simStream) ClipboardChanges() <-chan string {
	if st.clip == nil {
		return nil
	}
	return st.clip.changes
}

// simPasteboard is `simcap <udid> -pasteboard`, which prints a line each
// time the simulator's pasteboard changes. It runs beside one video stream
// and stops with it.
type simPasteboard struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	changes chan string
	exited  chan struct{}
}

// watchSimPasteboard starts the watcher. A watcher that fails to start is
// not worth the stream: the channel closes and the viewer is offered no
// copies.
func watchSimPasteboard(bin, udid string, echo func(string) bool) *simPasteboard {
	p := &simPasteboard{changes: make(chan string, 4), exited: make(chan struct{})}
	cmd := exec.Command(bin, udid, "-pasteboard")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		close(p.changes)
		close(p.exited)
		return p
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		close(p.changes)
		close(p.exited)
		return p
	}
	p.cmd, p.stdin = cmd, stdin
	go func() {
		defer close(p.exited)
		defer close(p.changes)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			// A copy marked confidential, a password manager's, is `confidential`
			// with no text, and is not offered to anyone.
			rest, ok := strings.CutPrefix(sc.Text(), "text ")
			if !ok {
				continue
			}
			b, err := base64.StdEncoding.DecodeString(rest)
			if err != nil || echo(string(b)) {
				continue
			}
			select {
			case p.changes <- string(b):
			default:
				// Behind by four copies; the viewer wants the newest.
				select {
				case <-p.changes:
				default:
				}
				select {
				case p.changes <- string(b):
				default:
				}
			}
		}
		_ = cmd.Wait()
	}()
	return p
}

// stop closes the watcher's stdin, which is how simcap is told to exit.
func (p *simPasteboard) stop() {
	if p.cmd == nil {
		return
	}
	_ = p.stdin.Close()
	select {
	case <-p.exited:
	case <-time.After(time.Second):
		_ = p.cmd.Process.Kill()
		<-p.exited
	}
}
