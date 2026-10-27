# Architecture

Moved out of CLAUDE.md on 2026-10-24. Shape, layout, transports, running it,
sleep, auth, configuration, adaptive bitrate, client telemetry, known gaps and
environment notes.

## Shape

```
Android device ──scrcpy──> Go server ──WebSocket or WebTransport──> Safari
                              │                                       │
                         H.264 Annex B                          WebCodecs
                         access units                        VideoDecoder → canvas
```

The server speaks the scrcpy wire protocol (v2.x/3.x) to a device over adb, or
generates a synthetic 60fps test pattern. It reframes the stream into access
units and ships them to the browser, which decodes with WebCodecs and paints to
a canvas. Pointer and key events go back over the same connection.

### Layout

| Path | What it is |
| --- | --- |
| `cmd/loupe/main.go` | Flags, target registration, HTTP/3 + TLS startup |
| `internal/scrcpy/` | Wire protocol: control messages, stream header, framing |
| `internal/scrcpy/server/` | The pinned scrcpy-server jar, embedded and staged for `adb push` |
| `internal/h264/` | Annex B parsing, SPS/PPS, access unit boundaries |
| `internal/session/` | Session lifecycle, `OutMsg`, protocol constants, pacer |
| `internal/session/pacer.go` | End-to-end congestion control (see below) |
| `internal/session/bitrate.go` | Adaptive bitrate policy: when to change, and to what |
| `internal/session/relaunch.go` | Swapping the encoder underneath a live session |
| `internal/httpapi/server.go` | Routes, asset stamping, request tracing |
| `internal/httpapi/conn.go` | `Conn` interface: transport-agnostic send/recv |
| `internal/httpapi/viewer.go` | Shared viewer loop: pacing, keyframe gate, stats |
| `internal/httpapi/webtransport.go` | `wtConn`, one uni stream per access unit |
| `internal/wsx/` | Dependency-free RFC 6455 WebSocket |
| `internal/target/` | ADB device, emulator, and `Synthetic` targets |
| `internal/composetree/` | The compose agent (embedded `.so` per ABI and dex), its socket protocol, the inspector.jar cache and the `GetComposables` decoder |
| `internal/target/compose.go`, `composeannotate.go` | Attaching that agent to the foreground app, and naming uiautomator nodes after composables |
| `tools/composeagent/` | Source for the agent: a JVMTI `.so` that loads a dex hosting Google's Compose inspector (`make composeagent`) |
| `internal/target/avd.go`, `sdk.go` | Installed AVDs read from their `.ini` files, and finding the SDK |
| `internal/target/emulator.go` | One AVD, running or not: boots on open, attaches by name |
| `internal/target/avdsnapshot.go` | The `loupe-warm` snapshot: when to load it, and saving it after a cold boot |
| `internal/target/avdresolve.go` | Which AVD each running emulator serial is |
| `internal/target/simulator.go` | iOS simulators from `simctl`, streamed through simcap |
| `internal/target/iosdevice.go` | Physical iPhones and iPads from `devicectl`, over a cable or Wi-Fi: listed, launched into and screenshotted, and streamed on a cable |
| simcap's `phone.swift` | `simcap -phone`: a physical iPhone's screen-capture device, the one QuickTime records from, into simcap's encoder |
| simcap's `wifi.swift` | `simcap -phone-wifi`: a phone's mirrored stream over Wi-Fi, received as RTP in place of avconferenced, HEVC to H.264 |
| simcap's `encoder.swift` | simcap's H.264 encoder and wire format, shared by a simulator's framebuffer and a phone's capture |
| `tools/xcrunner/` | XCUITest runner for phones below iOS 27: touch through `XCUICoordinate`, reached over usbmux |
| `internal/target/xcrunner.go`, `internal/usbmux/` | Starting that runner with `xcodebuild` and dialling it through usbmuxd |
| simcap's `devices.swift` | `simcap -devices`: which CoreDevice HID capabilities a physical device offers |
| `internal/target/simposture.go` | A Duo's posture, folded through simcap's `hinge` |
| `internal/target/siminput.go` | Simulator input: one `simcap -input` per simulator, keycode and character tables, the CoreDevice route and the Device Hub reclaim behind it |
| `simcap/` | Swift helper: simulator framebuffer to H.264 in scrcpy's wire format, HID input (`input.swift`, and `hub.swift` through CoreDevice) and the accessibility tree (`ax.swift`) |
| `internal/target/ui.go` | uiautomator hierarchy dump and parse |
| `internal/target/frame.go` | `Framer`: emulator skins and derived bezels |
| `internal/target/simframe.go` | `Framer` for simulators: DeviceKit chrome rasterised by `sips` |
| `internal/target/input.go` | `Actuator`: tap/swipe/text/key in device pixels |
| `internal/target/launch.go` | `Launcher`: open an installed app; aapt2 lookup and APK package names |
| `internal/httpapi/agent.go` | `/api/ui` and `/api/input` |
| `internal/httpapi/record.go` | `/api/record`: start, stop, list, fetch, delete and contact-sheet a recording |
| `internal/record/` | The recordings store, one recorder per recording, and the contact sheet's ffmpeg plan |
| `internal/target/crashes.go`, `internal/httpapi/crashes.go` | `/api/crashes`: a simulator's crash reports from the host's DiagnosticReports |
| `internal/target/iosdevicecrashes.go` | `/api/crashes` on a physical iPhone: its crash directory through devicectl, copied into a cache |
| `internal/target/androidcrashes.go` | `/api/crashes` on Android: crashes, native crashes and ANRs from `dumpsys dropbox` |
| `internal/httpapi/media.go`, `internal/target/media.go` | `/api/media`: a photo or video into the device's library, sniffed by its leading bytes |
| `web/drop.js` | Dropping a file on the viewer: photos and videos to `/api/media`, an `.apk` to `/api/install` |
| `internal/mp4/` | Dependency-free MP4 muxer for one H.264 track, and `ReadSamples` to read one back |
| `internal/session/tap.go` | `Tap`: every frame the encoder produced, for a recorder, and the hold that keeps a session open without a viewer |
| `internal/target/lifecycle.go` | `Stopper` and `Deleter`: stopping an emulator or a simulator, and deleting an AVD or a simulator |
| `internal/httpapi/devicelife.go` | `POST /api/devices/shutdown`, `/boot` and `/reset`, `DELETE /api/devices`, the rule for which rows offer them, and the reopen guard's `start=1` |
| `internal/httpapi/avds.go` | `/api/avds`: creating an AVD through the `android` CLI from a fixed profile |
| `internal/httpapi/createslot.go` | The one creation in flight, shared by `/api/avds` and `/api/devices`, and its row in `/api/targets` |
| `internal/httpapi/devices.go` | `/api/devices/options` and `POST /api/devices`: an AVD from `avdmanager` or a simulator from `simctl`, from what is installed |
| `internal/target/avddevices.go`, `sysimages.go` | The SDK's device definitions read out of `sdklib.core.jar`, installed system images, and `config.ini` patching |
| `internal/target/simtypes.go` | Installed iOS runtimes, their device types, and each type's size and group |
| `internal/httpapi/frame.go` | `/api/frame` and its skin art |
| `internal/httpapi/auth.go` | Shared-token middleware |
| `internal/httpapi/manifest.go`, `web/icon*` | Web app manifest and icons for Add to Home Screen |
| `internal/httpapi/quicconn.go` | The QUIC connection behind a request, for `PathRTT` |
| `internal/httpapi/config.go` | `/api/config`, and applying a change to the manager |
| `internal/config/` | The persisted settings file, its defaults and its field table |
| `internal/trace/` | NDJSON trace writer |
| `internal/tspath/` | How Tailscale carries a viewer: direct, peer relay or DERP, for the trace |
| `internal/awake/` | Holds off the Mac's idle sleep while a viewer or agent is active |
| `web/jitter.js` | Jitter buffer and the arrival-jitter estimator |
| `web/jitter.replay.mjs` | Replays `wtprobe -dump` arrivals through a jitter buffer |
| `web/transport.js` | WebTransport and WebSocket behind one interface |
| `web/pane.js` | One target's stream: its connection, decoder, render loop and metrics |
| `web/panes.js`, `web/paneheader.js` | Open panes (up to four, side by side on docked desktop; one at phone width), which is focused, the `?target=` list in the URL; and each pane's header strip |
| `web/layout.js` | Contain fit for the device box, and skin art placement |
| `web/orientation.js` | Whether the fetched bezel still matches the stream's shape |
| `web/pill.js`, `web/health.js` | Target pill, and the server's health verdict read as its dot |
| `web/drawer.js`, `web/targets.js` | Device picker (pinned open on the left when docked), the `/api/targets` round trip, explaining a target that cannot open, and each row's agent line (claimed, driving, hands off) |
| `internal/httpapi/claims.go` | Agents' claims on a target, the person's hands-off set, and who drove a target in the last 10s |
| `web/newdevice.js` | New device sheet: one form (Platform, OS, Device grouped Phone, Foldable and Tablet, Name) over `GET /api/devices/options` and `POST /api/devices`; after an iOS create it watches the options for `job.createdId` and switches to the new `sim:<udid>`. A server with only the old `android` CLI gets the profile list over `/api/avds/profiles` |
| `web/helpsheets.js` | The sheets two states open: How to connect a phone from No devices, and Switch server from Can't reach, which fetches nothing and keeps the last five servers in `localStorage` |
| `web/sheet.js` | Tools sheet: grab handle, half and full detents; pinned open as a right-hand panel when docked |
| `web/corners.js`, `web/handedness.js` | Nav row and overflow corner, and which side they sit on |
| `web/appearance.js` | Light, Dark or System: the stored choice, `data-theme` and the status bar's `theme-color`. `index.html` applies it in an inline script before the module, so a reload does not flash the other theme |
| `web/fold.js` | Fold button at the inner end of the nav row, for a target with a `posture` setting |
| `web/appcard.js` | Foreground app card in the Tools sheet: initials tile, readable name, screen and version, and a Details disclosure for activity, PID and APK |
| `web/devicesettings.js` | `/api/settings` toggles in the Tools sheet |
| `web/streamconfig.js` | The Tools sheet's Stream group: Max size, Bitrate and Frame rate pickers over `/api/config` |
| `web/connstats.js` | The Connection page's rows: Link, Video and Problems, with units and the thresholds that colour a value |
| `web/eventlog.js` | `MsgEvent` decode, `/api/events` catch-up, log rows |
| `web/inspect.js` | Inspect mode: a half sheet with the full `/api/ui` tree, a full-screen 3D tab that stacks the nodes by depth (drag to orbit, pinch or the slider for spacing), and a Tapped tab, where a tap on the stream selects a node instead of reaching the device, and Tap or Copy (the `/api/input` request) acts on it |
| `web/toast.js` | The one shared `#toast` timer |
| `web/states.js` | Connecting, Reconnecting, Can't reach, Can't stream and No devices, and the action sheet that asks before a reclaim |
| `web/immersive.js` | Full screen: the stream alone, left by holding a handle at the top edge |
| `web/doubletap.js` | Double-tap on the bezel or blank stage enters full screen; on the blank stage it leaves |
| `web/keyboard.js` | Phone keyboard: the corner button's open and close, the device's hardware keyboard while it is up, the optional key strip (Esc, Tab, arrows, Paste, Done) above it, and a hidden textarea whose input goes over the session as `MsgText` and keys |
| `web/clipboard.js` | Clipboard: the banner offering a device copy, and the menu's paste and copy |
| `web/record.js` | Record in the More menu, the pill's red dot and elapsed time, and the banner that shares or saves the finished MP4 |
| `web/touches.js` | "Show touches": a dot under each finger, and replays of other viewers' and agents' gestures |
| `web/pip.js` | Picture in Picture behind `?pip=1`: the canvas through `captureStream()` into a hidden `<video>`, floated from the More menu |
| `internal/session/arbiter.go`, `control.go` | Input arbitration: a person's input holds the target against agents' `/api/input` |
| `web/control.js` | The "driving" / "waiting for you" banner, and the viewer's `X-Loupe-Viewer` header |
| `internal/target/clipboard.go`, `simclipboard.go` | Clipboard on Android (scrcpy's control socket) and on a simulator (simcap's CoreDevice pasteboard) |
| `internal/target/adbkeyboard.go` | Android's hardware keyboard: a scrcpy UHID device per session, and typing over the control socket |
| `web/livecheck.js` | Layout self-check run against a live page (see Verification) |
| `web/` | Browser client (vanilla ES modules, no build step) |
| `testdata/client/` | JSON fixtures the `web/*.test.mjs` suites load |
| `action/` | GitHub Action: install a PR's APK through `/api/install` and comment a viewer link on the PR |
| `cmd/loupe/mcp.go`, `internal/mcpserver/` | `loupe mcp`: an MCP server on stdio that drives a running loupe through the Agent API |

