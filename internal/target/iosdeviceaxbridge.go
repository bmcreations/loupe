package target

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
)

// simcapBridgeName is the AX bridge binary, shipped beside simcap. It builds
// its own CoreDevice tunnel over USB and streams the phone's accessibility
// dump archives; iosdeviceax.go decodes them.
const simcapBridgeName = "simcap-bridge"

var _ Inspector = (*IOSDevice)(nil)

// maxAXFrame caps a single dump archive. Real trees are a few MiB (the lock
// screen is ~1.6MB); this is only a guard against a desynced pipe.
const maxAXFrame = 64 << 20

// Inspect implements Inspector for a physical iPhone. CoreDevice refuses its
// remoteAXService to a plain CoreDevice client, so the tree comes over
// simcap-bridge's own tunnel instead. It covers the frontmost application
// only -- what is on screen -- the same as a simulator's Inspect, and reports
// device pixels, from CoreDevice's display size (ScreenSize).
//
// The phone's own path is untouched: ScreenSize and Tap still go through
// simcap and CoreDevice; only the tree is new.
func (d *IOSDevice) Inspect(ctx context.Context) (*UI, error) {
	d.mu.Lock()
	info, bin := d.info, d.simcap
	d.mu.Unlock()
	if bin == "" {
		return nil, fmt.Errorf("simcap is not built (make simcap); it is what reaches a physical iPhone")
	}
	if info.Transport != "wired" {
		return nil, fmt.Errorf("%w: the accessibility tree comes over USB", ErrIOSStreamNeedsCable)
	}
	w, h, err := d.ScreenSize(ctx)
	if err != nil {
		return nil, err
	}
	frames, err := axBridgeDump(ctx, bin, info.UDID)
	if err != nil {
		return nil, err
	}
	root, err := decodePhoneAXMerged(frames...)
	if err != nil {
		return nil, err
	}
	return phoneAXToUI(root, w, h)
}

// axBridgeDump runs simcap-bridge for one tree dump and returns the raw dump
// archives it streamed, in arrival order (InitialDump then AdditionalData).
func axBridgeDump(ctx context.Context, simcapBin, udid string) ([][]byte, error) {
	bridge := filepath.Join(filepath.Dir(simcapBin), simcapBridgeName)
	cmd := exec.CommandContext(ctx, bridge, udid)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s: %w", simcapBridgeName, err)
	}
	frames, readErr := readAXFrames(stdout)
	// Drain any remaining stdout so the child never blocks on a full pipe,
	// then reap it.
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()
	if waitErr != nil {
		return nil, fmt.Errorf("%s: %v: %s", simcapBridgeName, waitErr, strings.TrimSpace(stderr.String()))
	}
	if readErr != nil {
		return nil, fmt.Errorf("%s: read frames: %w", simcapBridgeName, readErr)
	}
	if len(frames) == 0 {
		return nil, fmt.Errorf("%s: no accessibility dump: %s", simcapBridgeName, strings.TrimSpace(stderr.String()))
	}
	return frames, nil
}

// readAXFrames reads length-prefixed frames ([u32 LE length][bytes]) until EOF.
func readAXFrames(r io.Reader) ([][]byte, error) {
	var frames [][]byte
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return frames, nil
			}
			return frames, err
		}
		n := binary.LittleEndian.Uint32(hdr[:])
		if n == 0 || n > maxAXFrame {
			return frames, fmt.Errorf("frame length %d out of range", n)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return frames, err
		}
		frames = append(frames, buf)
	}
}
