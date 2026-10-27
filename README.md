# loupe

Stream an Android device, an Android emulator or an iOS simulator to a browser
at 60fps, with real touch input. Built so the client can be an iPhone.

Single static Go binary. No runtime dependencies, no npm, no build step for the
client, and the scrcpy-server jar is embedded rather than installed separately.
iOS simulators need one more piece, a signed helper binary that `make simcap` downloads.

## Why this shape

The hard part of remote Android testing is not the video. It is input fidelity
and the honesty of your latency numbers. So:

- The browser speaks a **semantic** protocol (`finger 2 moved to x,y`), not
  scrcpy bytes. The backend is swappable and a native iOS client can reuse this
  server unchanged.
- Every target is behind one `Target` interface. A phone, an emulator, a
  simulator and a synthetic test pattern are interchangeable.
- The codec string handed to `VideoDecoder` is **derived from the stream's own
  SPS**, not guessed. A wrong guess silently drops iOS to software decode,
  which is the difference between a usable session and a hot phone showing a
  slideshow.
- The client reports what only it can know — decode rate, render rate, rtt as
  the phone measures it — and the server joins that with its own counters into
  one trace record per second. Numbers read off a phone screen are not
  evidence.

One result is worth stating before anything else: **the iPhone's hardware
decoder was never the bottleneck.** A trace of 48 steady-state samples showed
decoded fps median 60.0 with `decodeQueueSize` never exceeding 0 — the decoder
sat idle waiting for frames. Everything since has been transport work.

## Quick start

Validate the client half first, with no Android hardware involved:

```sh
go build -o loupe ./cmd/loupe
./loupe -synthetic -no-devices
```

Open the printed URL, which carries the access token. You should see a moving
test pattern and a stats overlay reading ~60 fps. **If that does not hold
60fps, nothing on the device side will fix it** — stop and debug the client,
not adb.

Then a real device:

```sh
adb devices                  # confirm it is listed and authorized
./loupe
```

scrcpy does not need to be installed. The server jar is embedded in the binary
and pushed on every launch, which is also why a hand-rolled scrcpy repro
behaves differently: scrcpy-server deletes its own jar on exit.

## Targets

- **Android devices** are tracked live with `adb track-devices`, so a phone
  plugged in after startup appears in the picker, and one unplugged stays
  listed as missing with the reason rather than vanishing.
- **Every installed AVD** is a target with the id `avd:<name>`, booted or not.
  Opening one boots it, and an emulator started from Android Studio is matched
  to its AVD by name. `-avd <name>` boots one at startup. **Only `-gpu host`
  reaches 60fps.**
- **Every available iOS simulator** is a target with the id `sim:<udid>`, once
  simcap is built:

  ```sh
  make simcap    # downloads the signed binary into .build/simcap
  ./loupe
  ```

  simcap reads the simulator's framebuffer, encodes it with VideoToolbox and
  writes scrcpy's wire format, so the rest of the server cannot tell it from a
  device. It also drives input through the simulator's HID stack, and
  `/api/ui` reads the accessibility tree. The frameworks it opens are private
  and move with Xcode, which is why it is built apart from `make build`.
- **`-synthetic`** registers a 60fps test pattern with no hardware behind it.

With the frame turned on in the viewer, an emulator is drawn in its SDK skin, a
phone in a bezel derived from what it reports about its display, and a
simulator in the chrome Xcode itself draws. The art is read from the installed
SDK or Xcode and never copied into this repo.

**Full screen** in the overflow menu hides every control and the bezel and
fits the stream to the whole page, so a simulator of your own phone fills its
screen. Hold the handle at the top edge to leave. In a Safari tab the address
bar stays, because iPhone Safari only lets video go full screen. Opened from
the Home Screen, the page asks for no Safari bars
(`apple-mobile-web-app-capable`); that has not been tried on iOS 27 yet.

