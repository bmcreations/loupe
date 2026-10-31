// Package httpapi exposes the target registry and streaming endpoint over HTTP.
package httpapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bmcreations/loupe/internal/clipshare"
	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
	"github.com/bmcreations/loupe/internal/trace"
)

// idleGrace is how long a session with no viewers stays alive before it is
// torn down. Without it, a page refresh would cost a full scrcpy relaunch,
// which is the better part of a second on a real device. It is a var only so
// tests can shorten it before a manager starts.
var idleGrace = 15 * time.Second

// Manager owns the set of known targets and the sessions running against them.
type Manager struct {
	trace *trace.Writer

	mu sync.Mutex
	// opts is under mu because the settings API replaces it while sessions are
	// being opened. It is read into a local before Acquire drops the lock.
	opts target.Options
	// clientBufferMs is the jitter-buffer ceiling handed to each new session.
	// It is not a target.Option: it configures the browser, not the capture,
	// and target.Options is passed straight to the device launcher.
	clientBufferMs int
	humanHold      time.Duration
	// emulatorPause is how long a headless emulator goes unused before it is
	// paused, or 0 for never. See emupause.go.
	emulatorPause time.Duration
	// claimed reports whether a target has a live claim. Nil means none do.
	// A claimed emulator is never paused or shut down by the idle clock.
	claimed func(id string) bool
	// rescanAVDs re-reads the AVD directory and registers any AVD not yet a
	// target, so one made by avdmanager or Android Studio appears without a
	// restart. Nil means no rescan. rescanAt throttles it.
	rescanAVDs func()
	rescanAt   time.Time
	// emulatorShutdown is how long one goes unused before it is shut down,
	// or 0 for never.
	emulatorShutdown time.Duration
	// iosSimcap is the simcap physical iPhones stream through, or "".
	iosSimcap string
	// xcrunner is the built XCUITest runner phones below iOS 27 take
	// touches through, or "".
	xcrunner string
	// simListedAt is when the simctl listing last applied began; see
	// SyncSimulatorsAt.
	simListedAt time.Time
	// adaptiveBitRate turns the per-session bitrate controller on. When it is
	// set, opts.BitRate is a ceiling the session may relaunch below rather than
	// the rate it holds for its whole life.
	adaptiveBitRate bool
	// simSlim is whether boots a simulator target starts are slim. Atomic
	// because a booting simulator reads it without m.mu.
	simSlim atomic.Bool
	targets map[string]target.Target
	// peerRows is the peers' own rows as last merged, ids already
	// namespaced. The PeerTargets in targets mirror it, and Targets lists it
	// as it is rather than recomputing it from anything local.
	peerRows []TargetInfo
	// peerDial opens a stream to a peer's target, set by SetPeers.
	peerDial target.PeerDialer
	// peerCall reaches a peer's HTTP routes, set by SetPeers.
	peerCall target.PeerCaller
	sessions map[string]*session.Session
	// overlays is what extensions have put over each target; see overlay.go.
	overlays *overlayStore
	reapers  map[string]*time.Timer

	// clipShare is the shared clipboard each local session feeds and is
	// written from, or nil. See clipshare_targets.go.
	clipShare *clipshare.Service
	// clipSent is the Seq of the newest shared value written to each
	// target's current session, so the fan-out and the on-open write never
	// send one value twice or an older one after it. Guarded by clipSentMu,
	// not m.mu: the writers run off m.mu.
	clipSentMu sync.Mutex
	clipSent   map[string]uint64

	// presence is adb's word for each device it tracks, or "missing" once adb
	// stops listing it. A target adb does not track -- synthetic, an AVD not
	// yet booted -- has no entry and is always available.
	presence map[string]string
	// lastErr is why the most recent Acquire of a target failed, cleared by
	// the next one that succeeds. A browser cannot read the 502 that carries
	// it: a WebSocket that fails before upgrading reports only that it closed,
	// so the client asks for it here instead.
	lastErr map[string]string
	// adbDown is why adb is unreachable, or "" while it answers. While it is
	// set, every adb device and every AVD without a live session is
	// unavailable with that reason, rather than failing at the jar push or
	// trying to boot an AVD that is already running.
	adbDown string
	// shutAt is the reopen guard EndSession arms on each target. While the
	// shutdown or delete runs, and for shutDownGrace after it returns, Acquire
	// refuses the target: a viewer that never got the end message, or a stale
	// tab, would otherwise reconnect and boot the device again.
	shutAt map[string]endedAt
	// lifeBusy holds the targets with a shutdown or delete running, whether
	// a request or the idle check started it, so a second one answers 409
	// rather than racing the first.
	lifeBusy map[string]bool
	// gone is when Unregister forgot each target. A listing or scan that began
	// before then still names the device, and must not register it again.
	gone map[string]time.Time
}

// endedAt is one target's reopen guard. armed is EndSession's token, which
// ClearEnded and FinishEnded match so a request touches only its own guard.
// While pending the shutdown or delete is still running and the guard does
// not expire; FinishEnded sets at, from which shutDownGrace counts.
type endedAt struct {
	armed   time.Time
	at      time.Time
	reason  string
	pending bool
}

// refuses reports whether the guard still refuses an open at now.
func (e endedAt) refuses(now time.Time) bool {
	return e.pending || now.Sub(e.at) < shutDownGrace
}

// goneFor is how long Unregister's mark outlives the target. A simctl listing
// takes about 0.24s and an AVD scan less, so for those it only bounds the map.
// For that long Acquire also answers the id as deleted rather than unknown.
const goneFor = time.Minute

// shutDownGrace is how long a target shut down by EndSession refuses to open
// after the shutdown returns. A var so tests can shorten it.
var shutDownGrace = 10 * time.Second

// ErrJustShutDown is what Acquire returns for a target whose shutdown or
// delete is running or returned within shutDownGrace, or one shut down while
// this open was running. The
// handlers match it to close with the same reason the original close gave.
var ErrJustShutDown = errors.New("target was just shut down")

// refusedError is Acquire's refusal of a target EndSession just ended. It
// matches ErrJustShutDown and carries the reason EndSession was given
// ("shutDown" or "deleted"), so a reconnect hears what the original close said.
type refusedError struct {
	label, reason string
}

func (e *refusedError) Error() string   { return e.label + ": " + ErrJustShutDown.Error() }
func (e *refusedError) Is(t error) bool { return t == ErrJustShutDown }

// endedReason is the reason a refused Acquire carries, "shutDown" when err
// does not name one.
func endedReason(err error) string {
	var re *refusedError
	if errors.As(err, &re) && re.reason != "" {
		return re.reason
	}
	return "shutDown"
}

