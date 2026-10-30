// Command loupe streams an Android device or emulator to a browser at 60fps.
//
// It is a single static binary that needs only adb at runtime: the
// scrcpy-server jar is embedded and staged to disk on demand, so it can be
// dropped onto a machine inside someone else's network without asking them to
// install a toolchain.
package main

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bmcreations/loupe/internal/awake"
	"github.com/bmcreations/loupe/internal/clipshare"
	"github.com/bmcreations/loupe/internal/config"
	"github.com/bmcreations/loupe/internal/httpapi"
	"github.com/bmcreations/loupe/internal/peer"
	"github.com/bmcreations/loupe/internal/record"
	scrcpyserver "github.com/bmcreations/loupe/internal/scrcpy/server"
	"github.com/bmcreations/loupe/internal/target"
	"github.com/bmcreations/loupe/internal/trace"
	"github.com/bmcreations/loupe/web"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

// tunnelMTU is the MTU of a Tailscale tun device, and the smallest link this
// server is expected to be reached over.
const tunnelMTU = 1280

// quicInitialPacketSize is the UDP payload quic-go pads Initial packets to.
//
// quic-go defaults this to 1280, which is the tunnel MTU rather than what fits
// inside it: with headers those datagrams are 1308 bytes on the wire over IPv4
// and 1328 over IPv6, and Tailscale drops both. 1200 is quic-go's
// MinInitialPacketSize and leaves room for either header. Path MTU discovery
// stays enabled, so a link that can carry more grows past this after the
// handshake.
const quicInitialPacketSize = 1200

func quicConfig(tr *trace.Writer) *quic.Config {
	return &quic.Config{
		InitialPacketSize: quicInitialPacketSize,
		Tracer:            quicTracer(tr),
		// Reliable stream resets, which is what lets the sender abandon a
		// frame that has gone stale mid-flight while keeping the bytes the
		// peer already has. webtransport-go turns this on for its own
		// listener regardless; it is set here so the dependency is visible
		// where the config is read, and so a config built for any other
		// listener carries it too.
		EnableStreamResetPartialDelivery: true,
	}
}

// settingsWebTransportMaxSessions is SETTINGS_WEBTRANSPORT_MAX_SESSIONS from
// draft-ietf-webtrans-http3-07 and later, which is what Safari looks for.
// webtransport-go advertises only draft-02's SETTINGS_ENABLE_WEBTRANSPORT
// (0x2b603742), so without this the browser reads the SETTINGS frame, finds
// nothing it recognises, and closes the connection with H3_NO_ERROR before
// sending its CONNECT request. Nothing reaches the handler, so the server sees
// a clean handshake and then a polite goodbye.
const settingsWebTransportMaxSessions = 0xc671706a

// maxWebTransportSessions is what that setting advertises. It needs headroom
// over the number of viewers: a reload opens the new session before the old
// one has finished closing.
const maxWebTransportSessions = 16

// Session-level limits on what one client may open and send. A session needs a
// single control stream; everything above that is slack so the limit is never
// the thing that breaks, since running out of session credit stalls silently
// rather than erroring.
const (
	maxSessionStreams = 64
	maxSessionData    = 16 << 20
)

// newWebTransportServer builds the HTTP/3 server WebTransport runs on.
//
// It is a function rather than a literal in main because each of these fields
// is load-bearing in a way that fails silently. webtransport-go sets none of
// them for you, and the failures all look like a healthy handshake followed by
// the browser hanging up:
//
//   - Without NextProtos the server selects no ALPN, because webtransport-go
//     hands this TLS config straight to quic.ListenEarly rather than letting
//     http3.Server fill it in. Clients drop the connection with CRYPTO_ERROR
//     0x178 right after TLS.
//   - Without ConfigureHTTP3Server there is no SETTINGS_ENABLE_WEBTRANSPORT,
//     no HTTP/3 datagram support, and no ConnContext stashing the QUIC
//     connection. Upgrade reads that connection back out of the request
//     context, so every CONNECT fails with "missing QUIC connection" even
//     though the handshake and SETTINGS look fine.
//   - Without Config the session-level stream and data limits are never
//     advertised, and Safari reads an absent limit as zero. Every stream it
//     opens then blocks forever on credit that never arrives: the promise from
//     createBidirectionalStream or createUnidirectionalStream neither resolves
//     nor rejects. Server-opened streams still work, because those run on
//     Safari's own limits, so the session looks alive the whole time.
func newWebTransportServer(addr string, cert tls.Certificate, tr *trace.Writer) *webtransport.Server {
	srv := &webtransport.Server{
		H3: &http3.Server{
			Addr: addr,
			TLSConfig: &tls.Config{
				Certificates: []tls.Certificate{cert},
				NextProtos:   []string{http3.NextProtoH3},
			},
			QUICConfig:         quicConfig(tr),
			AdditionalSettings: webTransportSettings(),
			// Stash the QUIC connection so the pacer can read the path round
			// trip from the transport, which measures it per ACK, instead of
			// waiting for the phone to POST a number over the link it is
			// throttling. Set before ConfigureHTTP3Server, which chains to an
			// existing ConnContext rather than replacing it -- setting it after
			// would drop webtransport-go's own hook and reproduce trap 14's
			// `missing QUIC connection`.
			ConnContext: httpapi.WithQUICConn,
		},
		Config: sessionFlowControl(),
	}
	webtransport.ConfigureHTTP3Server(srv.H3)
	return srv
}

