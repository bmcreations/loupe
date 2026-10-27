package httpapi

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/clipshare"
	"github.com/bmcreations/loupe/internal/target"
)

// clipTarget is a Clipboarder whose stream reports the device clipboard on
// a channel the test feeds, and which records every write it is given.
type clipTarget struct {
	id      string
	changes chan string

	mu     sync.Mutex
	writes []clipWrite
}

type clipWrite struct {
	text  string
	paste bool
}

func newClipTarget(id string) *clipTarget {
	return &clipTarget{id: id, changes: make(chan string, 4)}
}

func (c *clipTarget) ID() string        { return c.id }
func (c *clipTarget) Kind() target.Kind { return target.KindDevice }
func (c *clipTarget) Label() string     { return c.id }
func (c *clipTarget) Open(context.Context, target.Options) (target.Stream, error) {
	return &clipStream{stubStream: stubStream{frames: make(chan target.Frame)}, changes: c.changes}, nil
}
func (c *clipTarget) ReadClipboard(context.Context) (string, error) { return "", nil }
func (c *clipTarget) WriteClipboard(_ context.Context, text string, paste bool) error {
	c.mu.Lock()
	c.writes = append(c.writes, clipWrite{text, paste})
	c.mu.Unlock()
	return nil
}

func (c *clipTarget) written() []clipWrite {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]clipWrite(nil), c.writes...)
}

type clipStream struct {
	stubStream
	changes chan string
}

func (s *clipStream) ClipboardChanges() <-chan string { return s.changes }

// clipManager is a manager sharing the clipboard, with a session open on
// each of ids.
func clipManager(t *testing.T, on bool, ids ...string) (*Manager, *clipshare.Service, map[string]*clipTarget) {
	t.Helper()
	m := NewManager(target.Options{})
	share := clipshare.New()
	share.SetEnabled(on)
	m.SetClipShare(share)
	targets := map[string]*clipTarget{}
	for _, id := range ids {
		ct := newClipTarget(id)
		targets[id] = ct
		m.Register(ct)
		if _, err := m.Acquire(t.Context(), id); err != nil {
			t.Fatalf("Acquire %s: %v", id, err)
		}
	}
	t.Cleanup(m.Shutdown)
	return m, share, targets
}

func TestFanOutWritesOtherSessions(t *testing.T) {
	_, share, ts := clipManager(t, true, "a", "b")
	ts["a"].changes <- "copied on a"
	eventually(t, "b written", func() bool { return len(ts["b"].written()) == 1 })
	if w := ts["b"].written()[0]; w != (clipWrite{"copied on a", false}) {
		t.Fatalf("b got %+v, want the text without paste", w)
	}
	if cur, _ := share.Current(); cur.Origin != "target:a" {
		t.Fatalf("origin = %q, want target:a", cur.Origin)
	}
	time.Sleep(50 * time.Millisecond)
	if w := ts["a"].written(); len(w) != 0 {
		t.Fatalf("a, the origin, was written %+v", w)
	}
}

func TestFanOutSkipsTargetsWithoutSession(t *testing.T) {
	m, share, ts := clipManager(t, true, "a")
	idle := newClipTarget("idle")
	m.Register(idle)
	share.Publish(clipshare.Value{Text: "x", Origin: "mac"})
	eventually(t, "a written", func() bool { return len(ts["a"].written()) == 1 })
	time.Sleep(50 * time.Millisecond)
	if w := idle.written(); len(w) != 0 {
		t.Fatalf("a target with no session was written %+v", w)
	}
}

