package httpapi

import "github.com/bmcreations/loupe/internal/session"

// inputQueueDepth is how many input messages a viewer can have waiting for
// the device before its read loop blocks. A drag sends one move every 8-16ms,
// so this is a few seconds of moves behind a slow device.
const inputQueueDepth = 256

// answeredInline reports whether a viewer's read loop handles a message
// itself instead of queuing it behind input. Pings have to be: their reply is
// the client's rtt sample. Stats and hello do no device work, so there is
// nothing to gain by making them wait.
func answeredInline(msgType byte) bool {
	switch msgType {
	case session.MsgPing, session.MsgClientStats, session.MsgHello:
		return true
	}
	return false
}

// inputQueue runs one viewer's input messages in order on their own
// goroutine, so a message waiting on the device does not hold up the read
// loop.
type inputQueue struct {
	ch   chan []byte
	done chan struct{}
}

func newInputQueue(run func(payload []byte)) *inputQueue {
	q := &inputQueue{ch: make(chan []byte, inputQueueDepth), done: make(chan struct{})}
	go func() {
		defer close(q.done)
		for p := range q.ch {
			run(p)
		}
	}()
	return q
}

// push queues a copy of payload: the QUIC and WebTransport connections reuse
// their read buffer on the next Recv.
func (q *inputQueue) push(payload []byte) {
	q.ch <- append([]byte(nil), payload...)
}

// close stops taking input and waits for what is queued to reach the device,
// so a viewer's last touch-up is not lost when it disconnects.
func (q *inputQueue) close() {
	close(q.ch)
	<-q.done
}
