package emugrpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const injectAudioPath = "/android.emulation.control.EmulatorController/injectAudio"

// ErrConnection wraps a failure below gRPC: the dial, or the connection or
// HTTP/2 stream going away before the emulator sent a status.
var ErrConnection = errors.New("emulator gRPC connection failed")

// errClosed is what a Send sees after Close.
var errClosed = errors.New("emugrpc: stream closed")

const (
	// dialTimeout bounds connecting to 127.0.0.1.
	dialTimeout = 2 * time.Second
	// statusWait is how long a failed write waits for the status that
	// explains it. A rejection usually ends the call first and fails the
	// write second, and the status says which rejection it was.
	statusWait = 500 * time.Millisecond
)

// AudioStream is one injectAudio call on its own connection. Send and
// CloseAndRecv belong to one goroutine; Close can be called from any, and
// more than once.
type AudioStream struct {
	cc     *http.ClientConn
	pw     *io.PipeWriter
	cancel context.CancelFunc

	done chan struct{}
	// err is why the call ended, set before done closes: a *Status (OK
	// included) or an error wrapping ErrConnection.
	err error

	closed    atomic.Bool
	closeOnce sync.Once
}

// InjectAudio opens an injectAudio call to ep. The call lasts until
// CloseAndRecv, Close, ctx ending, or the emulator ending it. It returns once
// the request is on its way; the emulator answers nothing until the call
// ends, so a rejection shows up later, from Send, Done or CloseAndRecv.
func InjectAudio(ctx context.Context, ep Endpoint) (*AudioStream, error) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(ep.Port))
	// The emulator serves HTTP/2 without TLS (h2c with prior knowledge).
	// One connection per call, outside any pool, so Close tears down exactly
	// this call and nothing idle keeps the emulator's microphone.
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	tr := &http.Transport{
		Protocols:   &protocols,
		DialContext: (&net.Dialer{Timeout: dialTimeout}).DialContext,
	}
	cc, err := tr.NewClientConn(ctx, "http", addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConnection, err)
	}
	callCtx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, "http://"+addr+injectAudioPath, pr)
	if err != nil {
		cancel()
		cc.Close()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("Te", "trailers")
	if ep.Token != "" {
		req.Header.Set("Authorization", "Bearer "+ep.Token)
	}
	s := &AudioStream{cc: cc, pw: pw, cancel: cancel, done: make(chan struct{})}
	go s.run(req)
	return s, nil
}

// run waits out the call and records how it ended.
func (s *AudioStream) run(req *http.Request) {
	defer close(s.done)
	resp, err := s.cc.RoundTrip(req)
	if err != nil {
		s.err = fmt.Errorf("%w: %v", ErrConnection, err)
		return
	}
	defer resp.Body.Close()
	// Trailers-Only: a call refused outright carries its status in the
	// headers, with no body and no trailers.
	if st, ok := statusFrom(resp.Header); ok {
		s.err = st
		return
	}
	if resp.StatusCode != http.StatusOK {
		s.err = httpStatus(resp.StatusCode)
		return
	}
	// injectAudio answers google.protobuf.Empty, one zero-length message.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		s.err = fmt.Errorf("%w: %v", ErrConnection, err)
		return
	}
	if _, err := splitFrames(body); err != nil {
		s.err = &Status{Code: Internal, Message: err.Error()}
		return
	}
	if st, ok := statusFrom(resp.Trailer); ok {
		s.err = st
		return
	}
	s.err = fmt.Errorf("%w: the call ended with no grpc-status", ErrConnection)
}

// Send writes one packet. The emulator keeps only the first packet's
// Format. Once the call has ended, or a write fails, Send returns why: the
// call's status when the emulator sent one (OK too, for a call the emulator
// ended early), else an error wrapping ErrConnection.
func (s *AudioStream) Send(p AudioPacket) error {
	if s.closed.Load() {
		return errClosed
	}
	// HTTP/2 still takes request data for a moment after the server has
	// ended the call, so a write alone would not notice a refusal.
	select {
	case <-s.done:
		return s.err
	default:
	}
	msg := p.Marshal()
	if _, err := s.pw.Write(appendFrame(make([]byte, 0, frameHeader+len(msg)), msg)); err != nil {
		return s.endedBy(err)
	}
	return nil
}

func (s *AudioStream) endedBy(writeErr error) error {
	if errors.Is(writeErr, errClosed) {
		return writeErr
	}
	select {
	case <-s.done:
		return s.err
	case <-time.After(statusWait):
		return fmt.Errorf("%w: %v", ErrConnection, writeErr)
	}
}

// CloseAndRecv ends the request and waits up to timeout for the call's
// status: nil for OK. The emulator plays out what it has buffered, up to
// about 300 ms, before it answers. The connection stays open until Close.
func (s *AudioStream) CloseAndRecv(timeout time.Duration) error {
	s.pw.Close()
	select {
	case <-s.done:
	case <-time.After(timeout):
		return fmt.Errorf("%w: no status %v after closing the stream", ErrConnection, timeout)
	}
	var st *Status
	if errors.As(s.err, &st) && st.Code == OK {
		return nil
	}
	return s.err
}

// Done is closed once the call has ended, for any reason.
func (s *AudioStream) Done() <-chan struct{} { return s.done }

// Err is why the call ended: a *Status, OK included, or an error wrapping
// ErrConnection. It is nil until Done is closed.
func (s *AudioStream) Err() error {
	select {
	case <-s.done:
		return s.err
	default:
		return nil
	}
}

// Close cancels the call if it is still going, closes its connection, and
// returns once the call has ended, so the emulator's microphone is free for
// the next call when it returns.
func (s *AudioStream) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.pw.CloseWithError(errClosed)
		s.cancel()
		s.cc.Close()
	})
	<-s.done
	return nil
}
