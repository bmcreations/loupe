package main

import (
	"bufio"
	"math/rand"
	"net"
	"testing"
	"time"
)

// A browser loads the page over TCP and then opens WebTransport to the same
// host:port over UDP. With -tcp the shaper's port answers both, so the page
// reaches the server unshaped and the stream it opens goes through the plan.
func TestTCPPassesThroughToTheServer(t *testing.T) {
	srv, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go func() {
		c, err := srv.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		line, _ := bufio.NewReader(c).ReadString('\n')
		c.Write([]byte("echo " + line))
	}()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dial, err := dialUpstream(srv.Addr().String(), "")
	if err != nil {
		t.Fatal(err)
	}
	go passTCP(ln, dial)
	defer ln.Close()

	c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	got, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if got != "echo hello\n" {
		t.Fatalf("got %q through the relay, want %q", got, "echo hello\n")
	}
}

const relayProfile = "0s rate=5M delay=45ms spike=250ms-950ms spikelen=200ms-2s spikeevery=5s-20s window=256k"

func TestTCPPlanKeysParse(t *testing.T) {
	p, err := ParsePlan(relayProfile)
	if err != nil {
		t.Fatal(err)
	}
	ph := p.At(0)
	want := Profile{
		Rate: 5_000_000, Delay: 45 * time.Millisecond,
		Spike:      Span{250 * time.Millisecond, 950 * time.Millisecond},
		SpikeLen:   Span{200 * time.Millisecond, 2 * time.Second},
		SpikeEvery: Span{5 * time.Second, 20 * time.Second},
		Window:     256 << 10,
	}
	ph.Name = ""
	if ph != want {
		t.Fatalf("parsed %+v, want %+v", ph, want)
	}
	if err := p.CheckTCP(); err != nil {
		t.Errorf("CheckTCP: %v", err)
	}
	// The UDP relay has no spikes or window; running this plan there would
	// report a spiky link that never spiked.
	if err := p.CheckUDP(); err == nil {
		t.Error("CheckUDP accepted spike and window keys")
	}
}

func TestTCPPlanRefusesWhatAStreamCannotDo(t *testing.T) {
	p, err := ParsePlan("0s delay=45ms loss=0.01")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.CheckTCP(); err == nil {
		t.Error("CheckTCP accepted loss on a byte stream")
	}
	for _, bad := range []string{
		"0s spike=500ms",                              // no length or interval
		"0s spike=500ms spikelen=1s",                  // no interval
		"0s spike=500ms spikelen=3s spikeevery=2s-5s", // overlapping spikes
		"0s spike=900ms-200ms spikelen=1s spikeevery=5s",
		"0s window=-1",
	} {
		if _, err := ParsePlan(bad); err == nil {
			t.Errorf("ParsePlan(%q) accepted it", bad)
		}
	}
}

// Spikes are drawn from the seed, so two runs with one -seed see the same
// link, and every spike lies inside the plan's spans.
func TestSpikesRepeatForASeedAndStayInTheirSpans(t *testing.T) {
	p, _ := ParsePlan(relayProfile)
	prof := p.At(0)
	windows := func(seed int64) []spikeWindow {
		var got []spikeWindow
		s := &spiker{rnd: rand.New(rand.NewSource(seed)), onSpike: func(w spikeWindow) { got = append(got, w) }}
		for at := time.Duration(0); at < 10*time.Minute; at += 10 * time.Millisecond {
			s.At(at, prof)
		}
		return got
	}
	a, b := windows(7), windows(7)
	if len(a) < 10 {
		t.Fatalf("%d spikes in 10 minutes at one per 5-20s", len(a))
	}
	if len(a) != len(b) {
		t.Fatalf("seed 7 drew %d then %d spikes", len(a), len(b))
	}
	var prevEnd time.Duration
	for i, w := range a {
		if w != b[i] {
			t.Fatalf("spike %d: %+v then %+v for one seed", i, w, b[i])
		}
		if w.Delay < prof.Spike.Min || w.Delay > prof.Spike.Max ||
			w.Len < prof.SpikeLen.Min || w.Len > prof.SpikeLen.Max {
			t.Errorf("spike %d %+v is outside the plan", i, w)
		}
		if i > 0 && w.Start-a[i-1].Start < prof.SpikeEvery.Min {
			t.Errorf("spike %d starts %v after the last, under spikeevery %v", i, w.Start-a[i-1].Start, prof.SpikeEvery.Min)
		}
		if w.Start < prevEnd {
			t.Errorf("spike %d overlaps the one before", i)
		}
		prevEnd = w.Start + w.Len
	}
}