**Phone keyboard**, also in the overflow menu, types with your phone's own
keyboard instead of the device's, on a simulator or an Android device.
Turning it on connects a hardware keyboard, so the device stops raising its
keyboard over half the stream: on a simulator it is Simulator.app's "Connect
Hardware Keyboard" switch, and on Android a virtual keyboard the scrcpy
server creates for the session and removes when it ends. **Keyboard** then
raises your phone's. What you type goes over the session. The choice is
remembered on the phone and sent again each time it connects.

**Clipboard** goes both ways, on a simulator or an Android device. Copy
something on the device and a banner offers it; tap it and it is on your
phone. **Paste to device** in the overflow menu sends your phone's clipboard
into the field that has focus, after iOS asks you to allow the paste.
**Copy from device** reads the device's clipboard on demand. On Android this
needs a session open.

## Transports

Both implement one `Conn` interface, so nothing above them branches on which is
live. The client picks WebTransport and falls back to WebSocket; `?transport=ws`
on the same link forces the old path, which is how the two get compared.

- **WebSocket** is TCP: reliable and ordered, and therefore unable to abandon a
  frame once written. A frame that has stopped mattering is still delivered,
  ahead of everything behind it.
- **WebTransport** is QUIC, one unidirectional stream per access unit. Frames
  are delivered independently, each is whole or absent, and the sender can
  reset a frame that goes stale mid-flight.

WebTransport needs HTTP/3, so it cannot be run behind a TCP proxy. Serve TLS
directly with `-tls-cert` / `-tls-key`.

## Auth

Every request needs a shared token, because `/api/input` means reaching the
port is enough to drive a device rather than only to watch one. `-no-auth`
turns it off and says so at startup.

The token comes from `-token`, then `LOUPE_TOKEN`, then
`~/Library/Caches/loupe/token`, and is generated into that file if absent. It
is presented three ways because three callers cannot share one: an agent sends
`Authorization: Bearer`, a returning browser sends the cookie, and a freshly
opened link carries `?token=`, which a page load then redirects away so it does
not sit in history or leak through `Referer`.

It is one token with no scoping. Everyone holding it can drive every target,
and the trace cannot tell two holders apart.

## Agent API

Every endpoint takes `?target=`, so a process can drive a device while a human
watches the same session. `docs/api.schema.json` is the full request and
response schema.

- `GET /api/ui` returns the view hierarchy: `uiautomator dump` on Android, the
  accessibility tree on a simulator. `?interactive=1` prunes layout
  containers — on one Compose screen, 6202 bytes down to 3620. Each node
  carries a precomputed `center`, because that is the one derivation every
  caller needs and leaving it to them invites rounding disagreements that land
  a tap on a neighbour.
- `POST /api/input` takes `{"action": ...}` with one of `tap`, `swipe`,
  `text`, `key`, `button`, `pinch`, `rotate` or `hardwareKeyboard`. A target
  that cannot do an action answers 501: `pinch` is simulator-only and
  `rotate` Android-only today. `hardwareKeyboard` (`{"on": true|false}`)
  works on both, and on Android needs a session open, answering 409
  without one. `launch` (`{"package": ..., "activity": ...}`) opens an
  installed app: its launcher activity, or the named one on Android, and
  a bundle ID on a simulator.
  `move` (`{"x": ..., "y": ...}`) touches nothing and moves the agent's
  cursor on every viewer, so an agent can point before it acts.
- `GET /api/clipboard` returns `{"text": ...}`, the device's clipboard, and
  `POST /api/clipboard` sets it from `{"text": ..., "paste": true|false}`.
  With `paste` the text also goes into the focused field: through scrcpy's
  own paste on Android, typed on a simulator. Android needs a session open
  and answers 409 without one.
- `GET /api/screenshot` returns one PNG at the device's own resolution, with
  no session open.

**Coordinates are device pixels, not stream coordinates.** `/api/input` goes
through `adb shell input` on Android and simcap on a simulator, so what
`/api/ui` reported is what gets tapped, with no session required and no scale
factor to get wrong. On Android it costs a few hundred milliseconds per call,
which is the right trade for a caller working from the hierarchy and the wrong
one for a finger on a screen.

