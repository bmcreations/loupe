package target

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bmcreations/loupe/internal/h264"
	"github.com/bmcreations/loupe/internal/scrcpy"
)

// ADBTarget streams any device reachable over adb. A physical handset and a
// running emulator are the same thing at this layer: the emulator type in this
// package exists only to boot one and then hand control here.
type ADBTarget struct {
	adb     ADB
	info    DeviceInfo
	jar     string
	version string
	kind    Kind

	// describeCache holds the result of Describe, once it succeeds. See
	// describe.go.
	describeCache describeCache

	// settingsMu serializes ApplySettings across concurrent callers on this
	// target. talkBack's write is a read-modify-write of a shared,
	// colon-separated list, and two overlapping PUTs racing that would let
	// one caller's change clobber the other's read.
	settingsMu sync.Mutex

	// kbMu guards hwKeyboard and streams. See adbkeyboard.go.
	kbMu       sync.Mutex
	hwKeyboard bool
	streams    map[*adbStream]struct{}

	// compose holds the compose agent's attachment. See compose.go.
	composeOnce sync.Once
	compose     *composeDevice
}

// NewADBTarget builds a target for an already-attached device. jar and version
// must refer to the same scrcpy-server build; a mismatch fails at Open with a
// connect timeout rather than a clear message, because the server exits before
// it ever listens.
func NewADBTarget(adb ADB, info DeviceInfo, jar, version string) *ADBTarget {
	kind := KindDevice
	if info.Emulator {
		kind = KindEmulator
	}
	return &ADBTarget{adb: adb, info: info, jar: jar, version: version, kind: kind}
}

func (t *ADBTarget) ID() string    { return t.info.Serial }
func (t *ADBTarget) Kind() Kind    { return t.kind }
func (t *ADBTarget) Label() string { return t.info.Label() }

// Info is what adb reported about the device when this target was built. It
// is fixed for the life of the target; a device whose state changes gets a new
// one.
func (t *ADBTarget) Info() DeviceInfo { return t.info }

// Open launches scrcpy-server on the device and returns a live stream.
func (t *ADBTarget) Open(ctx context.Context, opts Options) (Stream, error) {
	if !t.info.Ready() {
		return nil, fmt.Errorf("device %s is in state %q, not ready to stream", t.info.Serial, t.info.State)
	}

	sess, err := scrcpy.Launch(ctx, scrcpy.Config{
		ADBPath:       t.adb.Path,
		Serial:        t.info.Serial,
		ServerJar:     t.jar,
		ServerVersion: t.version,
		MaxSize:       opts.MaxSize,
		MaxFPS:        opts.MaxFPS,
		BitRate:       opts.BitRate,

		VideoCodec:          opts.VideoCodec,
		KeyframeIntervalSec: opts.KeyframeIntervalSec,
	})
	if err != nil {
		return nil, err
	}

	hdr := sess.Header()
	name := hdr.DeviceName
	if name == "" {
		name = t.info.Label()
	}

	s := &adbStream{
		sess: sess,
		meta: Meta{
			Codec:  hdr.Codec,
			Width:  hdr.Width,
			Height: hdr.Height,
			Name:   name,
		},
		frames: make(chan Frame, 64),
		clip:   newClipState(),
	}
	go s.pump()
	go s.clip.watch(sess.Clipboard())
	// A keyboard that fails to attach is not worth the video: the send
	// fails only on a control socket that is already going, and the
	// stream's own error will say so.
	_ = t.track(s)
	return s, nil
}

// adbStream adapts a scrcpy session to the Stream interface, translating
// semantic input events into scrcpy control messages.
type adbStream struct {
	sess   *scrcpy.Session
	frames chan Frame

	// metaMu guards meta, which is not fixed for the life of the stream.
	// scrcpy sends its codec meta once, at stream start, so after a rotation
	// the header describes the shape the picture used to be. The live size is
	// read from each parameter set instead; see noteConfig.
	metaMu sync.RWMutex
	meta   Meta

	// bufMu guards ctlBuf. Control messages are small and infrequent relative
	// to frames, so one shared buffer behind a mutex beats per-call allocation.
	bufMu  sync.Mutex
	ctlBuf [scrcpy.TouchMessageLen]byte

	closeOnce sync.Once

	// kbMu guards hwKeyboard, whether this server has the UHID keyboard.
	kbMu       sync.Mutex
	hwKeyboard bool
	// untrack removes the stream from its target's set; nil until Open
	// registers it.
	untrack func()

	// settleMu guards settleUntil: when the last burst of typed text is
	// taken to have landed, so a key sent before then waits. See
	// textSettleTime in adbkeyboard.go.
	settleMu    sync.Mutex
	settleUntil time.Time

	// clip is the device clipboard as the server last reported it.
	clip *clipState
}

