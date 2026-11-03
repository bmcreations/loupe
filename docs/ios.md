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
| Install | `/api/install` with a simulator build, as a zipped `.app` or an `.ipa` (`docs/agent-api.md`). |

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
- **Install** is `/api/install` with an `.ipa` or a zipped `.app`, through
  devicectl, about 8s for a 22 MB build (`docs/agent-api.md`).
- **Open URL** opens http and https in Safari, about 1.4s; other schemes
  are a 400, and a universal link does not reach its app
  (`docs/agent-api.md`).
- **Wi-Fi video** is a stream of our own, so it is the part most likely to
  break with an Xcode update. A locked phone looks like a still screen.
- Plugging a phone in can raise macOS's "what is this audio device" prompt
  once. "Other device" answers it.
- **`/api/ui`** has no tree for a phone. The phone's service for one,
  `com.apple.accessibility.axAuditDaemon.remoteAXService`, is refused on the
  Mac: remoted logs `Client lacks privilege to interact with service` for
  CoreDeviceService, the process every CoreDevice client goes through. Tried
  on 2026-11-03 (macOS 26B118, Xcode 27.1 beta, iOS 27.2 24B115) as a socket
  (`fileHandle(toServiceNamed:)`, which Xcode's DeviceKit uses to feed
  AccessibilityAudit's `AXAuditRemoteDevice`) and as a RemoteXPC connection
  in both modes. Each answered CoreDevice error 4000, POSIX 83. The same
  calls opened `com.apple.instruments.dtservicehub` and
  `com.apple.coredevice.deviceinfo`, so the refusal belongs to this service,
  not the transport. `axAuditDaemon.remoteserver.shim.remote`, the service
  pymobiledevice3's accessibility client uses, is refused the same way. The
  phone does not list `remoteAXServiceLarger`. Reaching either would take a
  tunnel of our own, outside remoted. The probe is `simcap <coredevice id>
  -phone-ax-probe <service>` on simcap's `feat/ios-remote-ax` branch.

#### A tunnel of our own was tried and is blocked too

The follow-up asked whether a tunnel we open ourselves, outside remoted,
reaches the AX service where the Mac's CoreDevice client could not. It does
not over Wi-Fi, the only transport reachable without plugging the phone in.
Tried on 2026-11-03 (macOS 26B118, iOS 27.2 24B115) against bmc-iphone at
192.168.1.108, using the open-source Rust crate `idevice-rs`
(github jkcoxson/idevice, MIT), the same crate Device MCP by Proxyman
vendors.

Their path, read from the crate and from the competitor's embedded strings,
needs no root and no Mac entitlement: usbmuxd hands any user the pair record
(`ReadPairRecord` over `/var/run/usbmuxd`, no root), lockdown opens the
CoreDeviceProxy service, a CDTunnel handshake brings up a tunnel, and a
userspace TCP stack (not a utun device) carries RSD and DTX over it. Every
step there is root-free. The catch is the transport: it is a cable path.

Over Wi-Fi lockdown the phone grants the service but never answers the
handshake behind it. `StartService` for
`com.apple.internal.devicecompute.CoreDeviceProxy` returns a port with
`EnableServiceSSL`, the TLS session comes up on that port, the client sends
the `CDTunnel` handshake request, and the phone sends nothing back. The same
silence follows `com.apple.dt.remotepairingdeviced.lockdown`: the port and
TLS come up, the client sends `attemptPairVerify`, and the reply never
arrives. Two independent clients (idevice-rs, and pymobiledevice3 driving
the same handshake) both completed TLS and both hung, so the silence is the
phone's, not one library's bug.

The silence is by design: these are USB-transport services. pymobiledevice3
documents the promptless pair route as "reached over the USB lockdown
transport" and the no-root tunnel as "reaching RSD over usbmux instead of
the NCM link" (`remote/tunnel_service.py`, `remote/utils.py`). So the
root-free path Device MCP uses is not a Wi-Fi bypass of the Mac's refusal;
it is the same path moved onto a cable, where usbmuxd carries the lockdown
service and CoreDeviceProxy answers. Without the cable we cannot run it.

One Wi-Fi route stays untested: fresh RemotePairing over `_remotepairing._tcp`
(bmc-iphone.local:49152), which shows a six-digit code on the phone and
registers a new trusted host. It is a different mechanism from the pair-verify
above and might complete where verify does not, but it writes a persisted
trust record to the device, so it needs an explicit go-ahead before it runs.

No root was needed for anything tried, and nothing tried needed a root-only
record: usbmuxd returned both phones' pair records to an ordinary user, and
the userspace TCP stack needs no utun device. `/var/db/lockdown` is
`Operation not permitted` to us, but we never had to read it; the Mac's own
RemotePairing record there is what makes a cable run promptless, and reading
it is not part of the no-root path. The cable path now ships as `simcap-bridge`
in the simcap repo (`bridge/`), a Rust binary that pins idevice-rs by git rev;
its MIT notice is in that repo's `THIRD_PARTY_NOTICES.md`. The original probe
remains as scratch on this machine (`~/dev/idevice-spike`). Opening our Wi-Fi
tunnel did
not disturb CoreDevice: the utun count and `devicectl list devices` were
unchanged before and after, because the tunnel never reached the stage that
creates an interface.

On a cable the tunnel comes up and the phone refuses the service itself.
Run on bmc-iphone (iOS 27.2, 24B115) through usbmuxd, with the pair record
read from file because idevice-rs's own `get_pair_record` hangs: CDTunnel up
in 69ms, RSD in 88ms with 85 services, no root. RSD lists
`com.apple.accessibility.axAuditDaemon.remoteAXService` with entitlement
`AppleInternal`, and `remoteserver.shim.remote` with
`com.apple.mobile.lockdown.remote.trusted`. `remoteAXServiceLarger`, the
service Device MCP names, is not listed. RSD flags it as RemoteXPC, but the service speaks DTX on the raw stream, the
way `dtservicehub` does. RSD's `xpc` flag is not the wire protocol: before
recording that the phone refuses a service, open it the way the nearest
working client does, with a control that speaks the same protocol. A RemoteXPC handshake makes the phone close the
channel. Over DTX it stays open: after `_notifyOfPublishedCapabilities:`
the phone sends its own capabilities and a `hostAPIVersion` request, and
`deviceAPIVersion` on the root channel answers `26`. The server object is
`AXAuditDevicesAppRemoteServer`. Other root-channel selectors that Xcode's
AccessibilityAudit framework names (`deviceCapabilities`,
`deviceInspectorSupportedEventTypes`, `deviceFetchSpecialElement:`) answer
"does not respond to the selector", so the inspector and tree calls sit
behind a channel or handshake step not found yet. The entitlement field does
not block a client on our own tunnel. The Mac-side refusal through remoted
still applies to CoreDevice clients.

The tree comes from `clientNeedsAccessibility:` (argument `true`) on the
root channel. Xcode 27.1's `AXAuditRemoteDevice` uses the same call, and
`deviceAPIVersion` is the only other root call it needs. The phone then
pushes `processDataFromRemoteDevice:` messages without being asked: an
NSKeyedArchive wrapping NSData, which holds an `AXPTranslatorResponse` with a
`treeDump`. The first full dump from bmc-iphone was 142 KB: 1592 archive
objects, with element frames and centers in points on a 402x874 screen
(1206x2622 pixels at 3x), roles such as `Static Text` and `Image`, and labels
from the screen, here the lock screen clock and a Live Activity. The phone
re-sends the dump on its own after a change.

#### The tree is decoded without Apple's framework

The dump is the same shape of data simcap reads for simulators, so the first
plan was to hand it back to the system `AccessibilityPlatformTranslation`
framework and read the rebuilt elements, the way the simulator path does.
That was tried offline on 2026-11-03 against the captured dumps and does not
work. The framework rebuilds the element tree, but every getter on a rebuilt
element (`accessibilityRole`, `accessibilityLabel`, children) returns empty.
The elements carry `_isRemoteElement`, which routes every read through
HIServices `_AXCurrentRequestCanAccessRemoteDeviceContent`. That reads a
process-global flag the AX runtime sets only while it is dispatching a request
for a registered remote-device host client, the role Xcode's
`AXAuditRemoteDevice` and VoiceOver play. A plain signed helper does not
satisfy it, and simcap's entitlements carry only camera. The simulator path is
unaffected because simulator elements are local, not remote, so that gate is
never consulted.

So the phone tree is decoded directly from the archive, with no Apple
accessibility framework and no entitlement in play. The archive carries
everything the `ui` output needs as plain data. Its layout: the outer
NSKeyedArchive's root is an NSData whose bytes are a second archive, whose root
is the `AXPTranslatorResponse`; `resultData.treeDump` is an array of
per-element responses; the entries with `associatedRequestType == 5` each
describe one element. An element's key is its
`associatedTranslationObject.objectID`; its attributes are a dictionary keyed
by integer `AXPAttributeType` codes, of which `ui` uses `78` (the role word
and traits), `33` (label or value, sometimes an attributed string with the
text under `NS.string`), `48` (the frame, an `NSValue` rect in points), and
`8`/`9` (the ordered child object IDs). The role word (`Button`, `Static
Text`, `Image`, `Header`) maps to the matching macOS `AX` role constant so the
rest of loupe's view-hierarchy handling is unchanged. The phone sends two
dumps: an InitialDump with geometry and labels but no roles, then an
AdditionalData dump that adds the roles, so the decoder merges dumps by object
ID. The decoder and its tests live in loupe `internal/target/iosdeviceax.go`.

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
- Over Wi-Fi, the decode and re-encode adds about 8ms at the median over the
  HEVC passthrough, measured on one iOS 27.2 phone over one 20s run. From an
  access unit's arrival in simcap to a decoded picture in LoupeClient's
  VideoToolbox decoder on the same Mac, HEVC took p50 5.4ms and p90 6.7ms
  (n=238), and H.264 took p50 about 13.4ms: 5.0ms from arrival to the start of
  the re-encode, and 8.4ms (p90 10.2ms, n=187) from there to the decoded
  picture. Neither number includes the Wi-Fi leg from the phone, which both
  share, or the display. The H.264 track was 496x1080, the HEVC track the
  phone's own 1216x2656.

  Method: run a spare loupe with `SIMCAP_LATENCY=1`, so simcap logs t0 as wall
  time and, every 5s, the arrival-to-encode wait. An HEVC frame's PTS is its
  arrival after t0, and an H.264 frame's PTS is the start of its encode. Run
  two `nativeprobe -t0wall <us>` at once on the same session, one with
  `&decode=h265`; each turns PTS into wall time and reports PTS to decoded,
  skipping the first 60 pictures. The H.264 figure is the probe's plus
  simcap's wait.
- iPhone Mirroring's window is another Wi-Fi route and was not taken.
