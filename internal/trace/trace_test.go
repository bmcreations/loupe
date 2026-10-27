package trace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A deploy here is a restart: the supervisor notices a new binary and brings
// the server back within two seconds. Truncating at open meant building the
// fix for a trap destroyed the trace that demonstrated it.
func TestReopeningKeepsTheEarlierRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	first.Record(map[string]any{"kind": "client", "run": "first"})
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	second.Record(map[string]any{"kind": "client", "run": "second"})
	if err := second.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	recs := read(t, path)
	var runs []string
	opens := 0
	for _, r := range recs {
		switch r["kind"] {
		case "client":
			runs = append(runs, r["run"].(string))
		case "trace-open":
			opens++
		}
	}
	if got := strings.Join(runs, ","); got != "first,second" {
		t.Fatalf("runs %q, want \"first,second\"", got)
	}
	// Without a marker per open, the only thing separating two servers' rows is
	// a gap in the timestamps, which is also what a quiet minute looks like.
	if opens != 2 {
		t.Fatalf("trace-open records %d, want 2", opens)
	}
}

func TestEveryRecordIsStamped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	w.Record(map[string]any{"kind": "bitrate", "gate": "window"})
	_ = w.Close()

	for _, r := range read(t, path) {
		if s, ok := r["t"].(string); !ok || s == "" {
			t.Fatalf("record %v has no timestamp", r)
		}
	}
}

// The roll bounds the file without costing the run that is starting: it moves
// what is there aside rather than deleting it, and the new run gets an empty
// file rather than appending to something already at the limit.
func TestAnOversizedTraceIsRolledAsideRatherThanAppendedTo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trace.jsonl")
	big := make([]byte, maxTraceBytes+1)
	for i := range big {
		big[i] = '\n'
	}
	if err := os.WriteFile(path, big, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	w, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	w.Record(map[string]any{"kind": "client", "run": "fresh"})
	_ = w.Close()

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Size() > 4096 {
		t.Fatalf("trace is %d bytes, so it was appended to rather than rolled", st.Size())
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rolled-aside trace is missing: %v", err)
	}
}

func TestANilWriterIsANoOp(t *testing.T) {
	var w *Writer
	w.Record(map[string]any{"kind": "client"})
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func read(t *testing.T, path string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}
