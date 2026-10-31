# Agent API

The `/api/*` routes an agent drives, and how claims and the human hold share
a device between agents and a person. Request and response shapes are in
`docs/api.schema.json`; the session wire protocol the viewers speak is in
`docs/protocol.md`. Command-line and MCP wrappers are in [cli.md](cli.md) and
[mcp.md](mcp.md).

## Conventions

- Every request needs the shared token (`Authorization: Bearer`, cookie or
  `?token=`).
- A route that touches a device takes `?target=<id>`. A process can drive
  a target while a person watches the same session.
- Coordinates are device pixels, as `/api/ui` reports them.
- An agent names itself with `X-Loupe-Agent: <name>` (64 printable bytes at
  most) and, to tell two sessions of one agent apart, `X-Loupe-Session: <id>`.
  Both show in the trace and on the viewer's banner. The viewer sends
  `X-Loupe-Viewer`; agents never should (see Claims and the human hold).
- A target that cannot do something answers 501. A target that is shut down
  answers 502 "open it in the viewer to boot it" to routes that need it
  running, except where a route says otherwise.
- Errors are JSON with `error` and, where a caller can branch on it, `code`.

Common refusals on routes that change a device:

| Status | `code` | Meaning |
|---|---|---|
| 409 | `humanHeld` | A person is touching the target. Has `retryAfterMs` and a `Retry-After` header. |
| 409 | `claimedBy` | Another agent holds a claim. The holder is in `claim`. |
| 409 | `handsOff` | The target is marked off limits. No retry time. |
| 409 | `interrupted` | A person sent an interrupt flag the agent has not read ([flags.md](flags.md)). |

## Routes

| Route | Purpose |
|---|---|
| `GET /api/ui` | View hierarchy |
| `GET /api/screenshot` | One PNG |
| `POST /api/input` | Tap, swipe, text, keys, buttons, launch, open URL, wait |
| `POST /api/sequence` | Several input steps in one request |
| `POST /api/drop` | Paste an image into a simulator text field |
| `POST /api/install` | Install an APK or split set |
| `POST /api/media` | Add a photo or video to the device library |
| `POST`, `DELETE /api/camera` | Feed the back camera an image |
| `POST /api/devices/boot`, `shutdown`, `reset`, `DELETE /api/devices` | Device lifecycle |
| `GET /api/logcat` | Device log |
| `GET /api/crashes` | Crash reports |
| `GET`, `POST /api/clipboard` | Device clipboard |
| `GET`, `POST /api/clipboard/shared` | Shared clipboard |
| `GET /api/handoff` | Link that moves a session to another browser |
| `GET`, `POST`, `DELETE /api/overlays` | Images and boxes over the video |
| `GET`, `POST /api/extensions` | Extension registry and proxy |
| `GET`, `POST`, `DELETE /api/claims` | Claims |
| `POST /api/devices/handsoff` | Hands-off marker |

## Read the screen

### `GET /api/ui?target=<id>`

Returns the view hierarchy (`uiautomator dump` on Android). Each node has a
`center`, so a caller taps a node without computing it.

| Param | Effect |
|---|---|
| `interactive=1` | Prunes layout containers. |
| `compose=1` | Names nodes after the composables that drew them. |

A dump takes about 2 seconds on an emulator. The tree is not cached.

`compose=1` works on a debuggable Android app only. Nodes inside a
`ComposeView` gain `composable` (`MessageRow`) and `source`
(`MessageList.kt:661`), and the response adds `composables`, the full
composable tree with bounds, including wrappers that have no node. A node is
named only when a composable's bounds equal its own, so empty full-screen
overlays and nodes clipped by another stay unnamed. The first call per app
version downloads the Compose inspector for that version from Google's Maven
into `~/Library/Caches/loupe/compose-inspector/<version>/`. A failure never
fails the request: the plain tree comes back with `composeError` saying why
(`app is not debuggable`). It combines with `interactive=1`.

### `GET /api/screenshot?target=<id>`

