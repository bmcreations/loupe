// Package scrcpy implements the wire protocol spoken by scrcpy-server: the
// video stream framing it emits, and the control messages it accepts.
//
// The layouts here track scrcpy server 2.x and 3.x. They are NOT compatible
// with 1.x, which used a shorter touch message and a different stream header.
// If you swap the jar, re-check the two Encode functions below first; a silent
// layout drift shows up as ignored input rather than an error.
package scrcpy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"time"
)

// Control message type tags, from the server's ControlMessageReader.
const (
	typeInjectKeycode    byte = 0
	typeInjectText       byte = 1
	typeInjectTouchEvent byte = 2
	typeInjectScrollEven byte = 3
	typeGetClipboard     byte = 8
	typeSetClipboard     byte = 9
	typeUhidCreate       byte = 12
	typeUhidDestroy      byte = 14
	typeResetVideo       byte = 17
)

// ResetVideoMessageLen is the wire size of a reset-video message.
const ResetVideoMessageLen = 1

// Android MotionEvent action constants.
const (
	ActionDown   uint8 = 0
	ActionUp     uint8 = 1
	ActionMove   uint8 = 2
	ActionCancel uint8 = 3
)

// Android KeyEvent action constants.
const (
	KeyActionDown uint8 = 0
	KeyActionUp   uint8 = 1
)

// Frame flags packed into the high bits of the 64-bit PTS field.
const (
	flagConfig   uint64 = 1 << 63
	flagKeyFrame uint64 = 1 << 62
	ptsMask      uint64 = (1 << 62) - 1
)

// ErrShortBuffer indicates a control message could not be encoded into the
// caller's buffer.
var ErrShortBuffer = errors.New("scrcpy: buffer too small for control message")

// TouchMessageLen is the exact wire size of an inject-touch message on
// server 2.x and 3.x.
const TouchMessageLen = 32

// KeycodeMessageLen is the exact wire size of an inject-keycode message.
const KeycodeMessageLen = 14

// ScrollMessageLen is the exact wire size of an inject-scroll message.
const ScrollMessageLen = 21

// EncodeTouch writes an inject-touch message into buf, which must be at least
// TouchMessageLen bytes, and returns the number of bytes written.
//
// Pointer identity, not action encoding, is what makes multi-touch work:
// scrcpy-server tracks the set of live pointer ids and derives Android's
// pointer-indexed ACTION_POINTER_DOWN and ACTION_POINTER_UP itself. Send plain
// Down, Move and Up with stable distinct ids per finger and pinch gestures
// arrive intact.
//
// screenW and screenH must be the dimensions the coordinates are expressed in,
// and they must match the encoder's current picture exactly. The server does
// not rescale a mismatch: PositionMapper.map returns null unless the size
// agrees, and injectTouch then logs "Ignore positional event generated for
// size WxH (current size is WxH)" and drops the event. So a stale value here
// is not input landing in the wrong place -- it is input landing nowhere,
// which is what a rotation used to produce. See trap 42.
func EncodeTouch(buf []byte, action uint8, pointerID uint64, x, y int32, screenW, screenH uint16, pressure float32, buttons uint32) (int, error) {
	if len(buf) < TouchMessageLen {
		return 0, ErrShortBuffer
	}
	buf[0] = typeInjectTouchEvent
	buf[1] = action
	binary.BigEndian.PutUint64(buf[2:], pointerID)
	binary.BigEndian.PutUint32(buf[10:], uint32(x))
	binary.BigEndian.PutUint32(buf[14:], uint32(y))
	binary.BigEndian.PutUint16(buf[18:], screenW)
	binary.BigEndian.PutUint16(buf[20:], screenH)
	binary.BigEndian.PutUint16(buf[22:], encodeFixedU16(pressure))
	// actionButton: which button changed state. Zero for touch.
	binary.BigEndian.PutUint32(buf[24:], 0)
	// buttons: the set currently held. Zero for touch.
	binary.BigEndian.PutUint32(buf[28:], buttons)
	return TouchMessageLen, nil
}

// EncodeKeycode writes an inject-keycode message into buf, which must be at
// least KeycodeMessageLen bytes.
func EncodeKeycode(buf []byte, action uint8, keycode int32, repeat, metaState uint32) (int, error) {
	if len(buf) < KeycodeMessageLen {
		return 0, ErrShortBuffer
	}
	buf[0] = typeInjectKeycode
	buf[1] = action
	binary.BigEndian.PutUint32(buf[2:], uint32(keycode))
	binary.BigEndian.PutUint32(buf[6:], repeat)
	binary.BigEndian.PutUint32(buf[10:], metaState)
	return KeycodeMessageLen, nil
}

