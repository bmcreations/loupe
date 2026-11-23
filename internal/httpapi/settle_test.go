package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// signInAt is signInScreen with its Sign in row centred at y, as it would be
// partway through a push.
func signInAt(y int) *target.UI {
	ui := signInScreen()
	n := ui.Root.Children[0]
	n.Bounds = [4]int{100, y - 100, 980, y + 100}
	n.Center = [2]int{540, y}
	return ui
}

func TestAMatchTapRightAfterInputTapsWhereTheNodeSettles(t *testing.T) {
	s, ft := matchServer(t, signInScreen(), signInAt(900), signInAt(700), signInAt(600))
	if rec := postInput(s, "dev1", `{"action":"tap","match":{"text":"Sign in"}}`); rec.Code != http.StatusOK {
		t.Fatalf("first tap: status = %d: %s", rec.Code, rec.Body.String())
	}
	rec := postInput(s, "dev1", `{"action":"tap","match":{"text":"Sign in"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("second tap: status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(ft.taps) != 2 || ft.taps[1] != [2]int{540, 600} {
		t.Fatalf("taps = %v; want the second at 540,600, where the row stopped", ft.taps)
	}
	if ft.inspects < 5 {
		t.Fatalf("inspects = %d; the second tap must look past the row's last move", ft.inspects)
	}
}

func TestAFirstMatchTapTakesOneLook(t *testing.T) {
	s, ft := matchServer(t, signInScreen())
	if rec := postInput(s, "dev1", `{"action":"tap","match":{"text":"Sign in"}}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if ft.inspects != 1 {
		t.Fatalf("inspects = %d; want 1 with no input before it", ft.inspects)
	}
}

func TestAWaitAfterInputIsNotHeldForTheScreenToSettle(t *testing.T) {
	s, ft := matchServer(t, signInScreen(), signInAt(900), signInAt(700))
	postInput(s, "dev1", `{"action":"tap","match":{"text":"Sign in"}}`)
	if rec := postInput(s, "dev1", `{"action":"wait","match":{"text":"Sign in"}}`); rec.Code != http.StatusOK {
		t.Fatalf("wait: status = %d: %s", rec.Code, rec.Body.String())
	}
	if ft.inspects != 2 {
		t.Fatalf("inspects = %d; a wait returns on the first dump that has its node", ft.inspects)
	}
}

func TestATapWhoseNodeArrivesLateTapsWhereItSettles(t *testing.T) {
	s, ft := matchServer(t, emptyScreen(), signInAt(900), signInAt(600))
	rec := postInput(s, "dev1", `{"action":"tap","match":{"text":"Sign in","waitMs":3000}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(ft.taps) != 1 || ft.taps[0] != [2]int{540, 600} {
		t.Fatalf("taps = %v; want one at 540,600", ft.taps)
	}
}

// A wait that ran past settleAfter returns as soon as its node appears, so
// the tap after it is the one that has to wait out the animation.
func TestATapAfterAWaitWhoseNodeArrivedLateIsHeld(t *testing.T) {
	s, ft := matchServer(t, emptyScreen(), signInAt(900), signInAt(700), signInAt(600))
	if rec := postInput(s, "dev1", `{"action":"wait","match":{"text":"Sign in","waitMs":3000}}`); rec.Code != http.StatusOK {
		t.Fatalf("wait: status = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := postInput(s, "dev1", `{"action":"tap","match":{"text":"Sign in"}}`); rec.Code != http.StatusOK {
		t.Fatalf("tap: status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(ft.taps) != 1 || ft.taps[0] != [2]int{540, 600} {
		t.Fatalf("taps = %v; want one at 540,600", ft.taps)
	}
}

// movingTarget's Sign in row moves down a pixel on every dump.
type movingTarget struct {
	matchTarget
}

func (f *movingTarget) Inspect(ctx context.Context) (*target.UI, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspects++
	return signInAt(600 + f.inspects), nil
}

func TestASettleGivesUpOnAScreenThatNeverStops(t *testing.T) {
	m := NewManager(target.Options{})
	ft := &movingTarget{matchTarget{fakeAgentTarget: fakeAgentTarget{id: "dev1", screenW: 1080, screenH: 2400}}}
	m.Register(ft)
	s := &Server{mgr: m}
	s.inputs.touch("dev1", time.Now())
	began := time.Now()
	rec := postInput(s, "dev1", `{"action":"tap","match":{"text":"Sign in"}}`)
	took := time.Since(began)
	if rec.Code != http.StatusOK || len(ft.taps) != 1 {
		t.Fatalf("status %d taps %v; want the tap sent once settleMax ran out", rec.Code, ft.taps)
	}
	if took < settleMax || took > settleMax+time.Second {
		t.Fatalf("took %v; want about settleMax (%v)", took, settleMax)
	}
}

func TestAMoveDoesNotCountAsInputThatMovesTheScreen(t *testing.T) {
	for _, a := range []string{"move", "wait", "hardwareKeyboard"} {
		if movesScreen(a) {
			t.Errorf("movesScreen(%q) = true", a)
		}
	}
	for _, a := range []string{"tap", "swipe", "key", "button", "text", "scroll", "launch"} {
		if !movesScreen(a) {
			t.Errorf("movesScreen(%q) = false", a)
		}
	}
}