// NewManager builds an empty manager with the given default stream options.
func NewManager(opts target.Options) *Manager {
	return &Manager{
		opts:     opts,
		targets:  make(map[string]target.Target),
		sessions: make(map[string]*session.Session),
		reapers:  make(map[string]*time.Timer),
		presence: make(map[string]string),
		lastErr:  make(map[string]string),
		shutAt:   make(map[string]endedAt),
		lifeBusy: make(map[string]bool),
		gone:     make(map[string]time.Time),
		overlays: newOverlayStore(),
	}
}

// SetTrace attaches a trace writer so session termination is recorded. Without
// it a stream that ends on its own is indistinguishable in the trace from one
// that is merely quiet: every counter freezes either way.
func (m *Manager) SetTrace(tr *trace.Writer) { m.trace = tr }

// Options returns the stream options new sessions will be opened with.
func (m *Manager) Options() target.Options {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.opts
}

// SetOptions replaces the options new sessions will be opened with.
//
// Running sessions keep the options they were opened with. scrcpy takes its
// encoder settings at launch and offers no way to change them afterwards, so
// applying these to a live session would mean tearing it down and rebuilding
// it -- a visible interruption in exchange for a setting the user can get by
// reconnecting.
func (m *Manager) SetOptions(o target.Options) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opts = o
}

// SetClientBuffer sets the jitter-buffer ceiling new sessions report to their
// viewers. Sessions already open keep the value they were built with, for the
// same reason SetOptions does: init is cached per session.
func (m *Manager) SetClientBuffer(ms int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clientBufferMs = ms
}

// SetHumanHold sets how long a viewer's last input holds new sessions'
// targets against agents. Sessions already open keep the hold they opened
// with, like SetClientBuffer.
func (m *Manager) SetHumanHold(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.humanHold = d
}

// SetSimulatorSlim sets whether boots a simulator target starts from now on
// are slim (target/simslim.go). A simulator already booted keeps the services
// it booted with until it next boots.
func (m *Manager) SetSimulatorSlim(on bool) { m.simSlim.Store(on) }

// SetAdaptiveBitRate turns the bitrate controller on or off for sessions
// opened from now on. Like the stream options it does not reach a session that
// is already running, which is why the settings API reports it as applying next
// session.
func (m *Manager) SetAdaptiveBitRate(on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.adaptiveBitRate = on
}

// AdaptiveBitRate reports whether new sessions run the bitrate controller.
func (m *Manager) AdaptiveBitRate() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.adaptiveBitRate
}

// Register adds or replaces a target.
func (m *Manager) Register(t target.Target) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.targets[t.ID()] = t
}

// SyncDevices brings the registered adb devices in line with what adb lists
// now. A new serial is registered through build; a known one whose state or
// model changed is rebuilt, since an ADBTarget keeps the state it was built
// with; one adb no longer lists is kept and marked "missing" rather than
// removed, so the picker can say where it went instead of it silently
// vanishing from under a viewer that had it selected.
//
// An emulator is the exception. One whose AVD has a target -- devices carry
// the name once target.AVDResolver has read it -- is that target running,
// not a device of its own, so it is attached to the AVD rather than listed
// twice. An AVD adb no longer lists has stopped, and is detached so the next
// Open boots it again.
func (m *Manager) SyncDevices(devices []target.DeviceInfo, build func(target.DeviceInfo) target.Target) {
	m.mu.Lock()
	defer m.mu.Unlock()

	avds := map[string]*target.Emulator{}
	// An emulator this server is booting shows up in adb before it can say
	// which AVD it is, so its serial is claimed by the port it was given.
	booting := map[string]*target.Emulator{}
	for _, t := range m.targets {
		if e, ok := t.(*target.Emulator); ok {
			avds[e.AVD()] = e
			if s := e.BootingAs(); s != "" {
				booting[s] = e
			}
		}
	}

	running := map[string]bool{}
	seen := map[string]bool{}
	for _, d := range devices {
		e := avds[d.AVD]
		if e == nil {
			e = booting[d.Serial]
		}
		if e != nil {
			running[e.AVD()] = true
			if d.Ready() && d.AVD == e.AVD() {
				was := e.Serial()
				e.Attach(d)
				if was != d.Serial {
					m.traceAVD(e, "running", d.Serial)
				}
			}
			// A serial target from before its name was known.
			if cur, ok := m.targets[d.Serial]; ok && isADB(cur) {
				delete(m.targets, d.Serial)
				delete(m.presence, d.Serial)
			}
			continue
		}
		seen[d.Serial] = true
		switch cur, ok := m.targets[d.Serial]; {
		case !ok:
			m.targets[d.Serial] = build(d)
		case isADB(cur) && cur.(*target.ADBTarget).Info() != d:
			m.targets[d.Serial] = build(d)
		}
		m.setPresence(d.Serial, d.State)
	}
	for id, t := range m.targets {
		if isADB(t) && !seen[id] {
			m.setPresence(id, "missing")
		}
	}
	for name, e := range avds {
		if !running[name] && e.Serial() != "" {
			m.traceAVD(e, "stopped", e.Serial())
			e.Detach()
		}
	}
}

// SyncSimulators brings the registered iOS simulators in line with a simctl
// listing. A new one is registered; a known one takes the fresh listing, so
// "Boots on open" follows a simulator booted or shut down from Xcode. One
// simctl no longer lists has been deleted, and is kept and marked missing
// for the same reason an unplugged device is.
func (m *Manager) SyncSimulators(sims []target.SimInfo, simcap string) {
	m.SyncSimulatorsAt(time.Now(), sims, simcap)
}

// SyncSimulatorsAt is SyncSimulators for a listing whose simctl call began at
// listedAt. A listing that began before the one last applied is dropped: the
// 5s poll can start before `simctl create` returns and apply after the
// creator's own sync, and would mark the new simulator missing until the next
// poll.
func (m *Manager) SyncSimulatorsAt(listedAt time.Time, sims []target.SimInfo, simcap string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if listedAt.Before(m.simListedAt) {
		return
	}
	m.simListedAt = listedAt

	seen := map[string]bool{}
	for _, info := range sims {
		id := target.SimulatorID(info.UDID)
		if at, ok := m.gone[id]; ok && !listedAt.After(at) {
			// Deleted after this listing began: simctl had not yet dropped it.
			continue
		}
		seen[id] = true
		if cur, ok := m.targets[id].(*target.Simulator); ok {
			if was := cur.Info(); was.State != info.State {
				m.trace.Record(map[string]any{
					"kind": "target-state", "target": id, "state": info.State, "was": was.State,
				})
			}
			cur.Update(info)
		} else {
			sim := target.NewSimulator(info, simcap)
			sim.SetBootHooks(m.simSlim.Load, func(r map[string]any) { m.trace.Record(r) })
			m.targets[id] = sim
		}
		m.setPresence(id, "device")
	}
	for id, t := range m.targets {
		if _, ok := t.(*target.Simulator); ok && !seen[id] {
			m.setPresence(id, "missing")
		}
	}
}

