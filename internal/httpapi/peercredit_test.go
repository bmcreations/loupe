package httpapi

import (
	"net"
	"net/netip"
	"testing"
)

func TestPeerCreditOnlyRises(t *testing.T) {
	addr := netip.MustParseAddrPort("100.91.141.96:51000")
	c := PeerCreditFor(addr)
	defer ForgetPeerCredit(addr, c)
	if c.Uni() != 0 {
		t.Fatalf("fresh credit: %d", c.Uni())
	}
	c.NoteUni(100)
	c.NoteUni(140)
	c.NoteUni(120)
	if got := c.Uni(); got != 140 {
		t.Errorf("Uni = %d, want 140", got)
	}
}

func TestPeerCreditMeetsByAddress(t *testing.T) {
	// The tracer sees an IPv4 address; the connection may report it mapped
	// into IPv6. Both must find the same entry.
	addr := netip.MustParseAddrPort("100.91.141.96:51001")
	c := PeerCreditFor(addr)
	c.NoteUni(7)
	mapped := &net.UDPAddr{IP: net.ParseIP("::ffff:100.91.141.96"), Port: 51001}
	if got := peerCreditOf(mapped); got != c {
		t.Fatalf("mapped address found %v, want the tracer's entry", got)
	}
	ForgetPeerCredit(addr, c)
	if got := peerCreditOf(mapped); got != nil {
		t.Errorf("after close found %v", got)
	}
	if got := peerCreditOf(mapped).Uni(); got != 0 {
		t.Errorf("missing credit Uni = %d", got)
	}
}
