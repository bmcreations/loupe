# loupe

Stream an Android device or emulator to a browser at 60fps, with real touch
input. Built so the client can be an iPhone.

Single static Go binary. No runtime dependencies, no npm, no build step for the
client, and the scrcpy-server jar is embedded rather than installed separately.
iOS simulators and iPhones are coming soon.

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
./loupe -addr 127.0.0.1:8080 -synthetic -no-devices
```

Open the printed URL, which carries the access token when the server runs in a
terminal. You should see a moving
test pattern and a stats overlay reading ~60 fps. **If that does not hold
60fps, nothing on the device side will fix it** — stop and debug the client,
not adb.

Then a real device:

```sh
adb devices                  # confirm it is listed and authorized
./loupe -tls-cert host.crt -tls-key host.key
```

Without TLS the server only binds loopback: over plain HTTP the token, the
cookie and the stream would cross the network in the clear. `-insecure-http`
lifts that.

scrcpy does not need to be installed. The server jar is embedded in the binary
and pushed on every launch, which is also why a hand-rolled scrcpy repro
behaves differently: scrcpy-server deletes its own jar on exit.

## Targets

- **Android devices** are tracked live with `adb track-devices`. A phone
  plugged in after startup appears in the picker, and one unplugged stays
  listed as missing, with the reason.
- **Every installed AVD** is a target with the id `avd:<name>`, booted or not.
  Opening one boots it. **Only `-gpu host` reaches 60fps.**
- **iOS simulators and iPhones** are coming soon.
- **`-synthetic`** registers a 60fps test pattern with no hardware behind it.

The viewer can draw each device in its own frame, type with your phone's
keyboard instead of the device's, and pass the clipboard both ways.
`docs/targets.md` covers each target and those viewer features.

## Agent API

Every `/api/*` endpoint takes `?target=`, so an agent can drive a device while
a person watches the same session:

```sh
curl -H "Authorization: Bearer $(cat ~/Library/Caches/loupe/token)" \
  "http://127.0.0.1:8080/api/ui?target=avd:Pixel_10&interactive=1"
```

`/api/ui` returns the accessibility tree with a precomputed `center` for each
node, and `/api/input` taps and types in the same device pixels.
`POST /api/camera` points an AVD's or iOS simulator's back camera at a PNG or
JPEG, so a scanner can be tested without holding a phone up to a screen;
`DELETE` gives the camera back. `loupe mcp`
exposes the same routes as an MCP server on stdio. `docs/agent-api.md` lists
every route, and how claims stop an agent and a person fighting over one
screen.

## Running it as a service

```sh
make agent-install   # write the LaunchAgent plist and bootstrap it
make build           # this IS the deploy: the supervisor restarts onto it
make agent-status    # state, pid, last exit
make agent-log       # tail the run log
```

The supervisor runs a copy of `./loupe` from `.run/` and restarts onto a new
build within 2s. `docs/architecture.md` explains why, under Running it.

## Docs

`docs/README.md` indexes everything. The ones most people need:

| Doc | For |
| --- | --- |
| `docs/running.md` | Every flag, the settings file, reading the stats overlay, tests |
| `docs/targets.md` | AVDs, devices, the phone keyboard, the clipboard, device frames |
| `docs/agent-api.md` | Driving a device from a script or an agent |
| `docs/extensions.md` | Building an extension that loupe proxies under `/api/ext` |
| `docs/architecture.md` | How it fits together, and the known gaps |
| `docs/traps.md` | Search it for the symptom you are seeing before debugging |

## Licensing

loupe is Apache 2.0. It embeds and redistributes the scrcpy-server jar, which
is also Apache 2.0; `third_party/scrcpy/LICENSE` and `THIRD_PARTY_NOTICES.md`
carry that text and the upstream attribution. Section 6 grants no trademark
rights: nothing here may imply this is scrcpy or that Genymobile endorses it.
