# Targets

AVDs, Revyl cloud devices, the Android phone keyboard, the clipboard and
device frames. iOS simulators and physical iPhones are in `docs/ios.md`.

## AVD targets

Every installed AVD is a target, booted or not, with the id `avd:<name>`. The
picker shows its OS, API level and form without booting it. The AVD directory
is rescanned on each `GET /api/targets` and on any `avd:<name>` the server does
not know (at most every 2 seconds), so an AVD made with `avdmanager` or Android
Studio appears and boots without a restart. A removed AVD stays listed until
a restart; delete through `DELETE /api/devices`.

### Running emulators

A running emulator is its AVD, matched by name, never by port. An emulator
started from Android Studio is the same target as one this server booted.

- While the AVD runs, every capability (`/api/ui`, `/api/input`, screenshots,
  frames) goes to the emulator. While it is not running, each fails with "open
  it in the viewer to boot it".
- The serial (`emulator-5554`) still works as an id, and `/api/targets`
  reports it as `serial`. The alias lasts only while the AVD runs.
- `-avd <name>` boots that AVD at startup.
- The emulator binary comes from the SDK: `-android-sdk`, `ANDROID_HOME`,
  `ANDROID_SDK_ROOT`, then `~/Library/Android/sdk`.
- A boot picks a free even console port in 5554-5584.
- The emulator runs in its own session, so restarting the server leaves it
  running and the next process attaches by name. Only the picker's Shut Down or
  Delete stops one.
- A booted AVD runs with its window hidden and no audio. `emulatorWindow`
  (`-emulator-window`) shows the window from the next boot. An emulator started
  from Android Studio keeps its window.

### Opening a shut-down AVD

A shut-down AVD opens from `loupe-warm`, a snapshot loupe saves on the first
boot, so a later open takes seconds instead of a cold boot. On a quiet Mac a
warm open measured 2.2-2.5s to connect and 3.3-3.8s to the first frame; a cold
first boot took about 22s. Under heavy load a warm open took up to 8.5s.

You see the Home Screen with the current clock, not the app you left.

By default every boot from the snapshot discards what the last session
changed: files written to `/sdcard/Download` are gone, and an `/api/install`
lasts only while that emulator process runs. The snapshot costs 1.7-2GB in
each AVD directory loupe has cold-booted.

To keep an install, or anything else set up by hand, for every later boot,
use Save & Shut Down in the picker (`POST /api/devices/shutdown?save=1`). It
ends the stream, saves the state over `loupe-warm` and shuts the AVD down;
the next open starts there, and each later boot still discards what its own
session changed. The save writes the 1.7-2GB snapshot again and pauses the VM
for 1-2s. The picker row then reads "Start state saved <date>", from
`startSavedAt` in the listing. A save that fails, or a stream that has not let
go within 3s, leaves the AVD running and unsaved; the stream has already
ended by then, so reopen it to keep watching. Reset goes back to the
system image's own data and removes the saved start state.

`emulatorKeepState` (off by default, `-emulator-keep-state`) saves the state
over `loupe-warm` before a shutdown, the idle one and the picker's Shut Down,
so the next open resumes where the last left off. Delete never saves. A failed
save is traced and the shutdown goes on.

### Idle pause and shutdown

A headless emulator nobody is using is paused, then shut down. A windowed
emulator is never touched, since it may be on someone's screen.

| Setting | Default | Flag | Effect |
| --- | --- | --- | --- |
| `emulatorPauseSec` | 300 | `-emulator-pause` | Seconds unused before the VM is paused; 0 never pauses |
| `emulatorShutdownSec` | 1800 | `-emulator-shutdown` | Seconds unused before the emulator is shut down; 0 never |
| `emulatorKeepState` | off | `-emulator-keep-state` | Save state before a shutdown |

All three are also in the config file and `/api/config`, and apply immediately.

An emulator counts as in use while it has a session (a viewer or a recording)
or a `/api/logcat?follow=1`, and for any call that reaches it. A live claim
also holds it: neither the pause nor the shutdown touches a claimed emulator
(the clock restarts while the claim lasts), and taking a claim resumes one
that is paused. A claim lapses 10 minutes after its holder's last call.

- **Paused** means the VM is stopped and its CPU use drops to near zero (about
  0.3%). The next call that needs the device, or an open, resumes it in about
  10ms. `/api/targets` never resumes one: it reports `paused: true`.
