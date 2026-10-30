# loupe

Stream Android devices, emulators and iOS simulators from your Mac to a
browser at 60 fps, with live touch input. The browser can be an iPhone's
Safari, so you can test on any device from your phone. Agents can drive
the same device through an HTTP API or MCP while you watch.

loupe is one Go binary, and Go is all it takes to build. The scrcpy-server
jar is embedded, so there is no scrcpy to install, and the web client has
no build step, so there is no npm. At runtime it uses the tools your
targets already need: adb, the Android emulator, Xcode.

## What it does

**Streams devices**

- **Android phones over adb.** They are tracked live: one plugged in later
  appears in the picker, and one unplugged stays listed as missing, with
  the reason.
- **Every installed AVD**, booted or not. Opening one boots it from a warm
  snapshot. loupe pauses an idle emulator after 5 minutes and shuts it down
  after 30. Reset returns one to a clean state, and New device creates an
  AVD from the system images you have installed.
- **iOS simulators**, through [simcap](https://github.com/bmcreations/simcap), a
  Swift helper (`make simcap`). Physical iPhones and iPads are a preview: they stream
  over a cable, and input is still being worked on. See
  [docs/ios.md](docs/ios.md) for what simcap relies on and the supported
  use.
- **Devices on your other machines.** Add another loupe server you run as a
  peer and its targets join your picker. See
  [docs/remote-targets.md](docs/remote-targets.md).

**Feels like the device is in your hand**

- Video goes over WebTransport (HTTP/3), with WebSocket as a fallback.
  Adaptive bitrate lowers the encoder's rate when the link can't carry it,
  and raises it again when the link recovers.
- Multi-touch, swipes and hardware buttons.
- Type with the phone's own keyboard instead of the device's.
- The clipboard works both ways, and can be shared across every open
  target and the Mac's pasteboard.
- Each device is drawn in its own frame.
- Open a link in Safari, then add it to the Home Screen to run loupe full
  screen as an app.

**Lets agents work beside you**

- An HTTP API covers the accessibility tree with a precomputed `center` per
  node, tap, swipe, text, keys, app launch, deep links, screenshots, APK
  install, photo and video import, logs, crash reports, and boot and
  shutdown. On a debuggable Compose app, `/api/ui?compose=1` names each node
  after the composable that drew it, with its source line.
- `loupe mcp` serves the same routes as MCP tools, so an agent such as
  Claude Code needs no curl.
- An agent claims a device before it drives it. Your own input holds
  agents off, and you can mark a device hands off entirely. Viewers see the
  agent's cursor before it taps.
- **Flags** let you box an area of the screen, write a note, and send the
  screenshot and UI tree to an agent, or interrupt one mid-task.
- **Recording** writes the encoder's output to an MP4, with no re-encode.

**Fits into your workflow**

- A GitHub Action installs a pull request's APK on a target and comments on
  the PR with a link that opens it. See [action/README.md](action/README.md).
- Extensions are separate programs that loupe proxies under `/api/ext`, and
  they can draw overlays on a device. See [docs/extensions.md](docs/extensions.md).
- `loupe pair` gives a device its own session, which you can revoke without
  re-keying every other device.

## Quick start

```sh
git clone https://github.com/bmcreations/loupe
cd loupe
make build
./loupe -addr 127.0.0.1:8080 -synthetic -no-devices
```

The test pattern is generated with ffmpeg (`brew install ffmpeg`). Open the
printed link. You should see a moving test pattern with a stats
overlay at about 60 fps. If that doesn't hold 60 fps, nothing on the device
side will fix it, so debug the browser first.

Then plug in a device and drop `-synthetic -no-devices`. To reach loupe from
a phone you need TLS, since without it the server binds loopback only.
[docs/getting-started.md](docs/getting-started.md) walks through TLS with
Tailscale, iOS simulators, running loupe as a launchd service, and
connecting an agent.

## Driving a device from a script

```sh
TOKEN=$(cat ~/Library/Caches/loupe/token)
curl -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:8080/api/ui?target=avd:Pixel_10&interactive=1"
curl -H "Authorization: Bearer $TOKEN" -X POST \
  -d '{"action":"tap","x":540,"y":1200}' \
  "http://127.0.0.1:8080/api/input?target=avd:Pixel_10"
```

Coordinates are device pixels, the same ones `/api/ui` reports. Drive
from the accessibility tree rather than from the video.
[docs/agent-api.md](docs/agent-api.md) lists every route.

## Security

Anyone who can reach the port can drive every device on it, so every
request needs the shared token or a paired session, and plain HTTP is
refused on anything but loopback. The token and every session have full
access; there are no scopes yet. [docs/security.md](docs/security.md)
says what is protected and what isn't.

## Why the numbers can be trusted

The browser reports what only it can measure: decode rate, render rate,
and round trip as the phone sees it. The server joins that with its own
counters into one trace record per second (`-trace`). The codec string
handed to `VideoDecoder` comes from the stream's own SPS rather than a
guess, because a wrong guess silently drops iOS to software decoding.

That trace settled the first question anyone asks. Over 48 steady-state
samples the iPhone decoded at a median of 60.0 fps with its decode queue
never above 0: the hardware decoder sat idle, waiting for frames. All the
work since has been on the transport.

## Docs

| Doc | For |
| --- | --- |
| [getting-started.md](docs/getting-started.md) | From a clone to a phone, a service and an agent |
| [running.md](docs/running.md) | Every flag, the settings file, reading the stats overlay, tests |
| [targets.md](docs/targets.md) | AVDs, Android devices, the phone keyboard, the clipboard, device frames |
| [ios.md](docs/ios.md) | iOS simulators, and physical iPhones in preview |
| [agent-api.md](docs/agent-api.md) | Every `/api/*` route, claims and the human hold |
| [mcp.md](docs/mcp.md) | `loupe mcp` and pushing flags into Claude Code |
| [security.md](docs/security.md) | The token, TLS, pairing and sessions |
| [traps.md](docs/traps.md) | Search it for the symptom you see before debugging |

[docs/README.md](docs/README.md) indexes the rest, including architecture,
the wire protocol, flags, recording and extensions.

## Licensing

loupe is Apache 2.0. It embeds and redistributes the scrcpy-server jar,
which is also Apache 2.0; `third_party/scrcpy/LICENSE` and
`THIRD_PARTY_NOTICES.md` carry that text and the upstream attribution.
Section 6 grants no trademark rights: nothing here may imply this is scrcpy
or that Genymobile endorses it.

simcap, the iOS helper, is also Apache 2.0 and lives in its own repo,
[bmcreations/simcap](https://github.com/bmcreations/simcap). `make simcap`
clones and builds it with Xcode; loupe runs without it (Android only). It calls private
frameworks from your own Xcode install; [docs/ios.md](docs/ios.md) says
which, and what that means for an Xcode update.

iPhone, iPad, Mac, macOS and Xcode are trademarks of Apple Inc. loupe is
not affiliated with Apple.