Returns one PNG of the framebuffer at the device's own resolution. It needs no
open session. A target with no screenshot support (synthetic) answers 501.

## Input

### `POST /api/input?target=<id>`

Body: `{"action": "...", ...}`. Field shapes are in `docs/api.schema.json`.

| Action | Fields | Notes |
|---|---|---|
| `tap` | `x`, `y` or `match` | See Tap by selector. |
| `swipe` | `x`, `y`, `x2`, `y2`, `ms` | |
| `text` | `text` | |
| `key` | `keycode` | |
| `button` | `button` | `back`, `home`, `lock` or `recents`. |
| `pinch`, `rotate`, `reclaim`, `hardwareKeyboard` | see schema | |
| `launch` | `package`, `activity?` | See Launch. |
| `openURL` | `url` | See Open a URL. |
| `move` | `x`, `y` | Touches nothing. |
| `wait` | `ms` or `match` | Touches nothing. |

An action a person's hold, a claim or hands-off refuses answers 409 as in
Conventions. `move`, `wait`, `hardwareKeyboard` and reads are never refused by
the human hold.

An Android `tap` or `swipe` the input dispatcher threw away answers 409
`code: "touchRejected"` with its reason, because `input` itself exits 0 either
way.

#### Cursor

`move` (`{"x": ..., "y": ...}`) sends the agent's cursor to a point on every
viewer, so a person sees where the agent is about to act. It is live only: not
in the event log or `/api/events`, it passes the human hold and leaves the
banner alone. Another agent's claim still refuses it. `loupe mcp`'s `tap` and
`swipe` send one first. After 2.5 seconds without input the cursor dims and
pulses where it last acted for 20 seconds more, then fades.

#### Tap by selector

`{"action":"tap","match":{"text":"Sign in"}}` taps a node instead of a point.

| `match` field | Meaning |
|---|---|
| `id` | Resource id on Android, accessibility identifier on iOS. A bare Android name (`send`) matches any package's `:id/send`. |
| `text` | The node's text. On a simulator a button's label is its `text`. |
| `desc` | The content description. |
| `contains` | `true` makes `text` and `desc` case-insensitive substring matches. Otherwise they match exactly. |

A node has to satisfy every field given. Other body fields:

| Field | Effect |
|---|---|
| `index` | Picks the nth match in tree order. |
| `waitMs` | Dumps again every 250ms, up to this many ms (at most 20000), until the node appears. Replaces a sleep-and-retry after a navigation. |

The server taps the node's `center`, moves the cursor there first, and answers
`{"ok":true,"matched":{...}}` with the node (no children). Nodes with no area
and nodes a simulator marks `occluded` never match. Matches with identical
bounds count once.

| Status | `code` | Meaning |
|---|---|---|
| 404 | `noMatch` | No node matched. |
| 409 | `ambiguous` | More than one matched. `candidates` lists them and nothing was touched. Add `index`. |
| 400 | | A body with both `match` and `x`/`y`. |

The lookup runs before arbitration, so a missing node is a 404 even while a
person holds the target. Each match tap costs a full dump.

#### Wait

`{"action":"wait","ms":1500}` pauses (at most 20000).
`{"action":"wait","match":{"text":"Inbox"}}` dumps until the node appears and
answers with it, for 10 seconds unless the match sets `waitMs`. It is not
arbitrated and not logged, so a held target does not refuse it. In a sequence
it holds the next step until a screen has loaded.

#### Launch

`{"action":"launch","package":"com.android.settings"}` opens an installed app.
`activity` names a specific one (qualified, or starting with `.`); otherwise
the launcher activity is used. On a simulator `package` is the bundle ID and
`activity` is a 400. An app already in front is a success.

