# Frame Acks, Flow Control, Paintover Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop phone viewers on slow links (cellular, ~100 ms RTT) from stuttering and smearing, by acking frames, holding back the encoder instead of dropping frames, sizing the bitrate from queueing delay, and sharpening a still screen.

**Architecture:** Browser viewers ack each frame's pts after it is presented. The server keeps a per-viewer in-flight list and turns ack times into a queueing-delay estimate and a delivery rate. Slice 2 uses the backlog to pause simcap's encoder (no frame is made, so none is dropped and no keyframe wait follows). Slice 3 drives a simulator's live bitrate from the delay estimate. Slice 4 has simcap re-encode a still screen at high quality for a short burst. Design follows voxl (github.com/tembo/voxl, MIT, commit 3eda628: `src/hub.rs` `Client::on_ack`, `src/video.rs` `flow_ok`, `RateCtl`, paint-over); attribute it in commits that port its constants.

**Tech Stack:** Go (loupe server, `internal/session`, `internal/httpapi`), plain JS web client (`web/`), Swift simcap (separate repo, bmcreations/simcap).

**Spec:** this plan. Evidence it argues from: the 2026-11-08/09 phone traces in handset `.superpowers/sdd/2026-11-06-mac-onboarding/` (abA.txt, abB.txt, streak-rec/) and the 08:45 run on build 9ac82a3681ea (awaitFrac 0.2-0.6 with the bitrate held at 4 Mbps).

## Global Constraints

- Branch `feat/frame-acks`, worktree `/Users/bmc/dev/loupe-wt/frame-acks`, based on `fix/bitrate-ladder` 1f31c342 plus the two `fix/web-frame-order` commits.
- One slice per deploy. Each slice is deployed to the live :18456 server with the swap tooling and gets one phone run on cellular before the next slice starts. Slice 2 and 3 do not start until slice 1's trace shows `queueMs` rising in the seconds where `awaitFrac` is high.
- Viewers that never send an ack (native iOS QUIC until LoupeClient is updated, old browsers) keep exactly today's behavior.
- Android (scrcpy) has no pause and its bitrate change is a relaunch: slices 2-4 change only simulator sessions (targets implementing `target.LiveBitRater`); Android gets only the ack-based staleness of Task 6.
- simcap changes build on the live simcap commit 8314fe91 (`/Users/bmc/dev/loupe-wt/ios-remote-ax/.build/simcap.ref`), never on `.build/simcap-src` at 9c80e1e, which lacks it. Confirm ancestry before installing (reflection 2026-11-03).
- New message type: `MsgFrameAck byte = 0x1A` (0x19 is `QUICStreamSlot`).

## Review Focus

- A viewer whose tab is hidden stops acking: it must stop gating others after 4000 ms of no acks (clear its in-flight list, mark `needKey`, exclude it until it acks again).
- pts restarts when the encoder is rebuilt (resize, rotate, relaunch): acks for the old stream must not match new entries. Clear a viewer's in-flight list whenever a config frame is sent to it.
- An ack for a pts the server never recorded (shed frame, reconnect) is ignored, not treated as acking everything.
- Two viewers, one on the Mac at 1 ms RTT and one on the phone: the phone's backlog decides the pause (product decision below), the Mac viewer's acks must never lift a pause the phone needs.
- simcap paused while the screen is still: resuming must send the next captured picture, not stay black until something moves.

## Decision: local viewers are never slowed by remote ones (user, 9 November)

Handset on the Mac viewing a device on the same Mac must always be quick. A remote viewer (browser, iOS app, or Handset on another machine) behaves like any other remote surface. So:

- A viewer is local when its connection comes from loopback. Only remote viewers' acks can pause the encoder (Task 5) or move the bitrate (Task 7), and only while no local viewer is attached.
- With a local viewer attached, the encoder runs at the local rate and full frame rate. A congested remote viewer then falls back to per-viewer handling: Task 6's ack-based skip for that viewer only.
- Today's adapter breaks this rule too: `intervalBytes` takes the slowest viewer, so a phone on cellular lowers the Mac viewer's bitrate. Task 7 must exclude local viewers' presence from that minimum, and the existing adapter gets the same change in Slice 2.
- Follow-up, not in this plan: a second simcap encoder for remote viewers would give both sides their own rate. Revisit if the per-viewer skip stutters with a local viewer attached.