- **Shut down** counts from the last use, so a paused emulator keeps ageing and
  is shut down without a resume. The next open boots from `loupe-warm`, so
  whatever changed since the boot is gone unless `emulatorKeepState` is on.
- A Shut Down from the picker during an idle shutdown answers 409 `busy`. An
  open while the idle shutdown runs is refused instead of booting into it.
- Only loupe resumes a paused emulator. Android Studio's Run, `adb install` or
  `adb shell` from a terminal hangs on a paused emulator until a loupe call
  wakes it. Claim the emulator first (`POST /api/claims`) to keep it awake
  for that work; `emulatorPauseSec` 0 turns the pause off.

### Shut down, delete and reset

| Action | Route | Result |
| --- | --- | --- |
| Save & Shut Down | `POST /api/devices/shutdown?target=avd:<name>&save=1` | Saves the state over `loupe-warm`, then stops the emulator; every later boot starts from it |
| Shut Down | `POST /api/devices/shutdown?target=avd:<name>` | Stops the emulator; the AVD stays and opens from `loupe-warm` |
| Delete | `DELETE /api/devices?target=avd:<name>` | Stops it, then removes the AVD and its `loupe-warm` snapshot |
| Reset | `POST /api/devices/reset?target=avd:<name>` | Stops it without saving and wipes it: the next boot is a cold `-wipe-data` boot, then a fresh `loupe-warm` |

The picker offers Save & Shut Down, Shut Down and Delete on a running AVD,
and Reset and Delete on a shut-down one (see `docs/agent-api.md`). Reset in
the picker asks first.

- Shutdown took about 1.6s with a viewer attached; delete about 1.8s.
- After a Shut Down or Delete, a plain open of that target is refused for 10s,
  so the viewer's reconnect does not boot the emulator you just stopped. An
  explicit `start=1` open lifts that after a Shut Down. A deleted target is
  refused as `deleted` for a minute and unknown after that.
- Reset returns the system image's own data, as Android Studio's Wipe Data does,
  so anything set up by hand after the first boot is lost. It takes a cold boot
  (39s on a loaded Mac). Removing only the snapshot is not a reset: once a kept
  state has been saved over `loupe-warm`, a cold boot reads the disk as the last
  session left it.

### New device

New device in the picker creates an AVD or a simulator from what is already
installed. Nothing is downloaded.

- `GET /api/devices/options` lists the platforms that can create, each with
  `os`, `devices` and `pairs` (which devices an OS takes). A platform with no
  OS installed is listed with `os: []`.
- `POST /api/devices {platform, os, device, name}` answers 202 with the row id:
  `avd:<name>`, or `sim-new:<name>` for a simulator, which has no UDID yet. The
  finished job reports `createdId` as `sim:<udid>`.
- Android is offered when `avdmanager` is found at startup
  (`-android-avdmanager`, then `cmdline-tools/*/bin` in the SDK) and runs.
  Startup runs `avdmanager list device -c` once to check, since avdmanager
  is a script that starts Java. iOS is offered when `simctl` lists
  simulators and simcap runs.
- `/api/avds` and `/api/devices` share one slot. A create on either answers 409
  `creating` while the other runs, and 409 `avdExists` when a device of that
  name exists on that platform.
- A failed create's row stays in `/api/targets` as `createFailed` until the
  next create on either route, or a restart: the slot is in memory only. No
  target is behind the row, so `DELETE /api/devices` answers 404 for it.
- Only valid pairs are offered. TV, Wear, automotive and desktop devices are
  never listed, a device is not offered on an image older than its API floor
  (`pixel_fold` needs 34, `pixel_9` 35), and a tablet image takes tablets only.
  An iOS runtime offers only the device types it supports.
- The SDK's avdmanager has no Pixel 10 definition, so New device cannot make
  one.
- Android names match `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$` and collide without
  regard to case. Simulator names match `^[A-Za-z0-9][A-Za-z0-9 ._()-]{0,63}$`
  and are also compared without case.
- A finished AVD gets `hw.keyboard=yes`, `hw.gpu.enabled=yes`, a display name
  and, for a Play Store image, `PlayStore.enabled=true`. If that patch fails the
  create still succeeds and the job carries a `warning`.
- The new simulator appears in the picker straight away.

Not run against a live server or a phone: only the parsers ran against a real
SDK and Xcode, and the create paths against fakes.

#### Without avdmanager

With no avdmanager, or one that does not run, `profilesFallback` is true and
the client uses Google's `android` CLI through `/api/avds/*`.

