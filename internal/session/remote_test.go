package session

import (
	"sync"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// fakeRemote records the ops a session sends the remote encoder, as the ops
// simcap would read them off stdin.
type fakeRemote struct {
	keyStream
	remote chan target.Frame
	mu     sync.Mutex
	ops    []string
}

func newFakeRemote() *fakeRemote {
	return &fakeRemote{keyStream: keyStream{frames: make(chan target.Frame)}, remote: make(chan target.Frame, 16)}
}

func (f *fakeRemote) Remote() <-chan target.Frame { return f.remote }
func (f *fakeRemote) note(op string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, op)
	return nil
}
func (f *fakeRemote) RemoteGate(open bool) error {
	if open {
		return f.note("open")
	}
	return f.note("hold")
}
func (f *fakeRemote) RemoteKeyframe() error      { return f.note("key") }
func (f *fakeRemote) RemoteCap(bps int) error    { return f.note("cap") }
func (f *fakeRemote) RemoteMaxSize(px int) error { return f.note("size") }
func (f *fakeRemote) opsNow() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

func remoteSession(t *testing.T) (*Session, *fakeRemote) {
	t.Helper()
	fr := newFakeRemote()
	s := newTestSession(t)
	s.stream = fr
	s.attachRemote(fr)
	return s, fr
}

func waitOps(t *testing.T, fr *fakeRemote, want ...string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		got := fr.opsNow()
		if len(got) == len(want) {
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("ops %v, want %v", got, want)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ops %v, want %v", got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRemoteViewerNeedsAcksAndToBeRemote(t *testing.T) {
	s, _ := remoteSession(t)
	cases := []struct {
		o    ViewerOptions
		want string
	}{
		{ViewerOptions{Acks: true}, "remote"},
		{ViewerOptions{Acks: true, Local: true}, "main"},
		{ViewerOptions{}, "main"},
		{ViewerOptions{Local: true}, "main"},
	}
	for _, c := range cases {
		id, _ := s.AddViewerWith(c.o)
		if got := s.ViewerTrack(id); got != c.want {
			t.Errorf("%+v on %s, want %s", c.o, got, c.want)
		}
	}
}

func TestNoRemoteTrackWithoutAStreamThatHasOne(t *testing.T) {
	s := newTestSession(t)
	id, _ := s.AddViewerWith(ViewerOptions{Acks: true})
	if got := s.ViewerTrack(id); got != "main" {
		t.Fatalf("on %s", got)
	}
}

func TestRemoteJoinAsksForAKeyframeAndIsToldItsTrack(t *testing.T) {
	s, fr := remoteSession(t)
	_, out := s.AddViewerWith(ViewerOptions{Acks: true})
	if p := initOf(t, <-out); p.Track != "remote" {
		t.Fatalf("init track %q", p.Track)
	}
	waitOps(t, fr, "key")
	_, mainOut := s.AddViewerWith(ViewerOptions{Local: true})
	if p := initOf(t, <-mainOut); p.Track != "" {
		t.Fatalf("main init track %q", p.Track)
	}
}

// A remote viewer starts from the config and the keyframe that follows it, with
// the codec string read from the remote encoder's own SPS.
func TestRemoteTrackStartsFromConfigAndKey(t *testing.T) {
	s, fr := remoteSession(t)
	_, out := s.AddViewerWith(ViewerOptions{Acks: true})
	<-out // init
	sps := []byte{0, 0, 0, 1, 0x67, 0x64, 0x00, 0x28, 0xac, 0xd9, 0x40, 0x78, 0x02, 0x27, 0xe5, 0xc0, 0x44, 0x00, 0x00, 0x03, 0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xf0, 0x3c, 0x60, 0xc6, 0x58, 0, 0, 0, 1, 0x68, 0xe9, 0x3b, 0x2c, 0x8b}
	fr.remote <- target.Frame{Config: true, Data: sps}
	fr.remote <- target.Frame{PTS: time.Millisecond, Data: []byte{0, 0, 0, 1, 0x41, 1}} // delta before the key: not decodable
	fr.remote <- target.Frame{PTS: 2 * time.Millisecond, Key: true, Data: []byte{0, 0, 0, 1, 0x65, 1}}
	fr.remote <- target.Frame{PTS: 3 * time.Millisecond, Data: []byte{0, 0, 0, 1, 0x41, 2}}
	var got []OutMsg
	codec := ""
	deadline := time.After(time.Second)
	for len(got) < 3 {
		select {
		case m := <-out:
			if m.Control {
				if m.Data[0] == MsgInit {
					codec = initOf(t, m).Codec
				}
				continue
			}
			got = append(got, m)
		case <-deadline:
			t.Fatalf("got %d messages", len(got))
		}
	}
	waitFor(t, "codec", func() bool {
		select {
		case m := <-out:
			if m.Control && m.Data[0] == MsgInit {
				codec = initOf(t, m).Codec
			}
		default:
		}
		return codec == "avc1.640028"
	})
	if !got[0].Config || !got[1].Key || got[2].Key || got[1].PTSMicros != 2000 || got[2].PTSMicros != 3000 {
		t.Fatalf("sequence %+v", got)
	}
}

func TestRemoteJoinerIsReplayedCachedConfig(t *testing.T) {
	s, fr := remoteSession(t)
	fr.remote <- target.Frame{Config: true, Data: []byte{0, 0, 0, 1, 0x67, 1, 2, 3}}
	waitFor(t, "cfg cached", func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.remote.cfg != nil
	})
	_, out := s.AddViewerWith(ViewerOptions{Acks: true})
	<-out
	if m := next(t, out); !m.Config || m.track != trackRemote {
		t.Fatalf("replay %+v", m)
	}
}

