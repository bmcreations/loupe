# Targets

Moved out of CLAUDE.md on 2026-10-24. AVDs, iOS simulators, physical iOS
devices, the Android keyboard, clipboard, and device frames.

### AVD targets

Every installed AVD is a target, booted or not, with the id `avd:<name>`.
They are read from `~/.android/avd/*.ini` and each `config.ini`, which gives
the OS, API level and form without booting anything. Not `avdmanager`: on
this machine it fails to load Pixel_10, because its device table stops at the
Pixel 9.

**A running emulator is its AVD, matched by name, never by port.**
`AVDResolver` reads `ro.boot.qemu.avd_name` once per adb connection (serial
plus `transport_id`), and `SyncDevices` attaches that serial to the AVD's
target instead of listing it. So an emulator started from Android Studio is
the same target as one this server booted. The old code gave the i-th `-avd`
the port `5554+i*2` and streamed whatever was on it: with Pixel_10 already on
5554, `-avd acmechat_blockstore` would have shown Pixel_10 under that name.
A boot now picks a free even console port in 5554-5584 and checks the name
before attaching.

While it runs, an AVD delegates every capability to an `ADBTarget` on its
serial (`emulator_attached.go`). Before that change, an emulator booted
through `-avd` answered 501 to `/api/ui`, `/api/input`, screenshots and
frames, while the same emulator found through adb had all of them. While it
is not running, each one fails with "open it in the viewer to boot it".

The serial still works as an id. `Manager.canonical` maps a running AVD's
serial to its target for `Acquire`, `Target` and `Session`, and
`/api/targets` reports it as `serial`, which `chooseTarget` matches, so a page
that saved `emulator-5554` does not fall back to the Acme One. The alias lasts
only while the AVD runs.

The emulator binary comes from the SDK (`-android-sdk`, `ANDROID_HOME`,
`ANDROID_SDK_ROOT`, then `~/Library/Android/sdk`), because launchd's PATH
does not reach it and a bare `emulator` could not start anything under the
supervisor. It is started in its own session, so the supervisor restarting on
every build leaves it running, and the next process attaches by name. Only the
picker's Shut Down or Delete (below) stops one. `-avd <name>` now means boot that AVD at startup.

**Shut Down and Delete stop an AVD, and Delete removes it.** The picker's
swipe actions call `POST /api/devices/shutdown?target=avd:<name>` and
`DELETE /api/devices?target=avd:<name>` (see Agent API). Stop sends
`adb -s <serial> emu kill` and falls back to SIGTERM on the pid
`runningAVDPid` finds when adb cannot reach the emulator, and returns once the
process is gone and adb has dropped the serial. Delete stops first, then
removes `<avdhome>/<name>.ini` and the directory its `path=` names, which takes
the `loupe-warm` snapshot with it, and `Manager.Unregister` drops the target,
its session, reaper and serial alias, since `registerAVDs` only adds. Before
ending a session the server arms a reopen guard: while the stop or delete
runs, and for `shutDownGrace` (10s) after the shutdown returns, a plain open of
that target is refused, because the viewer's reconnect would otherwise boot
the emulator that was just shut down. A failed stop or delete lifts it. A
shut-down AVD is again `bootsOnOpen` and opens from `loupe-warm`. A `deleted`
target is refused as `deleted` for a minute after `Unregister` and is unknown
after that, and an explicit `start=1` open lifts only a `shutDown` guard, never
a `deleted` one, and not while a shutdown or delete of that target is still
running. `Unregister` also marks the id gone, so a simctl listing or AVD scan
that began before the delete cannot register it again
(`Manager.RegisterScanned`, `SyncSimulatorsAt`).

Measured through a spare server on 15 October, on a throwaway `loupe_swipe`
(pixel_6, android-34 google_apis_playstore arm64): the first boot was 21.0s
(`tookMs`) with the `loupe-warm` save 2.4s more, and the next attach took the
first frame 4.6s after `wtprobe` began. A shutdown with `wtprobe` attached
answered 200 in 1.6s (`tookMs` 1294), `pgrep -f loupe_swipe` was empty 3s
later, and the trace has the viewer's read ending with `err:"shutDown"`.
`/api/targets` read `bootsOnOpen` true straight after and 10s and 20s later.
`DELETE` answered 204 in 1.8s (`tookMs` 1745), `~/.android/avd/loupe_swipe*`
was gone and the row with it.

**A wedged adb server looks like every AVD being shut down.** On 2026-09-27
the server accepted connections and reset them, track-devices failed 2304
times, Pixel_10 was never attached, and every open tried to boot a second
Pixel_10, which failed with an error about `-gpu host`. Three things now
cover that:

- Before booting, `runningAVDPid` reads the process table for a
  `qemu-system-*` or `emulator` with `@<name>` or `-avd <name>`, and refuses
  with "already running (pid N) but adb cannot reach it". Not the AVD's lock
  files: `hardware-qemu.ini.lock` holds qemu's pid, but both locks survive a
  crash.
- `target.ADBHealth` counts track-devices runs that end without a list.
  After 3 in a row (about 3s of backoff) the manager marks every adb device
  and every AVD without a live session unavailable, state `adbUnreachable`,
  with adb's own stderr in the reason, traced as `kind:"adb"`,
  `state:"unreachable"`. A session already streaming is left alone.
