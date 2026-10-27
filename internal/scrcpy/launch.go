package scrcpy

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// devicePath is where the server jar is staged on the device. It matches
// upstream scrcpy so a stale jar from a normal scrcpy run gets overwritten
// rather than sitting alongside ours.
const devicePath = "/data/local/tmp/scrcpy-server.jar"

// Config describes one scrcpy-server launch.
type Config struct {
	// ADBPath is the adb binary to invoke. Empty means "adb" on PATH.
	ADBPath string
	// Serial selects the device. Empty means adb's default, which fails when
	// more than one device is attached, so callers should always set it.
	Serial string
	// ServerJar is the host path to scrcpy-server matching ServerVersion.
	ServerJar string
	// ServerVersion must equal the jar's version string exactly. The server
	// aborts on mismatch, which surfaces here as a connect timeout rather than
	// a useful message, so it is worth getting right.
	ServerVersion string

	// VideoCodec selects the device-side encoder. Empty means DefaultVideoCodec.
	// h265 and av1 are accepted by this server build, but nothing here has
	// verified that the browser decodes them, so changing it is an experiment.
	VideoCodec string
	// KeyframeIntervalSec bounds the GOP, in seconds. Zero means
	// DefaultKeyframeIntervalSec.
	KeyframeIntervalSec int

	MaxSize int
	MaxFPS  int
	BitRate int

	// LogLevel is scrcpy-server's own log level. Empty means
	// DefaultLogLevel. "verbose" is the only way to see the server report an
	// input event it discarded, which is what cmd/touchrotprobe reads.
	LogLevel string
}

// DefaultVideoCodec is the one codec the client is known to decode.
const DefaultVideoCodec = "h264"

// DefaultKeyframeIntervalSec bounds the GOP at one second.
//
// scrcpy does not emit keyframes on a timer and the encoder's own default
// interval is effectively unbounded: an idle emulator produced exactly one
// keyframe in six seconds, the initial one. Every place loupe holds frames
// back until a keyframe arrives -- the viewer loop when it sheds, the fan-out
// when a viewer's channel fills, a viewer that has just joined mid-GOP -- then
// waits forever rather than stalling, because nothing was ever going to
// arrive. Each of those paths asks for a keyframe explicitly, but that request
// travels over the control socket and can fail; this is the floor underneath
// them, and it costs one extra I-frame per second of bitrate.
const DefaultKeyframeIntervalSec = 1

// DefaultLogLevel is what scrcpy-server logs at.
//
// "verbose" additionally reports every input event the server discarded, which
// it does silently otherwise: an event whose screen size does not match the
// encoder's current one is dropped rather than misplaced, and at info level the
// only symptom is a device that stops responding. cmd/touchrotprobe raises it
// for exactly that reason. It is too noisy to leave on for a real session.
const DefaultLogLevel = "info"

// withDefaults fills the fields that have one, so a zero Config still
// describes a working launch and serverOptions can stay free of fallbacks.
func (c Config) withDefaults() Config {
	if c.ADBPath == "" {
		c.ADBPath = "adb"
	}
	if c.VideoCodec == "" {
		c.VideoCodec = DefaultVideoCodec
	}
	if c.KeyframeIntervalSec <= 0 {
		c.KeyframeIntervalSec = DefaultKeyframeIntervalSec
	}
	if c.LogLevel == "" {
		c.LogLevel = DefaultLogLevel
	}
	return c
}

// serverOptions is every option Launch passes to scrcpy-server, as key=value
// in the order the server reads them.
//
// It is a function rather than an inline slice so the names can be checked
// against the jar that has to parse them, without a second copy of the list to
// drift. scrcpy warns on an unknown option and carries on, so a rename lands
// as changed behaviour and no error at all.
func serverOptions(cfg Config, scid string) []string {
	return []string{
		"scid=" + scid,
		"log_level=" + cfg.LogLevel,
		"tunnel_forward=true",
		"control=true",
		"cleanup=true",
		"audio=false",
		"video=true",
		"send_device_meta=true",
		"send_frame_meta=true",
		"send_codec_meta=true",
		"send_dummy_byte=true",
		// The default, spelled out because target.ClipboardWatcher depends
		// on it: the server sends the clipboard each time it changes.
		"clipboard_autosync=true",
		"video_codec=" + cfg.VideoCodec,
		"video_codec_options=i-frame-interval:int=" + strconv.Itoa(cfg.KeyframeIntervalSec),
		"max_size=" + strconv.Itoa(cfg.MaxSize),
		"max_fps=" + strconv.Itoa(cfg.MaxFPS),
		"video_bit_rate=" + strconv.Itoa(cfg.BitRate),
	}
}

