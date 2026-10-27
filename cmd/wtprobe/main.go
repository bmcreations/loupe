// Command wtprobe drives a real WebTransport session against a running loupe
// server and reports what it received.
//
// It exists because a browser cannot tell you why a WebTransport session failed:
// Safari reports a bare "WebTransportError" with no stack, and a handshake that
// dies below HTTP leaves nothing in the server trace either, so both ends look
// silent and agree on nothing. This probe is the tiebreaker. Point it at the
// address the phone uses, not at loopback, or it will not exercise the path
// that actually breaks.
//
//	go run ./cmd/wtprobe https://100.64.0.1:18456/wt?target=synthetic
//
// -tap x,y sends one touch, down then up 60ms later, in stream pixels once
// the first frame has arrived, which checks the viewer's input path end to
// end without a browser. It drives the target, so point it only at one
// nobody is using. -tapafter holds the tap back, so it can follow a fold or
// a rotation made mid-session.
//
// -quic probes the raw-QUIC transport a native client uses instead, on the
// same link: the host, target, token and start are read from it as /wt reads
// them.
package main

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
)

// wtFrameHeaderLen is the WebTransport frame prefix: frameID u32, ptsMicros
// u64, flags u8. Shorter than session.FrameHeaderLen because the stream
// boundary already delimits the frame.
const wtFrameHeaderLen = 4 + 8 + 1

func main() {
	dur := flag.Duration("for", 6*time.Second, "how long to collect frames")
	insecure := flag.Bool("insecure", true, "skip certificate verification (dialing by IP)")
	tapAt := flag.String("tap", "", "x,y in stream pixels: send one tap after the first frame")
	tapAfter := flag.Duration("tapafter", 0, "wait this long after the first frame before -tap")
	dumpTo := flag.String("dump", "", "write one line per frame to this file: arrival ms since the first frame, pts us, flags")
	useQUIC := flag.Bool("quic", false, "use the raw-QUIC transport a native client uses (ALPN loupe/1) instead of WebTransport; the URL's host and query are read the same way")
	connectBudget := flag.Duration("connect", 20*time.Second, "how long CONNECT may take; the server answers before it opens the target, so a boot no longer counts here")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: wtprobe [flags] https://host:port/wt?target=ID")
		os.Exit(2)
	}

	var tap *[2]int32
	if *tapAt != "" {
		x, y, ok := strings.Cut(*tapAt, ",")
		xi, xerr := strconv.Atoi(x)
		yi, yerr := strconv.Atoi(y)
		if !ok || xerr != nil || yerr != nil {
			fmt.Fprintln(os.Stderr, "-tap wants x,y")
			os.Exit(2)
		}
		tap = &[2]int32{int32(xi), int32(yi)}
	}

	probe := run
	if *useQUIC {
		probe = runQUIC
	}
	if err := probe(flag.Arg(0), *dur, *connectBudget, *insecure, tap, *tapAfter, *dumpTo); err != nil {
		fmt.Fprintln(os.Stderr, "probe failed:", err)
		os.Exit(1)
	}
}

// controlMsg frames one message the way the server's control stream reads
// it: a u32 big-endian length, then the message.
func controlMsg(b ...byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(b)))
	return append(out, b...)
}

// touchMsg is a MsgTouch: action, pointer id u32, x i32, y i32, pressure u16.
func touchMsg(action uint8, x, y int32) []byte {
	b := []byte{session.MsgTouch, action}
	b = binary.BigEndian.AppendUint32(b, 0)
	b = binary.BigEndian.AppendUint32(b, uint32(x))
	b = binary.BigEndian.AppendUint32(b, uint32(y))
	b = binary.BigEndian.AppendUint16(b, 0xffff)
	return controlMsg(b...)
}