- After 6 (about 30s), loupe restarts the adb server itself, at most once
  every 10 minutes: `adb kill-server`, then SIGTERM to whatever is still
  listening on 5037 if its command is `adb` (the wedged server resets
  kill-server's connection too), then `adb start-server`. Traced as
  `action:"restart"` with `killedPid` and `tookMs`. This drops Android
  Studio's adb connection for a second or two; it reconnects on its own, and
  the person who could restart adb by hand is usually at the phone, not the
  Mac. A binary that fails to start does not count towards a restart.

The restart has only run under the unit tests, not against a wedged server.

**A shut-down AVD opens from loupe's own snapshot, `loupe-warm`.** The
emulator was always started with `-no-snapshot-save`, and nothing shuts one
down gracefully, so quickboot never had anything to load: on
api34_emoji_check `snapshots/default_boot` was an 11-byte `snapshot.pb` and
nothing else, and an open was a cold boot, CONNECT 12.45s and first frame
12.82s through `wtprobe`. Now a boot this server starts loads `loupe-warm`
when `<avd>/snapshots/loupe-warm` holds both `snapshot.pb` and `ram.bin`.
When it does not, or the emulator's log lacks "Successfully loaded snapshot
'loupe-warm'", loupe saves one with `adb emu avd snapshot save` once
`sys.boot_completed` reads 1, before scrcpy starts: a snapshot with a scrcpy
server inside would restore it with its socket gone. The log check is for a
snapshot the emulator skips for a cold boot without failing, which a new
system image or another `-gpu` should cause; that case has not been provoked
here. The emulator's output
goes to `~/Library/Caches/loupe/emulator-<avd>.log`, a file rather than a
pipe because the emulator outlives the server. Each boot is traced as
`kind:"emulator"`, `action:"boot"`, with `snapshot` (`loaded`, `saved`,
`saveFailed` or `cold`), `tookMs` and `saveMs`.

Measured on a throwaway AVD, `loupe_bench` (pixel_6, android-34
google_apis_playstore arm64, 1080x2400), through a spare server and
`wtprobe -connect 4m`. It was deleted afterwards; `avdmanager create avd -n
loupe_bench -k "system-images;android-34;google_apis_playstore;arm64-v8a" -d
pixel_6` makes another. "Moved aside" is the `-no-snapshot-load` case:

| Open | Boot | CONNECT | First frame |
| --- | --- | --- | --- |
| First boot ever, then save (1.2s) | 19.7s | 22.2s | 22.7s |
| Snapshot moved aside, then save (2.6s) | 11.4s | 14.6s | 15.9s |
| `loupe-warm`, 5 runs, load average about 6 | 1.71-1.89s | 2.22-2.45s | 3.29-3.83s |

Under an `xcodebuild test` booting a simulator, load average 122, the same
warm open took up to 8.5s, so the 5s target holds on a quiet Mac only.
`WaitForBoot` polls every 100ms rather than 500ms, which was a quarter of a
warm boot. About 0.6s goes between `wt-attempt` and the emulator starting,
not yet broken down: `findRunning` reads `ro.boot.qemu.avd_name` from every
running emulator, `runningAVDPid` runs `ps`, and `reservePort` lists adb. What the restore
shows is the Home Screen with the current clock.

Loupe waits for `sys.boot_completed` rather than streaming the boot. From a
snapshot the property is restored as 1, so the wait is only adb attaching,
and a cold boot has no display for scrcpy until late in the boot anyway.

**An AVD this server boots runs with its window hidden**, `-qt-hide-window
-no-audio`, since scrcpy reads the guest display over adb and the stream
carries no audio. It used to be `-no-window`, and that cost frames: it starts
`qemu-system-aarch64-headless`, which under continuous `adb shell input swipe`
on a throwaway `loupe_fps` (pixel_6, android-36 google_apis_playstore)
streamed 22-44fps through `wtprobe` over two runs, pts p50 17-33ms, against
53-55fps and p50 16.7ms from the windowed binary, and Pixel_10 under the
launchd server read 27fps, p50 33.3ms. With the window hidden, Pixel_10
under the launchd server read p50 16.8-17.8ms over two 12s runs of Settings
scrolled by `input swipe`, load average 20-31; the frame counts (288 and
432) include the pauses between swipes. In the guest, `dumpsys gfxinfo` put
the missed frames on slow issue draw commands, the host GPU path, not the
app. `-qt-hide-window` runs the windowed binary with no window: `lsappinfo`
lists it as a `UIElement`, so it has no Dock tile, a `loupe-warm` saved by the
headless binary loaded from it in 785ms, it idled at 9-12% CPU and paused to
0.1%. `HeadlessAVDs` counts it as headless, so the idle pause and shutdown
still apply. `emulatorWindow` (config file, `/api/config`, `-emulator-window`)
shows the window, and applies as `nextBoot`: the console has no command to
show a hidden window or open one, so a running emulator keeps what it started
with. An emulator started from Android Studio keeps its window. The earlier
headless measurement on `loupe_headless` (pixel_6, android-34) read 14% CPU
idle headless against 49-93% windowed and 78% against 132-149% streaming
swipes "at the same frame count"; the frame rate in that run was not
recorded, and the runs above say the headless one was not 60.

**A headless emulator nobody has used for five minutes is paused.**
`adb emu avd stop` stops the VM and `emu avd start` resumes it; both go to
the emulator console, not adbd, so they answer while the guest is stopped,
as `emu avd name`, `status` and `kill` do. Anything through `adb shell` hangs
until the resume, and adb keeps listing the serial as `device`, so nothing in
track-devices says it is paused. The manager checks every 30s
(`internal/httpapi/emupause.go`) and pauses an attached AVD that runs
headless (`headlessAVDs`), has no session (so no viewer and no recording),
holds no logcat follow, and has had no call reach it for `emulatorPauseSec`
(300 by default, 0 never pauses; config file, `/api/config`,
`-emulator-pause`; applies `now`). A windowed emulator is never paused,
since it may be on someone's screen. Every delegate goes through
`running()`, which resumes first, and so do `Open` and `findRunning`; a
claim reaches no device and wakes nothing. `/api/targets` never resumes one:
a paused AVD reports `paused: true` and the description `config.ini` gives,
without the Android version getprop would. After a restart the console's
`avd status` says whether a previous process left it paused, and
`AVDResolver` names an emulator whose getprop fails through `emu avd name`,
without which a paused emulator would never be attached and an open would
boot a second copy. Only loupe resumes one: Android Studio's Run, `adb install` or `adb shell`
from a terminal hangs on a paused emulator until a loupe call wakes it.
The reverse is covered for delegated calls and Open: `wake` asks the
console's `avd status` first unless loupe already knows the VM is paused,
one console round trip (unmeasured; `avd stop` and `avd start` took
10-14ms), so a VM stopped from a terminal with `adb emu avd stop` is
resumed by the next such call instead of hanging it. That resume carries
`found: true`. `/api/targets` does not go through `wake`, and still reads
such a VM as running until one of those calls.
Each change is traced as `kind:"emulator"`,
`action:"pause"` or `"resume"`, with `tookMs`.

Measured on a throwaway `loupe_pause` (pixel_6, android-34
google_apis_playstore) through a spare server with `-emulator-pause 30`:
the pause landed on the second check, 60s after start, and took 12-14ms;
qemu went from 3.5% to 0.3% CPU. A resume took 10-11ms. While paused, an
`/api/input` tap answered 200 in 87ms, a screenshot in 0.53-0.67s, and a
`wtprobe` open had CONNECT in 24ms and its first frame at 537ms. One tap
arrived as the check paused it and was resumed 130ms later. After a server
restart with it paused, three listings took 47-49ms each and read `paused`.
A shutdown of it while paused answered 200 in 1.5s and the process was
gone. A delegated call holds the emulator until it returns, as a logcat
follow does, so a pause cannot land under an install or an `adb shell`
that outlasts the idle window (`TestInFlightCallHoldsOffAPause`).

Under the launchd server on 22 October, acmechat_blockstore and Pixel_10
paused at 07:31, five minutes after the deploy, in 22ms and 14ms, and qemu
read 0.3-0.4% CPU. A `/api/logcat?follow=1` on acmechat_blockstore from
07:35 kept it running while Pixel_10, resumed at 07:34, paused at 07:40; it paused at 07:44, after the follow closed.
A pause frees no memory outright: `footprint` stays about 2.9GB, because
it counts compressed pages. macOS does compress the stopped guest's RAM
(compressed memory rose 3.4GB to 5.2GB within 2 minutes on `loupe_pause`),
so physical pressure drops, and only a shutdown returns it all.

**A headless emulator nobody has used for half an hour is shut down.**
The same 30s pass, with the same rules for which emulators qualify,
stops one idle for `emulatorShutdownSec` (1800 by default, 0 never; config
file, `/api/config`, `-emulator-shutdown`; applies `now`). Both settings
count from the last use, so a paused emulator keeps ageing and is killed
without a resume, since `emu kill` reaches the console of a stopped VM.
The pass claims the target the way the picker's Shut Down does, so a Shut
Down during an idle stop answers 409 `busy` (the busy set moved from
`Server` to `Manager` for that), and it arms the reopen guard while the
kill runs, so an open in that window is refused instead of booting into
it. The guard is lifted the moment the stop returns, with no
`shutDownGrace`: that grace turns away a viewer reconnecting after a
shutdown, and an idle emulator had no viewer. The next open boots it from
`loupe-warm`, which also means **whatever changed since it booted is gone**,
an `/api/install` included, unless `emulatorKeepState` is on (see below).
Each stop is traced as `kind:"emulator"`, `action:"shutdown"`, with
`idleMs`, `tookMs`, `paused` and `saved`, apart from the picker's
`kind:"agent"` shutdown.

Measured on a throwaway `loupe_pause` (pixel_6, android-34
google_apis_playstore) through a spare server with `-emulator-pause 30
-emulator-shutdown 90` and its own `ANDROID_AVD_HOME`, so it could not see
the AVDs the launchd server was using: a pause in 12ms, then a shutdown at
`idleMs` 90066, `paused` true, in 1741ms. A `wtprobe` open 4.5s after it was
not refused and booted from `loupe-warm` (`snapshot: loaded`, 1846ms).

**`/wt` answers CONNECT before it opens the target, so the boot happens
inside the session.** The client gives the WebTransport handshake 6s before
falling back to WebSocket, and answering only once the AVD was up sent
every slower boot there: acmechat_blockstore on 7 October and
Hinge_Pixel_Fold on 9 October both stalled "at ready" with `wt-attempt`
logged. Now the session opens, the control stream is accepted, and then
the target is acquired; a target that cannot open closes the session with
code 1 and the error as its message, which `wtprobe` prints. The viewer
keeps its overlay and its backoff until init rather than until the
transport opens. Through a spare server on Hinge_Pixel_Fold from
`loupe-warm` at load average 27: CONNECT 72ms, boot 5.2s, first frame
6.08s, which the old order would have sent to WebSocket. So the CONNECT
column in the table above includes the boot, and a `wtprobe` CONNECT time
no longer does.

**By default every boot from the snapshot discards what the last session
changed.** `-no-snapshot-save` stays, so the disk reverts with the RAM: a file
written to `/sdcard/Download` was gone after the next warm boot, and an
`/api/install` lasts only while that emulator process runs. The snapshot also
costs 1.7-2GB in each AVD directory loupe has cold-booted.

**`emulatorKeepState` saves over `loupe-warm` before a shutdown**, the idle
one and the picker's Shut Down, so the next open resumes where the last one
left off. It is off by default (config file, `/api/config`,
`-emulator-keep-state`; applies `now`, read at each shutdown). Delete never
saves. The save is `adb emu avd snapshot save`, which goes to the console,
so a paused emulator is saved without a resume. A running one is saved only
after its scrcpy server has exited, at most 3s (`scrcpyGone`), because a
snapshot with a scrcpy server inside restores it with its socket gone. A
failed save is traced and the shutdown goes on. Each save is traced as
`kind:"emulator"`, `action:"save"`, with `tookMs`, `waitMs` for the wait on
scrcpy, and `lingered` true when the 3s ran out with one still there.

Measured on a throwaway `loupe_keep` (pixel_6, android-34
google_apis_playstore) through a spare server with `-emulator-keep-state
-emulator-pause 30 -emulator-shutdown 90` and its own `ANDROID_AVD_HOME`, at
load average 20-40: the idle shutdown found it paused, saved in 4.45s and
stopped 1.1s later; a file written to `/sdcard/Download` before the pause was
there after the next boot, which loaded `loupe-warm` in 2.2s. A picker Shut
Down with `wtprobe` attached saved in 4.4s (`tookMs` 5867 for the whole
shutdown), and the reopen streamed, first frame 4.7s after `wtprobe` began. That run predates `lingered`, so
whether its save waited the scrcpy server out is not known, and a reopen
that streams does not settle it: a restored server with a dead socket would
exit and a fresh one start in its place.

**Reset goes back to a clean device.** `POST /api/devices/reset?target=avd:`
stops the emulator without saving, removes `loupe-warm` and leaves a
`loupe-wipe` file in the AVD's directory, so the next boot is a cold one with
`-wipe-data` and `keepWarm` saves a fresh snapshot from it. The file is
removed once that boot is up, so a boot that fails tries again. Removing the
snapshot alone is not a reset: once a kept state has been saved over
`loupe-warm` there is no clean snapshot left, and a cold boot reads the disk
as the last session left it. On `loupe_keep` the file in `/sdcard/Download`
survived a cold boot after only the snapshot was removed. Wiping means
clean is the system image's own data, as Android Studio's Wipe Data is, so
anything set up by hand after the first boot goes too. On `loupe_keep`
after a reset, `/api/devices/boot` cold-booted it with `-wipe-data` in 39s
at load average about 60, the file was gone, `loupe-wipe` was removed and a
fresh `loupe-warm` saved in 1.3s.

Listing, attaching to the running Pixel_10, the serial alias and the
delegated screenshot were checked against a live server on a spare port.

**New device… creates from what is installed, choosing OS, device and name.**
`GET /api/devices/options` lists the platforms that can create, each with
`os`, `devices` and `pairs` (which devices an OS takes), and
`POST /api/devices {platform, os, device, name}` answers 202 with the row id
(`avd:<name>`, or `sim-new:<name>` for a simulator, which has no UDID yet).
Nothing is downloaded: an OS is an image or runtime already on the Mac, and a
platform with none is listed with `os: []`. Android is offered when
`avdmanager` is found (`-android-avdmanager`, then `cmdline-tools/*/bin` in the
SDK, `latest` first); iOS when `simctl` lists simulators and simcap runs,
whether or not device discovery is on, since a simulator the picker would not
show is not worth making. The `android` CLI below is the fallback: with no
avdmanager, `profilesFallback` is true and the client uses `/api/avds/*`,
which behave as before. `/api/avds` and `/api/devices` share one slot, so a
create on either answers 409 `creating` while the other runs, and 409
`avdExists` now means a device of that name exists on that platform. The
trace is `kind:"agent"`, `action:"create"`, with `platform`, `name`, `tookMs`
and `outcome`.

Measured by running the tools, on this Mac:

- **avdmanager follows `ANDROID_AVD_HOME`**, then `$ANDROID_USER_HOME/avd`,
  then `~/.android/avd`, unlike the CLI. So it has no check against
  `AVDHome()`; loupe passes `ANDROID_AVD_HOME=<AVDHome()>` to be sure. It
  runs without `JAVA_HOME`.
- **`-d <device>` is required and stdin is `/dev/null`.** Without `-d` it asks
  for a custom hardware profile, and with a closed stdin that fails with
  `Range [0, 0 + -1) out of bounds`. The command is `avdmanager create avd -n
  <name> -k <image> -d <device> [--skin <device>]`; `--skin` is passed when
  `$SDK/skins/<device>` exists, which is what gives `/api/frame` a skin. No
  `--force`.
- **The exit status is reliable here**, unlike the CLI's: 0, or 1 for any
  error including a duplicate name (`Error: Android Virtual Device 'x' already
  exists.`, then a stray `null` line). The message is the `Error:` lines with
  the `\r` progress bars, the "SDK XML versions up to 3" warning and `Auto-
  selecting single ABI` removed. A create took 0.7-0.9s.
- **It checks nothing about the pair.** It made a TV or a Wear device on a
  phone image. So the server filters: a device with a `tag-id` (TV, Wear,
  automotive, desktop) is never offered, nor an image whose first tag starts
  `android-`, nor a device on an image older than its `api-level` floor
  (`pixel_fold` is `34-`, `pixel_9` `35-`), and a `*tablet*` image takes
  tablets only. Those floors mean API 34 offers no Pixel 9; whether Android
  Studio does is not checked.
- **Devices come from `com/android/sdklib/devices/{nexus,devices}.xml` inside
  `cmdline-tools/*/lib/sdklib/sdklib.core.jar`**, read once per process (8ms,
  31 devices here). A foldable is a `hinge` or `foldable-region`, decided
  before size since `pixel_fold` is 7.58in; a tablet is 7.0in or more
  (`TabletDiagonalIn`: phones run to 6.9in and `nexus_7` is 7.0in). Legacy ids
  that are not `^[a-z0-9_]+$` (`7.6in Foldable`) are dropped.
- **Images are read per request** from `$SDK/system-images/android-*/<tag>/<abi>/source.properties`,
  for the host ABI. The package id is built from the directory names, not
  `SystemImage.TagId`: the API 37.0 image is in `google_apis_playstore_ps16k`
  and says `google_apis_playstore,page_size_16kb,...`. `ApiLevel` can be
  `36.1` or `37.0`. Android version names come from a table in `sysimages.go`
  and an API not in it reads `API <n>`.
- **A created `config.ini` lacks what Studio writes**, so after a success
  loupe sets `hw.keyboard=yes`, `hw.gpu.enabled=yes`,
  `avd.ini.displayname=<name>` and, for a `playstore` image,
  `PlayStore.enabled=true`. The write is atomic (temp file, fsync, rename); a
  failure leaves the create a success and shows as `warning` on the job and
  `patchError` in the trace. That a patched AVD then scans with the right
  level and form (`target=android-36.1` and `image.sysdir.1=.../android-36.1/...`
  included) is covered by a fixture test (`TestScanAVDsReadsAvdmanagerOutput`),
  not by a real create, which has not been run.
- **The SDK's avdmanager has no Pixel 10 definition** (its `nexus.xml` has no
  `pixel_10`), so New device… cannot make one; Studio's Pixel_10 came from
  elsewhere.