// OptionNames returns the option keys Launch sends. It exists so the package
// that embeds the jar can assert them against its dex.
func OptionNames() []string {
	var names []string
	for _, opt := range serverOptions(Config{}.withDefaults(), "0") {
		k, _, _ := strings.Cut(opt, "=")
		names = append(names, k)
	}
	return names
}

// Frame is one encoded access unit read off the video socket.
type Frame struct {
	PTS      time.Duration
	Key      bool
	Config   bool
	Data     []byte
	Captured time.Time
}

// Session is a running scrcpy-server with its video and control sockets open.
type Session struct {
	cfg       Config
	header    StreamHeader
	frames    chan Frame
	clipboard chan string
	video     net.Conn
	control   net.Conn
	shell     *exec.Cmd
	localSock string

	controlMu sync.Mutex

	closeOnce sync.Once
	closed    chan struct{}

	errMu sync.Mutex
	err   error
}

// Launch pushes the server jar, starts it on the device, and connects both
// sockets. On success the caller owns the Session and must Close it.
//
// The ctx governs setup only. Once Launch returns, the stream runs until Close
// or until the device goes away.
func Launch(ctx context.Context, cfg Config) (*Session, error) {
	cfg = cfg.withDefaults()
	if cfg.ServerJar == "" {
		return nil, errors.New("scrcpy: ServerJar is required")
	}
	if cfg.ServerVersion == "" {
		return nil, errors.New("scrcpy: ServerVersion is required and must match the jar")
	}

	s := &Session{
		cfg:       cfg,
		frames:    make(chan Frame, 64),
		clipboard: make(chan string, clipboardQueue),
		closed:    make(chan struct{}),
	}

	if out, err := s.adb(ctx, "push", cfg.ServerJar, devicePath); err != nil {
		return nil, fmt.Errorf("push server jar: %w (%s)", err, strings.TrimSpace(out))
	}

	scid, err := newSCID()
	if err != nil {
		return nil, err
	}
	socketName := "localabstract:scrcpy_" + scid

	sock := forwardSocket(scid)
	s.localSock = sock
	// A leftover from a killed run would make adb refuse to bind.
	_ = os.Remove(sock)
	local := "localfilesystem:" + sock

	if out, err := s.adb(ctx, "forward", local, socketName); err != nil {
		return nil, fmt.Errorf("adb forward: %w (%s)", err, strings.TrimSpace(out))
	}
	// From here on any failure must undo the forward, or the port leaks for the
	// lifetime of the adb server.
	cleanupForward := func() {
		_, _ = s.adb(context.Background(), "forward", "--remove", local)
		_ = os.Remove(sock)
	}

	args := append(s.adbArgs(
		"shell",
		"CLASSPATH="+devicePath,
		"app_process", "/", "com.genymobile.scrcpy.Server",
		cfg.ServerVersion,
	), serverOptions(cfg, scid)...)
	shell := exec.Command(cfg.ADBPath, args...)
	shellOut, err := shell.StdoutPipe()
	if err != nil {
		cleanupForward()
		return nil, err
	}
	shell.Stderr = shell.Stdout
	if err := shell.Start(); err != nil {
		cleanupForward()
		return nil, fmt.Errorf("start scrcpy server: %w", err)
	}
	s.shell = shell
	// The server's own log is the only place a version mismatch or a permission
	// failure is explained, so keep it flowing rather than letting the pipe fill
	// and block the server.
	go drainServerLog(shellOut)

	// The server needs a moment between process start and socket listen. Dial in
	// a retry loop rather than sleeping a fixed amount, which is either slow or
	// flaky depending on the device.
	// send_dummy_byte makes the first accepted socket carry one leading byte,
	// whose only purpose is to prove the tunnel is real before the server does
	// any expensive setup. Connecting and consuming it is one operation: see
	// dialVideo for why they cannot be separated.
	video, err := dialVideo(ctx, sock, 15*time.Second)
	if err != nil {
		_ = shell.Process.Kill()
		cleanupForward()
		// The version hint is only earned when the dial itself failed. Safari
		// routinely opens a WebTransport connection, abandons it and opens
		// another, which cancels the acquire already running for the first; the
		// hint then sends the reader to check a jar that is working fine.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("connect video socket: %w (caller went away before the server was reachable)", err)
		}
		return nil, fmt.Errorf("connect video socket: %w (server may have exited; check version %q matches the jar)", err, cfg.ServerVersion)
	}
	s.video = video

	control, err := dialRetry(ctx, sock, 5*time.Second)
	if err != nil {
		_ = video.Close()
		_ = shell.Process.Kill()
		cleanupForward()
		return nil, fmt.Errorf("connect control socket: %w", err)
	}
	s.control = control

	// The forward has done its job once both sockets are established.
	cleanupForward()

	// Read the header straight off the socket: ReadStreamHeader uses exact-size
	// reads, so no buffering layer can swallow frame bytes behind it.
	header, err := ReadStreamHeader(video)
	if err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("read stream header: %w", err)
	}
	s.header = header

	go s.readFrames()
	go s.readDeviceMessages()
	return s, nil
}

