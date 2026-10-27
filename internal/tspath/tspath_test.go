package tspath

import (
	"errors"
	"net/netip"
	"testing"
)

// A redacted `tailscale status --json` from this Mac: the phone direct on the
// home LAN, a peer direct over the internet, one on a peer relay and one on
// DERP. Only the fields Parse reads are kept.
const fixture = `{"Peer":{
 "a":{"TailscaleIPs":["100.96.135.54","fd7a:115c:a1e0::1"],"CurAddr":"192.168.1.108:41641","Relay":"ord","PeerRelay":""},
 "b":{"TailscaleIPs":["100.70.0.2"],"CurAddr":"172.56.10.20:3478","Relay":"ord","PeerRelay":""},
 "c":{"TailscaleIPs":["100.70.0.3"],"CurAddr":"","Relay":"nyc","PeerRelay":"100.70.0.9:40000:vni:7"},
 "d":{"TailscaleIPs":["100.70.0.4"],"CurAddr":"","Relay":"ord","PeerRelay":""}
}}`

func TestParse(t *testing.T) {
	cases := []struct {
		ip   string
		want Path
	}{
		{"100.96.135.54", Path{Kind: "direct", Addr: "192.168.1.108:41641", Scope: "lan", Relay: "ord"}},
		{"fd7a:115c:a1e0::1", Path{Kind: "direct", Addr: "192.168.1.108:41641", Scope: "lan", Relay: "ord"}},
		{"100.70.0.2", Path{Kind: "direct", Addr: "172.56.10.20:3478", Scope: "internet", Relay: "ord"}},
		{"100.70.0.3", Path{Kind: "peerRelay", Addr: "100.70.0.9:40000:vni:7", Relay: "nyc"}},
		{"100.70.0.4", Path{Kind: "derp", Relay: "ord"}},
	}
	for _, c := range cases {
		got, err := Parse([]byte(fixture), netip.MustParseAddr(c.ip))
		if err != nil || got != c.want {
			t.Errorf("Parse(%s) = %+v, %v; want %+v", c.ip, got, err, c.want)
		}
	}
	if _, err := Parse([]byte(fixture), netip.MustParseAddr("100.70.0.99")); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown peer: err = %v, want ErrNotFound", err)
	}
}

// A QUIC remote arrives as an IPv4-mapped IPv6 address when the listener is
// dual-stack, and it must still match the peer's plain IPv4.
func TestParseUnmapsV4InV6(t *testing.T) {
	got, err := Parse([]byte(fixture), netip.MustParseAddr("::ffff:100.70.0.4"))
	if err != nil || got.Kind != "derp" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestFieldsNameTheRelayOnlyWhenRelayed(t *testing.T) {
	if _, ok := (Path{Kind: "direct", Relay: "ord"}).Fields()["derp"]; ok {
		t.Error("a direct path reported a derp region")
	}
	if got := (Path{Kind: "derp", Relay: "ord"}).Fields()["derp"]; got != "ord" {
		t.Errorf("derp = %v, want ord", got)
	}
}

func TestLookupSkipsAddressesOutsideTheTailnet(t *testing.T) {
	for _, s := range []string{"192.168.1.108", "127.0.0.1", "8.8.8.8"} {
		if _, err := Lookup(t.Context(), netip.MustParseAddr(s)); !errors.Is(err, ErrNotFound) {
			t.Errorf("Lookup(%s) err = %v, want ErrNotFound", s, err)
		}
	}
}