- **Android names** match `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$` (no leading `-`
  or `.`, which `-n` and `emulator -avd` could read as a flag) and collide
  without case, as avdmanager's `equalsIgnoreCase` does.
- **simctl allows duplicate names**, `/`, emoji and the empty name, so loupe
  holds its own rule (`^[A-Za-z0-9][A-Za-z0-9 ._()-]{0,63}$`) and compares
  against the simulators the manager lists, without case. A create printed the
  UDID and took 0.2-0.3s. A failure puts its main message on **stdout**
  (`Unable to create a device for device type: ...`, exit 147 for an
  incompatible pair) and the cause on stderr (`code=403 ... Incompatible
  device`); both are kept. Pairs come only from each runtime's
  `supportedDeviceTypes`: iOS 27.1 supports exactly one, iPhone Duo, so
  iPhone 17 with 27.1 is refused.
- **The new simulator is registered at once** (`ListSimulators`, then
  `SyncSimulators`), since the 5s poll has no kick, and the job reports
  `createdId` `sim:<udid>`.
- Sizes come from each device type's `capabilities.plist` through plutil: the
  diagonal is the display's width and height over its pitch (iPhone 17: 1206x2622 at
  460, 6.3in). A Duo has two integrated panels with digitizers, so it is a
  foldable and shows its larger, upright inner panel (2853x2007). 72 types took
  47ms after a 132ms `simctl list`, then are cached for the process.

Not run against a live server or a phone: only the parsers were run against
this Mac's real SDK and Xcode, and the create paths against fakes.

**With no avdmanager, New device… creates an AVD through Google's `android` CLI.**
`GET /api/avds/profiles` lists `android emulator create --list-profiles`
(`small_phone`, `medium_phone`, `medium_tablet` and three desktops, 0.4s,
cached per process), each with `exists`. `POST /api/avds {"profile"}`
answers 202 and runs `android emulator create --sdk=<sdk> <profile>` in the
background. The CLI is found by `-android-cli`, then `/usr/local/bin` and
`/opt/homebrew/bin`, since launchd's PATH reaches neither; it is a static
binary, and under launchd's bare PATH it behaved as from a shell. Without
it, both endpoints answer 404 and the picker hides the entry.

What the CLI does was learned by running it, not from its docs, which say
none of it:

- **The AVD is named after the profile and there is no name flag**, so a
  profile makes one AVD. A second create of it prints `Error: AVD
  small_phone already exists`, so a profile whose `avd:<name>` is already a
  target is dimmed in the sheet and answers 409 `avdExists` before anything
  runs. One create runs at a time; another answers 409 `creating`, since
  two would share a download.