### Transports

Both implement `httpapi.Conn`, so nothing above them branches on which is live.

- **WebSocket** is TCP. Reliable and ordered, and therefore unable to abandon a
  frame once written. A frame that has stopped mattering is still delivered,
  ahead of everything behind it.
- **WebTransport** is QUIC, one unidirectional stream per access unit. Frames
  are delivered independently, each is whole or absent, and the sender can
  `RESET_STREAM` a frame that goes stale mid-flight.

- **Raw QUIC**, ALPN `loupe/1` on the same UDP port, is for a native client:
  Network.framework has QUIC and no WebTransport. ALPN picks between it and
  HTTP/3 during the handshake (`cmd/loupe/quiclisten.go`), so the listener has
  to switch on datagrams and partial-delivery resets itself, as
  `webtransport.Server` would have, or `/wt` breaks silently
  (`TestSharedQUICListenerKeepsWebTransportWorking`). Every stream is
  client-opened, its first byte saying what it is: `0x18` the control stream,
  whose first message is `QUICOpen` (target, token, start) in place of `/wt`'s
  query, and `0x19` a slot the server writes one frame on, with `/wt`'s 13-byte
  header, then closes or resets. Server-opened streams per frame do not work
  here: Network.framework never granted more stream credit than its initial
  limit, 100 of 100 uni and 100 of 100 bidi, while quic-go grants credit for
  streams the client opens (prototypes in `tools/nwquic/`). `wtprobe -quic`
  probes it. Through a spare synthetic server on the Tailscale address it ran
  61.0fps from 1s to 7s, the same as `/wt` beside it, with the first keyframe
  600-900ms after the config on both.