func TestGateDecisions(t *testing.T) {
	ok := gateViewer{Acking: true, LastAckAgo: 100 * time.Millisecond, OldestMs: 100, BaseMs: 50, Backlog: 3}
	late := ok
	late.OldestMs = 171 // base 50 + 120 = 170
	edge := ok
	edge.OldestMs = 170
	deep := ok
	deep.Backlog = 9
	deepEdge := ok
	deepEdge.Backlog = 8
	silentLate := late
	silentLate.LastAckAgo = 4*time.Second + time.Millisecond
	silentNeverAcked := gateViewer{SinceJoin: 5 * time.Second, OldestMs: 9999, Backlog: 99}
	fresh := gateViewer{SinceJoin: time.Second}
	cases := []struct {
		name string
		in   []gateViewer
		want bool
	}{
		{"no viewers holds", nil, false},
		{"healthy opens", []gateViewer{ok}, true},
		{"oldest past rtt+120 holds", []gateViewer{ok, late}, false},
		{"oldest at rtt+120 opens", []gateViewer{edge}, true},
		{"backlog 9 holds", []gateViewer{deep}, false},
		{"backlog 8 opens", []gateViewer{deepEdge}, true},
		{"silent 4 s is left out", []gateViewer{ok, silentLate}, true},
		{"never-acked past 4 s is left out", []gateViewer{silentNeverAcked}, true},
		{"just joined opens", []gateViewer{fresh}, true},
		{"one late of many holds", []gateViewer{ok, ok, late}, false},
	}
	for _, c := range cases {
		if got := remoteGateOpen(c.in); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

// The gate sends 0x82 only when the decision changes, holds with nobody on the
// track, and opens again when a viewer joins.
func TestGateSendsOnlyOnChange(t *testing.T) {
	s, fr := remoteSession(t)
	now := time.Now()
	step := func() { now = now.Add(10 * time.Millisecond); s.remoteGateTick(now, 10*time.Millisecond) }

	step()
	step()
	if ops := fr.opsNow(); len(ops) != 0 {
		t.Fatalf("held with no viewers should send nothing (starts held): %v", ops)
	}
	id, _ := s.AddViewerWith(ViewerOptions{Acks: true})
	waitOps(t, fr, "key")
	step()
	step()
	waitOps(t, fr, "key", "open")

	// A frame in flight for 400 ms with no ack and a 50 ms base holds the gate.
	s.mu.RLock()
	v := s.viewers[id]
	s.mu.RUnlock()
	v.flow.Sent(1, 1000, now.Add(-400*time.Millisecond))
	step()
	waitOps(t, fr, "key", "open", "hold")
	step()
	step()
	waitOps(t, fr, "key", "open", "hold")

	// Its ack clears the backlog: open again.
	v.flow.Ack(1, now)
	step()
	waitOps(t, fr, "key", "open", "hold", "open")

	s.RemoveViewer(id)
	step()
	waitOps(t, fr, "key", "open", "hold", "open", "hold")
}

// A relaunched stream's encoder starts held, so the gate sends its state again.
func TestGateResendsAfterStreamSwap(t *testing.T) {
	s, fr := remoteSession(t)
	s.AddViewerWith(ViewerOptions{Acks: true})
	waitOps(t, fr, "key") // the join's keyframe ask goes to the old stream
	now := time.Now()
	s.remoteGateTick(now, 10*time.Millisecond)
	fr2 := newFakeRemote()
	s.attachRemote(fr2)
	s.remoteGateTick(now.Add(10*time.Millisecond), 10*time.Millisecond)
	waitOps(t, fr2, "open")
}

func TestRemoteSecondReportsOpenFraction(t *testing.T) {
	s, _ := remoteSession(t)
	var got RemoteSecond
	s.SetRemoteObserver(func(r RemoteSecond) { got = r })
	id, _ := s.AddViewerWith(ViewerOptions{Acks: true})
	now := time.Now()
	s.remoteGateTick(now, 750*time.Millisecond) // opens
	s.RemoveViewer(id)
	s.remoteGateTick(now, 250*time.Millisecond) // holds
	s.remote.frames.Store(30)
	s.remote.keys.Store(1)
	s.reportRemote()
	if got.Frames != 30 || got.Keys != 1 || got.CapBps != DefaultRemoteCapBps {
		t.Fatalf("%+v", got)
	}
	if got.Gate != 0.75 || got.HeldMs != 250 {
		t.Fatalf("gate %v held %v ms, want 0.75 and 250", got.Gate, got.HeldMs)
	}
	s.reportRemote()
	if got.Frames != 0 || got.Gate != 0 {
		t.Fatalf("second did not reset: %+v", got)
	}
}

// A shed by a remote viewer never reaches the counters the bitrate controller
// reads; one by a local viewer does.
func TestAdapterIgnoresRemoteViewerSheds(t *testing.T) {
	s, _ := remoteSession(t)
	remote, _ := s.AddViewerWith(ViewerOptions{Acks: true})
	plain, _ := s.AddViewerWith(ViewerOptions{}) // not local, no acks: main track
	local, _ := s.AddViewerWith(ViewerOptions{Local: true})
	for _, r := range []ShedReason{ShedPacer, ShedStale, ShedAwaitKey, ShedAbandon} {
		s.NoteShedFor(remote, r)
		s.NoteShedFor(plain, r)
	}
	if b := s.adapterShedBreakdown(); b != (ShedCounts{}) {
		t.Fatalf("adapter saw %+v", b)
	}
	if b := s.ShedBreakdown(); b.Pacer != 2 || b.Stale != 2 || b.AwaitKey != 2 || b.Abandon != 2 {
		t.Fatalf("totals %+v", b)
	}
	s.NoteShedFor(local, ShedStale)
	if b := s.adapterShedBreakdown(); b.Stale != 1 || b.Pacer != 0 {
		t.Fatalf("adapter saw %+v", b)
	}
}

// Neither a starved remote viewer, on the remote track or the main one, nor
// its sent bytes, make a sample; a local viewer's do.
func TestSamplerIgnoresRemoteViewers(t *testing.T) {
	s, _ := remoteSession(t)
	old := time.Now().Add(-time.Hour)
	age := func(id uint64) {
		s.mu.Lock()
		s.viewers[id].joined = old
		s.mu.Unlock()
	}
	remote, _ := s.AddViewerWith(ViewerOptions{Acks: true})
	plain, _ := s.AddViewerWith(ViewerOptions{})
	age(remote)
	age(plain)
	for _, id := range []uint64{remote, plain} {
		s.NoteShedFor(id, ShedStale)
		s.NoteShedFor(id, ShedAbandon)
		s.NoteShedFor(id, ShedAwaitKey)
	}
	if sample, ok := newBitrateSampler().next(s, time.Now()); ok {
		t.Fatalf("remote viewers produced a sample: %+v", sample)
	}

	local, _ := s.AddViewerWith(ViewerOptions{Local: true})
	age(local)
	s.NoteShedFor(local, ShedStale)
	s.NoteShedFor(remote, ShedStale)
	sample, ok := newBitrateSampler().next(s, time.Now())
	if !ok || sample.Shed != 1 || sample.AwaitKey != 0 {
		t.Fatalf("sample %+v ok=%v, want the local viewer's one stale shed", sample, ok)
	}
}

// A full remote queue asks for a remote keyframe, not a main one.
func TestRemoteQueueOverflowAsksRemoteEncoder(t *testing.T) {
	s, fr := remoteSession(t)
	id, _ := s.AddViewerWith(ViewerOptions{Acks: true})
	waitOps(t, fr, "key")
	s.mu.Lock()
	v := s.viewers[id]
	v.needsKey = false
	for len(v.out) < cap(v.out) {
		v.out <- OutMsg{}
	}
	s.mu.Unlock()
	s.broadcast(OutMsg{Data: []byte{1}, track: trackRemote})
	waitOps(t, fr, "key", "key")
}