- **It exits 0 on that error.** The verdict is no `Error:` line and the
  AVD's `config.ini` appearing, never the exit status.
- **The first create downloaded the API 36.1 Play Store image**, 2.3 GB,
  in about 4.6 minutes, though two creates overlapped for most of it.
  Without a terminal it printed one URL line for all of that, so progress
  is read from the filesystem: the row says `Preparing` for 15s, then
  `Getting the system image` until the `.avd` directory appears, then
  `Creating the device`. With the image already here, `medium_phone` took
  0.44s, so a create that has nothing to download finishes inside the
  `Preparing` window and never claims a download.
- **It ignores `ANDROID_AVD_HOME`.** With that set to one scratch directory
  and `ANDROID_USER_HOME` to another, the AVD landed in the second, under
  `avd/`. The emulator and loupe's scan read `ANDROID_AVD_HOME` first, so
  with it set elsewhere a created AVD would never appear; loupe then offers
  no create and says so at startup. A fresh `ANDROID_USER_HOME` also costs
  the CLI about 12s and 246 MB installing itself there on first use.

While it runs, `/api/targets` carries a row for `avd:<profile>` with state
`creating`; a failure leaves state `createFailed` with the CLI's error
until the next create. On success the server rescans the AVD directory and
registers each AVD not already a target, so the new one is in the picker
without a restart. Before this the AVD list was read only at startup, which
also kept an AVD made in Android Studio out until one. The success path,
the failure row and the 409 were checked against a fake CLI on a spare
server.

**No devices cannot appear against a real server.** It needs an empty
`/api/targets`, and a server that registers nothing at startup registers
the synthetic pattern instead (`main.go`, `registered == 0`). Its New
device… and How to connect a phone buttons were seen through a proxy that
answered `/api/targets` with `[]`, in front of a spare server with an adb
that lists nothing and an empty `ANDROID_USER_HOME`.

### iOS simulator targets

Every available iOS simulator is a target, booted or not, with the id
`sim:<udid>`, streamed through `simcap`. simcap writes scrcpy's video
wire format, so `simStream` reads it with the parser a device uses and nothing
above `internal/target/simulator.go` knows the frames came from CoreSimulator.

**Simulators are listed only when simcap is built** (`make simcap`, into
`.build/simcap`, which `-simcap` can move). A simulator in the picker that
fails every open with "simcap is not built" is a target the viewer can see and
cannot watch, which is worse than its absence. simcap stays out of
`make build` for the reason in Verification discipline.

`simctl` has no equivalent of `adb track-devices`, so it is polled every 5s,
about 0.24s a listing. A failed listing keeps the last one rather than marking
every simulator missing. `SyncSimulators` owns only simulators and
`SyncDevices` only adb targets, so neither marks the other's targets missing.
Only iOS runtimes are listed: simcap has run against an iPhone and nothing
else.

Opening one boots it if needed. Shut Down and Delete from the picker stop one
and remove one, below.

**`simulatorSlim` boots a simulator with 168 background daemons disabled**
(`internal/target/simslim.go`, label list copied from simslim under its MIT
notice). On loupe-probe (iPhone 17 Pro, iOS 27.0) a slim boot ran 80 processes
against 215 and summed to a 2804 MB footprint against 8888 MB. Idle CPU 90s
after boot was 2% of a core against 26-160%, though both settle near 1% within
five minutes. Stream frame rate and lateness did not change, so this is a
memory setting, not a smoothness one.

- The overrides live in a host file,
  `/private/var/tmp/com.apple.CoreSimulator.SimDevice.<udid>/disabled.plist`,
  not in the device's data directory. `launchd_sim` reads it only at boot, so
  the setting reaches only boots loupe performs: a simulator already booted,
  or booted from Xcode, keeps whatever it last booted with. The file survives
  `simctl erase`, not `simctl clone`.
- iOS 18.5 and newer read it. Older runtimes are left alone.
- `loupe-slim.txt` beside it lists the labels loupe disabled. Turning the
  setting off enables only those at the next boot, so overrides set another
  way, simslim's included, stay. With the setting off and no marker, loupe
  never writes the file.
- Each slim boot traces `{"kind":"simulator","action":"slim"}` with `wanted`
  and `honoured`, the second read back through `launchctl print-disabled`
  once the simulator is up. The file is undocumented, so that count is the
  only evidence a slim boot took.

**Shut Down and Delete on a simulator.** Stop is `xcrun simctl shutdown
<udid>` (input process closed first, and a Stop that races a boot is
guarded) and Delete is `simctl delete <udid>` after a stop. The reopen guard
is the AVD's: refused while the stop runs and for 10s of `shutDownGrace`
after it returns, a `deleted` target refused, and `start=1` lifting only a
`shutDown` guard. After either
the simulator list is synced at once, and a delete unregisters the target so
it does not stay as a dimmed `missing` row. Through the same spare server on a
throwaway `loupe_swipe` (iPhone 17, iOS 27.0): a shutdown with `wtprobe`
attached answered 200 in 3.3s (`tookMs` 3297), `simctl list` read it Shutdown
and `bootsOnOpen` was true at once and 10s later. A plain open 22s after it,
past the guard, booted it again. A `DELETE` of it while it was running
answered 204 in 3.4s (`tookMs` 3364), and `simctl list devices` no longer
listed it.

Not run: the swipe itself in any browser (the built-in browser's navigation to
the spare server's self-signed `https://127.0.0.1` address was denied or
failed, so the gesture, the overlay and `__loupeLayoutCheck()` with the new
screen are covered by node tests only), Safari on a real iPhone, VoiceOver, a
shutdown or delete over WebSocket, `wtprobe` printing the close code (it prints
one only when no frame arrived, so code 3 is read from the trace's
`viewer-stop` `err:"shutDown"` and the unit tests), and shutting down an
emulator through the pid fallback with adb wedged.

**An iPhone Duo streams whichever panel is lit.** It has an outer panel,
1398x2034 (screen ID 1), and an inner one, 2007x2853 (screen ID 3), and
simcap used to take the larger, so a folded Duo streamed black. simcap
polls CoreDevice's `displayInfo` every 500ms for the display that is both
`active` and backlit, and a fold rebuilds the encoder at the new size. The
inner panel declares `nativeRotation` 270 in the device type's
`capabilities.plist`: its framebuffer is portrait and its Home Screen is
landscape, so simcap turns it upright with a `VTPixelRotationSession` and
streams 2853x2007. Touches go to that panel's digitizer target (`display3`
rather than `mainScreen`, which is the outer panel) after mapping the
upright point back into the framebuffer. The accessibility tree's frames
are already upright, but its root reports the outer panel's size, so the
root is resized to the lit panel. `/api/screenshot` passes simcap's
`display` answer to `simctl io screenshot --display`. Checked on
`loupe-duo`, unfolded, through a spare server: the screenshot, `/api/ui`
centres, an `/api/input` tap on About and a `wtprobe -tap` on Back. Folded,
a tap opened Settings.

**simcap folds a Duo the way Device Hub's hinge slider does.** Neither
`simctl` nor `devicectl` in Xcode 27.1 can. Device Hub's CoreDevice plugin
sends a vendor-defined HID report, page 0xff61, usage 0x5b, version 0,
whose payload is a dictionary in `IOCFSerialize`'s binary form: `provider`
`com.apple.Virtualization.VirtualMachines`, `source` `hinge-slider-control`,
`type` `range` and `value` the angle, 0 to 180. Those were read out of the
plugin binary, not a header. simcap's `hinge <degrees>` sends the same
report through CoreDevice's `vendorDefined` capability, and `posture` and
`panels` read it back. On a simulator with two panels `/api/settings`
offers `posture`, `closed` or `opened`, and the PUT returns once the lit
panel has followed; every other simulator still answers 501. A fold took
2.4s and an unfold 1.0s from the viewer's fold button on `loupe-duo`.

A CoreDevice lookup sometimes never returns: one input process under the
server read posture `none` for its whole life while fresh ones beside it
answered in under a second, with the load average at 370. So the display
watcher retries the lookup every 5s, and a hinge on the legacy route opens
the vendor capability by itself, with ten seconds to do it.

**Input goes through a second simcap, `simcap <udid> -input`**, one per
simulator and apart from the video process, so the stream's touches and keys
and `/api/input` share one HID client, input works with no session open, and a
video relaunch never takes input down. It reads one command per line on stdin
and answers every one with `ok` or `err <why>`, so a tap that failed is an
error rather than a silence. Coordinates are framebuffer pixels, the space
`/api/screenshot` and `/api/ui` use. It stops after two minutes idle and
restarts on the next command.