**Safari's QUIC connection carries 16 MiB and stops** (trap 52), so a
WebTransport session moves to a fresh connection before then: the server
sends `MsgRotate` (0x09) at 12 MiB or 6000 streams, or after 1s with no
stream credit, and `web/rotate.js` opens a second `/wt` and promotes it on
its first keyframe. A connection that freezes anyway is closed with code 2
(`internal/httpapi/peerwindow.go`), and the page reconnects at once.

A session that ends on purpose says why. `MsgEnded` (0x0A, JSON
`{"reason":"shutDown"|"deleted"}`) is sent on the video connection, so it is
the same over WebSocket and WebTransport, and a viewer that got it does not
reconnect. A WebTransport session also closes with code 3 and the reason as the
message, for a client that only sees the close; code 1 stays an open failure
(`wtAcquireFailed`).

Client picks WebTransport and falls back to WebSocket. Force the old path with
`?transport=ws` on the same link, which is how the two get compared.

Frame headers differ: WebSocket carries `session.FrameHeaderLen` = 18 bytes
inside the envelope, WebTransport carries 13 (`frameID` u32, `ptsMicros` u64,
`flags` u8) because the stream boundary already delimits the frame.

## Running it

The server runs under launchd as `dev.bmcreations.loupe`. It starts at login
and comes back after a crash.

