package httpapi

import (
	"net"
	"reflect"
	"testing"
)

func TestLANIPsKeepsPrivateIPv4Only(t *testing.T) {
	cidr := func(s string) net.Addr {
		ip, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatal(err)
		}
		n.IP = ip
		return n
	}
	got := lanIPs([]net.Addr{
		cidr("192.168.1.78/24"),
		cidr("100.101.102.103/32"), // Tailscale
		cidr("169.254.10.1/16"),    // link-local
		cidr("127.0.0.1/8"),
		cidr("153.66.177.107/24"), // public
		cidr("10.0.0.5/8"),
		cidr("172.20.1.2/16"),
		cidr("fd7a:115c:a1e0::1/128"),
		cidr("192.168.1.78/24"), // repeat
	})
	want := []string{"10.0.0.5", "172.20.1.2", "192.168.1.78"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lanIPs = %v, want %v", got, want)
	}
}

func TestSetLANNeedsEveryInterfaceAndACert(t *testing.T) {
	s := &Server{}
	s.SetLAN(":18456", nil)
	if s.lan != nil {
		t.Fatal("no certificate should leave /api/lan off")
	}
	s.SetLAN("127.0.0.1:18456", nil)
	if s.lan != nil {
		t.Fatal("a loopback listener should leave /api/lan off")
	}
}
