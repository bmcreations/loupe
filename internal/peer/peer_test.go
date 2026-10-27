package peer

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// selfSigned returns a certificate for 127.0.0.1 that no system root signed.
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "loupe-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// owner serves rows on /api/targets behind a bearer check, on addr (or a
// free port when empty) with cert.
func owner(t *testing.T, addr string, cert tls.Certificate, rows string) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(rows))
	}))
	if addr != "" {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		ts.Listener.Close()
		ts.Listener = l
	}
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

func TestAddPinsASelfSignedCertAndPersistsItBesideTheToken(t *testing.T) {
	cert := selfSigned(t)
	ts := owner(t, "", cert, `[{"id":"a"}]`)
	path := filepath.Join(t.TempDir(), "servers.json")
	reg, err := NewRegistry(NewStore(path), nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])

	got, err := reg.Add(context.Background(), Server{Name: "mini", URL: ts.URL, Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Pin != Fingerprint(leaf) {
		t.Fatalf("pin = %q, want %q", got.Pin, Fingerprint(leaf))
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("servers.json: %v mode %v", err, st.Mode())
	}
	again, _ := NewRegistry(NewStore(path), nil)
	if names := again.Names(); len(names) != 1 || names[0] != "mini" {
		t.Fatalf("reloaded names = %v", names)
	}
	if again.peers["mini"].srv.Pin != got.Pin || again.peers["mini"].srv.Token != "tok" {
		t.Fatalf("reloaded server lost its pin or token: %+v", again.peers["mini"].srv)
	}
}

func TestAChangedCertificateFailsAsCertChanged(t *testing.T) {
	cert := selfSigned(t)
	ts := owner(t, "", cert, `[]`)
	reg, _ := NewRegistry(NewStore(""), nil)
	added, err := reg.Add(context.Background(), Server{Name: "mini", URL: ts.URL, Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	addr := ts.Listener.Addr().String()
	ts.Close()
	owner(t, addr, selfSigned(t), `[]`)

	c := NewClient(added, nil)
	defer c.Close()
	if _, err := c.Targets(context.Background()); !errors.Is(err, ErrCertChanged) {
		t.Fatalf("err = %v, want certChanged", err)
	}
	// A server added fresh at the same address takes the new certificate.
	if _, err := reg.Add(context.Background(), Server{Name: "mini2", URL: ts.URL, Token: "tok"}); err != nil {
		t.Fatalf("re-adding: %v", err)
	}
}

func TestAnUnpinnedUnverifiedCertIsRefusedOutsideAdd(t *testing.T) {
	ts := owner(t, "", selfSigned(t), `[]`)
	c := NewClient(Server{Name: "x", URL: ts.URL, Token: "tok"}, nil)
	defer c.Close()
	if _, err := c.Targets(context.Background()); !errors.Is(err, ErrServerDown) {
		t.Fatalf("err = %v, want a failed dial", err)
	}
}

func TestAddRefusesWhatWouldNotWork(t *testing.T) {
	ts := owner(t, "", selfSigned(t), `[]`)
	reg, _ := NewRegistry(NewStore(""), nil)
	ctx := context.Background()

	var invalid *InvalidError
	for _, s := range []Server{
		{Name: "a/b", URL: ts.URL, Token: "tok"},
		{Name: "mini", URL: "http://" + ts.Listener.Addr().String(), Token: "tok"},
		{Name: "mini", URL: ts.URL},
	} {
		if _, err := reg.Add(ctx, s); !errors.As(err, &invalid) {
			t.Errorf("Add(%+v) = %v, want InvalidError", s, err)
		}
	}
	var se *StatusError
	if _, err := reg.Add(ctx, Server{Name: "mini", URL: ts.URL, Token: "wrong"}); !errors.As(err, &se) || se.Status != 401 {
		t.Errorf("wrong token: %v", err)
	}
	if _, err := reg.Add(ctx, Server{Name: "mini", URL: "https://127.0.0.1:1", Token: "tok"}); !errors.Is(err, ErrServerDown) {
		t.Errorf("unreachable: %v", err)
	}
	if n := reg.Names(); len(n) != 0 {
		t.Fatalf("failed adds left %v in the list", n)
	}
	if _, err := reg.Add(ctx, Server{Name: "mini", URL: ts.URL, Token: "tok"}); err != nil {
		t.Fatal(err)
	}
	var taken *NameTakenError
	if _, err := reg.Add(ctx, Server{Name: "mini", URL: ts.URL, Token: "tok"}); !errors.As(err, &taken) {
		t.Errorf("duplicate name: %v", err)
	}
}

func TestADerivedNameIsUniqueAndValid(t *testing.T) {
	for url, want := range map[string]string{
		"https://mac-mini.local:18456": "mac-mini",
		"https://127.0.0.1:18456":      "127-0-0-1",
		"https://[::1]:1":              "1",
	} {
		if got := DeriveName(url); got != want || ValidName(got) != nil {
			t.Errorf("DeriveName(%q) = %q, want %q", url, got, want)
		}
	}
	ts := owner(t, "", selfSigned(t), `[]`)
	reg, _ := NewRegistry(NewStore(""), nil)
	a, err := reg.Add(context.Background(), Server{URL: ts.URL, Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := reg.Add(context.Background(), Server{URL: ts.URL, Token: "tok"})
	if err != nil || a.Name == b.Name {
		t.Fatalf("names %q %q err %v", a.Name, b.Name, err)
	}
}

func TestASilentPeerKeepsItsRowsThenGoesStaleThenIsDropped(t *testing.T) {
	defer SetTimings(20*time.Millisecond, 150*time.Millisecond, 500*time.Millisecond)()
	cert := selfSigned(t)
	ts := owner(t, "", cert, `[{"id":"a"}]`)
	reg, _ := NewRegistry(NewStore(""), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg.Start(ctx)
	if _, err := reg.Add(ctx, Server{Name: "mini", URL: ts.URL, Token: "tok"}); err != nil {
		t.Fatal(err)
	}
	addr := ts.Listener.Addr().String()
	ts.Close()

	wait := func(what string, ok func() bool) {
		t.Helper()
		for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
			if ok() {
				return
			}
		}
		t.Fatalf("never: %s", what)
	}
	rows := func() (Rows, bool) {
		for _, r := range reg.Rows() {
			return r, true
		}
		return Rows{}, false
	}
	wait("state down", func() bool { return reg.List()[0].State == "down" })
	if r, ok := rows(); !ok || r.Stale {
		t.Fatalf("rows right after the owner stopped = %+v %v, want kept and fresh", r, ok)
	}
	wait("stale", func() bool { r, ok := rows(); return ok && r.Stale })
	wait("dropped", func() bool { _, ok := rows(); return !ok })

	owner(t, addr, cert, `[{"id":"a"},{"id":"b"}]`)
	wait("back up with both rows", func() bool { r, ok := rows(); return ok && !r.Stale && len(r.Rows) == 2 })
	var row struct{ ID string }
	_ = json.Unmarshal(reg.Rows()[0].Rows[0], &row)
	if row.ID != "a" || reg.List()[0].State != "up" {
		t.Fatalf("after reconnect: %+v %+v", row, reg.List())
	}
}
