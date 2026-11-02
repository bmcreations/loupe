# Architecture

How the pieces fit: package layout, transports, deployment, sleep, auth,
configuration, adaptive bitrate, client telemetry and known gaps. Flags and
the stats overlay are in [running.md](running.md); a first run is in
[getting-started.md](getting-started.md).

## Shape

```
Android device ──scrcpy──> Go server ──WebSocket or WebTransport──> Safari
                              │                                       │
                         H.264 Annex B                          WebCodecs
                         access units                        VideoDecoder → canvas
```

The server speaks the scrcpy wire protocol (v2.x/3.x) to a device over adb, or
generates a synthetic 60fps test pattern. iOS simulators and iPhones come
through the simcap helper in the same wire format. The server reframes the
stream into access units and ships them to the browser, which decodes with
WebCodecs and paints to a canvas. Pointer and key events go back over the same
connection.

### Layout

| Path | What it is |
| --- | --- |
| `cmd/loupe/` | Flags, target registration, HTTP/3 + TLS startup; `mcp.go` is `loupe mcp` |
| `cmd/` (others) | Probes (`wtprobe`, `simprobe`, `overlaptest`, ...) and `linkshaper`; see the `loupe-probes` skill |
| `internal/scrcpy/` | scrcpy wire protocol: control messages, stream header, framing. `server/` embeds the pinned scrcpy-server jar |
| `internal/h264/` | Annex B parsing, SPS/PPS, access unit boundaries |
| `internal/session/` | Session lifecycle, protocol constants, the pacer (congestion control), adaptive bitrate policy (`bitrate.go`) and encoder swap (`relaunch.go`), input arbitration (`arbiter.go`), `Tap` for recorders |
| `internal/httpapi/` | Routes, auth, the transport-agnostic `Conn`, the shared viewer loop, WebTransport; one file per `/api/*` area |
| `internal/wsx/` | Dependency-free RFC 6455 WebSocket |
| `internal/target/` | Targets and what you do to them: ADB device, AVD and emulator lifecycle, iOS simulator, physical iPhone, `Synthetic`; plus input, app launch, UI tree, device frames, clipboard, crashes, media |
| `internal/composetree/`, `tools/composeagent/` | Compose inspector agent (embedded `.so` per ABI and dex) and its decoder; `make composeagent` rebuilds it |
| `tools/xcrunner/`, `internal/usbmux/` | XCUITest runner for iPhones below iOS 27, reached over usbmuxd |
| simcap | Swift helper, cloned and built by `make simcap`: simulator and iPhone capture to H.264, HID input, accessibility tree |
| `internal/record/`, `internal/mp4/` | Recordings store and a dependency-free MP4 muxer |
| `internal/config/` | The persisted settings file, its defaults and field table |
| `internal/pairing/` | Paired sessions (see [security.md](security.md)) |
| `internal/clipshare/` | The shared clipboard |
| `internal/peer/`, `internal/dest/`, `internal/t3/` | Other servers this one knows about, flag destinations, T3 Code's CLI |
| `internal/mcpserver/` | MCP server on stdio over the Agent API |
| `internal/trace/`, `internal/tspath/` | NDJSON trace writer; how Tailscale carries a viewer (direct, peer relay, DERP) |
| `internal/awake/` | Holds off the Mac's idle sleep while a viewer or agent is active |
| `web/` | Browser client: vanilla ES modules, no build step. `app.js` is the only importer of client modules |
| `testdata/client/` | JSON fixtures the `web/*.test.mjs` suites load |
| `action/` | GitHub Action: install a PR's APK through `/api/install` and comment a viewer link |

Client modules by job: `transport.js` (WebTransport and WebSocket behind one
interface), `jitter.js`, `pane.js` and `panes.js` (one target's stream; up to
four side by side on docked desktop), `drawer.js` and `targets.js` (device
picker), `sheet.js` (Tools sheet), `states.js` (Connecting, Reconnecting,
Can't reach, Can't stream, No devices), `keyboard.js`, `clipboard.js`,
`record.js`, `inspect.js`, `drop.js` (drop a photo, video or `.apk` on the
viewer), `control.js` (the "driving" banner). Every new module must be listed
in `clientModules` (trap 43).