// InjectTextMax is the most UTF-8 bytes one inject-text message carries.
// scrcpy's own client stops at 300; the server would read more, but a longer
// string is one the server's key-character map is no likelier to type.
const InjectTextMax = 300

// AppendInjectText appends an inject-text message for s: the tag, a u32
// byte length and the UTF-8. s longer than InjectTextMax is an error rather
// than a truncation, so the caller splits on a rune boundary it chooses.
//
// The server types it through the virtual keyboard's key-character map, one
// key event pair per character, and logs and skips a character the map has
// no keys for, so an emoji is silently dropped rather than failing the rest.
func AppendInjectText(buf []byte, s string) ([]byte, error) {
	if len(s) > InjectTextMax {
		return buf, fmt.Errorf("inject text: %d bytes, the limit is %d", len(s), InjectTextMax)
	}
	buf = append(buf, typeInjectText)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(s)))
	return append(buf, s...), nil
}

// AppendUhidCreate appends a message that makes the server create a UHID
// device: the tag, then id, vendor and product as u16, the name behind a
// one-byte length and the HID report descriptor behind a two-byte length.
// Read out of scrcpy-server 3.3.4's ControlMessageReader.parseUhidCreate.
//
// The device lives as long as the server process: scrcpy closes every UHID
// device it created when it exits, so a session ending cannot leave one
// behind.
func AppendUhidCreate(buf []byte, id, vendor, product uint16, name string, desc []byte) ([]byte, error) {
	if len(name) > 0xff {
		return buf, fmt.Errorf("uhid create: name is %d bytes, the limit is 255", len(name))
	}
	if len(desc) > 0xffff {
		return buf, fmt.Errorf("uhid create: descriptor is %d bytes", len(desc))
	}
	buf = append(buf, typeUhidCreate)
	buf = binary.BigEndian.AppendUint16(buf, id)
	buf = binary.BigEndian.AppendUint16(buf, vendor)
	buf = binary.BigEndian.AppendUint16(buf, product)
	buf = append(buf, byte(len(name)))
	buf = append(buf, name...)
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(desc)))
	return append(buf, desc...), nil
}

// AppendUhidDestroy appends a message that removes the UHID device id.
func AppendUhidDestroy(buf []byte, id uint16) []byte {
	buf = append(buf, typeUhidDestroy)
	return binary.BigEndian.AppendUint16(buf, id)
}

// KeyboardDescriptor is a HID boot keyboard with no LED report: a modifier
// byte, a reserved byte and six key slots. Nothing is ever sent through it.
// It exists so Android sees an alphabetic keyboard attached, which is what
// stops the on-screen one rising when a field takes focus; typing goes
// through inject-text and inject-keycode instead. Leaving out the LEDs means
// the device has no output report, so Android never has a caps lock state to
// send back over a control socket nothing reads.
var KeyboardDescriptor = []byte{
	0x05, 0x01, // Usage Page (Generic Desktop)
	0x09, 0x06, // Usage (Keyboard)
	0xa1, 0x01, // Collection (Application)
	0x05, 0x07, //   Usage Page (Keyboard/Keypad)
	0x19, 0xe0, //   Usage Minimum (Left Control)
	0x29, 0xe7, //   Usage Maximum (Right GUI)
	0x15, 0x00, //   Logical Minimum (0)
	0x25, 0x01, //   Logical Maximum (1)
	0x75, 0x01, //   Report Size (1)
	0x95, 0x08, //   Report Count (8)
	0x81, 0x02, //   Input (Data, Variable, Absolute): modifiers
	0x95, 0x01, //   Report Count (1)
	0x75, 0x08, //   Report Size (8)
	0x81, 0x01, //   Input (Constant): reserved
	0x95, 0x06, //   Report Count (6)
	0x75, 0x08, //   Report Size (8)
	0x15, 0x00, //   Logical Minimum (0)
	0x25, 0x65, //   Logical Maximum (101)
	0x05, 0x07, //   Usage Page (Keyboard/Keypad)
	0x19, 0x00, //   Usage Minimum (0)
	0x29, 0x65, //   Usage Maximum (101)
	0x81, 0x00, //   Input (Data, Array): keys
	0xc0, // End Collection
}

// EncodeScroll writes an inject-scroll message into buf, which must be at
// least ScrollMessageLen bytes. Scroll deltas are in "wheel clicks", signed
// and clamped to [-16, 16) by the 16-bit fixed-point encoding.
func EncodeScroll(buf []byte, x, y int32, screenW, screenH uint16, hScroll, vScroll float32, buttons uint32) (int, error) {
	if len(buf) < ScrollMessageLen {
		return 0, ErrShortBuffer
	}
	buf[0] = typeInjectScrollEven
	binary.BigEndian.PutUint32(buf[1:], uint32(x))
	binary.BigEndian.PutUint32(buf[5:], uint32(y))
	binary.BigEndian.PutUint16(buf[9:], screenW)
	binary.BigEndian.PutUint16(buf[11:], screenH)
	binary.BigEndian.PutUint16(buf[13:], uint16(encodeFixedI16(hScroll)))
	binary.BigEndian.PutUint16(buf[15:], uint16(encodeFixedI16(vScroll)))
	binary.BigEndian.PutUint32(buf[17:], buttons)
	return ScrollMessageLen, nil
}

