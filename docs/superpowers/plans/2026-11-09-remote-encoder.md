# Remote encoder: skip instead of drop for remote viewers

Replaces slices 2-4 of `2026-11-09-frame-acks-flow-control.md`. Slice 1 (acks) and
Task 6 (ack-based staleness) stay as built.

## Why

Same cellular link, one minute apart (9 November, 10:41-10:45, iPhone 17 sim
`CC79E655-F343-4146-ABB7-5A607375DCA5`):

| | voxl 3eda628 | loupe `4dc9308659b4` |
|---|---|---|
| Base RTT | 57-86 ms | 75-81 ms |
| Keyframes while scrolling | 0 | about one per drop |
| Frames held for a keyframe | none | 40-90% from 10:43:27 |
| Oldest unacked frame | mostly < 300 ms | up to 2.9 s |

loupe encodes every capture and each viewer drops what it can't send. A drop
needs a keyframe, the keyframe fills the link, the next delta goes stale. voxl
does not capture while the slowest viewer's in-flight frames exceed about
`rtt + 120 ms`, so every encoded frame is delivered and no keyframe is needed.
Every rule change since 8 November moved where the drops start (stale rule,
pacer, bitrate gates); none removed them.

## Design rules

1. Local viewers (`ViewerOptions.Local`, loopback) stay on today's encoder and
   stream. Nothing a remote viewer does changes their frames, rate or keyframes.
2. Remote viewers that ack get a second encoder (the remote track). It encodes a
   capture only while the remote gate is open. Remote-track viewers never shed a
   delta for age or pacing; a lost frame (abandoned write) is the only thing that
   asks for a remote keyframe.
3. The remote encoder uses quality-based rate under a cap
   (`kVTCompressionPropertyKey_Quality` + `DataRateLimits`), no keyframe interval,
   and keyframes only on join or loss.
4. The main encoder's bitrate adapter ignores remote viewers.
5. Remote viewers that don't ack yet (native iOS until handset ships acks) stay on
   the main track with today's drop rules, and no longer lower its bitrate.
6. The browser draws remote-track frames on decode, with no newest-of-clump buffer.

Performance choice: one remote encoder implementation, in simcap, on the Mac's
hardware VideoToolbox. Sims feed it the rotated/scaled pixel buffer simcap already
has, so the second encode costs one extra `VTCompressionSessionEncodeFrame` and no
decode. Android feeds it by hardware-decoding scrcpy's H.264 on the Mac. That keeps
the device's encoder and the local stream untouched and avoids a scrcpy fork. A
patched scrcpy (live `PARAMETER_KEY_VIDEO_BITRATE`, `PARAMETER_KEY_SUSPEND`) was
rejected: it has one encoder per device, so gating it for a phone would slow Mac
Handset, which rule 1 forbids.

## Base commits

- loupe: branch `feat/remote-encoder` off `feat/frame-acks` (f93223ce or later).
- simcap: branch `feat/remote-encoder` off **8314fe91**, the commit the live simcap
  was built from. The clone at `/Users/bmc/dev/loupe-wt/ios-remote-ax/.build/simcap-src`
  has it (its HEAD 4e48b01 is behind it; check out 8314fe91 explicitly).
  `/Users/bmc/dev/loupe/.build/simcap-src` (9c80e1e) does not, so never build from it.

## Slice R1: remote track for sims, gated (one phone run)

### Task 1: simcap encoder state into a class (no behavior change)

`encoder.swift` keeps session, scaler, rotator, pools, `outWidth`/`outHeight`,
`lastParams`, `forceKey` and bitrate as globals. Move them into `final class
Encoder` with an output fd. `write()` takes the fd. `phone.swift:108,168` and
`main.swift` use the main instance. Check: on one sim, one minute of capture
before and after has the same packet count within 2% and the same SPS.

### Task 2: `-remote` flag, second encoder on fd 4

fd 3 is `-hevc`. With `-remote`, simcap opens fd 4 and runs a second `Encoder`:
H.264, real-time, low-latency rate control, no reordering,
`MaxKeyFrameIntervalDuration` = 0 (none), `Quality` 0.6, `DataRateLimits` cap
default 4 Mbps, its own max size. It encodes the same `pixels` buffer as the main
encoder inside `encodePixels`. Same wire format as stdout (header + be64/be32
packets). First, probe that the hardware H.264 encoder on this Mac accepts
`Quality` together with `DataRateLimits` and low-latency mode; if it rejects
`Quality`, fall back to `AverageBitRate` = cap and record that in the plan.

New stdin ops, length-checked like 0x80/0x81:
- `0x82 u8`: remote gate, 0 = hold, 1 = open. Held captures are skipped, not queued.
  On open, the next capture is encoded at once.