| Status | `code` | Meaning |
|---|---|---|
| 400 | | Package or activity name fails the pattern check. Checked before arbitration, so it is a 400 even under a hold. |
| 404 | `notInstalled` | The package is not on the device. |
| 422 | `noLaunchActivity` | Installed, with no launcher activity. |
| 404 | `noSuchActivity` | The named activity does not exist. |
| 422 | `noCameraShim` | A camera image is staged and the app could not load the camera shim (see Camera). |
| 502 | | Any other failure. A shim that could not be built or signed has an error starting "camera shim:". |

The trace record has `package`, `tookMs`, `activity` when named, and `started`,
the component the device reports. For a trampoline that is not the one asked
for.

#### Open a URL

`{"action":"openURL","url":"https://example.com/x"}` hands a URL to whatever the
device registered for it: a deep link, a universal link or a web page.

- The URL needs a scheme and a host (`mailto:` has none), no whitespace or
  control bytes, and at most 2048 bytes. Otherwise 400, checked before
  arbitration.
- Nothing handles it: 422 `noHandler`.
- On a simulator, an http or https URL an installed app claims opens in that
  app, and `started` is its bundle ID (see "Universal links on a simulator" in
  `docs/ios.md`). Any other URL goes to `simctl openurl`.
- A physical iPhone answers 501. devicectl opens a URL only as the payload of
  launching a named app.
- A timeout waiting for the handler to draw still counts as success when the
  device named an activity. The trace carries `timedOut`.

The URL's path and query never reach the trace or an error, because a deep
link can carry a token. The trace has `scheme`, `host` and `urlLen`, and errors
name the URL as `scheme://host/…`.

### `POST /api/sequence?target=<id>`

Runs 1 to 50 `/api/input` bodies in order:

```json
{"steps":[
  {"action":"tap","match":{"text":"Email"}},
  {"action":"text","text":"a@b.co"},
  {"action":"tap","match":{"text":"Sign in"}},
  {"action":"wait","match":{"text":"Inbox"}}
]}
```

One round trip replaces one model turn per step, and the cursor moves from step
to step without a pause. Each step is validated, arbitrated, logged and traced
as if sent alone, so a person who takes the device back stops the sequence at
the next step. A coordinate `tap` or `swipe` is preceded by a `move`.

If every step answers 200, the response is `{"ok":true,"steps":[...]}`. The first
step that answers anything else stops the rest. The response then carries that
step's status, `"failed": <index>`, its `error` (prefixed `step N:`) and `code`
at the top level, where a single input would put them, and `steps` with each
step's `status` and `body` up to and including it.

A step that depends on an earlier step's screen should look for its node with
a `wait`, or with `waitMs` on its own `match`.

### `POST /api/drop?target=<id>&x=<px>&y=<px>`

Pastes the request body, an image, into the text field at x, y. iOS simulators
only. Answers 200 `{target, ok, tookMs}`. MCP tool `drop` takes a path on
disk. Timing and limits are under Clipboard in `docs/targets.md`.

| Status | `code` | Meaning |
|---|---|---|
| 400 | | Not PNG, JPEG, GIF, WebP or HEIC (a video included), or `x`/`y` missing or not integers. |
| 413 | | Over 50 MiB. |
| 422 | `noPaste` | No Paste appeared at the point. It is not a field that takes an image. Fall back to `/api/media`. |
| 422 | `pasteFailed` | Paste was tapped, then tried once more, and the field's value still did not change. A field that exposes no value is taken on trust. |
| 501 | `unsupported` | Not an iOS simulator. |
| 501 | `simcapOutdated` | The simcap helper predates image paste. Run `make simcap`. |

It is input: claims, hands-off and the human hold apply as to a tap, and
`X-Loupe-Viewer` makes it a person's. Viewers see the cursor move to x, y and
a `tap` event there. The image is never traced.

## Apps and files

### `POST /api/install?target=<id>`

Installs the request body as an APK. A `multipart/form-data` body installs a
base and its splits together:

```sh
curl -F apk=@base.apk -F apk=@split.apk "$URL/api/install?target=$ID"
```

Up to 64 file parts, any field name. **Splits are the normal case**: anything
built from an App Bundle is a base plus config splits, and the base alone is
refused with `INSTALL_FAILED_MISSING_SPLIT`.