func (s *adbStream) Meta() Meta {
	s.metaMu.RLock()
	defer s.metaMu.RUnlock()
	return s.meta
}

func (s *adbStream) Frames() <-chan Frame { return s.frames }
func (s *adbStream) Err() error           { return s.sess.Err() }

// noteConfig updates the stream's dimensions from a parameter set, and reports
// whether they changed.
//
// A rotation does not restart the stream and does not resend the codec meta:
// measured on emulator-5554 with cmd/rotprobe, the socket stays up and a fresh
// SPS arrives about 800ms after the display turns over. The SPS is therefore
// the only in-band statement of the current size, and it is authoritative --
// scrcpy-server's own verbose log reported "current size is 1080x480" for the
// same parameter set this reads 1080x480 from.
//
// Getting this wrong does not misplace a touch, it deletes it. PositionMapper
// compares the size on the wire against the encoder's current size and returns
// null when they differ, and Controller then logs "Ignore positional event
// generated for size ..." and drops the event. So a stale size here is a
// screen that has simply stopped responding.
func (s *adbStream) noteConfig(data []byte) bool {
	sps := h264.FindSPS(data)
	if sps == nil {
		return false
	}
	w, h, err := h264.SPSDimensions(sps)
	if err != nil {
		// An unparsable parameter set is not worth failing a live session
		// over: the previous size is still the best answer available, and an
		// HEVC stream reaches here with no NAL type 7 at all.
		return false
	}

	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	if s.meta.Width == w && s.meta.Height == h {
		return false
	}
	s.meta.Width, s.meta.Height = w, h
	return true
}

func (s *adbStream) Close() error {
	s.closeOnce.Do(func() {
		if s.untrack != nil {
			s.untrack()
		}
		_ = s.sess.Close()
	})
	return nil
}

// pump copies frames from the scrcpy session, converting the type. It closes
// s.frames when the source ends.
func (s *adbStream) pump() {
	defer close(s.frames)
	for f := range s.sess.Frames() {
		// Before the frame is forwarded, so a viewer cannot see a picture of
		// the new shape while Meta still describes the old one.
		if f.Config {
			s.noteConfig(f.Data)
		}
		s.frames <- Frame{
			PTS:      f.PTS,
			Key:      f.Key,
			Config:   f.Config,
			Data:     f.Data,
			Captured: f.Captured,
		}
	}
}

func (s *adbStream) Touch(t Touch) error {
	action := scrcpy.ActionMove
	switch t.Action {
	case TouchDown:
		action = scrcpy.ActionDown
	case TouchUp:
		action = scrcpy.ActionUp
	case TouchCancel:
		action = scrcpy.ActionCancel
	}

	pressure := t.Pressure
	// Android treats a zero-pressure down as a hover, which produces no tap.
	// Clients that do not report pressure send zero, so normalize it here.
	if t.Action != TouchUp && pressure <= 0 {
		pressure = 1
	}

	meta := s.Meta()
	s.bufMu.Lock()
	n, err := scrcpy.EncodeTouch(
		s.ctlBuf[:], action, t.PointerID, t.X, t.Y,
		uint16(meta.Width), uint16(meta.Height), pressure, 0,
	)
	msg := s.ctlBuf[:n]
	s.bufMu.Unlock()
	if err != nil {
		return err
	}
	return s.sess.SendControl(msg)
}

func (s *adbStream) Scroll(sc Scroll) error {
	meta := s.Meta()
	var buf [scrcpy.ScrollMessageLen]byte
	n, err := scrcpy.EncodeScroll(
		buf[:], sc.X, sc.Y,
		uint16(meta.Width), uint16(meta.Height),
		sc.HScroll, sc.VScroll, 0,
	)
	if err != nil {
		return err
	}
	return s.sess.SendControl(buf[:n])
}

func (s *adbStream) RequestKeyframe() error {
	var buf [scrcpy.ResetVideoMessageLen]byte
	n, err := scrcpy.EncodeResetVideo(buf[:])
	if err != nil {
		return err
	}
	return s.sess.SendControl(buf[:n])
}

func (s *adbStream) Key(k Key) error {
	action := scrcpy.KeyActionDown
	if k.Action == KeyUp {
		action = scrcpy.KeyActionUp
	} else {
		s.waitForText()
	}
	var buf [scrcpy.KeycodeMessageLen]byte
	n, err := scrcpy.EncodeKeycode(buf[:], action, k.Keycode, 0, 0)
	if err != nil {
		return err
	}
	return s.sess.SendControl(buf[:n])
}
