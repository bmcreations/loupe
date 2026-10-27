// Package target defines the contract every streamable Android source satisfies,
// plus the concrete implementations: a physical device over adb, a managed
// emulator, and a synthetic test pattern used to validate the client without
// any Android hardware attached.
package target

import (
	"context"
	"errors"
	"time"

	"github.com/bmcreations/loupe/internal/scrcpy"
)

// Kind identifies the class of a target. It is surfaced to clients so the UI can
// label a session, and is used to pick default stream options.
type Kind string

const (
	KindDevice    Kind = "device"
	KindEmulator  Kind = "emulator"
	KindSynthetic Kind = "synthetic"
	KindSimulator Kind = "simulator"
	// KindIOSDevice is a physical iPhone or iPad, reached through devicectl.
	KindIOSDevice Kind = "iosDevice"
)

// AcceptsInput reports whether a stream of this kind takes touch and key
// input. A simulator does, through simcap's -input mode. A physical iOS
// device does not stream yet, so it takes nothing either.
func (k Kind) AcceptsInput() bool { return k != KindSynthetic && k != KindIOSDevice }

// Options are the stream parameters requested by the caller. Implementations
// treat these as a request, not a guarantee, and report what they actually
// negotiated in Meta.
type Options struct {
	// MaxSize bounds the longest edge of the encoded video, in pixels. Zero means
	// the target's native resolution. Lowering this is the single most effective
	// lever on both encode cost and bandwidth.
	MaxSize int
	// MaxFPS caps the capture rate. Zero means the target's default.
	MaxFPS int
	// BitRate is the requested video bitrate in bits per second.
	BitRate int
	// VideoCodec names the device-side encoder. Empty means the target's
	// default, which is h264 -- the only codec the client is known to decode.
	// Synthetic ignores this.
	VideoCodec string
	// KeyframeIntervalSec bounds the GOP, in seconds. Zero means the target's
	// default: one second on Android, simKeyframeIntervalSec on a simulator. This is the floor under every path that holds
	// frames back waiting for a keyframe: without it the encoder's own interval
	// is effectively unbounded and such a wait never ends. Synthetic already
	// emits one per second and ignores this.
	KeyframeIntervalSec int
}

// DefaultOptions are tuned for a 60fps session over a link that is NOT a LAN:
// a Tailscale path that may be relayed rather than direct, or a phone on
// cellular. 8 Mbps is a LAN number, and on a relayed path it produces a
// permanent backlog rather than a better picture. Raise it when you have
// measured the link and know it can carry more.
// DefaultMaxFPS is the capture rate used when Options leaves MaxFPS at zero.
// Named rather than repeated, because the fan-out queue is sized from it and a
// second copy would let the two disagree about what "the default" is.
const DefaultMaxFPS = 60

func DefaultOptions() Options {
	return Options{
		MaxSize:             1080,
		MaxFPS:              DefaultMaxFPS,
		BitRate:             4_000_000,
		VideoCodec:          scrcpy.DefaultVideoCodec,
		KeyframeIntervalSec: 0,
	}
}

// Meta describes a live stream. It is known only after Open succeeds, because
// the device reports its own post-negotiation dimensions.
type Meta struct {
	// Codec is a fourcc-style identifier, currently always "h264".
	Codec string
	// Width and Height are the dimensions of the encoded video, which may differ
	// from the device's physical resolution when MaxSize forced a downscale.
	Width  int
	Height int
	// Name is a human-readable device label, e.g. "Pixel 8 Pro".
	Name string
}

// Frame is one encoded access unit in Annex B byte-stream format, ready to hand
// to a decoder without further framing.
type Frame struct {
	// PTS is the presentation timestamp reported by the target's encoder.
	PTS time.Duration
	// Key reports whether this access unit is independently decodable.
	Key bool
	// Config reports a parameter-set-only packet (SPS/PPS). These carry no
	// picture data and must be fed to the decoder as its configuration.
	Config bool
	// Data is the Annex B payload. Ownership passes to the receiver.
	Data []byte
	// Captured is the host clock reading when the frame left the target. It is
	// the origin of every latency number the client displays.
	Captured time.Time
}

// TouchAction mirrors the subset of Android's MotionEvent actions the control
// path needs. Multi-pointer bookkeeping is the target's problem, not the
// caller's: send plain Down/Up/Move with stable, distinct pointer ids and the
// implementation derives the pointer-indexed action.
type TouchAction uint8

