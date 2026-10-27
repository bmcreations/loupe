// Package trace writes a newline-delimited JSON record per sample so a session
// can be analyzed after the fact instead of read off a phone screen.
//
// The client is the only place several of these numbers exist: the server
// cannot see the decoder's output rate, the render rate, or the round trip as
// the phone measures it. So the client reports them back and they are joined
// with the server's own counters into one record per second.
package trace

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Writer appends JSON records to a file. It is safe for concurrent use.
type Writer struct {
	mu  sync.Mutex
	f   *os.File
	enc *json.Encoder
}

// maxTraceBytes is the size at which the existing trace is rolled aside at
// open. One roll is kept, so the worst case on disk is twice this and the run
// being started never has to share a file with an arbitrarily long history.
const maxTraceBytes = 64 << 20

// Open appends to the trace file at path, creating it if absent.
//
// Appending rather than truncating, because the supervisor restarts the server
// on every build and truncating made a deploy destroy the evidence for the
// thing being deployed. Several traps here were diagnosed across a restart --
// trap 21's 56-minute gap is only visible because the rows on both sides of it
// are in one file -- and a trace that cannot outlive a rebuild cannot show
// that. The run marker below is what delimits one server's rows from the next.
func Open(path string) (*Writer, error) {
	if st, err := os.Stat(path); err == nil && st.Size() > maxTraceBytes {
		// Best effort: a failed roll is not a reason to start without a trace.
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open trace file: %w", err)
	}
	w := &Writer{f: f, enc: json.NewEncoder(f)}
	w.Record(map[string]any{"kind": "trace-open", "pid": os.Getpid()})
	return w, nil
}

// Record writes one sample, stamping it with the wall clock. A nil Writer is a
// no-op, so callers do not need to check whether tracing is enabled.
func (w *Writer) Record(rec map[string]any) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	rec["t"] = time.Now().Format(time.RFC3339Nano)
	// A failed trace write must never disturb the stream it is observing.
	_ = w.enc.Encode(rec)
	_ = w.f.Sync()
}

// Close flushes and closes the file.
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}