func TestSpikeDelayHoldsOnlyInsideItsWindow(t *testing.T) {
	prof := Profile{
		Spike: Span{800 * time.Millisecond, 800 * time.Millisecond}, SpikeLen: Span{time.Second, time.Second},
		SpikeEvery: Span{5 * time.Second, 5 * time.Second},
	}
	s := &spiker{rnd: rand.New(rand.NewSource(1))}
	for _, c := range []struct {
		at   time.Duration
		in   bool
		name string
	}{
		{0, false, "arming"},
		{4999 * time.Millisecond, false, "before the first"},
		{5 * time.Second, true, "first spike's start"},
		{5999 * time.Millisecond, true, "first spike's end"},
		{6 * time.Second, false, "after the first"},
		{10500 * time.Millisecond, true, "second spike"},
	} {
		d, ok := s.At(c.at, prof)
		if ok != c.in || (ok && d != 800*time.Millisecond) {
			t.Errorf("%s (%v): delay %v spiking %v, want spiking %v", c.name, c.at, d, ok, c.in)
		}
	}
	// A phase without spikes clears them, and the next spiking phase waits a
	// whole interval from its own start.
	if _, ok := s.At(11*time.Second, Profile{}); ok {
		t.Error("spiking in a phase that has none")
	}
	if _, ok := s.At(12*time.Second, prof); ok {
		t.Error("spiked at once on re-entering a spiking phase")
	}
}

// A spike is a head-of-line stall. Bytes read during it wait the spike's
// delay, and bytes read after it are clamped behind them rather than
// overtaking, so the stream arrives in order and in a burst when it ends.
func TestASpikeStallsTheStreamInOrder(t *testing.T) {
	p, _ := ParsePlan("0s delay=45ms spike=900ms spikelen=1s spikeevery=10s")
	t0 := time.Unix(1_000_000, 0)
	sh := newTCPShaper(p, 1, t0)
	pipe := sh.newPipe("down")

	var last time.Time
	for ms := 0; ms <= 12_500; ms += 50 {
		now := t0.Add(time.Duration(ms) * time.Millisecond)
		rel := pipe.schedule(now, 1000)
		if rel.Before(last) {
			t.Fatalf("chunk read at %dms released at %v, before the chunk ahead of it at %v", ms, rel.Sub(t0), last.Sub(t0))
		}
		last = rel
		lag := rel.Sub(now)
		switch {
		case ms < 10_000:
			if lag != 45*time.Millisecond {
				t.Errorf("at %dms, before the spike: lag %v, want the base 45ms", ms, lag)
			}
		case ms < 11_000:
			if lag != 900*time.Millisecond {
				t.Errorf("at %dms, in the spike: lag %v, want 900ms", ms, lag)
			}
		case ms+45 < 11_850:
			// Read after the spike ended, still queued behind its last chunk.
			if want := t0.Add(10_950*time.Millisecond + 900*time.Millisecond); !rel.Equal(want) {
				t.Errorf("at %dms, after the spike: released %v, want clamped to %v", ms, rel.Sub(t0), want.Sub(t0))
			}
		default:
			if lag != 45*time.Millisecond {
				t.Errorf("at %dms, recovered: lag %v, want 45ms", ms, lag)
			}
		}
	}
}

