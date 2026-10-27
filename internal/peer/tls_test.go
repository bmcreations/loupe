package peer

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/quic-go/quic-go"
)

// testCA stands in for a public CA: it is added to verifyRoots for the
// test, and leaf issues certificates it signed.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "loupe test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	old := verifyRoots
	verifyRoots = pool
	t.Cleanup(func() { verifyRoots = old })
	return &testCA{cert: cert, key: key}
}

// leaf issues a server certificate for dnsName and ips.
func (ca *testCA) leaf(t *testing.T, dnsName string, ips ...net.IP) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: dnsName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  ips,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if dnsName != "" {
		tmpl.DNSNames = []string{dnsName}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der, ca.cert.Raw}, PrivateKey: key}
}

// quicOwner accepts QUIC connections with cert on a free 127.0.0.1 port and
// returns its URL.
func quicOwner(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	ln, err := quic.ListenAddr("127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{session.QUICALPN}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			go func() { <-c.Context().Done() }()
		}
	}()
	return "https://" + ln.Addr().String()
}

func dialBoth(t *testing.T, srv Server, cert tls.Certificate) (httpErr, quicErr error) {
	t.Helper()
	ts := owner(t, "", cert, `[]`)
	hs := srv
	hs.URL = ts.URL
	c := NewClient(hs)
	defer c.Close()
	_, httpErr = c.Targets(context.Background())
	qs := srv
	qs.URL = quicOwner(t, cert)
	qc, quicErr := quicDial(context.Background(), qs)
	if qc != nil {
		qc.CloseWithError(0, "")
	}
	return httpErr, quicErr
}

func TestAnIPPeerMustPresentACertificateForItsAddress(t *testing.T) {
	ca := newTestCA(t)
	// The control: a CA-signed certificate for 127.0.0.1 is accepted with
	// no pin on both transports.
	httpErr, quicErr := dialBoth(t, Server{Name: "x", Token: "tok"}, ca.leaf(t, "", net.ParseIP("127.0.0.1")))
	if httpErr != nil || quicErr != nil {
		t.Fatalf("a certificate for the dialled IP: http %v, quic %v", httpErr, quicErr)
	}
	// The same CA's certificate for another name is not this peer's.
	httpErr, quicErr = dialBoth(t, Server{Name: "x", Token: "tok"}, ca.leaf(t, "other.example"))
	if !errors.Is(httpErr, ErrServerDown) {
		t.Errorf("http accepted a certificate for another name: %v", httpErr)
	}
	if !errors.Is(quicErr, ErrServerDown) {
		t.Errorf("quic accepted a certificate for another name: %v", quicErr)
	}
}

func TestAPinHoldsEvenWhenTheChainVerifies(t *testing.T) {
	ca := newTestCA(t)
	pinned, _ := x509.ParseCertificate(selfSigned(t).Certificate[0])
	srv := Server{Name: "x", Token: "tok", Pin: Fingerprint(pinned)}
	httpErr, quicErr := dialBoth(t, srv, ca.leaf(t, "", net.ParseIP("127.0.0.1")))
	if !errors.Is(httpErr, ErrCertChanged) {
		t.Errorf("http: %v, want certChanged", httpErr)
	}
	if !errors.Is(quicErr, ErrCertChanged) {
		t.Errorf("quic: %v, want certChanged", quicErr)
	}
}

func TestAPeerWithNoHostIsRefused(t *testing.T) {
	ca := newTestCA(t)
	leaf, _ := x509.ParseCertificate(ca.leaf(t, "", net.ParseIP("127.0.0.1")).Certificate[0])
	cs := tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	if err := TLSConfig("", "", nil).VerifyConnection(cs); err == nil {
		t.Fatal("a CA-valid certificate passed with no host to verify it against")
	}
}

// countingOwner is owner, counting every request that reaches it.
func countingOwner(t *testing.T, cert tls.Certificate) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts, &n
}