---

## Slice 1: acks as measurement only

### Task 1: Browser sends a frame ack after present

**Files:**
- Modify: `web/transport.js` (WS `onmessage` ~:144 and WT `drainFrame` ~:347 pass `ptsMicros` through, already do; add `sendFrameAck(ptsMicros)` on both connection objects)
- Modify: `web/pane.js:690` (`present()` calls `conn.sendFrameAck(frame.ptsMicros)` once per presented frame)
- Modify: `internal/session/protocol.go` (add `MsgFrameAck = 0x1A` with a doc comment: 1 type byte + u64 BE pts, sent after the frame is presented)
- Test: `web/transport.test.js` (or the existing transport test file)

**Interfaces:**
- Produces: wire message `[0x1A][pts u64 BE]` on the WS socket and on the WT/QUIC control stream.

- [ ] **Step 1:** Test `sendFrameAck encodes type and pts`: for pts `0x0102030405060708`, the bytes sent equal `[0x1A,1,2,3,4,5,6,7,8]`, on the WS fake and on the WT control-stream fake.
- [ ] **Step 2:** Test `present acks each frame once`: present 3 frames, a frame dropped before present (decoder overrun) sends no ack.
- [ ] **Step 3:** Run, see both fail; implement; run the full web suite (`make test-web` or the command the repo uses), all pass.
- [ ] **Step 4:** Commit `feat(web): ack each frame after it is presented`.

### Task 2: Server tracks in-flight frames and queueing delay per viewer

