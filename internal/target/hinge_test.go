package target

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestParseHingeAngle(t *testing.T) {
	for _, v := range []any{0.0, 1.0, 90.0, 180.0} {
		if got, err := parseHingeAngle(v); err != nil || float64(got) != v.(float64) {
			t.Errorf("parseHingeAngle(%v) = %v, %v", v, got, err)
		}
	}
	for _, v := range []any{-1.0, 180.5, 181.0, 90.5, "90", nil, true} {
		if _, err := parseHingeAngle(v); err == nil {
			t.Errorf("parseHingeAngle(%v) took it", v)
		}
	}
}

// A drag: one write goes out, five more queue behind it, and only the last
// of the five is sent. The four it replaced wait for that write and answer
// with it, so none returns while the first write is still running.
func TestHingeCoalescerSendsOnlyTheNewestQueuedAngle(t *testing.T) {
	var c hingeCoalescer
	var mu sync.Mutex
	var sent []int
	started := make(chan struct{}, 10)
	release := make(chan struct{})
	apply := func(ctx context.Context, deg int) error {
		mu.Lock()
		sent = append(sent, deg)
		mu.Unlock()
		started <- struct{}{}
		<-release
		return nil
	}
	ctx := t.Context()

	first := make(chan error, 1)
	go func() { first <- setErr(&c, ctx, 10, apply) }()
	<-started

	results := make([]chan error, 5)
	for i := range results {
		results[i] = make(chan error, 1)
		deg := 20 + i*10
		go func(ch chan error) { ch <- setErr(&c, ctx, deg, apply) }(results[i])
		// Each queues before the next, so the order is the drag's.
		waitQueued(t, &c, deg)
	}
	for i, ch := range results {
		select {
		case err := <-ch:
			t.Fatalf("Set %d answered %v before anything was sent in its place", i, err)
		default:
		}
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	for i, ch := range results {
		if err := <-ch; err != nil {
			t.Fatalf("Set %d: %v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(sent, []int{10, 60}) {
		t.Fatalf("sent %v, want [10 60]", sent)
	}
}

func waitQueued(t *testing.T, c *hingeCoalescer, deg int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		q := c.queued
		c.mu.Unlock()
		if q != nil && q.deg == deg {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%d never queued", deg)
}

// The write's own error reaches the caller that is waiting on it, and the
// coalescer is ready for the next one afterwards.
func TestHingeCoalescerReportsTheWriteError(t *testing.T) {
	var c hingeCoalescer
	boom := errors.New("console gone")
	if err := setErr(&c, t.Context(), 90, func(context.Context, int) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if err := setErr(&c, t.Context(), 91, func(context.Context, int) error { return nil }); err != nil {
		t.Fatalf("after a failure: %v", err)
	}
}

// A caller that gives up stops waiting; the write still finishes, and the
// next Set goes out after it.
func TestHingeCoalescerCallerContextEnds(t *testing.T) {
	var c hingeCoalescer
	var mu sync.Mutex
	var applied []int
	record := func(deg int) {
		mu.Lock()
		applied = append(applied, deg)
		mu.Unlock()
	}
	started, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	first := make(chan error, 1)
	go func() {
		first <- setErr(&c, ctx, 90, func(_ context.Context, deg int) error {
			close(started)
			<-release
			record(deg)
			return nil
		})
	}()
	// Cancel once 90 is on its way, so it is not merely replaced by 91.
	<-started
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) || !errors.Is(err, ErrStillSending) {
		t.Fatalf("err = %v", err)
	}
	close(release)
	if err := setErr(&c, t.Context(), 91, func(_ context.Context, deg int) error { record(deg); return nil }); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(applied, []int{90, 91}) {
		t.Fatalf("applied %v, want [90 91]", applied)
	}
}

// A range's min and max are on the wire even at zero; a bool has neither.
func TestSettingJSONKeepsARangesZeroMin(t *testing.T) {
	b, err := json.Marshal([]Setting{hingeSetting(0), {ID: "darkMode", Type: SettingBool, Value: true, Supported: true, Applies: "immediately"}})
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"id":"hingeAngle","type":"range","value":0,"min":0,"max":180,"step":1,"supported":true,"applies":"immediately"},` +
		`{"id":"darkMode","type":"bool","value":true,"supported":true,"applies":"immediately"}]`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
}

func setErr(c *hingeCoalescer, ctx context.Context, deg int, apply func(context.Context, int) error) error {
	_, err := c.Set(ctx, deg, apply)
	return err
}

type hingeResult struct {
	sent int
	err  error
}

// coalescerUnderTest runs Sets on c whose apply records the angle and holds
// the first write until release is closed. Every later write returns
// failWith.
func coalescerUnderTest(t *testing.T, c *hingeCoalescer, failWith error) (set func(deg int) chan hingeResult, release func(), sent func() []int) {
	var mu sync.Mutex
	var got []int
	started, hold := make(chan struct{}), make(chan struct{})
	apply := func(_ context.Context, deg int) error {
		mu.Lock()
		got = append(got, deg)
		n := len(got)
		mu.Unlock()
		if n == 1 {
			close(started)
			<-hold
			return nil
		}
		return failWith
	}
	set = func(deg int) chan hingeResult {
		ch := make(chan hingeResult, 1)
		go func() { s, err := c.Set(t.Context(), deg, apply); ch <- hingeResult{s, err} }()
		return ch
	}
	first := set(10)
	<-started
	t.Cleanup(func() {
		if r := <-first; r.err != nil || r.sent != 10 {
			t.Errorf("first Set = %+v", r)
		}
	})
	return set, func() { close(hold) }, func() []int {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

// A chain of replacements, 20 by 30 by 40 while 10 is going out: 30 is
// never sent, so all three answer 40, the one that was.
func TestHingeCoalescerReportsTheAngleSentInItsPlace(t *testing.T) {
	var c hingeCoalescer
	set, release, sent := coalescerUnderTest(t, &c, nil)
	var chain []chan hingeResult
	for _, deg := range []int{20, 30, 40} {
		chain = append(chain, set(deg))
		waitQueued(t, &c, deg)
	}
	release()
	for i, ch := range chain {
		if r := <-ch; r.err != nil || r.sent != 40 {
			t.Fatalf("Set %d = %+v, want sent 40", 20+i*10, r)
		}
	}
	if got := sent(); !slices.Equal(got, []int{10, 40}) {
		t.Fatalf("sent %v, want [10 40]", got)
	}
}

// A replaced Set does not answer 200 for a write that failed in its place:
// it gets that write's error.
func TestHingeCoalescerReportsTheReplacingWritesError(t *testing.T) {
	var c hingeCoalescer
	boom := errors.New("console gone")
	set, release, _ := coalescerUnderTest(t, &c, boom)
	replaced := set(20)
	waitQueued(t, &c, 20)
	replacing := set(30)
	waitQueued(t, &c, 30)
	release()
	for name, ch := range map[string]chan hingeResult{"replaced": replaced, "replacing": replacing} {
		if r := <-ch; !errors.Is(r.err, boom) {
			t.Fatalf("%s Set = %+v, want %v", name, r, boom)
		}
	}
}
