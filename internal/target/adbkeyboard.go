package target

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/bmcreations/loupe/internal/scrcpy"
)

// A hardware keyboard on Android is a UHID device the scrcpy server creates
// on request. Android classes it as an alphabetic keyboard, and with one
// attached a focused field raises no on-screen keyboard -- the same effect
// `scrcpy --keyboard=uhid` has. Nothing is typed through it: it is there to
// be seen. Text and keys still arrive as inject-text and inject-keycode on
// the same control socket.
//
// The device belongs to one scrcpy server and goes when it exits, so a
// session that ends leaves nothing attached. The setting is kept on the
// target rather than on a stream because a bitrate relaunch replaces the
// server: the new stream opens before the old one closes, and creates the
// keyboard again from the setting. It is cleared when the last stream
// closes, so the keyboard lasts exactly as long as the session. Otherwise
// the next phone to connect would find its on-screen keyboard gone without
// having asked; a viewer that wants it sends the switch on every session.

// uhidKeyboardID names the one UHID device loupe creates. scrcpy's own
// client uses 1 for its keyboard and 2 for its mouse; loupe never runs
// beside it on the same server, so the value only has to be stable.
const uhidKeyboardID = 1

// uhidKeyboardName is what `dumpsys input` lists the device as, which is
// how to tell it from the emulator's own keyboards when checking.
const uhidKeyboardName = "loupe keyboard"

// ErrNoSession is returned when the keyboard is asked for and no stream is
// open to create it on: the keyboard lives in the scrcpy server, so there is
// nothing to attach it to.
var ErrNoSession = errors.New("no session is open on this device; the keyboard attaches when one is")

// SetHardwareKeyboard implements HardwareKeyboarder. It applies the setting
// to every open stream, and a stream opened while it holds applies it
// itself.
func (t *ADBTarget) SetHardwareKeyboard(ctx context.Context, on bool) error {
	t.kbMu.Lock()
	if len(t.streams) == 0 {
		t.kbMu.Unlock()
		if on {
			return ErrNoSession
		}
		return nil
	}
	t.hwKeyboard = on
	streams := make([]*adbStream, 0, len(t.streams))
	for s := range t.streams {
		streams = append(streams, s)
	}
	t.kbMu.Unlock()

	var errs []error
	for _, s := range streams {
		if err := s.setHardwareKeyboard(on); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// track registers a stream opened on this target, and creates the keyboard
// on it if one is wanted. Registering and reading the setting under the one
// lock means a SetHardwareKeyboard racing the open reaches the stream
// either here or through the set, never neither.
func (t *ADBTarget) track(s *adbStream) error {
	t.kbMu.Lock()
	if t.streams == nil {
		t.streams = make(map[*adbStream]struct{})
	}
	t.streams[s] = struct{}{}
	on := t.hwKeyboard
	t.kbMu.Unlock()
	s.untrack = func() { t.untrack(s) }
	if on {
		return s.setHardwareKeyboard(true)
	}
	return nil
}

func (t *ADBTarget) untrack(s *adbStream) {
	t.kbMu.Lock()
	delete(t.streams, s)
	if len(t.streams) == 0 {
		t.hwKeyboard = false
	}
	t.kbMu.Unlock()
}

// liveStream returns an open stream on this target, or nil. Any one will
// do: every stream is on the same device, and input sent through any of
// them lands in the same focused window.
func (t *ADBTarget) liveStream() *adbStream {
	t.kbMu.Lock()
	defer t.kbMu.Unlock()
	for s := range t.streams {
		return s
	}
	return nil
}

func (s *adbStream) setHardwareKeyboard(on bool) error {
	s.kbMu.Lock()
	defer s.kbMu.Unlock()
	if s.hwKeyboard == on {
		return nil
	}
	var msg []byte
	if on {
		var err error
		msg, err = scrcpy.AppendUhidCreate(nil, uhidKeyboardID, 0, 0, uhidKeyboardName, scrcpy.KeyboardDescriptor)
		if err != nil {
			return err
		}
	} else {
		msg = scrcpy.AppendUhidDestroy(nil, uhidKeyboardID)
	}
	if err := s.sess.SendControl(msg); err != nil {
		return fmt.Errorf("hardware keyboard: %w", err)
	}
	s.hwKeyboard = on
	return nil
}

// Text implements TextTyper.
func (s *adbStream) Text(text string) error { return s.typeText(text) }

// textSettleTime is how long a key waits behind a burst of n typed
// characters: 300ms and 20ms more for each, capped at a second.
//
// Text and keys leave in order on one socket and do not land in order. On
// emulator-5556 (Android 16, Gboard), "hi loupe" as one inject-text followed
// by a backspace left "hi loue", "hi lope" or "hi lupe" -- the backspace
// landing at a cursor position the IME had not caught up with. Measured by
// cmd/kbprobe -seq, the gap between the text and the key against runs that
// came out wrong: 0ms 1 of 3, 20ms 3 of 3, 50ms 1 of 3, 160ms 3 of 5,
// 460ms 0 of 4, 960ms 0 of 4. Pure keycodes sent back to back went wrong in
// 3 of 4 runs with the hardware keyboard attached. One character per
// message kept order in 9 of 9 runs at 30, 60 and 100ms apart, so a phone
// typing a key at a time never waits; what waits is the key after a paste
// or a predicted word.
func textSettleTime(n int) time.Duration {
	if n < 2 {
		return 0
	}
	d := 300*time.Millisecond + time.Duration(n)*20*time.Millisecond
	if d > time.Second {
		d = time.Second
	}
	return d
}

// waitForText blocks until the last burst of text is taken to have landed.
func (s *adbStream) waitForText() {
	s.settleMu.Lock()
	until := s.settleUntil
	s.settleMu.Unlock()
	if d := time.Until(until); d > 0 {
		time.Sleep(d)
	}
}

// typeText sends s as inject-text messages, split on rune boundaries so no
// message carries half a character.
func (s *adbStream) typeText(text string) error {
	if d := textSettleTime(utf8.RuneCountInString(text)); d > 0 {
		defer func() {
			s.settleMu.Lock()
			s.settleUntil = time.Now().Add(d)
			s.settleMu.Unlock()
		}()
	}
	for text != "" {
		n := len(text)
		if n > scrcpy.InjectTextMax {
			n = scrcpy.InjectTextMax
			for n > 0 && !utf8.RuneStart(text[n]) {
				n--
			}
		}
		msg, err := scrcpy.AppendInjectText(nil, text[:n])
		if err != nil {
			return err
		}
		if err := s.sess.SendControl(msg); err != nil {
			return err
		}
		text = text[n:]
	}
	return nil
}

// pressKey sends a down and an up for keycode.
func (s *adbStream) pressKey(keycode int) error {
	if err := s.Key(Key{Action: KeyDown, Keycode: int32(keycode)}); err != nil {
		return err
	}
	return s.Key(Key{Action: KeyUp, Keycode: int32(keycode)})
}
