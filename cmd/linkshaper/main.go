// Command linkshaper is a UDP relay that impairs the link between a loupe
// client and a loupe server, so the adaptive bitrate controller can be driven
// through congestion and recovery on demand.
//
// It exists because the controller had only ever been exercised by carrying a
// phone somewhere with bad signal. That is not a test: it is not repeatable, it
// cannot be run twice with one variable changed, and the interesting half --
// recovery -- needs the link to get better again at a known moment.
//
// Userspace and unprivileged, deliberately. dummynet via dnctl/pfctl would be
// more faithful, and needs root on every run; this needs nothing, so it is the
// one that will actually get run.
//
//	go run ./cmd/linkshaper -listen :18457 -to 127.0.0.1:18456 \
//	    -plan "0s rate=0; 20s rate=400k delay=120ms queue=300ms; 110s rate=0"
//
// Then point a client at the shaper's port instead of the server's. Dial the
// tailnet address for the server if the path matters: loopback has a 16KB MTU
// and hides anything the real path cannot carry (trap 11).
package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"sync"
	"time"
)

// relayBuf is the read buffer. QUIC datagrams here are capped at 1200 bytes by
// trap 11's InitialPacketSize, but the buffer is a full MTU so an oversized
// datagram is reported rather than silently truncated into a corrupt one.
const relayBuf = 2048

// queueDepth bounds the in-flight delayed datagrams per direction. It is a
// backstop on memory, not the impairment: the queue the model cares about is
// Link.MaxQueue, which is expressed in time.
const queueDepth = 16384

func main() {
	listen := flag.String("listen", ":18457", "address to accept client packets on")
	to := flag.String("to", "127.0.0.1:18456", "the real server")
	planSrc := flag.String("plan", "0s rate=0", "impairment schedule; see ParsePlan")
	seed := flag.Int64("seed", 1, "random seed, so a run can be repeated exactly")
	every := flag.Duration("report", 5*time.Second, "how often to print counters")
	flag.Parse()

	plan, err := ParsePlan(*planSrc)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad plan:", err)
		os.Exit(2)
	}
	if err := run(*listen, *to, plan, *seed, *every); err != nil {
		log.Fatalln("linkshaper:", err)
	}
}

func run(listen, to string, plan Plan, seed int64, every time.Duration) error {
	srvAddr, err := net.ResolveUDPAddr("udp", to)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", to, err)
	}
	lnAddr, err := net.ResolveUDPAddr("udp", listen)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", listen, err)
	}
	ln, err := net.ListenUDP("udp", lnAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", listen, err)
	}
	defer ln.Close()

	s := &shaper{
		ln: ln, srv: srvAddr, plan: plan,
		start: time.Now(), rnd: rand.New(rand.NewSource(seed)),
		flows: map[string]*flow{},
	}
	fmt.Printf("linkshaper %s -> %s\n", ln.LocalAddr(), srvAddr)
	for _, ph := range plan {
		fmt.Printf("  at %-6v rate=%-9s delay=%-7v jitter=%-7v loss=%.2f queue=%v\n",
			ph.Offset, rateString(ph.Rate), ph.Delay, ph.Jitter, ph.Loss, ph.MaxQueue)
	}
	go s.reportLoop(every)

	buf := make([]byte, relayBuf)
	for {
		n, from, err := ln.ReadFromUDP(buf)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		f, err := s.flowFor(from)
		if err != nil {
			log.Println("flow:", err)
			continue
		}
		f.up.offer(buf[:n], s.profile())
	}
}

type shaper struct {
	ln    *net.UDPConn
	srv   *net.UDPAddr
	plan  Plan
	start time.Time
	rnd   *rand.Rand

	mu    sync.Mutex
	flows map[string]*flow
	stats stats
}

// counts is split from its mutex so a reporter can take a snapshot by value.
type counts struct {
	upSent, upDrop     uint64
	downSent, downDrop uint64
	upBytes, downBytes uint64
}

type stats struct {
	mu sync.Mutex
	counts
}

func (s *shaper) profile() Profile { return s.plan.At(time.Since(s.start)) }

// nextSeed draws a seed for one pipe. Called under s.mu from flowFor, which is
// the only caller, so the sequence is deterministic for a given -seed and a
// given order of arrivals.
func (s *shaper) nextSeed() int64 { return s.rnd.Int63() }