func run(rawURL string, dur, connectBudget time.Duration, insecure bool, tap *[2]int32, tapAfter time.Duration, dumpTo string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), dur+connectBudget)
	defer cancel()

	start := time.Now()

	// v0.13.0 dropped Dialer, so the QUIC connection is dialled here and the
	// WebTransport client is layered on top of it.
	//
	// InitialPacketSize is pinned for the same reason the server pins it: over
	// the tailnet a larger Initial is dropped outright and the handshake dies
	// below HTTP, logging nothing at either end.
	qconn, err := quic.DialAddr(ctx, u.Host,
		&tls.Config{InsecureSkipVerify: insecure, NextProtos: []string{"h3"}},
		&quic.Config{EnableDatagrams: true, InitialPacketSize: 1200, EnableStreamResetPartialDelivery: true},
	)
	if err != nil {
		return fmt.Errorf("dial quic after %v: %w", time.Since(start).Round(time.Millisecond), err)
	}

	// Session flow control is advertised here as well as on the server, so the
	// probe negotiates it the way a browser does. Without it the server sees a
	// peer that does not do flow control and disables it for the session, which
	// is precisely the case that hid the Safari bug.
	tr := &webtransport.Transport{
		Config: &webtransport.Config{
			MaxIncomingStreams:    64,
			MaxIncomingUniStreams: 64,
			MaxIncomingData:       16 << 20,
		},
	}
	cc, err := tr.NewClientConn(qconn)
	if err != nil {
		return fmt.Errorf("new client conn: %w", err)
	}

	rsp, sess, err := cc.Dial(ctx, rawURL, nil)
	if err != nil {
		// A timeout here with no wt-attempt in the server trace means the
		// handshake never reached HTTP. Check packet sizes against the path
		// MTU before looking anywhere above QUIC.
		return fmt.Errorf("dial after %v: %w", time.Since(start).Round(time.Millisecond), err)
	}
	fmt.Printf("CONNECT %d in %v\n", rsp.StatusCode, time.Since(start).Round(time.Millisecond))

	// Trap 8: QUIC streams are lazy, so the server's AcceptStream blocks until
	// the client sends something.
	control, err := sess.OpenStreamSync(ctx)
	if err != nil {
		return fmt.Errorf("open control stream: %w", err)
	}
	// Framed like every other control message. A bare byte was read as the
	// start of a length prefix, which went unnoticed only because nothing
	// was ever sent after it.
	if _, err := control.Write(controlMsg(session.MsgHello)); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	deadline := time.Now().Add(dur)
	col, err := newCollector(start, dumpTo, func() {
		if tap != nil {
			go sendTap(control, tap, tapAfter)
		}
	})
	if err != nil {
		return err
	}
	defer col.close()
	// The server accepts the session before it opens the target, so a target
	// it cannot open ends the session rather than refusing CONNECT, and the
	// reason is in the close.
	var ended error
	for {
		sctx, scancel := context.WithDeadline(context.Background(), deadline)
		st, err := sess.AcceptUniStream(sctx)
		scancel()
		if err != nil {
			ended = err
			break
		}
		b, err := io.ReadAll(st)
		if err != nil || len(b) < wtFrameHeaderLen {
			continue
		}
		col.add(b)
	}
	_ = sess.CloseWithError(0, "probe done")

	col.report(dur)
	var se *webtransport.SessionError
	if col.frames == 0 && errors.As(ended, &se) {
		return fmt.Errorf("session closed with code %d before any frame: %s", se.ErrorCode, se.Message)
	}
	if col.frames == 0 {
		return fmt.Errorf("session opened but no frames arrived")
	}
	return nil
}

// describe prints the distribution of a set of millisecond gaps. A 60fps
// source should sit at 16.7ms across the board; a bursty one has a p50 near
// zero and a long tail, which is the shape that collapses in a renderer
// painting at most one frame per tick.
func describe(label string, v []float64) {
	if len(v) == 0 {
		return
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	at := func(f float64) float64 { return s[min(int(float64(len(s))*f), len(s)-1)] }
	var sum float64
	for _, x := range s {
		sum += x
	}
	// Frames closer together than half a 60Hz tick cannot be shown separately.
	var clumped int
	for _, x := range s {
		if x < 8.3 {
			clumped++
		}
	}
	fmt.Printf("%s n=%d mean %.1fms | p10 %.1f p50 %.1f p90 %.1f p99 %.1f max %.1f | under 8.3ms: %d (%.0f%%)\n",
		label, len(s), sum/float64(len(s)), at(.10), at(.50), at(.90), at(.99), s[len(s)-1],
		clumped, 100*float64(clumped)/float64(len(s)))
}