// clipboardQueue bounds the device clipboards waiting for a reader. Only the
// newest matters, so a full queue drops its oldest.
const clipboardQueue = 4

// Clipboard yields the device's clipboard each time the server sends it:
// when it changes on the device (clipboard_autosync), and in answer to a
// get-clipboard message. It closes when the control socket does.
func (s *Session) Clipboard() <-chan string { return s.clipboard }

// offerLatest sends v on ch, dropping the oldest queued value to make room.
// This goroutine is the only sender, so one drop is always enough.
func offerLatest(ch chan string, v string) {
	select {
	case ch <- v:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- v:
	default:
	}
}

// readDeviceMessages reads what the server writes up the control socket.
//
// Nothing read it before clipboards were wanted, which was harmless only
// because nothing the server sends was ever asked for. A socket nobody reads
// fills its buffer and then blocks the server's sender thread.
func (s *Session) readDeviceMessages() {
	defer close(s.clipboard)
	r := bufio.NewReader(s.control)
	for {
		m, err := ReadDeviceMessage(r)
		if err != nil {
			if !isExpectedClose(err, s.closed) && !errors.Is(err, io.EOF) {
				fmt.Printf("[scrcpy] control socket: %v\n", err)
			}
			return
		}
		if !m.IsClipboard() {
			continue
		}
		offerLatest(s.clipboard, m.Text)
	}
}

// Header returns the stream metadata read during Launch.
func (s *Session) Header() StreamHeader { return s.header }

// Frames yields encoded access units until the stream ends.
func (s *Session) Frames() <-chan Frame { return s.frames }

// SendControl writes a pre-encoded control message to the device.
func (s *Session) SendControl(msg []byte) error {
	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	if s.control == nil {
		return errors.New("scrcpy: control socket not open")
	}
	// A slow or wedged control socket must not stall the caller's request
	// handler indefinitely; input that is seconds late is worse than dropped.
	_ = s.control.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err := s.control.Write(msg)
	return err
}

// Err reports why the frame channel closed, or nil on a clean shutdown.
func (s *Session) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *Session) setErr(err error) {
	s.errMu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.errMu.Unlock()
}

// Close tears down the session. It is safe to call more than once.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		if s.video != nil {
			_ = s.video.Close()
		}
		if s.control != nil {
			s.controlMu.Lock()
			_ = s.control.Close()
			s.controlMu.Unlock()
		}
		if s.shell != nil && s.shell.Process != nil {
			_ = s.shell.Process.Kill()
			// Reap so the adb child does not linger as a zombie for the life of
			// the server process.
			go func() { _ = s.shell.Wait() }()
		}
	})
	return nil
}

// readFrames is the video socket pump. It owns s.frames and closes it on exit.
func (s *Session) readFrames() {
	defer close(s.frames)
	r := bufio.NewReaderSize(s.video, 1<<20)
	for {
		select {
		case <-s.closed:
			return
		default:
		}

		hdr, err := ReadPacketHeader(r)
		if err != nil {
			if !isExpectedClose(err, s.closed) {
				s.setErr(fmt.Errorf("read packet header: %w", err))
			}
			return
		}
		// A corrupt header reads as an enormous size. Reject it here rather than
		// attempting a multi-gigabyte allocation.
		if hdr.Size == 0 || hdr.Size > 32<<20 {
			s.setErr(fmt.Errorf("implausible packet size %d, stream desynchronized", hdr.Size))
			return
		}

		data := make([]byte, hdr.Size)
		if _, err := io.ReadFull(r, data); err != nil {
			if !isExpectedClose(err, s.closed) {
				s.setErr(fmt.Errorf("read packet body: %w", err))
			}
			return
		}

		frame := Frame{
			PTS:      hdr.PTS,
			Key:      hdr.Key,
			Config:   hdr.Config,
			Data:     data,
			Captured: time.Now(),
		}

		select {
		case s.frames <- frame:
		case <-s.closed:
			return
		default:
			// The consumer is behind. Dropping a frame here is correct: this is a
			// live stream, and a backlog would show up as growing latency that
			// never recovers. Parameter sets and keyframes are the exception,
			// since dropping those corrupts everything after them.
			if frame.Config || frame.Key {
				select {
				case s.frames <- frame:
				case <-s.closed:
					return
				}
			}
		}
	}
}