**Files:**
- Create: `internal/session/flow.go` (`FlowEst`), `internal/session/flow_test.go`
- Modify: `internal/httpapi/inputqueue.go:17` (MsgFrameAck answered inline, not queued behind input)
- Modify: `internal/httpapi/server.go:576` (dispatch `MsgFrameAck` to the viewer's `FlowEst`)
- Modify: `internal/httpapi/viewer.go:~475-545` (record each delivered frame; clear on config frame)

**Interfaces:**
- Produces:
  - `type FlowEst struct` (mutex inside; safe across the write loop and the reader)
  - `func (f *FlowEst) Sent(pts uint64, bytes int, at time.Time)`
  - `func (f *FlowEst) Ack(pts uint64, at time.Time)` — pops every entry up to and including `pts`; unknown pts is ignored
  - `func (f *FlowEst) Reset()`
  - `func (f *FlowEst) Snapshot(now time.Time) FlowStats`
  - `type FlowStats struct { Acking bool; Backlog int; OldestMs float64; QueueMs float64; BaseMs float64; DeliveredBps float64; LastAckAgo time.Duration }`
- Constants copied from voxl: base = minimum send->ack time, replaced when a lower sample arrives or the base is 10 s old; `QueueMs = 0.7*QueueMs + 0.3*(sample-base)`; delivered rate over ≥0.5 s windows, `rate = 0.6*old + 0.4*new` (first window sets it).
- A viewer is `Acking` from its first ack.

- [ ] **Step 1:** Tests in `flow_test.go`: `TestFlowAckPopsThroughPts` (send 1,2,3; ack 2 -> Backlog 1), `TestFlowUnknownAckIgnored` (ack 99 with 1..3 in flight -> Backlog 3), `TestFlowQueueDelayFromExcess` (samples 50,50,150 ms -> BaseMs 50, QueueMs 30 after the third: 0.3*100), `TestFlowBaseExpires` (base 50 at t0, sample 80 at t0+11s -> BaseMs 80), `TestFlowDeliveredRate` (125000 bytes acked over 0.5 s -> DeliveredBps 2_000_000), `TestFlowResetClears`.
- [ ] **Step 2:** Run `go test ./internal/session -run TestFlow`, fail; implement; pass.
- [ ] **Step 3:** Viewer wiring: `NoteSent` site calls `flow.Sent(msg.PTS, len, now)` only for delivered frames; a config frame calls `flow.Reset()`. Test in `internal/httpapi` (existing viewer test harness): `TestViewerAckMeasuresQueue` drives two frames through a fake conn, feeds a `MsgFrameAck` through `dispatch`, asserts `Snapshot().Backlog == 1`.
- [ ] **Step 4:** `go test ./... && go vet ./...` pass. Commit `feat(session): measure per-viewer queueing delay from frame acks`.

### Task 3: Trace and client report carry the flow numbers

**Files:**
- Modify: the per-second viewer trace record (where `shedAwaitKey`/`offered` are written, `relaunch.go:360` `reportBitrate` and the viewer stats record)
- Modify: `web/connstats.js:18-25` (show `queueMs` and `backlog` in the readout)

**Interfaces:**
- Produces trace fields per viewer per second: `acking`, `queueMs`, `baseMs`, `backlog`, `oldestMs`, `ackBps`.

- [ ] **Step 1:** Test the trace record includes the six fields for an acking viewer and omits them for a non-acking one.
- [ ] **Step 2:** Implement, `go test ./...` pass, commit `feat(trace): record frame-ack flow numbers per viewer`.
- [ ] **Step 3: Deploy + gate.** Build, swap onto :18456, user does one minute in the phone browser on cellular. Pass condition: in seconds with `awaitFrac > 0.2`, `queueMs` or `oldestMs` exceeds `baseMs + 80`. If not, stop and report: the stutter is not link queueing.

## Slice 2: pause the encoder instead of dropping

Superseded by `2026-11-09-remote-encoder.md` (Tasks 4, 5, 7, 8). Task 6 stands.

### Task 4: simcap pause/resume op

**Files (simcap repo, from 8314fe91):**
- Modify: `encoder.swift` (~:300 op dispatch): op `0x82` + 1 byte (1 = pause, 0 = resume). While paused, captured frames and the 250 ms idle re-encode are discarded before encoding. On resume, the next frame (or an immediate idle re-encode of the last picture) is encoded as a normal P-frame.
- Modify (loupe): `internal/target/simulator.go:~661-690` add `simcapSetPaused` and `func (s *Simulator) SetPaused(bool) error`; `internal/target/target.go:207-222` add `type LivePauser interface { SetPaused(bool) error }`.

- [ ] **Step 1:** simcap test or harness check: with pause on for 1 s, stdout emits no frames; after resume, a frame arrives within 300 ms with no keyframe flag. Record the simcap commit.
- [ ] **Step 2:** loupe test `TestSimulatorSetPausedWritesOp` (fake stdin gets `[0x82,1]` then `[0x82,0]`).
- [ ] **Step 3:** Commit both repos; `make simcap` from the new commit; check `.build/simcap.ref` descends from 8314fe91.

### Task 5: Session gates the encoder on acking viewers' backlog

**Files:**
- Create: `internal/session/gate.go`, `gate_test.go`
- Modify: the session pump (where frames from the encoder fan out) to evaluate the gate every frame and on every ack.

**Interfaces:**
- Produces `func flowOK(viewers []FlowStats, rttMs []float64, frameIntervalMs float64) (ok bool, stalled []int)`. Copied from voxl `flow_ok`: per acking viewer with Backlog > 0, `rtt` defaults to 40 when unknown; `maxN = clamp(ceil((rtt+40)/interval)+2, 3, 40)`; `maxAge = rtt + 120`; `OldestMs > 4000` -> stalled (caller resets it and sets its needKey); else `Backlog >= maxN || OldestMs > maxAge` -> not ok.
- Session calls `LivePauser.SetPaused(!ok)` only on change.

- [ ] **Step 1:** Tests: `TestFlowOKUnderLimit`, `TestFlowOKBacklogCount` (rtt 100, interval 16.7 -> maxN 11), `TestFlowOKAge` (OldestMs 221 at rtt 100 -> false), `TestFlowOKStalledExcluded` (4001 ms -> ok true, stalled [i]), `TestFlowOKIgnoresNonAcking`, plus the Review Focus case `TestGateFastViewerCannotLiftPause`.
- [ ] **Step 2:** Implement per the user's answer to the open decision. `go test ./... && go vet ./...`.
- [ ] **Step 3:** Commit, deploy, phone run. Pass: `awaitFrac` under 0.05 for the browser on the iPhone sim, no rise in `queueMs`.

### Task 6: Ack-based staleness for viewers that ack (all targets)

**Files:** `internal/httpapi/viewer.go:513`.
- For an acking viewer, replace the `QueuedAt` staleness check with `Snapshot().OldestMs > rtt + 120` (same needKey path). Non-acking viewers keep `StalenessFor`.
- [ ] Test `TestStaleUsesAckAgeWhenAcking`; implement; commit. Ships with Task 5.

## Slice 3: delay-based bitrate for simulators

### Task 7: `RateCtl` drives `LiveBitRater`

**Files:** Create `internal/session/ratectl.go`, `ratectl_test.go`; modify `relaunch.go` `adaptLoop` to use `RateCtl` when the target is a `LiveBitRater` and at least one viewer is acking, otherwise `BitrateAdapter` as today.

**Interfaces:** `func NewRateCtl(maxBps, minBps float64) *RateCtl`; `func (r *RateCtl) Update(now time.Time, viewers []FlowStats, rttMs []float64) (targetBps float64)`.

Constants (voxl `RateCtl`, except the start and floor): evaluate every 100 ms; congested when any started viewer has `QueueMs > 30` or `OldestMs > rtt+80`; clear when every `QueueMs <= 15`. On congestion, at most every 300 ms: `t = target*0.85`; if the slowest congested viewer's `DeliveredBps >= target*0.5`, set ceiling to it and `t = min(t, rate*0.85)`; `target = max(t, min)`; leave slow start. Slow start (not congested, clear, ≥250 ms since last raise, best `DeliveredBps >= target*0.5`): `target = min(target*1.5, max)`. Otherwise when clear, ≥1 s since last cut and content uses ≥50% of target: forget the ceiling after 15 s; below `0.9*ceiling` raise `target*1.06 + 100 kbps` capped at `0.9*ceiling` every 200 ms; at or above it raise ×1.02 per 1 s. Start at `max` (loupe's sim max, 4 Mbps), floor `1.5 Mbps` (text at 664x1440 smears below that, streak-rec s135/s154). Send `SetBitRate` only when the target moves more than 5%.

