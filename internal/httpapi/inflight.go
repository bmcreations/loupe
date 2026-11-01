package httpapi

import "sync"

// inflightLimit bounds how many written frames a transport remembers. It is
// the client's slot count (quicSlotBacklog is the most a connection may queue):
// a frame older than that has long since been acked or reset.
const inflightLimit = 64

type inflightEntry struct {
	cancel func()
	key    bool // keyframes and parameter sets are never recalled
}

// inflightFrames remembers streams whose frame was written whole and closed
// but may not have been acked. A closed stream still holds congestion window
// and, for WebTransport, stream credit until the peer has it, so after a shed
// the stale inter-frames queued ahead of the recovery keyframe compete with it
// for a link that was already too slow. CancelStale resets them: the client
// skips a reset frame and resynchronises on the keyframe, which is what the
// shed already requires.
type inflightFrames struct {
	mu sync.Mutex
	l  []inflightEntry
}

// note records a frame just closed. cancel resets its stream.
func (f *inflightFrames) note(key bool, cancel func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.l) >= inflightLimit {
		copy(f.l, f.l[1:])
		f.l = f.l[:len(f.l)-1]
	}
	f.l = append(f.l, inflightEntry{cancel: cancel, key: key})
}

// CancelStale resets every remembered inter-frame stream and forgets all
// entries (a keyframe's stream is left to finish). A stream the peer already
// acked ignores the reset. It returns how many were reset.
func (f *inflightFrames) CancelStale() int {
	f.mu.Lock()
	l := f.l
	f.l = nil
	f.mu.Unlock()
	n := 0
	for _, e := range l {
		if e.key {
			continue
		}
		e.cancel()
		n++
	}
	return n
}
