package target

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/emugrpc"
)

// MicFeeder is a target whose microphone loupe can feed: an AVD, through the
// emulator's gRPC injectAudio, which works with -no-audio.
type MicFeeder interface {
	// OpenMic starts a call that feeds the device's microphone. The call
	// holds the emulator's one microphone until Close.
	OpenMic(ctx context.Context) (MicCall, error)
}

// MicCall is one open microphone feed. *emugrpc.AudioStream is one.
type MicCall interface {
	Send(emugrpc.AudioPacket) error
	CloseAndRecv(timeout time.Duration) error
	Done() <-chan struct{}
	Err() error
	Close() error
}

var _ MicFeeder = (*Emulator)(nil)

// OpenMic opens injectAudio on the emulator this AVD runs as, waking it if it
// was paused for being idle and holding off the idle pause until Close: a
// guest that is paused records nothing, and a feed can outlast the idle
// window with no other call reaching the device.
//
// The gRPC port and token come from the emulator's running file. An emulator
// started without -grpc fails with emugrpc.ErrNoGRPC.
func (e *Emulator) OpenMic(ctx context.Context) (MicCall, error) {
	in, release, err := e.running()
	if err != nil {
		return nil, err
	}
	call, err := openMic(ctx, in.Info().Serial)
	if err != nil {
		release()
		return nil, err
	}
	return &heldMic{AudioStream: call, release: release}, nil
}

func openMic(ctx context.Context, serial string) (*emugrpc.AudioStream, error) {
	port, err := consolePort(serial)
	if err != nil {
		return nil, err
	}
	dir, err := emugrpc.RunningDir()
	if err != nil {
		return nil, err
	}
	ep, err := emugrpc.FindEndpoint(dir, port)
	if err != nil {
		return nil, err
	}
	return emugrpc.InjectAudio(ctx, ep)
}

// consolePort is the console port in an emulator serial, 5558 in
// emulator-5558.
func consolePort(serial string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(serial, "emulator-"))
	if err != nil || !strings.HasPrefix(serial, "emulator-") {
		return 0, fmt.Errorf("%w: %q is not a local emulator serial", emugrpc.ErrNoGRPC, serial)
	}
	return n, nil
}

// heldMic releases the emulator's idle-pause hold when the call closes.
type heldMic struct {
	*emugrpc.AudioStream
	once    sync.Once
	release func()
}

func (h *heldMic) Close() error {
	err := h.AudioStream.Close()
	h.once.Do(h.release)
	return err
}
