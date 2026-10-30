# Running loupe

Flags, the settings file, the stats overlay and the tests. How the server is
deployed under launchd, TLS and auth are in `docs/architecture.md` under
Running it.

## Flags

Settings are settled once at startup: **an explicit flag beats the file, and
the file beats the default.** The file is described under Configuration below.

| Flag | Default | Notes |
| --- | --- | --- |
| `-addr` | `:8080` | Listen address. Without TLS only a loopback address is allowed |
| `-config` | per-user dir | Settings file to read and write |
| `-tls-cert` / `-tls-key` | | Serve HTTPS directly. Required for WebTransport, and for any address but loopback unless `-insecure-http` |
| `-insecure-http` | `false` | Serve plain HTTP on an address other machines can reach |
| `-token` | cache file | Shared token every request must present |
| `-no-auth` | `false` | Serve with no token at all; anyone who can reach the port can drive any device |
| `-trace` | | Write per-second JSON-lines metrics here |
| `-t3-projects` | | Comma-separated T3 Code project paths a flag can be sent to (setting `t3Projects`, applies at once) |
| `-max-size` | `1080` | Longest video edge. The biggest lever on both encode cost and bandwidth |
| `-max-fps` | `60` | Capture rate cap |
| `-bitrate` | `4000000` | Bits per second; a ceiling, not a fixed rate |
| `-adaptive-bitrate` | `true` | Relaunch the encoder below `-bitrate` when the link cannot carry it |
| `-video-codec` | `h264` | `h265` and `av1` reach the device; only h264 decodes in the browser |
| `-keyframe-interval` | `0` | Seconds between keyframes, as the encoder counts them. 0 is 1 on Android, 5 on a simulator |
| `-max-buffer` | `50` | Client jitter-buffer ceiling in ms. Added tap latency, paid for clumped arrivals |
| `-human-hold` | `5000` | ms a viewer's last input keeps agents' `/api/input` off the device |
| `-shared-clipboard` | `false` | Share one clipboard across every open target and this Mac's pasteboard |
| `-synthetic` | `false` | Register the test pattern target |
| `-no-devices` | `false` | Skip adb device discovery |
| `-adb` | `adb` on PATH | Path to adb |
| `-android-sdk` | `ANDROID_HOME`, `ANDROID_SDK_ROOT`, then `~/Library/Android/sdk` | Android SDK root |
| `-emulator` | from the SDK | Path to the emulator binary |
| `-android-avdmanager` | `cmdline-tools/*/bin` in the SDK | avdmanager, which creates AVDs from installed system images |
| `-android-cli` | `/usr/local/bin/android`, then `/opt/homebrew/bin/android` | Google's android CLI, which creates AVDs from the picker |
| `-avd` | | AVD to boot at startup. Repeatable. Every installed AVD is listed either way |
| `-gpu` | `host` | Emulator GPU mode. **Only `host` reaches 60fps** |
| `-emulator-window` | `false` | Open the emulator's window when booting an AVD; off runs it headless with no audio |
| `-emulator-pause` | `300` | Seconds a headless emulator goes unused before it is paused; 0 never pauses |
| `-emulator-shutdown` | `1800` | Seconds a headless emulator goes unused before it is shut down; 0 never shuts one down |
| `-emulator-keep-state` | `false` | Save an emulator's state into its snapshot before shutting it down, so the next boot resumes it |
| `-scrcpy-server` | embedded | Path to a jar to use instead of the embedded one |
| `-scrcpy-version` | embedded | Version string for `-scrcpy-server`; must match that jar exactly |
| `-simcap` | `.build/simcap` | The simcap helper. Simulators are listed only when it exists |
| `-no-simulators` | `false` | Skip iOS simulator discovery |
| `-simulator-slim` | `false` | Boot simulators with background daemons disabled, which frees most of their memory |
| `-no-ios-devices` | `false` | Skip physical iPhone and iPad discovery through devicectl |
| `-xcrunner` | `.build/xcrunner` | XCUITest runner (`make xcrunner TEAM=...`); iPhones below iOS 27 take touches through it over a cable |

`loupe mcp`, `loupe pair`, `loupe sessions`, `loupe tap` and `loupe sequence`
are clients of a running server, with flags of their own and no banner.
`loupe tap` and `loupe sequence` are in `docs/agent-api.md` beside
`loupe mcp`.

## Configuration

The settings file is `~/Library/Application Support/loupe/config.json`, or
wherever `-config` points, written with the defaults on first run.
`GET /api/config` reports what the server uses, what is on disk, where each
value came from, and whether a flag pins it so a write would not take effect.
`docs/architecture.md` covers the file and the API under Configuration.

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
trace (`-trace`): the server writes one record per second joining its counters
with the client's report, which is the only place the two can be compared on
the same timeline. `docs/architecture.md` describes the fields under Client
telemetry.

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

Test against a spare server on another port, not one a phone is using.
`docs/traps.md` lists the traps this has already hit, symptom first.
