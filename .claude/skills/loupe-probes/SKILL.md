---
name: loupe-probes
description: Use when verifying loupe end to end without a browser or phone: wtprobe, overlaptest, rotprobe/touchrotprobe, simprobe, kbprobe, nativeprobe, jitter replay, or the page layout self-check. Covers which probe answers which question and how to run it.
---

## Verification discipline

Server-side WebTransport is proven with **`cmd/wtprobe`**, a native Go client,
not inferred from a browser. When a browser and a server disagree, run the probe:

```sh
go run ./cmd/wtprobe "https://100.64.0.10:18456/wt?target=synthetic"
```

`cmd/overlaptest` is the same idea for the relaunch path. It opens a second
scrcpy server against a real device while the first is still streaming, then
closes the first, which is the one part of the swap that fakes cannot exercise:

```sh
go run ./cmd/overlaptest ACME00000000001
```

`cmd/rotprobe` and `cmd/touchrotprobe` are the pair for rotation. The first
reports what the stream does across a rotation -- whether it ends, whether the
codec meta is resent, and what each parameter set's SPS decodes to. The second
answers the only question that matters for input, by sending a touch with the
stale size and then with the live one and reading scrcpy's own verdict:

```sh
go run ./cmd/touchrotprobe emulator-5554
```

It raises the server's log level to verbose through `scrcpy.Config.LogLevel`,
because the rejection is logged at VERBOSE and is silent otherwise. Point
neither of them at a device someone is using -- they rotate it.

`cmd/simprobe` is the one for iOS simulators. It runs `simcap`, our own
Swift helper that reads the simulator framebuffer through CoreSimulator and
encodes it with VideoToolbox, and reads the output with the scrcpy parser, so
a pass means the bytes are already in the shape the session consumes:

```sh
make simcap
go run ./cmd/simprobe -animate <udid>
```

simcap is kept apart
from `make build` because the frameworks it opens are private and move with
Xcode -- SimulatorKit is under `Contents/SharedFrameworks` in Xcode 27, not
`Developer/Library/PrivateFrameworks`.

Measured on an iPhone 17 simulator, iOS 27.0, 1206x2622: first frame 625-790ms
after launch, 59-60fps under motion with arrival p50 16.7ms and p90 18.3ms, a
keyframe every second and 14-21ms after a `TYPE_RESET_VIDEO` byte, and exit in
under 10ms when stdin closes. A static screen still gets a frame out at
startup and a keyframe a second, from a 250ms idle floor.

`-setbitrate bps` sends simcap's live bitrate command at the halfway point,
and the `kbps/second` line shows whether the encoder followed it.

`-animate` uses a CSS transform, not a `requestAnimationFrame` loop: rAF
painted at 6-7fps in the simulator's Safari, and a probe driven by it measures
the page. Point it at a simulator nobody is using -- it opens Safari.

`cmd/kbprobe` attaches the Android hardware keyboard through its own
session and types, saving a screenshot at each step (with it attached,
Gboard's `mInputShown` still reads true, so only a picture says whether it
went down). `-seq 'text:hi,key:67'` sends steps back to back and prints the
focused field, which is how the ordering above was measured. It types into
whatever has focus.

`wtprobe -tap x,y` sends one tap in stream pixels after the first frame, which
checks a target's viewer input path without a browser. It drives the target.

`wtprobe -dump file` writes each frame's arrival, PTS and flags, and
`node web/jitter.replay.mjs file [jitter.js] [interactive]` replays them
through a jitter buffer on a 60Hz grid and reports how long frames waited to
be drawn. That is the number a user feels as lag, and a browser cannot give it
for two versions of the buffer on the same arrivals -- or at all when its pane
is hidden, since a hidden page does not paint. The client now reports it live
as `holdMs` beside `bufferMs`, because the two disagreed by 50ms before the
clock followed the transit floor.



A healthy synthetic run reports a few hundred frames, a keyframe every second,
and a first frame starting `00 00 00 01 67`. Point it at the address the phone
uses. Loopback has a 16KB MTU and will hide anything the path cannot carry,
which is how trap 11 stayed invisible.

The client reports what only it can know (decode rate, render rate, rtt as the
phone measures it) and the server joins it with its own counters into one trace
record per second. Read `loupe-trace.jsonl`, do not read numbers off a phone.

Layout is checked the same way, by asking the page rather than reading a
screenshot. `window.__loupeLayoutCheck()` returns an array of violations, and
`[]` means the page is laid out correctly. Its rules: with a skin
frame shown, the mask rect equals the screen rect to the pixel (trap 40); the
screen fits inside the stage on both axes (trap 18); the screen does not
intersect the nav row, the overflow corner or a half-height inspect sheet; the
nav row sits on the side the handedness setting names; the control, clipboard
and record banners (`.recBanner`) cover neither the top bar nor the
immersive handle, and float over the top of the device, since `#stage`
keeps no slot for them; and in immersive mode no control or
bezel is shown and the screen is a contain fit centred on the stage. Docked (`html.docked`, a window 900px or wider), the picker is pinned on the
left and Tools on the right, each `min(360px, 28vw)` wide, with no open or
close: neither panel may intersect the screen, the nav row and overflow
corner may not sit under a panel, the hand side is judged against the
midline between the panels' inner edges, Inspect takes Tools' slot, and
immersive hides both. Those rules were run in Playwright against a spare
server at 1440, 1024 and 900 wide, right-handed at 900, and back to phone
width; not in Safari or on an iPad. `window.__loupeLayout()` returns the rects it read.
The rules live in `web/livecheck.js` and are pure, with a node test, because
`app.js` has no harness (trap 21).

