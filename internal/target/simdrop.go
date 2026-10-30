package target

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// An ImageDropper puts an image into the text field at a point, the way a
// person pastes one. Only an iOS simulator does: its pasteboard is reached
// through CoreDevice, where adb has no way to put an image on Android's.
type ImageDropper interface {
	// CanDropImage is nil when DropImage can work here at all. It is
	// ErrSimcapOutdated when the helper has no image pasteboard, and it
	// touches nothing on the device.
	CanDropImage(ctx context.Context) error
	// DropImage pastes the image file at the host path into the field at
	// x, y in device pixels. It is ErrNoPaste when no Paste was offered
	// there, and the image is then on the pasteboard and nowhere else. It
	// is ErrPasteFailed when Paste was tapped twice and the field did not
	// change either time.
	DropImage(ctx context.Context, path string, x, y int) (DropResult, error)
}

// DropResult says how a drop went, for the trace.
type DropResult struct {
	// LongPressed is true when a long press was tried, because the second
	// tap raised no Paste or because the first Paste did not take.
	LongPressed bool
	// Verified is true when the field's value was read changing after
	// Paste. It is false when the field exposes no value to compare, and
	// the drop was then taken on trust.
	Verified bool
	// Retried is true when the first Paste left the field as it was and
	// the edit menu was raised and its Paste tapped a second time.
	Retried bool
}

// ErrNoPaste is a drop on a point where no Paste item appeared: not a field,
// or a field that takes no paste.
var ErrNoPaste = errors.New("no Paste appeared at that point; it is not a field that takes an image")

// ErrPasteFailed is a drop where Paste was tapped and the field's value had
// not changed 1.5s later, and the second try raised no Paste or changed
// nothing either.
var ErrPasteFailed = errors.New("Paste was tapped but the field did not change, and a second try did not paste it either; the image was not pasted")

// ErrSimcapOutdated is a drop through a simcap built before it could put an
// image on the pasteboard: its -caps has no pasteboardimage.
var ErrSimcapOutdated = errors.New("this simcap cannot put an image on the pasteboard; rebuild it with make simcap")

// pasteMatch is the edit menu's Paste, found the way /api/input's match
// finds a node.
var pasteMatch = Match{Text: "Paste"}

const (
	// pbImageBudget covers simcap's own 30s for an image, which for a large
	// one is mostly AppKit writing it as TIFF.
	pbImageBudget = 30 * time.Second
	// dropTapGap keeps the two taps from being a double tap, which selects
	// a word rather than raising the edit menu. The first also raises the
	// keyboard, which the second should land after.
	dropTapGap = 700 * time.Millisecond
	// pasteWait is how long the tree is polled for Paste after each try.
	pasteWait      = 1500 * time.Millisecond
	pastePollEvery = 250 * time.Millisecond
	// pasteSettle is the gap between the two reads that must agree on where
	// Paste is before it is tapped. The edit menu can move once it is up:
	// on dev.spike.target its Paste was read at 165,569 and then at 195,516.
	pasteSettle = 150 * time.Millisecond
	// pastedWait is how long the field is polled for the paste to show.
	pastedWait = 1500 * time.Millisecond
	// treeWait is how long a drop waits for the app's tree to settle before
	// its first tap. On dev.spike.target it settled about 2s after a launch.
	treeWait = 5 * time.Second
	// dropLongPressMs is a press held long enough for UIKit's edit menu.
	dropLongPressMs = 800
)

// CanDropImage implements ImageDropper. A -caps that gave no answer is not
// cached, so a slow start is asked again on the next drop.
func (s *Simulator) CanDropImage(ctx context.Context) error {
	s.pbImageMu.Lock()
	defer s.pbImageMu.Unlock()
	if s.pbImage == nil {
		has, err := simcapHas(ctx, s.simcap, "pasteboardimage")
		if err != nil {
			return err
		}
		s.pbImage = &has
	}
	if !*s.pbImage {
		return ErrSimcapOutdated
	}
	return nil
}

