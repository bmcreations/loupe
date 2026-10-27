package main

import (
	"crypto/tls"
	"crypto/x509"
	"strings"
	"testing"
)

func TestPhoneURLUsesTheCertificatesName(t *testing.T) {
	cert := &tls.Certificate{Leaf: &x509.Certificate{DNSNames: []string{"mac.example.ts.net"}}}
	ips := []string{"https://localhost:18456", "https://100.1.2.3:18456"}
	cases := []struct {
		name   string
		cert   *tls.Certificate
		useTLS bool
		token  string
		want   string
	}{
		{"cert name", cert, true, "abc", "https://mac.example.ts.net:18456/?token=abc"},
		{"no token", cert, true, "", "https://mac.example.ts.net:18456/"},
		// An IP would fail the certificate check on the phone, so nothing.
		{"cert without a name", &tls.Certificate{Leaf: &x509.Certificate{}}, true, "abc", ""},
		{"cert failed to load", nil, true, "abc", ""},
		{"plain http", nil, false, "abc", "https://100.1.2.3:18456/?token=abc"},
	}
	for _, c := range cases {
		if got := phoneURL(c.cert, c.useTLS, ":18456", c.token, ips); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	if got := phoneURL(nil, false, ":18456", "", ips[:1]); got != "" {
		t.Errorf("localhost only: got %q, want nothing", got)
	}
}

func TestBannerQRFitsTheAgentLogTail(t *testing.T) {
	u := "https://my-mac.tailnet-0000.ts.net:18456/?token=" + strings.Repeat("f", 64)
	code, err := bannerQR(u)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(code, "\n"); lines > 30 {
		t.Errorf("QR is %d lines; make agent-log shows 40 in all", lines)
	}
	if strings.Contains(code, "\x1b") {
		t.Error("QR carries an escape sequence; loupe-run.log is plain text")
	}
}