// sessionFlowControl is what the client is allowed to open and send inside one
// WebTransport session, advertised in SETTINGS and topped up by capsules.
//
// The client needs exactly one bidirectional stream, for control. The rest of
// this is headroom: the limits cost nothing when unused, and a client that
// runs out does not fail loudly, it stalls.
func sessionFlowControl() *webtransport.Config {
	return &webtransport.Config{
		MaxIncomingStreams:    maxSessionStreams,
		MaxIncomingUniStreams: maxSessionStreams,
		MaxIncomingData:       maxSessionData,
	}
}

// webTransportSettings is merged into the HTTP/3 SETTINGS frame.
//
// This is the draft-07 spelling of the max-sessions setting, which is what
// Safari 27 responds to. It has to be set here because webtransport-go v0.13.0
// advertises draft-15's 0x14e9cd29 under that name instead. Both go out, along
// with the draft-02 and draft-13 enable settings ConfigureHTTP3Server adds to
// this same map. A browser that finds no setting it recognises closes with
// H3_NO_ERROR before it ever sends CONNECT.
func webTransportSettings() map[uint64]uint64 {
	return map[uint64]uint64{settingsWebTransportMaxSessions: maxWebTransportSessions}
}

type avdList []string

func (a *avdList) String() string     { return strings.Join(*a, ",") }
func (a *avdList) Set(v string) error { *a = append(*a, v); return nil }

