package target

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ADB wraps the adb binary. It holds no state beyond the binary location, so
// the zero value works whenever adb is on PATH.
type ADB struct {
	// Path is the adb binary. Empty means "adb" on PATH.
	Path string
}

func (a ADB) bin() string {
	if a.Path == "" {
		return "adb"
	}
	return a.Path
}

// DeviceInfo is one entry from `adb devices -l`.
type DeviceInfo struct {
	Serial string
	// State is adb's own word for it: "device" means ready, "unauthorized"
	// means the user has not accepted the debugging prompt, "offline" means the
	// daemon sees it but cannot talk to it.
	State    string
	Model    string
	Emulator bool
	// USB is true when `adb devices -l` names a `usb:` port, which it does
	// for a cable and never for adb over Wi-Fi.
	USB bool
	// TransportID is adb's handle for this connection. It changes when the
	// same serial reconnects, which is the only way to tell an emulator
	// restarted on a reused port -- possibly a different AVD -- from the one
	// that was there before.
	TransportID string
	// AVD is the name of the AVD an emulator was booted from, filled in by
	// AVDResolver rather than parsed, since adb does not list it. Empty on a
	// physical device and on an emulator not yet resolved.
	AVD string
}

// transport is Description.Transport for a device adb lists.
func (d DeviceInfo) transport() string {
	switch {
	case d.Emulator:
		return "emulator"
	case d.USB:
		return "usb"
	default:
		return "wifi"
	}
}

// Ready reports whether the device can actually be streamed.
func (d DeviceInfo) Ready() bool { return d.State == "device" }

// Label renders a human-readable name, falling back to the serial when the
// model property is not exposed.
func (d DeviceInfo) Label() string {
	model := strings.ReplaceAll(d.Model, "_", " ")
	if model == "" {
		return d.Serial
	}
	return fmt.Sprintf("%s (%s)", model, d.Serial)
}

// Devices lists every attached device, including ones that are not ready.
// Callers decide whether to filter: an unauthorized device is worth showing in
// the UI precisely so the user learns they need to accept the prompt.
func (a ADB) Devices(ctx context.Context) ([]DeviceInfo, error) {
	out, err := exec.CommandContext(ctx, a.bin(), "devices", "-l").Output()
	if err != nil {
		return nil, fmt.Errorf("adb devices: %w", err)
	}
	return parseDevices(string(out))
}

// TrackDevices calls fn with the full device list every time adb's changes,
// starting with the list as it stands, until ctx ends or the adb server goes
// away. One long-lived connection rather than a poll: every `adb devices` is a
// fresh loopback connection, and a poll every few seconds is the churn trap 33
// ran the Mac out of ephemeral ports with.
//
// `track-devices -l` is missing from `adb help` and works on adb 35 regardless.
func (a ADB) TrackDevices(ctx context.Context, fn func([]DeviceInfo)) error {
	cmd := exec.CommandContext(ctx, a.bin(), "track-devices", "-l")
	// adb says why on stderr -- "protocol fault (couldn't read status):
	// Connection reset by peer" from a wedged server -- and exits 1, so
	// without it the error is only the exit status.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("adb track-devices: %w", err)
	}
	readErr := readTrackBlocks(out, fn)
	waitErr := cmd.Wait()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return readErr
	}
	if waitErr != nil {
		if msg := lastLine(stderr.String()); msg != "" {
			return fmt.Errorf("adb track-devices: %w: %s", waitErr, msg)
		}
		return fmt.Errorf("adb track-devices: %w", waitErr)
	}
	return io.EOF
}

// readTrackBlocks decodes track-devices output: each update is a four-digit
// hex length followed by that many bytes in `adb devices -l` format, without
// the header line. An empty list is "0000", and it matters most of all -- it
// is how the last device leaving is reported.
func readTrackBlocks(r io.Reader, fn func([]DeviceInfo)) error {
	br := bufio.NewReader(r)
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return err
		}
		n, err := strconv.ParseUint(string(hdr[:]), 16, 16)
		if err != nil {
			return fmt.Errorf("adb track-devices: bad length %q", hdr[:])
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(br, body); err != nil {
			return err
		}
		devices, err := parseDevices(string(body))
		if err != nil {
			return err
		}
		fn(devices)
	}
}

