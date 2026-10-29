package clipshare

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func enabled(t *testing.T) *Service {
	t.Helper()
	s := New()
	s.SetEnabled(true)
	return s
}

func TestPublishAssignsIDAndSeq(t *testing.T) {
	s := enabled(t)
	v, ok := s.Publish(Value{Text: "a", Origin: "mac"})
	if !ok {
		t.Fatal("first publish dropped")
	}
	if v.Seq != 1 {
		t.Fatalf("seq = %d, want 1", v.Seq)
	}
	if !strings.HasPrefix(v.ID, s.instance+"-") || !strings.HasSuffix(v.ID, "-1") {
		t.Fatalf("id = %q, want %s-1", v.ID, s.instance)
	}
	if v.At == 0 {
		t.Fatal("at not filled in")
	}
	if cur, ok := s.Current(); !ok || cur != v {
		t.Fatalf("current = %+v %v, want %+v", cur, ok, v)
	}
}

// A target echoing back what the fan-out just wrote to it carries a new
// origin and the same text. Text alone has to be enough to drop it.
func TestPublishDropsSameText(t *testing.T) {
	s := enabled(t)
	s.Publish(Value{Text: "a", Origin: "target:x"})
	if _, ok := s.Publish(Value{Text: "a", Origin: "target:y"}); ok {
		t.Fatal("same text from another origin was accepted")
	}
	if cur, _ := s.Current(); cur.Seq != 1 {
		t.Fatalf("seq = %d, want 1", cur.Seq)
	}
}

func TestPublishDropsOwnIDBack(t *testing.T) {
	s := enabled(t)
	v, _ := s.Publish(Value{Text: "a", Origin: "mac"})
	// Change the text so only the id can drop it.
	v.Origin, v.Text = "peer:x", "b"
	if _, ok := s.Publish(v); ok {
		t.Fatal("own id coming back was accepted")
	}
}

// A sender whose clock is behind must not lose to the value held here: the
// order is arrival, not the At it sent.
func TestPublishIgnoresSenderAt(t *testing.T) {
	s := enabled(t)
	if _, ok := s.Publish(Value{Text: "a", Origin: "mac"}); !ok {
		t.Fatal("first copy dropped")
	}
	v, ok := s.Publish(Value{Text: "b", ID: "peer-1", At: 1, Origin: "peer:x"})
	if !ok || v.At <= 1 {
		t.Fatalf("publish = %+v %v, want accepted and stamped with this clock", v, ok)
	}
}

func TestPublishDisabled(t *testing.T) {
	s := New()
	if _, ok := s.Publish(Value{Text: "a", Origin: "mac"}); ok {
		t.Fatal("publish accepted while off")
	}
	if _, ok := s.Current(); ok {
		t.Fatal("current set while off")
	}
}

// Turning it off forgets the value, so turning it on again does not hand a
// newly opened target whatever was copied before.
func TestDisableForgetsTheValue(t *testing.T) {
	s := enabled(t)
	s.Publish(Value{Text: "secret", Origin: "mac"})
	s.SetEnabled(false)
	s.SetEnabled(true)
	if cur, ok := s.Current(); ok {
		t.Fatalf("current = %+v after off and on", cur)
	}
}

func TestWaitReturnsNewer(t *testing.T) {
	s := enabled(t)
	got := make(chan Value, 1)
	go func() {
		v, err := s.Wait(context.Background(), 0)
		if err != nil {
			t.Error(err)
		}
		got <- v
	}()
	time.Sleep(20 * time.Millisecond)
	s.Publish(Value{Text: "a", Origin: "mac"})
	select {
	case v := <-got:
		if v.Text != "a" || v.Seq != 1 {
			t.Fatalf("wait = %+v", v)
		}
	case <-time.After(time.Second):
		t.Fatal("wait did not return")
	}

	// A value already newer than after returns at once.
	v, err := s.Wait(context.Background(), 0)
	if err != nil || v.Seq != 1 {
		t.Fatalf("wait after 0 = %+v %v", v, err)
	}
}

func TestWaitTimesOut(t *testing.T) {
	s := enabled(t)
	s.Publish(Value{Text: "a", Origin: "mac"})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := s.Wait(ctx, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline", err)
	}
}