**The video honours `maxSize`, so the stream is smaller than the
framebuffer.** simcap took no size and streamed loupe-probe at its full
1206x2622 while an Android device was capped at 1080, so a simulator put about
3.4 times the bytes on the same link: the same few seconds captured directly
were 1.64MB native and 0.48MB at 496x1080, both at 8 Mbps. `-maxsize` scales
the longer side, keeping both sides even for the encoder. Stream touches
arrive in stream pixels, so `simInput` scales them up by the framebuffer size
it reads once with `size`; `/api/input` stays in framebuffer pixels. Checked
on `loupe-probe` through a spare server: a `wtprobe -tap` at Settings' centre
in 496x1080 stream pixels opened Settings.

The HID calls are private SimulatorKit, and each signature in `input.swift` is
the one that was run on Xcode 27, not one read from a header: touch is the
nine-argument `IndigoHIDMessageForMouseNSEvent` aimed at target `0x32`, which
went 10 for 10 on open-and-close taps where the IOHIDEvent digitizer recipe
baguette uses missed 3 of 16. Buttons are `IndigoHIDMessageForButton`, home 0
and lock 1, and the app switcher is two Home presses 150ms apart. Keys are
`IndigoHIDMessageForHIDArbitrary` on the keyboard page. Each message took
1-3ms. The builders run on simcap's main thread: the mouse one reads AppKit's
per-thread state, and off the main thread the simulator drops what it builds.

Verified on `loupe-probe` through the server: `/api/input` taps, a swipe
between Home Screen pages both ways, "Loupe? 42" typed into Search, home,
lock and the app switcher; a tap sent over a live WebTransport session with
`wtprobe -tap`; and in the viewer, the nav L's Home and a click on the canvas
opening Photos. The legacy HID call carries a single point, so on that route
a second finger is dropped rather than turned into a jump of the first.

**Two fingers need the CoreDevice route.** Its digitizer takes a second point
in the same event, and it accepts a gesture that starts with one point and
continues with two, and the reverse. A human's fingers never land at the same
moment, so the stream path relies on this: the first finger goes down as a
one-point touch, the second turns later moves into `touch2`, and lifting
either one returns to one point. That sequence zoomed a photo in Photos on
`loupe-probe`. `/api/input {"action":"pinch","x","y","scale"}` runs the same
gesture as simcap's `pinch` command, spreading between 30% and 60% of the
screen's short side for a scale of 2. Photos zoomed and unzoomed through the
server both ways. Nothing has yet sent two fingers from a real viewer on a
phone. There is no rotation yet.

**`/api/ui` on a simulator is `AXPTranslator`**, the private translator
Xcode's Accessibility Inspector uses, reached through the same input process
as a `ui` command. It covers the frontmost app, SpringBoard on the Home
Screen. Frames arrive in points and `parseAXTree` scales them to framebuffer
pixels by the root's width, 3 on an iPhone 17 (1206 over 402), so a node's
`center` goes straight into `/api/input`. That was checked on `loupe-probe`
by tapping the reported centres of Settings on the Home Screen and then of
General inside it. The label maps to `text`; a text field's contents or
placeholder go in a new `value` field, which Android never sets.

**The tree holds what is covered, and `occluded` catches only part of it.**
With Settings' search open, the root list is still in the tree underneath,
and tapping its General row's centre opened Privacy & Security from the
results on top. simcap hit-tests each node's centre and marks it `occluded`
when another element answers there, or when the centre is off the screen,
and `?interactive=1` drops occluded nodes. It is one-way: the hit test
follows the accessibility tree rather than the pixels, so blank space drawn
over a row lets the hit through to the row. On that screen General and
Accessibility, under result rows, were marked, and Appearance, under the
blank area, was not. On the Settings root and the Home Screen nothing was
marked but StandBy, whose centre is under the floating search bar.

A dump took 50-70ms on the Home Screen and 0.54-0.77s on the Settings root
through the server, most of it the hit tests' parent walks, and 1-4.6s for
the first dump from a fresh input process.

**Device Hub takes buttons and keys away.** Xcode 27 replaced Simulator.app
with Device Hub, which runs `dtuhidd` inside each simulator it sees and sets
`com.apple.coredevice.dtuhidd.active`. While that is set, backboardd ignores
the legacy HID client for buttons and keys: Home is answered `ok` and does
nothing, so the flag is the only way to know. Touch got through a hold on
`loupe-probe`, which baguette describes as a boot-order race.

**Only a backboardd restart takes them back.** Clearing the flag, posting it,
and killing `dtuhidd` each left Home doing nothing with Settings in front,
because backboardd picks its input owner once, at startup. So `Reclaim`
clears the flag, restarts backboardd, waits for a new SpringBoard, and
relaunches the app that was in front (simcap's `front` names its pid,
`launchctl list` its bundle). That took 9.1s on `loupe-probe` and reopened
Settings. What the app held in memory is lost, and the other apps close. It
holds until Device Hub is relaunched.

That is now the fallback. It is automatic on a **claimed** simulator -- one
this server booted or has reclaimed once -- where a button or key that finds
the flag set reclaims and then goes through (11.7s for that Home press). On
any other it answers 409 with `code: "hubHeld"`, since someone else may be
using it, and the viewer asks before sending
`/api/input {"action":"reclaim"}`. Each reclaim is traced as
`kind:"agent"`, `action:"reclaimed"`, with `tookMs` and `reopened`, so a
ten-second press has its reason beside it.

**The phone's keyboard replaces the simulator's through a hardware keyboard.**
Typing HID keys does not hide the on-screen keyboard: with "ab" typed into
Home Screen Search on `loupe-probe`, iOS kept its keyboard over the lower half.
SimDevice's `setHardwareKeyboardEnabled:keyboardType:error:`, which is
Simulator.app's "Connect Hardware Keyboard", does. Its encoding reads
`B32@0:8B16C20^@24`, so simcap calls the IMP directly with a BOOL, a `UInt8`
type of 0 and an `NSError**`; `hwkeyboard on|off` on the input process runs
it. On, the keyboard went away with Search still focused, and off brought it
back. It is a CoreSimulator call rather than HID input, so it goes through
while Device Hub holds the keys. Nothing reads the setting back, so the
viewer sends `/api/input {"action":"hardwareKeyboard","on":true}` when its
keyboard button raises the phone's keyboard, again on each new session while
it stays up, and `off` shortly after the field loses focus.

`web/keyboard.js` raises the phone's keyboard from a transparent textarea
focused inside the menu tap, cancels each `beforeinput`, and sends text as
`MsgText` (0x16, UTF-8 up to 4096 bytes) and Enter or Backspace as key
messages, on the session connection so they stay in order with touches.
They first went as `/api/input`, and still did when that Search field took
`xy`, Backspace, `z it's` and left `abcdxz it's`. The session route skips
the focused-field read `/api/input` does to redact passwords, which is a
uiautomator dump on Android (2.1s on an emulator), so the server withholds
the text from every viewer-typed event instead and records only its length.
Nothing has been typed from an iPhone yet, so autocorrect, dictation and a
held Backspace are unmeasured there.

**CoreDevice goes around the hold, so nothing is reclaimed.** Device Hub
itself sends input through CoreDevice's HID capabilities, and simcap now
opens the same ones: `hid.button`, `hid.keyboard` and `hid.digitizer` on
the device CoreDevice lists under the simulator's UDID. `hub.swift` holds
them; simcap's `coredevice/CoreDevice.swift` is a stub interface for a
framework that ships no `.swiftmodule`, declaring only the symbols simcap
calls, each recovered from its mangled name on Xcode 27. The stub is
compiled and never linked -- the real framework, weak-linked from
`/Library/Developer/PrivateFrameworks`, is what runs. Two details were not
readable from the symbols and were found by running: `DeviceManager.shared`
lists only the two physical iPhones, so simcap builds its own manager with
`.simulators` allowed, and Home and Lock are consumer usages 0x40 and 0x30.
No entitlement is needed; `devicectl` carries none that matter either.

With `dtuhidd.active` read as 1 and nothing reclaimed on `loupe-probe`,
through `/api/input` on a server that had not booted it: Home from Settings,
a tap opening Settings, swipes between Home Screen pages both ways, and
"loupe" typed into Search, each answered 200 in 0.08-0.49s. It is all or
nothing -- touch goes through CoreDevice too when it opens, so one client
owns the simulator's input -- and a simcap that cannot open it says so on
stderr and uses the legacy client, where the reclaim above still applies.
simcap reports which in reply to `route`, which the server sends when each
input process starts, and `checkHub` does not read the flag at all on the
CoreDevice route.