### Transports

Both stream transports implement `httpapi.Conn`, so nothing above them
branches on which is live. The client picks WebTransport and falls back to
WebSocket; `?transport=ws` forces WebSocket on the same link.

| Transport | Carries | Behavior |
| --- | --- | --- |
| WebSocket (`/ws`) | TCP | Reliable and ordered. A frame that has stopped mattering is still delivered, ahead of everything behind it |
| WebTransport (`/wt`) | QUIC, one unidirectional stream per access unit | Frames are delivered independently, each whole or absent. The sender resets a frame that goes stale mid-flight |
| Raw QUIC, ALPN `loupe/1` | Same UDP port | For a native client; Network.framework has QUIC but no WebTransport |

Raw QUIC streams are client-opened, and the first byte says what the stream is:
`0x18` is the control stream, whose first message (`QUICOpen`: target, token,
start) replaces `/wt`'s query string; `0x19` is a slot the server writes one
frame on, then closes or resets. `wtprobe -quic` probes it.

Frame headers differ. WebSocket carries 18 bytes inside the envelope;
WebTransport carries 13 (`frameID` u32, `ptsMicros` u64, `flags` u8) because
the stream boundary already delimits the frame.

**Connection rotation.** Safari's QUIC connection carries 16 MiB and stops
(trap 52), so a WebTransport session moves to a fresh connection first. The
server sends `MsgRotate` (0x09) at 12 MiB or 6000 streams, or after 1s with no
stream credit. `web/rotate.js` opens a second `/wt` and promotes it on its
first keyframe. A connection that freezes anyway is closed with code 2, and
the page reconnects at once.

**Ending on purpose.** `MsgEnded` (0x0A, JSON `{"reason":"shutDown"|"deleted"}`)
goes out on the video connection, the same over WebSocket and WebTransport. A
viewer that got it does not reconnect. A WebTransport session also closes with
code 3 and the reason as the message, for a client that only sees the close.

**Start versus reconnect.** `/ws` and `/wt` take `start=1`, meaning the open
is an explicit Start. It lifts the shut-down reopen guard (never a `deleted`
one, and not while the shutdown is still running). The client sends it only
from the shut-down screen's Start it, so a stale tab cannot boot a device
someone shut down.

## Running it