```sh
make agent-install   # write ~/Library/LaunchAgents/... and bootstrap it
make build           # this IS the deploy: the supervisor restarts onto it
make agent-status    # state, pid, last exit
make agent-log       # tail loupe-run.log
make agent-uninstall # bootout and remove the plist
```

Then `https://my-mac.tailnet-0000.ts.net:18456/` on the phone.

That address is this deployment's, not a requirement. The server serves TLS
on whatever it binds and needs no particular network; only
`loupe-supervisor.sh` assumes Tailscale, because it issues the cert with
`tailscale cert` and so covers only the `ts.net` name. A host reachable some
other way needs its own cert and a path for TCP and UDP on the `-addr` port. See
`docs/cloud-emulators.md`.

`loupe-supervisor.sh` runs a **copy** of `./loupe` from `.run/`, never
`./loupe` itself. macOS returns ETXTBSY on a write to a running executable, so
running the original would make `make build` fail for as long as the server was
up. The supervisor polls `./loupe`'s sha256 every 2s and restarts onto a new
build, which is why building is the whole deploy step.

TLS is served directly rather than through `tailscale serve`, because that only
proxies TCP and so cannot carry HTTP/3. It also hid backpressure (see trap 4).

Tests: `make test`, which is `go test ./...` and `node web/run-tests.mjs`.

### Sleep

**A sleeping Mac pauses everything**: the server, every emulator and every
simulator, and the phone retries against nothing until it wakes. Waking a Mac
remotely is not dependable -- Wake-on-LAN is poor over Wi-Fi and a Bonjour
Sleep Proxy wakes on TCP, not on the QUIC the video uses -- so loupe keeps the
Mac from falling asleep instead of trying to wake it.

`internal/awake` holds a `PreventUserIdleSystemSleep` assertion while any
viewer is attached, and for two minutes after each `/api/input`, because an
agent's requests come seconds apart with no connection between them. When
nothing wants it, it is released and the Mac sleeps on its own schedule. It
is `caffeinate -i -w <loupe pid>`, not IOKit through cgo, so the build stays
cgo-free, and `-w` ends the assertion with the server, so a crash or a
supervisor restart cannot leave the Mac held awake. `pmset -g assertions`
lists it as caffeinate asserting on loupe's behalf. Each change is traced as
`kind:"awake"`.

It prevents idle sleep only. Closing the lid still sleeps a MacBook, and the
display may turn off, which costs the stream nothing.

### Auth

Every request needs the shared token, because `/api/input` means reaching the
port is now enough to drive a device rather than only to watch one. `-no-auth`
turns it off and says so at startup.

The token is read from `-token`, then `LOUPE_TOKEN`, then
`~/Library/Caches/loupe/token`, and generated into that file if absent.
Persisting it is not incidental: the supervisor restarts on every build, and a
per-start token would invalidate the phone's bookmark each time, which ends
with the token being switched off. The startup banner prints the full link, and a QR code of
the one a phone can open: its host is the TLS certificate's first DNS name,
since the other links are localhost and interface IPs, which fail the
certificate check on a phone (`cmd/loupe/qr.go`). It is about 25 lines of
half blocks, so it fits `make agent-log`'s tail.

Three ways to present it, because three callers cannot share one. An agent
sends `Authorization: Bearer`. A browser that has been here sends the cookie.
A freshly opened link carries `?token=`, and a page load is then redirected to
strip it, so it does not sit in history or leak through `Referer`.

`/ws` and `/wt` are exempt from that redirect and must stay exempt.
WebTransport sends no cookies, so `?token=` is the only credential `/wt` has;
redirecting it would strip that and produce trap 11's symptom -- a transport
that fails with nothing in the log. `TestAuthDoesNotRedirectTheTransportEndpoints`
covers it. `/healthz` is open so the supervisor can poll it.

`/ws` and `/wt` also take `start=1`, which says the open is an explicit Start
and not a reconnect. `explicitStart` lifts the target's shut-down reopen guard
before `Acquire`, unless a shutdown or delete of that target is still running,
and never a `deleted` guard. The client sends it once, from the shut-down
screen's Start it, and never on a reconnect, so a stale tab cannot boot a
device someone shut down.

**The Home Screen app keeps its own cookies**, apart from Safari's, so its
first launch would have no credential and answer 401. The manifest's
`start_url` carries `?token=` when the request for it held the token, and
the usual redirect then sets the cookie and strips it. The manifest and the
icons are open, because iOS fetches them while adding to the Home Screen and
nothing says it sends the cookie; a request without the token gets a plain
`/`. Checked on `loupe-probe` through a spare server: the add sheet showed
the icon and the token start URL, and the launched app loaded standalone
and connected.

The cookie is `SameSite=Strict`, since `/api/input` drives a real device and a
cross-site POST would otherwise carry it. It is `Secure` only over TLS: setting
that unconditionally locks a plain-http run out, because the redirect has
already removed the token from the URL. It is deliberately not `HttpOnly` --
the client has to read it to build the `/wt` URL, and it came from the address
bar regardless.


### Configuration

Settings are settled once at startup, in this order: **an explicit flag beats
the file, and the file beats the default.** The file is
`~/Library/Application Support/loupe/config.json`, or wherever `-config`
points, and it is written with the defaults on first run so there is something
to edit. Flag defaults come from `config.Default()` rather than being spelled
out a second time in `main.go`, so the two cannot drift apart.

