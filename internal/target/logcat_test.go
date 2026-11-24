package target

import (
	"context"
	"io"
	"testing"
	"time"
)

// A log reads oldest first, so a dump its deadline stopped holds the old
// end. The logcat handler relies on Close saying so, rather than passing
// that part off as the newest.
func TestStreamStoppedByItsDeadlineIsAnError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	s, err := StartStream(ctx, "yes", nil, "yes", "line")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, s); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := s.Close(); err == nil {
		t.Error("Close = nil; want the deadline reported")
	}
}