func TestAddSendsNoTokenUntilTheFingerprintIsConfirmed(t *testing.T) {
	cert := selfSigned(t)
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	ts, requests := countingOwner(t, cert)
	reg, _ := NewRegistry(NewStore(""), nil)
	ctx := context.Background()

	_, err := reg.Add(ctx, Server{Name: "mini", URL: ts.URL, Token: "tok"})
	var confirm *ConfirmFingerprintError
	if !errors.As(err, &confirm) || confirm.Fingerprint != Fingerprint(leaf) || confirm.Changed {
		t.Fatalf("unconfirmed add = %v, want a request to confirm %s", err, Fingerprint(leaf))
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("%d requests reached the peer before its certificate was confirmed", n)
	}

	wrong, _ := x509.ParseCertificate(selfSigned(t).Certificate[0])
	_, err = reg.Add(ctx, Server{Name: "mini", URL: ts.URL, Token: "tok", Pin: Fingerprint(wrong)})
	if !errors.As(err, &confirm) || !confirm.Changed || confirm.Fingerprint != Fingerprint(leaf) {
		t.Fatalf("add with the wrong fingerprint = %v, want refused", err)
	}
	if n := requests.Load(); n != 0 || len(reg.Names()) != 0 {
		t.Fatalf("wrong fingerprint: %d requests, list %v", n, reg.Names())
	}

	got, err := reg.Add(ctx, Server{Name: "mini", URL: ts.URL, Token: "tok", Pin: strings.ToUpper(Fingerprint(leaf))})
	if err != nil {
		t.Fatal(err)
	}
	if got.Pin != Fingerprint(leaf) || requests.Load() == 0 {
		t.Fatalf("confirmed add: pin %q, %d requests", got.Pin, requests.Load())
	}
}

// The probe and the token-bearing dial are two handshakes. A peer that shows
// the confirmed certificate to the first and its own to the second must get
// no request, because the second dial requires the confirmed pin.
func TestAddRefusesACertificateSwappedAfterTheProbe(t *testing.T) {
	a, b := selfSigned(t), selfSigned(t)
	leafA, _ := x509.ParseCertificate(a.Certificate[0])
	var handshakes, requests atomic.Int32
	// tls.Listen rather than httptest: httptest fills Certificates, which wins
	// over GetCertificate when the client sends no SNI, as an IP dial doesn't.
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			if handshakes.Add(1) == 1 {
				return &a, nil
			}
			return &b, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`[]`))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	reg, _ := NewRegistry(NewStore(""), nil)
	_, err = reg.Add(context.Background(), Server{Name: "mini", URL: "https://" + ln.Addr().String(), Token: "tok", Pin: Fingerprint(leafA)})
	if !errors.Is(err, ErrCertChanged) {
		t.Fatalf("add = %v, want ErrCertChanged", err)
	}
	if n := requests.Load(); n != 0 || len(reg.Names()) != 0 {
		t.Fatalf("%d requests reached the swapped certificate, list %v", n, reg.Names())
	}
	if n := handshakes.Load(); n < 2 {
		t.Fatalf("%d handshakes; the test needs the probe and the dial", n)
	}
}

func TestAddTakesAVerifyingCertificateWithNoPin(t *testing.T) {
	ca := newTestCA(t)
	ts, _ := countingOwner(t, ca.leaf(t, "", net.ParseIP("127.0.0.1")))
	reg, _ := NewRegistry(NewStore(""), nil)
	got, err := reg.Add(context.Background(), Server{Name: "mini", URL: ts.URL, Token: "tok"})
	if err != nil || got.Pin != "" {
		t.Fatalf("add = %+v %v, want added with no pin", got, err)
	}
}

func TestAddRefusesAFingerprintThatIsNotOne(t *testing.T) {
	ts, requests := countingOwner(t, selfSigned(t))
	reg, _ := NewRegistry(NewStore(""), nil)
	var invalid *InvalidError
	if _, err := reg.Add(context.Background(), Server{Name: "mini", URL: ts.URL, Token: "tok", Pin: "abc"}); !errors.As(err, &invalid) || requests.Load() != 0 {
		t.Fatalf("add = %v, %d requests", err, requests.Load())
	}
}
