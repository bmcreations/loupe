package httpapi

import (
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
)

// PeerCredit is the most unidirectional streams a QUIC peer has allowed on
// one connection, learned from its transport parameters and MAX_STREAMS
// frames. quic-go does not expose the number, but its qlog tracer sees both,
// so the tracer writes it here and wtConn reads it to tell how much room to
// open streams is left (peerwindow.go, lowCredit).
type PeerCredit struct {
	uni atomic.Uint64
}

// NoteUni records a limit the peer granted. Limits only rise, and frames can
// arrive out of order, so a lower one is ignored.
func (c *PeerCredit) NoteUni(n uint64) {
	if c == nil {
		return
	}
	for {
		cur := c.uni.Load()
		if n <= cur || c.uni.CompareAndSwap(cur, n) {
			return
		}
	}
}

// Uni is the limit, or zero before the peer has sent one.
func (c *PeerCredit) Uni() uint64 {
	if c == nil {
		return 0
	}
	return c.uni.Load()
}

// The tracer and the connection meet by the peer's address. quic-go gives the
// tracer only a parent of the connection's context, which nothing can add a
// value to afterwards, and the http3 ConnContext runs after the tracer is
// made. A peer that migrates gets a new entry the connection never reads, so
// it reports zero -- unknown -- and the credit-low rule stays out of the way.
var peerCredits sync.Map // netip.AddrPort -> *PeerCredit

// PeerCreditFor returns the credit for a peer address, making it if needed.
func PeerCreditFor(addr netip.AddrPort) *PeerCredit {
	addr = netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
	c, _ := peerCredits.LoadOrStore(addr, &PeerCredit{})
	return c.(*PeerCredit)
}

// ForgetPeerCredit drops a closed connection's entry, if it is still c.
func ForgetPeerCredit(addr netip.AddrPort, c *PeerCredit) {
	addr = netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
	peerCredits.CompareAndDelete(addr, c)
}

// peerCreditOf looks up a connection's credit by its remote address, or nil.
func peerCreditOf(remote net.Addr) *PeerCredit {
	u, ok := remote.(*net.UDPAddr)
	if !ok {
		return nil
	}
	ap := u.AddrPort()
	ap = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	c, ok := peerCredits.Load(ap)
	if !ok {
		return nil
	}
	return c.(*PeerCredit)
}