// encodeFixedU16 maps [0,1] onto the full uint16 range, the representation
// scrcpy uses for pressure. Values outside the range are clamped rather than
// wrapped, because a wrapped pressure reads as a released finger.
func encodeFixedU16(v float32) uint16 {
	if v <= 0 {
		return 0
	}
	if v >= 1 {
		return math.MaxUint16
	}
	return uint16(v * math.MaxUint16)
}

// encodeFixedI16 maps [-16,16) onto the int16 range for scroll deltas.
func encodeFixedI16(v float32) int16 {
	const scale = 1 << 11 // 2048 units per wheel click
	f := v * scale
	if f >= math.MaxInt16 {
		return math.MaxInt16
	}
	if f <= math.MinInt16 {
		return math.MinInt16
	}
	return int16(f)
}

// StreamHeader is the metadata scrcpy-server emits once, before any frames,
// when send_device_meta and send_codec_meta are enabled.
type StreamHeader struct {
	DeviceName string
	Codec      string
	Width      int
	Height     int
}

// deviceNameLen is the fixed-width, NUL-padded device name field.
const deviceNameLen = 64

// ReadStreamHeader consumes the device and codec metadata from r.
//
// It assumes the server was launched with send_device_meta=true and
// send_codec_meta=true, which is what Launch requests. If you disable either,
// this will consume frame bytes as header and the first decode will fail with
// a corrupt-bitstream error rather than anything that points back here.
func ReadStreamHeader(r io.Reader) (StreamHeader, error) {
	var h StreamHeader

	nameBuf := make([]byte, deviceNameLen)
	if _, err := io.ReadFull(r, nameBuf); err != nil {
		return h, fmt.Errorf("read device name: %w", err)
	}
	// The field is NUL-padded; everything after the first NUL is filler.
	for i, b := range nameBuf {
		if b == 0 {
			nameBuf = nameBuf[:i]
			break
		}
	}
	h.DeviceName = string(nameBuf)

	var codecMeta [12]byte
	if _, err := io.ReadFull(r, codecMeta[:]); err != nil {
		return h, fmt.Errorf("read codec meta: %w", err)
	}
	codecID := binary.BigEndian.Uint32(codecMeta[0:])
	h.Codec = fourCC(codecID)
	h.Width = int(binary.BigEndian.Uint32(codecMeta[4:]))
	h.Height = int(binary.BigEndian.Uint32(codecMeta[8:]))

	if h.Width <= 0 || h.Height <= 0 {
		return h, fmt.Errorf("scrcpy: implausible stream dimensions %dx%d, check server version match", h.Width, h.Height)
	}
	return h, nil
}

// fourCC renders a codec id as its four-character name, e.g. "h264".
func fourCC(v uint32) string {
	return string([]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// PacketHeader is the per-frame prefix emitted when send_frame_meta is on.
type PacketHeader struct {
	PTS    time.Duration
	Size   uint32
	Key    bool
	Config bool
}

// ReadPacketHeader consumes one 12-byte frame prefix from r.
func ReadPacketHeader(r io.Reader) (PacketHeader, error) {
	var buf [12]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return PacketHeader{}, err
	}
	raw := binary.BigEndian.Uint64(buf[0:])
	return PacketHeader{
		PTS:    time.Duration(raw&ptsMask) * time.Microsecond,
		Size:   binary.BigEndian.Uint32(buf[8:]),
		Key:    raw&flagKeyFrame != 0,
		Config: raw&flagConfig != 0,
	}, nil
}

// EncodeResetVideo writes a reset-video request, which makes the device's
// encoder emit a fresh keyframe.
//
// The viewer can only resynchronize on a keyframe, so after shedding an
// inter-frame it has to wait for the next one. scrcpy does not emit them on a
// timer: the synthetic pattern's keyframe per second is its own behaviour, not
// the protocol's. On a real device the interval is effectively unbounded, so
// without asking there may never be another one.
//
// Opcode 17 is TYPE_RESET_VIDEO, read out of scrcpy-server 3.3.4's own dex.
func EncodeResetVideo(buf []byte) (int, error) {
	if len(buf) < ResetVideoMessageLen {
		return 0, fmt.Errorf("reset video: buffer is %d bytes, need %d", len(buf), ResetVideoMessageLen)
	}
	buf[0] = typeResetVideo
	return ResetVideoMessageLen, nil
}
