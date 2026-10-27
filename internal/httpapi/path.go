package httpapi

import (
	"context"
	"net"
	"net/netip"
	"time"

	"github.com/bmcreations/loupe/internal/tspath"
)

// pathRecheck is when the path is read a second time. A Tailscale connection
// starts on DERP and moves to a direct path once hole punching succeeds,
// usually within a few seconds, so a single reading at open would call most
// sessions relayed.
const pathRecheck = 10 * time.Second

// recordPath traces how Tailscale carries this viewer's traffic, as one
// kind:"path" record at open and a second at pathRecheck if it changed. It
// returns at once; the reads run on their own goroutine.
//
// This is what lets the trace tell a phone on cellular (direct, scope
// internet, or derp) from one on the home Wi-Fi (direct, scope lan). A viewer
// that did not arrive over Tailscale records nothing.
func (s *Server) recordPath(transport, targetID, remoteAddr string) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !tspath.IsTailnet(ip) {
		return
	}
	go func() {
		rec := func(p tspath.Path, after time.Duration) {
			m := p.Fields()
			m["kind"] = "path"
			m["transport"] = transport
			m["target"] = targetID
			m["remote"] = ip.Unmap().String()
			m["afterMs"] = after.Milliseconds()
			s.trace.Record(m)
		}
		first, err := tspath.Lookup(context.Background(), ip)
		if err != nil {
			return
		}
		rec(first, 0)
		time.Sleep(pathRecheck)
		again, err := tspath.Lookup(context.Background(), ip)
		if err == nil && again != first {
			rec(again, pathRecheck)
		}
	}()
}
