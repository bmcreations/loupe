package target

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/bmcreations/loupe/internal/scrcpy"
)

// simcap's stdin commands for the remote encoder, outside scrcpy's opcodes
// like 0x80 and 0x81.
const (
	// simcapRemoteGate: this byte, then u8, 0 to hold and 1 to open.
	simcapRemoteGate byte = 0x82
	// simcapRemoteKey: this byte alone. A remote keyframe.
	simcapRemoteKey byte = 0x83
	// simcapRemoteCap: this byte, then u32 bits per second.
	simcapRemoteCap byte = 0x84
	// simcapRemoteMaxSize: this byte, then u16 longest edge.
	simcapRemoteMaxSize byte = 0x85
)

// remoteTrackWanted decides whether to start simcap with -remote. The
// LOUPE_REMOTE_TRACK environment variable overrides what simcap's -caps said:
// "off" (or 0, false, no) never, "on" (1, true, yes) even for a build that
// does not list "remote" there.
func remoteTrackWanted(capAdvertised bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOUPE_REMOTE_TRACK"))) {
	case "off", "0", "false", "no":
		return false
	case "on", "1", "true", "yes":
		return true
	}
	return capAdvertised
}

// pumpRemote reads the remote track from fd 4. Like pumpPassthrough it never
// blocks: a stalled reader here would stall simcap's encode loop, and the main
// track with it. A reader that fell behind resumes at a keyframe, which it
// asks simcap for.
func (s *simStream) pumpRemote(r *os.File) {
	defer close(s.remote)
	defer r.Close()
	hdr, err := scrcpy.ReadStreamHeader(r)
	if err != nil {
		return
	}
	fmt.Fprintf(simcapLog, "simcap remote: %s %dx%d\n", hdr.Codec, hdr.Width, hdr.Height)
	s.readRemote(r)
}

// readRemote reads packets from r into s.remote until r ends.
func (s *simStream) readRemote(r io.Reader) {
	waitKey := false
	for {
		ph, err := scrcpy.ReadPacketHeader(r)
		if err != nil {
			return
		}
		buf := make([]byte, ph.Size)
		if _, err := io.ReadFull(r, buf); err != nil {
			return
		}
		f := Frame{PTS: ph.PTS, Key: ph.Key, Config: ph.Config, Data: buf, Captured: time.Now()}
		if waitKey && !f.Key && !f.Config {
			continue
		}
		select {
		case s.remote <- f:
			waitKey = false
		default:
			if !waitKey {
				waitKey = true
				go func() { _ = s.RemoteKeyframe() }()
			}
		}
	}
}

// Remote implements RemoteStream.
func (s *simStream) Remote() <-chan Frame {
	if s.remote == nil {
		return nil
	}
	return s.remote
}

func (s *simStream) writeStdin(b []byte) error {
	s.inMu.Lock()
	defer s.inMu.Unlock()
	_, err := s.stdin.Write(b)
	return err
}

func (s *simStream) remoteOn() error {
	if s.remote == nil {
		return fmt.Errorf("simcap: no remote track")
	}
	return nil
}

// RemoteGate implements RemoteStream.
func (s *simStream) RemoteGate(open bool) error {
	if err := s.remoteOn(); err != nil {
		return err
	}
	b := []byte{simcapRemoteGate, 0}
	if open {
		b[1] = 1
	}
	return s.writeStdin(b)
}

// RemoteKeyframe implements RemoteStream.
func (s *simStream) RemoteKeyframe() error {
	if err := s.remoteOn(); err != nil {
		return err
	}
	return s.writeStdin([]byte{simcapRemoteKey})
}

// RemoteCap implements RemoteStream.
func (s *simStream) RemoteCap(bps int) error {
	if err := s.remoteOn(); err != nil {
		return err
	}
	if bps <= 0 || bps > math.MaxUint32 {
		return fmt.Errorf("simcap: remote cap %d out of range", bps)
	}
	var b [5]byte
	b[0] = simcapRemoteCap
	binary.BigEndian.PutUint32(b[1:], uint32(bps))
	return s.writeStdin(b[:])
}

// RemoteMaxSize implements RemoteStream.
func (s *simStream) RemoteMaxSize(px int) error {
	if err := s.remoteOn(); err != nil {
		return err
	}
	if px < 0 || px > math.MaxUint16 {
		return fmt.Errorf("simcap: remote maxsize %d out of range", px)
	}
	var b [3]byte
	b[0] = simcapRemoteMaxSize
	binary.BigEndian.PutUint16(b[1:], uint16(px))
	return s.writeStdin(b[:])
}