- avdmanager that does not run is the usual case on a Mac set up by the CLI
  alone that still has `cmdline-tools` from an old Android Studio and no Java
  runtime. There avdmanager exits 1 with `Unable to locate a Java Runtime`,
  and startup logs `warning: New device is not using <path>, which does not
  run` with that message.

- `GET /api/avds/profiles` lists the CLI's profiles (`small_phone`,
  `medium_phone`, `medium_tablet` and three desktops), each with `exists`.
- `POST /api/avds {"profile"}` answers 202 and creates in the background.
- The CLI is found by `-android-cli`, then `/usr/local/bin`,
  `/opt/homebrew/bin` and loupe's own download (below). It is looked up on
  every request, so a CLI installed while loupe runs is offered without a
  restart. Without it both endpoints answer 404, `profilesFallback` is false,
  and the picker hides the entry.
- `--sdk` is the SDK root as found on that request. With none, the CLI uses
  `~/Library/Android/sdk`.
- The profile list is kept once a listing names profiles. A failed or empty
  listing is asked again on the next request.
- The AVD is named after the profile, so a profile makes one AVD. A profile
  whose `avd:<name>` is already a target answers 409 `avdExists`. One create
  runs at a time; another answers 409 `creating`.
- While it runs, `/api/targets` carries a row for `avd:<profile>` with state
  `creating`, and the row text moves from `Preparing` to `Getting the system
  image` to `Creating the device`. A failure leaves state `createFailed` with
  the CLI's error until the next create. On success the AVD appears in the
  picker without a restart.
- The first create downloads the system image (2.3 GB for API 36.1, about 4.6
  minutes). With the image already installed a create took under a second.
- The CLI ignores `ANDROID_AVD_HOME`. With it set elsewhere, a created AVD
  would never appear, so loupe offers no create: both endpoints answer 404
  with both paths, checked per request, and startup logs the same warning.
- AVDs registered after startup get the emulator from the SDK as found then,
  so an AVD whose create brought the emulator boots without a restart.

#### Without an SDK

A Mac with neither Android Studio nor an SDK needs only the CLI. On a macOS
27.2.1 VM with no SDK, `android info` reported `~/Library/Android/sdk` as the
SDK root, and the first `android emulator create small_phone` fetched
platform-tools, the emulator and the system image into it (37 MB, 1.2 GB and
2.3 GB, in 2m25s). Phones and Firebase devices need adb with no create, so
`/api/android/cli` installs the CLI and adb. It sits beside `/api/avds`
rather than under `/api/remote` because emulators need the CLI with no
Firebase account.

- `GET` reports `installed` and `path` for the CLI, `adbInstalled` and
  `adbPath`, `canInstall`, `installing` with `step` (`cli`, then
  `platform-tools`), and `error` from the last install.
- `POST` installs whatever is missing in the background and answers 202, or
  200 when nothing is. A missing CLI that this server cannot install is 501.
- The CLI comes from Homebrew's `android-cli` cask when brew is installed, else
  as a checksum-verified download into
  `~/Library/Application Support/loupe/bin/android`, which needs no admin.
- adb comes from `android sdk install platform-tools`, with the same `--sdk` as
  a create. The verdict is adb being found and no `Error:` line, not the exit
  status.
- One install runs at a time, shared with `POST /api/remote/cli`. During either
  one, `POST /api/android/cli` answers 409 `installing`. `/api/remote/cli`
  still answers 202 and reports the running CLI step as `cliInstalling`.
- adb counts as installed only where loupe runs it from: `-adb`, else `PATH`.
  The Handset Mac app launches loupe with `<sdk>/platform-tools` on `PATH`
  before it exists. A loupe started without that gets an `error` naming the
  path to pass as `-adb`.
- With no adb, every AVD reads `adbUnreachable` after 3 track-devices failures
  and the watcher retries at most every 30s. A finished `platform-tools` step
  or AVD create cuts that wait short, so the first AVD can open at once.

The route, the per-request lookup and the cut-short wait have run only against
fakes.

### When adb is wedged

A wedged adb server looks like every AVD being shut down. Loupe handles it in
three steps:

- Before booting, it looks for a running emulator process for that AVD and
  refuses with "already running (pid N) but adb cannot reach it", rather than
  booting a second copy.
- After 3 track-devices failures in a row (about 3s), every adb device and
  every AVD without a live session reports state `adbUnreachable`, with adb's
  stderr as the reason. A session already streaming is left alone.