const (
	TouchDown   TouchAction = 0
	TouchUp     TouchAction = 1
	TouchMove   TouchAction = 2
	TouchCancel TouchAction = 3
)

// KeyAction mirrors Android's KeyEvent actions.
type KeyAction uint8

const (
	KeyDown KeyAction = 0
	KeyUp   KeyAction = 1
)

// Android keycodes for the navigation keys a reviewer actually needs. The full
// table is large and uninteresting; add entries here as the client grows.
const (
	KeycodeHome       = 3
	KeycodeBack       = 4
	KeycodeAppSwitch  = 187
	KeycodePower      = 26
	KeycodeVolumeUp   = 24
	KeycodeVolumeDown = 25
)

// Touch is a single pointer event in stream coordinate space, meaning the
// coordinate system described by Meta.Width and Meta.Height rather than the
// device's physical pixels.
type Touch struct {
	Action    TouchAction
	PointerID uint64
	X, Y      int32
	// Pressure is normalized to [0,1]. A Down with zero pressure is treated as
	// a hover by Android, so implementations clamp it up.
	Pressure float32
}

// Scroll is a wheel or two-finger scroll gesture anchored at X,Y.
type Scroll struct {
	X, Y    int32
	HScroll float32
	VScroll float32
}

// Key is a hardware or navigation key event.
type Key struct {
	Action  KeyAction
	Keycode int32
}

// Stream is one live capture session against a target. All methods are safe for
// concurrent use except Close, which must be called exactly once.
type Stream interface {
	// Meta returns the negotiated stream parameters.
	Meta() Meta
	// Frames yields encoded access units in decode order. The channel is closed
	// when the stream ends; check Err afterwards to distinguish a clean stop
	// from a failure.
	Frames() <-chan Frame
	// Touch, Scroll and Key inject input. They return ErrNoControl on targets
	// that cannot accept input, such as the synthetic pattern.
	Touch(Touch) error
	Scroll(Scroll) error
	Key(Key) error
	// RequestKeyframe asks the encoder for a fresh keyframe. The viewer calls
	// it after shedding an inter-frame, because it cannot decode again until
	// one arrives and the device does not send them on a timer.
	RequestKeyframe() error
	// Err reports why the frame channel closed. It returns nil on a clean stop.
	Err() error
	// Close tears down the capture and releases the target.
	Close() error
}

// LiveBitRater is a Stream whose encoder can change bitrate while it runs.
// VideoToolbox can; scrcpy cannot, so a device changes bitrate by relaunching
// its encoder instead (session.Relaunch). LiveBitRate reports whether this
// stream can do it at all, since the capability can depend on the helper
// binary's build rather than on the type.
type LiveBitRater interface {
	LiveBitRate() bool
	SetBitRate(bps int) error
}

// LiveResizer is a Stream whose encoder can change the size it encodes at while
// it runs. The new size arrives as a new SPS and a keyframe, as a rotation
// does. LiveResize reports whether this stream can do it, which depends on the
// helper binary's build. SetMaxSize takes the longest edge in pixels, 0 for
// the size the stream was opened with, and never goes above that size.
type LiveResizer interface {
	LiveResize() bool
	SetMaxSize(px int) error
}

// Target is an addressable Android source that can be opened for streaming.
// The three implementations in this package differ only in how they reach an
// adb endpoint, or in the synthetic case, how they fake one.
type Target interface {
	// ID is a stable identifier, unique within a server process.
	ID() string
	// Kind classifies the target for display and defaulting.
	Kind() Kind
	// Label is a human-readable name for the session picker.
	Label() string
	// Open starts a capture. The returned Stream owns any resources allocated
	// here and releases them on Close.
	Open(ctx context.Context, opts Options) (Stream, error)
}

// TextTyper is implemented by streams that can type a string into whatever
// has focus, so a viewer typing on its own phone's keyboard costs one
// message per burst rather than a key pair per character. An Android stream
// sends scrcpy's inject-text; a simulator's types through its input process.
type TextTyper interface {
	Text(s string) error
}

// ErrNoControl is returned by Stream input methods on targets that cannot
// accept input, such as the synthetic test pattern.
var ErrNoControl = errors.New("target: this target does not accept input")