// SetIOSSimcap names the simcap that streams physical iPhones, for devices
// already listed and those to come.
func (m *Manager) SetIOSSimcap(bin string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.iosSimcap = bin
	for _, t := range m.targets {
		if d, ok := t.(*target.IOSDevice); ok {
			d.SetSimcap(bin)
		}
	}
}

// SetXCRunner names the built XCUITest runner's products directory, for
// devices already listed and those to come.
func (m *Manager) SetXCRunner(dir string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.xcrunner = dir
	for _, t := range m.targets {
		if d, ok := t.(*target.IOSDevice); ok {
			d.SetXCRunner(dir)
		}
	}
}

// SyncIOSDevices brings the registered physical iOS devices in line with a
// devicectl listing. devicectl keeps a paired device listed while the Mac
// cannot reach it, so presence is "device" while it has a transport and
// "offline" while it does not, and a device it stops listing, unpaired, is
// kept and marked missing. A change of transport, cable to Wi-Fi or back, is
// traced, since it changes what the device can carry.
func (m *Manager) SyncIOSDevices(devs []target.IOSDeviceInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()

	seen := map[string]bool{}
	for _, info := range devs {
		id := target.IOSDeviceID(info)
		seen[id] = true
		if cur, ok := m.targets[id].(*target.IOSDevice); ok {
			if was := cur.Info(); was.Transport != info.Transport {
				m.trace.Record(map[string]any{
					"kind": "target-state", "target": id, "transport": info.Transport, "was": was.Transport,
				})
			}
			cur.Update(info)
		} else {
			d := target.NewIOSDevice(info)
			d.SetSimcap(m.iosSimcap)
			d.SetXCRunner(m.xcrunner)
			m.targets[id] = d
		}
		state := "offline"
		if info.Reachable() {
			state = "device"
		}
		m.setPresence(id, state)
	}
	for id, t := range m.targets {
		if _, ok := t.(*target.IOSDevice); ok && !seen[id] {
			m.setPresence(id, "missing")
		}
	}
}

// traceAVD records an AVD starting or stopping. It is not presence: an AVD
// that is not running is still available, since opening it boots it.
func (m *Manager) traceAVD(e *target.Emulator, state, serial string) {
	fmt.Printf("target: %s is %s as %s\n", e.ID(), state, serial)
	m.trace.Record(map[string]any{
		"kind": "target-state", "target": e.ID(), "state": state, "serial": serial,
	})
}

// canonical maps a requested id to a registered one. An AVD's running serial
// is accepted for the AVD, so an agent or a bookmark that named
// emulator-5554 before AVDs had ids of their own still reaches it. Called
// with mu held.
func (m *Manager) canonical(id string) string {
	if _, ok := m.targets[id]; ok {
		return id
	}
	for _, t := range m.targets {
		if e, ok := t.(*target.Emulator); ok && e.Serial() == id {
			return e.ID()
		}
	}
	return id
}

func isADB(t target.Target) bool {
	_, ok := t.(*target.ADBTarget)
	return ok
}

// setPresence records a device's state, tracing only a change. Called with mu
// held.
func (m *Manager) setPresence(id, state string) {
	was, known := m.presence[id]
	if known && was == state {
		return
	}
	m.presence[id] = state
	fmt.Printf("target: %s is %s\n", id, state)
	m.trace.Record(map[string]any{
		"kind": "target-state", "target": id, "state": state, "was": was,
	})
}

// SetADBDown records that adb has stopped answering, and why, or with "" that
// it answers again. A target already streaming keeps its session: the scrcpy
// server it runs has its own forwarded socket and does not need adb to go on.
func (m *Manager) SetADBDown(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.adbDown = reason
}

// adbState is the state reported for a target adb cannot reach.
const adbState = "adbUnreachable"

// adbBlocked reports whether a target needs adb to open. Called with mu held.
func (m *Manager) adbBlocked(id string, t target.Target) bool {
	if m.adbDown == "" {
		return false
	}
	if _, live := m.sessions[id]; live {
		return false
	}
	_, emu := t.(*target.Emulator)
	return emu || isADB(t)
}

// unavailable explains why a target cannot be opened, or returns nil if it
// can. Called with mu held.
func (m *Manager) unavailable(id string, t target.Target) error {
	if m.adbBlocked(id, t) {
		return fmt.Errorf("adb is not responding, so %s cannot be opened (%s). loupe restarts the adb server if this lasts; `adb kill-server && adb start-server` on the Mac does it now", t.Label(), m.adbDown)
	}
	if _, peer := t.(*target.PeerTarget); peer {
		for _, row := range m.peerRows {
			if row.ID == id && !row.Available {
				return fmt.Errorf("%s is not available: %s", t.Label(), row.Reason)
			}
		}
		return nil
	}
	state, tracked := m.presence[id]
	if !tracked || state == "device" {
		return nil
	}
	if t.Kind() == target.KindIOSDevice {
		switch state {
		case "missing":
			return fmt.Errorf("%s is no longer paired with this Mac: devicectl no longer lists it", t.Label())
		default:
			return fmt.Errorf("%s is paired but this Mac cannot reach it: plug it in, or bring it onto the same network", t.Label())
		}
	}
	switch state {
	case "missing":
		if t.Kind() == target.KindSimulator {
			return fmt.Errorf("%s no longer exists: simctl no longer lists it", t.Label())
		}
		return fmt.Errorf("%s is not connected: adb no longer lists it", t.Label())
	case "unauthorized":
		return fmt.Errorf("%s has not authorized this computer: accept the USB debugging prompt on the device", t.Label())
	case "offline":
		return fmt.Errorf("%s is offline: adb can see it but cannot talk to it", t.Label())
	default:
		return fmt.Errorf("%s is in adb state %q, not ready to stream", t.Label(), state)
	}
}

// describeTimeout bounds how long a single target's Describe may take before
// its fields are simply omitted from the response. /api/targets is on the
// page-load path, and a device that is slow to answer getprop -- or an AVD
// that has not finished booting -- must not hold up every other target's
// listing.
//
// A package var, not a const, so a test can shrink it well below 2s and
// still assert real wall-clock timing without the test suite paying for a
// 2s sleep per case.
var describeTimeout = 2 * time.Second