func (s *Session) adbArgs(args ...string) []string {
	if s.cfg.Serial != "" {
		return append([]string{"-s", s.cfg.Serial}, args...)
	}
	return args
}

func (s *Session) adb(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, s.cfg.ADBPath, s.adbArgs(args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// dialVideo connects the video socket and consumes the dummy byte, retrying
// both together.
//
// With tunnel_forward=true the host end of the forward is adbd, not the server.
// adbd accepts a TCP connection straight away and only then tries to reach
// localabstract:scrcpy_<scid> on the device; while the server is still starting
// that fails and adbd closes the connection. So a successful dial proves
// nothing, and reading the dummy byte outside the retry turns a normal startup
// delay into a hard failure about 200ms in -- long before the server would have
// been ready. The device says so plainly in logcat:
//
//	E adbd: failed to connect to socket 'localabstract:scrcpy_<scid>':
//	        could not connect to localabstract address
//
// Retrying is safe because the two outcomes are distinguishable. EOF means adbd
// rejected us and the server accepted nothing, so no server-side socket slot
// was consumed. A dummy byte means the server accepted, and that connection is
// the video socket and is kept.
func dialVideo(ctx context.Context, sock string, budget time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(budget)
	var lastErr error
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			if lastErr == nil {
				lastErr = errors.New("timed out waiting for the server to listen")
			}
			return nil, lastErr
		}
		conn, err := dialRetry(ctx, sock, remaining)
		if err != nil {
			return nil, err
		}
		// A short deadline, not the whole budget: the server writes the dummy
		// byte immediately once it has accepted, so anything slower is adbd
		// holding a connection it cannot complete.
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		var dummy [1]byte
		if _, err := io.ReadFull(conn, dummy[:]); err != nil {
			_ = conn.Close()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		_ = conn.SetReadDeadline(time.Time{})
		return conn, nil
	}
}

// dialRetry connects to the forwarded socket, retrying until the server is
// listening or the budget expires.
func dialRetry(ctx context.Context, sock string, budget time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(budget)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		// No Nagle to disable: a unix socket writes straight through, which is
		// the other reason this end does not want to be TCP.
		conn, err := net.DialTimeout("unix", sock, time.Second)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	return nil, lastErr
}

// forwardSocket names the host end of the adb forward.
//
// A unix socket rather than a loopback port, because the port was pure cost.
// Both ends are on this machine, so TCP bought nothing, and allocating one
// meant asking the kernel for a free port and then racing adb to bind it --
// a window trap 12 already recorded, where lsof reports a port free while the
// kernel is still tearing the listener down. It also made every launch depend
// on the host's ephemeral range, which trap 33 emptied: with 9 of 16384 ports
// left, freePort and the dial both failed and every target died at acquire.
// A path has no such range to exhaust, and the scid already makes it unique.
func forwardSocket(scid string) string {
	return filepath.Join(os.TempDir(), "loupe-scrcpy-"+scid+".sock")
}

// newSCID generates the server's session id, a 31-bit value rendered as eight
// hex digits. The high bit stays clear because the server parses it as a
// signed int.
func newSCID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[0] &= 0x7f
	return hex.EncodeToString(b[:]), nil
}

func drainServerLog(r io.ReadCloser) {
	defer r.Close()
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			fmt.Printf("[scrcpy-server] %s\n", line)
		}
	}
}

// isExpectedClose reports whether an error is just the consequence of our own
// Close, so shutdown does not get logged as a failure.
func isExpectedClose(err error, closed <-chan struct{}) bool {
	select {
	case <-closed:
		return true
	default:
	}
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed)
}