| Param | Effect |
|---|---|
| `allowTest=1` | Allows `android:testOnly` APKs, which is what Android Studio's Run produces. Without it: `INSTALL_FAILED_TEST_ONLY`. |
| `launch=1` | Launches the installed package afterwards. |
| `package=` | Names the package when `aapt2` is not available. |

| Status | Meaning |
|---|---|
| 400 | A part is not a zip (the error names it). |
| 413 | Upload over 1 GiB. |
| 422 | The package manager refused. The response has adb's output and the `INSTALL_FAILED_*` code split out. |

Any `Failure [...]` line fails the install whatever adb's exit status, and so
does a zero exit with no `Success` line.

A successful install names its `package`, read from the APK. If `aapt2` is
missing, `?package=` stands in. If `aapt2` reads a different name than
`?package=`, the response carries `packageError` and nothing is launched.

`launch=1` arbitrates like `/api/input` (a held target is
`launch.code: "humanHeld"`). The outcome is a nested `launch` object (`ok`,
`activity`, `error`, `code`, `retryAfterMs`, `tookMs`). **The response status
stays the install's**: the APK is on the device whatever the launch says.

### `POST /api/media?target=<id>`

Adds the request body, a photo or a video, to the device's library so it shows
in Photos or Google Photos. Answers 200 `{target, ok, name, kind, bytes, tookMs}`.

- The type comes from the file's leading bytes: PNG, JPEG, GIF, WebP, HEIC,
  MP4 or MOV. Anything else is 400, before anything is pushed.
- Over 1 GiB is 413.
- Android and iOS simulators only (a simulator must be booted). Physical iOS
  devices and synthetic answer 501.
- Not arbitrated, because it changes no screen.

Dropping a file on the viewer page routes it by type (`web/drop.js`). A picture
goes into a simulator text field through `/api/drop`, not here.

### `POST /api/camera?target=<id>`, `DELETE /api/camera?target=<id>`

`POST` points the device's back camera at the request body, a PNG or JPEG sent
raw. `DELETE` gives the camera back. Both answer 200 `{target, ok, applies,
tookMs}`; `POST` adds the image's `width` and `height`.

Supported on AVDs, iOS simulators and physical iPhones. Physical Android
devices, `emulator-` serials and synthetic answer 501. Not arbitrated.

| Status | Meaning |
|---|---|
| 400 | Not a PNG or JPEG, or over 8192px a side. |
| 413 | Over 32 MiB. |

`applies` says when an app sees the image:

| Value | Meaning |
|---|---|
| `cameraOpen` | AVD booted with the camera flag. The next time an app opens the camera. A preview already showing keeps the old image until the app closes and reopens the camera. |
| `boot` | AVD running without the flag, or not running. Shut it down and boot it through loupe. |
| `live` | iOS: an app this server launched with the shim is running and follows swaps. |
| `launch` | iOS: no such app is running. The next `launch` picks the image up. |

**AVDs.** The emulator takes an image only as a boot flag, so loupe passes it
on any boot it starts while an image is staged. Turning the image on or off
therefore costs one cold boot (about 14 to 15 seconds against 3.4 from the
snapshot, measured on one AVD). A new image at the same path loads warm.
`DELETE` on an AVD running with the flag stages black. The image is fitted
whole and centered so the app sees all of it.

**iOS simulators.** The simulator has no camera, so loupe builds a shim on the
first `POST` (about 4 seconds) and `launch` (the `/api/input` action and the
MCP tool) starts the app with it, **relaunching it** if it is running without
the shim. An app that already has the shim is only brought to the front.

The shim supplies one back camera, 1920x1080 frames at 30 per second, QR
metadata detection and a preview layer. It differs from a phone in these
ways: `videoRotationAngle` is kept but not applied, QR bounds are in the
landscape frame's coordinates, metadata is QR only (asking for another type
throws), and there is no photo or movie output. `DELETE` makes apps with the
shim see black and later launches plain.

