package httpapi

import (
	"context"

	"github.com/quic-go/quic-go"
)

// quicConnKey is the context key under which the QUIC connection carrying a
// request is stashed. It is our own key, deliberately: webtransport-go stores
// the same connection under an unexported key of its own, and the only way to
// read that one back is through a *webtransport.Session, which does not expose
// the connection or its statistics.
type quicConnKey struct{}

// WithQUICConn stashes the QUIC connection on a request context.
//
// This is wired as the http3.Server's ConnContext *before*
// webtransport.ConfigureHTTP3Server runs, because that function chains to
// whatever ConnContext it finds rather than replacing it. Setting it afterwards
// would drop webtransport-go's own hook and produce trap 14's fourth row, a
// `missing QUIC connection` from Upgrade with nothing in the server log.
func WithQUICConn(ctx context.Context, conn *quic.Conn) context.Context {
	return context.WithValue(ctx, quicConnKey{}, conn)
}

// QUICConnFrom recovers the connection. The second return distinguishes "no
// connection was stashed" from "a nil one was", which is what lets a test tell
// a ConnContext that ran from one that was replaced.
func QUICConnFrom(ctx context.Context) (*quic.Conn, bool) {
	conn, ok := ctx.Value(quicConnKey{}).(*quic.Conn)
	return conn, ok
}
