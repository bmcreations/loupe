# iOS simulators and iPhones

Not offered in public loupe yet: iOS support is coming soon. This page
describes how it works.

How loupe streams and drives iOS simulators through simcap, and physical
iPhones and iPads through CoreDevice. Android targets, the phone keyboard,
the clipboard and device frames are in `docs/targets.md`.

## iOS simulator targets

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

## Physical iOS devices

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
Release builds of simcap are signed with a team's Developer ID under the
identifier `dev.bmcreations.loupe.simcap`,
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
