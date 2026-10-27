// Package peer holds the servers this one has been told about: their URLs,
// tokens and pinned certificates, the polling that keeps their target rows
// fresh, and the HTTP and TLS plumbing for reaching them. It knows nothing of
// httpapi, so the hub that merges the rows and the target package that
// represents them both import it without a cycle.
package peer

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// Server is one peer: a loupe process this server was told to reach. Token is
// the peer's shared token, so a Server must never reach /api/config, the trace
// or a response body; Info is what leaves the process.
type Server struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Token string `json:"token"`
	// Pin is the hex SHA-256 of the peer's leaf certificate, the fingerprint
	// the person confirmed when the server was added. It is empty when the certificate verified against the
	// system roots, which needs no pin and survives a renewal.
	Pin string `json:"pin,omitempty"`
	// Warning is set by Registry.Add when the add-time QUIC probe could not
	// tell whether the peer serves this server's protocol. It is not stored.
	Warning string `json:"-"`
}

// nameRE keeps a name short and safe to put in front of a target id and in a
// query string: the first "/" in a hub id is what splits server from owner.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

// ValidName reports whether name can prefix target ids.
func ValidName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("server name %q must be 1-32 characters of letters, digits, '.', '_' or '-', and must not contain '/'", name)
	}
	return nil
}

// NormalizeURL reduces raw to scheme and host. Only https is accepted: the
// token travels in a header on every poll, and a pin means nothing over
// plaintext.
func NormalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("server url %q is not a URL", raw)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("server url %q must be https", raw)
	}
	return "https://" + u.Host, nil
}

// DeriveName picks a name from the URL's host: the first label of a hostname,
// the whole of an address, made to fit nameRE.
func DeriveName(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "peer"
	}
	host := u.Hostname()
	if !strings.Contains(host, ":") && !isIP(host) {
		host, _, _ = strings.Cut(host, ".")
	}
	var b strings.Builder
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == '.' || r == ':':
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-_")
	if len(name) > 32 {
		name = name[:32]
	}
	if name == "" {
		return "peer"
	}
	return name
}

func isIP(host string) bool { return net.ParseIP(host) != nil }