// DropImage implements ImageDropper. It sets the general pasteboard to the
// image, taps the point to focus the field and again to raise the edit menu,
// and taps its Paste. When the second tap raises none it long-presses once.
// The edit menu's Paste is a secure control and asks the user nothing,
// where Cmd+V would put up "Allow Paste".
//
// A Paste tap can land and paste nothing, so the field's value is read
// before and after it. When it has not changed, the edit menu is raised
// again with a long press and Paste tapped once more.
func (s *Simulator) DropImage(ctx context.Context, path string, x, y int) (DropResult, error) {
	var res DropResult
	if err := s.requireBooted(); err != nil {
		return res, err
	}
	if err := s.CanDropImage(ctx); err != nil {
		return res, err
	}
	// The pasteboard watcher sees an image as an empty text change; marking
	// "" as this server's own set keeps it from reaching the viewer as a copy.
	empty := ""
	s.pbSet.Store(&empty)
	if _, err := s.input().do(ctx, pbImageBudget, "pbsetimg %s", base64.StdEncoding.EncodeToString([]byte(path))); err != nil {
		return res, fmt.Errorf("set pasteboard: %w", err)
	}
	if err := s.waitForTree(ctx); err != nil {
		return res, err
	}
	if err := s.Tap(ctx, x, y); err != nil {
		return res, err
	}
	if err := sleepCtx(ctx, dropTapGap); err != nil {
		return res, err
	}
	if err := s.Tap(ctx, x, y); err != nil {
		return res, err
	}
	paste, ui, err := s.raisePaste(ctx, x, y, &res)
	if err != nil {
		return res, err
	}
	if paste == nil {
		return res, ErrNoPaste
	}
	v, err := s.tapPaste(ctx, paste, ui, x, y)
	if err != nil || v == pasteTook || v == pasteUnknown {
		res.Verified = v == pasteTook
		return res, err
	}
	// The first Paste did not take. Its menu may still be up; if not, a
	// long press raises it again. The pasteboard still holds the image.
	res.Retried = true
	if paste, ui, err = s.raisePaste(ctx, x, y, &res); err != nil {
		return res, err
	}
	if paste == nil {
		return res, ErrPasteFailed
	}
	v, err = s.tapPaste(ctx, paste, ui, x, y)
	if err != nil {
		return res, err
	}
	if v == pasteMissing {
		return res, ErrPasteFailed
	}
	res.Verified = v == pasteTook
	return res, nil
}

// waitForTree polls until the app's tree reads the same app twice in a row,
// for up to treeWait. Straight after a launch the first read can still be
// the home screen, whose Application has no name, and the reads after it
// answer "has no frame" for a second or two while the app comes up. Taps
// sent in that window are lost, so the drop would find no Paste. When time
// runs out after some read succeeded the drop goes ahead on it.
func (s *Simulator) waitForTree(ctx context.Context) error {
	deadline := time.Now().Add(treeWait)
	var last string
	var lastErr error
	read := false
	for {
		ui, err := s.Inspect(ctx)
		app, ok := treeApp(ui)
		switch {
		case err != nil:
			lastErr, last = err, ""
		case ok && app == last:
			return nil
		default:
			read = true
			last = app
		}
		if time.Now().Add(pastePollEvery).After(deadline) {
			if read {
				return nil
			}
			return lastErr
		}
		if err := sleepCtx(ctx, pastePollEvery); err != nil {
			return err
		}
	}
}

// treeApp is the name of the app a tree is of, and false for a tree with
// none: the home screen's Application, read before a launched app is up.
func treeApp(ui *UI) (string, bool) {
	if ui == nil || ui.Root == nil {
		return "", false
	}
	return ui.Root.Text, ui.Root.Text != ""
}

// raisePaste waits for the edit menu's Paste, and long-presses the point
// once when it does not come. It is nil, not an error, when none appears.
// A tree that would not read is taken as no Paste the first time: an app
// just launched can answer "has no frame" again after it first read.
func (s *Simulator) raisePaste(ctx context.Context, x, y int, res *DropResult) (*Node, *UI, error) {
	paste, ui, err := s.waitForPaste(ctx)
	if paste != nil || ctx.Err() != nil {
		return paste, ui, err
	}
	res.LongPressed = true
	if err := s.Swipe(ctx, x, y, x, y, dropLongPressMs); err != nil {
		return nil, nil, err
	}
	return s.waitForPaste(ctx)
}