**Xcode's Device Hub can take a simulator's buttons and keys away.** When
CoreDevice's HID route opens, simcap goes around it and nothing needs doing.
When it does not, a simulator this server booted is taken back automatically,
and any other answers 409 with `code: "hubHeld"` until the caller sends
`{"action":"reclaim"}`. A reclaim restarts SpringBoard, takes about 9s, and
closes every app but the one in front, which it reopens.

**Claim a device before driving it.** Two agents on one device tap through each
other's tests, so an agent says which device is its own:

```sh
curl -k -X POST -H "Authorization: Bearer $(cat ~/Library/Caches/loupe/token)" \
  -H "X-Loupe-Agent: claude" -H "X-Loupe-Session: $SESSION" \
  -d '{"link": "https://example.com/session"}' \
  "https://localhost:18456/api/claims?target=$TARGET"
```

`-k` is needed because the TLS certificate names the tailnet host, not
`localhost`.

- `POST /api/claims?target=` claims the device, or renews the caller's own
  claim, and answers 200 with the claim: `target`, `agent`, `session`, `link`,
  `since` and `idleMs`. The caller needs a name, from `X-Loupe-Agent` and/or
  `X-Loupe-Session` (or `agent` and `session` in the body); with neither it is
  a 400. Names are printable ASCII, 64 bytes at most. The optional `link` must
  be an absolute https URL of at most 512 bytes, and the picker draws it. A
  claim is held by the session when one is named, otherwise by the agent.
- `DELETE /api/claims?target=` releases it with 204, also when nobody holds
  it. Another caller's claim answers 409 `claimedBy`; `?force=1` releases it
  anyway.
- `GET /api/claims` lists every live claim, oldest first. `GET /api/targets`
  also carries each row's `claim`, `handsOff` and `drivenBy`.

A claim is a lease. Any call the holder makes to a target endpoint, reads
included, renews it, and it lapses 10 minutes after the last one. Claims are
kept in memory, so a server restart drops them.

Input from anyone else on a claimed target answers 409 with
`code: "claimedBy"` and the holder's claim under `claim`; claiming it yourself
answers the same. A device the person marked hands off in the picker answers
409 with `code: "handsOff"` to every agent, the claim's holder included, and
that mark survives a restart. Pick another device, or ask; neither is a lock a
holder can force. Only `/api/input`, `launch` through `/api/install`, and
device boot, shutdown and delete are refused this way. Reads such as
`/api/ui` and `/api/screenshot` are not, and a person at a viewer is never
refused. The names are what callers say they are, so this is coordination, not
security: the token is still shared.

Do not drive this from the video. Locating a button in a decoded frame is image
work to recover what the accessibility tree states outright, and the tree
survives a theme change, a resolution change and a downscale.

## Configuration

Settings are settled once at startup: **an explicit flag beats the file, and
the file beats the default.** The file is
`~/Library/Application Support/loupe/config.json`, or wherever `-config` points,
and it is written with the defaults on first run so there is something to edit.

`GET /api/config` reports each setting four ways. The last two exist because a
settings UI that cannot see them will claim success for a write that did
nothing:

| Field | Meaning |
| --- | --- |
| `value` | what the server is actually using |
| `fileValue` | what is on disk |
| `source` | `default`, `file` or `flag` |
| `pinned` | a flag is overriding the file, so a write will not take effect |

A `PUT` is all-or-nothing. Applying three of four settings and then failing
leaves the caller unable to say what took effect.

`applies: "nextSession"` does not mean immediately: an open session keeps the
options it started with, and the idle grace is 15s, so a viewer already
watching sees nothing change until their session ends.

## Adaptive bitrate

`bitRate` is a ceiling, not a fixed rate. When the link cannot carry what the
encoder produces, the session relaunches the encoder lower and probes back up.
`-adaptive-bitrate=false` turns it off. The synthetic target adapts by
relaunching ffmpeg at the new rate, which makes it the way to drive the
controller through `cmd/linkshaper` without a device.