// flowFor returns the relay state for one client address, opening a
// server-facing socket the first time it is seen.
//
// One socket per client, because the server distinguishes connections by the
// address it sees them from. Sharing one would make every client look like the
// same peer to the server, and QUIC would treat the mixture as one connection
// migrating back and forth.
func (s *shaper) flowFor(from *net.UDPAddr) (*flow, error) {
	key := from.String()
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.flows[key]; ok {
		return f, nil
	}
	srvConn, err := net.DialUDP("udp", nil, s.srv)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", s.srv, err)
	}
	// Each pipe gets its own source. rand.Rand is not safe for concurrent use
	// and the two directions run on different goroutines, so sharing one would
	// be a data race -- and a shaper that corrupts its own impairment is worse
	// than no shaper, because the run still produces numbers.
	f := &flow{client: from, srv: srvConn}
	f.up = newPipe(s, "up", s.nextSeed(), func(b []byte) { _, _ = srvConn.Write(b) })
	f.down = newPipe(s, "down", s.nextSeed(), func(b []byte) { _, _ = s.ln.WriteToUDP(b, from) })
	s.flows[key] = f
	fmt.Printf("flow %s <-> %s\n", key, s.srv)

	go func() {
		buf := make([]byte, relayBuf)
		for {
			n, err := srvConn.Read(buf)
			if err != nil {
				return
			}
			f.down.offer(buf[:n], s.profile())
		}
	}()
	return f, nil
}

type flow struct {
	client   *net.UDPAddr
	srv      *net.UDPConn
	up, down *pipe
}

// pipe applies a Link to one direction and delivers on time.
//
// Delivery is a plain FIFO rather than a priority queue, which is sound only
// because Link.Schedule never reorders: release times come out monotonically
// non-decreasing, so the datagram at the head of the queue is always the next
// one due.
type pipe struct {
	shaper *shaper
	dir    string
	link   Link
	q      chan timed
	write  func([]byte)
}

type timed struct {
	release time.Time
	payload []byte
}

func newPipe(s *shaper, dir string, seed int64, write func([]byte)) *pipe {
	p := &pipe{shaper: s, dir: dir, q: make(chan timed, queueDepth), write: write}
	p.link.Rand = rand.New(rand.NewSource(seed))
	go p.deliver()
	return p
}

func (p *pipe) offer(b []byte, prof Profile) {
	p.link.Rate, p.link.Delay = prof.Rate, prof.Delay
	p.link.Jitter, p.link.Loss, p.link.MaxQueue = prof.Jitter, prof.Loss, prof.MaxQueue

	fate := p.link.Schedule(time.Now(), len(b))
	if fate.Drop {
		p.shaper.stats.note(p.dir, 0, false)
		return
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	select {
	case p.q <- timed{release: fate.Release, payload: cp}:
	default:
		// The depth backstop, not the model. Reaching it means the configured
		// queue is deeper than this many datagrams, which is a plan worth
		// knowing about rather than silently absorbing.
		p.shaper.stats.note(p.dir, 0, false)
	}
}

func (p *pipe) deliver() {
	for item := range p.q {
		if d := time.Until(item.release); d > 0 {
			time.Sleep(d)
		}
		p.write(item.payload)
		p.shaper.stats.note(p.dir, len(item.payload), true)
	}
}

func (s *stats) note(dir string, n int, sent bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if dir == "up" {
		if sent {
			s.upSent++
			s.upBytes += uint64(n)
		} else {
			s.upDrop++
		}
		return
	}
	if sent {
		s.downSent++
		s.downBytes += uint64(n)
	} else {
		s.downDrop++
	}
}

func (s *shaper) reportLoop(every time.Duration) {
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	var last counts
	for range t.C {
		s.stats.mu.Lock()
		cur := s.stats.counts
		s.stats.mu.Unlock()

		prof := s.profile()
		secs := every.Seconds()
		fmt.Printf("%6.0fs %-9s down %5d pkt %8.0f kbps drop %4d | up %5d pkt drop %4d\n",
			time.Since(s.start).Seconds(), rateString(prof.Rate),
			cur.downSent-last.downSent,
			float64(cur.downBytes-last.downBytes)*8/1000/secs,
			cur.downDrop-last.downDrop,
			cur.upSent-last.upSent, cur.upDrop-last.upDrop)
		last = cur
	}
}

func rateString(bps int) string {
	switch {
	case bps <= 0:
		return "unshaped"
	case bps >= 1e6:
		return fmt.Sprintf("%.1fM", float64(bps)/1e6)
	default:
		return fmt.Sprintf("%.0fk", float64(bps)/1e3)
	}
}
