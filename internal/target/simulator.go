package target

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bmcreations/loupe/internal/h264"
	"github.com/bmcreations/loupe/internal/scrcpy"
)

// SimInfo is one iOS simulator as `simctl list` reports it.
type SimInfo struct {
	UDID      string
	Name      string
	State     string // simctl's word: Booted, Shutdown, Booting, ...
	OSVersion string // "27.0"
	Form      string // phone or tablet
	// DeviceType is CoreSimulator's device type identifier, which is what
	// leads to the bezel art (see simframe.go).
	DeviceType string
}

// Booted reports whether simctl says the simulator is running.
func (s SimInfo) Booted() bool { return s.State == "Booted" }

// ListSimulators returns every available iOS simulator. Only iOS: simcap has
// been run against an iPhone and nothing else, and a watch or a TV has no
// touch screen for the rest of the viewer to drive.
func ListSimulators(ctx context.Context) ([]SimInfo, error) {
	out, err := exec.CommandContext(ctx, "xcrun", "simctl", "list", "-j", "devices", "available").Output()
	if err != nil {
		return nil, fmt.Errorf("simctl list: %w", err)
	}
	return parseSimctlList(out)
}

func parseSimctlList(raw []byte) ([]SimInfo, error) {
	var doc struct {
		Devices map[string][]struct {
			UDID        string `json:"udid"`
			Name        string `json:"name"`
			State       string `json:"state"`
			IsAvailable bool   `json:"isAvailable"`
			DeviceType  string `json:"deviceTypeIdentifier"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("simctl list: %w", err)
	}
	var out []SimInfo
	for runtime, devs := range doc.Devices {
		// com.apple.CoreSimulator.SimRuntime.iOS-27-0
		const prefix = "com.apple.CoreSimulator.SimRuntime.iOS-"
		if !strings.HasPrefix(runtime, prefix) {
			continue
		}
		version := strings.ReplaceAll(strings.TrimPrefix(runtime, prefix), "-", ".")
		for _, d := range devs {
			if !d.IsAvailable {
				continue
			}
			form := "phone"
			if strings.Contains(d.DeviceType, ".iPad") {
				form = "tablet"
			}
			out = append(out, SimInfo{UDID: d.UDID, Name: d.Name, State: d.State, OSVersion: version, Form: form, DeviceType: d.DeviceType})
		}
	}
	return out, nil
}

// Simulator is one iOS simulator, booted or not, streamed through
// simcap. simcap writes scrcpy's video wire format, so the stream is
// read with the same parser a device's is and nothing above this file knows
// the frames came from CoreSimulator.
//
// Input goes through a second simcap, in -input mode; see siminput.go.
type Simulator struct {
	simcap string

	inputOnce sync.Once
	in        *simInput

	hubMu      sync.Mutex
	hubChecked time.Time
	hubHeld    bool

	// claimed is set once this server has booted the simulator or taken its
	// input back from Device Hub. From then on a hold is taken back without
	// asking, since whoever is driving it is already doing so through here.
	// reclaimMu keeps two presses that both find it held from restarting
	// SpringBoard twice.
	claimed     atomic.Bool
	reclaimMu   sync.Mutex
	lastReclaim atomic.Pointer[ReclaimRecord]

	mu   sync.Mutex
	info SimInfo

	// frames is the rasterised bezel per panel, each rendered on first
	// request; framePanels is the screen IDs that have a bezel, read once.
	frameMu     sync.Mutex
	frames      map[int]*simFrameCache
	framePanels []int // nil until read

	// bootMu serializes boots, so two viewers opening a shut-down simulator at
	// once do not both run simctl boot.
	bootMu sync.Mutex
	booted atomic.Bool

	// slim reports, at each boot this target starts, whether the boot should
	// be slim (simslim.go). trace receives what that boot did. Nil skips both.
	// Both are guarded by bootMu.
	slim  func() bool
	trace func(map[string]any)

	// panels is the device type's built-in panel count once simcap has
	// answered it, and 0 before. Two makes it foldable (simposture.go).
	panels atomic.Int32
	// panelsRead is set once Describe has started the capabilities.plist
	// read, so a single-panel device type is read once, not every listing.
	panelsRead atomic.Bool

	capsOnce   sync.Once
	liveCaps   bool
	pbWatcher  bool
	liveResize bool

	// pbImage is whether simcap's -caps lists pasteboardimage, nil until
	// it has answered (simdrop.go). Guarded by pbImageMu.
	pbImageMu sync.Mutex
	pbImage   *bool
	// hitCap is whether simcap's -caps lists hit, nil until it has
	// answered (simhit.go). Guarded by pbImageMu.
	hitCap *bool

	// pbSet is the text a viewer last set on the pasteboard, so the watcher
	// does not hand it straight back to them as a copy on the simulator.
	pbSet atomic.Pointer[string]

	// shimmed is the pid of each app this server launched with the camera
	// shim, by bundle ID, so a launch only relaunches an app that does not
	// have it. Guarded by shimMu.
	shimMu  sync.Mutex
	shimmed map[string]int
}

// NewSimulator returns a target for info, streamed by the simcap binary at
// simcapPath.
func NewSimulator(info SimInfo, simcapPath string) *Simulator {
	s := &Simulator{simcap: simcapPath, info: info}
	s.booted.Store(info.Booted())
	return s
}

// SetBootHooks sets what a boot this target starts reads and reports: slim
// is asked at each boot whether to boot slim, trace gets one record of it.
func (s *Simulator) SetBootHooks(slim func() bool, trace func(map[string]any)) {
	s.bootMu.Lock()
	defer s.bootMu.Unlock()
	s.slim, s.trace = slim, trace
}

// SimulatorID is the target id for a simulator's UDID.
func SimulatorID(udid string) string { return "sim:" + udid }

func (s *Simulator) ID() string    { return SimulatorID(s.UDID()) }
func (s *Simulator) Kind() Kind    { return KindSimulator }
func (s *Simulator) Label() string { return s.Info().Name }

// UDID is the simulator's CoreSimulator id.
func (s *Simulator) UDID() string { return s.Info().UDID }

// Info is what simctl last said about this simulator.
func (s *Simulator) Info() SimInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

// Update records a fresh listing, so the drawer's "Boots on open" follows a
// simulator booted or shut down from Xcode.
func (s *Simulator) Update(info SimInfo) {
	s.mu.Lock()
	s.info = info
	s.mu.Unlock()
	s.booted.Store(info.Booted())
}

// BootsOnOpen implements Booter.
func (s *Simulator) BootsOnOpen() bool { return !s.booted.Load() }

// Describe implements Describer from the listing, with no subprocess: simctl
// already said everything else the picker shows. A Duo is foldable once
// simcap or the device type's capabilities.plist has counted its panels;
// the first Describe starts that read in the background and says phone.
func (s *Simulator) Describe(context.Context) (Description, error) {
	i := s.Info()
	d := Description{OS: "ios", OSVersion: i.OSVersion, Form: i.Form, Transport: "simulator"}
	s.frameMu.Lock()
	panels := len(s.framePanels)
	s.frameMu.Unlock()
	if s.panels.Load() > 1 || panels > 1 {
		d.Form = "foldable"
	} else if panels == 0 && i.DeviceType != "" && s.panelsRead.CompareAndSwap(false, true) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			s.chromePanels(ctx)
		}()
	}
	return d, nil
}

// Open boots the simulator if it is not running, then starts simcap on it.
// Nothing shuts it down afterwards, the same as an AVD: a simulator someone
// else booted is theirs, and one this server booted costs nothing to leave.
func (s *Simulator) Open(ctx context.Context, opts Options) (Stream, error) {
	if err := s.ensureBooted(ctx); err != nil {
		return nil, err
	}
	st, err := openSimcap(ctx, s.simcap, s.UDID(), s.Label(), opts)
	if err != nil {
		return nil, err
	}
	st.sim, st.in = s, s.input()
	st.live = s.liveBitRate(ctx)
	st.resize = s.liveResize
	if s.pbWatcher {
		st.clip = watchSimPasteboard(s.simcap, s.UDID(), s.echoOfSet)
	}
	return st, nil
}

// liveBitRate asks simcap's -caps once whether it takes a bitrate change on
// stdin. A build from before it would read the command's bytes as noise, and
// one of them could be 17 and force a keyframe, so an older build is left on
// the bitrate it was launched with.
func (s *Simulator) liveBitRate(ctx context.Context) bool {
	s.capsOnce.Do(func() {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, s.simcap, "-caps").Output()
		if err != nil {
			return
		}
		for _, c := range strings.Fields(string(out)) {
			switch c {
			case "bitrate":
				s.liveCaps = true
			case "pasteboard":
				s.pbWatcher = true
			case "resize":
				s.liveResize = true
			}
		}
	})
	return s.liveCaps
}

// Boot boots the simulator if it is not running and waits for SpringBoard,
// without opening a stream.
func (s *Simulator) Boot(ctx context.Context) error { return s.ensureBooted(ctx) }

func (s *Simulator) ensureBooted(ctx context.Context) error {
	s.bootMu.Lock()
	defer s.bootMu.Unlock()
	if s.booted.Load() {
		return nil
	}
	udid := s.UDID()
	wanted, slimRec := s.prepareSlim()
	// "Unable to boot device in current state: Booted" is the listing being
	// stale, not a failure; bootstatus below settles it either way.
	if out, err := exec.CommandContext(ctx, "xcrun", "simctl", "boot", udid).CombinedOutput(); err != nil &&
		!strings.Contains(string(out), "current state: Booted") {
		return fmt.Errorf("boot %s: %v: %s", s.Label(), err, strings.TrimSpace(string(out)))
	}
	// -b boots it if needed and returns once SpringBoard is up, which is
	// when the framebuffer shows the home screen rather than a boot logo.
	if out, err := exec.CommandContext(ctx, "xcrun", "simctl", "bootstatus", udid, "-b").CombinedOutput(); err != nil {
		return fmt.Errorf("wait for %s to boot: %v: %s", s.Label(), err, strings.TrimSpace(string(out)))
	}
	s.booted.Store(true)
	s.claimed.Store(true)
	if slimRec != nil {
		if len(wanted) > 0 {
			n, err := slimHonoured(ctx, udid, wanted)
			slimRec["honoured"] = n
			if err != nil {
				slimRec["error"] = err.Error()
			}
		}
		s.trace(slimRec)
	}
	s.reclaimAfterBoot(ctx)
	return nil
}

// prepareSlim sets the store for the boot about to start and returns the
// labels it should come up with disabled, plus the trace record to finish
// once it has. The store is read only at boot, so a simulator simctl does not
// list as shut down is left alone, as is a runtime that ignores the store.
func (s *Simulator) prepareSlim() ([]string, map[string]any) {
	if s.slim == nil {
		return nil, nil
	}
	info := s.Info()
	if info.State != "Shutdown" || !slimPersists(info.OSVersion) {
		return nil, nil
	}
	on := s.slim()
	wanted, err := setSlim(info.UDID, on)
	if s.trace == nil || (!on && wanted == nil && err == nil) {
		return wanted, nil
	}
	rec := map[string]any{
		"kind": "simulator", "action": "slim", "target": s.ID(),
		"on": on, "wanted": len(wanted),
	}
	if err != nil {
		rec["error"] = err.Error()
	}
	return wanted, rec
}

// simcapStartBudget bounds the wait for simcap's stream header. It was 625-790ms
// on an iPhone 17 simulator, so this is only reached when simcap is stuck.
const simcapStartBudget = 10 * time.Second

// simKeyframeIntervalSec is a simulator's GOP when Options leaves it at zero.
// Longer than Android's one second because simcap's keyframe request cannot
// get lost: it is a byte on simcap's stdin, answered within 17ms. Behind a 6
// Mbps, 30ms shaped link a 5s GOP cut hitches from 21 to 6 in 22s for the
// same bytes; at 1s the five frames after each keyframe landed 65-72ms late.
const simKeyframeIntervalSec = 5

func openSimcap(ctx context.Context, bin, udid, name string, opts Options, extra ...string) (*simStream, error) {
	args := []string{udid}
	if opts.BitRate > 0 {
		args = append(args, "-bitrate", fmt.Sprint(opts.BitRate))
	}
	if opts.MaxFPS > 0 {
		args = append(args, "-fps", fmt.Sprint(opts.MaxFPS))
	}
	// Without it a simulator streamed its whole framebuffer, 1206x2622 on an
	// iPhone 17, against 484x1080 for an Acme One under the same setting.
	if opts.MaxSize > 0 {
		args = append(args, "-maxsize", fmt.Sprint(opts.MaxSize))
	}
	args = append(args, extra...)
	// After extra: simcap reads -phone at args[1] once it has taken the flags
	// it knows, so a build without -keyint must still find it there.
	keyint := opts.KeyframeIntervalSec
	if keyint <= 0 {
		keyint = simKeyframeIntervalSec
	}
	args = append(args, "-keyint", fmt.Sprint(keyint))
	// Not CommandContext: ctx is the open request's, and the stream outlives
	// it. Close ends simcap by closing its stdin.
	cmd := exec.Command(bin, args...)
	// Its own process group, so Close's kill reaches the child -phone
	// re-spawns for TCC (disclaimResponsibility) as well: a SIGKILL to the
	// parent alone left that child holding the phone's capture device.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderr := simcapStderr(udid)
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start simcap %s: %w", bin, err)
	}

	s := &simStream{
		cmd:    cmd,
		stdin:  stdin,
		out:    stdout,
		stderr: stderr,
		frames: make(chan Frame, 64),
		exited: make(chan struct{}),
	}
	go func() { _ = cmd.Wait(); close(s.exited) }()

	type result struct {
		hdr scrcpy.StreamHeader
		err error
	}
	got := make(chan result, 1)
	go func() {
		hdr, err := scrcpy.ReadStreamHeader(stdout)
		got <- result{hdr, err}
	}()
	var r result
	select {
	case r = <-got:
	case <-ctx.Done():
		r.err = ctx.Err()
	case <-time.After(simcapStartBudget):
		r.err = fmt.Errorf("no stream header in %s", simcapStartBudget)
	}
	if r.err != nil {
		s.Close()
		return nil, fmt.Errorf("simcap %s: %w%s", udid, r.err, stderr.suffix())
	}
	if r.hdr.DeviceName != "" {
		name = r.hdr.DeviceName
	}
	s.meta = Meta{Codec: r.hdr.Codec, Width: r.hdr.Width, Height: r.hdr.Height, Name: name}
	go s.pump()
	return s, nil
}

// simStream is simcap's stdout read as a scrcpy video socket.
type simStream struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    io.Reader
	stderr *tailBuffer
	frames chan Frame
	exited chan struct{}

	metaMu sync.RWMutex
	meta   Meta

	inMu sync.Mutex // serializes writes to stdin

	errMu sync.Mutex
	err   error

	closeOnce sync.Once
	closing   atomic.Bool

	// sim and in are the device's input, shared with /api/input: a
	// simulator's or a physical iPhone's. Nil when the stream was opened
	// without one.
	sim simcapInputOwner
	in  *simInput
	// route, when set, is what input lines go through instead of in: a
	// physical iPhone's, which sends touches below iOS 27 to the XCUITest
	// runner. oneFinger drops a second finger, which that runner lacks.
	route     func(ctx context.Context, budget time.Duration, format string, a ...any) (string, error)
	oneFinger bool

	// live is whether this simcap build takes simcapSetBitRate on stdin.
	live bool
	// resize is whether it takes simcapSetMaxSize, and maxSize the last size
	// sent, 0 for the one it was opened with. maxSize is guarded by inMu.
	resize  bool
	maxSize int

	// clip is the pasteboard watcher running beside the video, nil for a
	// simcap build without one.
	clip *simPasteboard

	touchMu sync.Mutex
	fingers []finger // down, in the order they landed; at most two

	// screenW and screenH are the framebuffer's size as simcap's `size`
	// answered it, read against a picture of forW x forH. Under touchMu.
	screenW, screenH int
	forW, forH       int
}

type finger struct {
	id   uint64
	x, y int32
}

func (s *simStream) Meta() Meta {
	s.metaMu.RLock()
	defer s.metaMu.RUnlock()
	return s.meta
}

func (s *simStream) Frames() <-chan Frame { return s.frames }

func (s *simStream) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *simStream) pump() {
	defer close(s.frames)
	for {
		ph, err := scrcpy.ReadPacketHeader(s.out)
		if err == nil {
			buf := make([]byte, ph.Size)
			if _, err = io.ReadFull(s.out, buf); err == nil {
				if ph.Config {
					s.noteConfig(buf)
				}
				s.frames <- Frame{PTS: ph.PTS, Key: ph.Key, Config: ph.Config, Data: buf, Captured: time.Now()}
				continue
			}
		}
		if !s.closing.Load() {
			<-s.exited
			s.errMu.Lock()
			s.err = fmt.Errorf("simcap ended: %w%s", err, s.stderr.suffix())
			s.errMu.Unlock()
		}
		return
	}
}

// noteConfig follows the picture's size from each parameter set, as a
// device stream does: simcap starts a new encoder session when the
// framebuffer changes shape and sends a fresh SPS rather than a new header.
func (s *simStream) noteConfig(data []byte) {
	sps := h264.FindSPS(data)
	if sps == nil {
		return
	}
	w, h, err := h264.SPSDimensions(sps)
	if err != nil {
		return
	}
	s.metaMu.Lock()
	s.meta.Width, s.meta.Height = w, h
	s.metaMu.Unlock()
}

// RequestKeyframe sends simcap scrcpy's TYPE_RESET_VIDEO byte.
func (s *simStream) RequestKeyframe() error {
	var buf [scrcpy.ResetVideoMessageLen]byte
	n, err := scrcpy.EncodeResetVideo(buf[:])
	if err != nil {
		return err
	}
	s.inMu.Lock()
	defer s.inMu.Unlock()
	_, err = s.stdin.Write(buf[:n])
	return err
}

// simcapSetBitRate is simcap's stdin command for a live bitrate change: this
// byte, then the rate as a big-endian u32 in bits per second. It sits outside
// scrcpy's opcodes, 0 to 22 as of 4.1, so it cannot be mistaken for one.
const simcapSetBitRate = 0x80

func (s *simStream) LiveBitRate() bool { return s.live }

// SetBitRate changes the encoder's average bitrate without restarting it, so
// no viewer waits for a keyframe and nothing overlaps.
func (s *simStream) SetBitRate(bps int) error {
	if !s.live {
		return fmt.Errorf("simcap %s: %w", s.cmd.Path, errors.ErrUnsupported)
	}
	if bps <= 0 || bps > math.MaxUint32 {
		return fmt.Errorf("simcap: bitrate %d out of range", bps)
	}
	var buf [5]byte
	buf[0] = simcapSetBitRate
	binary.BigEndian.PutUint32(buf[1:], uint32(bps))
	s.inMu.Lock()
	defer s.inMu.Unlock()
	_, err := s.stdin.Write(buf[:])
	return err
}

// simcapSetMaxSize is simcap's stdin command for a live size change: this
// byte, then the longest edge as a big-endian u16, 0 for the -maxsize flag's.
const simcapSetMaxSize = 0x81

func (s *simStream) LiveResize() bool { return s.resize }

// SetMaxSize changes the longest edge simcap encodes at. simcap rebuilds its
// encoder only when the size changes, and the next frame is a keyframe with
// the new SPS; noteConfig and the input rescale follow it as they do a
// rotation. A size equal to the last one sent writes nothing.
func (s *simStream) SetMaxSize(px int) error {
	if !s.resize {
		return fmt.Errorf("simcap %s: %w", s.cmd.Path, errors.ErrUnsupported)
	}
	if px < 0 || px > math.MaxUint16 {
		return fmt.Errorf("simcap: maxsize %d out of range", px)
	}
	s.inMu.Lock()
	defer s.inMu.Unlock()
	if px == s.maxSize {
		return nil
	}
	var buf [3]byte
	buf[0] = simcapSetMaxSize
	binary.BigEndian.PutUint16(buf[1:], uint16(px))
	if _, err := s.stdin.Write(buf[:]); err != nil {
		return err
	}
	s.maxSize = px
	return nil
}

// Close ends simcap by closing its stdin, which it exits on in under 10ms, and
// kills it if it has not gone within a second.
func (s *simStream) Close() error {
	s.closeOnce.Do(func() {
		s.closing.Store(true)
		if s.clip != nil {
			s.clip.stop()
		}
		s.inMu.Lock()
		_ = s.stdin.Close()
		s.inMu.Unlock()
		select {
		case <-s.exited:
		case <-time.After(time.Second):
			_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
			<-s.exited
		}
	})
	return nil
}

// simcapLog is where simcap's stderr is echoed: the server's own stderr,
// which is loupe-run.log under launchd.
var simcapLog io.Writer = os.Stderr

// simcapStderr is a simcap process's stderr: kept for errors and echoed to
// simcapLog a line at a time under the device's id. Kept only, a -phone
// waiting on the camera prompt said so nowhere a person could read while
// it waited.
func simcapStderr(id string) *tailBuffer {
	return &tailBuffer{max: 4096, echo: simcapLog, id: id}
}

// tailBuffer keeps the last max bytes written to it, so an error can quote
// what simcap said last without holding a long session's log.
type tailBuffer struct {
	mu   sync.Mutex
	max  int
	b    []byte
	echo io.Writer // nil keeps the tail only
	id   string
	part []byte // an echoed line not yet ended
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if over := len(t.b) - t.max; over > 0 {
		t.b = t.b[over:]
	}
	if t.echo != nil {
		t.part = append(t.part, p...)
		for {
			i := bytes.IndexByte(t.part, '\n')
			if i < 0 {
				break
			}
			line := strings.TrimPrefix(string(t.part[:i]), "simcap: ")
			fmt.Fprintf(t.echo, "simcap %s: %s\n", t.id, line)
			t.part = t.part[i+1:]
		}
		if len(t.part) > t.max {
			t.part = t.part[:0]
		}
	}
	return len(p), nil
}

// suffix is the last line simcap wrote, as ": line", or "".
func (t *tailBuffer) suffix() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(string(t.b)), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		return ": " + last
	}
	return ""
}

var (
	_ Target    = (*Simulator)(nil)
	_ Booter    = (*Simulator)(nil)
	_ Describer = (*Simulator)(nil)

	_ Actuator      = (*Simulator)(nil)
	_ ButtonPresser = (*Simulator)(nil)
	_ ScreenSizer   = (*Simulator)(nil)
	_ Screenshotter = (*Simulator)(nil)
	_ Inspector     = (*Simulator)(nil)
	_ Launcher      = (*Simulator)(nil)
	_ URLOpener     = (*Simulator)(nil)
	_ Stream        = (*simStream)(nil)
	_ LiveBitRater  = (*simStream)(nil)
	_ LiveResizer   = (*simStream)(nil)
)
