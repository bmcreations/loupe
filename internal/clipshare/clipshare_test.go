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

func TestPublishDropsOlder(t *testing.T) {
	s := enabled(t)
	if _, ok := s.Publish(Value{Text: "a", Origin: "mac", At: 1000}); !ok {
		t.Fatal("at 1000 dropped")
	}
	if _, ok := s.Publish(Value{Text: "b", Origin: "mac", At: 999}); ok {
		t.Fatal("at 999 accepted after 1000")
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
