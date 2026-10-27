package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"

	"github.com/bmcreations/loupe/internal/httpapi"
	"github.com/bmcreations/loupe/internal/session"
)

// A WebTransport handshake dies silently when the server's Initial packets do
// not fit the path. The client's own Initial is padded to the 1200-byte spec
// floor and arrives, so the session looks like it is opening, and the failure
// is a timeout with nothing logged above QUIC: no wt-attempt, no wt-error.
// Reproduced by dialing the tailnet address, where no datagram over 1231 bytes
// arrived while the server padded to 1280.
func TestQUICInitialPacketFitsTheTunnel(t *testing.T) {
	// The largest IP header the server may be reached behind, IPv6 at 40 bytes,
	// plus the 8-byte UDP header.
	const overhead = 40 + 8

	got := quicConfig(nil).InitialPacketSize
	if got == 0 {
		t.Fatal("InitialPacketSize unset; quic-go defaults to 1280, which does not fit a 1280-byte tunnel")
	}
	if onWire := int(got) + overhead; onWire > tunnelMTU {
		t.Errorf("Initial packet is %d bytes on the wire, over the %d-byte tunnel MTU", onWire, tunnelMTU)
	}
}

// Safari completes the QUIC handshake, reads SETTINGS, and closes with
// H3_NO_ERROR before sending its CONNECT when it finds no WebTransport setting
// it recognises. webtransport-go advertises only draft-02's key, so the
// draft-07 one has to be added here. Both must go out: webtransport-go's own
// client, which cmd/wtprobe uses, still checks for draft-02.
func TestWebTransportAdvertisesTheCurrentDraftSetting(t *testing.T) {
	s := webTransportSettings()

	n, ok := s[settingsWebTransportMaxSessions]
	if !ok {
		t.Fatalf("SETTINGS_WEBTRANSPORT_MAX_SESSIONS (0x%x) not advertised; browsers will close with H3_NO_ERROR",
			uint64(settingsWebTransportMaxSessions))
	}
	if n == 0 {
		t.Error("advertised max sessions is 0, which tells the client not to open any")
	}

	// ConfigureHTTP3Server writes draft-02's key into this same map. If it
	// ever stops, cmd/wtprobe breaks while browsers keep working, and the
	// probe is the thing used to tell the two apart.
	const draft02 = 0x2b603742
	if _, collides := s[draft02]; collides {
		t.Error("draft-02 key set here as well; leave it to webtransport-go so the two cannot disagree")
	}
}

