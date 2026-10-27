# Agent API

The `/api/*` endpoints agents drive, claims and the human hold, the MCP
server, and recording. `docs/api.schema.json` is the request and response
schema; `docs/protocol.md` is the session wire protocol the viewers speak.

### Agent API

Five endpoints, all taking `?target=`, so a process can drive a device while a
human watches the same session.

- `GET /api/ui` returns the view hierarchy from `uiautomator dump`.
  `?interactive=1` prunes layout containers; on a Compose screen that was 6202
  bytes down to 3620. Each node carries `center`, precomputed, because it is
  the one derivation everything needs and leaving it to the caller invites
  rounding disagreements that land a tap on a neighbour.
- `GET /api/ui?compose=1` names nodes after the composables that drew them.
  It only works on a debuggable Android app: loupe attaches a JVMTI agent
  (`internal/composetree`) to the foreground app with `am attach-agent` and
  hosts the Compose inspector that ships inside the app's own `ui-android`
  version, downloaded once from Google's Maven into
  `~/Library/Caches/loupe/compose-inspector/<version>/`. Nodes inside a
  `ComposeView` gain `composable` (`MessageRow`) and `source`
  (`MessageList.kt:661`). The response adds `composables`, the full
  composable tree with bounds, including the wrappers that have no node.
  A node is named only when a composable's bounds equal its own. Each
  node anchors on a composable that carries semantics (`semantics` in
  `composables`) and takes the outermost name between that anchor and the
  one above it. Where uiautomator stacks nodes at one rectangle, a node
  anchors on the lowest such composable that encloses, in the composition,
  what its children anchored on, so a sibling branch at the same rectangle
  (a bottom bar's `AnimatedContent`) cannot take it. On acmechat's chat
  screen the full-screen stack reads three unnamed host Views, then
  `ProvidePermissionChecker`, `SharedTransitionLayout`, `ChatFlowScreen`,
  `FlowConversationScreen`, `OverlayBars`. Empty full-screen overlays and
  nodes clipped by another (their bounds shrink) stay unnamed.
  A failure never fails the request: the plain tree comes back with
  `composeError` saying why (`app is not debuggable`, a target with no
  inspector). It combines with `?interactive=1`. On emulator-5554 with
  acmechat the plain call took 2.27s, the first `compose=1` after a cold
  start 3.82s with the inspector already cached, and later ones about
  2.45s, with the body growing from 13 KB to 63 KB. The first download of
  an inspector version adds its fetch from Maven; that wasn't timed.
- `POST /api/input` takes `{"action":"tap|swipe|text|key", ...}`.
  `move` (`{"x": ..., "y": ...}`) touches nothing: it sends the agent's
  cursor there on every viewer, as a `MsgCursor` (`docs/protocol.md`), so
  a person watching sees where the agent is about to act. It is live only,
  kept out of the event log and `/api/events`, passes the human hold and
  leaves the banner alone, but another agent's claim still refuses it.
  `loupe mcp`'s `tap` and `swipe` send one before they touch.
  On Android a tap or swipe the input dispatcher threw away answers 409
  `code: "touchRejected"` with its reason, since `input` exits 0 either way
  (trap 50).

  `launch` (`{"package": ..., "activity": ...}`) opens an installed app.
  On Android it is `cmd package resolve-activity` for the launcher
  activity, or the named one (qualified or starting with `.`), then
  `am start -W -n`. Not `monkey -p <pkg> -c LAUNCHER 1`: on emulator-5554
  it exited 252 with "No activities found to run" for a package that was
  not installed and for one with no launcher activity alike, and names
  nothing when it works. `pm path` tells those two apart, and they answer
  404 `notInstalled` and 422 `noLaunchActivity`; a named activity that
  does not exist is 404 `noSuchActivity`. `am` exits 1 on that, and older
  devices exit 0 after printing it, so an `Error:` line fails the launch
  whatever the status. An app already in front is a success. The package
  and activity reach the device shell only after a pattern check and
  `shellQuote`. The check runs before arbitration, so a bad name is a 400
  even while a person holds the target, not a 409 no retry would fix. On a simulator `package` is the bundle ID and goes to
  `xcrun simctl launch`; an activity there is a 400. Through a spare
  server on emulator-5554: Settings from the Home Screen in 0.34s, the
  mask sample cold in 1.5s and in front already in 0.10s, and each
  failure above with its code. Each launch is traced as `kind:"agent"`,
  `action:"launch"`, with `package`, `tookMs`, `activity` when named and
  `started`, the component am reports, which for a trampoline is not the
  one asked for. Through a spare server on `loupe-probe`, Settings came
  to the front from the Home Screen in 0.16s and Safari in 0.44s. simctl's
  error for a missing bundle names no cause, so a failed launch asks
  `simctl get_app_container`, which exits 2 for an app that is not there,
  and answers 404 `notInstalled` as Android does. Any other exit leaves
  the launch's own error, a 502.

  `openURL` (`{"url": ...}`) hands a URL to whatever the device has
  registered for it: a deep link, a universal link or an https page. On
  Android it is `am start -W -a android.intent.action.VIEW -d`, the URL
  through `shellQuote`; a URL nothing resolves prints "unable to resolve
  Intent" and exits 1, which answers 422 `noHandler`. On a simulator it is
  `simctl openurl`, which exits 115 (LSApplicationWorkspaceErrorDomain)
  for the same case, also 422 `noHandler`. A physical iPhone answers 501:
  devicectl opens a URL only as the payload of launching a named app.
  `CheckURL` runs before arbitration, as `launch`'s name check does: a
  scheme is required, no host (`mailto:` has none), no whitespace or
  control bytes, at most 2048 bytes, so a bad URL is a 400 under a hold.
  **The URL's path and query never reach the trace or an error**, since a
  deep link carries its token there: the record has `scheme`, `host` and
  `urlLen`, an error names the URL as `scheme://host/…`, and on Android
  `started`, the activity am reports. Through a spare server, on Pixel_10
  (emulator-5556) an https URL opened Chrome in 0.10-0.20s and a scheme
  nothing handles was 422 in 26ms; on `loupe-probe` an https URL opened
  Safari in 0.48-0.54s and the unhandled scheme was 422 in 123ms.

  **An `am start -W` that times out still opened the URL** when it names
  an activity: am prints `Status: timeout` once its wait for the handler
  to draw runs out, and the intent was delivered all the same. That is a
  success, traced with `timedOut`. On `api34_emoji_check` (API 34, Chrome
  the only browser), two of three https opens first answered 502 after
  25.0s and 15.0s that way. After the fix the same three answered 200 in
  0.69-1.95s with `started` naming a Chrome activity, none timing out, so
  the `timedOut` path is covered by unit tests only, and the unhandled
  scheme was 422 in 0.18-0.37s. A `token=secret` query on every URL was in
  neither the trace nor the server log.

**A person at a viewer holds the target against agents.** Any viewer input
-- a finger down, a key, text, a scroll, or the viewer's own `/api/input` --
holds it while a finger is down and for `humanHoldMs` (5000 by default, in
the config file) after the last input. While held, an agent's `tap`,
`swipe`, `pinch`, `text`, `key`, `button`, `rotate`, `reclaim`, `launch` or
`openURL` answers 409
`code: "humanHeld"` with `retryAfterMs` and a `Retry-After` header, and
nothing reaches the device. There is no override: an agent that could
force its way past the person would put the fight back. Reads,
`hardwareKeyboard` and `move` are never refused, and with no session open there is no
hold at all.

The shared token cannot tell callers apart, so they say who they are. The
viewer sends `X-Loupe-Viewer` with init's viewer id; a live id makes the
request the person's, and anything else is an agent's. An agent may send
`X-Loupe-Agent: <name>` (64 printable bytes at most), which names it in the
trace and on the banner. The rules are in `internal/session/arbiter.go`,
a clock-driven state machine with no I/O.

Viewers see it as `MsgControl` (0x08, JSON `{held, holder, agent,
agentName}`), sent on every change and after init: `web/control.js` shows
"claude is driving" while an agent's input landed in the last 10s, and
"claude is waiting for you" while one was refused and no action of its
has settled since. A handover is one change from waiting to driving; going
idle in between made the banner vanish for a moment. Each refusal is a
`kind:"agent"` record with `refused`, `retryAfterMs` and `heldBy`; every
`/api/input` record carries `caller` and `viewer` or `agent`; and each hold
or banner change is one `kind:"control"` record, never one per touch.

**Agents claim a device, and the person can mark one hands off.** The human
hold keeps agents off a device a person is touching; it says nothing about two
Claude sessions on one device, and it ends with the session. `claims.go` covers
both. An agent sends `POST /api/claims?target=` with `X-Loupe-Agent: <name>`
and, to tell two sessions of one agent apart, `X-Loupe-Session: <id>`; a body
`{"agent","session","link"}` may say the same, and `link` must be an https URL
of at most 512 bytes. While the claim lasts, any other agent's input on that
target answers 409 `code: "claimedBy"` with the holder in `claim`, before
anything reaches the device. It is a lease: every call from the holder,
`/api/ui` and `/api/screenshot` included, renews it, and 10 minutes after its
last call it lapses (`claimLease`). `DELETE /api/claims?target=` releases it;
someone else's needs `?force=1`, which is what the picker's Release sends.
`GET /api/claims` lists them. Claims are in memory, so a restart drops them.

Hands off is `POST /api/devices/handsoff?target=&on=1|0`, the picker's swipe
action. It refuses every agent, a claim's holder included, with 409
`code: "handsOff"` and no retry time. It is saved to `handsoff.json` beside the
config file, because every `make build` restarts the server. The order is
hands off, then another agent's claim, then the human hold, so an agent is told
the reason that will last longest. Neither refuses a person: a live viewer id
in `X-Loupe-Viewer` for any session, not only this target's, is the picker
acting for them. The shutdown, delete and install `?launch=1` routes go
through the same check as `/api/input`. Reads are never refused.

`/api/targets` carries `claim`, `handsOff` and `drivenBy`, the agent whose
input went through in the last 10s, recorded with or without an open session,
which the arbiter's banner cannot see. The picker shows them as a second line
under the row's detail (hands off outranks a claim, a claim outranks driving)
and draws the claim's `link` as Open session beside the row. There is no URL
known to open a Claude session from Safari on a phone, so the link is
whatever the agent sent, and drawn only when it is https. Each claim, release
and lapse is traced as `kind:"agent"`, `action:"claim"`, `"release"` or
`"claimExpired"` (traced when the lapse is next read, not on a timer), a refusal carries `refused: "claimedBy"` or `"handsOff"`, and
the client reports `picker-handsoff` and `picker-release` with `target`, `on`
and `outcome`.
- `GET /api/screenshot` returns one PNG of the framebuffer, at the device's own
  resolution and with no session open. 266ms on emulator-5554 (1080x2424),
  444ms on an Acme One (1200x2670). `Screenshotter` is optional the way
  `Inspector` is, so synthetic answers 501.

  `adb exec-out`, not `adb shell`: shell runs its output through a pty on older
  transports, which rewrites 0x0a and corrupts every PNG. screencap also writes
  its errors to stdout and exits zero on some devices, so the PNG signature is
  checked rather than trusted.

  Reading a still off the video path instead would answer a different question.
  A canvas frame has been through `MaxSize` and through a lossy encoder, and on
  a stalled link it is whatever was painted last rather than what is on the
  device now. The web client's button does fall back to the canvas, but only
  for a target with no adb behind it.
- `POST /api/install` installs the request body as an APK with
  `adb install -r`, or a `multipart/form-data` body as a base and its splits
  with `adb install-multiple -r` (`curl -F apk=@base.apk -F apk=@split.apk`,
  up to 64 file parts, any field name). **Splits are the normal case**, not
  an edge: anything built from an App Bundle arrives as a base plus config
  splits, and the base alone is refused with `INSTALL_FAILED_MISSING_SPLIT`.
  `com.google.android.contactkeys` pulled off emulator-5554 was a base and
  three splits (ABI, language, density), 5.6MB, and went back in 533ms as
  one multipart request. Each file is checked for a zip signature (400 naming
  the part), the whole upload is capped at 1 GiB (413, before staging when
  `Content-Length` says so), and everything is staged in one temp directory
  under index names, never the caller's filenames, because adb takes paths
  and not stdin. A refusal is 422 with adb's
  output and the `INSTALL_FAILED_*` code split out, since the code is what a
  caller branches on. **The exit status is not the verdict.** Older adb exits
  0 after printing `Failure [...]`, so any Failure line fails the install, and
  so does a zero exit with no `Success` line. adb 35 exits 1 and prints the
  Failure after `adb: failed to install <path>: `, which the same pattern
  reads.

  `?allowTest=1` adds `-t`. Android Studio's Run marks its APK
  `android:testOnly`, and without `-t` the package manager refuses it with
  `INSTALL_FAILED_TEST_ONLY`: both `dev.bmcreations.mask.sample` builds on
  emulator-5554 did. With it, a 9.2MB reinstall took 128ms through the
  server. Each install is traced as `kind:"agent"`, `action:"install"`, with
  `bytes`, `apks`, `tookMs`, `outcome` (`ok`, `refused`, `tooLarge`, `error`),
  `code` and `package`.

  **A successful install names its package**, read with `aapt2 dump
  packagename` from the newest SDK build-tools (found as the emulator is,
  since launchd's PATH lacks the SDK). A config split carries its base's
  name -- all four contactkeys APKs read `com.google.android.contactkeys`
  -- so the first part answers for the set. `?package=` stands in when
  aapt2 is missing, and when aapt2 reads a different name the response
  carries `packageError` and nothing is launched, since that would open
  some other app.

  `?launch=1` then runs the `launch` action on that package, arbitrated
  like `/api/input` (a held target is `launch.code: "humanHeld"`). Its
  outcome is a nested `launch` object (`ok`, `activity`, `error`, `code`,
  `retryAfterMs`, `tookMs`), and **the status stays the install's**: the
  APK is on the device whatever the launch says, and a 409 or 502 would
  read as the install failing. The 9.2MB mask sample went in and came to
  the front in 284ms plus 1.5s through a spare server on emulator-5554.
- `POST /api/media?target=<id>` adds the request body, a photo or a video,
  to the device's library, so it shows in Photos or Google Photos, and
  answers 200 `{target, ok, name, kind, bytes, tookMs}`. Dropping a file into
  an app is not possible: nothing carries a drag through adb or HID. The type
  comes from the file's leading bytes (PNG, JPEG, GIF, WebP, HEIC, MP4, MOV),
  never the name; anything else is 400 before it is pushed. 1 GiB cap (413).
  Not arbitrated, as `/api/install` is not: it changes no screen. On Android
  it is `adb push` to `/sdcard/Pictures` or `/sdcard/Movies` and a
  `MEDIA_SCANNER_SCAN_FILE` broadcast. On API 34 (`loupe_headless`) the push
  alone was indexed, since shared storage is FUSE; on API 29 (`loupe_api29`)
  only the broadcast indexed it, and `content call --method scan_file` threw
  a NullPointerException there, so the broadcast is sent on every level. A
  PNG and a video showed in Google Photos on API 34. On a simulator it is
  `simctl addmedia`, which needs the simulator booted; a PNG and a 2s video
  showed in Photos. Physical iOS devices and synthetic answer 501. Traced as
  `kind:"agent"`, `action:"media"`, with `ext`, `bytes`, `tookMs` and
  `outcome`; the file's name is not. In the viewer, dropping a file on the
  page outlines the stage and routes it by type (`web/drop.js`). A drop
  dispatched in the built-in browser against a throwaway simulator lit the
  outline and answered "Added to Photos" in 914ms; a real drag from Finder
  has not been tried.
- `POST /api/devices/shutdown?target=<id>` stops an AVD or simulator and
  answers 200 with its `TargetInfo`; `DELETE /api/devices?target=<id>` stops it
  if running, deletes it and answers 204. The target must be an `avd:` or
  `sim:` id that is not `creating` and is available, and for shutdown not
  already `bootsOnOpen`; anything else is 422 `notSupported` with the reason
  (a `device`, `iosDevice`, `synthetic`, `sim-new:` or bare `emulator-NNNN`
  row, or an unknown id). 409 `busy` is a second request while a shutdown or
  delete of that target runs, 409 `recording` (with the recording's `id`) a
  recording in progress, which is never ended as a side effect, and 409
  `humanHeld` a person holding the target, arbitrated as `launch` is. A
  failed stop or delete is 502 and lifts the reopen guard again. Each is
  traced as `kind:"agent"`, `action:"shutdown"` or `"delete"`, with
  `platform`, `tookMs`, `outcome` (`ok`, `error` or `refused`) and `code` on
  a refusal. Viewers are sent `MsgEnded` before the session closes.
  With `emulatorKeepState` on, an AVD's shutdown saves its state first and
  the trace carries `saved`.
- `POST /api/devices/boot?target=<id>` boots an AVD or simulator without
  opening a stream, and answers 200 with its `TargetInfo` once it is up; a
  target already running answers 200 at once. It lifts a `shutDown` reopen
  guard as `start=1` does, is refused 409 `busy` while a shutdown or delete
  of that target runs, and is arbitrated as `launch` is. The same 422
  `notSupported` rows as shutdown apply. A failed boot is 502. Nothing holds
  the emulator afterwards, so an AVD booted this way is paused and shut down
  by the idle clock like any other. Traced as `kind:"agent"`,
  `action:"boot"`, with `platform`, `tookMs`, `outcome` and `wasRunning`.
  Through a spare server: `loupe_keep` from `loupe-warm` in 2.2s, and a new
  throwaway simulator (iPhone 17) in 33.6s on its first boot, then 0.4s and
  `wasRunning` true for a second call. A second call on the running
  `loupe_keep` took 4.1s, though the trace put `Boot` at 29ms: the handler
  reads the target's row before and after through `Manager.Info`, which
  builds the whole `/api/targets` list, and that took about 2s at a load
  average over 60.
- `POST /api/devices/reset?target=avd:<name>` is Reset above: it stops the
  AVD without saving, discards `loupe-warm` and wipes its data on the next
  boot. It works on a stopped AVD, answers 200 with its `TargetInfo`, and is
  refused and arbitrated as shutdown is. A simulator is 422 `notSupported`.
- `GET /api/logcat` returns `logcat -v threadtime` as plain text: the last
  `?lines=` (default 500), or everything since `?since=`, which takes a
  duration back from now (`5m`) or an RFC 3339 time and is trimmed to
  `lines` here, since `-T` has no limit of its own. At most one of `?pid=`,
  `?package=` and `?foreground=1` narrows it to a process; the last goes
  through `ForegroundApp`, the same read as the Tools sheet's app card. The
  pid it used and the package, when there is one, come back in
  `X-Loupe-Pid` and `X-Loupe-Package`. A package with no process is 409.
  `?follow=1` keeps it open and flushes each read: a `log -t` line reached
  curl 62ms after it was written on emulator-5554, and hanging up killed the
  adb child. The pid is fixed when the log opens, so a follow outlives an app
  restart by reading the process that died.

  A package reaches the device's shell only through `pidof`, and only after
  `ValidPackageName` and `shellQuote`. Everything else passed to logcat is
  digits formatted here.

  `Installer` and `Logcatter` are optional the way `Inspector` is: an adb
  device has both, a running AVD delegates to its `ADBTarget`, a shut-down
  AVD answers "open it in the viewer to boot it" (502, as `/api/screenshot`
  does), simulators and physical iOS devices have their own (below), and
  synthetic answers 501.

  **A simulator's log is its unified log**, `simctl spawn <udid> log show`
  for a dump and `log stream` for a follow, `--style compact` as text and
  `?format=ndjson` as `--style ndjson`, one JSON object an entry with the
  unified log's own fields (`application/x-ndjson`). A package is a bundle ID
  (`ValidBundleID`, hyphens allowed), resolved to a pid from `launchctl list`
  inside the simulator; one that is listed with no pid is 409, as on
  Android. A lines-only dump tries `--last` 1m, 10m and 1h in turn and stops
  at the first that holds enough lines, because `log show` with no window
  reads the whole store: on loupe-probe 10m was 545k lines in 10.1s. A follow
  has no backlog, so `since` with `follow` is 422 `notSupported` rather than
  ignored. `?format=ndjson` on an adb target is 422 too.

  **A physical iPhone's log is lockdownd's `com.apple.syslog_relay`**,
  reached through usbmuxd over a cable or Wi-Fi (`internal/usbmux/lockdown.go`):
  the pair record from usbmuxd, held in memory only, a lockdown session
  upgraded to TLS with its certificate, then `StartService`. The relay sends
  each line NUL-terminated and vis-encodes every byte that is not printable
  ASCII (`\M-b\M^@\M-&` for an ellipsis), which `unvis` undoes. On connect
  it sends a backlog, 64KB in under a second on bmc-iphone, so a dump reads
  until 500ms of quiet, at most 3s, and keeps the last `lines`; through a
  spare server on bmc-iphone (iOS 27.2, Wi-Fi) `lines=5` answered in 3.6s,
  the cap, since a busy phone is never quiet. `?pid=` filters on the `[pid]`
  in each line, and a continuation line goes with the message above it.
  It is text only, with no history to query by time, and nothing maps a
  bundle ID to a pid here, so `format=ndjson`, `since`, `package` and
  `foreground` are 422 `notSupported`. A phone usbmuxd does not list is 502.
  Brandon's iPhone (iOS 26.6) was listed by devicectl on Wi-Fi and not by
  usbmuxd, so it has not been read.

- `GET /api/crashes?target=sim:<udid>` lists a simulator's crash reports,
  newest first: the last `?limit=` (20 by default) or those since `?since=`,
  which takes what logcat's does. Each row is `id` (the report's
  `incident_id`), `kind` (`crash`), `time`, `process`, `bundleID` (empty for
  a daemon), `version`, `build`, `pid` and `summary`, the exception and
  termination (`EXC_CRASH (SIGABRT), Abort trap: 6`). `GET
  /api/crashes/<id>?target=` is the `.ips` as written, as `text/plain`, since
  it is two JSON documents. Reports are host files in
  `~/Library/Logs/DiagnosticReports`, only `bug_type` 309, and a row belongs
  to a simulator when its `coalitionName` names that UDID. That directory
  also holds every other simulator's reports and the Mac's own, so **a fetch
  re-checks the target** and answers 404 for another's report as for a
  missing one. The id must be a UUID. A shut-down simulator's reports are
  read without booting it. Synthetic answers 501. Traced as
  `kind:"agent"`, `action:"crashes"` with `count` and `tookMs`, or
  `"crash"`; no report content.

- `GET /api/clipboard?target=<id>` reads the device's clipboard and answers
  `{"target","text"}`. `POST` with `{"text": ..., "paste": true}` sets it,
  and `paste` also puts the text into whatever has focus. `paste` is
  optional. A simulator's paste is typed rather than sent as Cmd+V, because
  iOS asks before an app reads what another process set
  (`docs/targets.md`, Clipboard). The text must be UTF-8 and at most
  64 KiB. Text just over that answers 413. A body more than 1 KiB past the
  cap fails the read instead and answers 400.
  - **Android needs a session open.** scrcpy-server is the only thing on
    the device that reads or sets the clipboard, so with no viewer,
    recording or other holder of the session the call answers 409. A read
    the device doesn't answer within 2s fails with 502.
  - **A simulator needs only to be booted.** The call goes through simcap's
    input process, which starts on demand, so no session is needed. A
    shut-down simulator answers 502.
  - Physical iPhones and synthetic targets answer 501.
  - **A peer target's call goes to its owner**, and the owner's refusal
    comes back unchanged.
  - **A set changes that target only**, even with `sharedClipboard` on:
    the target drops its own write as an echo, so it is not shared
    (`docs/targets.md`, Clipboard).
  - **Neither claims nor the human hold gate it.** `handleClipboard` goes
    straight to the target, so an agent can set or paste while a person is
    driving.
  - It is traced as `kind:"agent"` with `action` `clipboard-read`,
    `clipboard-set` or `clipboard-paste`. The text is never traced.
  - The phone's own clipboard UI is in `web/clipboard.js` and uses this
    route.

- `GET /api/handoff?target=<id>&target=<id>` returns `{"url"}`, the web
  client's link for up to four targets, the first one focused: the startup
  banner's phone link, on the certificate's host, with the token. The Mac
  app's Continue in Browser opens it, copies it, or shows it as a QR code,
  so a session moves to a browser on another device. A server with no
  address another device can open (no TLS certificate) answers 404, and a
  fifth target 400 (`TestHandoff*`).

- `GET`, `POST` and `DELETE /api/overlays?target=<id>` put an
  extension's output in front of everyone watching the target: an image
  over the video (a design to compare against), labelled boxes (where it
  differs), or both. loupe neither makes nor reads them; it keeps the
  latest set per target, sends it to every viewer as `MsgOverlay`, and
  serves each image at `GET /api/overlays/image?target=&id=&v=`, so a
  client only ever talks to loupe. POST takes `{id, source, label?, image?
  (base64 PNG or JPEG), screen?: [w, h], bounds?: [x1, y1, x2, y2],
  opacity?, boxes?: [{bounds, label?, kind?}]}` in device pixels; posting
  an id again replaces it in place. At most 8 per target, 200 boxes each,
  12 MiB a post. `DELETE ?id=` or `?source=` removes some; neither removes
  all. The web client and the Mac app compare by fade, swipe or
  difference.
  (`internal/httpapi/overlay_test.go`).

- `POST /api/extensions` registers an extension that has something for
  viewers to read: `{name, url, label?}`, where `url` must be http or https
  on `127.0.0.1`, `::1` or `localhost` (anything else is 400, since anyone
  with the token could otherwise point loupe at another host). A
  registration lasts 90 seconds, so an extension re-registers on a timer.
  `GET /api/extensions` lists them. `/api/ext/{name}/...` passes through to
  the extension with the prefix, loupe's `Authorization` header and the
  `token` query parameter removed, so a client reads it with loupe's token
  alone. An extension that doesn't answer gets 502 and drops out of the
  list. loupe never reads what passes through; docs/extensions.md is the contract for writing one
  (`internal/httpapi/extension_test.go`).

  **A report lands well after the crash.** The row's `time` is the body's
  `captureTime`; the header's `timestamp`, and the file, come later: 22s on
  a Acmechat crash already on disk, and 36s on `loupe-probe` when Calendar
  was sent SIGABRT from the host, which was listed 37s after the signal.
  SIGKILL leaves no report. A listing of 239 reports took 17ms, so nothing
  is cached.

  **A physical iPhone's reports are copied off it through devicectl**:
  `devicectl device info files --domain-type systemCrashLogs` lists the
  phone's crash directory and its `Retired`, and `device copy from` fetches
  one file (`internal/target/iosdevicecrashes.go`). Each copy is its own
  devicectl run, about 1.7s whatever the size, and the directory is mostly
  not crashes: on Brandon's iPhone it held 101 `.ips` files and one crash, the
  rest analytics, Siri feedback, Jetsam and `.cpu_resource` reports, beside
  spindumps of 2-12MB. So only `.ips` files under 4MB whose process part is
  not one of those names are copied, four at a time, into
  `~/Library/Caches/loupe/ios-crashes/<udid>` under their own names, once;
  a file the phone no longer has leaves the cache. The cache is then read
  by the simulator parser with no coalition, since the phone holds only its
  own reports, and `bug_type` 309 still decides. Through a spare server on
  Brandon's iPhone (iOS 26.6, Wi-Fi): the first listing took 2.9s and
  copied one report, the next 0.8s, a 19KB fetch 0.49s, and a missing id
  was 404. Only an XCUITest runner's SIGABRT was there to read, so an app
  crash on the phone and a crash on bmc-iphone (27.2) have not been listed.

  **On Android the reports are the dropbox's**, read with `adb exec-out
  dumpsys dropbox --print --file <tag>`, no root needed, for six tags:
  `data_app_`, `system_app_` and `system_server_` with `crash`,
  `native_crash` and `anr`. One call per tag, in parallel, since dumpsys
  matches every positional argument at once. The tags are constants, so
  nothing a caller sends reaches the device shell. The id is the tag and
  the entry's epoch millis from its file name
  (`data_app_crash-1792722095531`), and that millis is the row's `time`,
  since the header's clock time is the device's local time. `kind` is
  `crash`, `nativeCrash` or `anr`; `bundleID` is the package, `build` its
  versionCode and `version` its versionName, from the body's `Package:`
  line. The summary is the exception line for a crash, the `signal` line
  and the abort message for a native crash, and `Subject:` for an ANR. A
  fetch is that entry's text as dumpsys prints it, header first. The body
  opens with `Process:` on API 29 and with `SystemUptimeMs:` on API 37;
  both are read. A running AVD delegates to its serial, a shut-down one
  answers "open it in the viewer to boot it", and a call resumes a paused
  emulator. **Dropbox is under `/data`**, so without `emulatorKeepState` a
  boot from `loupe-warm` discards every report since it was saved. Its
  limits are set per device, so how many entries a busy session keeps is
  not known. A listing prints whole entries, ANR traces included, and one
  ANR was 197KB.

**`loupe mcp` puts the Agent API behind MCP**, so an agent that speaks MCP
needs no curl. It is a client of a running server, not a second server: a
stdio MCP server (the official Go SDK) that turns each tool call into one
request. Tools: `targets`, `ui`, `screenshot` (an image, not JSON), `point`,
`tap`, `swipe`, `type_text`, `key`, `button`, `launch`, `open_url`, `logcat`,
`crashes`, `crash`, `install`, `boot` and `release`. `TestEveryToolNamesARoute` checks each
against `httpapi.Routes()`.

- **Flags:** `-url` (default `https://localhost:18456`), `-token` and
  `-agent` (default `claude`). The token is read from `-token`, then
  `LOUPE_TOKEN`, then the server's token file; `loupe mcp` never writes one.
  TLS is verified except for a loopback host, whose cert names the ts.net
  host instead.
- **It claims before it drives.** Every request carries `X-Loupe-Agent` and
  `X-Loupe-Session`, a UUID per process. The first driving call on a target
  claims it; a 409 `claimedBy` forgets the claim so the next call tries
  again. Reads claim nothing. On exit, SIGINT or SIGTERM included, each
  claim is released with a plain `DELETE`, never `?force=1`.
- **It never sends `X-Loupe-Viewer`**, which would make its input a
  person's and walk through the human hold. A refusal comes back as a tool
  error with the status, `code`, `retryAfterMs` and the holder.
- **Stdout is the MCP wire**, so `main` dispatches `mcp` before the server's
  flags and banner, and everything else goes to stderr.

Run through a go-sdk client over stdio against the launchd server on
Pixel_10 (emulator-5556), 22 October: `ui` in 3.2s, `screenshot` as a 169KB
PNG in 219ms, then `button` home claimed the target, `launch` Settings in
0.47s, `release` emptied `/api/claims`, a `tap` claimed again and closing the
client released it. With another agent's claim on the target, `button` and
`launch` came back as tool errors naming `claimedBy` and the holder, and
that claim was still there after the client exited.

Point an MCP config at a copy of the binary, not at `./loupe`: a client
holds the executable open for its whole session, and `make build` then
fails with ETXTBSY, which is why the supervisor runs from `.run/`.

```sh
cp loupe ~/.local/bin/loupe
claude mcp add loupe -- ~/.local/bin/loupe mcp
```

### Recording

**A recording is the encoder's output, unchanged, in an MP4.** The session
already holds every access unit the device produced, stamped with the
device's own capture time (trap 31), so the server writes those to a file:
H.264 at the session's size and bitrate, one sample per access unit, each
sample's duration the gap to the next PTS, no re-encode. That gives a
recording at the encoder's frame rate on the device's clock, whatever the
link or the phone did, which an iOS screen recording of the viewer cannot:
that one has been through the link, the jitter buffer and `present()`.

What it does not show: a frame the encoder never produced, and the viewer's
experience, which is `wtprobe -dump` and `jitter.replay.mjs`. It follows the
session's bitrate, so adaptive bitrate can lower quality partway through;
the metadata lists each change in `bitrateChanges`. It does not pin the
bitrate, which would starve the viewers the controller is protecting.

The frames come from a `Tap` (`internal/session/tap.go`): every frame after
the PTS offset and before broadcast, not paced, not shed, not counted by the
bitrate controller and not arbitrated. It holds 512 frames, about 8s at
60fps. One that fills is closed rather than skipping a frame, and the
recording ends as `fellBehind`, so a file never has a silent hole. The
recorder starts writing at the first keyframe after the config;
`firstKeyMs` is how long that took. A PTS that goes backwards is clamped to
one tick (`TestPTSGoingBackwardsIsClampedToOneTick`). The muxer
(`internal/mp4`) puts `moov` at the end and patches a 64-bit `mdat` size at
`Close`.

Seven routes, all behind the token:

- `POST /api/record?target=` starts one and answers 201 `{"id"}`. It is a
  read, so it is not arbitrated and a person's hold does not refuse it. It
  opens the session the way `/wt` does, so a shut-down AVD boots here. A
  target already recording answers 409 `code: "recording"` with that
  recording's `id`. A server shutting down answers 503, a target that cannot
  open 502.
- `GET /api/record` lists every recording, newest first, in progress or not.
- `GET /api/record/{id}` is one recording's metadata: `target`, `started`,
  `ended`, `reason`, `frames`, `bytes`, `durationMs`, `firstKeyMs`, `width`,
  `height`, `bitrateChanges`, `inProgress`, and `error` after a failed write.
- `POST /api/record/{id}/stop` answers 200 with the metadata once the file
  is closed, so the download can start straight after. Stopping one that has
  already ended answers its metadata again.
- `GET /api/record/{id}.mp4` serves the file through `ServeContent`, so a
  Range request answers 206 and Safari can seek.
- `GET /api/record/{id}/frames?from=&to=` is a JPEG contact sheet, through
  ffmpeg, of the samples between two times (a Go duration or bare seconds),
  with `X-Loupe-Frames` listing each tile's PTS in milliseconds. `fps`
  thins the samples to that rate; absent means every frame. Either way at
  most 60 tiles are kept, sampled evenly. `fps` given as 0, below 0, above
  240 or not a number is 400; a range over 10s or with no frames in it is
  422. Tiles are 8 across. ffmpeg is found on PATH, then `/opt/homebrew/bin`
  and `/usr/local/bin`, since launchd's PATH reaches neither; without it the
  sheet answers 501 `code: "noFFmpeg"` and everything else still works.
- `DELETE /api/record/{id}` answers 204; one in progress is 409
  `code: "recording"`.

**A recording holds its session open with no viewer attached.** `POST`
takes a `Session.Hold`, and `Manager.Release` and the idle reaper count
holds with viewers (`ViewerCount()+Holds()`), so the phone leaving does not
end it after `idleGrace`. The recorder's end gives the hold and the acquire
back, whichever way it ends (`TestRecordingSurvivesTheLastViewerLeaving`).

It ends for one of six reasons, in `reason` and on the `kind:"record"`,
`action:"end"` trace record:

| `reason` | Cause |
| --- | --- |
| `stopped` | `/stop` |
| `limit` | 10 minutes, or the file reaching the store's cap |
| `resized` | A config with another picture size, which is a rotation: an MP4 track has one size |
| `fellBehind` | The tap filled |
| `sessionEnded` | The session closed, including at server shutdown |
| `writeError` | The disk, or the metadata write after the file closed, with `error` set (`TestWriteErrorEndsAsWriteError`) |

Frames already in the tap when a reason arrives are written before it
ends. The start is traced as `kind:"record"`, `action:"start"`, with `id`,
`target`, `tookMs` (which includes opening the session) and the caller.

**The store is `~/Library/Caches/loupe/recordings`**, `<id>.mp4` beside
`<id>.json`, the json written only once the file is closed, so the
directory is the record and only recordings in progress live in memory.
At startup an `.mp4` with no `.json` (a crash) and anything that ended more
than 7 days ago are deleted. The cap is 1 GiB, checked when a recording
starts and when one ends: the oldest finished ones go first, and one in
progress is never deleted (`TestCapDeletesTheOldestFinishedRecordings`).
The cap is also the most one recording may write, since `bitRate` goes to
100 Mbps and ten minutes of that is about 7.5 GB: a recording whose samples
pass it ends as `limit` (`TestCapEndsARecordingAsLimit`). So the total can
exceed the cap by what the recordings in progress hold, each at most the
cap, until the next check.
On shutdown, closing the sessions ends every recording as `sessionEnded`
and main waits for the files to close; a start after that is refused
(`TestWaitReturnsOnceRecordersClose`). One recording per target at a time.

**In the viewer**, Record in the More menu starts one on the current
target. The pill shows a red dot and the elapsed time, and a tap on it
stops. The page polls `/api/record/{id}` every 2s to learn of an end it did
not ask for. Stopping downloads the file with a running count, then shows a
banner: Share calls `navigator.share` with the file inside that tap, since
Safari allows it only there and the file arrived after an await, and Save
is a link to an object URL that lives until the banner goes, because iOS
may ask before it saves and read the blob after the answer. Switching
target stops this page's recording. The page keeps the id it started in
`sessionStorage`, per tab, so a reload brings the pill back by adopting that
recording if it is still in progress on the target. A recording an agent or
another viewer started is never adopted, so a switch cannot stop it. On a 409
from Record the page adopts the id the body names, since the person pressed
Record, and stores it. With `sessionStorage` blocked, a reload forgets the
recording.

Measured through spare servers, each run once on 15 October:

- **Synthetic**, `-no-devices -no-simulators -no-auth` on :18473, recording
  started and stopped with `curl`: 1051 frames in 17.5s at 720x1560, 7.6MB,
  every sample 16.7ms by `ffprobe -show_packets`, 18 keyframes,
  `firstKeyMs` 45, start `tookMs` 2.
- **Pixel_10 on emulator-5554**, `-no-simulators -no-ios-devices` on :18475,
  `avd:Pixel_10` from the Home Screen with no viewer attached, Settings
  opened halfway through by `/api/input` `launch` (answered in 605ms): 97
  frames in 9.48s at 480x1080, 559KB, 2 keyframes, `firstKeyMs` 108, start
  `tookMs` 893, which includes opening the session. The still screen is not
  one long sample. The first second is ten 100ms samples, which matches the
  `repeat-previous-frame-after` the pinned jar sets on the encoder; then one
  sample of 4.87s, the Home Screen at rest; then the launch's transition at
  16-40ms a sample from 5.87s, a keyframe at 6.78s, and 100ms samples again
  with one of 357ms. Why the encoder stopped repeating for those 4.87s is
  not explained.
- **Contact sheets** of that recording: `from=0&to=2` answered in 56ms, 11
  identical Home Screen tiles at PTS 0-1000, 2160x1216 with the empty cells
  green. `from=5.8&to=7` was 56 tiles in 65ms. With `fps=10` it was 12:
  the Home Screen, the Settings splash growing from its icon, the splash
  alone, the list fading in and Settings' home. `to=11` was 422, `fps=0`
  and `fps=241` 400.
- A second `/stop` answered the metadata, the start while recording 409
  with the `id`, and a Range request on the `.mp4` 206.

Not run: QuickTime was not opened on either file; nothing was recorded from
a browser, so the menu, pill, banner, Share, Save and the re-adopt are
covered by node tests only, as is the `.recBanner` layout rule; and the
emulator run was 9.5s with no viewer, under `idleGrace`, so it does not
show the hold. The hold, `fellBehind`, `limit`, `resized`, `sessionEnded`,
`writeError`, the cap and bitrate changes during a recording are covered
by unit tests, not live runs.

