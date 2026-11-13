// Package emugrpc is a client for the one Android emulator gRPC call loupe
// makes: EmulatorController/injectAudio, which feeds the guest's microphone.
//
// It encodes the two protobuf messages and the gRPC message framing by hand
// over net/http's own HTTP/2 client, rather than pulling in
// google.golang.org/grpc and generated code for a service with dozens of
// calls loupe never uses.
// The messages are in the emulator's own emulator_controller.proto
// (emulator/lib in the SDK); the tests check the bytes against protoc's.
package emugrpc

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Channels is AudioFormat.Channels.
type Channels uint64

const (
	Mono   Channels = 0
	Stereo Channels = 1
)

// SampleFormat is AudioFormat.SampleFormat.
type SampleFormat uint64

const (
	U8  SampleFormat = 0 // unsigned 8 bit
	S16 SampleFormat = 1 // signed 16 bit, little endian
)

// DeliveryMode is AudioFormat.DeliveryMode.
type DeliveryMode uint64

const (
	// ModeUnspecified blocks the call until the guest reads, so with no
	// recorder open the stream stalls once the emulator's buffer is full.
	ModeUnspecified DeliveryMode = 0
	// ModeRealTime never blocks: newer audio overwrites what the guest has
	// not read yet, so the client has to send at the capture rate.
	ModeRealTime DeliveryMode = 1
)

// AudioFormat is android.emulation.control.AudioFormat.
type AudioFormat struct {
	SamplingRate uint64       // field 1
	Channels     Channels     // field 2
	Format       SampleFormat // field 3
	Mode         DeliveryMode // field 4
}

// AudioPacket is android.emulation.control.AudioPacket. The emulator honours
// Format on the first packet of a call only.
type AudioPacket struct {
	Format    *AudioFormat // field 1
	Timestamp uint64       // field 2, microseconds
	Audio     []byte       // field 3
}

const (
	wireVarint = 0
	wireBytes  = 2
)

// Marshal encodes f as proto3 does: a field at its zero value is left out.
func (f AudioFormat) Marshal() []byte {
	var b []byte
	b = appendVarintField(b, 1, f.SamplingRate)
	b = appendVarintField(b, 2, uint64(f.Channels))
	b = appendVarintField(b, 3, uint64(f.Format))
	b = appendVarintField(b, 4, uint64(f.Mode))
	return b
}

// Marshal encodes p as proto3 does. A non-nil Format is written even when
// every field in it is zero, as an empty message.
func (p AudioPacket) Marshal() []byte {
	var b []byte
	if p.Format != nil {
		b = appendBytesField(b, 1, p.Format.Marshal())
	}
	b = appendVarintField(b, 2, p.Timestamp)
	if len(p.Audio) > 0 {
		b = appendBytesField(b, 3, p.Audio)
	}
	return b
}

func appendVarintField(b []byte, field int, v uint64) []byte {
	if v == 0 {
		return b
	}
	b = binary.AppendUvarint(b, uint64(field)<<3|wireVarint)
	return binary.AppendUvarint(b, v)
}

func appendBytesField(b []byte, field int, v []byte) []byte {
	b = binary.AppendUvarint(b, uint64(field)<<3|wireBytes)
	b = binary.AppendUvarint(b, uint64(len(v)))
	return append(b, v...)
}

// frameHeader is gRPC's length prefix: a compressed flag byte, then the
// message length as a big-endian uint32.
const frameHeader = 5

// appendFrame appends msg as one uncompressed gRPC message.
func appendFrame(b, msg []byte) []byte {
	b = append(b, 0)
	b = binary.BigEndian.AppendUint32(b, uint32(len(msg)))
	return append(b, msg...)
}

// errShortFrame is a body that ended inside a gRPC message.
var errShortFrame = errors.New("emugrpc: body ended inside a message")

// splitFrames returns the messages in body, a run of whole gRPC frames.
func splitFrames(body []byte) ([][]byte, error) {
	var msgs [][]byte
	for len(body) > 0 {
		if len(body) < frameHeader {
			return nil, errShortFrame
		}
		if body[0] != 0 {
			return nil, fmt.Errorf("emugrpc: compressed message (flag %d) without negotiating compression", body[0])
		}
		n := binary.BigEndian.Uint32(body[1:frameHeader])
		if uint64(len(body)-frameHeader) < uint64(n) {
			return nil, errShortFrame
		}
		msgs = append(msgs, body[frameHeader:frameHeader+int(n)])
		body = body[frameHeader+int(n):]
	}
	return msgs, nil
}