// Every field newWebTransportServer sets fails silently when it is missing: the
// handshake completes, SETTINGS look plausible, and the browser hangs up with
// no request ever reaching the handler. Each assertion below stands for one
// deploy spent finding that out.
func TestWebTransportServerIsFullyConfigured(t *testing.T) {
	srv := newWebTransportServer(":0", selfSignedCert(t), nil)
	h3 := srv.H3

	if !slices.Contains(h3.TLSConfig.NextProtos, http3.NextProtoH3) {
		t.Errorf("ALPN %q missing from NextProtos %q; the server selects no protocol and clients abort with CRYPTO_ERROR 0x178",
			http3.NextProtoH3, h3.TLSConfig.NextProtos)
	}

	// The three things ConfigureHTTP3Server is responsible for.
	const draft02 = 0x2b603742
	if h3.AdditionalSettings[draft02] != 1 {
		t.Error("SETTINGS_ENABLE_WEBTRANSPORT not advertised; webtransport-go clients refuse with \"server didn't enable WebTransport\"")
	}
	if !h3.EnableDatagrams {
		t.Error("HTTP/3 datagrams disabled; clients refuse with \"server didn't enable HTTP/3 datagram support\"")
	}
	if h3.ConnContext == nil {
		t.Error("no ConnContext; Upgrade cannot recover the QUIC connection and every CONNECT fails with \"missing QUIC connection\"")
	}

	// Both ConnContext hooks have to survive, and only the ordering in
	// newWebTransportServer achieves that: ConfigureHTTP3Server chains to
	// whatever it finds, so ours must be set first.
	//
	// Each direction fails silently and differently. Lose webtransport-go's and
	// every CONNECT dies in Upgrade with "missing QUIC connection", trap 14's
	// fourth row. Lose ours and nothing fails at all -- PathRTT just returns
	// zero forever, the pacer quietly falls back to the phone's reports, and
	// the only evidence is `rttSource` reading "client" on a QUIC transport.
	if _, ok := httpapi.QUICConnFrom(h3.ConnContext(context.Background(), nil)); !ok {
		t.Error("ConnContext does not stash the QUIC connection for the pacer; PathRTT will return zero forever and rttSource will read \"client\" on a QUIC transport")
	}
	// ConfigureHTTP3Server chains to an existing ConnContext, so ours has to be
	// in place before it runs. Set it afterwards and it replaces webtransport-go's
	// hook instead of wrapping it, and every CONNECT dies in Upgrade with
	// "missing QUIC connection" -- trap 14's fourth row, which logs nothing.
	// The assertion above cannot see that, because ours works either way.
	if reflect.ValueOf(h3.ConnContext).Pointer() == reflect.ValueOf(httpapi.WithQUICConn).Pointer() {
		t.Error("ConnContext is ours unwrapped; it was set after ConfigureHTTP3Server and dropped webtransport-go's own hook")
	}

	// The draft-07 setting has to survive ConfigureHTTP3Server, which writes
	// into this same map. v0.13.0 advertises draft-15's 0x14e9cd29 under its
	// own name for max sessions, so this one is ours to keep setting.
	if h3.AdditionalSettings[settingsWebTransportMaxSessions] == 0 {
		t.Error("SETTINGS_WEBTRANSPORT_MAX_SESSIONS lost; Safari closes with H3_NO_ERROR before sending CONNECT")
	}

	// Session flow control is opt-in. Leave Config nil and webtransport-go
	// advertises no session limits at all, which Safari reads as a limit of
	// zero: createBidirectionalStream and createUnidirectionalStream return
	// promises that never settle, in either direction. Streams the server
	// opens keep working throughout, because those spend Safari's credit
	// rather than ours, so the session looks healthy from both ends.
	if srv.Config == nil {
		t.Fatal("no session flow control; every stream the client opens blocks forever on credit that is never granted")
	}
	for _, lim := range []struct {
		name string
		got  int64
	}{
		{"MaxIncomingStreams", srv.Config.MaxIncomingStreams},
		{"MaxIncomingUniStreams", srv.Config.MaxIncomingUniStreams},
		{"MaxIncomingData", srv.Config.MaxIncomingData},
	} {
		// Zero means "do not send the setting", which is the bug. Negative
		// means "send a limit of zero", which is the same stall stated out
		// loud.
		if lim.got <= 0 {
			t.Errorf("%s is %d; the setting is either omitted or sent as zero, and the client stalls on its first stream",
				lim.name, lim.got)
		}
	}
}

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "loupe-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// The shared listener replaces webtransport.Server.ListenAndServe, so it has
// to switch on what that would have. ServeQUICConn refuses a connection that
// lacks datagrams or partial-delivery resets, and the refusal reaches no log:
// /wt fails the way trap 14 describes. And h3 must come first in the ALPN
// list, or a client offering both is handed the native protocol.
func TestSharedQUICListenerKeepsWebTransportWorking(t *testing.T) {
	srv := newWebTransportServer(":0", selfSignedCert(t), nil)

	q := listenerQUICConfig(srv.H3.QUICConfig)
	if !q.EnableDatagrams {
		t.Error("datagrams off; ServeQUICConn refuses every /wt connection")
	}
	if !q.EnableStreamResetPartialDelivery {
		t.Error("partial-delivery resets off; ServeQUICConn refuses every /wt connection")
	}
	if srv.H3.QUICConfig != nil && q == srv.H3.QUICConfig {
		t.Error("listener config aliases the HTTP/3 server's; it must be a clone")
	}

	tc := listenerTLSConfig(srv.H3.TLSConfig)
	want := []string{http3.NextProtoH3, session.QUICALPN}
	if !slices.Equal(tc.NextProtos, want) {
		t.Errorf("NextProtos %q, want %q", tc.NextProtos, want)
	}
	if slices.Contains(srv.H3.TLSConfig.NextProtos, session.QUICALPN) {
		t.Error("listenerTLSConfig changed the HTTP/3 server's own config")
	}
}