**Physical iPhones.** The same shim, for **development-signed** apps only:
dyld ignores the launch environment in an App Store, TestFlight or enterprise
build. The app must be signed by a team this Mac has an Apple Development
identity for, which holds when it was built and installed from this Mac. The
phone must be unlocked. Otherwise `launch` answers 422 `noCameraShim`. Each
devicectl call takes 1 to 5 seconds, and a launch has a 2 minute budget while
an image is staged.

An app that needs something the shim does not fake can read the staged image
itself in a debug build. `LOUPE_CAMERA_FILE` names the file (relative to the
app's home on a phone). Feed the scanner from it, skip `AVCaptureDevice`, and
re-read it when its modification date changes:

```swift
#if DEBUG
/// Frames from the image loupe stages, for a build loupe launched.
final class LoupeCameraFrames {
    let url: URL
    init?() {
        guard let path = ProcessInfo.processInfo.environment["LOUPE_CAMERA_FILE"] else { return nil }
        url = URL(fileURLWithPath: path, relativeTo: URL(fileURLWithPath: NSHomeDirectory(), isDirectory: true))
    }
    /// The current image, or nil when it is cleared: treat as a covered lens.
    func image() -> CGImage? {
        guard let src = CGImageSourceCreateWithURL(url as CFURL, nil) else { return nil }
        return CGImageSourceCreateImageAtIndex(src, 0, nil)
    }
}
#endif
```

This is a sketch and has not been exercised in an app.

## Device lifecycle

These routes take an `avd:` or `sim:` target that is available and not
`creating`. Anything else (a `device`, `iosDevice`, `synthetic`, `sim-new:` or
bare `emulator-NNNN` row, or an unknown id) is 422 `notSupported` with the
reason. They are arbitrated like `launch`, so `humanHeld`, `claimedBy` and
`handsOff` apply.

| Route | Effect | Answer |
|---|---|---|
| `POST /api/devices/boot?target=[&wait=]` | Boots without opening a stream. A running target answers 200 at once. `wait` is how long to hold the request: a duration (`90s`, `10m`) or seconds, default 4 minutes, at most 15 minutes. `wait=0` starts the boot in the background and answers at once. | 200 `TargetInfo` once up; 202 `BootStatus` for `wait=0`; 504 when `wait` ran out first |
| `GET /api/devices/boot?target=avd:<name>` | How an AVD's boot is going, without waking or arbitrating anything. | 200 `BootStatus` |
| `POST /api/devices/shutdown?target=` | Stops the target. Refused if it is already `bootsOnOpen`, unless an emulator process for the AVD is still booting; that stop saves no snapshot. | 200 `TargetInfo` |
| `DELETE /api/devices?target=` | Stops it if running, then deletes it. | 204 |
| `POST /api/devices/reset?target=avd:<name>` | Stops the AVD without saving, discards `loupe-warm` and wipes its data on the next boot. Works on a stopped AVD. A simulator is 422 `notSupported`. | 200 `TargetInfo` |

Other errors:

| Status | `code` | Meaning |
|---|---|---|
| 409 | `busy` | A shutdown or delete of that target is already running. |
| 409 | `recording` | A recording is in progress (response has its `id`). It is never ended as a side effect. |
| 400 | | `wait` is not a duration or is out of range. |
| 502 | | The boot, stop or delete failed. |
| 504 | | The boot outran `wait`; the emulator is still tracked (see below). |

`BootStatus` has `state` (`up`, `booting`, `stopped`), `processAlive` and `pid`
(an emulator process for that AVD, whoever started it), `serial`, `adbState`
(adb's word, such as `offline`, or `absent`), `bootCompleted`
(`sys.boot_completed`, read only while adb says `device` and the VM is not
paused), `elapsedMs` and `logPath`, the file the emulator's stdout and stderr go
to (`~/Library/Caches/loupe/emulator-<name>.log`). A target that is not an AVD
answers 422 `notSupported`.

A boot that runs past `wait` (or past the emulator's own 3-minute budget when
no `wait` bounds it) does not kill the emulator. loupe keeps tracking the
process: the 504 or 502 error names its pid, the log path and the log's last
lines, a later `boot` waits on the same process, and `POST
/api/devices/shutdown` stops it. A boot that leaves adb showing `offline` with
the process alive is a hung emulator, not a slow one; read `logPath`, then shut
it down. `wait` bounds only the HTTP call. After a server restart the emulator is found
by name, as for any AVD loupe did not start.

Viewers get `MsgEnded` before the session closes. An AVD booted this way is
paused and shut down by the idle clock like any other. With
`emulatorKeepState` on, shutdown saves the AVD's state first. A boot lifts the
reopen guard that a shutdown set, as `start=1` does.

## Logs and crashes

### `GET /api/logcat?target=<id>`

Returns the device log as plain text.

| Param | Effect |
|---|---|
| `lines` | Last N lines. Default 500. |
| `since` | Everything since a duration back from now (`5m`) or an RFC 3339 time, trimmed to `lines`. |
| `pid`, `package`, `foreground=1` | At most one of these narrows the log to a process. `foreground` uses the foreground app. |
| `follow=1` | Keeps the response open and flushes each read. |
| `format=ndjson` | Simulators only. One JSON object per entry (`application/x-ndjson`). |

The pid used and the package, when there is one, come back in `X-Loupe-Pid`
and `X-Loupe-Package`. A package with no process is 409. A follow fixes the pid
when it opens, so it keeps reading the old process after an app restart.

Per platform:

| Target | Source | Limits |
|---|---|---|
| Android | `logcat -v threadtime` | `format=ndjson` is 422. |
| iOS simulator | Unified log. `package` is a bundle ID. | `since` with `follow` is 422 `notSupported`. |
| Physical iPhone | The device's syslog relay, over USB or Wi-Fi | Text only. `format=ndjson`, `since`, `package` and `foreground` are 422 `notSupported`. A dump waits for the backlog to go quiet, up to 3 seconds. An unlisted phone is 502. |
| Synthetic | | 501 |

A shut-down AVD answers 502 "open it in the viewer to boot it".

### `GET /api/crashes?target=<id>`

Lists crash reports, newest first, on iOS simulators, physical iPhones and
Android.

| Param | Effect |
|---|---|
| `limit` | Last N reports. Default 20. |
| `since` | Reports since then. Takes what logcat's `since` does. |

Each row has `id`, `kind`, `time`, `process`, `bundleID` (empty for a daemon),
`version`, `build`, `pid` and `summary` (`EXC_CRASH (SIGABRT), Abort trap: 6`).

`GET /api/crashes/<id>?target=<id>` returns one report as `text/plain`. An id
that belongs to another target, or does not exist, is 404. On iOS the id is a
UUID.

| Platform | Reports | `kind` |
|---|---|---|
| iOS simulator | Host crash reports for that simulator. Works on a shut-down simulator. | `crash` |
| Physical iPhone | Copied off the device and cached under `~/Library/Caches/loupe/ios-crashes/<udid>`. Analytics, Jetsam and spindump files are skipped. | `crash` |
| Android | The device's dropbox: app and system crashes, native crashes and ANRs. | `crash`, `nativeCrash`, `anr` |

A report appears well after the crash, tens of seconds on a simulator.
SIGKILL leaves none. On Android, `time` is the entry's epoch time, `bundleID`
is the package, and `build` and `version` are its versionCode and versionName.
A call resumes a paused emulator. Dropbox lives under `/data`, so without
`emulatorKeepState` a boot from `loupe-warm` discards every report since it
was saved. Synthetic answers 501.

## Clipboard

### `GET /api/clipboard?target=<id>`, `POST /api/clipboard?target=<id>`

`GET` answers `{"target","text"}`. `POST` with `{"text": ..., "paste": true}`
sets the clipboard, and `paste` (optional) also puts the text into whatever has
focus. Text must be UTF-8 and at most 64 KiB: just over is 413, and a body more
than 1 KiB past the cap is 400.

| Target | Needs | Failure |
|---|---|---|
| Android | An open session (viewer, recording or other holder). | 409 with none. 502 if the device does not answer in 2 seconds. |
| iOS simulator | Booted. No session needed. | 502 if shut down. |
| Physical iPhone, synthetic | | 501 |

- A peer target's call goes to its owner, and the owner's refusal comes back
  unchanged.
- A set changes that target only, even with `sharedClipboard` on
  (`docs/targets.md`, Clipboard).
- A simulator's paste is typed, because iOS asks before an app reads what
  another process set.
- Neither claims nor the human hold gate this route, so an agent can set or
  paste while a person is driving.
- The text is never traced.

### `GET /api/clipboard/shared`, `POST /api/clipboard/shared`

The shared clipboard (`sharedClipboard`, `docs/targets.md`, Clipboard).

`GET ?after=<seq>&waitMs=<ms>` long-polls. It answers
`200 {"seq","id","text","origin","at"}` once a value with a `seq` past `after`
exists (at once if one already does), and 204 if none arrives within `waitMs`
(default 25000, at most 30000). `after` absent or 0 returns the current value.

| Field | Meaning |
|---|---|
| `seq` | Counts values on this server only. |
| `id` | `<instance>-<n>`. The value's identity across servers. |
| `origin` | `target:<id>`, `mac`, `participant` or `peer:<name>`. |
| `at` | When this server accepted the value, in its clock's Unix ms. |

`POST {"text", "id", "at"}` offers text and answers 204. `id` and `at` are
optional. The server stamps its own `at`, and every POST is stored with origin
`participant`. A value whose `id` or text matches the current one is dropped,
still with 204. Empty or non-UTF-8 text is 400, over 64 KiB is 413.

With `sharedClipboard` off, both answer 409 `code: "disabled"`, and a GET already
waiting gets that 409 too. A server without the service answers 404.

A hub bridges the shared clipboard to each of its peers; two servers that hold
each other as peers settle without looping.

## Viewers and extensions

### `GET /api/handoff?target=<id>&target=<id>`

Returns `{"url"}`: the web client's link for up to four targets, the first one
focused, on the certificate's host and with the token. The Mac app's Continue
in Browser opens it, copies it or shows it as a QR code. A server with no
address another device can open (no TLS certificate) answers 404, and a fifth
target is 400.

### `/api/overlays?target=<id>`

Puts an extension's output in front of everyone watching the target: an image
over the video (a design to compare against), labelled boxes (where it
differs), or both. loupe neither makes nor reads them. It keeps the latest set
per target and sends it to every viewer. The web client and the Mac app compare
by fade, swipe or difference.

`POST` body, in device pixels:

```json
{"id": "...", "source": "...", "label": "?", "image": "base64 PNG or JPEG ?",
 "screen": [w, h], "bounds": [x1, y1, x2, y2], "opacity": 1,
 "boxes": [{"bounds": [x1, y1, x2, y2], "label": "?", "kind": "?"}]}
```

Posting an id again replaces it. Limits: 8 overlays per target, 200 boxes each,
12 MiB per post. `GET` lists them, `GET /api/overlays/image?target=&id=&v=`
serves an image, and `DELETE ?id=` or `?source=` removes some (neither
removes all).

### `/api/extensions`

`POST {name, url, label?}` registers an extension that has something for
viewers to read. `url` must be http or https on `127.0.0.1`, `::1` or
`localhost`, otherwise 400. A registration lasts 90 seconds, so an extension
re-registers on a timer. `GET` lists them.

`/api/ext/{name}/...` passes through to the extension with the prefix, loupe's
`Authorization` header and the `token` query parameter removed, so a client
reads it with loupe's token alone. An extension that does not answer gets 502
and drops out of the list. [extensions.md](extensions.md) is the contract for
writing one.

## Claims and the human hold

### The human hold

A person at a viewer holds the target against agents. Any viewer input (a
finger down, a key, text, a scroll, or the viewer's own `/api/input`) holds it
while a finger is down and for `humanHoldMs` (5000 by default, in the config
file) after the last input.

While held, an agent's `tap`, `swipe`, `pinch`, `text`, `key`, `button`,
`rotate`, `reclaim`, `launch` or `openURL` answers 409 `code: "humanHeld"` with
`retryAfterMs` and a `Retry-After` header, and nothing reaches the device.
There is no override. Reads, `hardwareKeyboard` and `move` are never refused,
and with no session open there is no hold.

An interrupt flag holds the target until the person sends it, then refuses the
interrupted agent with `code: "interrupted"` until it reads the flag
([flags.md](flags.md)).

The shared token cannot tell callers apart, so they say who they are. The
viewer sends `X-Loupe-Viewer` with its viewer id from init; a live id makes the
request the person's, and anything else is an agent's.

Viewers show a banner: "claude is driving" while an agent's input landed in the
last 10 seconds, and "claude is waiting for you" while one was refused and no
action of its has settled since.

### Claims

The hold keeps agents off a device a person is touching. A claim keeps two
agent sessions off one device. It is a lease:

| Route | Effect |
|---|---|
| `POST /api/claims?target=` | Claims the target for the caller (`X-Loupe-Agent`, `X-Loupe-Session`). A body `{"agent","session","link"}` may say the same. `link` is an https URL of at most 512 bytes. |
| `DELETE /api/claims?target=` | Releases it. Someone else's needs `?force=1`, which is what the picker's Release sends. |
| `GET /api/claims` | Lists claims. |

A claim also keeps an emulator awake. The idle pause and idle shutdown skip a
claimed AVD (a claim restarts its idle clock, so the pause comes one
`emulatorPauseSec` after the claim lapses), and taking a claim resumes an
emulator that is already paused. Raw `adb install` or `adb shell` against a
claimed emulator therefore does not hang. The lease lapses 10 minutes after
the holder's last call to loupe, so a longer raw-adb job should keep calling
(`GET /api/claims` renews nothing; any `?target=` call does). `/api/targets`
reports `paused: true` on a paused emulator.

While a claim lasts, any other agent's input on the target answers 409
`code: "claimedBy"` with the holder in `claim`, before anything reaches the
device. Every call from the holder, `/api/ui` and `/api/screenshot` included,
renews the lease, and it lapses 10 minutes after the last call. Claims are
saved to `claims.json` beside the config file and survive a restart, unless
the lease ran out while the server was down. A renewal is saved at most once a
minute, so a restored lease can be up to a minute short. Claims are kept out of
`handsoff.json`, which holds only what the person marked.

### Hands off

`POST /api/devices/handsoff?target=&on=1|0` marks a target off limits to every
agent, a claim's holder included. Input answers 409 `code: "handsOff"` with no
retry time. The mark is saved to `handsoff.json` beside the config file and
survives a restart.

The shutdown, delete and install `?launch=1` routes go through the same check
as `/api/input`. Reads are never refused.

When more than one applies, an agent is told hands off first, then another
agent's claim, then the human hold. None of them refuses a person: a live
`X-Loupe-Viewer` id for any session is the picker acting for them.

### In the picker

`/api/targets` carries `claim`, `handsOff` and `drivenBy` (the agent whose
input went through in the last 10 seconds, with or without an open session).
The picker shows them under the row's detail (hands off outranks a claim, a
claim outranks driving) and draws the claim's `link` as Open session when it
is https.

## See also

- [mcp.md](mcp.md): `loupe mcp`, its tools, and pushing flags over channels.
- [cli.md](cli.md): the same tools as shell commands.
- [flags.md](flags.md): flags, watches and interrupts, and where they can be sent.
- [security.md](security.md): pairing a device with its own session, and revoking it.
- [recording.md](recording.md): recording a target to MP4.
