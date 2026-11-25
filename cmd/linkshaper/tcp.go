package main

import (
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"sync"
	"time"
)

// tcpChunk is the most the shaper reads from one side at a time. A chunk is
// scheduled as one unit, so it is kept well under any window worth setting.
const tcpChunk = 16 << 10

// tcpSockBuf shrinks the kernel buffers on the shaper's own sockets, so the
// bytes held between the two ends are mostly the ones the window counts and
// not a kernel buffer the plan cannot see.
const tcpSockBuf = 64 << 10

// tcpQueueDepth bounds the chunks waiting for release per direction. A full
// queue blocks the reader, which is backpressure, not loss.
const tcpQueueDepth = 4096

// dialUpstream returns how the shaper reaches the server. src, when set, is
// the local IP to dial from: the server tells a local viewer from a remote one
// by the peer address it sees, and a loopback server dialled from a LAN
// address sees that address, so a shaped viewer on the same Mac is judged as
// the remote viewer it stands in for.
func dialUpstream(to, src string) (func() (net.Conn, error), error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	if src != "" {
		ip := net.ParseIP(src)
		if ip == nil {
			return nil, fmt.Errorf("-src %q is not an IP address", src)
		}
		d.LocalAddr = &net.TCPAddr{IP: ip}
	}
	return func() (net.Conn, error) { return d.Dial("tcp", to) }, nil
}

// passTCP relays every TCP connection on ln to the server, unshaped. A browser
// viewer loads the page over TCP and then dials WebTransport at the page's own
// host:port, so the shaper's port has to serve the page too. Only the UDP side
// (the stream) is the link under test; the page and its API calls are not.
func passTCP(ln net.Listener, dial func() (net.Conn, error)) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			s, err := dial()
			if err != nil {
				log.Println("tcp:", err)
				return
			}
			defer s.Close()
			done := make(chan struct{}, 2)
			go func() { io.Copy(s, c); done <- struct{}{} }()
			go func() { io.Copy(c, s); done <- struct{}{} }()
			<-done
		}()
	}
}

// tcpShaper applies a Plan to TCP connections: the WebSocket viewer's link.
//
// A byte stream cannot drop, so the impairments are the ones TCP shows its
// application: added delay, a bandwidth cap, and delay spikes. A spike is a
// head-of-line stall. Bytes read while it lasts are held for the spike's
// delay, and bytes after it queue behind them in order, so when it ends they
// arrive together, which is what a relay that stalls and catches up delivers.
type tcpShaper struct {
	plan   Plan
	start  time.Time
	spikes *spiker

	mu    sync.Mutex
	rnd   *rand.Rand
	pipes []*tcpPipe
}

func newTCPShaper(plan Plan, seed int64, start time.Time) *tcpShaper {
	rnd := rand.New(rand.NewSource(seed))
	s := &tcpShaper{plan: plan, start: start, rnd: rnd}
	s.spikes = &spiker{rnd: rand.New(rand.NewSource(rnd.Int63()))}
	return s
}

func (s *tcpShaper) serve(ln net.Listener, dial func() (net.Conn, error)) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c, dial)
	}
}

func (s *tcpShaper) handle(c net.Conn, dial func() (net.Conn, error)) {
	defer c.Close()
	u, err := dial()
	if err != nil {
		log.Println("tcp:", err)
		return
	}
	defer u.Close()
	shrinkBuffers(c)
	shrinkBuffers(u)
	fmt.Printf("%6.1fs tcp %s -> %s from %s\n", time.Since(s.start).Seconds(), c.RemoteAddr(), u.RemoteAddr(), u.LocalAddr())

	var once sync.Once
	kill := func() { once.Do(func() { c.Close(); u.Close() }) }
	up, down := s.newPipe("up"), s.newPipe("down")
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); up.relay(c, u, kill) }()
	go func() { defer wg.Done(); down.relay(u, c, kill) }()
	wg.Wait()
	fmt.Printf("%6.1fs tcp %s closed\n", time.Since(s.start).Seconds(), c.RemoteAddr())
}

func shrinkBuffers(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(tcpSockBuf)
		_ = tc.SetWriteBuffer(tcpSockBuf)
	}
}

func (s *tcpShaper) newPipe(dir string) *tcpPipe {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := &tcpPipe{dir: dir, shaper: s, q: make(chan timed, tcpQueueDepth)}
	// Jitter draws from the link's own source; rand.Rand is not safe across
	// the two directions' goroutines.
	p.link.Rand = rand.New(rand.NewSource(s.rnd.Int63()))
	s.pipes = append(s.pipes, p)
	return p
}

// tcpPipe is one direction of one shaped connection.
//
// Only the reader goroutine touches link, so it needs no lock; the counters
// are shared with the writer and the reporter.
type tcpPipe struct {
	dir    string
	shaper *tcpShaper
	link   Link
	q      chan timed

	mu       sync.Mutex
	inflight int // read from the source, not yet written to the destination
	peak     int
	written  uint64
}

// relay reads src until it ends and delivers each chunk to dst on time.
func (p *tcpPipe) relay(src, dst net.Conn, kill func()) {
	done := make(chan struct{})
	go func() { p.deliver(dst, kill); close(done) }()
	buf := make([]byte, tcpChunk)
	for {
		p.waitRoom()
		n, err := src.Read(buf)
		if n > 0 {
			p.offer(time.Now(), buf[:n])
		}
		if err != nil {
			break
		}
	}
	close(p.q)
	<-done
}

