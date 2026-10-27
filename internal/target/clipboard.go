package target

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/scrcpy"
)

// Clipboarder is implemented by targets whose clipboard loupe can read and
// set. An Android device or emulator reaches it through the open scrcpy
// session, so it needs one open.
type Clipboarder interface {
	ReadClipboard(ctx context.Context) (string, error)
	// WriteClipboard sets the clipboard, and with paste also pastes it into
	// whatever has focus.
	WriteClipboard(ctx context.Context, text string, paste bool) error
}

// ClipboardWatcher is implemented by streams that report the device's
// clipboard each time it changes, so a viewer can be offered what was
// copied on the device without asking for it. The channel closes with the
// stream.
type ClipboardWatcher interface {
	ClipboardChanges() <-chan string
}

// ErrClipboardNoSession is returned when an Android clipboard is asked for
// and no stream is open: scrcpy-server is the only thing on the device that
// reads or sets it.
var ErrClipboardNoSession = errors.New("no session is open on this device; its clipboard is reached through one")

// clipboardReadTimeout bounds a get-clipboard round trip. The answer comes
// back up the control socket in the time a control message takes, so a
// wait longer than this is a server that is not going to answer.
const clipboardReadTimeout = 2 * time.Second

// clipState is an Android stream's view of the device clipboard, fed by
// every clipboard message the server sends.
type clipState struct {
	mu      sync.Mutex
	text    string
	known   bool
	updated chan struct{} // closed and replaced on each message
	changes chan string
}

func newClipState() *clipState {
	return &clipState{updated: make(chan struct{}), changes: make(chan string, 4)}
}

// watch consumes the session's clipboard messages until it ends. A message
// is offered as a change only when its text differs from the last one, so
// the answer to a read, or the device echoing back what a viewer just set,
// is not announced as something copied on the device.
func (c *clipState) watch(in <-chan string) {
	defer close(c.changes)
	for text := range in {
		c.mu.Lock()
		changed := !c.known || text != c.text
		c.text, c.known = text, true
		close(c.updated)
		c.updated = make(chan struct{})
		c.mu.Unlock()
		if !changed {
			continue
		}
		select {
		case c.changes <- text:
		default:
			// A viewer behind by four clipboards wants the newest.
			select {
			case <-c.changes:
			default:
			}
			select {
			case c.changes <- text:
			default:
			}
		}
	}
}

// set records text as the clipboard, so the device echoing it back is not
// taken for a copy on the device.
func (c *clipState) set(text string) {
	c.mu.Lock()
	c.text, c.known = text, true
	c.mu.Unlock()
}

func (c *clipState) waitChan() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.updated
}

func (c *clipState) current() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text
}

// ClipboardChanges implements ClipboardWatcher.
func (s *adbStream) ClipboardChanges() <-chan string { return s.clip.changes }

func (s *adbStream) readClipboard(ctx context.Context) (string, error) {
	wait := s.clip.waitChan()
	if err := s.sess.SendControl(scrcpy.AppendGetClipboard(nil, 0)); err != nil {
		return "", fmt.Errorf("get clipboard: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, clipboardReadTimeout)
	defer cancel()
	select {
	case <-wait:
		return s.clip.current(), nil
	case <-ctx.Done():
		return "", fmt.Errorf("get clipboard: the device did not answer: %w", ctx.Err())
	}
}

func (s *adbStream) writeClipboard(text string, paste bool) error {
	msg, err := scrcpy.AppendSetClipboard(nil, 0, text, paste)
	if err != nil {
		return err
	}
	s.clip.set(text)
	// Paste lands in the focused field the way typed text does, so it waits
	// behind a burst the same way a key does; see textSettleTime.
	if paste {
		s.waitForText()
	}
	if err := s.sess.SendControl(msg); err != nil {
		return fmt.Errorf("set clipboard: %w", err)
	}
	return nil
}

// ReadClipboard implements Clipboarder.
func (t *ADBTarget) ReadClipboard(ctx context.Context) (string, error) {
	s := t.liveStream()
	if s == nil {
		return "", ErrClipboardNoSession
	}
	return s.readClipboard(ctx)
}

// WriteClipboard implements Clipboarder.
func (t *ADBTarget) WriteClipboard(ctx context.Context, text string, paste bool) error {
	s := t.liveStream()
	if s == nil {
		return ErrClipboardNoSession
	}
	return s.writeClipboard(text, paste)
}

func (e *Emulator) ReadClipboard(ctx context.Context) (string, error) {
	in, done, err := e.running()
	if err != nil {
		return "", err
	}
	defer done()
	return in.ReadClipboard(ctx)
}

func (e *Emulator) WriteClipboard(ctx context.Context, text string, paste bool) error {
	in, done, err := e.running()
	if err != nil {
		return err
	}
	defer done()
	return in.WriteClipboard(ctx, text, paste)
}