// version is set at release by -ldflags "-X main.version=..."; a plain go
// build says dev.
var version = "dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println(version)
		os.Exit(0)
	}
	// `loupe mcp` is a client of a running server, with flags of its own, and
	// must not print the banner: its stdout is the MCP wire.
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		os.Exit(runMCP(os.Args[2:]))
	}
	// So are `loupe pair` and `loupe sessions`, which use the master token
	// to mint codes and manage sessions over loopback.
	if len(os.Args) > 1 && os.Args[1] == "pair" {
		os.Exit(runPair(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "sessions" {
		os.Exit(runSessions(os.Args[2:]))
	}
	tightenOutput(os.Stdout)
	tightenOutput(os.Stderr)
	def := config.Default()
	var (
		configPath    = flag.String("config", "", "settings file to read and write (default: the per-user application support dir)")
		addr          = flag.String("addr", def.Addr, "listen address")
		adbPath       = flag.String("adb", "", "path to adb (default: adb on PATH)")
		serverJar     = flag.String("scrcpy-server", "", "path to a scrcpy-server jar to use instead of the embedded one")
		serverVersion = flag.String("scrcpy-version", "", "version string for -scrcpy-server; must match that jar exactly")
		emulatorPath  = flag.String("emulator", "", "path to the emulator binary (default: the one in the Android SDK, then emulator on PATH)")
		androidSDK    = flag.String("android-sdk", "", "Android SDK root (default: ANDROID_HOME, ANDROID_SDK_ROOT, then ~/Library/Android/sdk)")
		avdManager    = flag.String("android-avdmanager", "", "path to avdmanager, which creates AVDs from installed system images (default: cmdline-tools/*/bin in the Android SDK)")
		androidCLI    = flag.String("android-cli", "", "path to Google's android CLI, which creates AVDs from the picker (default: /usr/local/bin/android, then /opt/homebrew/bin/android)")
		gpu           = flag.String("gpu", "host", "emulator -gpu mode; only 'host' reaches 60fps")
		synthetic     = flag.Bool("synthetic", false, "register a synthetic 60fps test pattern target")
		noDevices     = flag.Bool("no-devices", false, "skip adb device discovery")
		xcrunnerPath  = flag.String("xcrunner", ".build/xcrunner", "built XCUITest runner (make xcrunner TEAM=...); iPhones below iOS 27 take touches through it over a cable")
		simcapPath    = flag.String("simcap", "", "path to the simcap helper (default: .build/simcap when it exists, then simcap on PATH); iOS simulators are listed only when it exists (make simcap)")
		noSimulators  = flag.Bool("no-simulators", false, "skip iOS simulator discovery")
		noIOSDevices  = flag.Bool("no-ios-devices", false, "skip physical iPhone and iPad discovery through devicectl")
		maxSize       = flag.Int("max-size", def.MaxSize, "cap the longest edge of the video, in pixels")
		maxFPS        = flag.Int("max-fps", def.MaxFPS, "cap the capture frame rate")
		bitrate       = flag.Int("bitrate", def.BitRate, "video bitrate in bits per second; raise it on a LAN, lower it over a relay")
		videoCodec    = flag.String("video-codec", def.VideoCodec, "device-side encoder: h264, h265 or av1; only h264 is known to decode in the browser")
		keyframeSecs  = flag.Int("keyframe-interval", def.KeyframeIntervalSec, "seconds between keyframes; bounds how long any wait-for-keyframe stall can last; 0 is 1 on Android, 5 on a simulator")
		maxBufferMs   = flag.Int("max-buffer", def.MaxBufferMs, "ceiling on the client's jitter buffer in ms; absorbs clumped arrivals at the cost of that much added tap latency")
		humanHoldMs   = flag.Int("human-hold", def.HumanHoldMs, "ms a viewer's last input keeps agents' /api/input off the device")
		adaptive      = flag.Bool("adaptive-bitrate", def.AdaptiveBitRate, "relaunch the encoder below -bitrate when the link cannot carry what it produces, and probe back up when it can")
		emuPauseSec   = flag.Int("emulator-pause", def.EmulatorPauseSec, "seconds a headless emulator goes unused before it is paused; 0 never pauses")
		emuShutSec    = flag.Int("emulator-shutdown", def.EmulatorShutdownSec, "seconds a headless emulator goes unused before it is shut down; 0 never shuts one down")
		emuKeepFlag   = flag.Bool("emulator-keep-state", def.EmulatorKeepState, "save an emulator's state into its snapshot before shutting it down, so the next boot resumes it")
		simSlimFlag   = flag.Bool("simulator-slim", def.SimulatorSlim, "boot simulators with background daemons disabled, which frees most of their memory")
		shareClipFlag = flag.Bool("shared-clipboard", def.SharedClipboard, "share one clipboard across every open target and this Mac's pasteboard")
		emuWindowFlag = flag.Bool("emulator-window", def.EmulatorWindow, "open the emulator's window when booting an AVD; off runs it headless with no audio")
		tracePath     = flag.String("trace", "", "write a JSON-lines trace of per-second stream metrics to this file")
		t3Projects    = flag.String("t3-projects", "", "comma-separated T3 Code project paths whose threads a flag can be sent to")
		tlsCert       = flag.String("tls-cert", "", "TLS certificate; serve HTTPS directly instead of behind a proxy")
		tlsKey        = flag.String("tls-key", "", "TLS private key, required with -tls-cert")
		insecureHTTP  = flag.Bool("insecure-http", false, "serve plain HTTP on an address other machines can reach; without it, plain HTTP is loopback only")
		token         = flag.String("token", "", "shared token every request must present (default: read or create one in the cache dir)")
		noAuth        = flag.Bool("no-auth", false, "serve with no token at all; anyone who can reach the port can drive any device")
		avds          avdList
	)
	flag.Var(&avds, "avd", "AVD to boot at startup; repeatable. Every installed AVD is listed either way")
	flag.Parse()
	*simcapPath = resolveSimcap(*simcapPath)

	// Settings come from three places and the API has to be able to say which.
	// A flag given explicitly outranks the file for the life of the process, so
	// a write through /api/config would otherwise report success and then do
	// nothing -- the failure mode this whole path exists to avoid.
	cfgPath, cfg, pinned, err := settle(flag.CommandLine, *configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
	if !pinned["addr"] {
		*addr = cfg.Addr
	}
	if !pinned["maxSize"] {
		*maxSize = cfg.MaxSize
	}
	if !pinned["maxFps"] {
		*maxFPS = cfg.MaxFPS
	}
	if !pinned["bitRate"] {
		*bitrate = cfg.BitRate
	}
	if !pinned["videoCodec"] {
		*videoCodec = cfg.VideoCodec
	}
	if !pinned["keyframeIntervalSec"] {
		*keyframeSecs = cfg.KeyframeIntervalSec
	}
	if !pinned["maxBufferMs"] {
		*maxBufferMs = cfg.MaxBufferMs
	}
	if !pinned["humanHoldMs"] {
		*humanHoldMs = cfg.HumanHoldMs
	}
	if !pinned["adaptiveBitRate"] {
		*adaptive = cfg.AdaptiveBitRate
	}
	if !pinned["emulatorWindow"] {
		*emuWindowFlag = cfg.EmulatorWindow
	}
	if !pinned["emulatorKeepState"] {
		*emuKeepFlag = cfg.EmulatorKeepState
	}
	if !pinned["simulatorSlim"] {
		*simSlimFlag = cfg.SimulatorSlim
	}
	if !pinned["sharedClipboard"] {
		*shareClipFlag = cfg.SharedClipboard
	}
	if !pinned["emulatorPauseSec"] {
		*emuPauseSec = cfg.EmulatorPauseSec
	}
	if !pinned["emulatorShutdownSec"] {
		*emuShutSec = cfg.EmulatorShutdownSec
	}
	// Read at each boot, so a PUT reaches the next AVD this server starts.
	var emuWindow atomic.Bool
	emuWindow.Store(*emuWindowFlag)
	// Read at each shutdown, so a PUT reaches emulators already running.
	var emuKeep atomic.Bool
	emuKeep.Store(*emuKeepFlag)
	if !pinned["tlsCert"] {
		*tlsCert = cfg.TLSCert
	}
	if !pinned["tlsKey"] {
		*tlsKey = cfg.TLSKey
	}
	if !pinned["tracePath"] {
		*tracePath = cfg.TracePath
	}
	if !pinned["t3Projects"] {
		*t3Projects = cfg.T3Projects
	}

	useTLS := *tlsCert != "" && *tlsKey != ""
	if (*tlsCert == "") != (*tlsKey == "") {
		fmt.Fprintln(os.Stderr, "error: -tls-cert and -tls-key must be given together")
		os.Exit(2)
	}
	if err := checkPlainHTTP(*addr, useTLS, *insecureHTTP); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}
	if !useTLS && !loopbackOnly(*addr) {
		fmt.Fprintln(os.Stderr, "WARNING: -insecure-http: serving plain HTTP beyond loopback; "+
			"the token and the stream cross the network in the clear")
	}

	// Tracing is opt-in: it joins the server's counters with the metrics only
	// the client can measure, one record per second, so a session can be
	// diagnosed from the file rather than from a screenshot.
	var tracer *trace.Writer
	if *tracePath != "" {
		t, err := trace.Open(*tracePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: tracing disabled: %v\n", err)
		} else {
			tracer = t
			defer tracer.Close()
			fmt.Printf("trace: %s\n", *tracePath)
		}
	}

	opts := target.Options{
		MaxSize:             *maxSize,
		MaxFPS:              *maxFPS,
		BitRate:             *bitrate,
		VideoCodec:          *videoCodec,
		KeyframeIntervalSec: *keyframeSecs,
	}
	mgr := httpapi.NewManager(opts)
	mgr.SetClientBuffer(*maxBufferMs)
	mgr.SetHumanHold(time.Duration(*humanHoldMs) * time.Millisecond)
	mgr.SetEmulatorPause(time.Duration(*emuPauseSec) * time.Second)
	mgr.SetEmulatorShutdown(time.Duration(*emuShutSec) * time.Second)
	mgr.SetAdaptiveBitRate(*adaptive)
	mgr.SetSimulatorSlim(*simSlimFlag)
	// Off until sharedClipboard is: the Mac's pasteboard can hold passwords.
	share := clipshare.New()
	share.SetEnabled(*shareClipFlag)
	mgr.SetClipShare(share)
	adb := target.ADB{Path: *adbPath}

	needScrcpy := !*noDevices
	jar, version := *serverJar, *serverVersion
	if needScrcpy {
		var err error
		jar, version, err = resolveScrcpy(jar, version)
		if err != nil {
			// Not fatal: the synthetic target still works, and that is enough to
			// validate the client half. Say so plainly rather than exiting.
			fmt.Fprintf(os.Stderr, "warning: %v\n", err)
			fmt.Fprintf(os.Stderr, "         device and emulator targets are disabled; run with -synthetic to test the client\n")
			needScrcpy = false
		} else {
			fmt.Printf("scrcpy-server %s at %s\n", version, jar)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	go mgr.PauseIdleEmulators(ctx)
	defer cancel()

	registered := 0
	var registerAVDs func(time.Time, []target.AVD)
	// Closed when the device watcher has exited, so shutdown can wait for it
	// to kill its adb child rather than orphaning one per restart -- and the
	// supervisor restarts on every build.
	watcherDone := make(chan struct{})

	if needScrcpy {
		emu := *emulatorPath
		if emu == "" {
			emu = target.EmulatorBinary(target.SDKRoot(*androidSDK))
		}
		scannedAt := time.Now()
		installed, err := target.ScanAVDs(target.AVDHome())
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: listing AVDs: %v\n", err)
		}
		byName := map[string]*target.Emulator{}
		// registerAVDs makes a target of each AVD not already one. It runs at
		// startup and again after /api/avds creates one, so a new AVD is in
		// the picker without a restart. One deleted after the scan began is
		// skipped, since the scan could still read its .ini.
		registerAVDs = func(scannedAt time.Time, installed []target.AVD) {
			for _, a := range installed {
				if _, ok := mgr.Target("avd:" + a.Name); ok {
					continue
				}
				e := target.NewEmulator(adb, target.EmulatorConfig{
					AVD:          a.Name,
					DisplayName:  a.DisplayName,
					Desc:         a.Description(),
					EmulatorPath: emu,
					GPU:          *gpu,
					Dir:          a.Dir,
					Home:         target.AVDHome(),
					Trace:        tracer.Record,
					Window:       emuWindow.Load,
					KeepState:    emuKeep.Load,
				}, jar, version)
				if !mgr.RegisterScanned(e, scannedAt) {
					continue
				}
				byName[a.Name] = e
				fmt.Printf("target: %s (AVD)\n", e.ID())
				registered++
			}
		}
		registerAVDs(scannedAt, installed)
		// -avd boots at startup, so the first viewer does not wait out a boot.
		// An AVD it names that the scan did not find is still tried: the
		// emulator's own lookup may know somewhere this one does not.
		for _, name := range avds {
			e, ok := byName[name]
			if !ok {
				e = target.NewEmulator(adb, target.EmulatorConfig{
					AVD: name, EmulatorPath: emu, GPU: *gpu,
					Dir:       filepath.Join(target.AVDHome(), name+".avd"),
					Home:      target.AVDHome(),
					Trace:     tracer.Record,
					Window:    emuWindow.Load,
					KeepState: emuKeep.Load,
				}, jar, version)
				mgr.Register(e)
				registered++
			}
			go func() {
				if err := e.Boot(ctx); err != nil {
					fmt.Fprintf(os.Stderr, "warning: booting %s: %v\n", name, err)
				}
			}()
		}
	}

	// Devices go in after the AVDs, so the sync can see which serials those
	// already own. Every device is registered, ready or not: one that is
	// unauthorized is listed with the reason, which is how the user learns to
	// accept the prompt.
	if needScrcpy && !*noDevices {
		build := func(d target.DeviceInfo) target.Target {
			return target.NewADBTarget(adb, d, jar, version)
		}
		names := target.NewAVDResolver(adb)
		devices, err := adb.Devices(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: adb device discovery failed: %v\n", err)
		}
		devices = names.Resolve(ctx, devices)
		for _, d := range devices {
			switch {
			case d.AVD != "":
				fmt.Printf("target: %s (running avd:%s)\n", d.Serial, d.AVD)
			case d.Emulator:
				// An emulator whose AVD this scan did not find is still an adb
				// device and streams fine, so register it rather than skipping.
				fmt.Printf("target: %s (running emulator)\n", d.Label())
			case !d.Ready():
				fmt.Printf("target: %s (not ready, adb state is %q)\n", d.Serial, d.State)
			default:
				fmt.Printf("target: %s\n", d.Label())
			}
			if d.Ready() {
				registered++
			}
		}
		mgr.SyncDevices(devices, build)
		go func() {
			defer close(watcherDone)
			watchDevices(ctx, adb, names, mgr, build, tracer)
		}()
	} else {
		close(watcherDone)
	}

	// Simulators are listed only when simcap is built. A simulator in the
	// picker that fails every open with "simcap is not built" is a target
	// the viewer can see and cannot watch, which is worse than its absence.
	// simcapBin is set only once simctl lists simulators and simcap runs; a
	// simulator created without both would never reach the picker.
	var simcapBin string
	if !*noSimulators && runtime.GOOS == "darwin" {
		if bin, err := filepath.Abs(*simcapPath); err != nil {
			fmt.Fprintf(os.Stderr, "warning: simcap path: %v\n", err)
		} else if _, err := os.Stat(bin); err != nil {
			fmt.Printf("simulators: not listed, %s is not built (make simcap)\n", bin)
		} else if err := target.CheckSimcap(ctx, bin); err != nil {
			fmt.Printf("simulators: not listed, %v (make simcap)\n", err)
		} else if sims, err := target.ListSimulators(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "warning: iOS simulator discovery failed: %v\n", err)
		} else {
			mgr.SyncSimulators(sims, bin)
			for _, sim := range sims {
				fmt.Printf("target: %s (%s, iOS %s, %s)\n", target.SimulatorID(sim.UDID), sim.Name, sim.OSVersion, sim.State)
			}
			registered += len(sims)
			simcapBin = bin
			go watchSimulators(ctx, mgr, bin)
		}
	}

	// The Mac's own pasteboard joins the shared clipboard whenever simcap is
	// built, simulators or not. It runs only while sharedClipboard is on.
	if runtime.GOOS == "darwin" {
		if bin, err := filepath.Abs(*simcapPath); err == nil {
			if _, err := os.Stat(bin); err == nil {
				go clipshare.RunHostPasteboard(ctx, share, bin, tracer.Record)
			}
		}
	}

	// Physical iPhones and iPads, over a cable or Wi-Fi. They are not
	// counted in registered: only one on a cable streams, so a server with
	// only iPhones still needs the pattern to show anything.
	if !*noIOSDevices && runtime.GOOS == "darwin" {
		if bin, err := filepath.Abs(*simcapPath); err == nil && target.CheckSimcap(ctx, bin) == nil {
			mgr.SetIOSSimcap(bin)
		}
		// Set whether or not it is built yet: a touch looks for the
		// .xctestrun itself, so a `make xcrunner` needs no restart.
		if dir, err := filepath.Abs(*xcrunnerPath); err == nil {
			mgr.SetXCRunner(dir)
			if runs, _ := filepath.Glob(filepath.Join(dir, "*.xctestrun")); len(runs) == 1 {
				fmt.Printf("xcrunner: %s\n", runs[0])
			} else {
				fmt.Printf("xcrunner: not built in %s yet (make xcrunner TEAM=...)\n", dir)
			}
		}
		if devs, err := target.ListIOSDevices(ctx); err != nil {
			fmt.Printf("ios devices: not listed, %v\n", err)
		} else {
			mgr.SyncIOSDevices(devs)
			for _, d := range devs {
				fmt.Printf("target: %s (%s, iOS %s, %s)\n", target.IOSDeviceID(d), d.Name, d.OSVersion, cmp.Or(d.Transport, "unreachable"))
			}
			go watchIOSDevices(ctx, mgr)
		}
	}

	if *synthetic || registered == 0 {
		mgr.Register(&target.Synthetic{})
		fmt.Println("target: synthetic 60fps test pattern")
		registered++
	}

	// What the process is actually running with, flags included. It is not
	// the file: a flag pins its setting for the life of the process, so the
	// two diverge the moment the API writes one that a flag already holds.
	inForce := config.Config{
		Addr:                *addr,
		MaxSize:             *maxSize,
		MaxFPS:              *maxFPS,
		BitRate:             *bitrate,
		VideoCodec:          *videoCodec,
		KeyframeIntervalSec: *keyframeSecs,
		MaxBufferMs:         *maxBufferMs,
		HumanHoldMs:         *humanHoldMs,
		AdaptiveBitRate:     *adaptive,
		EmulatorWindow:      *emuWindowFlag,
		EmulatorPauseSec:    *emuPauseSec,
		EmulatorShutdownSec: *emuShutSec,
		EmulatorKeepState:   *emuKeepFlag,
		SimulatorSlim:       *simSlimFlag,
		SharedClipboard:     *shareClipFlag,
		TLSCert:             *tlsCert,
		TLSKey:              *tlsKey,
		TracePath:           *tracePath,
		T3Projects:          *t3Projects,
	}

	api := httpapi.NewServer(mgr, web.FS, tracer)
	api.SetAAPT2(target.AAPT2Binary(target.SDKRoot(*androidSDK)))
	// Hands-off sits beside the config, so a spare server with its own
	// -config keeps its own set.
	if err := api.SetHandsOffPath(filepath.Join(filepath.Dir(cfgPath), "handsoff.json")); err != nil {
		fmt.Fprintf(os.Stderr, "warning: hands-off list unreadable, starting empty: %v\n", err)
	}
	// Paired devices' sessions and pending codes, hashed. Beside hands-off
	// for the same reason: a spare server keeps its own.
	if err := api.SetSessionsPath(filepath.Join(filepath.Dir(cfgPath), "sessions.json")); err != nil {
		fmt.Fprintf(os.Stderr, "warning: sessions unreadable, pairing off and only the master token works: %v\n", err)
	}
	// Programs and webhooks a flag can go to. A file beside the config, not a
	// field in it: PUT /api/config would let any token holder name a program.
	api.SetDestinationsPath(filepath.Join(filepath.Dir(cfgPath), "destinations.json"))
	// The peers' tokens live here, so it is a file beside hands-off and not a
	// config field. The trace gets a peer's name and URL, never its token.
	peers, err := peer.NewRegistry(peer.NewStore(filepath.Join(filepath.Dir(cfgPath), "servers.json")), tracer.Record)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: servers list unreadable, starting empty: %v\n", err)
	}
	// Before SetPeers, whose first sync starts a bridge per peer.
	api.SetClipShare(share)
	api.SetPeers(peers)
	peers.Start(ctx)
	recDir := filepath.Join(scrcpyserver.DefaultDir(), "recordings")
	recStore, err := record.NewStore(recDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: recordings disabled: %v\n", err)
		recStore = nil
	} else {
		api.SetRecordings(recStore)
	}
	if registerAVDs != nil {
		// The CLI ignores ANDROID_AVD_HOME, so with it pointing elsewhere an
		// AVD would land where loupe never looks. Offer no create then.
		cli := target.AndroidCLI(*androidCLI)
		switch {
		case cli == "":
		case filepath.Clean(target.AVDHome()) != filepath.Clean(target.CLIAVDHome()):
			fmt.Fprintf(os.Stderr, "warning: not offering the android CLI's New device profiles: AVDs are read from %s and the android CLI creates them in %s\n", target.AVDHome(), target.CLIAVDHome())
		default:
			api.SetAVDCreator(httpapi.NewAVDCreator(cli, target.SDKRoot(*androidSDK), target.AVDHome(), registerAVDs, tracer.Record))
		}
	}
	// New device… from installed images. It does not depend on device
	// discovery for iOS (simcapBin), and avdmanager follows ANDROID_AVD_HOME,
	// so unlike the android CLI it needs no AVD-home check.
	sdkRoot := target.SDKRoot(*androidSDK)
	avdMgr := ""
	if registerAVDs != nil {
		avdMgr = target.AVDManagerBinary(sdkRoot, *avdManager)
	}
	if dc := httpapi.NewDeviceCreator(httpapi.DeviceCreatorConfig{
		Manager: mgr, SDK: sdkRoot, AVDManager: avdMgr, AVDHome: target.AVDHome(),
		RegisterAVDs: registerAVDs, Simcap: simcapBin, Trace: tracer.Record,
	}); dc != nil {
		api.SetDeviceCreator(dc)
	}
	api.KeepAwake(awake.New(awake.System, func(on bool, err error) {
		rec := map[string]any{"kind": "awake", "on": on}
		switch {
		case err != nil:
			rec["error"] = err.Error()
			fmt.Fprintf(os.Stderr, "warning: keep awake: %v\n", err)
		case on:
			fmt.Println("keep awake: holding off idle sleep")
		default:
			fmt.Println("keep awake: released")
		}
		tracer.Record(rec)
	}))
	// Live settings reach running code through the manager, which hands a
	// snapshot to each new session. An open session keeps what it started with.
	api.SetConfig(httpapi.NewConfigStore(cfgPath, cfg, pinned, inForce, func(c config.Config) {
		mgr.SetOptions(target.Options{
			MaxSize:             c.MaxSize,
			MaxFPS:              c.MaxFPS,
			BitRate:             c.BitRate,
			VideoCodec:          c.VideoCodec,
			KeyframeIntervalSec: c.KeyframeIntervalSec,
		})
		mgr.SetClientBuffer(c.MaxBufferMs)
		mgr.SetHumanHold(time.Duration(c.HumanHoldMs) * time.Millisecond)
		mgr.SetAdaptiveBitRate(c.AdaptiveBitRate)
		emuWindow.Store(c.EmulatorWindow)
		emuKeep.Store(c.EmulatorKeepState)
		mgr.SetSimulatorSlim(c.SimulatorSlim)
		share.SetEnabled(c.SharedClipboard)
		mgr.SetEmulatorPause(time.Duration(c.EmulatorPauseSec) * time.Second)
		mgr.SetEmulatorShutdown(time.Duration(c.EmulatorShutdownSec) * time.Second)
	}))
	fmt.Printf("config: %s\n", cfgPath)
	fmt.Printf("client build: %s\n", api.Build())

	// Auth is on by default now that /api/input exists. Reaching the port used
	// to mean you could watch a device; it now means you can drive one, and
	// the tailnet is not a small enough audience for that.
	authToken := ""
	if !*noAuth {
		var tokErr error
		if authToken, tokErr = resolveToken(*token); tokErr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", tokErr)
			os.Exit(1)
		}
		api.SetToken(authToken)
	} else {
		fmt.Println("WARNING: -no-auth: anyone who can reach this port can drive any target")
	}

	handler := api.Handler()

	srv := &http.Server{
		Addr:    *addr,
		Handler: handler,
		// Long-lived WebSockets must not be cut off by a write timeout, so only
		// the header read is bounded.
		ReadHeaderTimeout: 10 * time.Second,
	}

	// WebTransport runs over HTTP/3, which is QUIC over UDP, so it needs its
	// own listener on the same port number and a real certificate. It cannot
	// be proxied by anything that speaks only TCP, which is the tradeoff for
	// getting a transport that can cancel a frame already in flight.
	var wtSrv *webtransport.Server
	var quicLn *quicListener
	var bannerCert *tls.Certificate
	if useTLS {
		cert, certErr := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if certErr == nil {
			bannerCert = &cert
		}
		if certErr != nil {
			fmt.Fprintf(os.Stderr, "warning: webtransport disabled: %v\n", certErr)
		} else {
			wtSrv = newWebTransportServer(*addr, cert, tracer)

			// The HTTP/3 handler needs the Server's own upgrade path, so wire
			// it after construction.
			wtSrv.H3.Handler = handler
			api.EnableWebTransport(wtSrv)

			// One UDP listener for /wt and the native client's QUIC, told
			// apart by ALPN (quiclisten.go).
			ln, err := listenQUIC(*addr, wtSrv)
			if err != nil {
				fmt.Fprintf(os.Stderr, "webtransport: %v\n", err)
			} else {
				quicLn = ln
				go quicLn.serve(wtSrv, api)
			}
		}
	}

	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	advertised := advertiseURLs(*addr, scheme)
	// The banner's phone link is also what a native viewer hands to a
	// browser on another device (/api/handoff).
	handoff := phoneURL(bannerCert, useTLS, *addr, authToken, advertised)
	api.SetHandoffURL(handoff)

	go func() {
		banner{
			addr:         *addr,
			scheme:       scheme,
			handoff:      handoff,
			advertised:   advertised,
			token:        authToken,
			tokenWhere:   tokenWhere(*token),
			webTransport: wtSrv != nil,
			tls:          useTLS,
		}.write(os.Stdout, isTerminal(os.Stdout))

		var err error
		if useTLS {
			err = srv.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "server: %v\n", err)
			os.Exit(1)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig

	fmt.Println("\nshutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
	if quicLn != nil {
		_ = quicLn.Close()
	}
	if wtSrv != nil {
		_ = wtSrv.Close()
	}
	cancel()
	mgr.Shutdown()
	// Sessions closing end every recording as sessionEnded; this waits for
	// each to finish its file and metadata.
	if recStore != nil && !recStore.Wait(5*time.Second) {
		fmt.Fprintf(os.Stderr, "warning: recordings: timed out closing\n")
	}
	select {
	case <-watcherDone:
	case <-time.After(time.Second):
	}
}

var versionRe = regexp.MustCompile(`(\d+\.\d+(?:\.\d+)?)`)

// resolveScrcpy returns the server jar to push and the version string to launch
// it with.
//
// The embedded jar is the default and the only thing that is version-pinned.
// A host install is never picked up implicitly: loupe hardcodes constants read
// out of a specific dex, and scrcpy has changed them between releases without
// changing anything that fails loudly, so inheriting whatever version happens
// to be installed is a silent misparse waiting to happen.
//
// An explicit -scrcpy-server or LOUPE_SCRCPY_SERVER still wins, because
// pointing loupe at a locally built jar is how the next version gets tested.
// That path has to determine a version, and the version is not cosmetic:
// scrcpy-server exits on a mismatch before opening its socket, so getting it
// wrong surfaces as a connect timeout with nothing explaining it.
func resolveScrcpy(jar, version string) (string, string, error) {
	if jar == "" {
		jar = os.Getenv("LOUPE_SCRCPY_SERVER")
	}
	if version == "" {
		version = os.Getenv("LOUPE_SCRCPY_VERSION")
	}

	if jar == "" {
		path, err := scrcpyserver.Materialize("")
		if err != nil {
			return "", "", err
		}
		if version == "" {
			version = scrcpyserver.Version
		}
		return path, version, nil
	}

	if _, err := os.Stat(jar); err != nil {
		return "", "", fmt.Errorf("scrcpy-server jar %s: %w", jar, err)
	}
	if version == "" {
		out, err := exec.Command("scrcpy", "--version").Output()
		if err != nil {
			return "", "", fmt.Errorf("-scrcpy-server %s was given without -scrcpy-version, and `scrcpy --version` is not available to detect it", jar)
		}
		m := versionRe.FindStringSubmatch(string(out))
		if m == nil {
			return "", "", fmt.Errorf("could not parse a version from `scrcpy --version`; pass -scrcpy-version")
		}
		version = m[1]
	}
	return jar, version, nil
}

// advertiseURLs lists the URLs worth printing at startup, including the LAN
// address, because the entire point is opening this on a phone.
func advertiseURLs(addr, scheme string) []string {
	port := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		port = addr[i+1:]
	}
	urls := []string{scheme + "://localhost:" + port}

	ifaces, err := netInterfaceIPs()
	if err != nil {
		return urls
	}
	for _, ip := range ifaces {
		urls = append(urls, scheme+"://"+ip+":"+port)
	}
	return urls
}

// tokenFileName is where a generated token is kept between runs.
const tokenFileName = "token"

// resolveToken settles on the shared secret, in the order: the -token flag,
// LOUPE_TOKEN, a token saved from a previous run, then a fresh one.
//
// Persisting it is what makes this usable rather than merely secure. The
// supervisor restarts the server on every build, and a token generated per
// start would invalidate the bookmarked link on the phone each time, which
// ends with the token being turned off.
func resolveToken(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if env := os.Getenv("LOUPE_TOKEN"); env != "" {
		return env, nil
	}

	dir := scrcpyserver.DefaultDir()
	path := filepath.Join(dir, tokenFileName)

	if b, readErr := os.ReadFile(path); readErr == nil {
		if tok := strings.TrimSpace(string(b)); tok != "" {
			return tok, nil
		}
	}

	tok := httpapi.NewToken()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	// 0600: this is the only credential loupe has.
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Printf("token: new one written to %s\n", path)
	return tok, nil
}

// settle reads the settings file and works out which fields the command line
// has pinned. A missing file is not an error: it is a first run, and the
// defaults are written out so there is something for a settings UI to edit.
func settle(fs *flag.FlagSet, path string) (string, config.Config, map[string]bool, error) {
	pinned := map[string]bool{}
	fs.Visit(func(f *flag.Flag) {
		if name, ok := config.FieldForFlag(f.Name); ok {
			pinned[name] = true
		}
	})

	if path == "" {
		p, err := config.DefaultPath()
		if err != nil {
			return "", config.Default(), pinned, fmt.Errorf("settings: %w", err)
		}
		path = p
	}

	cfg, existed, err := config.Load(path)
	if err != nil {
		return path, config.Default(), pinned, fmt.Errorf("settings: %v; using defaults", err)
	}
	if !existed {
		if err := config.Save(path, cfg); err != nil {
			return path, cfg, pinned, fmt.Errorf("settings: %v", err)
		}
	}
	return path, cfg, pinned, nil
}

// simulatorPoll is how often simctl is listed. There is no track-devices for
// simctl; a listing costs about 0.24s with 19 simulators installed.
const simulatorPoll = 5 * time.Second

// watchSimulators keeps the simulators in the target list in step with
// simctl, so one created, deleted, booted or shut down in Xcode shows up
// without a restart.
func watchSimulators(ctx context.Context, mgr *httpapi.Manager, simcap string) {
	tick := time.NewTicker(simulatorPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		lctx, cancel := context.WithTimeout(ctx, simulatorPoll)
		listedAt := time.Now()
		sims, err := target.ListSimulators(lctx)
		cancel()
		if err != nil {
			// A failed listing is not every simulator deleted. Keep the last
			// one rather than marking them all missing.
			continue
		}
		mgr.SyncSimulatorsAt(listedAt, sims, simcap)
	}
}

// iosDevicePoll is how often devicectl is listed. Like simctl it has no
// stream of changes to follow, and a listing is slower than simctl's, so it
// is read half as often.
const iosDevicePoll = 10 * time.Second

// watchIOSDevices keeps the physical iOS devices in the target list in step
// with devicectl, so a phone paired, plugged in or walked onto the network
// shows up without a restart.
func watchIOSDevices(ctx context.Context, mgr *httpapi.Manager) {
	tick := time.NewTicker(iosDevicePoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		lctx, cancel := context.WithTimeout(ctx, iosDevicePoll)
		devs, err := target.ListIOSDevices(lctx)
		cancel()
		if err != nil {
			// As with simctl, a failed listing is not every device gone.
			continue
		}
		mgr.SyncIOSDevices(devs)
	}
}

// watchDevices keeps the target list in step with adb for the life of the
// process. The list used to be read once at startup, so a phone plugged in
// afterwards never appeared and one unplugged stayed selectable, failing every
// connect with an adb error about a feature set.
//
// track-devices ends when the adb server does, and running it again starts a
// new server, so the loop is also what recovers from `adb kill-server`. It
// does not recover from a server that is running and resets every
// connection, which is what target.ADBHealth is for: after a few failures in
// a row adb targets are reported unavailable with the reason, and after a
// few more the server is restarted.
func watchDevices(ctx context.Context, adb target.ADB, names *target.AVDResolver, mgr *httpapi.Manager, build func(target.DeviceInfo) target.Target, tr *trace.Writer) {
	health := target.NewADBHealth()
	backoff := time.Second
	for {
		heard := false
		err := adb.TrackDevices(ctx, func(ds []target.DeviceInfo) {
			if !heard {
				heard = true
				if health.Heard() {
					mgr.SetADBDown("")
					fmt.Println("adb: reachable again")
					tr.Record(map[string]any{"kind": "adb", "state": "reachable"})
				}
			}
			mgr.SyncDevices(names.Resolve(ctx, ds), build)
		})
		if ctx.Err() != nil {
			return
		}
		if heard {
			backoff = time.Second
		} else {
			backoff = min(backoff*2, 30*time.Second)
			v := health.Failed(time.Now(), err)
			if v.BecameDown {
				mgr.SetADBDown(err.Error())
				fmt.Fprintf(os.Stderr, "adb: unreachable after %d failures in a row: %v\n", v.Failures, err)
				tr.Record(map[string]any{"kind": "adb", "state": "unreachable", "failures": v.Failures, "error": err.Error()})
			}
			if v.Restart {
				start := time.Now()
				pid, rerr := adb.RestartServer(ctx)
				rec := map[string]any{"kind": "adb", "action": "restart", "failures": v.Failures, "tookMs": time.Since(start).Milliseconds()}
				if pid != 0 {
					rec["killedPid"] = pid
				}
				if rerr != nil {
					rec["error"] = rerr.Error()
				}
				fmt.Printf("adb: restarted the server after %d failures (killed pid %d, err %v)\n", v.Failures, pid, rerr)
				tr.Record(rec)
				// Try the new server straight away rather than after 30s.
				backoff = time.Second
			}
		}
		fmt.Fprintf(os.Stderr, "adb track-devices ended: %v; retrying in %s\n", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

// resolveSimcap picks the simcap helper when -simcap is not given: a
// checkout's own build first, so `make simcap` keeps working, then one on
// PATH, which is where Homebrew puts it. With neither, the checkout path
// stands, and the startup line says it is not built.
func resolveSimcap(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	const local = ".build/simcap"
	if _, err := os.Stat(local); err == nil {
		return local
	}
	if p, err := exec.LookPath("simcap"); err == nil {
		return p
	}
	return local
}
