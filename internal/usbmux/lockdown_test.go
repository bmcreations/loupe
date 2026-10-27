package usbmux

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

// testPEM makes a self-signed certificate and its key, as PEM.
func testPEM(t *testing.T) (cert, key []byte) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "host"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kd, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd})
}

// pairRecordXML is a pair record in the shape usbmuxd returns, with the
// <data> wrapped across lines as it is there.
func pairRecordXML(hostID string, cert, key []byte) []byte {
	data := func(b []byte) string {
		s := base64.StdEncoding.EncodeToString(b)
		var w strings.Builder
		for len(s) > 60 {
			w.WriteString("\n\t" + s[:60])
			s = s[60:]
		}
		w.WriteString("\n\t" + s + "\n\t")
		return "<data>" + w.String() + "</data>"
	}
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
	<key>HostCertificate</key>` + data(cert) + `
	<key>HostID</key><string>` + hostID + `</string>
	<key>HostPrivateKey</key>` + data(key) + `
	<key>SystemBUID</key><string>BUID-1</string>
	<key>WiFiMACAddress</key><string>00:00:00:00:00:00</string>
</dict></plist>`)
}

func TestParsePairRecordLoadsTheHostKeyPair(t *testing.T) {
	cert, key := testPEM(t)
	pr, err := parsePairRecord(pairRecordXML("HOST-1", cert, key))
	if err != nil {
		t.Fatal(err)
	}
	if pr.hostID != "HOST-1" || pr.systemBUID != "BUID-1" || len(pr.cert.Certificate) != 1 {
		t.Fatalf("got %q %q %d certs", pr.hostID, pr.systemBUID, len(pr.cert.Certificate))
	}
}

func TestParsePairRecordErrorsNeverCarryTheKey(t *testing.T) {
	cert, _ := testPEM(t)
	_, otherKey := testPEM(t)
	for name, rec := range map[string][]byte{
		"mismatched key": pairRecordXML("HOST-1", cert, otherKey),
		"no host id":     pairRecordXML("", cert, otherKey),
	} {
		_, err := parsePairRecord(rec)
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
		if strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), "BEGIN") {
			t.Fatalf("%s: error carries key material: %v", name, err)
		}
	}
}

func TestLockdownFramingRoundTrips(t *testing.T) {
	var b bytes.Buffer
	if err := lockdownSend(&b, map[string]any{"Request": "StartService", "Port": 49152}); err != nil {
		t.Fatal(err)
	}
	if n := int(b.Bytes()[0])<<24 | int(b.Bytes()[1])<<16 | int(b.Bytes()[2])<<8 | int(b.Bytes()[3]); n != b.Len()-4 {
		t.Fatalf("length prefix %d for a %d-byte body", n, b.Len()-4)
	}
	m, err := lockdownRecv(&b)
	if err != nil {
		t.Fatal(err)
	}
	if m["Request"] != "StartService" || m["Port"] != 49152 {
		t.Fatalf("got %v", m)
	}
}

func TestLockdownErrorReplyIsAnError(t *testing.T) {
	var b bytes.Buffer
	lockdownSend(&b, map[string]any{"Error": "InvalidHostID", "Request": "StartSession"})
	if _, err := lockdownRecv(&b); err == nil || !strings.Contains(err.Error(), "InvalidHostID") {
		t.Fatalf("got %v", err)
	}
}

// A phone that does not ask for TLS on the session still gets
// StartSession and then StartService, in that order, on one connection.
func TestStartServiceAsksForTheServiceAfterTheSession(t *testing.T) {
	host, phone := net.Pipe()
	defer host.Close()
	got := make(chan []map[string]any, 1)
	go func() {
		defer phone.Close()
		var seen []map[string]any
		m, _ := lockdownRecv(phone)
		seen = append(seen, m)
		lockdownSend(phone, map[string]any{"Request": "StartSession", "SessionID": "S"})
		m, _ = lockdownRecv(phone)
		seen = append(seen, m)
		lockdownSend(phone, map[string]any{"Request": "StartService", "Port": 50123})
		got <- seen
	}()
	pr := &pairRecord{hostID: "HOST-1", systemBUID: "BUID-1"}
	port, ssl, err := startService(context.Background(), host, pr, "com.apple.syslog_relay")
	if err != nil {
		t.Fatal(err)
	}
	if port != 50123 || ssl {
		t.Fatalf("port %d ssl %v", port, ssl)
	}
	seen := <-got
	if seen[0]["Request"] != "StartSession" || seen[0]["HostID"] != "HOST-1" || seen[0]["SystemBUID"] != "BUID-1" {
		t.Fatalf("first request %v", seen[0])
	}
	if seen[1]["Request"] != "StartService" || seen[1]["Service"] != "com.apple.syslog_relay" {
		t.Fatalf("second request %v", seen[1])
	}
}

func TestStartServiceWithNoPortIsAnError(t *testing.T) {
	host, phone := net.Pipe()
	defer host.Close()
	go func() {
		defer phone.Close()
		lockdownRecv(phone)
		lockdownSend(phone, map[string]any{"SessionID": "S"})
		lockdownRecv(phone)
		lockdownSend(phone, map[string]any{"Request": "StartService"})
	}()
	_, _, err := startService(context.Background(), host, &pairRecord{hostID: "H", systemBUID: "B"}, "x")
	if err == nil {
		t.Fatal("no error")
	}
}
