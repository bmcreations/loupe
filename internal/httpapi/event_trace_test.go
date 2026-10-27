package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
	"github.com/bmcreations/loupe/internal/trace"
)

// TestEventTraceDrainAccountsForEveryPublishedEvent exercises the two-step
// handoff in Acquire's event-forwarding goroutine (manager.go): on s.Done()
// it must drain whatever eventCh already holds before the stream-end record
// is written, and it must never lose an event silently -- one published after
// that point is counted in eventTraceDrops instead.
//
// It uses no real device: stubTarget opens instantly and its stream closes on
// Session.Close(), which is enough to drive the session through its whole
// lifecycle. Publishing straight through sess.PublishEvent (rather than via
// dispatch) is deliberate -- it can outrun the observer's channel and leave
// some events still buffered at the moment Close() is called, which is the
// only way to exercise the drain path rather than the steady-state one.
func TestEventTraceDrainAccountsForEveryPublishedEvent(t *testing.T) {
	dir := t.TempDir()
	tracePath := filepath.Join(dir, "trace.jsonl")
	tr, err := trace.Open(tracePath)
	if err != nil {
		t.Fatalf("trace.Open: %v", err)
	}
	defer tr.Close()

	m := NewManager(target.Options{MaxFPS: 60})
	m.SetTrace(tr)
	m.Register(&stubTarget{})

	sess, err := m.Acquire(context.Background(), "stub")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	const published = 500 // > eventCh's 256 capacity, to force some backlog
	for i := 0; i < published; i++ {
		sess.PublishEvent(session.Event{Source: "agent", Kind: "tap"})
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The manager's cleanup goroutine waits on eventDrainDone before writing
	// stream-end, so polling for that record's arrival is polling for the
	// drain to have finished.
	deadline := time.Now().Add(5 * time.Second)
	var lines []map[string]any
	for time.Now().Before(deadline) {
		lines = readTraceLines(t, tracePath)
		if len(lines) > 0 && lines[len(lines)-1]["kind"] == "stream-end" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(lines) == 0 || lines[len(lines)-1]["kind"] != "stream-end" {
		t.Fatalf("stream-end record never appeared; got %d lines", len(lines))
	}

	streamEnd := lines[len(lines)-1]
	eventTraceDrops := int(streamEnd["eventTraceDrops"].(float64))

	recorded := 0
	for _, l := range lines {
		if l["kind"] == "event" {
			recorded++
		}
	}

	if recorded+eventTraceDrops != published {
		t.Errorf("recorded(%d) + eventTraceDrops(%d) = %d, want %d (every published event must be either recorded or counted as a drop, never both or neither)",
			recorded, eventTraceDrops, recorded+eventTraceDrops, published)
	}

	// Ordering: nothing recorded as an "event" may appear after stream-end --
	// that would mean the drain goroutine was still writing when the cleanup
	// goroutine read eventTraceDrops and wrote its own record.
	for i, l := range lines {
		if l["kind"] == "stream-end" && i != len(lines)-1 {
			t.Fatalf("stream-end at line %d is not the last line (%d total)", i, len(lines))
		}
	}
}

func readTraceLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("open trace: %v", err)
	}
	defer f.Close()

	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		out = append(out, rec)
	}
	return out
}
