package emugrpc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

// h2cServer serves h on 127.0.0.1 over HTTP/2 without TLS, as the
// emulator's gRPC service does.
func h2cServer(t *testing.T, h http.HandlerFunc) Endpoint {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	srv.Config.Protocols = &p
	srv.Start()
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return Endpoint{Port: port, Token: "tok"}
}

// endWithTrailers answers as a gRPC server whose call ran: headers, the
// Empty reply, then grpc-status in the trailers.
func endWithTrailers(w http.ResponseWriter, code Code, msg string) {
	w.Header().Set("Content-Type", "application/grpc")
	w.Header().Set("Trailer", "Grpc-Status, Grpc-Message")
	w.WriteHeader(http.StatusOK)
	w.Write(appendFrame(nil, nil))
	w.Header().Set("Grpc-Status", strconv.Itoa(int(code)))
	w.Header().Set("Grpc-Message", msg)
}

func wantCode(t *testing.T, err error, code Code) *Status {
	t.Helper()
	var st *Status
	if !errors.As(err, &st) || st.Code != code {
		t.Fatalf("got %v, want %v", err, code)
	}
	return st
}

func waitDone(t *testing.T, s *AudioStream) {
	t.Helper()
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("call did not end")
	}
}

func TestInjectAudioSendsFramesAndEndsOK(t *testing.T) {
	type got struct {
		header http.Header
		path   string
		body   []byte
	}
	seen := make(chan got, 1)
	ep := h2cServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen <- got{r.Header.Clone(), r.URL.Path, body}
		endWithTrailers(w, OK, "")
	})
	s, err := InjectAudio(context.Background(), ep)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first := AudioPacket{Format: &AudioFormat{SamplingRate: 16000, Format: S16, Mode: ModeRealTime}, Audio: []byte{1, 2}}
	second := AudioPacket{Timestamp: 20000, Audio: []byte{3, 4}}
	for _, p := range []AudioPacket{first, second} {
		if err := s.Send(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CloseAndRecv(2 * time.Second); err != nil {
		t.Fatalf("CloseAndRecv: %v", err)
	}
	g := <-seen
	if g.path != "/android.emulation.control.EmulatorController/injectAudio" {
		t.Errorf("path %q", g.path)
	}
	for k, v := range map[string]string{"Content-Type": "application/grpc", "Te": "trailers", "Authorization": "Bearer tok"} {
		if g.header.Get(k) != v {
			t.Errorf("%s: %q, want %q", k, g.header.Get(k), v)
		}
	}
	want := appendFrame(appendFrame(nil, first.Marshal()), second.Marshal())
	if !bytes.Equal(g.body, want) {
		t.Errorf("body %x, want %x", g.body, want)
	}
	wantCode(t, s.Err(), OK)
}

// The emulator refuses a second call with a Trailers-Only reply before any
// packet, as emulator 37.3.6 does 2 ms after the call opens.
func TestInjectAudioTrailersOnlyRefusal(t *testing.T) {
	ep := h2cServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Grpc-Status", "9")
		w.Header().Set("Grpc-Message", "There can be only one microphone active")
		w.WriteHeader(http.StatusOK)
	})
	s, err := InjectAudio(context.Background(), ep)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	waitDone(t, s)
	st := wantCode(t, s.Err(), FailedPrecondition)
	if st.Message != "There can be only one microphone active" {
		t.Errorf("message %q", st.Message)
	}
	// A Send after the refusal reports the refusal, not a broken pipe, and
	// does not hang on a body nobody reads.
	sent := make(chan error, 1)
	go func() { sent <- s.Send(AudioPacket{Audio: make([]byte, 640)}) }()
	select {
	case err := <-sent:
		wantCode(t, err, FailedPrecondition)
	case <-time.After(2 * time.Second):
		t.Fatal("Send after the refusal hung")
	}
}

// A status in the trailers after the server read a packet, with its message
// percent-encoded as gRPC sends it.
func TestInjectAudioStatusInTrailers(t *testing.T) {
	ep := h2cServer(t, func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, frameHeader)
		io.ReadFull(r.Body, buf)
		endWithTrailers(w, InvalidArgument, "packet%20too%20large%3A%2010000")
	})
	s, err := InjectAudio(context.Background(), ep)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Send(AudioPacket{Audio: make([]byte, 10000)})
	waitDone(t, s)
	st := wantCode(t, s.Err(), InvalidArgument)
	if st.Message != "packet too large: 10000" {
		t.Errorf("message %q", st.Message)
	}
}

func TestInjectAudioHTTPErrorWithoutStatus(t *testing.T) {
	ep := h2cServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	s, err := InjectAudio(context.Background(), ep)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	waitDone(t, s)
	wantCode(t, s.Err(), Unauthenticated)
}

// A reset stream is the connection's failure, not a gRPC status.
func TestInjectAudioStreamReset(t *testing.T) {
	ep := h2cServer(t, func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	})
	s, err := InjectAudio(context.Background(), ep)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	waitDone(t, s)
	if !errors.Is(s.Err(), ErrConnection) {
		t.Fatalf("got %v, want ErrConnection", s.Err())
	}
}

func TestInjectAudioNothingListening(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	s, err := InjectAudio(context.Background(), Endpoint{Port: port})
	if err == nil {
		defer s.Close()
		waitDone(t, s)
		err = s.Err()
	}
	if !errors.Is(err, ErrConnection) {
		t.Fatalf("got %v, want ErrConnection", err)
	}
}

// Close mid-call ends the call on the server too, so the emulator's
// microphone is free when Close returns.
func TestInjectAudioCloseEndsServerCall(t *testing.T) {
	started, ended := make(chan struct{}), make(chan struct{})
	ep := h2cServer(t, func(w http.ResponseWriter, r *http.Request) {
		defer close(ended)
		w.Header().Set("Content-Type", "application/grpc")
		io.ReadFull(r.Body, make([]byte, frameHeader))
		close(started)
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	})
	s, err := InjectAudio(context.Background(), ep)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(AudioPacket{Audio: make([]byte, 640)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("server never got the call")
	}
	s.Close()
	s.Close() // twice is fine
	if !errors.Is(s.Err(), ErrConnection) {
		t.Errorf("Err after Close: %v, want ErrConnection", s.Err())
	}
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("server call still open after Close")
	}
	if err := s.Send(AudioPacket{Audio: []byte{1}}); !errors.Is(err, errClosed) {
		t.Errorf("Send after Close: %v, want errClosed", err)
	}
}
