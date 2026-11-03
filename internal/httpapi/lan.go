package httpapi

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"net"
	"net/http"
	"sort"
	"strconv"
)

// LANResponse is GET /api/lan's body: where a client on the same network can
// reach this server without going through a tunnel, and the key it must find
// there.
type LANResponse struct {
	// URLs are https://<private IPv4>:<port>, one per interface. Empty when
	// the server listens on loopback only or has no private address.
	URLs []string `json:"urls"`
	// SPKI is the lowercase hex SHA-256 of the certificate's
	// SubjectPublicKeyInfo. The certificate names the tunnel host, not these
	// addresses, so a client checks the key instead of the name. Read over a
	// connection that verified, it vouches for the same server at a LAN
	// address.
	SPKI string `json:"spki,omitempty"`
}

// lanListen is what /api/lan builds on (SetLAN).
type lanListen struct {
	port int
	spki string
}

// SetLAN turns on /api/lan for a server listening on addr with cert. Only a
// TLS server listening on every interface is reachable at a LAN address,
// so anything else leaves the endpoint answering with no URLs.
func (s *Server) SetLAN(addr string, cert *tls.Certificate) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil || cert == nil || len(cert.Certificate) == 0 {
		return
	}
	if host != "" && host != "0.0.0.0" && host != "::" {
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return
	}
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	s.lan = &lanListen{port: port, spki: hex.EncodeToString(sum[:])}
}

// handleLAN lists the LAN addresses now, not at startup: a laptop moves
// between networks while the server keeps running.
func (s *Server) handleLAN(w http.ResponseWriter, r *http.Request) {
	resp := LANResponse{URLs: []string{}}
	if s.lan != nil {
		resp.SPKI = s.lan.spki
		for _, ip := range lanIPs(interfaceAddrs()) {
			resp.URLs = append(resp.URLs, "https://"+net.JoinHostPort(ip, strconv.Itoa(s.lan.port)))
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// interfaceAddrs is the addresses of every interface that is up. A var so a
// test can supply its own.
var interfaceAddrs = func() []net.Addr {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.Addr
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		out = append(out, addrs...)
	}
	return out
}

// lanIPs keeps the private IPv4 addresses (10/8, 172.16/12, 192.168/16),
// sorted and without repeats. Loopback, link-local and Tailscale's 100.64/10
// are left out: a client already reaches the tailnet address, and the point
// is a path that does not depend on the tunnel. IPv6 is left out too, since
// Tailscale's own addresses are ULAs that look like any other.
func lanIPs(addrs []net.Addr) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		ip4 := ip.To4()
		if ip4 == nil || !ip4.IsPrivate() {
			continue
		}
		s := ip4.String()
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