// parseDevices reads the `adb devices -l` format.
func parseDevices(out string) ([]DeviceInfo, error) {
	var devices []DeviceInfo
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "List of devices") || strings.HasPrefix(line, "*") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		d := DeviceInfo{
			Serial:   fields[0],
			State:    fields[1],
			Emulator: strings.HasPrefix(fields[0], "emulator-"),
		}
		// Remaining fields are key:value pairs. model is absent on an
		// unauthorized device.
		for _, f := range fields[2:] {
			k, v, ok := strings.Cut(f, ":")
			if !ok {
				continue
			}
			switch k {
			case "model":
				d.Model = v
			case "transport_id":
				d.TransportID = v
			case "usb":
				d.USB = true
			}
		}
		devices = append(devices, d)
	}
	return devices, sc.Err()
}

// WaitForBoot blocks until the device reports a completed boot, which is later
// than adb reporting "device". Streaming before boot completes yields a black
// frame stream with no error, so this wait is load bearing for emulators.
func (a ADB) WaitForBoot(ctx context.Context, serial string, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		out, err := a.Shell(ctx, serial, "getprop", "sys.boot_completed")
		if err == nil && strings.TrimSpace(out) == "1" {
			return nil
		}
		// Short, because a boot from a snapshot is about 2s and arrives with
		// sys.boot_completed already 1: a 500ms poll was a quarter of it.
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("device %s did not finish booting within %s", serial, budget)
}

// Shell runs a command on the device and returns combined output.
func (a ADB) Shell(ctx context.Context, serial string, args ...string) (string, error) {
	full := []string{}
	if serial != "" {
		full = append(full, "-s", serial)
	}
	full = append(full, "shell")
	full = append(full, args...)
	out, err := exec.CommandContext(ctx, a.bin(), full...).CombinedOutput()
	return string(out), err
}

