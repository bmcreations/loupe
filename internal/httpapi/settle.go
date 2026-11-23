package httpapi

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// A tap sent while the screen is still moving from the last input is
// dropped: the node it names is already in the tree at its final bounds, so
// the tap goes out and answers 200, and the next step then waits out its
// whole waitMs for a screen that never came. Measured through a spare
// server on 23 November: on an iPhone 17 Pro simulator, iOS 26.2, a tap on
// Settings' General row sent within 300ms of Back answering was dropped 12
// times of 12, and from 400ms on landed 12 of 12; on a Pixel 7a API 36
// emulator, "Navigate up" tapped at once after entering a page was dropped 3
// of 3, and 200ms later landed 9 of 9. Neither the simulator's hit test nor
// the node's own bounds told the two apart.
//
// So a match tap that follows input on the same target within settleAfter
// dumps until the tree has not changed for settleQuiet, and taps the node as
// the last dump has it. One repeated dump is not enough: an easing animation
// moves a node by a pixel or two per frame near its end, and on the
// simulator two dumps 45ms apart matched at 368ms after Back while the
// search field went on moving until 465ms; a tap gated on that repeat was
// dropped 12 times of 12. settleMax bounds the extra wait, for a screen that never stops
// moving (a progress value, a clock); the tap then goes out as it would
// have without the gate.
const (
	settleAfter = time.Second
	settleQuiet = 150 * time.Millisecond
	settleMax   = 1500 * time.Millisecond
)

// lastInputs remembers when each target last took input that reached it.
type lastInputs struct {
	mu sync.Mutex
	at map[string]time.Time
}

func (l *lastInputs) touch(targetID string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.at == nil {
		l.at = map[string]time.Time{}
	}
	l.at[targetID] = now
}

// recent reports whether targetID took input within settleAfter of now.
func (l *lastInputs) recent(targetID string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	at, ok := l.at[targetID]
	return ok && now.Sub(at) < settleAfter
}

// movesScreen reports whether an action that reached the device can start a
// transition. A move, a wait or a keyboard switch draws nothing on it.
func movesScreen(action string) bool {
	switch action {
	case "move", "wait", "hardwareKeyboard":
		return false
	}
	return true
}

// treeSignature is what two dumps compare on to count as the same screen.
// The JSON form covers every field a node carries, bounds included.
func treeSignature(ui *target.UI) string {
	b, err := json.Marshal(ui)
	if err != nil {
		return ""
	}
	return string(b)
}
