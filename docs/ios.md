# iOS simulators and iPhones

iOS simulators are supported. Physical iPhones and iPads are a preview: a
paired phone is listed and streams, but touch below iOS 27 needs an XCUITest
runner. Android targets, the phone keyboard, the clipboard and device frames
are in `docs/targets.md`.

## Install simcap

Both need simcap, an Apache 2.0 Swift helper from
[bmcreations/simcap](https://github.com/bmcreations/simcap). loupe runs
without it, with Android targets only.

| Command | Result |
| --- | --- |
| `make simcap` | Clones it into `.build/simcap-src`, builds it with Xcode and writes `.build/simcap`. Enough for simulators. |
| `make simcap TEAM=<team id>` | The same, signed with your Apple Development identity. Streaming a phone needs this (see "Sign simcap with a team"). |
| `brew install --HEAD bmcreations/tap/simcap` | Builds the same source ad hoc onto PATH. Enough for simulators. |

`-simcap` overrides the path. simcap is not part of `make build`.

simcap uses private frameworks that ship with Xcode (CoreSimulator,
SimulatorKit, AccessibilityPlatformTranslation), loaded from your own Xcode
install. Apple does not support them, and an Xcode update can break simcap
until it is fixed. If simcap stops building or launching after an update,
file an issue on its repo.

## iOS simulator targets

Every available iOS simulator is a target, booted or not, with the id
`sim:<udid>`. Opening one boots it if needed. Only iOS runtimes are listed,
and simcap has only been run against iPhones.

- **Simulators are listed only when simcap is built.** Without it they would
  be in the picker and fail every open with "simcap is not built".
- The list refreshes every 5s. A failed listing keeps the last one rather
  than marking every simulator missing.
- **Shut Down and Delete** in the picker stop a simulator and remove it. A
  stopped simulator refuses a reopen while the stop runs and for 10s after
  it. A deleted simulator is
  dropped from the list at once, not left as a dimmed `missing` row.

### What works

| Feature | Behavior |
| --- | --- |
| Video | Honours `maxSize`: the longer side is scaled down, so a simulator uses about the same bandwidth as an Android device capped at 1080. |
| Input | Taps, swipes, Home, Lock, app switcher (two Home presses) and typing, from the viewer and `/api/input`. Works with no viewer session open. |
| Coordinates | `/api/input`, `/api/ui` and `/api/screenshot` use framebuffer pixels. Touches in the stream are scaled up for you. |
| Two fingers | The stream turns a second finger into a two-point gesture, which zoomed a photo in Photos. `/api/input {"action":"pinch","x","y","scale"}` runs the same gesture, spreading over 30% to 60% of the screen's short side for a scale of 2. No rotation. |
| `/api/ui` | The accessibility tree of the frontmost app, SpringBoard on the Home Screen. A node's `center` goes straight into `/api/input`. The label is `text`; a text field's contents or placeholder are `value`, which Android never sets. |
| Bitrate | Adaptive bitrate changes the encoder in place, with no restart or keyframe wait (see Adaptive bitrate in `docs/architecture.md`). |
| Keyboard | Opening the phone's keyboard from the viewer hides the simulator's on-screen keyboard, so it does not cover the lower half. |
| Open URL | `https` links open in the app that claims them (below). |
| Drop | A picture dropped on a text field is pasted (below). |

**`occluded` catches only part of what is covered.** With Settings' search
open, the root list is still in the tree under the results, and tapping a
covered row opens the wrong thing. A node is marked `occluded` when another
element answers at its centre or the centre is off the screen, and
`?interactive=1` drops those. The check follows the accessibility tree, not
the pixels, so blank space drawn over a row does not mark it.

A dump takes 50-70ms on the Home Screen and under a second on a busy screen,
and 1-4.6s the first time after the input process starts. The input process
stops after two minutes idle and restarts on the next command.

### Boot with fewer background daemons

`-simulator-slim` (config `simulatorSlim`) boots a simulator with 168
background daemons disabled. On an iPhone 17 Pro it cut memory footprint from
about 8.9 GB to 2.8 GB. Frame rate did not change, so it is a memory setting,
not a smoothness one.

- It applies only to boots loupe performs. A simulator already booted, or
  booted from Xcode, keeps what it last booted with.
- iOS 18.5 and newer. Older runtimes are left alone.
- `com.apple.assetsd` stays on: it is the Photos library, which `/api/media`
  needs.
- Turning the setting off re-enables only the daemons loupe disabled, so
  overrides you set another way stay.
- Each slim boot traces `{"kind":"simulator","action":"slim"}` with `wanted`
  and `honoured`. `honoured` is read back from the simulator and is the only
  evidence a slim boot took.

### Device Hub and buttons

Xcode 27 replaced Simulator.app with Device Hub. While it holds a simulator
through the legacy HID client, Home and keys answer `ok` and do nothing.
simcap normally sends input through CoreDevice, as Device Hub does, which
goes around the hold. Only if simcap cannot open that route does the legacy
client apply:

- On a simulator this server booted or has reclaimed once, a button or key
  that finds the hold takes the simulator back and then goes through. This
  restarts backboardd and relaunches the front app: it takes about 10s, what
  the app held in memory is lost and the other apps close. It holds until
  Device Hub is relaunched.
- On any other simulator the answer is 409 with `code: "hubHeld"`, since
  someone else may be using it. The viewer asks before sending
  `/api/input {"action":"reclaim"}`.
- Each reclaim is traced as `kind:"agent"`, `action:"reclaimed"`, with
  `tookMs` and `reopened`.

### Universal links on a simulator

`simctl openurl` hands every https URL to Safari. loupe works out which
installed app claims the link and opens it there, as a tap on the link would.
An app is a candidate when its `associated-domains` entitlement has
`applinks:<host>` (or `applinks:*.<domain>`) and the host's
apple-app-site-association file includes the URL's path. The file comes from
Apple's CDN, then from the host's `/.well-known/`.

If nothing claims the URL, the file cannot be fetched or the helper fails,
the URL goes to `simctl openurl`. A custom scheme such as `flipcash://open`
also goes through `simctl`. `/api/input`'s open-URL action reports the app's
bundle ID as `started` (`docs/agent-api.md`).

### Drop a picture on a field

Dropping an image on a text field in the viewer pastes it there. A photo or
video dropped anywhere else goes to the device's library (Photos). Both are
iOS-simulator behaviors. The routes and error codes are `POST /api/drop` and
`POST /api/media` in `docs/agent-api.md`.

### An iPhone Duo

A Duo streams whichever panel is lit, so a folded Duo does not stream black.
The viewer's fold button changes posture (`/api/settings` offers `posture`
as `closed` or `opened`; a fold takes about 2.4s and an unfold 1s). Every
other simulator answers 501 to a posture change.