func TestRateCapsTheStream(t *testing.T) {
	// 800 kbps is 100 kB/s: 50 chunks of 10 kB read at one instant take five
	// seconds to serialise, and leave in the order they were read.
	p, _ := ParsePlan("0s rate=800k delay=20ms")
	t0 := time.Unix(1_000_000, 0)
	pipe := newTCPShaper(p, 1, t0).newPipe("down")

	var last time.Time
	for i := 0; i < 50; i++ {
		rel := pipe.schedule(t0, 10_000)
		if rel.Before(last) {
			t.Fatalf("chunk %d released at %v, before the one ahead of it at %v", i, rel.Sub(t0), last.Sub(t0))
		}
		last = rel
	}
	got := last.Sub(t0)
	if want := 5*time.Second + 20*time.Millisecond; got < want-50*time.Millisecond || got > want+50*time.Millisecond {
		t.Errorf("500 kB at 800 kbps: last chunk released at %v, want about %v", got, want)
	}

	// Unshaped, the same burst leaves after the delay alone.
	unshaped, _ := ParsePlan("0s rate=0 delay=20ms")
	free := newTCPShaper(unshaped, 1, t0).newPipe("down")
	for i := 0; i < 50; i++ {
		last = free.schedule(t0, 10_000)
	}
	if got := last.Sub(t0); got != 20*time.Millisecond {
		t.Errorf("unshaped: last chunk released at %v, want 20ms", got)
	}
}

func TestPathFullOnWindowOrQueue(t *testing.T) {
	prof := Profile{Window: 1000, MaxQueue: 200 * time.Millisecond}
	for _, c := range []struct {
		inflight int
		backlog  time.Duration
		full     bool
	}{
		{999, 0, false},
		{1000, 0, true},
		{0, 200 * time.Millisecond, false},
		{0, 201 * time.Millisecond, true},
	} {
		if got := pathFull(prof, c.inflight, c.backlog); got != c.full {
			t.Errorf("inflight %d backlog %v: full %v, want %v", c.inflight, c.backlog, got, c.full)
		}
	}
	if pathFull(Profile{}, 1<<30, time.Hour) {
		t.Error("an unbounded profile reported full")
	}
}

// shapedPair runs a sink server and a shaper in front of it, and returns a
// connection to the shaper and the server side of the relayed connection.
func shapedPair(t *testing.T, plan string) (client, server net.Conn) {
	t.Helper()
	p, err := ParsePlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := srv.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	dial, _ := dialUpstream(srv.Addr().String(), "")
	go newTCPShaper(p, 1, time.Now()).serve(ln, dial)
	client, err = net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	select {
	case server = <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("the shaper never dialled the server")
	}
	t.Cleanup(func() { server.Close() })
	return client, server
}

// Through real sockets: an echo takes at least the plan's delay each way. Only
// the lower bound is tight; the upper one is loose so a loaded machine does not
// fail make test.
func TestShapedTCPDelaysBothWays(t *testing.T) {
	client, server := shapedPair(t, "0s delay=60ms")
	go func() {
		line, err := bufio.NewReader(server).ReadString('\n')
		if err == nil {
			server.Write([]byte("echo " + line))
		}
	}()
	client.SetDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	if _, err := client.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	got, err := bufio.NewReader(client).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	if got != "echo hello\n" {
		t.Fatalf("got %q, want the echo", got)
	}
	if took < 120*time.Millisecond || took > 3*time.Second {
		t.Errorf("round trip took %v, want at least 2 x 60ms", took)
	}
}

// The window is what makes the server feel the link: past it the shaper stops
// reading and the sender's writes block, as they do behind a real path. With
// no window the same writes are all accepted into the shaper's queue.
func TestTheWindowBlocksTheSender(t *testing.T) {
	const total = 32 << 20
	pushed := func(plan string) int {
		client, _ := shapedPair(t, plan)
		client.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
		n, _ := client.Write(make([]byte, total))
		return n
	}
	held := pushed("0s delay=5s window=64k")
	free := pushed("0s delay=5s")
	if held >= 8<<20 {
		t.Errorf("with a 64k window the sender pushed %d bytes in 500ms; it should have blocked", held)
	}
	if free <= held {
		t.Errorf("without a window the sender pushed %d bytes, no more than the %d with one", free, held)
	}
	t.Logf("pushed in 500ms: %d bytes with a 64k window, %d without", held, free)
}