It has to be a relaunch rather than a control message: scrcpy's opcodes run 0
to 17 and none of them set the bitrate, which is fixed when the encoder is
configured. A simulator is the exception: VideoToolbox changes rate on the
running encoder, so simcap takes the new rate on stdin and nothing restarts.
The new encoder opens before the old one closes, so a relaunch that
fails leaves the session exactly as it was. Measured on a Pixel-class device
with `cmd/overlaptest`, the second server came up in 488ms beside the live
first, both streamed during the overlap, and the second kept producing after
the first shut down.

The decision is measured goodput — payload bytes that actually reached a
viewer — floored at 250 kbps, stepping back up by half after 60s of almost no
shedding, capped at six changes per session so a marginal link cannot
oscillate.

## Flags

| Flag | Default | Notes |
| --- | --- | --- |
| `-addr` | `:8080` | Listen address |
| `-config` | per-user dir | Settings file to read and write |
| `-max-size` | `1080` | Longest video edge. The biggest lever on both encode cost and bandwidth |
| `-max-fps` | `60` | Capture rate cap |
| `-bitrate` | `4000000` | Bits per second; a ceiling, not a fixed rate |
| `-adaptive-bitrate` | `true` | Relaunch the encoder below `-bitrate` when the link cannot carry it |
| `-video-codec` | `h264` | `h265` and `av1` reach the device; only h264 decodes in the browser |
| `-keyframe-interval` | `0` | Seconds between keyframes, as the encoder counts them. 0 is 1 on Android, 5 on a simulator |
| `-max-buffer` | `50` | Client jitter-buffer ceiling in ms. Added tap latency, paid for clumped arrivals |
| `-avd` | | AVD to boot at startup. Repeatable. Every installed AVD is listed either way |
| `-gpu` | `host` | Emulator GPU mode. **Only `host` reaches 60fps** |
| `-adb` | `adb` on PATH | Path to adb |
| `-emulator` | from the SDK | Path to the emulator binary |
| `-android-sdk` | `ANDROID_HOME`, `ANDROID_SDK_ROOT`, then `~/Library/Android/sdk` | Android SDK root |
| `-synthetic` | `false` | Register the test pattern target |
| `-no-devices` | `false` | Skip adb device discovery |
| `-simcap` | `.build/simcap` | The simcap helper. Simulators are listed only when it exists |
| `-no-simulators` | `false` | Skip iOS simulator discovery |
| `-scrcpy-server` | embedded | Path to a jar to use instead of the embedded one |
| `-scrcpy-version` | embedded | Version string for `-scrcpy-server`; must match that jar exactly |
| `-tls-cert` / `-tls-key` | | Serve HTTPS directly. Required for WebTransport |
| `-token` | cache file | Shared token every request must present |
| `-no-auth` | `false` | Serve with no token at all |
| `-trace` | | Write per-second JSON-lines metrics here |

## Reading the stats overlay

| Metric | Good | Meaning |
| --- | --- | --- |
| `fps` | 58-60 | Frames actually painted to the canvas |
| `dec` | matches `fps` | Frames decoded. A gap between the two is presentation, not decode |
| `rtt` | < 60ms | Round trip as the phone measures it |
| `jitter` | two digits | Transit variation, against the sender's own frame spacing |
| `queue` | 0-2 | `decodeQueueSize`. Sustained growth means the decoder cannot keep up |
| `drop` | 0 | Client-side drops / decoder errors |
| `buf` | well under `-max-buffer` | Current jitter-buffer depth. At the ceiling is pure added latency |
| `via` | `wt` | Which transport is live |
| `shed` | 0 | Frames the server abandoned rather than send late |
| `kbps` | ~bitrate | Actual received rate |

`jitter` and `buf` are the pair to watch. A stream can report 60fps and still
feel wrong if frames arrive unevenly, and a buffer pinned at its ceiling is
latency you are paying for nothing.

The overlay is for glancing at. For anything you intend to act on, read the
trace: the server writes one record per second joining its counters with the
client's report, which is the only place the two can be compared on the same
timeline.

