package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/quic-go/quic-go"
)

// quicSlots is how many slots the probe keeps open, as the native client
// does.
const quicSlots = 32

// runQUIC drives the raw-QUIC transport the way a native client does: a
// control stream with the open message, and slots opened ahead of need, one
// frame on each. The URL's host, target, token and start are read as /wt
// would read them, so the same link probes either transport.
func runQUIC(rawURL string, dur, connectBudget time.Duration, insecure bool, tap *[2]int32, tapAfter time.Duration, dumpTo string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	q := u.Query()
	open := session.QUICOpen{Target: q.Get("target"), Token: q.Get("token"), Start: q.Get("start") == "1"}

	ctx, cancel := context.WithTimeout(context.Background(), dur+connectBudget)
	defer cancel()
	start := time.Now()

	qc, err := quic.DialAddr(ctx, u.Host,
		&tls.Config{InsecureSkipVerify: insecure, NextProtos: []string{session.QUICALPN}},
		&quic.Config{InitialPacketSize: 1200},
	)
	if err != nil {
		return fmt.Errorf("dial quic after %v: %w", time.Since(start).Round(time.Millisecond), err)
	}
	fmt.Printf("HANDSHAKE %s in %v\n", qc.ConnectionState().TLS.NegotiatedProtocol, time.Since(start).Round(time.Millisecond))

	control, err := qc.OpenStreamSync(ctx)
	if err != nil {
		return fmt.Errorf("open control stream: %w", err)
	}
	b, _ := json.Marshal(open)
	if _, err := control.Write(append([]byte{session.QUICStreamControl}, controlMsg(b...)...)); err != nil {
		return fmt.Errorf("send open: %w", err)
	}
	// The server reads the control stream for input and pings; nothing here
	// needs what it sends back, but it must be drained or its window fills.
	go func() { _, _ = io.Copy(io.Discard, control) }()

	col, err := newCollector(start, dumpTo, func() {
		if tap != nil {
			go sendTap(control, tap, tapAfter)
		}
	})
	if err != nil {
		return err
	}
	defer col.close()

	deadline := time.Now().Add(dur)
	frames := make(chan []byte, quicSlots)
	done := make(chan struct{})
	slotCtx, slotCancel := context.WithDeadline(ctx, deadline)
	defer slotCancel()
	// Each slot goroutine opens a slot, reads its one frame and opens the
	// next, so quicSlots are always waiting on the server.
	for range quicSlots {
		go func() {
			for slotCtx.Err() == nil {
				st, err := qc.OpenStreamSync(slotCtx)
				if err != nil {
					return
				}
				if _, err := st.Write([]byte{session.QUICStreamSlot}); err != nil {
					return
				}
				_ = st.Close()
				b, err := io.ReadAll(st)
				// A reset slot is a frame the server abandoned.
				if err != nil || len(b) < wtFrameHeaderLen {
					continue
				}
				select {
				case frames <- b:
				case <-done:
					return
				}
			}
		}()
	}
	for loop := true; loop; {
		select {
		case b := <-frames:
			col.add(b)
		case <-slotCtx.Done():
			loop = false
		case <-qc.Context().Done():
			loop = false
		}
	}
	close(done)
	ended := context.Cause(qc.Context())
	_ = qc.CloseWithError(0, "probe done")

	col.report(dur)
	var ae *quic.ApplicationError
	if col.frames == 0 && errors.As(ended, &ae) {
		return fmt.Errorf("connection closed with code %d before any frame: %s", ae.ErrorCode, ae.ErrorMessage)
	}
	if col.frames == 0 {
		return fmt.Errorf("connection opened but no frames arrived")
	}
	return nil
}