**Adaptive bitrate changes a simulator's encoder in place.** VideoToolbox
takes a new `AverageBitRate` on a running session, so simcap reads `0x80`
and a big-endian u32 on stdin and sets it. `simStream` implements
`target.LiveBitRater`, and the session's `changeBitRate` uses it in place of
`Relaunch`: no second simcap, no keyframe wait and no restart. A simulator is
never relaunched for bitrate. Its controller starts only when simcap's
`-caps` lists `bitrate`, because running two simcaps side by side has not
been tried. An older build would read the command as noise, and a byte of 17
inside the u32 would force a keyframe.

`simprobe -setbitrate` sends the command halfway through a run. On
`loupe-probe` under `-animate`, 4 Mbps down to 500 kbps went from about 2.3
Mbps to 480-520 kbps within a second, at 53-60fps. The reverse went from
about 490 kbps to 1.8-2.1 Mbps. Neither run produced a new config packet.
Through `cmd/linkshaper` at 400 kbps with 60ms delay, a `wtprobe` session on
`loupe-probe` under that page went 4 Mbps to 704 kbps (`starved`), then to
the 250 kbps floor, and after the link was released stepped up to 375 kbps.
Every change was live, and goodput read the new rate within a second.
Recovery was slow: the controller waited 60s at the floor before its first
step up, because its windows were sized for a relaunch (12s to settle, 60s
before stepping up), and the six-change cap would have stopped it around
1.3-1.9 Mbps for the rest of the session.

A simulator now gets live windows: 2s to settle, 15s before stepping up, 24
changes in any 30 minutes (see Adaptive bitrate). The same run on `loupe-probe` with the link
at 400 kbps, 60ms and a 300ms queue from 15s to 60s: `starved` took 4 Mbps
to 276 kbps, a clean window stepped up to 414 kbps while the link was still
shaped, which starved again to the 250 kbps floor. The first step after the
link was released came 13s later, and seven steps of half reached 4 Mbps
116s after the release, 17s apart, every one live. That was ten changes, four
more than the old cap. The run was 13517 frames in 250s; a synthetic run from
another session shared the machine throughout.

Measured through the server with `cmd/wtprobe` on `loupe-probe` (iPhone 17,
iOS 27.0): CONNECT 6.4s from a shut-down simulator, which is the boot, and
283ms once booted. Under the `-animate` page, a fresh session delivered 565
frames in 10s, pts p50 16.8ms and p99 19.9ms, a keyframe a second, first frame
a config packet. That matches simcap run directly (54-56fps on the same page
at both 4 and 8 Mbps). One earlier run read 45fps with pts p99 93ms; it reused
a session and started 3s after Safari opened the page, and did not repeat.

### Physical iOS devices

**Every iPhone and iPad paired with this Mac is a target, `ios:<udid>`,
over a cable or the network alike, and streams over either.** They come
from `xcrun devicectl list devices --json-output`, polled every 10s
(`watchIOSDevices`); `-no-ios-devices` turns it off. devicectl keeps a
paired device listed when the Mac cannot reach it and drops its transport,
so presence is `device` with a transport (`wired` or `localNetwork`),
`offline` without one, and `missing` once devicectl stops listing it. A
change of transport is traced as `kind:"target-state"` with `transport`.
A phone on Wi-Fi whose tunnel is idle reads `disconnected` and keeps
`localNetwork`, and stays reachable, since CoreDevice reopens the tunnel
when something uses it; one out of reach reads `unavailable` with no
transport. The kind is `iosDevice`, ranked after Android devices so
`chooseTarget` never opens one by default. `Open` on a Wi-Fi phone before
iOS 27, or with a simcap that lacks `phonewifi` in `-caps`, answers that
streaming needs a cable.

`launch` goes through `devicectl device process launch`, and
`/api/screenshot` through `devicectl device capture screenshot`. Both were
run through a spare server against two iPhone 16 Pros, one on iOS 26.6 and
one on 27.2. Launching Settings took 0.31-0.82s over a cable and 0.57-1.01s
over Wi-Fi, and a screenshot about 0.53s over a cable (1206x2622, 298KB).
A bundle the device lacks fails with CoreDeviceError 10002, the code for
any failed launch, and "The requested application <bundle> is not
installed." in its reason, which answers 404 `notInstalled`; any other
failure is a 502 with devicectl's words. Install is not wired:
`/api/install` reads every upload as an APK through aapt2. Logs are, through
syslog_relay (see `GET /api/logcat` under Agent API).

devicectl 651.13.4 (Xcode 27.0, `jsonVersion` 5) puts its fields under
`properties`: the name is `properties.state.name`, the OS version
`properties.software.osVersionNumber.stringValue`, and
`developerModeStatus` is an object, `{"enabled": {"mode": 1}}`. Read as a
string, that last one failed the whole document and listed nothing. The
deprecated `hardwareProperties`, `deviceProperties` and
`connectionProperties` are still read, and `properties` wins. The test
fixture is a redacted capture: a phone on Wi-Fi, a paired phone out of
reach, and two simulators.

**CoreDevice's HID reaches a real iPhone, over a cable and over Wi-Fi.**
`simcap -devices` opens each capability on each physical device. Both
phones read `button`, `keyboard`, `digitizer`, `vendorDefined` and
`pasteboard` as `ok` on `localNetwork`, and the 26.6 phone read the same on
`wired`; each opened in 14-31ms except `keyboard`, 248-461ms. A phone
devicectl lists but cannot reach answers 1001, "not supported by this
device", for every one, so that error means out of reach, not a missing
capability. `-press <identifier>` sent Home through `hid.button` and it
landed: Settings in front before, the Home Screen after, by
`devicectl device capture screenshot`, over a cable on the 26.6 phone and
over Wi-Fi on both. So input goes through CoreDevice HID as it does for a
simulator (`hub.swift`), not through iPhone Mirroring's window. Touch and
keys are opened but have not been sent.

**A phone on a cable streams through QuickTime's screen-capture device.**
`simcap <udid> -phone <name>` sets CoreMediaIO's
`kCMIOHardwarePropertyAllowScreenCaptureDevices`, waits for the muxed
`AVCaptureDevice`, and feeds its frames to the encoder a simulator uses
(`encoder.swift`), so the server reads it with the same parser. Through a
spare server on the 26.6 phone over USB, with `wtprobe`: CONNECT 1.74s,
first frame 1.85s, 833 frames in 15s, pts p50 16.8ms and p99 32.0ms, and
`maxSize` 1080 streamed 1206x2622 as 496x1080. Three things were found by
running it:

- The device is announced on the main run loop, so the wait spins it; a
  `Thread.sleep` loop never sees it.
- Its `uniqueID` is neither the UDID nor CoreDevice's identifier
  (`C8397C0D-...` for `00008140-001A4502...`), so it is matched by the name
  devicectl reports, and refused when two capture devices share a name.
- TCC counts it as a camera and charges the request to the process
  responsible for simcap. Launched from Claude Code that was refused with no
  prompt, for a missing `com.apple.security.device.camera` entitlement, and
  frames never came. Under launchd it would be loupe, re-signed by every
  `make build`. So `-phone` re-spawns itself with
  `responsibility_spawnattrs_setdisclaim`, and the grant is simcap's own;
  its Info.plist, linked into `__TEXT,__info_plist`, carries the usage
  string.

Plugging in a phone also raised macOS's "what is this audio device" prompt
once; "Other device" answered it.

**Sign simcap with a team, or every rebuild asks for the camera again.**
Signed ad hoc, simcap's designated requirement is its cdhash, so TCC reads
a rebuild as a new program: on 17 October tccd logged "Failed to match
existing code requirement" for the rebuilt simcap, then prompted, and the
26.6 phone streamed nothing while the prompt sat unanswered on the Mac.
Release builds of simcap are signed with a team's Developer ID under the identifier `dev.bmcreations.loupe.simcap`,
so the requirement is the identifier and the team (`subject.OU`), which a
rebuild keeps. sign.sh writes it out: codesign's default pins the leaf
certificate's common name, which a renewed certificate changes. That the grant then survives a rebuild follows from the requirement
and has not been watched across one yet. A `-phone` waiting on the prompt
now says so on stderr at once and gives up after 8s, inside the server's
10s open, so the viewer's error names the prompt. A denied grant fails the
same way. Every simcap's stderr is also echoed to the
server's, so `make agent-log` shows the wait as it happens, each line as
`simcap <id>: ...`.

`-phone` re-spawns itself for that grant, so the server starts simcap in
its own process group and its Close kills the group: a SIGKILL to the
parent alone left the child under launchd, holding the phone's capture
device, after every failed open. The child also exits when its parent does.

**Input is `simcap <identifier> -phone-input`**, over a cable or Wi-Fi,
`iosdeviceinput.go` on the Go side. The identifier is CoreDevice's UUID,
not the hardware UDID. It speaks the simulator input process's line
protocol, so `simInput` drives it unchanged and the stream's touches and
keys go through it as a simulator's do. Coordinates are screen pixels,
1206x2622 on an iPhone 16 Pro, which is both devicectl's screenshot size
and CoreDevice's `displayInfo` native size. Through a spare server on the
26.6 phone, `/api/input` taps opened Settings, Home went back, a swipe down
opened Search and `Loupe 42` typed with its capital, and a `wtprobe -tap`
on Settings' icon over a live WebTransport session (CONNECT 1.96s, first
frame 2.34s) opened Settings.

