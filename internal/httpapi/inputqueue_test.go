package httpapi

import (
	"sync"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/session"
)

// A ping read after a touch that is still waiting on the device is answered
// straight away. Before the queue, the read loop ran the touch itself and the
// ping waited for it, so the client measured the device's wait as its rtt.
func TestInputQueueDoesNotHoldUpTheReadLoop(t *testing.T) {
	release := make(chan struct{})
	q := newInputQueue(func(payload []byte) { <-release })

	touch := []byte{session.MsgTouch, 1}
	if answeredInline(touch[0]) {
		t.Fatal("a touch must go through the queue")
	}
	if !answeredInline(session.MsgPing) {
		t.Fatal("a ping must be answered inline")
	}

	start := time.Now()
	q.push(touch)
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("push blocked for %v behind a touch the device has not taken", d)
	}
	close(release)
	q.close()
}

// Input reaches the device in the order the viewer sent it, and close waits
// for all of it, so a disconnect does not drop a touch-up.
func TestInputQueueKeepsOrderAndDrainsOnClose(t *testing.T) {
	var mu sync.Mutex
	var got []byte
	q := newInputQueue(func(payload []byte) {
		time.Sleep(time.Millisecond)
		mu.Lock()
		got = append(got, payload[1])
		mu.Unlock()
	})
	buf := make([]byte, 2)
	for i := 0; i < 20; i++ {
		buf[0], buf[1] = session.MsgTouch, byte(i)
		q.push(buf) // reused, as the QUIC connection reuses its read buffer
	}
	q.close()
	if len(got) != 20 {
		t.Fatalf("got %d messages, want 20", len(got))
	}
	for i, b := range got {
		if int(b) != i {
			t.Fatalf("message %d carried %d: order or copy broken (%v)", i, b, got)
		}
	}
}
