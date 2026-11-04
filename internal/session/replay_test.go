package session

import (
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// pending returns everything queued on ch without waiting.
func pending(ch <-chan OutMsg) []OutMsg {
	var out []OutMsg
	for {
		select {
		case m := <-ch:
			out = append(out, m)
		default:
			return out
		}
	}
}

// A viewer that rebuilt its video view asks for the picture again and gets
// init, the parameter sets and the GOP from the last keyframe, even though its
// init is unchanged. Another viewer receives nothing.
func TestViewerReplayResendsInitAndGOP(t *testing.T) {
	s, ps := passSession()
	defer close(ps.alt)
	id, rebuilt := s.AddViewerWith(h265)
	_, other := s.AddViewerWith(h265)
	feedPass(t, ps, rebuilt, target.Frame{Config: true, Data: spsAU}, key(5), delta(6), delta(7))
	pending(rebuilt)
	pending(other)

	s.ViewerReplay(id)

	got := pending(rebuilt)
	if len(got) != 5 {
		t.Fatalf("replay sent %d messages, want init, config, key and two deltas", len(got))
	}
	if !got[0].Control || got[0].Data[0] != MsgInit {
		t.Fatalf("first message is not init")
	}
	if !got[1].Config {
		t.Fatalf("second message is not the parameter sets")
	}
	for i, want := range []uint64{5, 6, 7} {
		if got[2+i].PTSMicros != want*1000 {
			t.Fatalf("frame %d pts %dus, want %dms", i, got[2+i].PTSMicros, want)
		}
	}
	if !got[2].Key {
		t.Fatalf("GOP does not start at the key frame")
	}
	if extra := pending(other); len(extra) != 0 {
		t.Fatalf("other viewer got %d messages", len(extra))
	}

	// The live stream carries on from where the replay stopped.
	ps.alt <- delta(8)
	expect(t, rebuilt, 8)
	expect(t, other, 8)
	drained(t, rebuilt)
	if n := s.WaitCount(); n != 0 {
		t.Fatalf("replayed viewer waited on %d frames", n)
	}
}

// A second replay inside replayInterval is ignored.
func TestViewerReplayRateLimited(t *testing.T) {
	s, ps := passSession()
	defer close(ps.alt)
	id, v := s.AddViewerWith(h265)
	feedPass(t, ps, v, target.Frame{Config: true, Data: spsAU}, key(5))
	pending(v)

	s.ViewerReplay(id)
	first := len(pending(v))
	s.ViewerReplay(id)
	if n := len(pending(v)); n != 0 {
		t.Fatalf("replay %v after the last sent %d messages", replayInterval/2, n)
	}
	if first == 0 {
		t.Fatal("first replay sent nothing")
	}

	s.mu.Lock()
	s.viewers[id].lastReplay = time.Now().Add(-2 * replayInterval)
	s.mu.Unlock()
	s.ViewerReplay(id)
	if n := len(pending(v)); n != first {
		t.Fatalf("replay after the interval sent %d messages, want %d", n, first)
	}
}

// An H.264 viewer has no GOP cache: it gets init and parameter sets, and waits
// for a key frame.
func TestViewerReplayH264(t *testing.T) {
	s, _ := passSession()
	s.lastCfg = &OutMsg{Config: true, Data: make([]byte, FrameHeaderLen+1)}
	id, v := s.AddViewerWith(ViewerOptions{})
	_, other := s.AddViewerWith(ViewerOptions{})
	pending(v)
	pending(other)
	s.mu.Lock()
	s.viewers[id].needsKey = false
	s.mu.Unlock()

	s.ViewerReplay(id)

	got := pending(v)
	if len(got) != 2 || got[0].Data[0] != MsgInit || !got[1].Config {
		t.Fatalf("got %d messages, want init then config", len(got))
	}
	s.mu.RLock()
	latched := s.viewers[id].needsKey
	s.mu.RUnlock()
	if !latched {
		t.Fatal("viewer not waiting for a key frame")
	}
	if extra := pending(other); len(extra) != 0 {
		t.Fatalf("other viewer got %d messages", len(extra))
	}
}