// TargetInfo is the JSON shape returned by the targets endpoint. Description
// is embedded rather than nested so its fields (os, osVersion, apiLevel,
// form) sit at the top level of the JSON; a target that cannot describe
// itself, or has not yet, simply omits them rather than guessing.
type TargetInfo struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Active bool   `json:"active"`
	// BootsOnOpen is true for a registered AVD that ensureBooted has never
	// seen attached, so opening it will boot the emulator rather than join
	// something already running. Omitted (rather than false) for every target
	// that is not a target.Booter at all -- a device, a running emulator
	// already adopted as an ADBTarget, or synthetic -- since those never
	// boot anything on Open and "false" would otherwise be indistinguishable
	// from "not applicable".
	BootsOnOpen bool `json:"bootsOnOpen,omitempty"`
	// Serial is the adb serial a running AVD is attached as. A client that
	// saved the serial before AVDs had ids of their own matches on it.
	Serial string `json:"serial,omitempty"`
	// Headless is true for a running AVD whose emulator has no window on the
	// Mac, read from its qemu process, so it holds for one Android Studio
	// started too. Omitted for one with its window and for everything else.
	Headless bool `json:"headless,omitempty"`
	// Paused is true for a running AVD paused for being idle. The next open
	// or agent call resumes it.
	Paused bool `json:"paused,omitempty"`
	// Server names the peer that owns the target. Omitted for this server's
	// own rows. On a peer's row, ID is "<server>/<owner's id>" and every
	// other field is the owner's, apart from Active and LastError, which
	// are about sessions on this server.
	Server string `json:"server,omitempty"`
	target.Description

	// Available is false for a device adb cannot stream right now, and State
	// and Reason say why. State is adb's own word, or "missing".
	Available bool   `json:"available"`
	State     string `json:"state,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// LastError is why the last attempt to open this target failed, if it did.
	LastError string `json:"lastError,omitempty"`

	// Claim is the agent that has claimed this target, if one has.
	Claim *ClaimInfo `json:"claim,omitempty"`
	// HandsOff is true when the person has marked the target off limits to
	// every agent.
	HandsOff bool `json:"handsOff,omitempty"`
	// DrivenBy names the agent whose input last went through on this target,
	// for 10s after it did, whether or not a session is open.
	DrivenBy string `json:"drivenBy,omitempty"`
}

// Targets lists everything registered, marking which already have a session.
//
// Describing each target costs an adb call, so it happens outside m.mu and in
// parallel across targets -- collecting the snapshot under the lock and then
// releasing it before any of that work starts, so a slow device cannot block
// a concurrent Acquire or a second call to Targets. A target that only
// implements target.Target, not target.Describer (there are none today, but
// nothing requires there to be), is listed with its Description fields
// omitted rather than skipped.
//
// The order is fixed -- devices, then physical iOS devices, then emulators,
// then simulators, then the pattern, each by ID -- because a client polling the list reads a new order as a changed list
// and rebuilds a picker the viewer may have open.
func (m *Manager) Targets(ctx context.Context) []TargetInfo {
	return m.targetRows(ctx, true)
}

// LocalTargets is Targets without the peers' rows: what GET /api/targets
// answers a hub that polls it with ?local=1, so two servers that peer with
// each other list one another's devices once.
func (m *Manager) LocalTargets(ctx context.Context) []TargetInfo {
	return m.targetRows(ctx, false)
}

func (m *Manager) targetRows(ctx context.Context, withPeers bool) []TargetInfo {
	m.mu.Lock()
	out := make([]TargetInfo, 0, len(m.targets))
	for id, t := range m.targets {
		// A peer's row is the owner's, listed below; nothing here that reads
		// local adb, qemu or simctl state applies to it.
		if _, peer := t.(*target.PeerTarget); peer {
			continue
		}
		_, active := m.sessions[id]
		info := TargetInfo{
			ID:        id,
			Kind:      string(t.Kind()),
			Label:     t.Label(),
			Active:    active,
			Available: true,
			State:     m.presence[id],
			LastError: m.lastErr[id],
		}
		if m.adbBlocked(id, t) {
			info.State = adbState
		}
		if err := m.unavailable(id, t); err != nil {
			info.Available = false
			info.Reason = err.Error()
		}
		if b, ok := t.(target.Booter); ok {
			info.BootsOnOpen = b.BootsOnOpen()
		}
		if e, ok := t.(*target.Emulator); ok {
			info.Serial = e.Serial()
		}
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b TargetInfo) int {
		return cmp.Or(
			cmp.Compare(kindRank(a.Kind), kindRank(b.Kind)),
			simulatorOrder(a, b),
			cmp.Compare(a.ID, b.ID),
		)
	})
	targets := make(map[string]target.Target, len(m.targets))
	for id, t := range m.targets {
		targets[id] = t
	}
	var peers []TargetInfo
	if withPeers {
		peers = m.peerRowsLocked()
	}
	m.mu.Unlock()

	m.markHeadless(ctx, out, targets)

	var wg sync.WaitGroup
	for i := range out {
		// A device adb no longer lists would only spend describeTimeout
		// failing to answer getprop.
		if !out[i].Available {
			continue
		}
		d, ok := targets[out[i].ID].(target.Describer)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(i int, d target.Describer) {
			defer wg.Done()
			dctx, cancel := context.WithTimeout(ctx, describeTimeout)
			defer cancel()
			desc, err := d.Describe(dctx)
			if err != nil {
				return
			}
			out[i].Description = desc
		}(i, d)
	}
	wg.Wait()

	// After Describe, which is what asks the console about an emulator a
	// previous process paused.
	for i := range out {
		if e, ok := targets[out[i].ID].(*target.Emulator); ok && !out[i].BootsOnOpen {
			out[i].Paused = e.Paused()
		}
	}
	// After the local rows and not sorted in with them: a peer's rows keep
	// the order the owner listed them in, grouped by server, so a poll that
	// changes nothing does not reorder a picker the viewer has open.
	return append(out, peers...)
}

// peerRowsLocked copies the peers' rows with this hub's own view of them: a
// session is open on the hub, and the hub's last failed open. The owner's
// availability, state, claim and hands-off stand as it reported them.
// m.mu must be held.
func (m *Manager) peerRowsLocked() []TargetInfo {
	out := make([]TargetInfo, len(m.peerRows))
	for i, row := range m.peerRows {
		_, row.Active = m.sessions[row.ID]
		row.LastError = m.lastErr[row.ID]
		out[i] = row
	}
	return out
}

// peerTarget reports whether id is a peer's target.
func (m *Manager) peerTarget(id string) (*target.PeerTarget, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pt, ok := m.targets[id].(*target.PeerTarget)
	return pt, ok
}

// SetPeerCaller sets how a PeerTarget calls the owner's HTTP routes. Call it
// before SetPeerRows, like SetPeerDialer.
func (m *Manager) SetPeerCaller(c target.PeerCaller) {
	m.mu.Lock()
	m.peerCall = c
	m.mu.Unlock()
}

// SetPeerDialer sets how a PeerTarget opens its stream. Call it before
// SetPeerRows: targets registered earlier keep the dialer they were made with.
func (m *Manager) SetPeerDialer(d target.PeerDialer) {
	m.mu.Lock()
	m.peerDial = d
	m.mu.Unlock()
}

// SetPeerRows replaces the peers' rows with rows, whose ids are already
// "<server>/<owner id>", and registers a PeerTarget for each so the handlers'
// s.mgr.Target(id) resolves it. A row that has gone ends its session and is
// forgotten like a deleted device, so a viewer is told rather than left on a
// dead stream.
func (m *Manager) SetPeerRows(rows []TargetInfo) {
	want := make(map[string]TargetInfo, len(rows))
	for _, r := range rows {
		want[r.ID] = r
	}
	m.mu.Lock()
	m.peerRows = slices.Clone(rows)
	for _, r := range rows {
		owner := strings.TrimPrefix(r.ID, r.Server+"/")
		if cur, ok := m.targets[r.ID].(*target.PeerTarget); ok && cur.Kind() == target.Kind(r.Kind) && cur.Label() == r.Label {
			continue
		}
		m.targets[r.ID] = target.NewPeerTarget(r.Server, owner, target.Kind(r.Kind), r.Label, m.peerDial, m.peerCall)
		delete(m.gone, r.ID)
	}
	var gone []string
	for id, t := range m.targets {
		if _, peer := t.(*target.PeerTarget); !peer {
			continue
		}
		if _, ok := want[id]; !ok {
			gone = append(gone, id)
		}
	}
	m.mu.Unlock()
	for _, id := range gone {
		m.Unregister(id)
	}
}

// markHeadless sets Headless on each running AVD whose emulator has no
// window. It reads the process table once, and only when an AVD is running.
func (m *Manager) markHeadless(ctx context.Context, out []TargetInfo, targets map[string]target.Target) {
	var running []int
	for i := range out {
		if _, ok := targets[out[i].ID].(*target.Emulator); ok && !out[i].BootsOnOpen {
			running = append(running, i)
		}
	}
	if len(running) == 0 {
		return
	}
	headless := headlessAVDs(ctx)
	for _, i := range running {
		e := targets[out[i].ID].(*target.Emulator)
		out[i].Headless = headless[e.AVD()]
	}
}

// headlessAVDs is a var so a test can say which AVDs run headless.
var headlessAVDs = target.HeadlessAVDs

// simulatorOrder puts running simulators first, then sorts by name. Their ids
// are UDIDs, so ordering by id alone would scatter a dozen iPhones at random.
func simulatorOrder(a, b TargetInfo) int {
	if target.Kind(a.Kind) != target.KindSimulator || target.Kind(b.Kind) != target.KindSimulator {
		return 0
	}
	boot := func(t TargetInfo) int {
		if t.BootsOnOpen {
			return 1
		}
		return 0
	}
	return cmp.Or(cmp.Compare(boot(a), boot(b)), cmp.Compare(a.Label, b.Label))
}

func kindRank(k string) int {
	switch target.Kind(k) {
	case target.KindDevice:
		return 0
	case target.KindIOSDevice:
		return 1
	case target.KindEmulator:
		return 2
	case target.KindSimulator:
		return 3
	default:
		return 4
	}
}

// AllStats returns the live counters for every running session.
func (m *Manager) AllStats() []map[string]any {
	m.mu.Lock()
	sessions := make([]*session.Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()

	out := make([]map[string]any, 0, len(sessions))
	for _, s := range sessions {
		st := s.Stats()
		if cs := s.ClientStats(); cs != nil {
			st["client"] = cs
		}
		out = append(out, st)
	}
	return out
}

// Acquire returns the session for a target, starting one if needed. Every
// successful Acquire must be paired with a Release.
func (m *Manager) Acquire(ctx context.Context, targetID string) (*session.Session, error) {
	m.mu.Lock()
	targetID = m.canonical(targetID)

	// A pending teardown means a viewer left moments ago. Cancel it: this is
	// the page-refresh case, and reusing the live session makes the reload
	// near-instant.
	if timer, ok := m.reapers[targetID]; ok {
		timer.Stop()
		delete(m.reapers, targetID)
	}

	if s, ok := m.sessions[targetID]; ok {
		select {
		case <-s.Done():
			// Session died since it was created; fall through and rebuild it.
			delete(m.sessions, targetID)
		default:
			m.mu.Unlock()
			return s, nil
		}
	}

	t, ok := m.targets[targetID]
	if !ok {
		err := m.unknownLocked(targetID)
		m.mu.Unlock()
		return nil, err
	}
	if ended, ok := m.shutAt[targetID]; ok {
		if ended.refuses(time.Now()) {
			m.mu.Unlock()
			return nil, &refusedError{t.Label(), ended.reason}
		}
		delete(m.shutAt, targetID)
	}
	// Fail here rather than in adb: a device adb has stopped listing fails at
	// the jar push with "failed to get feature set", which is true and says
	// nothing about what to do.
	if err := m.unavailable(targetID, t); err != nil {
		m.lastErr[targetID] = err.Error()
		m.mu.Unlock()
		return nil, err
	}
	opts := m.opts
	clientBuf := m.clientBufferMs
	humanHold := m.humanHold
	adaptive := m.adaptiveBitRate
	share := m.clipShare
	// A relaunch would reconnect to the owner and wait for a keyframe, and
	// the owner's own controller already adapts to the link it can see.
	_, isPeer := t.(*target.PeerTarget)
	if isPeer {
		adaptive = false
	}
	m.mu.Unlock()
	began := time.Now()

	// Open outside the lock: launching scrcpy takes hundreds of milliseconds
	// and must not block the targets listing or other sessions.
	s, err := session.Open(ctx, targetID, t, opts)
	m.mu.Lock()
	if err != nil {
		m.lastErr[targetID] = err.Error()
	} else {
		delete(m.lastErr, targetID)
	}
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	// Before publishing, so no viewer can join and cache init without it.
	s.SetClientBuffer(clientBuf)
	if humanHold > 0 {
		s.SetHumanHold(humanHold)
	}
	// One record per change of hold or of agent state, never one per touch:
	// a finger dragging only refreshes a timestamp inside the arbiter, so
	// the fsync Record makes lands on a viewer's read loop only for the
	// first touch after the hold lapsed. Recorded in place rather than from
	// a goroutine, because Record stamps "t" and a goroutine could reorder
	// a driving/waiting pair.
	s.SetControlObserver(func(c session.ControlChange) {
		rec := map[string]any{"kind": "control", "target": targetID}
		if c.State != "" {
			rec["state"] = c.State
			rec["viewer"] = c.Viewer
			if c.State == "free" {
				rec["heldMs"] = c.HeldMs
			}
		} else {
			rec["state"] = c.Agent
			if c.AgentName != "" {
				rec["agent"] = c.AgentName
			}
		}
		m.trace.Record(rec)
	})

	// The resize and event observers go on here too, before the session is
	// published to m.sessions and before m.mu is even taken for that -- not
	// after, the way they used to be. A viewer cannot join before Acquire
	// returns this session to its caller, so the old position was not racing
	// a viewer; it was racing the pump goroutine session.Open above already
	// started, which can call onResize before anything here has set it.
	// Moving the calls earlier narrows that window but does not close it:
	// closing it needs the observer passed into Open itself, which is a wider
	// change than this task makes. This is the earliest point available
	// without it.
	s.SetResizeObserver(func(oldW, oldH, newW, newH int) {
		m.trace.Record(map[string]any{
			"kind":   "resize",
			"target": targetID,
			"from":   strconv.Itoa(oldW) + "x" + strconv.Itoa(oldH),
			"to":     strconv.Itoa(newW) + "x" + strconv.Itoa(newH),
		})
	})

	s.SetLiftObserver(func(viewer uint64, n int) {
		m.trace.Record(map[string]any{
			"kind":     "touch",
			"target":   targetID,
			"action":   "lifted",
			"viewer":   viewer,
			"pointers": n,
		})
	})

	s.SetClipboardObserver(func(runes, viewers int) {
		m.trace.Record(map[string]any{
			"kind":    "clipboard",
			"target":  targetID,
			"from":    "device",
			"textLen": runes,
			"viewers": viewers,
		})
	})
	// A peer's copies reach this server through its own shared clipboard,
	// not through the relay.
	if share != nil && !isPeer {
		s.SetClipboardSink(func(text string) {
			share.Publish(clipshare.Value{Text: text, Origin: clipOrigin(targetID)})
		})
	}

	// PublishEvent's observer runs synchronously on whichever viewer's read
	// loop published the event, and m.trace.Record fsyncs. Recording directly
	// there would make every viewer's touch wait on a disk write, so the
	// callback only ever hands the event to a buffered channel; one goroutine
	// drains it and does the actual, slower Record call. eventDrops counts
	// what the buffer could not hold when the drain goroutine falls behind --
	// traced on stream-end rather than silently discarded, because a channel
	// that is comfortably keeping up looks identical to one about to
	// overflow if nothing is counting drops.
	//
	// s.Done() closing does not mean events have stopped: a viewer's own
	// teardown (RemoveViewer's trailing-scroll flush, for one) can still call
	// PublishEvent after the stream has ended. So closing is a two-step
	// handoff rather than the goroutine simply returning: on Done it drains
	// whatever is already buffered non-blockingly, then flips eventClosed,
	// after which the observer counts every further call as a drop instead of
	// writing into a channel nothing will ever read again. eventClosed and the
	// observer's check-and-send are both under eventMu so the two goroutines
	// cannot race past each other -- either the send is ordered before the
	// drain sees it, or the closed flag is ordered before the observer's
	// check, never neither.
	eventCh := make(chan session.Event, 256)
	var eventDrops atomic.Uint64
	var eventMu sync.Mutex
	eventClosed := false
	eventDrainDone := make(chan struct{})
	go func() {
		defer close(eventDrainDone)
		recordEvent := func(e session.Event) {
			m.trace.Record(map[string]any{
				"kind":   "event",
				"target": targetID,
				"event":  e,
			})
		}
		for {
			select {
			case e := <-eventCh:
				recordEvent(e)
			case <-s.Done():
				eventMu.Lock()
				eventClosed = true
				eventMu.Unlock()
				// Drain what was already queued before the flag flipped.
				// Nothing new can arrive in the channel after this point,
				// since the observer now routes straight to eventDrops.
				for {
					select {
					case e := <-eventCh:
						recordEvent(e)
					default:
						return
					}
				}
			}
		}
	}()
	s.SetEventObserver(func(e session.Event) {
		eventMu.Lock()
		defer eventMu.Unlock()
		if eventClosed {
			eventDrops.Add(1)
			return
		}
		select {
		case eventCh <- e:
		default:
			eventDrops.Add(1)
		}
	})

	m.mu.Lock()
	// Unregister may have removed the target while it opened: publishing the
	// session would leave one nothing can reach or end.
	if _, ok := m.targets[targetID]; !ok {
		err := m.unknownLocked(targetID)
		m.mu.Unlock()
		_ = s.Close()
		return nil, err
	}
	// A shut down that landed while Open ran found no session to end.
	if ended, ok := m.shutAt[targetID]; ok && (ended.pending || ended.at.After(began)) {
		m.mu.Unlock()
		_ = s.Close()
		return nil, &refusedError{t.Label(), ended.reason}
	}
	// Another request may have won the race while we were opening.
	if existing, ok := m.sessions[targetID]; ok {
		m.mu.Unlock()
		_ = s.Close()
		return existing, nil
	}
	m.forgetClipSend(targetID)
	m.sessions[targetID] = s
	m.mu.Unlock()

	// After publishing, so a post that lands while this runs either finds
	// the session itself or is already in the store this reads.
	m.overlays.apply(targetID, s)
	if share != nil && !isPeer {
		m.applySharedClipboard(targetID, t, share)
	}

	// Started after publishing, so the controller cannot relaunch a session
	// that is about to be discarded as a lost race.
	if adaptive {
		s.SetBitrateObserver(func(rep session.BitrateReport) {
			m.recordBitrate(targetID, rep)
		})
		// The return value is the difference between a controller that ran and
		// found nothing to do and one that never started, and discarding it is
		// how the second stayed invisible.
		if !s.AdaptBitRate() {
			m.trace.Record(map[string]any{
				"kind":   "bitrate",
				"target": targetID,
				"gate":   "notStarted",
			})
		}
	} else {
		gate := "disabled"
		if isPeer {
			gate = "peer"
		}
		m.trace.Record(map[string]any{
			"kind":   "bitrate",
			"target": targetID,
			"gate":   gate,
		})
	}

	// Clean up the map when the underlying stream ends on its own, for example
	// because the cable was pulled.
	started := time.Now()
	go func() {
		<-s.Done()
		// Wait for the event-trace drain goroutine to finish flushing what it
		// had buffered before reading eventDrops: otherwise the stream-end
		// record's eventTraceDrops could be read mid-drain, before the count
		// (and the drained records' own position in the file, ahead of this
		// one) has settled.
		<-eventDrainDone
		m.mu.Lock()
		if m.sessions[targetID] == s {
			delete(m.sessions, targetID)
		}
		m.mu.Unlock()

		err := s.Err()
		if err != nil {
			fmt.Printf("[session %s] ended: %v\n", targetID, err)
		}
		if m.trace != nil {
			rec := map[string]any{
				"kind":      "stream-end",
				"target":    targetID,
				"uptimeMs":  time.Since(started).Milliseconds(),
				"framesOut": s.Stats()["framesOut"],
				// What the encoder produced, which framesOut cannot answer:
				// it counts per viewer, so two viewers double it and a latched
				// one stops moving it. A session that ends with framesIn 0 and
				// no err had a device that sent nothing.
				"framesIn": s.FrameCount(),
				"keysIn":   s.KeyCount(),
				"awaitKey": s.WaitCount(),
				"drop":     s.DropCount(),
				"shed":     s.ShedCount(),
				// eventsDropped is PublishEvent's own fan-out drop -- a
				// viewer's control channel was full. eventTraceDrops is a
				// second, separate count: this manager's forwarding channel
				// to the trace-writing goroutine was full. Either can be
				// nonzero on its own.
				"eventsDropped":   s.EventsDroppedCount(),
				"eventTraceDrops": eventDrops.Load(),
			}
			if err != nil {
				rec["err"] = err.Error()
			}
			m.trace.Record(rec)
		}
	}()

	return s, nil
}

// Release signals that a viewer has left. When the last one goes and no hold
// is outstanding (see session.Hold), the session is scheduled for teardown
// after the idle grace period. A hold counts as a viewer here and in the reaper.
func (m *Manager) Release(targetID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[targetID]
	if !ok || s.ViewerCount()+s.Holds() > 0 {
		return
	}
	if _, pending := m.reapers[targetID]; pending {
		return
	}

	var timer *time.Timer
	timer = time.AfterFunc(idleGrace, func() {
		// timer is read under m.mu, which Release holds while assigning it.
		m.mu.Lock()
		dead := m.reapLocked(targetID, s, timer)
		m.mu.Unlock()
		if dead != nil {
			_ = dead.Close()
		}
	})
	m.reapers[targetID] = timer
}

// reapLocked is the reaper timer self firing for session s: it returns the
// session to close, or nil. A timer that fired while Acquire waited on m.mu
// has been cancelled and its entry deleted, and Acquire has handed s back to
// a viewer that has not attached yet, so ViewerCount still reads 0. Only the
// timer still in m.reapers may act.
func (m *Manager) reapLocked(targetID string, s *session.Session, self *time.Timer) *session.Session {
	if m.reapers[targetID] != self {
		return nil
	}
	delete(m.reapers, targetID)
	cur, ok := m.sessions[targetID]
	if !ok || cur != s || cur.ViewerCount()+cur.Holds() > 0 {
		return nil
	}
	delete(m.sessions, targetID)
	return cur
}

// Shutdown closes every running session.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	sessions := make([]*session.Session, 0, len(m.sessions))
	for id, s := range m.sessions {
		sessions = append(sessions, s)
		delete(m.sessions, id)
	}
	for id, t := range m.reapers {
		t.Stop()
		delete(m.reapers, id)
	}
	m.mu.Unlock()

	for _, s := range sessions {
		_ = s.Close()
	}
}

// EndSession tells the viewers of one target's session why it is ending, then
// closes it. The target stays registered. A target with no session is a no-op.
//
// For "shutDown" and "deleted" it arms the reopen guard and returns its token.
// The guard refuses every open until the caller reports the outcome:
// FinishEnded when the device went down, which starts shutDownGrace, or
// ClearEnded when it did not. Any other reason returns the zero time.
func (m *Manager) EndSession(id, reason string) time.Time {
	m.mu.Lock()
	id = m.canonical(id)
	s, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	if t, pending := m.reapers[id]; pending {
		t.Stop()
		delete(m.reapers, id)
	}
	var armed time.Time
	if reason == "shutDown" || reason == "deleted" {
		armed = time.Now()
		m.shutAt[id] = endedAt{armed: armed, at: armed, reason: reason, pending: true}
	}
	m.mu.Unlock()

	if ok {
		_ = s.End(reason)
	}
	return armed
}

// lifeBegin claims the target for one shutdown or delete, and reports false
// when one is already running.
func (m *Manager) lifeBegin(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	id = m.canonical(id)
	if m.lifeBusy[id] {
		return false
	}
	m.lifeBusy[id] = true
	return true
}

// lifeInFlight reports whether a shutdown or delete of the target is running.
func (m *Manager) lifeInFlight(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lifeBusy[m.canonical(id)]
}

func (m *Manager) lifeEnd(id string) {
	m.mu.Lock()
	delete(m.lifeBusy, m.canonical(id))
	m.mu.Unlock()
}

// ClearEnded lifts the refusal EndSession armed, but only if armed is still
// the entry there: a later EndSession's guard is not this caller's to lift. A
// shut down or delete that failed left the device up, and "Start it" must
// reach it at once.
func (m *Manager) ClearEnded(id string, armed time.Time) {
	m.mu.Lock()
	id = m.canonical(id)
	if e, ok := m.shutAt[id]; ok && e.armed.Equal(armed) {
		delete(m.shutAt, id)
	}
	m.mu.Unlock()
}

// FinishEnded reports that the shutdown EndSession armed armed for has
// returned, so the guard expires shutDownGrace from now rather than from the
// request's start. A later EndSession's guard is left alone.
func (m *Manager) FinishEnded(id string, armed time.Time) {
	m.mu.Lock()
	id = m.canonical(id)
	if e, ok := m.shutAt[id]; ok && e.armed.Equal(armed) {
		e.pending = false
		e.at = time.Now()
		m.shutAt[id] = e
	}
	m.mu.Unlock()
}

// ClearShutDown lifts the reopen guard for an explicit Start, but only when
// the target was shut down: a deleted target stays refused, and so does one
// whose shutdown is still running. The guard stops automatic reconnects; a
// person pressing Start is not one.
func (m *Manager) ClearShutDown(id string) {
	m.mu.Lock()
	id = m.canonical(id)
	if e, ok := m.shutAt[id]; ok && e.reason == "shutDown" && !e.pending {
		delete(m.shutAt, id)
	}
	m.mu.Unlock()
}

// Unregister ends a target's session as deleted and forgets the target. It
// marks the id gone, so a simctl listing or an AVD scan that began before now
// cannot register it again.
func (m *Manager) Unregister(id string) {
	m.mu.Lock()
	id = m.canonical(id)
	m.mu.Unlock()

	m.EndSession(id, "deleted")

	m.mu.Lock()
	delete(m.targets, id)
	delete(m.presence, id)
	delete(m.lastErr, id)
	delete(m.shutAt, id)
	now := time.Now()
	for g, at := range m.gone {
		if now.Sub(at) > goneFor {
			delete(m.gone, g)
		}
	}
	m.gone[id] = now
	m.mu.Unlock()
}

// unknownLocked is Acquire's error for an id that is not a target. One
// Unregister forgot within goneFor is refused as deleted, so a tab that missed
// the end message shows the deleted screen rather than retrying a failed open.
// m.mu must be held.
func (m *Manager) unknownLocked(id string) error {
	if at, ok := m.gone[id]; ok && time.Since(at) < goneFor {
		return &refusedError{id, "deleted"}
	}
	return fmt.Errorf("unknown target %q", id)
}

// RegisterScanned registers t unless its id is already a target, or
// Unregister forgot it after scannedAt, when the scan that found it began. It
// reports whether t was registered.
func (m *Manager) RegisterScanned(t target.Target, scannedAt time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := t.ID()
	if _, ok := m.targets[id]; ok {
		return false
	}
	if at, ok := m.gone[id]; ok && !scannedAt.After(at) {
		return false
	}
	m.targets[id] = t
	return true
}

// Info returns the picker's row for one target.
func (m *Manager) Info(ctx context.Context, id string) (TargetInfo, bool) {
	m.mu.Lock()
	id = m.canonical(id)
	m.mu.Unlock()
	for _, ti := range m.Targets(ctx) {
		if ti.ID == id {
			return ti, true
		}
	}
	return TargetInfo{}, false
}

// Target looks up a registered target by id. It reports whether one exists
// rather than returning a nil interface, because a nil target.Target inside a
// non-nil interface value is not nil and the caller's check would pass.
func (m *Manager) Target(id string) (target.Target, bool) {
	m.mu.Lock()
	t, ok := m.targets[m.canonical(id)]
	m.mu.Unlock()
	if !ok && strings.HasPrefix(id, "avd:") && m.RescanAVDs() {
		m.mu.Lock()
		defer m.mu.Unlock()
		t, ok = m.targets[m.canonical(id)]
	}
	return t, ok
}

// Registered reports whether id is a registered target. Unlike Target it
// never rescans the AVD directory, so the rescan itself may call it.
func (m *Manager) Registered(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.targets[m.canonical(id)]
	return ok
}

// avdRescanEvery bounds how often an unknown avd: id or a listing triggers a
// scan of the AVD directory.
const avdRescanEvery = 2 * time.Second

// SetAVDRescan sets the function that re-reads the AVD directory and
// registers what is new.
func (m *Manager) SetAVDRescan(f func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rescanAVDs = f
}

// RescanAVDs looks for AVDs created outside loupe, at most once per
// avdRescanEvery. It reports whether it scanned.
func (m *Manager) RescanAVDs() bool {
	m.mu.Lock()
	f := m.rescanAVDs
	if f == nil || time.Since(m.rescanAt) < avdRescanEvery {
		m.mu.Unlock()
		return false
	}
	m.rescanAt = time.Now()
	m.mu.Unlock()
	f()
	return true
}

// Session looks up the running session for a target, if one exists. Unlike
// Acquire it never opens a device: a target with no viewers and no one
// driving it through /api/input has nothing to report, which is not an error
// -- GET /api/events reads this and answers an empty list rather than 404.
// It can also return a session that has already ended but is still waiting
// out idleGrace before the reaper deletes it from m.sessions; reading its
// ring is harmless, since EventsAfter and EventsSince both work off state
// that Close does not touch.
func (m *Manager) Session(id string) (*session.Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[m.canonical(id)]
	return s, ok
}

// HasViewer reports whether id is attached to any session now. The picker's
// actions on one target carry the viewer id of the session open on another,
// and claims and hands-off must not count that person as an agent.
func (m *Manager) HasViewer(id uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.HasViewer(id) {
			return true
		}
	}
	return false
}

// recordBitrate writes one interval of the bitrate controller to the trace.
//
// One record per second per session is deliberate. The controller's problem
// was never that it logged too little at the moment it acted -- it printed on
// both success and failure -- but that a run which never reached the decision
// looked exactly like one that reached it and declined. Only the gate that
// returned separates them, and a gate is per-interval.
func (m *Manager) recordBitrate(targetID string, rep session.BitrateReport) {
	rec := map[string]any{
		"kind":       "bitrate",
		"target":     targetID,
		"gate":       string(rep.Gate),
		"current":    rep.Current,
		"windowMs":   rep.WindowMs,
		"activeMs":   rep.ActiveMs,
		"awaitMs":    rep.AwaitMs,
		"frames":     rep.Frames,
		"shed":       rep.Shed,
		"bytes":      rep.Bytes,
		"shedFrac":   round3(rep.ShedFrac),
		"awaitKey":   rep.AwaitKey,
		"awaitFrac":  round3(rep.AwaitFrac),
		"goodputBps": rep.GoodputBps,
		"changes":    rep.Changes,
	}
	if rep.To != 0 {
		rec["from"], rec["to"] = rep.From, rep.To
		rec["live"] = rep.Live
		if rep.MaxSize != 0 {
			rec["maxSize"] = rep.MaxSize
		}
	}
	if rep.Err != "" {
		rec["err"] = rep.Err
	}
	m.trace.Record(rec)
}

// round3 keeps shedFrac readable in a trace meant to be scanned by eye.
func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

// AVDNames lists the AVDs registered as targets, running or not.
func (m *Manager) AVDNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for id := range m.targets {
		if name, ok := strings.CutPrefix(id, "avd:"); ok {
			out = append(out, name)
		}
	}
	return out
}

// SimulatorNames lists the names of the simulators registered as targets, as
// of the last listing.
func (m *Manager) SimulatorNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, t := range m.targets {
		if s, ok := t.(*target.Simulator); ok {
			out = append(out, s.Info().Name)
		}
	}
	return out
}
