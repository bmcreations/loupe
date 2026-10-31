# Targets

AVDs, the Android phone keyboard, the clipboard and device frames. iOS
simulators and physical iPhones are in `docs/ios.md`.

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
| Shut Down | `POST /api/devices/shutdown?target=avd:<name>` | Stops the emulator; the AVD stays and opens from `loupe-warm` |
| Delete | `DELETE /api/devices?target=avd:<name>` | Stops it, then removes the AVD and its `loupe-warm` snapshot |
| Reset | `POST /api/devices/reset?target=avd:<name>` | Stops it without saving and wipes it: the next boot is a cold `-wipe-data` boot, then a fresh `loupe-warm` |

The picker's swipe actions call the first two (see `docs/agent-api.md`).

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
- Android is offered when `avdmanager` is found (`-android-avdmanager`, then
  `cmdline-tools/*/bin` in the SDK). iOS is offered when `simctl` lists
  simulators and simcap runs.
- `/api/avds` and `/api/devices` share one slot. A create on either answers 409
  `creating` while the other runs, and 409 `avdExists` when a device of that
  name exists on that platform.
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

With no avdmanager, `profilesFallback` is true and the client uses Google's
`android` CLI through `/api/avds/*`.

- `GET /api/avds/profiles` lists the CLI's profiles (`small_phone`,
  `medium_phone`, `medium_tablet` and three desktops), each with `exists`.
- `POST /api/avds {"profile"}` answers 202 and creates in the background.
- The CLI is found by `-android-cli`, then `/usr/local/bin` and
  `/opt/homebrew/bin`. Without it both endpoints answer 404 and the picker hides
  the entry.
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
  would never appear, so loupe offers no create and says so at startup.

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
- **Simulators** use Xcode's DeviceKit chrome, read in place. They do not
  rotate yet. A Duo answers with the lit panel's frame, so it changes when the
  fold changes.
- The art is cached and ETagged. It is about 230KB for a Pixel.
- The frame is **off by default in the client**. It costs about a tenth of each
  axis of the screen.