## Running it as a service

The Makefile installs a launchd agent that starts at login and comes back after
a crash:

```sh
make agent-install   # write the LaunchAgent plist and bootstrap it
make build           # this IS the deploy: the supervisor restarts onto it
make agent-status    # state, pid, last exit
make agent-log       # tail the run log
make agent-uninstall
```

`loupe-supervisor.sh` runs a **copy** of `./loupe` from `.run/`, never `./loupe`
itself: macOS returns ETXTBSY on a write to a running executable, so running the
original would make `make build` fail for as long as the server was up. The
supervisor polls `./loupe`'s sha256 every 2s and restarts onto a new build,
which is why building is the whole deploy step.

The supervisor passes only what it genuinely owns — the port, the cert it
issues, and the trace file it rotates. Encoder settings are deliberately left to
the config file, because a flag on that line would be a setting `/api/config`
reports as `pinned` and the API cannot change.

## Tests

```sh
make test    # go test ./... and every web/*.test.mjs
```

Server-side WebTransport is proven with `cmd/wtprobe`, a native Go client,
rather than inferred from a browser. Point it at the address the phone uses:
loopback has a 16KB MTU and will hide anything the path cannot carry.

```sh
go run ./cmd/wtprobe "https://<host>:18456/wt?target=synthetic"
```

`cmd/simprobe` does the same for simcap on its own, reading its output with the
scrcpy parser:

```sh
go run ./cmd/simprobe -animate <udid>
```

`docs/traps.md` carries the traps this has already hit, each one with the symptom
first, because that is how you will meet it again.

## Known gaps

Ordered by how likely each is to bite you.

1. **Auth is one shared token with no scoping.** It is enough to stop the port
   being open, which is what `/api/input` made urgent, but everyone who has it
   can drive every target, and the trace cannot tell two holders apart.
2. **No input arbitration.** Any viewer's touch, key or scroll goes straight to
   the device with no driver check, and `/api/input` is a second writer beside
   them, so an agent and a human can fight over the same screen without either
   seeing the other.
3. **iOS simulators cannot rotate.** Touch, pinch, buttons, keys, text,
   screenshots, `/api/ui`, the bezel and adaptive bitrate work. `rotate`
   answers 501.
4. **The simulator input path is private API.** simcap calls SimulatorKit and
   CoreDevice symbols with no public header, each checked on Xcode 27. It is
   the part most likely to break with an Xcode update.
5. **`/api/ui` on a simulator can report a covered node as tappable.** A node
   is marked `occluded` when another element answers its centre, but blank
   space drawn over a row lets the check through to the row underneath.
6. **No install or logcat endpoint.** The loop this is for is change code,
   build, install, drive, look. The middle step is still a separate adb or
   simctl invocation.
7. **No audio.** The server launches scrcpy with `audio=false`. Adding it means
   a second socket and a parallel decode path in the client.
8. **H.265 streams end to end and still cannot decode.** Frames come back clean
   and correctly delimited, but the codec string is derived by looking for an
   H.264 SPS, so the client is configured with a provisional `avc1` string and
   fails. Making it work is an HEVC parameter-set parser, not a flag.
9. **A bezel does not follow a flip between the two landscapes.** The frame is
   refetched when the stream changes shape, and 90 to 270 degrees does not
   change it, so the punch hole stays on the wrong edge until the device passes
   through portrait. A bezel derived for a physical phone is hidden in
   landscape altogether.
10. **iPhone Safari cannot fullscreen a canvas**, only a `<video>` element. Add
    to Home Screen gets close.
11. **A session released after the 15s idle grace starts fresh.** Reconnecting
    inside that window resumes; after it, the encoder is relaunched.

## Licensing

loupe is Apache 2.0. It embeds and redistributes the scrcpy-server jar, which
is also Apache 2.0; `third_party/scrcpy/LICENSE` and `THIRD_PARTY_NOTICES.md`
carry that text and the upstream attribution. Section 6 grants no trademark
rights: nothing here may imply this is scrcpy or that Genymobile endorses it.