**Below iOS 27, touch goes through an XCUITest runner on the phone.**
On Brandon's iPhone (iOS 26.6, cable) CoreDevice's digitizer answered `ok`
for every touch and none landed, while Home through `hid.button` did. So a
phone below 27 on a cable sends `tap`, `swipe` and `touch` to
`tools/xcrunner`, a UI test that never returns: it listens on the phone's
own 127.0.0.1 and applies each command through `XCUICoordinate`. Buttons
and keys stay on CoreDevice. `make xcrunner TEAM=<id>` builds and signs it
into `.build/xcrunner`; the team is a parameter, never in the repo. The
server looks for it on each touch, so a `make xcrunner` needs no restart,
and starts it on the first touch with `xcodebuild
test-without-building`, which does not re-sign, and until it answers every
touch is a 503 `runnerStarting`, never an `ok`. It took about 5s to come up.

The Mac reaches it through usbmuxd's socket (`internal/usbmux`), the path
iproxy uses, without iproxy. The CoreDevice tunnel might reach a listener
on the phone and was not tried; a runner dialling out to the Mac would need
the Local Network prompt and the Mac's address. usbmuxd lists a Wi-Fi
phone too, and a lockdown service reaches it that way (the syslog below),
but `usbmux.Connect`, the runner's dial, takes the cable only, so below 27
on Wi-Fi a touch answers that it needs a cable. Whether the runner answers
over the network path was not tried.

`touch down` and `move` are recorded and `up` applies them: XCUITest has no
live finger, so a drag lands as `press(forDuration:thenDragTo:)` once the
finger lifts, and a second finger is refused. Through a spare server, an
`/api/input` tap on Photos' icon opened Photos in 1.7s; the runner alone
took 1.38s for a tap that opened Search. The runner process is letterboxed
(its `UIScreen` read 960x1440), so the screen size comes from XCUI's
springboard frame instead.

devicectl reads a wired phone `disconnected` within about 10s of its last
call while usbmuxd still reaches lockdownd, and each flap to `offline`
killed the runner. `disconnected` is now an idle tunnel on a cable as on
Wi-Fi; only `unavailable` drops the transport.

**A phone on Wi-Fi streams through `simcap -phone-wifi`, a receiver of
our own for the stream Device Hub shows.** Wi-Fi gives no capture device.
`CoreDeviceMediaStreamSupport` negotiates a mirrored stream
(`receiveMirroredPrimary`) and the phone starts sending, but the Mac's
receiver is avconferenced, which refuses any process without the
restricted `com.apple.videoconference.allow-conferencing` entitlement:
`vcMediaStreamSetSessionConfig failed authorization check`. Only Device Hub
and devicectl carry it, and AMFI killed an ad-hoc build that claimed it.
So avconferenced never opens the port, and simcap does (`wifi.swift`):

- The receiver address and port, and the Mac's negotiated SSRC, are read
  from CoreDevice's log lines for simcap's own pid (`ReceiverIP:`,
  `vcMediaStreamLocalSSRC`). Nothing in the API returns them. This is the
  part most likely to break with an Xcode update.
- The media is RTP over UDP inside the CoreDevice tunnel, though the offer
  asks for TCP. Payload type 100, HEVC (RFC 7798), unencrypted, RTCP on the
  same port, RTX and FEC off. bmc-iphone sent 1216x2656, not its 1206x2622
  screen; touches scaled by the stream size still landed.
- simcap must send RTCP receiver reports **under the negotiated SSRC**. With
  any other the phone stopped at 15s with "AVC video stream RTCP timeout";
  with it, a 75s run never stopped.
- HEVC is decoded with VideoToolbox and re-encoded as H.264 by
  `encoder.swift`, because Safari cannot decode HEVC (trap 24).
- The phone sends nothing while its screen is still, and a locked phone
  repeats its last frame, so simcap re-encodes the newest picture at the
  250ms idle floor and a lock is invisible in the stream.
- A phone before iOS 27 refuses: Brandon's iPhone, on 26.6, answered
  CoreDeviceError 9001, "Remote control requires iOS 27.0".

Measured on bmc-iphone (iOS 27.2) over Wi-Fi: a Home press reached the
received video 530-557ms after it was sent, against 641-678ms through
Device Hub's window; a launch's change arrived about 0.1s after devicectl
returned, so most of a Home press's half second is the phone, not the
stream. Through a spare server with `wtprobe`, from a phone at rest:
CONNECT 3.1s, first frame 3.4s, 658 frames in 20s of a static screen, and
a `wtprobe -tap` on the Weather widget opened Weather. The decode and
re-encode add to that, unmeasured. Nothing has streamed it under the
launchd server or to a real viewer yet. iPhone Mirroring's window is the
other Wi-Fi route and was not taken: one phone at a time, phone locked.

### Phone keyboard on Android

**Android hides its keyboard for a UHID keyboard the scrcpy server
creates**, the device `scrcpy --keyboard=uhid` makes. `hardwareKeyboard` on
an adb device or a running AVD sends `UHID_CREATE` (opcode 12, with a boot
keyboard descriptor and no LED report) on the session's control socket, and
`UHID_DESTROY` (14) to turn it off; the layout was read out of the pinned
jar's `ControlMessageReader`, not from memory. Nothing is typed through it.
It is there so Android sees a physical keyboard, and typing goes as
inject-text and inject-keycode on the same socket. `/api/input`'s `text` and
`key` take that socket too while a session is open, and `adb shell input`
when none is.

The device lives in the scrcpy server, so it needs a session: with none,
the switch answers 409. It goes when the server exits, and the setting is
cleared when the target's last stream closes, so it lasts exactly as long
as the session and the next phone to connect keeps its own keyboard. A
bitrate relaunch opens the new stream before the old one closes, and the
new one creates the keyboard again. On emulator-5556 (acmechat_blockstore,
Android 16, Gboard) Settings search kept focus with Gboard down to its
floating button, `dumpsys input` listed `loupe keyboard`, turning it off
brought Gboard back, and 22s after the viewer left the device was gone.

**Text and keys leave in order and do not land in order.** A burst of text
followed at once by a Backspace left `hi loue`, `hi lope` or `hi lupe` for
`hi loupe`: the key landing at a cursor position Gboard had not caught up
with. Keycodes alone did it too with the keyboard attached, and one
character per message never did at 30ms apart or more, which is how a
phone types. So a key waits behind a burst of two or more characters:
300ms plus 20ms each, capped at a second (`textSettleTime`, whose comment
has the runs). A paste or a predicted word is what waits, never a
keystroke. `cmd/kbprobe -seq` sends such a sequence and reads the focused
field back.

### Clipboard

**Android's clipboard rides the scrcpy control socket.** Get is type 8 with a
copy-key byte, set is type 9 with a u64 sequence, a paste byte and a string
with a u32 length, and the device answers with device message 0 and the same
string, all read out of the pinned jar. `clipboard_autosync` makes the server
send the clipboard each time it changes, so `readDeviceMessages` now reads
the control socket, which nothing did before; the session forwards each
change to viewers as `MsgClipboard` (0x07). A set from a viewer did not echo
back as an offer on emulator-5556.

**`simctl pbcopy` sets nothing on Xcode 27**: it exits 0 and `pbpaste` reads
the old value. simcap uses CoreDevice's pasteboard capability instead, the
one Device Hub syncs through, as `pbget`, `pbset <base64>` and a change
watch. The pasteboard name is `com.apple.UIKit.pboard.general`; the Mac's
general name reaches a second pasteboard no app reads. Set took 11-49ms and
a read 3-93ms on loupe-probe.

**A simulator paste is typed, not Cmd+V.** iOS asks before an app reads what
another process put on the pasteboard, and Home Screen Search refused a
Cmd+V with "Paste requires user authorization". Typing needs no permission,
so text with a character that has no key fails before anything is sent.

**Safari reaches the clipboard only inside a tap.** The offer is a banner to
tap, not a silent copy, and "Copy from device" writes a `ClipboardItem` from
a promise, the form Safari accepts after an await. "Paste to device" calls
`readText`, and iOS shows its Paste callout when another app set the
clipboard. All four paths were run in loupe-probe's Safari against
emulator-5556 and loupe-probe; none has run on a real iPhone yet.


### Device frames

`GET /api/frame` describes the bezel to draw around a stream, and
`/api/frame/asset` serves the images it names. `Framer` is optional the way
`Inspector` and `Screenshotter` are, so synthetic answers 501.

Two sources, and the response says which:

| `source` | Where it comes from |
| --- | --- |
| `skin` | The emulator's own SDK skin, exact art and exact geometry |
| `derived` | A generic bezel around what the device reports about itself |

A simulator answers `skin` too, from Xcode rather than the Android SDK: see
Simulator frames below.

The skin route resolves a running emulator through
`getprop ro.boot.qemu.avd_name` to `~/.android/avd/<name>.avd/config.ini`, and
reads `skin.path` from there. That path is **absolute**, which is why nothing
has to find `ANDROID_HOME` -- it is unset on this machine and the SDK works
regardless. `adb emu avd name` would answer the same question and needs the
emulator console and its own auth token, so the property is the cheaper route.
Measured on emulator-5554: `1198x2531`, display at `(59,55)` `1080x2424`,
corner radius 87.

**A skin's images are on the layout's own pixel grid, and neither is sized to
the frame.** `mask.webp` is exactly the display -- 1080x2424 on pixel_10 -- and
carries the screen's rounded corners and its punch hole, so it goes on the
display rect beside the canvas. `back.webp` is 1205x2535 against a layout
declaring 1198x2531, with its hole still at (59,55), so it is drawn at natural
size and the overhang is clipped. Scaling either to fit is trap 40.

**Skins are read from the installed SDK and never vendored.** The art is the
only way to make a Pixel look like a Pixel, and the Android SDK terms restrict
redistribution, so it stays where it was installed and
`THIRD_PARTY_NOTICES.md` is untouched. That also means the art is missing on a
machine without that skin, which is why a failed skin read falls through to
the derived path rather than erroring.

The derived route is `wm size`, `wm density` and the display's `cutoutSpec`.
Measured on an Acme One: `1284x2754` around `1200x2670`, notch `scale` 3 `dx` 600.
Two of its numbers are chosen rather than measured, and are commented as such:
the bezel width and the corner radius. **A device does not report its corner
radius** -- Android's `RoundedCorner` API reads 0 on both emulator-5554 and the
Acme One -- so only the skin route carries a real one.

The cutout is returned as an untransformed SVG path plus a `scale` and a `dx`,
rather than as a rewritten path. `@dp` scales by density/160 and the anchor
translates, and both are exactly what an SVG `transform` already does, so the
client wraps it in `translate(dx,0) scale(s)` and nothing has to parse path
commands to move them.

**The bezel turns with the device, and a skin has only one layout to turn.**
`/api/frame` reads the display's rotation and answers with the bezel for the
orientation the device is in, so the client draws art rather than deciding
geometry. Every Pixel 3 and later skin declares exactly one layout -- pixel_10
has `portrait` and nothing else -- so the other orientation is that layout
turned a quarter turn, which is exact: a rigid photograph of a phone turned 90
degrees is what the phone looks like turned 90 degrees. Legacy skins do ship a
second layout with its own art (pixel_2 has `land_back.webp`), and it is used
when its display rect matches the orientation wanted, but only the turned path
has been run against hardware.

**A layout's name is a label, not its orientation.** `pixel_tablet`'s only
layout is called `landscape` and references a part called `portrait`, and its
display is 2560x1600. Reading `layouts.portrait` -- which is what this code did
-- gets that device no bezel at all. The orientation comes from the display
rect's own shape, after the part's `rotation` is applied.

The turn is reported as `rotation`, a count of quarter turns clockwise the
client applies to the art, and the geometry in the response is already turned.
Both images have to turn: `mask.webp` is exactly the display, so in landscape
it is 1080x2424 against a 2424x1080 display rect, and stretching it onto that
rect is trap 40 again. They are turned about their own top-left corner and slid
back, which is why `turnedArt` shifts by the height on one turn and the width
on three.

**The slide back is by the layout's size, not the image's.** The server turns
the display rect about the layout, 1198x2531 on pixel_10, and `back.webp`
overhangs that by 7x4 px on the right and bottom. Sliding it back by its own
1205x2535 put its hole off the canvas, measured by alpha-scanning the rendered
art on emulator-5554: 6.2-6.4 art px on the short axis at rotation 1 and
2.7-3.9 at rotation 3, against 0.2-0.4 at rotation 0. Pivoting on the layout
brings every short-axis edge under 1 px at all four rotations, with the
overhang turned out past the top or left edge, where the wrap clips it. Up to
2 px remains along the long axis at 0.54 CSS px per art px, from `place`
rounding the art box to whole pixels. The mask is display-sized, so for it the
two sizes are the same.

**A flip between the two landscapes is polled for.** 90 to 270 leaves the
stream the same shape and the video carries nothing else about it, so the
frame fetched on the shape change would keep its turn and put the punch hole
on the wrong edge. While a turned skin frame is shown, the client asks
`/api/frame` every 3s (one `dumpsys`, 0.11-0.17s) and reapplies it when
`rotation` differs, reported as `event:"frame"` with `via:"recheck"`. On
Pixel_10 through a spare server, with Settings in front and
`settings put system user_rotation`, 1 to 3 and back each reapplied within 2s
and `__loupeLayoutCheck()` read `[]`. The home screen does not rotate there,
so a flip has to be tried with an app in front.

**A derived frame does not turn.** Its cutout is an SVG path anchored along one
edge by a `dx` that means nothing once the axes swap, and no device it runs
against can be rotated for testing here, so it stays at its natural orientation
and the client hides it in the other one.

**Simulator frames are Xcode's own chrome.** The device type's profile
names a DeviceKit chrome bundle and a framebuffer mask, both vector PDFs under
`/Library/Developer/DeviceKit`, read in place and never vendored. `sips`
rasterises them at the screen's scale so the art lands on the framebuffer's
pixel grid, as a skin's does (trap 40): iPhone 17 is 1308x2724 around a
1206x2622 screen at (51,51). The rendered art is cached per simulator, and a
failed render is not, so a timeout on a busy machine is retried.

Two things differ from an emulator skin and both were found by looking at the
page. **The mask cannot be black corners.** A skin's `mask.webp` is black where
the screen's corners are, which works because a Pixel's bezel is black there;
DeviceKit's chrome has a grey rim that curves inside the display rect, and
black corners cut square notches out of it. `cornerMask` fills the corners
with the bezel's own pixels instead. **And the corners reach past the phone.**
The display rect's corners lie outside the chrome's outer curve, where the art
is clear, so no mask can hide the video there. The client clips the canvas to
`cornerRadius` for every `skin` frame; on an emulator the mask already covers
the same pixels, so the clip changes nothing there.

A simulator frame does not turn with the device, because simulators do not
rotate yet. **A Duo has a frame per panel.** Its `capabilities.plist` names a
chrome and a mask for each display, and `/api/frame` answers with the lit
panel's, asking simcap which that is (3s at most; a single-panel simulator
never asks). The inner panel's `phone14` composite is 660x924 pt, smaller
than the 669x951 pt panel, so it is stretched to the panel plus
`chrome.json`'s `sizing` rim by a nine-slice, 2109x2955, and turned by its
`nativeRotation` 270 to 2955x2109 with `rotation` 3. The art is cached per
panel. When the fold changes, each half of the folding picture carries its
side of the bezel, and the fold waits up to a second for the other panel's
art before landing on it. Checked on `loupe-duo` through a spare server,
closing and opening with the frame on: `__loupeLayoutCheck()` read `[]` in
both postures.

Asset names are checked against the ones the parsed layout declared, which is
the whole check: a caller cannot name a path the layout does not mention, so
there is nothing to escape. The art is content-hash ETagged because it is
static per skin and about 230KB for a Pixel.

The frame is **off by default in the client**. It costs about a tenth of each
axis, and trap 18 was specifically the device screen not fitting on iOS.

**Coordinates are device pixels, not stream coordinates.** `Stream`'s
`Touch`/`Scroll`/`Key` are in the space `Meta.Width`/`Height` describes, which
`MaxSize` rescales and which exists only while something is streaming.
`/api/input` goes through `adb shell input` instead, so what `/api/ui` reported
is what gets tapped, with no session required and no scale factor to get wrong.
It costs a few hundred milliseconds per call against roughly nothing for a
scrcpy control message, which is the right trade for a caller working from the
hierarchy and the wrong one for a finger on a screen.

Do not drive this from the video. Locating a button in a decoded frame is image
work to recover what the accessibility tree states outright, and the tree
survives a theme change, a resolution change and a `MaxSize` downscale.

`input text` is the one payload that reaches a shell: `adb shell` joins its
arguments and hands them to sh **on the device**, so passing a slice on the
host proves nothing. `shellQuote` wraps unconditionally, and
`TestShellQuoteNeutralisesDeviceShellMetacharacters` runs each case through a
real sh to check it round-trips.

Every action is traced as `kind:"agent"`. A device that a human and a process
are both driving produces behaviour neither can explain alone, and this is what
puts the taps on the same timeline as the frames.