The server runs under launchd as `dev.bmcreations.loupe` through
`loupe-supervisor.sh`. It starts at login and comes back after a crash. The
commands (`make agent-install`, `agent-status`, `agent-log`, `agent-uninstall`)
are in [getting-started.md](getting-started.md#4-run-it-as-a-service).

- **`make build` is the deploy.** The supervisor runs a copy of `./loupe` from
  `.run/`, because macOS returns ETXTBSY on a write to a running executable. It
  polls `./loupe`'s sha256 every 2s and restarts onto a new build.
- **TLS is served directly**, not through `tailscale serve`, because that
  proxies TCP only and cannot carry HTTP/3 (trap 4).
- **Tailscale is an assumption of the supervisor, not the server.** The
  supervisor issues its certificate with `tailscale cert`, so it covers only the
  `ts.net` name. A host reachable another way needs its own certificate and a
  path for TCP and UDP on the `-addr` port.
- **Plain HTTP is loopback only.** See [running.md](running.md#plain-http).

### Sleep

A sleeping Mac pauses the server, every emulator and every simulator, and the
phone retries against nothing until it wakes. Waking a Mac remotely is not
dependable, so loupe keeps it from falling asleep instead.

`internal/awake` holds an idle-sleep assertion while any viewer is attached,
and for two minutes after each `/api/input`. When nothing wants it, the
assertion is released and the Mac sleeps on its own schedule. `pmset -g
assertions` lists it as `caffeinate` asserting on loupe's behalf, and it ends
with the server. Each change is traced as `kind:"awake"`.

It prevents idle sleep only. Closing a MacBook's lid still sleeps it, and the
display may turn off, which costs the stream nothing.

### Auth

Every request needs a credential, because reaching the port is enough to drive
a device. `-no-auth` turns it off and says so at startup. What this does and
does not guarantee is in [security.md](security.md).

**The token** is read from `-token`, then `LOUPE_TOKEN`, then
`~/Library/Caches/loupe/token`, and generated into that file if absent. It
persists because the supervisor restarts on every build, and a per-start token
would invalidate the phone's bookmark each time.

**The startup banner** differs by where stdout goes:

| stdout | Banner shows |
| --- | --- |
| A terminal | Full links with `?token=`, and a QR code of the link a phone can open (its host is the certificate's first DNS name) |
| Not a terminal (launchd's `loupe-run.log`) | Links without `?token=`, no phone link, QR code or `Bearer` line, and where the token is stored instead |

An authed `GET /api/handoff` returns the phone link. The run log and the trace
are owner-only, and the trace records request queries with the token replaced
by `REDACTED`.

**Presenting it.** An agent sends `Authorization: Bearer`. A browser that has
been here sends the cookie. A freshly opened link carries `?token=`, and a page
load is then redirected to strip it, so it stays out of history and
`Referer`. `/ws` and `/wt` are exempt from that redirect and must stay exempt:
WebTransport sends no cookies, so `?token=` is its only credential, and
stripping it fails the transport with nothing in the log (trap 11). `/healthz`
is open so the supervisor can poll it.

**The cookie** is `SameSite=Strict` and `Secure` only over TLS. It is not
`HttpOnly`, because the client reads it to build the `/wt` URL. A request that
authenticates by cookie alone must send no `Origin`, or one matching this
server's scheme, host and port; anything else gets a 403 with a JSON error.
That blocks a page on another port of the same host from riding the cookie.
Bearer and `?token=` requests skip the check. Every response carries
`Referrer-Policy: no-referrer`.

**The Home Screen app** keeps its own cookies, apart from Safari's. The web app
manifest's `start_url` carries `?token=` when the request for it held the token,
and the usual redirect then sets the cookie. The manifest and icons are open,
because iOS fetches them while adding to the Home Screen; a request without the
token gets a plain `/`.

**Paired sessions.** A paired device presents an `lsn_` session as `Authorization:
Bearer` or the `loupe_session` cookie (`HttpOnly`, `SameSite=Strict`, `Secure`
over TLS, 30-day `Max-Age`). The same Origin check applies. `GET /api/auth`
reports which credential was accepted; `POST /api/pairings` allows only the
master token or a `canPair` session; `POST /api/pair` and `GET /api/auth` are
open. A session carries the master token's full authority, and scopes are not
built. A browser paired this way cannot open `/wt` yet, so the web client still
uses the master token. Routes are in [security.md](security.md).

### Configuration

Settings are settled once at startup. **An explicit flag beats the file, and
the file beats the default.** The file is
`~/Library/Application Support/loupe/config.json`, or wherever `-config`
points, written with the defaults on first run. `GET /api/config` reports each
setting's value, file value, source and whether a flag pins it; the fields and
the write rules are in [running.md](running.md#configuration). Auth stays out of
the file: the token has its own file and permissions.

### Adaptive bitrate

(When a viewer starts waiting for a keyframe, QUIC and WebTransport also reset
the inter-frame streams already written, so they stop competing with the
keyframe for the link.)

**Stale frames are sent late on QUIC and WebTransport.** An inter frame that
waited longer than `150ms + base/2` (cap 600ms) in the viewer's queue used to be
shed, which starts a keyframe wait. The 5G trace showed that loop: one
`shedStale`, 12-15 `shedAwaitKey`, a forced keyframe; 422 of 2604 frames, with
abandons near 0. The suspect is a keyframe write holding the serialized sender
so the next frame is stale on arrival. A stream-per-frame transport delivers
every frame it writes, so such a frame is now sent late, with a fresh write
deadline (one measured from its queue time is already spent), and counted in
`sentLate`. The viewer sheds and waits for a keyframe only when the run of stale
frames has lasted 1s (a draining backlog is many consecutive stale frames, so a
count does not tell it from a stuck link) or a frame waited over 600ms. The
pacer's admit decision is unchanged. WebSocket keeps the old rule: its write is
committed to TCP and blocks everything behind it, so shedding early is its only
lever.

**Fewer keyframe waits on QUIC and WebTransport.** Any dropped inter frame costs
a keyframe wait of about 20 frames, so three triggers are softened there (the
WebSocket rules are unchanged):

- Inter frames get a 600ms write-deadline floor (`lateWaitMax`) instead of
  250ms, plus base/2 and the jitter allowance, still capped at 1s. The 5G trace
  abandoned 5 inter writes near 300ms on a link whose writes reached 296ms.
- The pacer backs off only once the excess over its baseline has stayed above
  150ms continuously for 300ms (`backoffHold`); dropping under resets the hold.
  A ~200ms spike across a dozen frames no longer collapses `keep`.
- A new session on an unknown non-LAN path (adaptive, nothing remembered) opens
  at 2 Mbps (`unknownPathStartBps`) instead of the ceiling, which stays the full
  rate so the recover gate can climb to it. LAN paths have no path key and open
  at the ceiling. The trace row is `resumeSkipped` with `why: cautiousStart`.

`bitRate` is a ceiling, not a fixed rate. When the link cannot carry what the
encoder produces, the session relaunches the encoder lower and probes back up.
An encoder that can change rate while running (a simulator's) is changed in
place instead. `-adaptive-bitrate=false` turns it off; it defaults on.

How it decides:

- **The measure is goodput:** payload bytes that actually reached a viewer, not
  bytes offered to the link. A frame the transport reset mid-flight does not
  count as delivered. It is taken over busy intervals only (10 or more frames a
  second), and only once there are 2s of them making up half the active time. A
  static screen sends 2 small frames a second, which is what the screen needed
  rather than what the link carries, so the goodput-aimed gates decline
  (`idle`) until a window measured the link.
- **Delay cut (immediate):** when a viewer's current round trip stays above 2x
  its baseline, or baseline plus 80ms, for 0.5s (two consecutive one-second
  samples) while the screen is busy, the rate is cut at once, ahead of the
  windows and the change cap. It aims at 0.8 of the bytes the link carried
  during the run (a saturated link delivers its capacity), or 0.7 of the
  current rate when keyframe waits left nothing to measure. The result is raised
  to 0.8 of the window's carried rate when that is measured and to at least 0.5
  of the current rate (a run of a mostly still screen reads far below what the
  link carries: 14:22:38, 4M -> 473k from a 2s run; the trace row holds
  `uncappedBps` and `capped`), and never leaves more than 0.85 of the current
  rate. A queue that outlasts a cut gets the next one after the settle. It does not change the picture size. It
  does not count against the change cap, but is limited to 8 per 10 minutes
  (3 for a relaunched encoder, which also settles 12s after each). After one,
  fast recovery additionally needs the viewer to be free of keyframe waits, so a
  link whose round trip came back but is still draining a backlog is not
  stepped up. A delay cut within 45s of a fast step up turns fast recovery off
  for two minutes. A single spike, or a slow round trip on a near-idle screen,
  does not cut.
- **Loss cut (immediate), once the phone has decoded a frame:** the delay cut cannot see a link that drops instead
  of queueing (5G policing, 13:23 trace: round trip flat at 100-130ms, "delivered
  23 of 60" for seconds on end). When the viewer decoded under 0.7 of what was
  produced (after the report-phase allowance), with a fresh stale shed or abandon
  in the same second, for 2 consecutive reports while busy and the round trip is
  not over the delay line, the rate is cut at once. The event is required so one
  abandon and the 13-33 keyframe-wait frames behind it (one report) is one loss,
  not many: wait frames are never sent and are in neither count. A second path
  reads what was sent whole and not decoded, plus abandons, over what was
  attempted, above 0.3 (frames sent whole and not decoded need no event). A
  second without a client report neither extends nor ends the run. It aims at
  0.8 of the bytes carried during the run, else 0.7 of the current rate, never
  more than 0.85 of it; it does not change the picture size, does not count
  against the change cap, and has the delay cut's limit (8 per 10 minutes, 3 for
  a relaunched encoder) counted separately. Recovery waits for keyframe waits to
  clear, as after a delay cut. The reading comes from `healthTracker`
  (`TakeLoss`) through `Session.NoteDelivery`; the worst viewer's wins, a viewer
  that has decoded beating one that has not. Nothing acts on delivery counts
  until the viewer's client has reported a decoded frame: a phone that decoded 0
  for its first 6s while receiving 2.4 Mbps read 0.142 delivered and was cut
  4M -> 1.94M. A lossy reading declined for that reason writes a `lossSkipped`
  record (`why: noDecodeYet`) at most every 10s.
- **Down:** on sustained shedding the new rate is the measured goodput, floored
  at 250 kbps. On a starved link it is goodput again, and one step at most
  halves the rate. A link carrying under half the current rate while the viewer
  waits on keyframes for a fifth of the window steps down too, aimed at the
  rate carried outside keyframe waits (`carriedBps` over `clearMs`, busy
  intervals with no wait, at least 2s of them) rather than over the wait: a
  6s wait in an 11s window read 1.08 Mbps against 1.83 Mbps carried when it
  ended. A window with a wait in nearly every second has nothing outside one to
  measure, so those two gates fall back to goodput over the whole busy time.
  The step is in rate only: a
  simulator's picture is resized by this gate only once the rate is at its
  250 kbps floor (two undercarried windows there, a size-only step that leaves
  the rate alone). The 13:24:33 cut had taken 4 Mbps to 1.68 Mbps and 416x904
  together.
- **Up:** after 60s with almost no shedding and almost no keyframe waiting, by
  half, with a 12s settle after each change and at most six changes per session.
- **Fast recovery:** when each viewer's current round trip (not the smoothed
  one) sits within 2x its baseline, or baseline plus 40ms, with nothing shed for
  capacity, for 10s (5s live), the rate steps up by half without waiting for the
  60s. These steps do not count against the change cap. A step down within 45s of
  one turns fast recovery off for two minutes. Keyframe waits stop counting as a
  down signal once the link has read healthy for 3s, whether or not an abandon
  was shed in the same stretch.
- **A live-changing encoder** (a simulator's) settles for 2s, needs 15s clean to
  step up, and allows 24 changes in any 30 minutes.
- **Slowest viewer wins:** the encoder is shared, so it fits inside the slower
  link. An interval with no viewer attached throughout is not counted.

**A simulator also steps down in size as the rate falls**, because a low rate
over a large picture comes out as blocks. After each live rate change from the
windowed gates (not a delay or loss cut, and the undercarried gate only at the floor) the size is picked from a ladder (native, then
1080, 900, 720, 540) so each pixel gets at least 0.07 bits; the ladder stops at
900 because simcap has no runtime fps control. A relaunched encoder instead
lowers the capture rate (60, 30, 20, 15) at the same bits-per-pixel target
before anything else. A step up needs 1.5 times the target, and
the native size returns only at the rate the session opened with. A recording
holds the size, since the MP4 writer ends on a size change. Only simcap
advertises `resize`, so Android and synthetic are unaffected.

**Synthetic adapts too**, by relaunching ffmpeg. It ignores `MaxSize`. That
makes it the way to drive the controller without hardware:

```sh
loupe -addr :18470 -synthetic -no-devices -no-simulators -no-auth \
    -bitrate 4000000 -trace trace.jsonl -tls-cert ... -tls-key ...
go run ./cmd/linkshaper -listen 100.64.0.10:18471 -to 100.64.0.10:18470 \
    -plan "0s rate=0; 10s rate=400k delay=60ms queue=300ms"
go run ./cmd/wtprobe -for 75s "https://100.64.0.10:18471/wt?target=synthetic"
```

**A returning client resumes where it was.** When a viewer leaves, the
session's rate is remembered for its path (remote IP and transport) if it had
held for 20s. A new session opened for the same path within 30 minutes starts
at that rate, below the configured one, and the controller climbs back to the
configured rate by fast recovery. Unknown paths and LAN clients (private,
loopback, link-local) start at the configured rate; the memory is in process
and is lost on restart (`pathrate.go`). The size is not remembered.

**A simulator sharpens when the screen goes still.** After motion (10 or more
frames in the 2s before), when the screen has sent at most one frame in 400ms,
simcap's rate is raised to 2x (up to 8 Mbps), a keyframe is requested, and the
steady rate returns 0.8s later. One per still period (the keyframe's own frames
do not re-arm it; three frames in 400ms do), at least 5s apart, and only while
the rate has held for 20s, every viewer's round trip is near baseline, nothing
has been shed for 3s and no viewer awaits a keyframe (`sharpen.go`). Android
(scrcpy) and synthetic encoders cannot change rate live, so they are skipped.

**The trace names what the controller did and why.** Every second is a
`kind:"bitrate"` record with its gate. Delay and loss cuts carry their inputs
(`rttMs`, `baseMs`, `produced`, `delivered`, `sent`, `events`, `abandons`,
`deliveredFrac`, `sentLossFrac`, `runMs`, `runBytes`, `carriedBps`); a sharpen
carries `recentFrames` and `motionFrames`. The viewer's per-second record
carries `sentLate`, `writeMaxMs` and `keyWriteMaxMs` (the longest `SendFrame`
this second, overall and for keyframes and parameter sets), `staleOverKey`
(stale frames queued before the preceding keyframe write finished) and
`staleSustained` (stale runs that ended in a shed) and `cfgResent` (parameter
sets written ahead of a keyframe because the viewer had not had the current
ones: the fan-out drops config like any frame when a queue is full, and
`lastCfg` is cleared when the stream is replaced), next to `shedStale`. A still
period after motion that did
not sharpen writes one `sharpenSkipped` record with `why`: `shed`,
`rateNotHeld`, `rtt`, `noViewer`, `awaitKey` or `tooSoon`. Opening a session
writes `resume` (with `remembered`) or `resumeSkipped` (`why`: `noPath`,
`nothingRemembered`, `cautiousStart`, `notBelowCeiling`); a viewer leaving writes `resumeSaved`
or `resumeNotSaved`. A size-only step has `sizeOnly:true`.

Health forgives up to three decoder rejections within 3s of a parameter set (a
resize leaves frames of the old size in flight), counts keyframe waits only
through the delivery ratio, and a picture gets up to 300ms of extra write
deadline while the current round trip is above baseline.

The policy lives in `bitrate.go` and the mechanism in `relaunch.go`, apart so
the policy is cheap to test. On Android, scrcpy fixes the bitrate when the
encoder is configured, so a change means starting a second scrcpy server and
swapping it in; the new encoder opens before the old one closes, so a failed
relaunch leaves the session as it was.

## Client telemetry

The client reports what only it can know over `/api/log`, recorded in the trace
as `kind:"client"`. Read the trace (`-trace`), not the overlay, to act on a
problem: it is the one place the server's counters and the client's reports sit
on the same timeline.

| Record | Meaning |
| --- | --- |
| `boot` | Inline classic script in `index.html`, before the module: `isSecureContext`, `typeof VideoDecoder`, `typeof WebTransport`. Also installs `error` and `unhandledrejection` handlers |
| `module`, `module-end` | `app.js` entering and reaching its last line. `boot` and `load` without `module-end` means evaluation aborted; the stack is in the trace |
| `overlay`, `overlay-action` | Every dead-end or Connecting state shown (`title`, `state`), and a tap on one of its buttons |
| `ended` | The server ended a target: `target`, `reason` (`shutDown` or `deleted`), `via` (`msg` or `close`) |
| `pane-open`, `pane-close`, `pane-focus` | Panes on docked desktop; records from a pane carry `pane` and `target` |
| `decode-error` | The decoder's message and whether a config was held, at most once a second. One with `beforeSuspendMs` is the page leaving, not the stream |
| `sheet-*`, `config-put`, `settings-put`, `inspect-*`, `keyboard-*`, `clipboard-*`, `record-*`, `pip-*`, `newdevice-*` | UI actions, so a tap sits on the same timeline as the frames. Clipboard records carry text length, never the text |

Server-side records worth knowing:

- **`wt-attempt`** fires on entry to the `/wt` handler. Its absence means the
  request never reached HTTP, which points below QUIC.
- **`clipboard`** records the shared clipboard (`from` is `shared`, `mac`,
  `device`, `participant` or `peer`) with `textLen` or `error`, never the text.
- **`path`** is recorded for each viewer on Tailscale: `direct` (with
  `pathAddr` and `pathScope` of `lan` or `internet`), `peerRelay` or `derp`
  (with the region). It is read from `tailscale status --json` at open and 10s
  later, and recorded again only if it changed. `pathScope: internet` or `derp`
  means the phone is away from home. A viewer not on Tailscale records nothing.

## Known gaps

- **Auth is one shared token with no scoping.** Everyone who has it can drive
  every target, and the trace cannot tell two holders apart.
- **Arbitration is human against agent, and claims are agent against agent.**
  Two viewers both go through, and the last to touch holds the target. Claims
  and `X-Loupe-Viewer` are honest-name coordination, not enforcement.
- **iOS simulators have no rotation.** Touch, pinch, buttons, keys, text,
  screenshots, `/api/ui`, the bezel and adaptive bitrate work (see
  [ios.md](ios.md)). The HID path is private and the part most likely to break
  with an Xcode update.
- **A derived device frame is hidden in landscape.** Only the skin route turns
  (see Device frames in [targets.md](targets.md)).
- **No audio.**
- **H.265 streams end to end but cannot decode in the browser.** It needs an HEVC
  parameter-set parser, not a flag.
- **iPhone Safari cannot fullscreen a canvas**, only a `<video>`. Add to Home
  Screen gets close.
- **A session released after the 15s idle grace starts fresh.** Reconnecting
  inside that window resumes; after it the encoder is relaunched.
- **`Synthetic` ignores `MaxSize`:** it is fixed at 720x1560. It honours
  `MaxFPS` and `BitRate`.

## Environment notes

- **Do not move to scrcpy 4.x without changing the parser.** The jar is
  embedded and pinned, so nothing moves under a run, but 4.1 adds
  `PACKET_FLAG_SESSION` at bit 63 and shifts every video packet flag down one:

  | Flag | 3.3.4 | 4.1 |
  | --- | --- | --- |
  | `PACKET_FLAG_SESSION` | -- | bit 63 |
  | `PACKET_FLAG_CONFIG` | bit 63 | bit 62 |
  | `PACKET_FLAG_KEY_FRAME` | bit 62 | bit 61 |

  Against 4.1, `protocol.go` reads a config packet as a keyframe, a keyframe as
  neither, and adds 2^61 microseconds to every keyframe PTS, because bit 61
  falls inside `ptsMask`. `send_codec_meta` is renamed `send_stream_meta`, and
  an unknown option only warns instead of failing, so it would launch cleanly
  and misparse in silence. Control opcodes 0-17 are unchanged; 18-22 are
  appended for camera, torch and resize. Migrating is three constants, the
  option rename, and a device run.
- **scrcpy is Apache 2.0 and the jar is redistributed.** The license is at
  `third_party/scrcpy/LICENSE` and the attribution in `THIRD_PARTY_NOTICES.md`.
  Nothing here may imply this is scrcpy or that Genymobile endorses it.
- **`webtransport-go` is pinned at v0.13.0.** Older versions break Safari
  (v0.9.0 rejects every CONNECT; v0.10.0 leaves `createBidirectionalStream()`
  hanging). v0.13.0 needs its flow-control limits and Safari 27's older
  settings key set by hand; `TestWebTransportServerIsFullyConfigured` asserts
  both.
- **`web/` has no build step.** Plain ES modules, served from `embed.FS`.
