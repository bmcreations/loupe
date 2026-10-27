package peer

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ErrCertChanged is the dial error for a pinned peer whose certificate does
// not match the pin taken when it was added, whether or not its chain
// verifies. It is fixed by removing the server and adding it again.
var ErrCertChanged = errors.New("certChanged")

// Fingerprint is the hex SHA-256 of a certificate's DER, the form Server.Pin
// holds.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// verifyRoots is the pool a peer's chain verifies against, nil for the
// system roots. It is a var so a test can stand up its own CA.
var verifyRoots *x509.CertPool

// TLSConfig trusts a peer the way an SSH client trusts a host key. With a pin,
// the certificate must match it, whatever its chain. With none, a certificate
// that verifies against the system roots for host is accepted, which is what
// `tailscale cert` or ACME gives and survives a renewal. With no pin and
// capture set, a certificate that does not verify is accepted and its
// fingerprint handed to capture: Add's probe uses that to learn what to ask
// the person to confirm, on a dial that carries no token.
//
// host is the name or IP the peer was dialled at, and the chain is verified
// against it, not against the ConnectionState's ServerName: that is empty for
// an IP peer on both net/http and quic-go, and an empty name skips the
// hostname check, so any CA-signed certificate for any name would pass. An
// empty host fails closed. It checks in VerifyConnection because it has to
// verify the chain itself (InsecureSkipVerify is what lets a self-signed
// certificate reach the check). Nothing here is specific to net/http, so the
// same config serves QUIC dials to the peer.
func TLSConfig(host, pin string, capture func(fingerprint string)) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: host,
		// Verification is VerifyConnection's job below, in full.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if host == "" {
				return errors.New("no peer host to verify the certificate against")
			}
			if len(cs.PeerCertificates) == 0 {
				return errors.New("peer presented no certificate")
			}
			leaf := cs.PeerCertificates[0]
			fp := Fingerprint(leaf)
			if pin != "" {
				if fp != pin {
					return fmt.Errorf("%w: %s no longer presents the certificate pinned when it was added", ErrCertChanged, host)
				}
				return nil
			}
			inter := x509.NewCertPool()
			for _, c := range cs.PeerCertificates[1:] {
				inter.AddCert(c)
			}
			_, verr := leaf.Verify(x509.VerifyOptions{DNSName: host, Intermediates: inter, Roots: verifyRoots})
			switch {
			case verr == nil:
				return nil
			case capture != nil:
				capture(fp)
				return nil
			default:
				return verr
			}
		},
	}
}

// peerHost is the host a peer URL dials, the name its certificate must carry:
// a DNS name or an IP, without brackets or port.
func peerHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// probeCert completes a TLS handshake with the peer at rawURL and hangs up.
// It reports the leaf's fingerprint when the chain does not verify for the
// host, and "" when it does. Nothing is written to the connection, so no
// token reaches a peer whose certificate nobody has confirmed yet.
func probeCert(ctx context.Context, rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = "443"
	}
	var unverified string
	d := tls.Dialer{Config: TLSConfig(host, "", func(fp string) { unverified = fp })}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrServerDown, err)
	}
	conn.Close()
	return unverified, nil
}

// normalFingerprint is fp as Fingerprint renders it, from either that form or
// openssl's colon-separated uppercase one, or "" when it is not a SHA-256.
func normalFingerprint(fp string) string {
	fp = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(fp), ":", ""))
	if b, err := hex.DecodeString(fp); err != nil || len(b) != sha256.Size {
		return ""
	}
	return fp
}