- [ ] Tests: `TestRateCtlBacksOffOnQueue`, `TestRateCtlCeilingOnlyWhenNearTarget`, `TestRateCtlRecoversToNinetyPercentCeiling`, `TestRateCtlIdleScreenDoesNotRaise`, `TestRateCtlFloor`, `TestRateCtlCeilingForgotten`, `TestRateCtlHysteresis5Percent`.
- [ ] Implement, `go test ./...`, commit, deploy, phone run. Pass: bitrate stays ≥1.5 Mbps and `queueMs` p90 under 60 ms on cellular.

## Slice 4: paintover for simulators

### Task 8: simcap sharpens a still screen

**Files (simcap):** the idle re-encode path (`main.swift:240-248`, `phone.swift:164-171`) and `encoder.swift`.
- After 150 ms with no new captured frame, the next 3 idle re-encodes set `kVTCompressionPropertyKey_Quality` to 1.0 and lift `AverageBitRate` to 3× the current target, then restore both. Any new captured frame cancels the burst. No burst while paused (Task 4).
- [ ] Check: record the stream (loupe recording API) while scrolling then stopping at 1.5 Mbps; contact sheet frames at stop+0, +200, +500 ms; text at +500 ms matches the 4 Mbps reference sheet (`streak-rec/sheet/s4m.png`). Attach the sheet to the PR.
- [ ] Commit simcap, rebuild, deploy, phone run. Pass: the user sees text sharpen within half a second of stopping a scroll.

---

Not in scope: native iOS acks (LoupeClient in the handset repo, a follow-up once slice 1 proves the signal), live bitrate for scrcpy (needs a scrcpy server patch), voxl's regional refinement (VideoToolbox has no per-macroblock QP).