// tapPaste taps Paste and polls the field at x, y until its value moves
// from what it was in ui, the tree Paste was found in.
func (s *Simulator) tapPaste(ctx context.Context, paste *Node, ui *UI, x, y int) (pasteVerdict, error) {
	before := readField(ui, x, y)
	if err := s.Tap(ctx, paste.Center[0], paste.Center[1]); err != nil {
		return pasteMissing, err
	}
	if !before.ok {
		return pasteUnknown, nil
	}
	deadline := time.Now().Add(pastedWait)
	v := pasteUnknown
	for {
		if err := sleepCtx(ctx, pastePollEvery); err != nil {
			return v, err
		}
		// A read that fails is one poll with nothing to compare, not the
		// end of the drop.
		after, err := s.Inspect(ctx)
		if err == nil {
			if v = judgePaste(before, readField(after, x, y)); v == pasteTook {
				return v, nil
			}
		}
		if time.Now().After(deadline) {
			return v, nil
		}
	}
}

// waitForPaste polls the tree for the edit menu's Paste for up to
// pasteWait, and returns it once two reads pasteSettle apart agree on where
// it is, with the later tree. It is nil, not an error, when none appears.
// A read that fails is polled past: an app just launched answers "has no
// frame" for its first second or so. The last such error is returned only
// when no read succeeded.
func (s *Simulator) waitForPaste(ctx context.Context) (*Node, *UI, error) {
	deadline := time.Now().Add(pasteWait)
	var seen *Node
	var lastErr error
	read := false
	for {
		ui, err := s.Inspect(ctx)
		if err != nil {
			lastErr = err
		} else {
			read = true
		}
		var found *Node
		if ui != nil {
			if f := FindNodes(ui.Root, pasteMatch); len(f) > 0 {
				found = f[0]
			}
		}
		if found != nil && seen != nil && found.Center == seen.Center {
			return found, ui, nil
		}
		seen = found
		wait := pastePollEvery
		if found != nil {
			wait = pasteSettle
		}
		if time.Now().Add(wait).After(deadline) {
			// Out of time with a Paste just seen: tap where it last was
			// rather than call it absent.
			if found != nil {
				return found, ui, nil
			}
			if !read {
				return nil, nil, lastErr
			}
			return nil, nil, nil
		}
		if err := sleepCtx(ctx, wait); err != nil {
			return nil, nil, err
		}
	}
}

// fieldRead is a text field's value as the tree gave it. ok is false when
// no node was there to read.
type fieldRead struct {
	value string
	ok    bool
}

// readField reads the field a drop at x, y pastes into: the deepest focused
// node, since the taps focused it, or failing that the deepest node under
// the point.
func readField(ui *UI, x, y int) fieldRead {
	if ui == nil {
		return fieldRead{}
	}
	if f := FocusedAll(ui); len(f) > 0 {
		return fieldRead{value: f[len(f)-1].Value, ok: true}
	}
	if n := deepestAt(ui.Root, x, y); n != nil {
		return fieldRead{value: n.Value, ok: true}
	}
	return fieldRead{}
}

// deepestAt is the last node in depth-first order whose bounds hold x, y.
func deepestAt(n *Node, x, y int) *Node {
	if n == nil {
		return nil
	}
	var hit *Node
	b := n.Bounds
	if x >= b[0] && x < b[2] && y >= b[1] && y < b[3] {
		hit = n
	}
	for _, c := range n.Children {
		if d := deepestAt(c, x, y); d != nil {
			hit = d
		}
	}
	return hit
}

// pasteVerdict is what the field says about a Paste tap.
type pasteVerdict int

const (
	// pasteMissing: the field was read before and after and has not changed.
	pasteMissing pasteVerdict = iota
	// pasteTook: the field's value changed. An image adds U+FFFC per
	// attachment, but any change counts: an app may show it some other way.
	pasteTook
	// pasteUnknown: the field gives nothing to compare, so the tap is taken
	// on trust.
	pasteUnknown
)

// judgePaste compares the field before and after a Paste tap. An empty value
// on both sides is a field that exposes none, not one that is still empty.
func judgePaste(before, after fieldRead) pasteVerdict {
	switch {
	case !before.ok || !after.ok:
		return pasteUnknown
	case before.value != after.value:
		return pasteTook
	case before.value == "":
		return pasteUnknown
	}
	return pasteMissing
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