- `0x83`: remote keyframe.
- `0x84 u32`: remote cap in bps (sets `DataRateLimits`).
- `0x85 u16`: remote max size.

The 250 ms idle re-encode applies to the remote encoder only while open.

Probe result (simcap d41e9c4, this Mac's hardware H.264): with low-latency rate
control, `Quality` is rejected (-12900); without it, `Quality` 0.2 and 0.9 gave
identical output. The remote encoder uses low-latency + `AverageBitRate` = cap +
`DataRateLimits` [cap/8 bytes, 1 s], both set live by 0x84. There is no
quality-based rate; slice R2's delay-based cap does that job instead.

Correction (11-09, simcap 66573d2): the 0.2 and 0.9 probe ran with
`DataRateLimits` set, and VideoToolbox ignores `Quality` whenever a limit is
set. Without one, `Quality` sets frame size (noisy content at 60 fps: 0.3
gave 2.8 Mbps, 0.6 gave 57 Mbps) and a live change applies on the next frame.
simcap's remote encoder now runs without `DataRateLimits` and steers `Quality`
from each delta frame's size against the cap, with a token-bucket frame budget
as the per-frame bound; 0x84 sets the steer's target and the budget's rate.

### Task 3: loupe tracks become an enum

`OutMsg.passthrough bool` becomes `OutMsg.track` (`trackMain`, `trackPassthrough`,
`trackRemote`); `broadcast` (`session.go:1814`) filters on `v.track`. Passthrough
stays as it is. Unit tests: a frame on each track reaches only its viewers.

### Task 4: remote track in the session

`openSimcapWith` adds `-remote` and an `ExtraFiles` pipe for fd 4 when the session
allows a remote track; `pumpRemote` reads it into `s.remote` the way
`pumpPassthrough` reads `s.alt`. `AddViewerWith` puts a viewer on `trackRemote`
when `!o.Local` and it acks (Hello capability). Join sends `0x83`. Codec string
comes from the main pump's H.264 path, not the HEVC passthrough helpers.

### Task 5: remote gate

A session loop every 10 ms reads `ViewerFlow` for each remote-track viewer:
open when every one has `oldestMs <= baseRtt + 120` and `backlog <= 8`; a viewer
with no ack for 4 s is left out (stall escape). Send `0x82` only on change.
Remote-track viewers skip the stale and pacer sheds in `viewer.go`; an abandoned
write asks for `0x83`, rate-limited to one per 500 ms.

### Task 6: main adapter ignores remote viewers

`intervalBytes` and the adapter samples exclude remote viewers (both tracks).
Test: a starved remote viewer on the main track never moves the main bitrate.

### Task 7: browser presents on decode

For `trackRemote` (told in the init message), `pane.js` draws each decoded frame
at once instead of the newest of each clump. Acks stay after present.

### Task 8: trace

Per viewer: `track`. Per session per second: `remoteGate` (open fraction),
`remoteHeldMs`, `remoteFrames`, `remoteKeys`, `remoteCapBps`.

Phone run check (browser, cellular): `remoteKeys` only at join and after an
abandon; `shedAwaitKey` 0 for the remote viewer; a local Mac viewer at the same
time keeps its frames per second and bitrate from before.

## Slice R2: delay-based cap

voxl's `RateCtl` on the remote viewers' `queueMs` (windowed-min base from
54164000): congested when `queueMs > 30` or `oldest > rtt + 80`; back off 15%
toward measured delivery only if delivery was at least 50% of the cap; slow start
x1.5; recover to 90% of the last ceiling, then +2%/s; raise only while content
uses at least half the cap. Floor 1.5 Mbps, ceiling 8 Mbps. Drives `0x84`.

## Slice R3: paintover

In simcap, when no screen callback arrives for 150 ms, encode the next 3 remote
frames at `Quality` 0.9 under the same cap, then restore. Skip while the gate is
held. Check from a recording's contact sheet first, then one phone run.

## Slice R4: Android through the remote encoder

simcap `-transcode` mode: Annex B access units in on fd 3 (config packets flagged
as today), VideoToolbox hardware decode (the `wifi.swift:359,412` code decodes
HEVC; add H.264), output into the remote `Encoder` on fd 4 with the same stdin
ops. loupe starts it for an Android session when the first remote acking viewer
joins and stops it when the last leaves. Local viewers keep scrcpy's stream
directly. Measure added latency (decode + encode) and Mac CPU on the Pixel 7a
emulator before the phone run.

## Slice R5: native iOS acks

handset LoupeClient sends the ack after present and the Hello capability, so the
iOS app moves onto the remote track. Run `make fixtures` in handset first (the
wire constants changed in slice 1).

## Not doing

- A scrcpy fork (see the performance choice).
- voxl's damage-region refinement; VideoToolbox has no per-region quality.
