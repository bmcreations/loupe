package target

import (
	"strings"
	"testing"
	"time"
)

// wheelStream is a 500x1000 stream whose framebuffer is the same size, so
// points pass through: one unit drags 140, slop and edge are 20.
func wheelStream(t *testing.T) (*simStream, string) {
	t.Helper()
	bin, logPath := fakeSimcapRoute(t, "hub")
	in := &simInput{bin: bin, udid: "u"}
	t.Cleanup(in.close)
	st := &simStream{in: in, sim: &Simulator{}, meta: Meta{Width: 500, Height: 1000},
		screenW: 500, screenH: 1000, forW: 500, forH: 1000}
	return st, logPath
}

func scrollAll(t *testing.T, st *simStream, evs ...Scroll) {
	t.Helper()
	for _, e := range evs {
		if err := st.Scroll(e); err != nil {
			t.Fatal(err)
		}
	}
}

func waitLift() { time.Sleep(wheelIdle + 150*time.Millisecond) }

func TestWheelNudgeUnderSlopNeverTouches(t *testing.T) {
	st, logPath := wheelStream(t)
	scrollAll(t, st, Scroll{X: 250, Y: 500, VScroll: 0.1})
	waitLift()
	if got := commands(t, logPath); len(got) != 0 {
		t.Errorf("sent %q, want nothing: a nudge must not tap", got)
	}
}

func TestWheelBurstDragsOneFingerAndLiftsWhenIdle(t *testing.T) {
	st, logPath := wheelStream(t)
	scrollAll(t, st,
		Scroll{X: 250, Y: 500, VScroll: 0.1},
		Scroll{X: 250, Y: 500, VScroll: 0.1},
		Scroll{X: 260, Y: 510, VScroll: 0.1}, // the pointer drifting does not move the anchor
	)
	waitLift()
	got := strings.Join(commands(t, logPath), " ")
	want := "touch_down_250_500 touch_move_250_528 touch_move_250_542 touch_up_250_542"
	if got != want {
		t.Errorf("sent %q, want %q", got, want)
	}
	// The next burst starts from its own pointer position.
	scrollAll(t, st, Scroll{X: 100, Y: 600, HScroll: -0.5})
	waitLift()
	got = strings.Join(commands(t, logPath), " ")
	want += " touch_down_100_600 touch_move_30_600 touch_up_30_600"
	if got != want {
		t.Errorf("sent %q, want %q", got, want)
	}
}

func TestWheelLiftsBeforeARealTouch(t *testing.T) {
	st, logPath := wheelStream(t)
	scrollAll(t, st, Scroll{X: 250, Y: 500, VScroll: 0.2})
	if err := st.Touch(Touch{Action: TouchDown, PointerID: 1, X: 10, Y: 10}); err != nil {
		t.Fatal(err)
	}
	scrollAll(t, st, Scroll{X: 250, Y: 500, VScroll: 0.5}) // dropped: a finger is down
	if err := st.Touch(Touch{Action: TouchUp, PointerID: 1, X: 10, Y: 10}); err != nil {
		t.Fatal(err)
	}
	waitLift()
	got := strings.Join(commands(t, logPath), " ")
	want := "touch_down_250_500 touch_move_250_528 touch_up_250_528 touch_down_10_10 touch_up_10_10"
	if got != want {
		t.Errorf("sent %q, want %q", got, want)
	}
}

func TestWheelTakesANewGripAtTheEdge(t *testing.T) {
	st, logPath := wheelStream(t)
	scrollAll(t, st,
		Scroll{X: 250, Y: 900, VScroll: 0.5}, // 970, inside the edge
		Scroll{X: 250, Y: 900, VScroll: 0.5}, // 1040: lift at 980, 60 left over
	)
	waitLift()
	got := strings.Join(commands(t, logPath), " ")
	want := "touch_down_250_900 touch_move_250_970 touch_up_250_980 touch_down_250_900 touch_move_250_960 touch_up_250_960"
	if got != want {
		t.Errorf("sent %q, want %q", got, want)
	}
}

func TestWheelWithoutInputIsNoControl(t *testing.T) {
	if err := (&simStream{}).Scroll(Scroll{VScroll: 1}); err != ErrNoControl {
		t.Errorf("Scroll = %v, want ErrNoControl", err)
	}
}