### Untested

- Two fingers from a real viewer on a phone.
- Typing from an iPhone: autocorrect, dictation and a held Backspace are
  unmeasured.
- The swipe gesture and the overlay in a browser through the self-signed
  address, Safari on a real iPhone and VoiceOver.

## Physical iOS devices

A preview. Streaming works over a cable, and over Wi-Fi from iOS 27. Input
depends on the iOS version, below.

Every iPhone and iPad paired with this Mac is a target, `ios:<udid>`, over a
cable or the network. They are listed from `xcrun devicectl list devices`
every 10s. `-no-ios-devices` turns the listing off.

| State | Meaning |
| --- | --- |
| `device`, transport `wired` or `localNetwork` | Reachable. |
| `disconnected` | An idle tunnel. The phone is still reachable and the tunnel reopens when something uses it. |
| `offline` / `unavailable` | Listed by devicectl, not reachable, no transport. |
| `missing` | devicectl no longer lists it. |

A phone is never opened by default; Android devices rank ahead of it.

| | Cable | Wi-Fi |
| --- | --- | --- |
| Video | through QuickTime's screen-capture device (tested on iOS 26.6) | iOS 27 or later (tested on 27.2) |
| Home, Lock, keys | CoreDevice HID | CoreDevice HID |
| Touch on iOS 27 or later | CoreDevice HID | CoreDevice HID |
| Touch below iOS 27 | an XCUITest runner on the phone | answers that it needs a cable |

Opening a Wi-Fi phone before iOS 27, or with a simcap that lacks `phonewifi`
in `-caps`, answers that streaming needs a cable. CoreDevice itself refuses
with 9001, "Remote control requires iOS 27.0".

- **Launch** is `/api/input {"action":"launch"}` through devicectl and takes
  under a second. A bundle the device lacks answers 404 `notInstalled`; any
  other failure is a 502 with devicectl's words.
- **Screenshot** is `/api/screenshot` through devicectl, about 0.5s over a
  cable.
- **Logs** come through `syslog_relay` (`GET /api/logcat` in
  `docs/agent-api.md`).
- **Install** is not wired: `/api/install` reads every upload as an APK.
- **Open URL** answers 501 (`docs/agent-api.md`).
- **Wi-Fi video** is a stream of our own, so it is the part most likely to
  break with an Xcode update. A locked phone looks like a still screen.
- Plugging a phone in can raise macOS's "what is this audio device" prompt
  once. "Other device" answers it.

### Sign simcap with a team

Streaming a phone over a cable asks for camera access (macOS treats the
screen-capture device as a camera). Signed ad hoc, simcap is a new program
to macOS after every rebuild, so it asks again and the phone streams nothing
while the prompt sits unanswered on the Mac.

```sh
make simcap TEAM=<team id>
```

signs simcap with that team's Apple Development identity under the
identifier `dev.bmcreations.loupe.simcap`, so the grant should survive a
rebuild. That has not been watched across one yet.

A stream waiting on the prompt says so on stderr at once and gives up after
8s, inside the server's 10s open, so the viewer's error names the prompt. A
denied grant fails the same way. Every simcap's stderr is echoed to the
server's, so `make agent-log` shows the wait as `simcap <id>: ...`.

### Touch below iOS 27

CoreDevice's digitizer answers `ok` for every touch on iOS 26.6 and none
land. Below iOS 27 on a cable, `tap`, `swipe` and `touch` go to a UI test
that runs on the phone and applies each one. Buttons and keys stay on
CoreDevice.

```sh
make xcrunner TEAM=<team id>
```

builds and signs it into `.build/xcrunner`. The team is a parameter, never
in the repo. The server finds it on each touch, so a rebuild needs no
restart.

- The first touch starts the runner. Until it answers, every touch is a 503
  `runnerStarting`, never an `ok`. It takes about 5s.
- A drag is recorded and lands when the finger lifts. A second finger is
  refused.
- A tap through the server takes about 1.7s.
- On Wi-Fi a touch answers that it needs a cable.

### Not yet

- Nothing has streamed a phone under the launchd server or to a real viewer.
- Over Wi-Fi, the decode and re-encode latency is unmeasured.
- iPhone Mirroring's window is another Wi-Fi route and was not taken.