`GET /api/config` reports each setting four ways, and the last two exist
because a settings UI that cannot see them will claim success for a write that
did nothing:

| Field | Meaning |
| --- | --- |
| `value` | what the server is actually using |
| `fileValue` | what is on disk |
| `source` | `default`, `file` or `flag` |
| `pinned` | a flag is overriding the file, so a write will not take effect |

`pinned` is what a settings UI needs to not lie. A flag that overrides the
file makes a `PUT` persist to disk and change nothing until the flag goes away,
so the `PUT` response names the pinned settings rather than returning a bare
200.

The installed plist pins nothing today. Its argv is `-addr`, `-synthetic`,
`-tls-cert`, `-tls-key` and `-trace`, so `maxSize` and `bitRate` both read
`source: default, pinned: false` on a live `GET`.

Reporting only the file value is the same bug one layer down: with 720 on disk
and `-max-size 1080` on the command line, a UI would display 720 while the
device streamed at 1080.

A `PUT` is all-or-nothing. Applying three of four settings and then failing
leaves the caller unable to say what took effect, and the file is the only
record of it.

**`applies: "nextSession"` does not mean immediately.** `SetOptions` replaces
what *future* sessions launch with; an open session keeps the options it
started with, and `idleGrace` is 15s, so a viewer already watching sees nothing
change until their session ends. Measured on an Acme One: a `PUT` of `maxFps` 30
gave pts p50 33.4ms on a fresh session, resetting to 60 still read 33.5ms
immediately afterwards, and 16.7ms once the grace had elapsed. The wire values
are pinned by `TestApplyValuesAreStable`, because a client hardcodes them.

Auth deliberately stays out of the config file. The token lives in its own
file with its own permissions, and a setting that can switch off
authentication does not belong in the same document as the frame rate.

### Adaptive bitrate

`bitRate` is a ceiling, not a fixed rate. When the link cannot carry what the
encoder produces, the session relaunches the encoder lower and probes back up.
An encoder that can change rate while it runs, a simulator's, is changed in
place instead (see iOS simulator targets).
`adaptiveBitRate` turns it off; it defaults on.

**A simulator also steps down in size as the rate falls.** At 1.6 Mbps a
497x1080 picture at 60fps gets 0.05 bits a pixel, and a fast scroll comes out
as blocks (the trace of 2026-10-26, sim:77763E2D on the internet path). After
each live rate change `maxsize.go` picks the longest edge from a ladder (the
native size, then 1080, 900, 720, 540) that gives each pixel at least 0.07
bits; 540 is the floor, because below it text stops being legible, and sends it to simcap on stdin (op `0x81`, a u16; 0 restores the
configured size). simcap rebuilds its encoder only when the size changes, the
same path a rotation takes, so viewers get a new SPS and a keyframe. A step
up needs 1.5 times the target, because the larger keyframe lands just as the
link has been judged clean. The native size returns only at the rate the
session opened with. A recording holds the size, since the mp4 writer ends on
a size change. Behind `linkshaper` at 1.2 Mbps with a scrolling simulator, the
stream went from 2622 to 1080 to 540 and back to 720 as the rate recovered;
`wtprobe`'s p90 frame gap was 22ms against 66ms without the ladder. Only
simcap advertises `resize` in `-caps`, so Android and synthetic are unchanged.

**Synthetic adapts too, by relaunching ffmpeg.** It was skipped on the claim
that it ignores `Options.BitRate`; it passes it to ffmpeg as `-b:v`,
`-maxrate` and `-bufsize`. Measured with `wtprobe` on a spare port, adaptive
off: 4 Mbps delivered 3.34 Mbps and 500 kbps delivered 0.40 Mbps, both at
61fps. A relaunch starts a second ffmpeg and swaps it in; its PTS restarts at
zero, and `pump`'s wall-clock offset carries the timeline across. What
synthetic does ignore is `MaxSize`.

That makes synthetic the way to drive the controller without hardware:

```sh
loupe -addr :18470 -synthetic -no-devices -no-simulators -no-auth \
    -bitrate 4000000 -trace trace.jsonl -tls-cert ... -tls-key ...
go run ./cmd/linkshaper -listen 100.64.0.10:18471 -to 100.64.0.10:18470 \
    -plan "0s rate=0; 10s rate=400k delay=60ms queue=300ms"
go run ./cmd/wtprobe -for 75s "https://100.64.0.10:18471/wt?target=synthetic"
```

In that run the controller went 4 Mbps to 1.53 Mbps (`starved`, 7s after the
link was shaped, with `awaitFrac` 0.52), then to the 250 kbps floor 22s later.
At the floor `awaitFrac` read 0 and goodput held at about 234 kbps, 61 frames
a second. No ffmpeg outlived the server. Recovery was not in that run.

The policy is in `bitrate.go` and the mechanism in `relaunch.go`, deliberately
apart: one decides, the other acts, and only the first is cheap to test.

**Why a relaunch and not a control message.** scrcpy's opcodes run 0 to 17 and
none of them set the bitrate -- it is fixed when the encoder is configured. So
adapting means opening a second scrcpy server and swapping it in. That happens
inside the session rather than by ending it and letting the client reconnect,
because trap 21 established that iOS reconnect is the fragile path.