- After 6 (about 30s), loupe restarts the adb server itself, at most once every
  10 minutes. This drops Android Studio's adb connection for a second or two;
  it reconnects on its own.

The restart has only run under unit tests, not against a wedged server.

### No devices

With no targets at all, the picker shows New device and How to connect a phone.

## Revyl cloud devices

A cloud Android emulator or iOS simulator on the user's own Revyl
(revyl.com) account is a target with the id `revyl:<sessionId>` and kind
`revyl`. loupe drives it through Revyl's `revyl` CLI, except input, which
goes over the device-proxy endpoint the CLI itself uses (see
[Input](#input)). The flags and `--json` shapes come from revyl-cli v0.2.27
(commit `35b06ed8`). It is all tested against a fake CLI, a fake device
proxy and a local WHEP server. On 2026-11-18 the device-proxy input,
`revyl device tap`, and the `workflow_run_id` that `device start` and
`device info` report were checked against a live account, with revyl-cli
v0.2.18.

### CLI and account

- The CLI is found by `-revyl-cli`, then `~/.revyl/bin/revyl` (where Revyl's
  install script puts it), `/opt/homebrew/bin`, `/usr/local/bin`, PATH, then
  loupe's own copy at `~/Library/Application Support/loupe/bin/revyl`. It is
  looked up on every request, so one installed later needs no restart.
- With none, `POST /api/revyl/cli` downloads the v0.2.27 release binary for
  the Mac's architecture into loupe's path and checks its SHA-256 against
  the value pinned in loupe. No Homebrew and no admin.
- Sign-in is the user's: `revyl auth login` stores `~/.revyl/credentials.json`,
  or `REVYL_API_KEY` is in loupe's environment. `POST /api/revyl/account`
  runs the login and answers with its approval URL and code once the CLI
  prints them. The CLI also opens the URL in the Mac's browser, and has no
  flag to stop it. The URL and code stay out of the trace.
- loupe reads the key for input over the device proxy: `REVYL_API_KEY`
  first, else `api_key` in `~/.revyl/credentials.json`, read again for every
  gesture so a new sign-in needs no restart. The key stays out of errors,
  the trace and every API response.
- Every run sets `REVYL_TELEMETRY_DISABLED=1` and `REVYL_NO_UPDATE_NOTIFIER=1`,
  and runs in `~/Library/Application Support/loupe/revyl`, so a project's
  `.revyl/config.yaml` in loupe's start directory is not read.

### Sessions

- `POST /api/revyl` starts one device at a time; another start answers 409.
  While it starts, `/api/targets` has a `revyl-starting:<key>` row with state
  `starting` and transport `cloud`. The start waits for Revyl's queue, up to
  6 minutes.
- Revyl bills from the start until the device stops or goes 900 seconds
  without a command (`--timeout`, the CLI's default). Whether a WHEP viewer
  alone keeps it alive is unknown.
- At startup, and every minute while loupe holds a device, loupe reads the
  account's sessions from `revyl device list`. A session it did not start is
  adopted as a target and left running when loupe exits. One loupe started
  is stopped at exit. A session that leaves the list leaves the picker.
- A start still running at exit is not waited out. The CLI gets SIGINT,
  which `revyl device start` catches to cancel the device at Revyl
  (revyl-cli v0.2.27 `cmd/revyl/device.go:1111`), and is killed 5 seconds
  later if it is still running. A start that finishes anyway prints its
  session, and loupe stops that device. Once loupe is shutting down,
  `POST /api/revyl` answers 503. A `DELETE /api/revyl` for a device still
  starting answers 409.
- Exit runs these stops first, at the same time as the Firebase releases
  and the HTTP shutdown, and gives them 10 seconds from the signal; the
  5-second SIGINT wait comes out of those 10. Under launchd the window can
  be shorter: launchd SIGKILLs a job ExitTimeOut after its SIGTERM, and
  `launchctl print` shows 5 seconds for a job whose plist sets none.
  loupe's own plist sets 30, and `loupe-supervisor.sh` passes the SIGTERM
  on within 2 seconds and gives loupe 25 seconds to exit, so the whole
  budget applies there. With 5 seconds the SIGINT wait can use all of it,
  so the stop of a device whose start finished anyway runs only with a
  longer window. A stop still running at the cutoff is abandoned, and whether
  Revyl acted on its request is not known. A device that comes up after
  loupe has gone is adopted by the next loupe and runs until it is stopped
  or Revyl's 900-second idle timeout ends it. Once exit begins,
  `POST /api/remote` answers 503 too.
- `DELETE /api/revyl` runs `revyl device stop` for up to 45 seconds whether
  or not the client waits for the answer, so a client that hangs up or times
  out does not leave the device billing. Until the CLI confirms, the device
  stays in `/api/revyl` as `stopping` and keeps its target, and a second
  `DELETE` answers 409. A stop that fails or runs out of time answers 502
  and puts the device back to `active` with `stopError`, so it can be
  stopped again. Exit waits for a stop in progress, within its 10 seconds.
- Until loupe has read the account's sessions once, it does not know a
  device left running before it restarted. Until then, a `DELETE /api/revyl`
  for a device it does not know runs `revyl device list` first, so that one is
  found and stopped instead of answered 404. If the list fails, the answer
  is 502, not 404. While a start runs, the list leaves sessions loupe does
  not know to the start, so the answer is 409 until the start is up.
- The WHEP, viewer and worker URLs Revyl returns are kept out of
  `/api/targets`, `/api/revyl` and the trace, and any URL in the CLI's
  error output is cut before it reaches either.
- For 2 minutes after loupe starts or stops a session, `revyl device list`
  is not trusted over loupe, in case it lags.

### Video

`revyl device info` gives the session's `whep_url`. loupe is a WHEP client:
it offers a receive-only H.264 track, assembles RTP into access units on the
marker bit, and hands them to the normal session, so viewers, recording and
the agent stream work unchanged. `Meta` takes the size from the SPS, and
`Open` returns only once the first SPS has arrived. A keyframe request is an
RTCP PLI, sent at most every 250 ms. No header is sent with the offer;
whether Revyl needs one is unknown.

Revyl's video loses packets while the screen moves, and it sends a keyframe
once a second whatever loupe asks for: on 2026-11-20 neither a PLI nor a FIR
brought one sooner. A lost packet that is never resent therefore costs about
a second of pictures, so loupe works to recover packets:

- The offer has RTX, a separate stream for resent packets, as a browser's
  does. Revyl resends on it, and pion puts the packets back in the track.
  Over the same 20 swipes, packets that never arrived fell from 18 of 12,591
  without it to 1 of 16,402 with it.
- Packets behind a gap wait up to 200 ms after they arrive for the gap to
  fill. Packets in order are not held. A resend usually takes 30-150 ms; a
  burst of about 100 lost packets took up to 1.15 s, which the hold does not
  cover.

A packet that arrives after its gap was given up on is dropped.

With a trace path set, the stream writes a `whep-answer` record with the
answer's codec, feedback and header-extension lines, and a `whep` record
each second it receives: `pkts`, `gaps` (forward jumps) and the `missing`
packets they skipped, `late` packets that filled one and `lateMaxMs`,
`stale` packets, `pics` assembled and `sent`, pictures dropped as
`dropBroken`, `dropAwaitKey` or `depackErr`, `plis`, `idrs`, `keyWaitMs`
(the longest PLI-to-keyframe wait), `tsSkips` (pictures more than two 60 fps
intervals apart), `queueMax` and `sendMaxMs`.

### Input

A viewer's press goes to the device as it happens while Revyl's worker
socket is open (see [Live touch](#live-touch)), so a drag moves the screen
while the finger is down. Otherwise Revyl takes whole gestures.

- A whole gesture is a press buffered from down to up and sent when the
  finger lifts: a tap, a long-press when held 500 ms or more, otherwise a
  drag from where it started to where it ended. Such a drag shows on the
  device only after the lift.
- Only the first finger is tracked. Multi-touch, pinch and scroll are not
  supported.
- Android takes pixels and iOS points. Stream pixels are scaled by the
  session's `screen_width` and `screen_height`. An iOS session that reports
  no size refuses input rather than guess the scale.
- Keys: Home, Back (Android only), Enter and Backspace. Revyl's phones take
  no other key.
- Text is typed at the last tap, so tap the field first. It does not clear
  the field.
- Gestures go one at a time, in order, each a round trip to Revyl, so input
  lags the finger by at least that. A viewer's input returns at once. Up to
  32 gestures wait, and past that a gesture is refused, but a viewer's text
  never is: typing that waits at the same point joins into one call, so a
  burst of keystrokes is one request.

Each gesture is a `POST` to
`https://backend.revyl.ai/api/v1/execution/device-proxy/<workflow_run_id>/<action>`
(revyl-cli `internal/api/client.go:6603`), with the session's
`workflow_run_id` from `revyl device info`. Revyl does not document this
endpoint. The actions are `tap`, `drag`, `longpress`, `go_home`, `back`,
`key` (`ENTER` or `BACKSPACE`) and `input`, with the bodies the CLI's MCP
tools send. On 2026-11-18, against revyl-cli v0.2.18, a tap took a median of
500 to 640 ms over one kept-alive HTTPS connection, against 860 to 1100 ms
for `revyl device tap`. The device's own part was about 6 ms.

- The request sends `Authorization: Bearer <key>` and `User-Agent:
  loupe/<version>`. A request that claimed to be the CLI, with
  `X-Revyl-Client: cli` and no CLI version, was refused with 426
  `cli_upgrade_required`.
- The CLI sends the gesture instead when there is no key or no
  `workflow_run_id`, or when Revyl did not act on the request: it was never
  sent, the backend refused it with 401, 403, 404 or 426, or the proxy
  answered a 5xx with code `worker_gone`, which the Revyl CLI documents as
  the action never reaching the device. After a 426 the
  stream stops trying the proxy; after a 401 or 403 it stops until the key
  changes.
- A gesture is not resent after any other 5xx, a timeout or a dropped connection
  once the request went out, or when the device answers `success: false`. The device
  may already have acted, and a tap sent twice lands twice.
- The trace has one `input` record per gesture: the gesture, `via` (`rest`
  or `cli`), `ms`, and why the proxy was skipped or failed. The key, the URL
  and typed text stay out of it.
- The proxy URL is fixed. The CLI's own overrides, such as
  `REVYL_BACKEND_URL`, move the CLI fallback but not the proxy.

### Live touch

A viewer's touches on Android and iOS go over the websocket Revyl's browser
viewer uses. Revyl does not document it; its shape was read from Revyl's
viewer and live sessions on 2026-11-19. Every failure falls back to whole
gestures.

- When the stream opens, loupe asks
  `GET https://backend.revyl.ai/api/v1/execution/streaming/worker-connection/<workflow_run_id>`,
  with the same bearer key, for `worker_ws_url`, and dials it with
  `Origin: https://app.revyl.ai`. The URL carries a token, so it stays out of
  the trace and errors.
- Touches are text frames `{"event_type": "STREAM", "action":
  "MANUAL_TOUCH_DOWN" | "MANUAL_TOUCH_MOVE" | "MANUAL_TOUCH_UP", "action_id",
  "client_sent_at_ms", "x", "y"}`, scaled onto the `device_width` and
  `device_height` worker-connection reports: pixels on Android (1080x2400 on
  a live session) and points on iOS (402x874 for an iPhone streamed at
  1206x2622). Without them, the session's `screen_width` and `screen_height`
  are used, as for whole gestures. The up adds `gesture`
  (`tap`, `long_press` or `drag`) and `duration_ms`. Moves are joined to the
  newest point, at most one every 16 ms.
- The size follows the socket's updates. A text frame with `device_width`
  and `device_height`, or `width` and `height` under `deviceDimensions` or
  `device_dimensions`, replaces it for every press that starts afterwards; a
  press already down keeps the size it started with. On Android the
  `DEVICE_INIT_STATUS` frame carries the pair, and Revyl's viewer takes a new
  size from its `elements` frames. A side under 1, from 65536 up, or not a
  number leaves the size as it was. On iOS a size that is the current one
  at 2x or 3x, or a half or third of it, either way up, is the screen in
  pixels and is ignored, so touches stay in points; no iOS frame with a
  size has been seen yet. Rotation still swaps the axes of the newest size,
  and the 1% shape check below uses it.
- The worker opens with a `DEVICE_INIT_STATUS` frame, and a press goes live
  only once its `manual_input_ready` is true. It answers each touch with
  `ACTION_RECEIVED` and the up with `ACTION_ACK`, carrying the up's
  `action_id` and `success`. The next gesture waits up to 3 s for that ack,
  so it cannot reach the device before the lift.
- A live press holds the queue from down to lift, so an agent's gesture
  waits behind it. A press nothing is heard of for 30 s is lifted. A drag or
  long-press that waited behind other gestures until its finger had lifted
  goes as one gesture, since its moves would otherwise land at once.
- A cancelled or quiet press whose down already went out is lifted where
  it is, so one that had not moved lands as a tap or long-press. A whole
  gesture that is cancelled is dropped instead.
- A stream whose aspect ratio is more than 1% off the reported size, turned
  with the stream, keeps its presses off the socket: each goes as one
  gesture, scaled by the session's size. Revyl's viewer turns input off past
  the same 1% on some iPhones rather than guess where a touch lands.
- The worker pings the socket and drops a viewer that does not answer, so
  each ping gets a pong with its id, as Revyl's viewer sends.
- With no socket, a press goes as one gesture and a later press tries the
  socket again: 2 s after a failure, doubling to a minute. If the socket
  fails partway through a press, the rest goes over the proxy at the lift:
  the whole gesture when nothing was sent, or a drag from the last point sent
  to the lift. A tap or long-press whose down already went out is not sent
  again, since it would land twice.
- A live press has one `input` record with `via: ws`, `frames`, and `ack`
  when no ack came. A whole gesture's record gives, under `live`, why the
  last try at the socket failed, or that the stream was of another shape. A
  frame that changes the size has a `live-size` record with the new `size`,
  the one it `was`, and the `frame` type it came in; one ignored on iOS
  also has `ignored`. The socket has a `live-open` record when it opens and
  a `live-close` record when it stops: `by` (`worker` with its close
  `status`, `loupe`, `eof` or `error`), `uptimeMs`, and the `pings` and
  `pongs` it answered.

### Agent input

`/api/input` and `/api/sequence` reach a Revyl device through an open
session (a viewer, a recording or another holder). An agent's gesture joins
the same queue as the viewer's touches, behind whatever is already waiting,
and goes out the same way. With no session the call answers 409 and says to
open the device in a viewer.

- The call answers once Revyl has, with Revyl's result: 200 when it acted,
  502 when it refused or failed. A viewer's input, by contrast, returns at
  once.
- Coordinates are stream pixels, as for every target, and are mapped like a
  viewer's touches: pixels on Android, points on iOS. The target reports the
  stream's size as its screen size, so the event log, the agent cursor,
  overlays and flags place a Revyl point, and a flag records the size.
  `/api/screenshot` still answers 501.
- `tap` sets where `text` types next, as a viewer's tap does. `text` before
  any tap answers 409. An agent's text is its own call: it does not join a
  viewer's typing, and a viewer's typing does not join it.
- `swipe` is a drag from the first point to the second, and `ms` is ignored.
  Neither the proxy's drag body nor `revyl device drag` takes a duration, and
  `revyl device swipe`, which does, takes a direction instead of an end point.
- `key` takes 3 (Home), 4 (Back, Android only), 66 (Enter) and 67
  (Backspace).
  Any other keycode answers 501, naming the ones the device takes. `button`
  takes `home`, and `back` on Android; `recents` and `lock` answer 501, and a
  name outside the button table answers 400 as on every target.
- An agent's gesture counts against the 32-gesture limit, text included, and
  past it is refused with 502.
- If the session closes before the gesture is sent, the call answers 409. If
  it closes while the gesture is in flight, the 409 says the gesture may have
  landed. A gesture still waiting when the 10 s request timeout runs out is
  taken off the queue, so it does not land after the agent was told it failed.
- Tap or wait by `match`, `scroll`, `pinch`, `rotate`, `reclaim`,
  `hardwareKeyboard`, `launch` and `openURL` answer 501. The first three need
  the UI tree, which a Revyl target cannot read.

Agent input is tested against the fake device proxy and CLI, not yet against
a live device.

### What it does not do

`/api/ui`, `/api/screenshot`, `/api/install`, logcat, crashes, settings and
the clipboard answer 501. The lifecycle routes under
`/api/devices` answer 422 `notSupported`; stop a device with `DELETE
/api/revyl`.

## Phone keyboard on Android

`hardwareKeyboard` on an adb device or a running AVD makes Android treat the
phone as having a physical keyboard, so it hides the on-screen keyboard while
you type from the viewer. Typing still goes in as text and key events.
`/api/input`'s `text` and `key` use the same path while a session is open, and
`adb shell input` when none is.

- It needs a session: with none, the switch answers 409.
- It lasts as long as the session. When the target's last stream closes the
  setting clears and the next phone to connect keeps its own keyboard.
- Turning it off brings the on-screen keyboard back.
- A key sent right after a burst of two or more characters waits for the text
  to land: 300ms plus 20ms each, capped at one second. A paste or a predicted
  word waits; a single keystroke never does.

## Clipboard

### Android

Copies on the device are offered to the viewer as a banner to tap, and the
viewer can set the device clipboard.

### Simulators

- A paste is typed, not Cmd+V, because iOS asks before an app reads what
  another process put on the pasteboard. Text with a character that has no key
  fails before anything is sent.
- `POST /api/drop` pastes an image into a text field through the edit menu. It
  taps the field, raises the menu, taps Paste, then reads the field back.
  `verified:true` means the field's value changed; `verified:false` means the
  field had no value to compare. Errors:
  - `noPaste`: no Paste item appeared, for example on a static label.
  - `pasteFailed`: two attempts left the field unchanged.
- A drop waits up to 5s after an app launch for its tree to read, since taps
  sent in the first 1.2-2.5s are lost. Straight after a launch it can still
  miss: one of five drops answered `noPaste`.
- A drop takes 2.5-5s.

### Safari

Safari gives access to the clipboard only inside a tap. Copy from device and
Paste to device each need a tap, and iOS shows its own Paste callout when
another app set the clipboard. All four paths ran in Safari against an
emulator and a simulator; none has run on a real iPhone.

### Shared clipboard

With `sharedClipboard` on (`-shared-clipboard`, off by default, applies
immediately), every copy replaces one shared value, and it is written to every
other target with a session and to the Mac. The source can be a target with a
session or the server's Mac.

- A target with no session misses it. Opening a session writes the current
  value to it, unless that target is where the value came from.
- Peer targets are never written, since their owner holds them.
- A copy carrying `org.nspasteboard.ConcealedType`, `TransientType` or
  `AutoGeneratedType`, which password managers set, is reported as
  `confidential` and stays on the Mac.
- It needs a simcap built with `hostpasteboard` (check `-caps`); run
  `make simcap` otherwise.
- Steps are traced as `kind:"clipboard"` with lengths only, never text.

Servers share it over `/api/clipboard/shared` (`docs/agent-api.md`). A hub
bridges its value to every peer that also has `sharedClipboard` on. Each server
writes only its own targets and its own Mac. Order is arrival order on each
server, and a same-millisecond tie goes to the larger id. Two copies made at
once on two servers each take the other's, and the servers hold different
values until the next copy.

Handset's Device > Share Clipboard toggles `sharedClipboard`. When the server is
on another Mac, Handset also syncs that Mac's pasteboard, skipping the same
three password-manager types. When the server reports `hostPasteboard` in
`GET /api/config`, meaning it mirrors this same Mac, Handset leaves the
pasteboard alone.

## Full screen

Full screen in the overflow menu hides every control and the bezel and fits the
stream to the whole page. Hold the handle at the top edge to leave.

In a Safari tab the address bar stays, because iPhone Safari only lets video go
full screen. Opened from the Home Screen, the page asks for no Safari bars
(`apple-mobile-web-app-capable`); that has not been tried on iOS 27.

## Device frames

`GET /api/frame` describes the bezel to draw around a stream, and
`/api/frame/asset` serves the images it names. A target without a framer
answers 501, as synthetic does.

| `source` | Where it comes from |
| --- | --- |
| `skin` | The emulator's own SDK skin, exact art and exact geometry; also Xcode's chrome for a simulator |
| `derived` | A generic bezel around what the device reports about itself |

- **Skins** are read from the installed SDK and never vendored, since the
  Android SDK terms restrict redistribution. On a machine without that skin, or
  when a skin read fails, the frame falls back to `derived`.
- **Derived** frames use `wm size`, `wm density` and the display cutout. A
  device does not report its corner radius, so only a skin carries a real one;
  a derived frame uses a chosen bezel width and radius.
- **Rotation.** `/api/frame` answers for the orientation the device is in.
  `rotation` is the number of clockwise quarter turns the client applies to the
  art, and the geometry in the response is already turned. While a turned skin
  frame is shown, the client rechecks every 3s so a flip between the two
  landscapes redraws.
- A **derived frame does not turn**: the client hides it in the other
  orientation.
- **Simulators** use Xcode's DeviceKit chrome, read in place. A Duo answers
  with the lit panel's frame, so it changes when the fold changes.
- A **turned simulator** keeps its picture's size: the content is drawn turned
  inside it, and the frame's geometry and art stay upright. `contentRotation`
  is the number of clockwise quarter turns the client applies to the picture
  and the frame together (3 for landscapeLeft). It is omitted when upright, and
  always on Android, whose picture already follows the turn. The stream's size
  does not change, so no init is resent; a client refetches the frame after a
  rotate.
- The art is cached and ETagged. It is about 230KB for a Pixel.
- The frame is **off by default in the client**. It costs about a tenth of each
  axis of the screen.
