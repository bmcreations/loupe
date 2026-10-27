package peer

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
)

// ErrCertChanged is the dial error for a peer whose certificate neither
// verifies against the system roots nor matches the pin taken when it was
// added. It is fixed by removing the server and adding it again.
var ErrCertChanged = errors.New("certChanged")

// Fingerprint is the hex SHA-256 of a certificate's DER, the form Server.Pin
// holds.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// TLSConfig trusts a peer the way an SSH client trusts a host key. A
// certificate that verifies against the system roots is accepted with no pin,
// which is what `tailscale cert` or ACME gives and survives a renewal. Any
// other must match pin. With no pin yet and capture set, the first such
// certificate is accepted and its fingerprint handed to capture: that is the
// trust-on-first-use step, taken only when a server is added.
//
// It checks in VerifyConnection, not VerifyPeerCertificate, because it has to
// verify the chain itself (InsecureSkipVerify is what lets a self-signed
// certificate reach the check) and only the ConnectionState carries the
// ServerName to verify against. Nothing here is specific to net/http, so the
// same config serves QUIC dials to the peer.
func TLSConfig(pin string, capture func(fingerprint string)) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// Verification is VerifyConnection's job below, in full.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("peer presented no certificate")
			}
			leaf := cs.PeerCertificates[0]
			inter := x509.NewCertPool()
			for _, c := range cs.PeerCertificates[1:] {
				inter.AddCert(c)
			}
			_, verr := leaf.Verify(x509.VerifyOptions{DNSName: cs.ServerName, Intermediates: inter})
			if verr == nil {
				return nil
			}
			fp := Fingerprint(leaf)
			switch {
			case pin != "":
				if fp != pin {
					return fmt.Errorf("%w: %s no longer presents the certificate pinned when it was added", ErrCertChanged, cs.ServerName)
				}
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
