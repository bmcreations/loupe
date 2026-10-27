// Package tspath reads how Tailscale is carrying traffic to a peer: directly,
// through a peer relay, or through a DERP server.
//
// The trace could not tell a cellular session from a Wi-Fi one. Every viewer
// arrives from its Tailscale address, so quic.remote names the phone and says
// nothing about the path underneath. Tailscale knows: a peer with a CurAddr
// is direct, and that address is a LAN one on Wi-Fi at home and a public one
// anywhere else. A peer without one is relayed, which costs latency that no
// setting here can win back.
//
// Loupe does not require Tailscale, so every failure here is quiet: no CLI,
// a CLI that errors, or an address outside Tailscale's range all mean there
// is nothing to record.
package tspath

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Path is one reading of how traffic reaches a peer.
type Path struct {
	// Kind is "direct", "peerRelay" or "derp".
	Kind string
	// Addr is the peer's endpoint when Kind is "direct", or the relay's when
	// it is "peerRelay".
	Addr string
	// Scope is "lan" when a direct Addr is a private address and "internet"
	// when it is public. Empty otherwise.
	Scope string
	// Relay is the peer's home DERP region. Tailscale reports it on direct
	// peers too, so it names the path only when Kind is "derp".
	Relay string
}

// Fields returns p as trace fields.
func (p Path) Fields() map[string]any {
	m := map[string]any{"path": p.Kind}
	if p.Addr != "" {
		m["pathAddr"] = p.Addr
	}
	if p.Scope != "" {
		m["pathScope"] = p.Scope
	}
	if p.Kind == "derp" && p.Relay != "" {
		m["derp"] = p.Relay
	}
	return m
}

// cgnat is the range Tailscale assigns its IPv4 addresses from.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// tailnetV6 is Tailscale's IPv6 ULA range.
var tailnetV6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")

// IsTailnet reports whether ip is in a range Tailscale assigns.
func IsTailnet(ip netip.Addr) bool {
	ip = ip.Unmap()
	return cgnat.Contains(ip) || tailnetV6.Contains(ip)
}

// ErrNotFound means the status lists no peer with that address.
var ErrNotFound = errors.New("no tailscale peer with that address")

type status struct {
	Peer map[string]struct {
		TailscaleIPs []string
		CurAddr      string
		Relay        string
		PeerRelay    string
	}
}

// Parse finds the peer holding ip in `tailscale status --json` output.
func Parse(data []byte, ip netip.Addr) (Path, error) {
	var st status
	if err := json.Unmarshal(data, &st); err != nil {
		return Path{}, err
	}
	ip = ip.Unmap()
	for _, p := range st.Peer {
		for _, s := range p.TailscaleIPs {
			a, err := netip.ParseAddr(s)
			if err != nil || a != ip {
				continue
			}
			switch {
			case p.CurAddr != "":
				out := Path{Kind: "direct", Addr: p.CurAddr, Relay: p.Relay}
				if ap, err := netip.ParseAddrPort(p.CurAddr); err == nil {
					if ap.Addr().IsPrivate() || ap.Addr().IsLoopback() || ap.Addr().IsLinkLocalUnicast() {
						out.Scope = "lan"
					} else {
						out.Scope = "internet"
					}
				}
				return out, nil
			case p.PeerRelay != "":
				return Path{Kind: "peerRelay", Addr: p.PeerRelay, Relay: p.Relay}, nil
			default:
				return Path{Kind: "derp", Relay: p.Relay}, nil
			}
		}
	}
	return Path{}, ErrNotFound
}

// candidates are where the CLI lives. launchd's PATH reaches none of the
// Homebrew ones, so loupe-supervisor.sh searches the same list.
var candidates = []string{
	"/opt/homebrew/bin/tailscale",
	"/usr/local/bin/tailscale",
	"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
}

var (
	cliOnce sync.Once
	cliPath string
)

func cli() string {
	cliOnce.Do(func() {
		if p, err := exec.LookPath("tailscale"); err == nil {
			cliPath = p
			return
		}
		for _, c := range candidates {
			if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
				cliPath = c
				return
			}
		}
	})
	return cliPath
}

// Lookup reads the path to the peer holding ip. It returns ErrNotFound for an
// address outside Tailscale's ranges without running anything.
func Lookup(ctx context.Context, ip netip.Addr) (Path, error) {
	if !IsTailnet(ip) {
		return Path{}, ErrNotFound
	}
	bin := cli()
	if bin == "" {
		return Path{}, errors.New("tailscale CLI not found")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "status", "--json").Output()
	if err != nil {
		return Path{}, err
	}
	return Parse(out, ip)
}
