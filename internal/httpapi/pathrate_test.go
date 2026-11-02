package httpapi

import (
	"context"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
)

func TestPathKeySkipsLANAndKeepsTransportApart(t *testing.T) {
	for _, lan := range []string{"192.168.1.20:5000", "10.0.0.4:1", "127.0.0.1:9", "[::1]:9", "169.254.1.1:2", "[fe80::1]:3"} {
		if k := pathKey("ws", lan); k != "" {
			t.Errorf("pathKey(%q) = %q, want none for a LAN client", lan, k)
		}
	}
	a, b := pathKey("ws", "203.0.113.9:4000"), pathKey("quic", "203.0.113.9:4001")
	if a == "" || b == "" || a == b {
		t.Errorf("keys %q and %q: want two distinct non-empty keys for one IP over two transports", a, b)
	}
	if pathKey("ws", "203.0.113.9:4000") != pathKey("ws", "203.0.113.9:5555") {
		t.Error("the same IP on a new source port is a different path")
	}
	if pathKey("ws", "garbage") != "" {
		t.Error("an unparseable address got a key")
	}
}

func TestPathRatesExpireAfterThirtyMinutes(t *testing.T) {
	p := newPathRates()
	now := time.Unix(1000, 0)
	p.now = func() time.Time { return now }
	p.put("k", 900_000)
	if bps, ok := p.get("k"); !ok || bps != 900_000 {
		t.Fatalf("get = %d, %v", bps, ok)
	}
	now = now.Add(29 * time.Minute)
	if _, ok := p.get("k"); !ok {
		t.Error("expired at 29 minutes")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := p.get("k"); ok {
		t.Error("still remembered after 31 minutes")
	}
}

// optsTarget records the options each Open was given.
type optsTarget struct {
	stubTarget
	opens chan target.Options
}

func (o *optsTarget) Open(ctx context.Context, opts target.Options) (target.Stream, error) {
	o.opens <- opts
	return o.stubTarget.Open(ctx, opts)
}

// A returning path starts at the rate remembered for it, an unknown path and a
// LAN client at the configured default, and the controller's ceiling stays the
// configured rate so it can climb back.
func TestAReturningPathStartsAtItsRememberedRate(t *testing.T) {
	m := NewManager(target.Options{BitRate: 4_000_000})
	m.SetAdaptiveBitRate(true)
	tgt := &optsTarget{opens: make(chan target.Options, 4)}
	m.Register(tgt)

	phone := pathKey("quic", "203.0.113.9:4000")
	m.paths.put(phone, 800_000)

	acquire := func(path string) int {
		t.Helper()
		s, err := m.AcquireFrom(context.Background(), "stub", path)
		if err != nil {
			t.Fatal(err)
		}
		got := (<-tgt.opens).BitRate
		_ = s.Close()
		m.mu.Lock()
		delete(m.sessions, "stub")
		m.mu.Unlock()
		return got
	}
	if got := acquire(phone); got != 800_000 {
		t.Errorf("returning path opened at %d, want 800000", got)
	}
	if got := acquire(pathKey("quic", "198.51.100.7:1")); got != unknownPathStartBps {
		t.Errorf("unknown path opened at %d, want the cautious %d", got, unknownPathStartBps)
	}
	if got := acquire(pathKey("quic", "192.168.1.5:1")); got != 4_000_000 {
		t.Errorf("LAN client opened at %d, want the 4 Mbps default", got)
	}
	// A remembered rate above the default never raises it.
	m.paths.put(phone, 9_000_000)
	if got := acquire(phone); got != 4_000_000 {
		t.Errorf("remembered 9 Mbps opened at %d, want capped at the 4 Mbps default", got)
	}
}

// The viewer leaving writes the path's rate once it has held.
func TestALeavingViewerRemembersItsStableRate(t *testing.T) {
	orig := pathStableFor
	pathStableFor = 0
	t.Cleanup(func() { pathStableFor = orig })

	m := NewManager(target.Options{BitRate: 4_000_000})
	m.Register(&stubTarget{})
	s := &Server{mgr: m}
	path := pathKey("ws", "203.0.113.9:1")
	sess, err := m.AcquireFrom(t.Context(), "stub", path)
	if err != nil {
		t.Fatal(err)
	}
	conn := &stalledConn{fakeConn: fakeConn{name: "stalled"}, closed: make(chan struct{})}
	done := make(chan struct{})
	go func() { s.serveViewerWith("stub", sess, conn, session.ViewerOptions{}, path); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = conn.Close()
		t.Fatal("serveViewerWith did not return")
	}
	if bps, ok := m.paths.get(path); !ok || bps != 4_000_000 {
		t.Fatalf("remembered %d, %v; want the session's 4 Mbps", bps, ok)
	}
}