**The new encoder opens before the old one closes**, so a relaunch that fails
leaves the session exactly as it was and a transient adb failure is not fatal to
a session that was working. The two overlap for the length of the launch.
Measured on an Acme One with `cmd/overlaptest`: the second server came up in 488ms
beside the live first, both streamed during the overlap, and the second kept
producing after the first shut down. That is worth having checked rather than
assumed -- scrcpy-server deletes its own jar on exit (trap 17), so the first's
cleanup runs while the second is already using it.

**The decision is goodput, not a guess.** `NoteSent` counts payload bytes that
actually reached a viewer, which is the only measure of what the link will
carry; every other counter says what was offered to it. On sustained shedding
the new bitrate is the measured goodput, floored at 250 kbps; on a starved link
it is the goodput again, with a halving as the ceiling on one step. A link carrying
under half the current rate while the viewer waits on keyframes for a fifth
of the window steps down to goodput too, which is the gap between those two
that trap 51 fell into. Recovery
steps up by half after 60s with almost no shedding and almost no keyframe
waiting, capped at six changes per
session so a marginal link cannot oscillate, with a 12s settle after each
change. Those three are sized by what a relaunch costs, so an encoder that
changes live (a simulator's) gets its own: a 2s settle, 15s clean before a
step up, and 24 changes in any 30 minutes rather than six for the session,
because a simulator session can run all afternoon and meet several episodes
(`NewLiveBitrateAdapter`, with the reasons beside the constants). The 10s down window is shared, because telling the link from a
burst takes as long whichever way the change is applied.

That only works because a frame the transport reset mid-flight is no longer
counted as one that reached a viewer. It was, and goodput read 1.56 Mbps on a
400 kbps link that was delivering nothing at all -- trap 35.

Three things the controller deliberately does not count:

- **Keyframe waits, in the shed fraction.** They are the consequence of a shed,
  not independent evidence of one. An abandoned frame is not one of these: it
  reached the link and arrived nowhere, so it counts as shedding (trap 35). Counting them there lets a single dropped
  frame argue for a change sixteen times over, which is what trap 27 split the
  counters to make visible. They are read separately by the starvation gate,
  which needs them sustained across a whole window rather than counted frame by
  frame -- see trap 34.
- **An interval with no viewer attached throughout.** There is nothing measuring
  the link, and a zero reads identically to a link that carried nothing, so the
  window restarts instead.
- **Anything but the slowest viewer.** The encoder is shared, so what it must
  fit inside is the slower link; summing would read two half-speed viewers as
  one fast one.

**Presentation timestamps are offset across the swap, and on one device the
offset is approximately zero.** scrcpy's PTS is absolute device uptime, measured back to back against
`/proc/uptime` and agreeing to 50ms (trap 31). So PTS continues across a swap
on the same device, `pump`'s offset computes to approximately zero, and the
arithmetic is harmless rather than load-bearing. It has not been measured
across a live relaunch, so it stays until it is.

What the offset was protecting is real. `web/jitter.js` anchors its clock on
the first PTS it sees and does not re-anchor on its own, so a jump backwards
leaves every later frame reading as already due -- presentation silently
degrades to draw-on-arrival for the rest of the session, with nothing in the
trace to say so. A backwards jump does happen: switching targets moves PTS to
a different device's uptime, in either direction.

**A stream ending is ambiguous now**, and `nextStream` resolves it under the
same lock the swap commits with: if the session's stream is still the one that
ended, nobody replaced it and the session ends. Making that one decision is what
stops a relaunch installing an encoder nothing will read -- which on a device
means a live scrcpy server holding a display for the life of the process.
`TestRelaunchDoesNotInstallAStreamNobodyWillRead` drives that race directly.


## Client telemetry

The client reports what only it can know, over `/api/log`, recorded as
`kind:"client"`. This is what separated "the client never ran" from "the client
ran and the transport failed" in trap 11, so keep it working:

- An inline classic script in `index.html`, before the module, reports `boot`
  with `isSecureContext`, `typeof VideoDecoder` and `typeof WebTransport`, and
  installs `error` / `unhandledrejection` handlers. Classic and inline so it
  survives a module that fails to parse.
- `app.js` reports `module` on entry and `module-end` at the bottom. `boot` and
  `load` with no `module-end` means evaluation aborted, and the stack will be in
  the trace.
- A record that comes from a pane (`web/pane.js`'s `report`, which
  everything the pane mounts reports through) carries `pane` (`p1`) and
  `target`, each unless the record set its own.
- `pane-open`, `pane-close` (each with `count`, the panes open after it) and
  `pane-focus` follow the panes on docked desktop; `layout` records the Tools
  sheet's Layout row set to Single.
- The overlay (`web/states.js`) reports `overlay` with `title`, the string
  the old `showOverlay` sent, kept word for word so older records still
  group, and `state`, its key. Every dead end funnels through it, and so
  does Connecting, on each page load and target switch. A tap on
  one of its buttons is `overlay-action` with `state` and `action`. The shut-down screen's state key is `shutDown` ("<label> is shut down" with Start it and Pick another device, or "<label> was deleted" with Pick another device only), and the `ended` event records a target the server ended, with `target`, `reason` (`shutDown` or `deleted`) and `via` (`msg` for `MsgEnded`, `close` for a WebTransport close with code 3). `picker-shutdown` and `picker-delete` report a picker action with `kind`, `target` and `outcome` (`ok`, the server's error code, the status, `network`, or `cancelled` for a Delete sheet that was dismissed).
  Switch server going somewhere is `switch-server` with `via` (`typed`
  or `recent`); it carries no host.
