package session

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// The status bar reaches init in coded pixels: emulator-5554's 63 of 2400
// display pixels, streamed at 648x1440, is 38.
func TestInitCarriesTheStatusBarInCodedPixels(t *testing.T) {
	s := &Session{
		ID:       "s",
		key:      "test",
		stream:   &keyStream{frames: make(chan target.Frame)},
		viewers:  make(map[uint64]*viewer),
		done:     make(chan struct{}),
		queueLen: 8,
		init:     InitPayload{CodedWidth: 648, CodedHeight: 1440},
	}
	_, out := s.AddViewer()
	first := readInit(t, out)
	if first.StatusBar != 0 {
		t.Fatalf("init statusBar = %d before it is read, want 0", first.StatusBar)
	}

	s.setStatusBar(63, 2400)
	if got := readInit(t, out); got.StatusBar != 38 {
		t.Fatalf("init statusBar = %d, want 38", got.StatusBar)
	}

	// The same bar again sends nothing.
	s.setStatusBar(63, 2400)
	for drain := time.After(50 * time.Millisecond); ; {
		select {
		case msg := <-out:
			if len(msg.Data) > 0 && msg.Data[0] == MsgInit {
				t.Fatal("unchanged status bar resent init")
			}
			continue
		case <-drain:
		}
		break
	}
}

// readInit returns the next init on out, skipping the other messages a
// joining viewer is sent (control state).
func readInit(t *testing.T, out <-chan OutMsg) InitPayload {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case msg := <-out:
			if len(msg.Data) < 1 || msg.Data[0] != MsgInit {
				continue
			}
			var got InitPayload
			if err := json.Unmarshal(msg.Data[1:], &got); err != nil {
				t.Fatalf("init is not InitPayload JSON: %v", err)
			}
			return got
		case <-deadline:
			t.Fatal("no init message")
			return InitPayload{}
		}
	}
}