// peerClipManager is a manager with a session open on the peer target
// mini/p, whose stream reports clipboard changes on the returned channel.
// Every call the hub makes to the owner is counted in calls.
func peerClipManager(t *testing.T) (*Manager, *clipshare.Service, chan string, *[]string, *sync.Mutex) {
	t.Helper()
	m := NewManager(target.Options{})
	share := clipshare.New()
	share.SetEnabled(true)
	m.SetClipShare(share)
	changes := make(chan string, 4)
	var mu sync.Mutex
	var calls []string
	m.SetPeerDialer(func(context.Context, string, string, target.Options) (target.Stream, error) {
		return &clipStream{stubStream: stubStream{frames: make(chan target.Frame)}, changes: changes}, nil
	})
	m.SetPeerCaller(func(_ context.Context, _ string, req target.PeerRequest) (*target.PeerResponse, error) {
		mu.Lock()
		calls = append(calls, req.Method+" "+req.Path)
		mu.Unlock()
		return &target.PeerResponse{Status: http.StatusOK, Body: []byte(`{"ok":true}`)}, nil
	})
	m.SetPeerRows([]TargetInfo{{ID: "mini/p", Kind: string(target.KindDevice), Label: "p", Server: "mini", Available: true}})
	if _, err := m.Acquire(t.Context(), "mini/p"); err != nil {
		t.Fatalf("Acquire mini/p: %v", err)
	}
	t.Cleanup(m.Shutdown)
	return m, share, changes, &calls, &mu
}

// A peer target's owner writes it from its own shared clipboard, so the
// hub writing it as well would set it twice.
func TestFanOutSkipsPeerTargets(t *testing.T) {
	m, share, _, calls, mu := peerClipManager(t)
	local := newClipTarget("local")
	m.Register(local)
	if _, err := m.Acquire(t.Context(), "local"); err != nil {
		t.Fatal(err)
	}
	share.Publish(clipshare.Value{Text: "x", Origin: "mac"})
	eventually(t, "local written", func() bool { return len(local.written()) == 1 })
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	for _, c := range *calls {
		if strings.Contains(c, "/api/clipboard") {
			t.Fatalf("the hub wrote the peer target's clipboard: %s", c)
		}
	}
}

// The relay makes a peer session a ClipboardWatcher, but a peer's copies
// reach this server through its own shared clipboard, never through the
// relay.
func TestPeerSessionNoSink(t *testing.T) {
	_, share, changes, _, _ := peerClipManager(t)
	changes <- "copied on the peer"
	time.Sleep(100 * time.Millisecond)
	if cur, ok := share.Current(); ok {
		t.Fatalf("a peer session published %+v", cur)
	}
}

// Writing b makes b's device report the same text back as a copy. That must
// not become a new shared value, or every write would echo round again.
func TestFanOutEchoNotRepublished(t *testing.T) {
	_, share, ts := clipManager(t, true, "a", "b")
	ts["a"].changes <- "one"
	eventually(t, "b written", func() bool { return len(ts["b"].written()) == 1 })
	before, _ := share.Current()
	ts["b"].changes <- "one"
	time.Sleep(100 * time.Millisecond)
	if after, _ := share.Current(); after.Seq != before.Seq {
		t.Fatalf("seq %d -> %d: b's echo was republished", before.Seq, after.Seq)
	}
	if w := ts["a"].written(); len(w) != 0 {
		t.Fatalf("a was written its own copy back: %+v", w)
	}
}

func TestNewSessionGetsCurrent(t *testing.T) {
	m, share, _ := clipManager(t, true)
	share.Publish(clipshare.Value{Text: "earlier", Origin: "mac"})
	late := newClipTarget("late")
	m.Register(late)
	if _, err := m.Acquire(t.Context(), "late"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "late written", func() bool { return len(late.written()) == 1 })
	if w := late.written()[0]; w != (clipWrite{"earlier", false}) {
		t.Fatalf("late got %+v", w)
	}
}

func TestFanOutOffWhenDisabled(t *testing.T) {
	m, share, ts := clipManager(t, false, "a", "b")
	ts["a"].changes <- "copied on a"
	time.Sleep(100 * time.Millisecond)
	if w := ts["b"].written(); len(w) != 0 {
		t.Fatalf("b written while off: %+v", w)
	}
	if _, ok := share.Current(); ok {
		t.Fatal("a device copy was shared while off")
	}
	late := newClipTarget("late")
	m.Register(late)
	if _, err := m.Acquire(t.Context(), "late"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if w := late.written(); len(w) != 0 {
		t.Fatalf("late written while off: %+v", w)
	}
}
