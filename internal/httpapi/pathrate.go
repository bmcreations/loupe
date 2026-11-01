package httpapi

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

// Resuming at the last bitrate that worked.
//
// Every new session opened at the configured 4 Mbps, so a phone on a 1 Mbps
// cellular link spent its first seconds in the queueing cascade each time it
// reconnected (the 12:38 trace). The manager remembers, per client path, the
// rate a session had held steadily when that client left, and a session opened
// for the same path starts there and climbs back by the controller's normal
// recovery.
const (
	// pathRateTTL is how long a remembered rate is trusted. A cellular link
	// changes with where the phone is; half an hour is a commute or a meeting.
	pathRateTTL = 30 * time.Minute

	maxPathRates = 256
)

// pathStableFor is how long a session's rate must have held when the viewer
// leaves for that rate to count as one that worked. A variable so a test need
// not wait it out.
var pathStableFor = 20 * time.Second

type pathRate struct {
	bps int
	at  time.Time
}

// pathRates is an in-memory map of client path to last stable bitrate.
type pathRates struct {
	mu  sync.Mutex
	m   map[string]pathRate
	now func() time.Time
}

func newPathRates() *pathRates {
	return &pathRates{m: map[string]pathRate{}, now: time.Now}
}

// pathKey names a client path: its remote IP and the transport, or "" for a
// path worth no memory. A LAN client (private, loopback or link-local) keeps
// starting at the default, since its link is not the thing that varies.
// Tailnet addresses are remembered: a phone reaches the Mac over a tailnet from
// cellular as often as from wifi, and an expired or wrong entry only costs a
// climb.
func pathKey(transport, remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return ""
	}
	ip = ip.Unmap()
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return ""
	}
	return ip.String() + "|" + transport
}

// get returns the remembered rate for key, or false when there is none or it
// has expired.
func (p *pathRates) get(key string) (int, bool) {
	if key == "" {
		return 0, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.m[key]
	if !ok {
		return 0, false
	}
	if p.now().Sub(r.at) > pathRateTTL {
		delete(p.m, key)
		return 0, false
	}
	return r.bps, true
}

// put remembers bps for key, dropping expired entries (and the oldest when the
// map is full) so it cannot grow without bound.
func (p *pathRates) put(key string, bps int) {
	if key == "" || bps <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for k, r := range p.m {
		if now.Sub(r.at) > pathRateTTL {
			delete(p.m, k)
		}
	}
	if _, ok := p.m[key]; !ok && len(p.m) >= maxPathRates {
		var oldest string
		var at time.Time
		for k, r := range p.m {
			if oldest == "" || r.at.Before(at) {
				oldest, at = k, r.at
			}
		}
		delete(p.m, oldest)
	}
	p.m[key] = pathRate{bps: bps, at: now}
}