- `handleTargets` traces the IDs it returned, so an empty list is visible as an
  empty list rather than as an absent record.
- The redesigned UI reports its own state changes: `sheet-open` and
  `sheet-close` with the detent, `sheet-page` with the page and `push` or
  `pop`, `config-put` with the setting, value and one of `ok`, `pinned` or
  `error`, `drawer-pick` with the from and to target
  ids, `dot-open` the pill's dot opening the Connection page, `hand`, `appearance` with the choice, and `settings-put` with the setting ids and one of `ok`,
  `error`, `unavailable` or `superseded`. `inspect.js` reports inspect mode
  turning on and off, `inspect-tab` the sheet's tab, `inspect-select` a node
  chosen with `via` (`stream`, `row` or `3d`), `inspect-tap` and `inspect-copy` the
  detail bar's two buttons, and `inspect-error` a failed `/api/ui`, and `immersive` reports full screen turning on or off
  with `via` (`menu` or `hold`). `keyboard-open` records the phone's keyboard being raised (with
  whether the field took focus), `keyboard-shown` the visual viewport 800ms
  later, which shrinks only if a keyboard came up, `keyboard-input` the type
  of the first three input events after each open, `keyboard-blur` the field
  losing focus, and `keyboard-error` a switch or a typed message failing, and `keystrip-pref` the Key strip setting. `newdevice-open` records the New device sheet opening, with `platforms` (the ids the server offers, comma separated) and, when the options request failed, `error` (the status or `network`); `newdevice-create` a Create press, with `platform`, `os`, `device`, `nameLength` and `outcome` (`ok`, `exists`, `creating` or `error`); `avd-create` a tap on the old profile list, with `profile` and `outcome` (`accepted`, the server's error code, or the status); `newdevice-error` a `nameRule` the browser cannot compile (`reason`, `platforms`), so validation is skipped; and `newdevice-resolved` the switch to a created simulator, with `id` and `result` (`listed`, `unlisted` or `moved`). `clipboard-offer`, `clipboard-copy`
  (`via` offer or menu), `clipboard-paste` and `clipboard-error` carry only
  the text's length, `touches-pref` the "Show touches" switch, and
  `control-banner` each banner state shown, with `agent`, and
  `control-takeover` the banner's Take over button, with `agent`. `pip-offered` records `?pip=1` with `supported`; `pip-request` the menu tap, with `api` (`standard` or `webkit`), `readyState` and the video's `w` and `h`; `pip-start` and `pip-stop` with `api`, `hidden` and, on a stop, `forMs`; and `pip-error` with `stage` (`play` or `request`) and `message`. On iOS 27.2 `hidden` read false for the whole of a 23s PiP, so the hidden-page paths in `web/pane.js` have not run on a phone. `record-start` records a
  recording this page started, with `target`; `record-adopt` one it took up
  after a reload, a switch or a 409, with `target` and `agoMs`;
  `record-stop` the end, with `reason`, `frames` and `via` (`menu`, `pill`,
  `switch` or `ended`); `record-share` with `outcome` (`shared` with `bytes`,
  `cancelled`, or `error` with `err`); `record-save` with `bytes`; and
  `record-error` with `action` and `err`, or `code: "recording"` for a
  start the server refused. These put a tap on the same
  timeline as the frames.
- `decode-error` carries the decoder's message and whether a config was
  held, at most once a second. Counting errors without their text is what
  left the cause of trap 48 unknown. iOS fails the decoder just before it
  hides the page, so the report waits up to a second: one followed by a
  suspend carries `beforeSuspendMs` and is the page leaving, not the
  stream. One without it is a stream fault. `visibility` is the page's
  state at the error, which still reads `visible` in that case.

Server-side, `wt-attempt` fires on entry to the `/wt` handler, before `Acquire`
and `Upgrade`. Its absence means the request never reached HTTP, which points
below QUIC rather than anywhere in the application.

Each viewer that arrives over Tailscale gets a `kind:"path"` record: `path`
is `direct`, `peerRelay` or `derp`, a direct path carries `pathAddr` and
`pathScope` (`lan` or `internet`), and a relayed one names its `derp`
region. It is read from `tailscale status --json` at open and again 10s
later, recorded a second time only if it changed, since a connection starts
on DERP and moves to direct once hole punching works. Before this the trace
could not tell a cellular session from a Wi-Fi one: `quic.remote` is always
the phone's Tailscale address. `pathScope: internet` or `derp` is the phone
away from home. A viewer not on Tailscale records nothing.


## Known gaps

- **Auth is one shared token with no scoping.** It is enough to stop the port
  being open, which is what `/api/input` made urgent, but everyone who has it
  can drive every target and the trace cannot tell two holders apart.
- **The transport clumps frames in flight, but it costs almost nothing.**
  Measured with `cmd/wtprobe`, a device under sustained motion captures at p50
  16.6ms, p90 17.9, max 27.0, with 1% of frames falling within half a 60Hz tick
  of the one before. By arrival the mean is unchanged at 16.6ms, so nothing is
  lost or delayed on average, but p90 is 24.7, p99 is 33.0 and 11% now fall
  inside that half-tick. `present()` paints at most one frame per
  `requestAnimationFrame`, so a clumped pair can only show its newer half.

  Do not read that 11% as 11% of frames lost. On a healthy run the client's
  render deficit is p50 **2.0%** (rendered p50 50, decoded 49, `skipped` 38 in
  56s) -- the jitter buffer absorbs nearly all of it. The 16.7% deficit seen
  before the trap 19 fix was measured at the same rtt p50 (10.9 vs 11.5) and
  the same jitter p50 (12.0 vs 12.3), differing mainly in `awaitKey`: 253 and
  270 against 0. Suppressed frames, not clumping, were the bulk of that gap.
  Where the clumping itself is introduced is still not established.
- **A derived frame is still hidden in landscape.** Only the skin route turns
  (see Device frames). Turning a derived frame means remapping its cutout
  anchor, and the one device that produces one here must not be rotated for
  testing.

- **iOS simulators have no rotation.**
  Touch, pinch, buttons, keys, text, screenshots, `/api/ui`, the bezel and
  adaptive bitrate work (see iOS simulator targets and Simulator frames),
  though `occluded` misses nodes covered by blank space. The HID path is
  private and is the part most likely to break with an Xcode update.
- **Arbitration is human against agent, and claims are agent against
  agent.** Two viewers still both go through, and the last to touch holds the
  target. An agent that sends no name cannot claim, and one that sends
  another's name is that one, so a claim is coordination between agents that
  name themselves honestly. Nothing stops an agent
  that sends a live viewer's id in `X-Loupe-Viewer`; with one shared token
  this is coordination, not enforcement. The banner and the 409 are
  checked by unit and handler tests, not yet with a person at a phone and
  an agent on the same target.
- No audio. `launch.go` passes `audio=false`.
- `Synthetic` ignores `MaxSize`: it is fixed at 720x1560 unless the target is
  constructed with a size. It does honour `MaxFPS` and `BitRate`, and adapts.

## Environment notes

- `webtransport-go` is v0.13.0. v0.9.0 hard-required a
  `Sec-Webtransport-Http3-Draft02` header that no current browser sends, so
  every Safari CONNECT was rejected. v0.10.0 dropped that check, and the pin
  stayed there to protect the draft-07 setting -- which is what kept Safari's
  `createBidirectionalStream()` hanging forever, because v0.10.0 implements
  only `CLOSE_SESSION` (0x2843) and never sends a `WT_MAX_STREAMS` capsule, so
  the client's stream allowance stays 0. v0.13.0 sends it, and `client-uni`
  went from `timeout 4000ms` to `ok` in 5-16ms.

  Two things do not come for free on v0.13.0. The flow-control limits are
  opt-in: a nil `webtransport.Server.Config` reproduces v0.10.0's behaviour
  exactly, so `sessionFlowControl()` sets them explicitly. And v0.13.0 speaks
  draft-15, advertising `0x14e9cd29` in place of the `0xc671706a` Safari 27
  responds to, so `webTransportSettings()` keeps sending the older key by hand.
  Both are asserted in `TestWebTransportServerIsFullyConfigured`.
- That upgrade pulled `quic-go` v0.53.0 -> v0.59.0, which **removed the
  `logging` package**. Connection tracing is now a qlog sink: see
  `cmd/loupe/quictrace.go`, which implements `qlogwriter.Trace` and forwards
  four low-frequency events. The per-packet qlog events would bury the trace.
- **scrcpy is Apache 2.0, and loupe now redistributes the jar.**
  `third_party/scrcpy/LICENSE` is byte-identical to upstream's, whose appendix
  names `Copyright (C) 2018 Genymobile` and `Copyright (C) 2018-2026 Romain
  Vimont`; the upstream README repeats both, and Homebrew's formula metadata
  agrees. There is no NOTICE file and no trademark or commercial clause, so
  section 4 reduces to shipping that text and keeping the attribution, which
  `THIRD_PARTY_NOTICES.md` does. Section 6 grants no trademark rights: nothing
  here may imply this is scrcpy or that Genymobile endorses it.
- **Do not move to scrcpy 4.x without changing the parser.** The jar is
  embedded and pinned now, so nothing moves underneath a run, but the reason to
  stay put is specific. 4.1 adds `PACKET_FLAG_SESSION` at bit 63 and shifts
  every existing video packet flag down one:

  | Flag | 3.3.4 | 4.1 |
  | --- | --- | --- |
  | `PACKET_FLAG_SESSION` | -- | bit 63 |
  | `PACKET_FLAG_CONFIG` | bit 63 | bit 62 |
  | `PACKET_FLAG_KEY_FRAME` | bit 62 | bit 61 |

  Against 4.1, `protocol.go` reads a config packet as a keyframe, a keyframe as
  neither, and adds 2^61 microseconds to every keyframe PTS, because bit 61
  falls inside `ptsMask`. `send_codec_meta` is renamed `send_stream_meta`, and
  an unknown option only warns (`Ln.w`) instead of failing, so it would launch
  cleanly and misparse in silence. Control opcodes 0-17 are unchanged,
  `TYPE_RESET_VIDEO` among them; 18-22 are appended for camera, torch and
  resize. Migrating is three constants, the option rename, and a device run.
- `web/` has no build step. Plain ES modules, served from `embed.FS`.
