package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/bmcreations/loupe/internal/httpapi"
	"github.com/bmcreations/loupe/internal/session"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

// quicListener is the one UDP port both QUIC transports share: HTTP/3 for
// /wt, and session.QUICALPN for a native client. ALPN picks between them
// during the handshake, so neither needs a port of its own.
//
// It replaces webtransport.Server.ListenAndServe, and so has to set up the
// listener the way that would have: the config cloned with datagrams and
// partial-delivery resets on, since ServeQUICConn refuses a connection without
// either and the refusal reaches no log (trap 14's shape).
type quicListener struct {
	ln *quic.EarlyListener
	// pc is closed separately: a listener built on a caller's PacketConn
	// leaves it open, and a port left bound is trap 12.
	pc net.PacketConn
}

// listenerQUICConfig is the config the shared listener runs: quicConfig plus
// what webtransport-go's own serve loop would have switched on.
func listenerQUICConfig(base *quic.Config) *quic.Config {
	c := &quic.Config{}
	if base != nil {
		c = base.Clone()
	}
	c.EnableDatagrams = true
	c.EnableStreamResetPartialDelivery = true
	return c
}

// listenerTLSConfig offers h3 first, so a browser that offers both is never
// handed the native protocol.
func listenerTLSConfig(base *tls.Config) *tls.Config {
	c := base.Clone()
	c.NextProtos = []string{http3.NextProtoH3, session.QUICALPN}
	return c
}

func listenQUIC(addr string, wt *webtransport.Server) (*quicListener, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	pc, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}
	ln, err := quic.ListenEarly(pc, listenerTLSConfig(wt.H3.TLSConfig), listenerQUICConfig(wt.H3.QUICConfig))
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	return &quicListener{ln: ln, pc: pc}, nil
}

// serve accepts until the listener closes, handing each connection to the
// transport its ALPN names.
func (l *quicListener) serve(wt *webtransport.Server, api *httpapi.Server) {
	for {
		qc, err := l.ln.Accept(context.Background())
		if err != nil {
			return
		}
		switch qc.ConnectionState().TLS.NegotiatedProtocol {
		case session.QUICALPN:
			go api.ServeQUIC(qc)
		default:
			go func() {
				if err := wt.ServeQUICConn(qc); err != nil && !errors.Is(err, http.ErrServerClosed) {
					fmt.Fprintf(os.Stderr, "webtransport: %v\n", err)
				}
			}()
		}
	}
}

// Close stops accepting. Connections already served end with their sessions:
// webtransport.Server.Close for /wt, and the manager's shutdown for the rest.
func (l *quicListener) Close() error {
	err := l.ln.Close()
	if cerr := l.pc.Close(); err == nil {
		err = cerr
	}
	return err
}