func TestDisableWakesWaiters(t *testing.T) {
	s := enabled(t)
	errc := make(chan error, 1)
	go func() {
		_, err := s.Wait(context.Background(), 0)
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	s.SetEnabled(false)
	select {
	case err := <-errc:
		if !errors.Is(err, ErrDisabled) {
			t.Fatalf("err = %v, want ErrDisabled", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("waiter not woken by disable")
	}
	if _, err := s.Wait(context.Background(), 0); !errors.Is(err, ErrDisabled) {
		t.Fatalf("wait while off = %v, want ErrDisabled", err)
	}
}

func TestSubscribeOrder(t *testing.T) {
	s := enabled(t)
	got := make(chan string, 3)
	cancel := s.Subscribe(func(v Value) { got <- v.Text })
	defer cancel()
	for _, text := range []string{"a", "b", "c"} {
		s.Publish(Value{Text: text, Origin: "mac"})
		// Newest wins past a buffer of four, so pace the three.
		time.Sleep(5 * time.Millisecond)
	}
	for _, want := range []string{"a", "b", "c"} {
		select {
		case text := <-got:
			if text != want {
				t.Fatalf("got %q, want %q", text, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("no %q", want)
		}
	}
}

func TestSubscribeCancelStops(t *testing.T) {
	s := enabled(t)
	got := make(chan string, 4)
	cancel := s.Subscribe(func(v Value) { got <- v.Text })
	cancel()
	s.Publish(Value{Text: "a", Origin: "mac"})
	select {
	case text := <-got:
		t.Fatalf("cancelled subscriber got %q", text)
	case <-time.After(30 * time.Millisecond):
	}
}

// A caller holding a seq from before this server restarted asks for one
// past the new count. It gets the current value rather than waiting for the
// count to catch up.
func TestWaitAfterFromAnEarlierProcess(t *testing.T) {
	s := enabled(t)
	s.Publish(Value{Text: "a", Origin: "mac"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	v, err := s.Wait(ctx, 7)
	if err != nil || v.Text != "a" {
		t.Fatalf("wait after 7 = %+v %v, want a", v, err)
	}
}

func TestWaitEnabled(t *testing.T) {
	s := New()
	done := make(chan error, 1)
	go func() { done <- s.WaitEnabled(context.Background()) }()
	time.Sleep(20 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("returned while off: %v", err)
	default:
	}
	s.SetEnabled(true)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("not woken by enable")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.SetEnabled(false)
	if err := s.WaitEnabled(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want canceled", err)
	}
}

func TestWhileEnabledEndsOnDisable(t *testing.T) {
	s := enabled(t)
	on, cancel := s.WhileEnabled(context.Background())
	defer cancel()
	s.Publish(Value{Text: "a", Origin: "mac"})
	select {
	case <-on.Done():
		t.Fatal("ended on a publish")
	case <-time.After(20 * time.Millisecond):
	}
	s.SetEnabled(false)
	select {
	case <-on.Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("not ended by disable")
	}
	off, cancel2 := s.WhileEnabled(context.Background())
	defer cancel2()
	select {
	case <-off.Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("not done while off")
	}
}

// A peer whose clock runs an hour fast must not hold its value against
// every copy made here for that hour.
func TestPublishIgnoresFutureAt(t *testing.T) {
	s := enabled(t)
	ahead := time.Now().Add(time.Hour).UnixMilli()
	v, ok := s.Publish(Value{Text: "a", ID: "peer-1", At: ahead, Origin: "peer:x"})
	if !ok || v.At >= ahead {
		t.Fatalf("publish = %+v %v, want At from this clock", v, ok)
	}
	if _, ok := s.Publish(Value{Text: "b", Origin: "mac"}); !ok {
		t.Fatal("a local copy after the fast peer's was dropped")
	}
}

// Two values accepted in the same millisecond settle on the larger id
// whichever arrives first, so a ring of peers does not trade them.
func TestPublishSameAtLargerIDWins(t *testing.T) {
	for _, order := range [][2]string{{"a-1", "b-1"}, {"b-1", "a-1"}} {
		s := enabled(t)
		s.now = func() int64 { return 1000 }
		s.Publish(Value{Text: order[0], ID: order[0], Origin: "peer:x"})
		s.Publish(Value{Text: order[1], ID: order[1], Origin: "peer:y"})
		if v, _ := s.Current(); v.ID != "b-1" {
			t.Fatalf("after %v current = %s, want b-1", order, v.ID)
		}
	}
}

// A clock that steps back does not order a new value before the current one.
func TestPublishAtNeverGoesBack(t *testing.T) {
	s := enabled(t)
	now := int64(2000)
	s.now = func() int64 { return now }
	s.Publish(Value{Text: "a", Origin: "mac"})
	now = 1500
	v, ok := s.Publish(Value{Text: "b", Origin: "mac"})
	if !ok || v.At != 2000 {
		t.Fatalf("publish = %+v %v, want At 2000", v, ok)
	}
}
