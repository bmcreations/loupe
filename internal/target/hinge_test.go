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
// of the five is sent. The four it replaced return at once with no error.
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
	go func() { first <- c.Set(ctx, 10, apply) }()
	<-started

	results := make([]chan error, 5)
	for i := range results {
		results[i] = make(chan error, 1)
		deg := 20 + i*10
		go func(ch chan error) { ch <- c.Set(ctx, deg, apply) }(results[i])
		// Each queues before the next, so the order is the drag's.
		waitQueued(t, &c, deg)
	}
	for i := 0; i < 4; i++ {
		select {
		case err := <-results[i]:
			if err != nil {
				t.Fatalf("superseded Set %d: %v", i, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("superseded Set %d still waiting", i)
		}
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-results[4]; err != nil {
		t.Fatal(err)
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
	if err := c.Set(t.Context(), 90, func(context.Context, int) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if err := c.Set(t.Context(), 91, func(context.Context, int) error { return nil }); err != nil {
		t.Fatalf("after a failure: %v", err)
	}
}

// A caller that gives up stops waiting; the write still finishes, and the
// next Set starts a fresh drain.
func TestHingeCoalescerCallerContextEnds(t *testing.T) {
	var c hingeCoalescer
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := c.Set(ctx, 90, func(context.Context, int) error { <-release; return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	close(release)
	if err := c.Set(t.Context(), 91, func(context.Context, int) error { return nil }); err != nil {
		t.Fatal(err)
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