// Push copies a host file to the device.
func (a ADB) Push(ctx context.Context, serial, local, remote string) error {
	out, err := exec.CommandContext(ctx, a.bin(), "-s", serial, "push", local, remote).CombinedOutput()
	if err != nil {
		return fmt.Errorf("adb push %s: %w: %s", remote, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Forward forwards a free host port to remote on the device (an adb
// forward spec such as "localabstract:name") and returns the port.
func (a ADB) Forward(ctx context.Context, serial, remote string) (int, error) {
	out, err := exec.CommandContext(ctx, a.bin(), "-s", serial, "forward", "tcp:0", remote).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("adb forward %s: %w: %s", remote, err, strings.TrimSpace(string(out)))
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("adb forward %s: unexpected output %q", remote, out)
	}
	return port, nil
}

// RemoveForward removes a forward made by Forward.
func (a ADB) RemoveForward(ctx context.Context, serial string, port int) error {
	out, err := exec.CommandContext(ctx, a.bin(), "-s", serial, "forward", "--remove", "tcp:"+strconv.Itoa(port)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("adb forward --remove %d: %w: %s", port, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ExecOut runs a command on the device and returns its stdout alone, with no
// pty translation and with stderr kept separate. Shell uses CombinedOutput,
// which is right for probing a property and wrong for anything structured: a
// single warning line from the device would be spliced into the middle of the
// payload and the parse would fail somewhere unrelated to the cause.
func (a ADB) ExecOut(ctx context.Context, serial string, args ...string) ([]byte, error) {
	full := []string{}
	if serial != "" {
		full = append(full, "-s", serial)
	}
	full = append(full, "exec-out")
	full = append(full, args...)

	cmd := exec.CommandContext(ctx, a.bin(), full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("adb exec-out %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return nil, fmt.Errorf("adb exec-out %s: %w", strings.Join(args, " "), err)
	}
	return stdout.Bytes(), nil
}

// AVDName is the name of the AVD a running emulator was booted from, which is
// what maps it back to a config.ini and from there to its skin.
//
// Read from a property rather than from `adb emu avd name`, which needs the
// emulator console and its own auth token; the property is on the path Shell
// already takes. It is empty on a physical device.
func (a ADB) AVDName(ctx context.Context, serial string) (string, error) {
	out, err := a.Shell(ctx, serial, "getprop", "ro.boot.qemu.avd_name")
	if err != nil {
		return "", fmt.Errorf("read avd name: %w", err)
	}
	name := strings.TrimSpace(out)
	if name == "" {
		return "", fmt.Errorf("device %s reports no avd name", serial)
	}
	return name, nil
}

// DisplaySize is the display's size in pixels.
//
// An override beats the physical size, because that is what the display
// actually is once `wm size` has been set and so what scrcpy captures.
func (a ADB) DisplaySize(ctx context.Context, serial string) (int, int, error) {
	out, err := a.Shell(ctx, serial, "wm", "size")
	if err != nil {
		return 0, 0, fmt.Errorf("read display size: %w", err)
	}
	w, h := 0, 0
	for _, line := range strings.Split(out, "\n") {
		label, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		dims := strings.SplitN(strings.TrimSpace(value), "x", 2)
		if len(dims) != 2 {
			continue
		}
		pw, err1 := strconv.Atoi(strings.TrimSpace(dims[0]))
		ph, err2 := strconv.Atoi(strings.TrimSpace(dims[1]))
		if err1 != nil || err2 != nil || pw <= 0 || ph <= 0 {
			continue
		}
		if strings.Contains(strings.ToLower(label), "override") {
			return pw, ph, nil
		}
		w, h = pw, ph
	}
	if w == 0 {
		return 0, 0, fmt.Errorf("no display size in %q", strings.TrimSpace(out))
	}
	return w, h, nil
}

// DisplayDensity is the display's density in dpi, with an override preferred
// for the same reason DisplaySize prefers one.
func (a ADB) DisplayDensity(ctx context.Context, serial string) (int, error) {
	out, err := a.Shell(ctx, serial, "wm", "density")
	if err != nil {
		return 0, fmt.Errorf("read display density: %w", err)
	}
	density := 0
	for _, line := range strings.Split(out, "\n") {
		label, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		d, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || d <= 0 {
			continue
		}
		if strings.Contains(strings.ToLower(label), "override") {
			return d, nil
		}
		density = d
	}
	if density == 0 {
		return 0, fmt.Errorf("no display density in %q", strings.TrimSpace(out))
	}
	return density, nil
}

// CutoutSpec is the display's cutout, in Android's own spec language, or an
// error when the device declares none.
//
// The spec never contains a brace -- it is an SVG path plus anchor and unit
// suffixes -- so the first closing brace ends it.
func (a ADB) CutoutSpec(ctx context.Context, serial string) (string, error) {
	out, err := a.Shell(ctx, serial, "dumpsys", "window", "displays")
	if err != nil {
		return "", fmt.Errorf("read display cutout: %w", err)
	}
	spec, ok := firstCutoutSpec(out)
	if !ok {
		return "", fmt.Errorf("device %s declares no cutout", serial)
	}
	return spec, nil
}

// firstCutoutSpec returns the first non-empty cutoutSpec in a dumpsys window
// dump.
//
// Non-empty is the whole point. A dump carries one entry per display and the
// blank ones come first as often as not -- an Acme One reports cutoutSpec={}
// ahead of its real notch -- so reading the first match alone silently drops
// the cutout on a device that plainly declares one.
func firstCutoutSpec(dump string) (string, bool) {
	rest := dump
	for {
		_, after, ok := strings.Cut(rest, "cutoutSpec={")
		if !ok {
			return "", false
		}
		spec, tail, ok := strings.Cut(after, "}")
		if !ok {
			return "", false
		}
		if spec = strings.TrimSpace(spec); spec != "" {
			return spec, true
		}
		rest = tail
	}
}

// DisplayRotation is the display's current rotation as a quarter-turn count,
// 0 to 3, matching Android's Surface.ROTATION_* constants.
//
// This is the one question the video stream cannot answer. A rotated encoder
// reports 1080x480 whether the device was turned left or right (trap 42), and
// the bezel has to be turned the same way the device was or the punch hole
// lands on the wrong edge.
func (a ADB) DisplayRotation(ctx context.Context, serial string) (int, error) {
	out, err := a.Shell(ctx, serial, "dumpsys", "window", "displays")
	if err != nil {
		return 0, fmt.Errorf("read display rotation: %w", err)
	}
	rotation, ok := displayRotation(out)
	if !ok {
		return 0, fmt.Errorf("device %s reports no display rotation", serial)
	}
	return rotation, nil
}

// displayRotation returns the rotation of display 0, as a quarter-turn count
// matching Android's Surface.ROTATION_* constants, from a dumpsys window
// displays dump.
//
// Three things in the dump look like the answer and are not.
//
// The first Display block is not display 0. emulator-5554 lists
// mDisplayId=1084 first -- a 1dpi 1080x480 surface that stays at ROTATION_0
// while the real screen turns -- so reading the first rotation returns a
// display nobody is watching. That is trap 39's shape, and it is also what
// left trap 42 recording this dump as an instrument not worth using: the
// cur=1080x480 it saw disagreeing with /api/ui was 1084's, not the screen's.
// Display 0 is the one scrcpy streams unless it is told otherwise, so it is
// the one whose rotation the picture follows.
//
// Virtual displays read "undefined" rather than a rotation, which the
// ROTATION_ in the key skips.
//
// And the policy block lists the device's rotation *constants*:
//
//	mLandscapeRotation=ROTATION_90 mSeascapeRotation=ROTATION_270
//	mPortraitRotation=ROTATION_0 mUpsideDownRotation=ROTATION_180
//
// Those are fixed for the device and say nothing about how it is held, so the
// key has to be the whole field name. Matching "Rotation=ROTATION_" reads
// ROTATION_90 off a portrait device the moment the ordering puts that block
// first, and the 1084 entry is the standing evidence that the ordering moves.
func displayRotation(dump string) (int, bool) {
	rest, ok := afterDisplayZero(dump)
	if !ok {
		return 0, false
	}
	const key = "mDisplayRotation=ROTATION_"
	i := strings.Index(rest, key)
	if i < 0 {
		return 0, false
	}
	switch {
	case strings.HasPrefix(rest[i+len(key):], "270"):
		return 3, true
	case strings.HasPrefix(rest[i+len(key):], "180"):
		return 2, true
	case strings.HasPrefix(rest[i+len(key):], "90"):
		return 1, true
	case strings.HasPrefix(rest[i+len(key):], "0"):
		return 0, true
	}
	return 0, false
}

// ScreenGeometry reads display 0's natural (unrotated) size and its current
// rotation from one dumpsys window displays call, so ADBTarget.ScreenSize
// costs a single adb connection instead of two (one for `wm size`, one for
// the rotation) and the two answers cannot disagree because they were read
// at different moments -- across a `wm size` override landing between them,
// or a different AVD booting on the same serial between calls. Both readings
// come off the same afterDisplayZero block DisplayRotation and CutoutSpec
// already use, so the decoy display trap 44 documents (a 1dpi surface that
// prints first and never rotates) cannot answer for either.
//
// rotErr and err are separate because a failed rotation read does not have
// to fail the whole call -- ADBTarget.ScreenSize can still answer for a
// square screen without knowing which way it is held -- while a missing
// natural size has nothing to fall back to.
//
// Confirmed live against emulator-5554 (read-only, not rotated):
// `init=1080x2424 420dpi ... cur=2424x1080` appears under `mDisplayId=0
// (organized)`, agreeing with the captured dump in skin_rotation_test.go and
// with trap 44's note. `wm size` is not read here for that reason -- it is
// kept, uncached, in DisplaySize for derivedFrame's override-aware read,
// which is a different question (what the display currently is, override
// included) from ScreenSize's (what it naturally is, to swap by rotation).
func (a ADB) ScreenGeometry(ctx context.Context, serial string) (w, h, rotation int, rotErr, err error) {
	out, err := a.Shell(ctx, serial, "dumpsys", "window", "displays")
	if err != nil {
		return 0, 0, 0, nil, fmt.Errorf("read display geometry: %w", err)
	}
	w, h, ok := naturalSize(out)
	if !ok {
		return 0, 0, 0, nil, fmt.Errorf("device %s reports no display 0 natural size", serial)
	}
	rotation, ok = displayRotation(out)
	if !ok {
		rotErr = fmt.Errorf("device %s reports no display rotation", serial)
	}
	return w, h, rotation, rotErr, nil
}

// naturalSize is display 0's natural, unrotated size: the "init=WxH" pair
// dumpsys prints once per display, read off the same block afterDisplayZero
// isolates for displayRotation so the decoy display cannot answer for this
// either.
func naturalSize(dump string) (int, int, bool) {
	rest, ok := afterDisplayZero(dump)
	if !ok {
		return 0, 0, false
	}
	const key = "init="
	i := strings.Index(rest, key)
	if i < 0 {
		return 0, 0, false
	}
	rest = rest[i+len(key):]
	if end := strings.IndexAny(rest, " \n"); end >= 0 {
		rest = rest[:end]
	}
	dims := strings.SplitN(rest, "x", 2)
	if len(dims) != 2 {
		return 0, 0, false
	}
	w, err1 := strconv.Atoi(dims[0])
	h, err2 := strconv.Atoi(dims[1])
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// afterDisplayZero returns the dump from the start of display 0's block.
// The id is matched to its end so that display 1084 cannot answer for it.
func afterDisplayZero(dump string) (string, bool) {
	const key = "mDisplayId=0"
	for i := 0; ; {
		j := strings.Index(dump[i:], key)
		if j < 0 {
			return "", false
		}
		i += j + len(key)
		if i >= len(dump) || dump[i] < '0' || dump[i] > '9' {
			return dump[i:], true
		}
	}
}