// schedule is when a chunk of n bytes read at now may be written: the plan's
// profile at now, with a spike's delay in place of Delay while one lasts.
func (p *tcpPipe) schedule(now time.Time, n int) time.Time {
	elapsed := now.Sub(p.shaper.start)
	prof := p.shaper.plan.At(elapsed)
	delay := prof.Delay
	if d, ok := p.shaper.spikes.At(elapsed, prof); ok {
		delay = d
	}
	p.link.Rate, p.link.Delay, p.link.Jitter = prof.Rate, delay, prof.Jitter
	// Never drops: CheckTCP refused loss, and a full queue stops the reader
	// (waitRoom) instead of tail-dropping.
	p.link.Loss, p.link.MaxQueue = 0, 0
	return p.link.Schedule(now, n).Release
}

func (p *tcpPipe) offer(now time.Time, b []byte) {
	release := p.schedule(now, len(b))
	cp := make([]byte, len(b))
	copy(cp, b)
	p.mu.Lock()
	p.inflight += len(b)
	if p.inflight > p.peak {
		p.peak = p.inflight
	}
	p.mu.Unlock()
	p.q <- timed{release: release, payload: cp}
}

// waitRoom holds the reader while the plan says the path is full. Not reading
// is the whole mechanism: the shaper's socket buffer fills, then the sender's,
// and the sender's writes block as they would behind a congested path.
func (p *tcpPipe) waitRoom() {
	for {
		now := time.Now()
		prof := p.shaper.plan.At(now.Sub(p.shaper.start))
		p.mu.Lock()
		in := p.inflight
		p.mu.Unlock()
		if !pathFull(prof, in, p.link.Backlog(now)) {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// pathFull reports whether the shaper should stop reading: more bytes held
// than the window, or a serialisation backlog longer than queue.
func pathFull(prof Profile, inflight int, backlog time.Duration) bool {
	if prof.Window > 0 && inflight >= prof.Window {
		return true
	}
	return prof.MaxQueue > 0 && backlog > prof.MaxQueue
}

func (p *tcpPipe) deliver(dst net.Conn, kill func()) {
	failed := false
	for item := range p.q {
		if !failed {
			if d := time.Until(item.release); d > 0 {
				time.Sleep(d)
			}
			if _, err := dst.Write(item.payload); err != nil {
				failed = true
				kill()
			}
		}
		// Drained even after a failure, so a reader parked in waitRoom sees
		// room, reads its closed socket and ends.
		p.mu.Lock()
		p.inflight -= len(item.payload)
		if !failed {
			p.written += uint64(len(item.payload))
		}
		p.mu.Unlock()
	}
	if !failed {
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}
}

// snapshot totals the bytes written and the largest in-flight count per
// direction across every connection, for the report line.
func (s *tcpShaper) snapshot() (up, down uint64, upPeak, downPeak int) {
	s.mu.Lock()
	pipes := append([]*tcpPipe(nil), s.pipes...)
	s.mu.Unlock()
	for _, p := range pipes {
		p.mu.Lock()
		if p.dir == "up" {
			up += p.written
			upPeak = max(upPeak, p.peak)
		} else {
			down += p.written
			downPeak = max(downPeak, p.peak)
		}
		p.mu.Unlock()
	}
	return
}

func (s *tcpShaper) reportLoop(every time.Duration) {
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	var lastUp, lastDown uint64
	for range t.C {
		up, down, upPeak, downPeak := s.snapshot()
		secs := every.Seconds()
		prof := s.plan.At(time.Since(s.start))
		fmt.Printf("%6.0fs %-9s down %8.0f kbps peak held %7d B | up %6.0f kbps peak held %6d B\n",
			time.Since(s.start).Seconds(), rateString(prof.Rate),
			float64(down-lastDown)*8/1000/secs, downPeak,
			float64(up-lastUp)*8/1000/secs, upPeak)
		lastUp, lastDown = up, down
	}
}

// spikeWindow is one delay spike, as offsets from the shaper's start.
type spikeWindow struct {
	Start, Len, Delay time.Duration
}

// spiker decides when the link's delay spikes. One per shaper, shared by both
// directions and every connection, because a stall on a relay is a property of
// the path rather than of one flow. Safe for concurrent use.
//
// Windows are drawn lazily as time is asked about, from a seeded source, so a
// run is repeatable for a given -seed and its log names each spike.
type spiker struct {
	mu      sync.Mutex
	rnd     *rand.Rand
	armed   bool
	next    time.Duration
	cur     spikeWindow
	onSpike func(spikeWindow)
}

// At reports the spike delay in force at elapsed under prof, if any.
func (s *spiker) At(elapsed time.Duration, prof Profile) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !prof.spikes() {
		// A phase without spikes disarms, so the next spiking phase waits a
		// whole interval from its own start rather than firing at once.
		s.armed = false
		return 0, false
	}
	if !s.armed {
		s.armed = true
		s.next = elapsed + draw(s.rnd, prof.SpikeEvery)
	}
	for elapsed >= s.next {
		w := spikeWindow{Start: s.next, Len: draw(s.rnd, prof.SpikeLen), Delay: draw(s.rnd, prof.Spike)}
		s.cur = w
		if s.onSpike != nil {
			s.onSpike(w)
		}
		s.next += draw(s.rnd, prof.SpikeEvery)
	}
	if s.cur.Len > 0 && elapsed >= s.cur.Start && elapsed < s.cur.Start+s.cur.Len {
		return s.cur.Delay, true
	}
	return 0, false
}

func draw(rnd *rand.Rand, s Span) time.Duration {
	if s.Max <= s.Min {
		return s.Min
	}
	return s.Min + time.Duration(rnd.Int63n(int64(s.Max-s.Min)+1))
}
