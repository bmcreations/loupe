package emugrpc

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestLiveInjectAudio runs against a real emulator. Set
// LOUPE_EMU_GRPC_CONSOLE to its console port (5558 for emulator-5558); it
// must have been booted with -grpc -grpc-use-token.
//
// It pushes 2 s of a 440 Hz tone in real-time mode and wants OK, checks
// that a second call while the first is open is FAILED_PRECONDITION, that a
// call aborted with Close leaves the microphone free for the next one at
// once, and that nothing is left holding it at the end.
func TestLiveInjectAudio(t *testing.T) {
	raw := os.Getenv("LOUPE_EMU_GRPC_CONSOLE")
	if raw == "" {
		t.Skip("LOUPE_EMU_GRPC_CONSOLE not set")
	}
	console, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := RunningDir()
	if err != nil {
		t.Fatal(err)
	}
	ep, err := FindEndpoint(dir, console)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("emulator-%d: pid %d, grpc port %d, token %d bytes", console, ep.PID, ep.Port, len(ep.Token))
	ctx := context.Background()
	format := &AudioFormat{SamplingRate: 16000, Channels: Mono, Format: S16, Mode: ModeRealTime}
	tone := toneS16(440, 16000, 2*time.Second)
	const packet = 640 // 20 ms

	// A: the tone, paced at the capture rate, with B opened halfway.
	a, err := InjectAudio(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	began := time.Now()
	var b *AudioStream
	for i := 0; i*packet < len(tone); i++ {
		p := AudioPacket{Timestamp: uint64(i) * 20000, Audio: tone[i*packet : (i+1)*packet]}
		if i == 0 {
			p.Format = format
		}
		if err := a.Send(p); err != nil {
			t.Fatalf("A packet %d after %v: %v", i, time.Since(began), err)
		}
		if i == 50 {
			b = openSecond(t, ep, format)
		}
		time.Sleep(time.Until(began.Add(time.Duration(i+1) * 20 * time.Millisecond)))
	}
	t.Logf("A: sent 2 s of tone in %v", time.Since(began).Round(time.Millisecond))
	closing := time.Now()
	if err := a.CloseAndRecv(2 * time.Second); err != nil {
		t.Fatalf("A ended with %v", err)
	}
	t.Logf("A: OK %v after closing the request", time.Since(closing).Round(time.Millisecond))
	a.Close()
	if b != nil {
		b.Close()
	}

	// C aborted mid-stream, then D at once.
	c, err := InjectAudio(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		p := AudioPacket{Timestamp: uint64(i) * 20000, Audio: tone[i*packet : (i+1)*packet]}
		if i == 0 {
			p.Format = format
		}
		if err := c.Send(p); err != nil {
			t.Fatalf("C packet %d: %v", i, err)
		}
	}
	aborting := time.Now()
	c.Close()
	t.Logf("C: Close returned in %v, call ended with %v", time.Since(aborting).Round(time.Millisecond), c.Err())
	pushAndClose(t, "D (right after C's abort)", ep, format, tone)

	// E: nothing left behind.
	pushAndClose(t, "E (final)", ep, format, tone)
}

// openSecond opens a call while another is open, reports when the emulator
// refuses it, and wants FAILED_PRECONDITION.
func openSecond(t *testing.T, ep Endpoint, format *AudioFormat) *AudioStream {
	t.Helper()
	opened := time.Now()
	b, err := InjectAudio(context.Background(), ep)
	if err != nil {
		t.Fatalf("B: %v", err)
	}
	select {
	case <-b.Done():
		t.Logf("B: ended %v after opening, before any packet: %v", time.Since(opened).Round(time.Millisecond), b.Err())
	case <-time.After(300 * time.Millisecond):
		t.Logf("B: still open 300 ms after opening with no packet sent; sending one")
		sent := time.Now()
		err := b.Send(AudioPacket{Format: format, Audio: make([]byte, 640)})
		select {
		case <-b.Done():
			t.Logf("B: ended %v after its first packet (Send: %v): %v", time.Since(sent).Round(time.Millisecond), err, b.Err())
		case <-time.After(time.Second):
			t.Fatalf("B: still open 1 s after its first packet")
		}
	}
	var st *Status
	if !errors.As(b.Err(), &st) || st.Code != FailedPrecondition {
		t.Fatalf("B ended with %v, want FAILED_PRECONDITION", b.Err())
	}
	return b
}

// pushAndClose sends 200 ms of tone on a new call and wants OK.
func pushAndClose(t *testing.T, name string, ep Endpoint, format *AudioFormat, tone []byte) {
	t.Helper()
	s, err := InjectAudio(context.Background(), ep)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	defer s.Close()
	for i := 0; i < 10; i++ {
		p := AudioPacket{Timestamp: uint64(i) * 20000, Audio: tone[i*640 : (i+1)*640]}
		if i == 0 {
			p.Format = format
		}
		if err := s.Send(p); err != nil {
			t.Fatalf("%s packet %d: %v", name, i, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	closing := time.Now()
	if err := s.CloseAndRecv(2 * time.Second); err != nil {
		t.Fatalf("%s ended with %v", name, err)
	}
	t.Logf("%s: OK %v after closing the request", name, time.Since(closing).Round(time.Millisecond))
}

func toneS16(hz float64, rate int, d time.Duration) []byte {
	n := int(d.Seconds() * float64(rate))
	b := make([]byte, 2*n)
	for i := 0; i < n; i++ {
		v := int16(0.3 * math.MaxInt16 * math.Sin(2*math.Pi*hz*float64(i)/float64(rate)))
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}
