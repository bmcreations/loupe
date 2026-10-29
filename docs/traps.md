# Traps already hit

Each of these cost real time. The symptom is given first, because that is how
you will meet it again. Section names such as Device frames or Adaptive bitrate
refer to `docs/architecture.md`, `docs/targets.md`, `docs/ios.md` and
`docs/agent-api.md`.

1. **fps reads ~130 when the encoder is set to 60.** x264 with
   `ultrafast`+`zerolatency` emits several slices per picture and each was
   counted as a frame. Fixed with `h264.StartsNewPicture()` reading
   `first_mb_in_slice`, plus `-x264-params sliced-threads=0`.

2. **Drop logic never fires under load.** Backpressure was at the wrong layer:
   the writer drained the Go channel into a blocking socket write with a 10s
   deadline, so the queue always looked empty. Fixed with staleness shedding
   (`session.MaxStaleness`, 150ms).

3. **rtt climbs to seconds with a nearly empty queue.** Backpressure at the
   wrong layer again, this time the kernel's `SO_SNDBUF`. Fixed with
   `tcp.SetWriteBuffer(64KB)` in `wsx`. rtt fell from 7.3s to 142ms.

4. **Everything looks healthy server-side and the phone is still behind.**
   Wrong layer a third time: `tailscale serve` accepted loopback writes
   instantly and buffered in the proxy, making the backlog invisible. This is
   why the pacer measures what the *phone* reports rather than anything local.

5. **Pacer collapses to its floor and stays there on a healthy link.**
   Thresholds were absolute against a relayed path with a 150-280ms baseline.
   Now every threshold is relative to the measured path baseline
   (`targetExcess`, `recoverExcess` in `pacer.go`). Covered by
   `TestPacerDoesNotCollapseOnASlowButHealthyPath`.

6. **Video freezes permanently after one overrun.** The jitter buffer's overrun
   guard nulled its baseline, and `present()` no-ops without one. Re-anchor
   instead of nulling, with `{ immediate: true }` so it does not re-anchor into
   the future. Covered in `web/jitter.test.mjs`.

7. **Safari runs a stale client and the behaviour makes no sense.** `embed.FS`
   has zero modtimes, so no `Last-Modified`, and there were no cache headers.
   Fixed with `no-store` plus content-hash stamped asset URLs (`stampIndex`).
   The stamp now covers the module's own imports too, not just the entry point.

8. **WebTransport server hangs on `AcceptStream`.** QUIC streams are lazy, so
   the client must send something first. Client sends `MsgHello` (0x15)
   immediately on connect.

9. **Init JSON arrives at the decoder as video.** Control messages shared the
   frame queue and the WebTransport path strips 18 bytes off frames. Fixed with
   `OutMsg.Control`, routed via `SendControl`.

10. **Playwright's bundled Chromium reports no H.264 support.** Verified:
    `VideoDecoder.isConfigSupported` returns false for every `avc1` string and
    true for vp8/vp9. That is a harness limitation, not a bug. Do not "fix" it.

11. **WebTransport fails on the phone and both ends look silent.** Safari
    reports a bare `WebTransportError` with an empty stack after ~7.5s, and the
    server logs nothing at all, not even the `wt-attempt` that fires on handler
    entry. The handshake was dying below HTTP: quic-go pads Initial packets to
    its default 1280-byte payload, which is 1308 bytes on the wire over IPv4 and
    1328 over IPv6, and Tailscale's tun is MTU 1280. The client's own Initial is
    padded to the 1200-byte spec floor and arrives fine, so the connection looks
    like it is opening; it is the server's reply carrying the certificate that
    never lands. Fixed with `quicConfig()` in `cmd/loupe/main.go`, pinning
    `InitialPacketSize` to 1200. Covered by
    `TestQUICInitialPacketFitsTheTunnel`.

    Reproduced with `cmd/wtprobe` against the tailnet address, not loopback:
    over the tunnel no datagram above 1231 bytes ever arrived, while loopback
    carried the full 1280 and completed. Dialing loopback proves nothing here.

12. **`address already in use` on a port nothing holds.** A double-click starts
    two copies of the `.command`; the second kills the first then binds a
    still-closing socket. The script is gone, but the underlying fact is not:
    `lsof` reports the port free while the kernel is still tearing the listener
    down. `wait_for_port` in the supervisor is that lesson. launchd also gives
    single-instance-per-label for free, which is what the lock directory was
    approximating. Separately, TCP 8445 and 8456 are not mysteriously
    unbindable on this Mac: `tailscale serve status` shows persistent proxies
    already bound to both (8445 -> 8890, 8456 -> 8456), along with 3200, 8443,
    8444, 8455, 8447, 8460, 9443 and :443 -> 8080. Check that list before
    blaming the kernel for a port. 18456 is clear.

13. **Safari completes the handshake, then hangs up without sending CONNECT.**
    The trace shows `quic started`, `version`, `alpn: h3`, then `closed` with
    `Application error 0x100 (remote)` about 90ms later, and no `wt-attempt` at
    all. H3_NO_ERROR, client-initiated: Safari read the SETTINGS frame, found no
    WebTransport setting it recognised, and left politely. webtransport-go
    advertises only draft-02's `SETTINGS_ENABLE_WEBTRANSPORT` (0x2b603742);
    Safari wants draft-07's `SETTINGS_WEBTRANSPORT_MAX_SESSIONS` (0xc671706a).
    Both go out now, via `webTransportSettings()` merged into
    `AdditionalSettings`. Keep both: browsers need the new one and
    `cmd/wtprobe`'s own client still checks for the old one.

14. **Everything about the WebTransport setup fails silently and identically.**
    Four separate misconfigurations all produce the same picture: a clean QUIC
    handshake, a plausible SETTINGS frame, and a browser that hangs up without
    the request ever reaching a handler. None of them log anything. They are
    collected in `newWebTransportServer` with one assertion each in
    `TestWebTransportServerIsFullyConfigured`, because the only way to tell them
    apart is the exact client-side error string:

    | Missing | Client says |
    | --- | --- |
    | `TLSConfig.NextProtos` | `CRYPTO_ERROR 0x178: server did not select an ALPN protocol` |
    | `SETTINGS_ENABLE_WEBTRANSPORT` | `server didn't enable WebTransport` |
    | `EnableDatagrams` | `server didn't enable HTTP/3 datagram support` |
    | `ConnContext` | `missing QUIC connection`, from `Upgrade` |

    The last three are what `webtransport.ConfigureHTTP3Server` does, which is
    an explicit call rather than something `Serve` does for you. The first is
    needed because `Serve` hands `TLSConfig` straight to `quic.ListenEarly`
    instead of letting `http3.Server` fill in ALPN. `cmd/wtprobe` surfaced all
    four in sequence, each one hidden behind the one before it.

15. **A real device freezes after one second and never recovers; synthetic
    only stutters.** The trace shows `shed` climbing for a minute while `sent`
    stays at 0 and every write counter sits frozen at the value it had when the
    last write happened. `pacerKeep` is 1 and `serverDrop` is 0, so nothing is
    throttling and nothing is being dropped -- it is all shed. `needKey`
    latches on the first shed and clears only when a keyframe arrives, and
    **scrcpy does not emit keyframes on a timer**. Verified on an emulator: six
    idle seconds produced exactly one keyframe, the initial one. The GOP is
    effectively unbounded, so one shed is permanent. `Synthetic` emits a
    keyframe per second, which is its own behaviour and not the protocol's, so
    it recovered every second and floored at 1-3fps instead of 0. That is why
    this was invisible until a device was attached. Fixed with
    `RequestKeyframe()` down the `target.Stream` interface, called from the
    shed path in `viewer.go` and rate-limited to `keyframeRequestInterval`
    because under sustained shedding it would otherwise fire once per frame.
    Opcode is `TYPE_RESET_VIDEO` = 17, read out of scrcpy-server 3.3.4's dex
    with `dexdump -d` and confirmed live: the device produced a keyframe 130ms
    after the request. Covered by `TestEncodeResetVideo`.

16. **Every device target fails at `acquire` with `read dummy byte: EOF`,
    about 200ms in.** Under `tunnel_forward=true` the host end of the adb
    forward is adbd, not scrcpy-server, so adbd accepts the TCP connection
    immediately and only then tries to reach the abstract socket on the device.
    While the server is still starting that fails and adbd closes, which
    arrives as EOF. A successful dial therefore proves nothing. `dialRetry`
    retried only the dial and the dummy-byte read sat outside the loop, so a
    normal startup delay became a hard failure well inside the 10s budget.
    `dialVideo` now retries connect and dummy byte together; that is safe
    because EOF means adbd rejected the connection and no server-side socket
    slot was consumed. The device says it plainly, so read logcat before
    theorising:

    ```
    E adbd: failed to connect to socket 'localabstract:scrcpy_5b2a6abd':
            could not connect to localabstract address
    ```

17. **A hand-rolled scrcpy repro fails in ways the real code never would.**
    scrcpy-server deletes its own jar on exit -- `cleanup` defaults to true --
    so the second manual run onwards dies with `ClassNotFoundException` and the
    shell prints only `Aborted`. loupe pushes the jar on every `Launch`, so it
    never sees this. Two runs of the same script disagreeing is the tell. Also
    `scid` is parsed with `Integer.parseInt(s, 16)` and must fit a signed
    31-bit int: anything above `7fffffff` aborts with `NumberFormatException`.

18. **The device screen runs off the bottom of the viewport on iOS.** Two
    independent causes, and fixing either alone leaves it broken. `html, body`
    had `height: 100%`, which on iOS resolves against the initial containing
    block and stays viewport-height with Safari's toolbars hidden -- with them
    shown the page is taller than what you can see. Now `100dvh`, with the
    percentage kept above it as a fallback. Separately `#screen` had
    `max-height: 100%` against `#screenWrap`, whose height is `auto`, so the
    percentage had nothing to resolve against and was dropped; a 20:9 device
    overflowed every time. `fitScreen()` in `app.js` now measures the stage and
    assigns pixels, driven by a `ResizeObserver`, which has no such
    circularity. Contain fit on both axes, deliberately: a tab bar along the
    bottom edge of the device is unusable if it is under the fold.

19. **A device streams for three seconds, then every counter freezes at once
    and stays frozen.** `framesOut`, `serverDrop` and `shed` all sit at the
    value they had in one particular second, for the rest of the session, while
    `viewers` is 1, `pacerKeep` is 1 and the client keeps reporting rtt of
    50-77ms. It looks exactly like a device that stopped sending, which is the
    trap: it is the *same* keyframe latch as trap 15, one layer up.

    `Session.broadcast` marks a viewer `needsKey` when its fan-out channel is
    full, and thereafter discards every inter-frame on a `continue` that
    incremented no counter at all. Nothing was measuring the branch, so a
    latched viewer and a silent device produced byte-identical trace rows. The
    viewer loop already called `RequestKeyframe` when it shed; this path asked
    for nothing, and scrcpy emits no keyframes on a timer, so one full channel
    was permanent.

    The tell is in the second before the freeze, and only in `serverDrop`:

    ```
    13:49:38 | d_out 24  d_serverDrop 2  d_shed 16
    13:49:39 | d_out  0  d_serverDrop 0  d_shed  0   <- and every second after
    ```

    Fixed by counting the suppressed frames as `framesWait`, traced as
    `awaitKey`, and by calling `askKeyframe` from the drop branch, rate limited
    to `keyframeRequestInterval` and run off the producer goroutine because the
    control socket write carries its own 2s deadline. A climbing `awaitKey` now
    means frames are arriving and the fan-out is holding them; a flat one means
    the source really is quiet. `stream-end` records `Err()` for the third case.
    Covered by `TestBroadcastAsksForAKeyframeWhenAViewerFallsBehind` and
    `TestSuppressedFramesAreCounted`.

    Two prior theories died against the source and are worth not repeating: the
    producer cannot block on a full viewer channel, because `broadcast` is a
    `select` with `default`; and `RequestKeyframe` cannot wedge it either,
    because `SendControl` sets a 2s write deadline.

20. **A device reads 10fps with `max_fps=60`, and the spacing is suspiciously
    regular.** Dead-regular 100.0ms gaps look like a configured cap, so the
    instinct is to hunt for the thing setting 10. Nothing is: 10fps is the
    panel's own adaptive refresh rate, and `10.0` is one of its advertised
    modes (`supportedRefreshRates=[120, 90, 60, 30, 24, 10, 1]` on a
    ACME00000000001). An idle screen has nothing new to show. The same device
    and the same flags, measured with `cmd/wtprobe`:

    ```
    sustained motion   533 frames in 9s   pts p50  16.6ms   ~59.2 fps
    idle                36 frames in 6s   pts p50 100.0ms   ~6 fps
    ```

    So a low `framesOut` on a quiet device is correct and not worth chasing,
    and the earlier "17 frames in 6s" reading was measuring an idle screen.
    Drive real motion before reading a frame rate: `adb shell input swipe` with
    gaps between the swipes spends most of its time idle and still reads 10fps.
    Rule out the pacer first, though -- decimating 60fps to keep one frame in
    six produces the same dead-regular 100ms PTS spacing. `pacerKeep 1` with
    `shed 0` in the trace is what separates them.

21. **The stream never comes back after the phone locks, and a reload is the
    only way out.** The reconnect logic is not missing and it is not broken:
    it fires once, while the page is merely hidden, and then never again. The
    trace shows the whole arc, and the gap at the end is the symptom:

    ```
    14:34:18  page hidden. Server streams 77s more, sent == framesOut, 0 errors
    14:35:32  stream-end
    14:35:33  client reconnects by itself, still hidden -- new conn, all probes ok
    14:36:12  payErr 1, "payload: deadline exceeded"   <- client stopped reading
    14:36:34  quic closed, by local, err "idle_timeout"
              ... 56 minutes, zero connect attempts ...
    15:32:08  GET / and boot -- a manual reload
    ```

    iOS does not throttle a locked page, it freezes it. rAF stops, timers stop,
    and the `setTimeout(connect, delay)` that `onClose` schedules may never run
    -- or never be scheduled, if the freeze lands before `onClose`. The second
    reconnect at 14:35:33 worked only because the page was still alive behind
    another app.

    Foregrounding was already handled, but the handler read
    `if (!document.hidden && state.ws && state.ws.open)` and so did nothing in
    the one case that matters, a connection that died while hidden. It now
    reconnects when the transport is not open, resetting the backoff, and
    `pageshow` is wired to the same path because a page restored from the
    back/forward cache can return without firing `visibilitychange` at all.

    The client also drops the transport on the way out, from the
    `visibilitychange` that still runs before the freeze. Nothing can be
    painted while hidden, and the 114 seconds of frames the server wrote into a
    frozen client are what produced the deadline and then the idle timeout.
    Both paths are traced as `suspend` and `resume`, because a resume that
    fires and fails looks identical to one that never fired.

    Suspend and resume live in `web/pane.js` now. `web/pane.test.mjs` pins
    them in `suspend then resume opens one new transport`: the suspended
    transport's late close queues no reconnect, and resume opens exactly one.
    The freeze itself is not reproducible there. Verify from the trace.

22. **A target is selected, the session opens, and the screen stays blank
    forever.** Same latch as traps 15 and 19, third location, and this one is
    on the join path so it needs no load at all to fire. The trace is flat from
    the first second:

    ```
    15:58:52 | out 0  await 10  shed 0  drop 0  sent 1 | fps 0  rtt 156  keep 1.00
    15:58:58 | out 0  await 10  shed 0  drop 0  sent 1 | fps 0  rtt 211  keep 1.00
    ```

    `awaitKey` frozen at 10 rather than climbing reads, by trap 19's own
    discriminator, as a device that has gone quiet -- and on an idle emulator
    that is nearly true (trap 20), which is what makes it convincing. It is
    not: `AddViewer` sets `needsKey: true` on every viewer, correctly, because
    a joiner lands mid-GOP and decoding from there produces garbage. But
    `askKeyframe` was reachable only from `broadcast`'s drop branch, so a
    viewer that joined without immediately receiving a keyframe had nothing
    that would ever ask for one, and scrcpy emits none on a timer. Ten
    inter-frames were suppressed, the panel went idle, and the wait was
    permanent. `AddViewer` now calls `askKeyframe` after releasing `mu`,
    through the same rate limiter. Covered by `TestJoiningAsksForAKeyframe`.

    Replaying `lastCfg` to a joiner is not a substitute. SPS and PPS configure
    the decoder; they do not give it a picture to start from.

23. **`i-frame-interval` is counted in frames, not seconds.** The GOP is
    bounded now -- `video_codec_options=i-frame-interval:int=1` goes out on
    every launch and defaults to one second -- but the encoder converts that
    interval using the frame rate it was configured with, not the wall clock.
    At `max_fps=60` the GOP is 60 frames, so on an idle panel running at its
    adaptive 10Hz floor (trap 20) one second of encoder time is six seconds of
    real time. Measured on an Acme One with `cmd/wtprobe`:

    ```
    idle,   interval 1    44 frames in 10s    1 keyframe
    motion, interval 1   344 frames in 10s    6 keyframes
    motion, interval 5   377 frames in 10s    2 keyframes
    ```

    Both motion rows are one keyframe per GOP, which is what shows the flag
    reaches the encoder. So this bounds the trap 15/19/22 family by frame
    count, not by time: a latch on a busy screen clears within a second, and on
    an idle one it can take several. That is still a stall rather than the
    permanent freeze an unbounded GOP gave, and an idle screen has nothing new
    to show anyway.

24. **H.265 streams end to end and still cannot decode.** `-video-codec h265`
    reaches the device, and the frames come back clean: 203 frames in 8s under
    motion, keyframes flagged, first frame starting `00 00 00 01 40` -- an HEVC
    VPS. scrcpy's own frame metadata delimits access units, so none of the
    Annex B picture-boundary logic is load-bearing on the device path, which is
    why the framing survives a codec it knows nothing about.

    What does not survive is the codec string. `resolveCodec` derives it with
    `h264.FindSPS`, which looks for NAL type 7; HEVC's SPS is type 33, so it
    finds nothing, returns, and the client is configured with the provisional
    `avc1.42E01E` and fails in `VideoDecoder`. Making h265 work is an HEVC
    parameter-set parser emitting `hvc1.*`, not a flag. The flag stays because
    it is how that experiment gets rerun, and the branch now logs rather than
    returning in silence.

25. **The pacer flatlines at its floor after the phone changes network, and
    the client's own rtt numbers look fine.** A 5G trace ran healthy for
    fifteen seconds -- rtt 7.8-9.0ms, 40-56fps, `keep 1.00`, `shed 0`, the best
    figures recorded against a real device -- then from 07:36:18 to 07:36:30
    `pacerKeep` sat pinned at 0.10 with `pacerBaseMs` still reading 7 while the
    client reported 500-2000ms.

    Not trap 5, which was absolute thresholds. These thresholds are relative;
    the *baseline* had stopped tracking. `baseWindow` was 200 samples, and a
    sample-counted window has no fixed duration: it is fed once per frame, so
    at the 29fps that link managed it spans seven seconds, and the phone gave
    up first. Recovery needs excess under 60ms, so against a stale 7ms floor it
    wanted rtt below 67ms on a path whose real floor was 233ms. There was no
    route back. The baseline is ten 500ms slots now, minimum across them:
    a handover is learned in five seconds, and a two-second burst of queuing
    spoils at most four slots. `TestPacerRelearnsTheBaselineAcrossAHandover`
    and `TestPacerBaselineSurvivesABurstOfQueuing` are the two directions.

    A warm-up guard was tried here and removed: it survived mutation because it
    cannot help. A viewer inherits the previous viewer's reading, since
    `clientStats` is session state, and a warm-up still lets that one sample
    set the floor. What fixes it is not inheriting a stale number.

26. **The controller acts six seconds late, and every counter says the link is
    fine right up until it isn't.** `pacerKeep` held at 1.00 through 07:36:43,
    44 and 45 while `shed` climbed 270 to 315 and `stalls` went 11 to 17. The
    client block in those three rows is *byte-identical* -- rtt 111.3, jitter
    132.9, fps 2 -- because no fresh report arrived. It backed off at 07:36:46
    on a sample reading 972ms.

    The pacer's only signal was an `/api/log` POST from the phone, which queues
    behind the video the pacer is throttling, so it goes stale exactly when it
    is needed. Eight stale runs of 3-7s appear in 96 rows, each bracketing a
    congestion event. The signal cannot be fixed by sending it more often; it
    is on the wrong side of the queue.

    `quic.Conn.ConnectionStats().SmoothedRTT` is maintained per ACK beneath
    anything that can queue it. Reaching it is the subtle part:
    `ConfigureHTTP3Server` *chains* to an existing `ConnContext` rather than
    replacing it, so ours goes on the `http3.Server` first. Both orderings fail
    silently and differently -- set ours second and webtransport-go's hook is
    dropped, giving trap 14's `missing QUIC connection`; omit ours and nothing
    fails at all, `PathRTT` returns zero forever and the pacer falls back
    without saying so. `TestWebTransportServerIsFullyConfigured` asserts both.

    Keep the client's number. It is the only measure that includes the phone's
    own decode and render backlog, it is the fallback on WebSocket and before
    the first ACK, and the gap between the two separates a slow link from a
    slow client. It carries a timestamp now and a reading older than 2.5s is
    discarded rather than re-fed, because holding the send fraction steady is
    an honest answer to having no signal. `rttSource` says which was used.

27. **`shed` counts three unrelated faults, and `shedRate` lies after every
    reconnect.** Pacer rejection is the controller working as designed,
    staleness is the link failing, and a keyframe wait is the decoder unable to
    use anything until the encoder answers. All three incremented one counter,
    so the 07:36 trace could only be read by arithmetic against `pacerKeep`.
    That is trap 19's unmeasured-branch shape a fourth time. `NoteShed` takes a
    `ShedReason` now and the trace carries `shedPacer`, `shedStale` and
    `shedAwaitKey`, which sum to `shed`.

    Separately, shed counts are session-scoped and survive a viewer while
    `lastShed` was viewer-scoped and restarted at zero, so the first row after
    a reconnect reported the whole accumulated total as one second's rate --
    200 and 207 in a stream doing about 30fps. Seeded from the session now.

28. **Frames are abandoned by the clock rather than by the link.** Twelve rows
    report `writeMaxMs` within 8ms of 250.0 with `lastErr: "payload: deadline
    exceeded"`, against a `writeAvgMs` of 27. `frameDeadline` (250ms) and
    `MaxStaleness` (150ms) were absolute, which assumes a fast path -- trap 5's
    mistake one layer down, fixed for `targetExcess`/`recoverExcess` and never
    for these. The two then disagree about the same path: the pacer can call a
    233ms baseline perfectly healthy while every frame on it trips a 150ms
    staleness check, because roughly 115ms of that is propagation before
    anything has gone wrong. Both add half the measured baseline now, capped
    (600ms, 1s) so a wild reading cannot disable shedding or leave one frame
    holding the transport. `session.StalenessFor` and `frameDeadlineFor` share
    the derivation so they cannot drift.

29. **A viewer stalls and the trace says the device went quiet.**
    emulator-5554 ended a 121s session with `drop 746`, `framesOut 737` and
    `awaitKey 0`. Dropping more frames than it delivered while suppressing none
    is only reachable one way, and it is the opposite of what it looks like.

    A keyframe skips `broadcast`'s resync branch by design -- it is the frame
    that ends the wait -- so it reaches the `select`, where a full channel drops
    it. Every drop called `askKeyframeSoon`, whose floor is
    `keyframeEdgeInterval` 50ms. At the 12fps that run was capped to, that is a
    fresh IDR request per frame: the encoder answers, the answer lands on a
    channel still full, it is dropped, and the drop asks again. A stalled viewer
    turns the whole stream into keyframes -- the largest frames there are, at
    the frame rate, on a link already too slow.

    `askKeyframeSoon` was written for trap 19 and bounds *control messages*, not
    IDRs. 20 control messages a second is nothing; 20 keyframes a second is the
    entire link. The ask fires on the edge into `needsKey` now, and again only
    once the channel has drained to half. One free slot is not room: the
    keyframe lands, the inter-frames behind it re-latch, and the loop resumes
    one frame on. `TestAStalledViewerDoesNotTurnTheStreamIntoKeyframes` and
    `TestANearlyFullViewerIsNotAskedForAKeyframe` are the two halves, with
    `TestADrainedViewerIsAskedForAKeyframeAgain` guarding the other direction --
    suppressing the ask entirely would buy trap 22's permanent blank screen.

    This is trap 19's unmeasured-branch shape a fifth time, and the trap 19
    tests missed it because none of them ever broadcast a keyframe. The trace
    could not separate it from a slow link either: `framesIn` and `framesDrop`
    move the same way under both. `keysIn` is recorded per second now.

30. **The jitter buffer sits at its ceiling while nothing is being lost.**
    `bufferMs` read 400.0 in 42 of 137 rows across a run where `drop`, `shed`
    and `awaitKey` were 0 for all 185 seconds. Nothing was being dropped and
    the buffer was still at maximum, which is pure added latency on something
    you are trying to tap.

    `client.jitterMs` is what drives it and is itself the bug: 17.3 at the
    start, 238.4 eleven seconds later, then 1203.8, 2183.0 and 4968.8. Each of
    those jumps lands on the first frame after an idle gap. `jitterBuf.tick`
    sets `desired = clamp(jitter * 1.5, min, max)`, so anything past 267ms
    holds the buffer at `maxBufferMs` and keeps it there.

    The estimator was RFC 3550's interarrival jitter with the sending term
    dropped -- `|arrivalGap - previousArrivalGap|` into a 1/16 EWMA. RFC 3550
    defines D(i,j) as `(Rj - Ri) - (Sj - Si)`, and approximating `Sj - Si` by
    the previous arrival gap is sound only at a constant rate. scrcpy is not
    one: an idle screen emits nothing for twenty seconds and then bursts
    (trap 20), so a stream whose transit never varied reports 24917ms of
    variation, and pays it twice -- once on the gap, once on the frame after.

    Every frame already carries `ptsMicros` on both transports, so the sender's
    own spacing was available and unused. `ArrivalJitter` in `web/jitter.js`
    uses it, and an idle gap now grows both terms together and contributes
    nothing. A backwards PTS re-anchors rather than feeding the average.

    The backwards guard is for a target switch, which moves PTS between two
    devices' uptimes; a relaunched encoder does not restart at zero. Trap 31
    completes the fix.

    It lives in `jitter.js` and not `app.js` because `app.js` still has no
    harness (trap 21). Four cases in `web/jitter.test.mjs`, each of which fails
    against the old form.

31. **The jitter buffer pins at its ceiling again, and lowering the ceiling
    only lowers where it pins.** With `maxBufferMs` dropped to 50 after
    trap 30, `bufferMs` read 49.4 to 50.2 in all but a handful of 113 rows --
    the same fault trap 30 described at 400. `client.jitterMs` peaked at
    182411579.7 and decayed monotonically for the following minute, which is
    the signature of one enormous sample rather than a sustained misreading.

    **PTS is absolute device uptime, and a parameter-set frame reports 0.**
    Measured back to back on emulator-5554:

    ```
    uptime before   352072.23 s
    frame id=1  pts          0.00 s  flags 0x02   config
    frame id=2  pts          0.00 s  flags 0x02   config
    frame id=3  pts     352074.20 s  flags 0x01   media
    uptime after    352074.25 s
    ```

    `handleFrame` called `arrival.observe` *above* its `if (f.config) return`,
    so the estimator was the one consumer that ever saw a config frame. The
    pair straddling one yields a `d` equal to the device's whole uptime --
    3.5e8ms on this emulator, 2.7e9ms on an Acme One -- and at EWMA weight 16 that
    takes about sixty samples to decay, holding `desired` at `maxBufferMs`
    throughout. Config frames arrive on every connection and again when the
    encoder re-sends parameter sets, which is why every `wt-open` spiked and
    one row spiked mid-session.

    Two fixes. `arrival.observe` moved below the config guard, because a
    parameter-set frame is not a presentation unit. And `ArrivalJitter`
    discards a sample whose implied transit change exceeds
    `maxPlausibleTransitMs` (5s): the transport abandons a frame at a deadline
    that caps at 4s, so nothing it delivers can legitimately disagree by five,
    and the buffer is capped in the low hundreds of milliseconds regardless.
    That was 1s when this was written (trap 28) and the margin was comfortable;
    trap 36 raised the keyframe deadline and spent most of it, which is why the
    keyframe cap stops at 4s rather than 5. The guard lives in `jitter.js` and not `app.js`
    because `app.js` still has no harness (trap 21), and it catches every
    other discontinuity of this shape -- a target switch moves PTS between two
    devices' uptimes, in either direction.

    Three cases in `web/jitter.test.mjs`, each failing with the guard removed
    or its threshold raised. Dropping the threshold to 1ms breaks the
    real-variation cases instead, so it is pinned from both sides rather than
    being a bound that can be widened until it does nothing.

    Lowering `maxBufferMs` is what made this visible and is worth noting as a
    method: the ceiling was hiding how far past it the estimate had gone.

32. **The bitrate controller aims at a rate far below what the link just
    carried, and every counter behind the decision looks reasonable.** Two
    rows from one window, 49 seconds apart:

    ```
    20:43:46  upWindow  windowMs 10999  frames 124  goodput   981,995
    20:44:35  upWindow  windowMs 59999  frames 158  goodput   194,172
    ```

    34 frames arrived in those 49 seconds -- an idle panel at its 10Hz floor,
    trap 20 -- and goodput fell 5.06x with nothing about the link changing.
    The first row of that window read 5,411,563.

    `goodput` divided `Bytes` by the whole wall-clock window, so every idle
    second counted as a second the link carried nothing, and the down branch
    used the figure **directly** as the new encoder bitrate. A session that
    idles and then sheds relaunches toward the 250 kbps floor on a link that
    had just carried megabits. The failure is in the direction that matters:
    the one path whose job is to recover is the one that adapts backwards.

    Goodput is measured over accumulated *active* time now -- intervals in
    which the encoder produced -- and each interval is charged what it
    actually spanned rather than an assumed second. The report carries
    `activeMs` beside `windowMs`, because the two agree until the screen goes
    idle and the gap between them is the whole fault. Pinned by
    `TestAnIdleStretchDoesNotDepressTheMeasuredGoodput`, which reads 1.0 Mbps
    on a link that carried 5.0 before the change.

    Not observed against a device: a run that would trigger it needs a starved
    link, which is what `cmd/linkshaper` exists to produce.

33. **Every target fails at `acquire`, and the error names a port that is
    plainly free.** Both a device and an emulator, on different ports:

    ```
    {"kind":"wt-error","stage":"acquire",
     "err":"connect control socket: dial tcp 127.0.0.1:49842: connect: can't assign requested address"}
    ```

    Nothing holds 49842, which sends you looking for a stale adb forward.
    There was none: `adb forward --list` was empty. `can't assign requested
    address` on a *dial* is not about the destination. It is the kernel
    failing to find a free **local** ephemeral port for the 4-tuple, and it
    had 9 of 16384 left:

    ```
    net.inet.ip.portrange.first: 49152   last: 65535     (16384 ports)
    distinct local ports in TIME_WAIT:   16375
    ```

    This is host-wide and nothing to do with loupe -- 40507 TIME_WAIT entries
    across Figma, adb, AirPlay and a long tail of services, only 758 of them
    adb's. It stops every outbound loopback connection on the Mac, which is
    why it takes out the adb control socket and `/api/config` alike.

    **`netstat` cannot diagnose this.** An entry whose timer expired but has
    not been reaped is byte-identical to a live one, so a frozen count proves
    nothing either way. Dial in a loop instead and count: 200 outbound
    loopback dials returned `ok=1 fail=199` with the same error, which settles
    it in one run.

    `msl` is 15s here, so TIME_WAIT should clear in 30s. It did not move
    across two full lifetimes with the churner already stopped. The way out is
    a wider range, which needs root:

    ```sh
    sudo sysctl -w net.inet.ip.portrange.first=16384   # 49151 ports, resets at reboot
    ```

    One contributing habit is worth dropping regardless: driving motion with a
    loop of `adb shell input swipe` opens a fresh connection to the adb server
    per invocation. Push the loop onto the device -- one `adb shell` running
    `for i in $(seq ...)` -- and 90 connections become 1.


34. **The controller declines every window while the viewer runs at 1.8fps.**
    A link shaped to 400 kbps against a 4 Mbps encoder starved a session for
    70 seconds and the adaptive bitrate controller never acted. Across
    21:22:45 to 21:23:54 on emulator-5554:

    ```
    framesOut     997 -> 4062   3065 produced
    sent          973 -> 1102    129 delivered
    shedAwaitKey   13 -> 2678   2665 suppressed
    shedStale       1 ->  119    118
    shedPacer       1 ->   13     12
    ```

    `shedFrac` was (12+118)/3065 = 0.04 against the 0.20 that would act, so
    three consecutive 60s windows returned `inconclusive`. 95% of the evidence
    sat in the one counter trap 27 forbids the controller to read.

    Trap 27's exclusion is still right: one drop causes many waits, so counting
    them frame by frame lets a single frame argue sixteen times over. What it
    missed is that a wait sustained across a whole window is not the echo of
    one drop. Nothing clears a wait but a keyframe landing, and a keyframe is
    the largest frame there is, so a wait still open after ten seconds is the
    encoder stating that its own smallest useful unit does not fit.

    Gated on that directly now: `awaitKey / frames >= 0.50` across the down
    window, reported as `awaitFrac` whether or not it fires. It aims at
    measured goodput, with a halving as the ceiling on one step; goodput is
    only trustworthy for that because it no longer counts frames the
    transport abandoned (trap 35).

    The demonstration, through `cmd/linkshaper` at 400 kbps with motion driven
    on the device. This is the first observed full cycle:

    ```
    21:36:03  starved  4000000 -> 2000000   awaitF 0.505  shedF 0.024
    21:36:25  starved  2000000 -> 1000000   awaitF 0.872  shedF 0.040
    21:36:46  starved  1000000 ->  500000   awaitF 0.886  shedF 0.040
    21:37:09  starved   500000 ->  250000   awaitF 0.890  shedF 0.041
    21:38:21  up        250000 ->  375000   awaitF 0.000  shedF 0.000
    ```

    `shedFrac` never leaves 0.024-0.047 across the whole descent, which is what
    makes this the sixth instance of the unmeasured-branch shape: the signal
    the controller was reading stayed flat and healthy while the one it was not
    reading went to 0.89. Both counters drop to exactly 0.000 the second the
    shaper lifts.

    `awaitFrac` occasionally reads just over 1.0 (1.02, 1.05). `framesIn` is
    loaded before the shed counters, so a frame arriving between the two loads
    counts as suppressed without counting as produced; it corrects on the next
    interval. Noise around the ratio, not drift, and irrelevant to a ten-second
    average against 0.50.

    **The decision is right and it arrives too late to help.** Measured over
    the same starved phase, the viewer got 2.2fps of 46.3 produced -- 4.8%,
    against the 1.8fps that provoked this. That 2.2fps is an overcount and the
    real figure is zero: it was read from `sent`, which at the time credited a
    frame the transport had reset mid-flight. See trap 35. Recovery is unambiguous, 45.6fps of
    46.9 produced (97.2%) once the shaper lifts, so the mechanism works; what
    does not is the descent. `downWindow` 10s plus `settleWindow` 12s is 22s a
    step, and 4 Mbps to the 250k floor is four steps, so 85 of the 90 impaired
    seconds were spent still oversubscribed. Fixing the signal was necessary
    and is not sufficient: the open question is whether a window this
    conservative is warranted when `awaitFrac` reads 0.89, or whether an
    unambiguous starvation should take a larger first step than a halving.

    Both were then measured, in trap 35. The descent is two steps over 22s now
    rather than four over 88, and it changes nothing the viewer can see,
    because the binding constraint on this link is the frame deadline rather
    than the encoder's rate.

    `TestAViewerStuckAwaitingAKeyframeIsTreatedAsAStarvedLink` is built from
    the measured numbers. `TestABriefKeyframeWaitDoesNotChangeTheEncoder` is
    trap 27's direction. Raising the threshold to 0.99 breaks the first,
    dropping it to 0.02 breaks the second, so it is pinned from both sides
    rather than being a bound that can be widened until it does nothing.

35. **The controller reads megabits of goodput off a link that is delivering no
    video at all.** A session shaped to 400 kbps reported `goodput` 1.56 Mbps,
    and after four descent steps still reported 685 kbps with the encoder
    already at 500 kbps. Trap 34 read 2.2fps of delivered video off the same
    counters. The real figure was zero: across the whole impairment the viewer
    received two 46-byte parameter-set frames per second and no pictures.

    `wtConn.SendFrame` returned `nil` for a frame it had reset mid-flight.
    Abandoning a stale frame is the entire reason WebTransport is here -- TCP
    cannot recall a frame once written -- but one layer up, "delivered" and
    "reset in flight" were the same value. Two decisions read it. `NoteSent`
    credited the bytes to goodput, which is the only direct measurement of what
    the link carries. And `if msg.Key { needKey = false }` ended the keyframe
    wait on a keyframe the client never got, after which the viewer forwarded
    inter-frames the decoder had no anchor for until one went stale and
    re-latched it. Seventh instance of the unmeasured-branch shape, and the
    first where the branch reported success rather than nothing.

    Every second of the run reads identically, at 4 Mbps and at the 250k floor
    alike:

    ```
    out +44   sent +2   bytes +92   aband +2   await +43   stale +0   payErr +2   base 379ms
    ```

    `SendFrame` returns `errFrameAbandoned` from both abandon paths now.
    `frameDelivered` maps that to neither delivered nor fatal, so the viewer
    re-latches `needsKey`, asks for a keyframe and counts `ShedAbandon` -- a
    fourth shed reason, because staleness is decided *before* the write and
    never reaches the link, while this consumed link time and arrived nowhere.
    The sampler folds Abandon into `Shed` for exactly that reason, and leaves
    `AwaitKey` out for trap 27's.

    With the accounting honest the descent is two steps over 22 seconds rather
    than four over 88:

    ```
    22:02:08  starved  4000000 -> 1477995  awaitF 0.508  shedF 0.025  good 1477995  win 48000
    22:02:30  starved  1477995 ->  250000  awaitF 0.893  shedF 0.048  good      775  win 10000
    ```

    The first step aims high because its window had been accumulating since
    before the shaping; the second window is purely starved and reads 775 bps.
    Recovery is one second: `awaitFrac` 0.893 to 0.000 and goodput 775 to
    1,235,646 as soon as the shaper lifts. `GateStarved` aims at measured
    goodput now with a halving as the *ceiling* on the step rather than the step
    itself, which is only safe because goodput no longer counts abandoned
    frames.

    **And it still delivers nothing, which answers trap 34's open question in
    the wrong direction.** The descent being four steps was not what kept the
    viewer black; at the 250k floor the trace is unchanged -- `sent +2`,
    `bytes +92`, `aband +2`. The binding constraint is the deadline, not the
    rate. `pacerBaseMs` sat at 370-415ms, so `frameDeadlineFor` gave roughly
    440ms, while a keyframe at 250 kbps on a 400 kbps link behind a 300ms queue
    needs about twice that to drain. The three `tooSmall` gates at 22:02:50,
    22:03:01 and 22:03:11 are the controller correctly asking for a rate below a
    floor it cannot cross. Trap 34's descent was never tested against this,
    because there the shaper lifted one second after the fourth step.

    A keyframe is the one frame whose loss is unrecoverable and which every
    other frame waits on, so a deadline that treats it like an inter-frame
    abandons the only thing worth waiting for. It has its own deadline now, and
    the factor was measured rather than guessed -- trap 36.

    `TestAnAbandonedFrameIsNeitherDeliveredNorFatal`,
    `TestSamplerCountsAbandonedFramesAsShedding` and
    `TestAStarvedWindowNeverRaisesTheBitrate` are the three directions. Removing
    `+ shedBy.Abandon` from the sampler left every other test green, which is
    what the second one exists to stop.

36. **Every frame the decoder cannot proceed without is killed by the clock,
    and the shed gate two hundred lines up says it never should be.**
    `broadcast` states the policy outright -- parameter sets and keyframes are
    never shed, because dropping them leaves the decoder with nothing to
    resynchronize against -- and then handed them to `frameDeadlineFor`, which
    abandoned them like any inter-frame. Trap 35 measured what that cost: on a
    400 kbps shaped link the viewer got no pictures at any encoder rate, two
    46-byte parameter sets a second and nothing else.

    **The write itself is what runs long, not the wait in front of it.** That
    distinction is not available from `writeMaxMs`, which averages keyframes in
    with inter-frames an order of magnitude smaller, so the keyframe counters
    are separate: `keyWrites`, `keyWriteAvgMs`, `keyWriteMaxMs`, `keyAbandon`,
    and `keyQueueMaxMs` for the wait between queuing and the write starting.
    The deadline runs from `QueuedAt`, so a keyframe could in principle spend
    its whole budget queued behind other frames. Measured, it does not:

    ```
    shaped    keyWriteMax 220-570ms per second, peak 1186.5   keyQueueMax 0.0ms in most seconds, peak 336.8
    unshaped  keyWriteMax 0.1-0.5ms                           keyQueueMax 0.0ms
    ```

    So 440ms was short by more than a factor of two at the worst. Do not assume
    which term it is: an abandoned write reports nothing about how much longer
    it needed, and the two are indistinguishable without the queue counter.

    `keyframeDeadlineFor` is a 2s floor plus **2x** the baseline, capped at 4s.
    The multiplier is larger than `frameDeadlineFor`'s 1/2x because the frame is
    an order of magnitude larger and pays the path cost over many more round
    trips. The floor carries the observed 1186.5ms worst case with about 2x to
    spare, and the cap was never reached.

    **The cap is 4s because of a client constant, not because it is a round
    number.** `maxPlausibleTransitMs` in `web/jitter.js` is 5s, and trap 31
    justifies that value by this deadline -- a frame the transport delivers
    cannot legitimately imply a larger transit change than the longest the
    transport was ever willing to wait for it. A cap at or above 5s makes the
    client discard samples from frames the server deliberately waited for, and
    trap 31's guard stops being sound. Changing one of these two numbers means
    reading the other.

    Same shaping as trap 35 (`cmd/linkshaper` 400 kbps / 120ms / 30ms jitter /
    300ms queue, emulator-5554, motion from one on-device loop):

    ```
    before   92 bytes/s delivered, 0 pictures, goodput 775 bps
    after    2325 frames, 140 keyframes, 10.36 MB in 100s, keyAbandon 0
             sent 4-9/s of ~45 produced, ~45 KB/s, goodput 330-366 kbps
    ```

    Goodput reading 330-366 kbps against a 400 kbps shaper is the measure
    finally agreeing with the shaper, which is what makes the controller's
    decisions worth acting on. It cost nothing on a healthy link: unshaped,
    `sent` equals `framesOut` and a keyframe write is under half a millisecond,
    so the longer deadline never binds.

    **What the viewer gets is still a keyframe-only stream.** `awaitFrac` stays
    at 0.52-0.77 through the shaped phase and the controller now gates
    `tooSmall` rather than descending further, correctly: goodput 366 kbps is
    above the 250 kbps the encoder is already at, so the link is no longer the
    binding constraint. The cycle is that a keyframe lands, the inter-frames
    behind it shed and re-latch the wait, and the next keyframe ends it -- about
    4 keyframes a second carrying nearly the whole payload. That is a 4fps
    slideshow rather than a freeze, which is the improvement; making the
    inter-frames between them survive is a separate question and is not
    measured.

    `TestAKeyframeGetsLongerThanAPictureItReplaces` and
    `TestTheKeyframeDeadlineScalesAndIsStillBounded` pin it from both sides, and
    the way they first failed to is worth keeping. Every assertion in them was
    originally relative -- `key < 2*picture`, `fast < keyframeDeadline`,
    `keyframeDeadlineFor(time.Hour) != keyframeDeadlineCap`. All three move with
    the constant they are checking, so dropping the floor to 250ms and raising
    the cap to an hour both left the suite green while reinstating trap 35.
    They assert against `observedKeyframeWrite`, the measured 1186.5ms, and
    against the client's 5s guard instead. A deadline test that names only other
    deadlines proves nothing.

    Five mutations, all killed: floor to 250ms, cap to an hour, cap to 5s, the
    baseline multiplier to `frameDeadlineFor`'s 1/2x, and `deadlineFor` no
    longer special-casing keyframes at all. The multiplier needed its own
    assertion -- the keyframe's margin over a picture has to *widen* as the path
    slows, which a shared term leaves constant while every other check passes.

37. **Throttling the keyframe ask on a starved link delivers less video, not
    more.** The reasoning behind it is sound and the result is the opposite of
    what it predicts. Three shed reasons latch `needsKey` and each calls
    `askForKey`; keyframes are never shed (trap 36), so every ask produces an
    intra-coded frame that is written in full. On a link that cannot carry the
    stream that reads as keyframes crowding out the inter-frames they exist to
    anchor, which argues for scaling the ask interval by what the last keyframe
    cost the link instead of holding the flat 500ms floor.

    It was built, it engaged, and it made every viewer-facing number worse. Two
    runs per arm through `cmd/linkshaper` at 400 kbps, matched 60s windows taken
    20s after shaping starts, motion from one on-device loop scrolling Settings:

    ```
              out/s   sent/s  sent/out  keys/s   KB/s  awaitFrac
    base      38.7     2.55     0.066    2.20    47.6    0.889
    base      38.0     2.56     0.067    2.31    48.7    0.895
    scaled    47.1     2.04     0.043    1.35    42.5    0.939
    scaled    48.1     2.27     0.047    1.47    45.2    0.932
    ```

    The mechanism did exactly what it was built to do -- keyframes fell from
    2.25 to 1.41 per second -- and delivery fell with it, 2.55 to 2.15 frames
    per second. No metric overlaps between the arms. `keyWriteMaxMs` exceeded
    250ms in 226 of the 235 sampled seconds, so the scaling was active nearly
    throughout rather than sitting on its floor; a run where it never leaves the
    floor proves nothing and is easy to produce by accident.

    The premise is what fails. `sent` 2.55/s against `keysIn` 2.25/s means
    almost every frame reaching a starved viewer is a keyframe: with `awaitFrac`
    at 0.89 the inter-frames behind each one shed before they can be written, so
    a keyframe displaces nothing. Throttling the ask throttles the only thing
    getting through. The encoder spends what it saves on inter-frames instead,
    which is why `out` is 24% higher in the scaled arm while `sent` is lower.

    This is trap 29 with its precondition removed. There a stalled viewer turned
    the stream into keyframes on an otherwise healthy link, where they displaced
    inter-frames that would have been delivered. Neither half of that holds on a
    link this far oversubscribed.

    The code is not in the tree. What it does settle is where the remaining
    lever is not: at the 250 kbps floor the encoder is still encoding the
    device's full 1080x2424, and `AdaptBitRate` never touches `MaxSize`.
    Shrinking the picture under starvation is untried.

    Two rig faults invalidated earlier runs of this and will invalidate the next
    one. Driving motion with swipes into whatever app happens to be foregrounded
    measures an idle panel the moment that app reaches a screen with nothing
    scrollable -- 84 frames in 108s at pts p50 100.0ms, which is trap 20 and not
    a link result. And the trace rotates on every restart, so `make build`
    between two arms puts them in different files; `loupe-trace.jsonl` holds
    only the run since the last deploy.

38. **The viewer reconnects, the byte counter climbs, and the canvas stays
    black for the rest of the session.** Reading from the HUD in a browser
    against emulator-5554:

    ```
    kbps 4844   fps 0   dec 0   queue --   drop 0   shed 0
    ```

    `queue` renders `--` only when the pane's `decoder` is null, and that is the
    whole fault. `onClose` calls `teardownDecoder`, which nulls the decoder and
    **leaves `info`**. On the reconnect `handleInit` computes `changed` by
    comparing the new init against the old one, an identical encoder compares
    equal, and `configureDecoder` is skipped -- so the viewer comes back with a
    live transport, a full stream of frames, and nothing to submit them to.
    `el.canvas.width = info.codedWidth` two lines above clears the canvas, so
    what is left is black rather than the last painted frame.

    `changed` starts with `!decoder` now. The signature to recognise is
    bytes arriving with `dec` at 0: that separates it from trap 20's idle panel,
    where the last frame stays painted and `kbps` is 0 too.

    This is trap 21's family -- the reconnect fires and the page is still dead
    afterwards -- and it is what trap 21 meant by iOS reconnect being the
    fragile path. The latch is in `handleInit` in `web/pane.js` now, which
    `web/pane.test.mjs` can drive, but no test there pins it yet: none
    delivers a second identical init across a close and checks the decoder
    was configured again.

39. **A device's cutout goes missing, and the same device reported one an hour
    earlier.** `dumpsys window displays` carries one entry per display and a
    Acme One reports two:

    ```
    cutoutSpec={}
    cutoutSpec={M 0,0 L -20, 0 L -20, 26 L 20, 26 L 20, 0 Z @dp}
    ```

    Cutting at the first `cutoutSpec={` reads the blank one. The ordering is not
    stable, which is what made this look like a device-state problem: the first
    live probe found the notch and a later one on the same awake device did not,
    so the obvious theory was that a cutout is only reported while the screen is
    on. It is not -- both entries are present either way.

    `firstCutoutSpec` scans for the first non-empty spec and is split out from
    the adb call so it can be run against the real dump text.
    `TestTheFirstNonEmptyCutoutSpecWins` and `TestADumpWithNoCutoutReportsNone`
    are the two directions, and reverting to the first match fails both.

40. **The emulator's screen does not fit its bezel, and every number the
    server reported was right.** `/api/frame` gave the layout exactly --
    1198x2531 with the display at (59,55) 1080x2424 -- and `fitScreen()` put
    the canvas on that rect to the pixel. What was wrong is that both skin
    images were stretched to a box rather than placed on the grid they were
    drawn on. Measured in the browser against the running client:

    ```
    back.webp  natural 1205x2535   rendered 297x628 (the frame box)
    mask.webp  natural 1080x2424   rendered 297x628 (the frame box)
    ```

    `mask.webp` is 99.1% transparent and exactly the display's size: it is the
    screen's rounded corners and its punch hole, nothing else. Stretched over
    the whole bezel it put both on the bezel's outer edge, which is the part
    you actually see. **Skin assets are on the layout's own pixel grid.** An
    alpha scan of `back.webp` settles it -- its hole runs x 59..1138 and
    y 55..2478, which is the display rect to the pixel, in the art's own
    coordinates.

    So the art is not free to be scaled to a box either, and it is *larger
    than the layout declares*: 1205x2535 against 1198x2531, extra on the right
    and bottom, with the hole still at (59,55). Fitting it to 1198 moves that
    hole's right edge about 6px in while the canvas stays at 1080 wide, which
    is the screen overhanging the bezel. It is drawn at natural size now,
    anchored at the frame origin, and `#screenWrap.framed` clips the overhang
    the way the emulator crops it.

    Read the alpha, not the filename. `back.webp` and `mask.webp` say nothing
    about which grid they belong to, and a layer stretched to 100% looks
    deliberate in the CSS. Neither image has an opaque region anywhere near
    its own bounding box, so `naturalWidth` is the only thing that locates
    them.

    Not covered by a test: `web/app.js` touches the DOM at module scope and
    still has no harness (trap 21). Verify by measuring
    `getBoundingClientRect()` on `#frameArt`, `#frameMask` and `#screen` --
    the mask and the canvas must agree exactly.

41. **Switching targets leaves the canvas black, and one tab is two viewers.**
    Observed in a browser switching the Acme One to emulator-5554: `kbps 3256`,
    `dec 0`, `drop 157/1`, and `decoder error EncodingError` in the console.
    A reload clears it. The server's side of a switch, from a second instance
    running the unfixed client:

    ```
    15:36:52.012  client connect emulator-5554
    15:36:52.017  client overlay "Disconnected"
    15:36:52.027  ws-attempt emulator-5554
    15:36:52.505  client connect emulator-5554
    15:36:54.108  ws-attempt emulator-5554    <- viewers 2 from here on
    ```

    The switch calls `reconnect`, which sets `manualClose` and closes the old
    transport, and then `connect`, which clears `manualClose` before the old
    transport's `close` event has fired. When it does fire, 5ms later, it reads
    as an unexpected drop: it runs `teardownDecoder`, shows the overlay and
    schedules a reconnect of its own. That reconnect assigns `state.ws` over
    the connection the switch had just opened without closing it, and both
    feed `handleFrame`.

    **`framesOut` counts per viewer, so it doubles too.** Decode rate matched
    `framesOut` to within 0.5%, which reads as healthy until you remember
    that. What gives it away is parity: `decFps` was even in all 556 rows
    across two runs, because every frame arrived twice. With the fix it reads
    odd and even, and `viewers` is 1. `framesIn` on `stream-end` is the
    per-encoder count, when the session ends inside the trace.

    `decodeErrors` stayed 0 in both reproductions, so the `EncodingError`
    itself was not reproduced. The race it most plausibly comes from is the
    stale `teardownDecoder`, which clears `pendingConfig` as well as the
    decoder; it fires after the new session's init whenever the old close is
    slower than the new handshake. That path is gone with the rest.

    Each `connect()` takes a generation and every transport callback checks
    it, so a replaced transport can neither touch the decoder nor reconnect.
    `manualClose` could not say this: it is one flag shared by every
    connection. A connect that finds a live transport closes it, and one
    requested mid-handshake is queued rather than dropped, since dropping it
    meant a switch made during a connect finished on the old target.

    The connection code is in `web/pane.js` now. `web/pane.test.mjs` pins it
    in `switchTarget ignores the old connection's close` and `pointer input
    after switchTarget goes to the new transport`; `rotator promote swaps the
    transport without a reconnect` covers the same generation check on a
    rotation. Extend those before changing it. Verify from `viewers` in the
    trace.

42. **The device is rotated, the video turns sideways, and every touch after
    that reaches nothing at all.** The obvious reading is that the client keeps
    the old canvas and so computes coordinates in the wrong space, which would
    put taps in the wrong place. It does keep the old canvas, and the taps do
    not land in the wrong place -- they are discarded. scrcpy-server 3.3.4's
    `PositionMapper.map` returns null unless the event's size matches the
    encoder's current picture, and `injectTouch` then logs at VERBOSE and
    returns, so the event never reaches Android:

    ```
    VERBOSE: Ignore positional event generated for size 480x1080 (current size is 1080x480)
    ```

    Read from the dex rather than inferred, then reproduced: every touch sent
    with the header's size after a rotation produced that line, down and up
    alike, and none reached the device.

    **What the stream does on rotation, measured on emulator-5554 rather than
    assumed.** The session survives -- same socket, no `stream-end`, the framer
    never desyncs. The codec meta is read once at startup and is *not* resent.
    What does arrive, about 800ms after the rotation, is a fresh config packet
    whose SPS carries the new dimensions:

    ```
    portrait   6742c0298d681e0227e59080808083c2211a80   480x1080
    landscape  6742c0298d680440f7979080808083c2211a80   1080x480
    ```

    So the parameter set is the only notice of a rotation there is, and
    `h264.SPSDimensions` is what reads it. That the derivation is exact is not
    taken on trust: scrcpy's own rejection message names the size it expects,
    and `current size is 1080x480` is the parser's output to the pixel. In
    portrait it agrees with the codec meta the server sent at startup, which is
    an independent second check.

    Three things follow, and the middle one is the part that is easy to miss.
    `adbStream` re-reads the size off every config frame so `EncodeTouch` and
    `EncodeScroll` carry the live one. The session republishes **init** rather
    than inventing a resize message, because the client already treats a change
    of `codedWidth`/`codedHeight` as grounds to resize the canvas and
    reconfigure the decoder -- the client side of this needed no change at all,
    which is why reading it before writing anything was worth the time. And the
    stream's meta is updated on the target's own pump, before the session sees
    the frame, so the ordering is guaranteed rather than racy.

    **A skin bezel turns with the stream; a derived bezel is hidden.** The
    skin's turn is computed by `/api/frame` from the display's rotation (see
    Device frames and trap 44). A derived cutout is anchored along the top by
    a `dx` that means nothing once the axes swap, so `frameSuitsStream` in
    `web/orientation.js` compares the *display rect's* orientation against the
    stream's and hides a frame that does not fit; it returns by itself on the
    way back. It is a module with a node test because `app.js` still has no
    harness (trap 21).

    **A rotation is invisible in the trace unless it is recorded.** The frame
    rate dips while the encoder restarts its GOP and the canvas changes shape,
    which from the trace alone reads as trap 20's idle panel. `kind:"resize"`
    carries both sizes:

    ```
    {"kind":"resize","target":"emulator-5554","from":"480x1080","to":"1080x480"}
    {"kind":"resize","target":"emulator-5554","from":"1080x480","to":"480x1080"}
    ```

    Verified end to end in a browser: the canvas follows both ways, the skin
    drops and returns, and a tap aimed at `/api/ui`'s reported centre for
    "Network & internet" (device px [529,614] at rotation 1) opened that
    screen. `drop` and `shed` stayed 0 across both rotations.

    Two rig faults to avoid repeating. Reading the HUD without driving motion
    reports `kbps 0` and `dec 0` on a perfectly healthy stream, because an idle
    panel emits nothing (trap 20) -- sample *during* an on-device motion loop,
    not after one. And `dumpsys window displays` reports `cur=1080x480` from
    its first entry while `/api/ui` reports 2424x1080; that is trap 39's shape
    again, a first entry that is not the display in question, so it is not an
    instrument worth using here.

    `TestSPSDimensionsReadsTheRotationPair` pins the parser against both
    captured parameter sets, and the orientation module's tests pin the bezel
    decision from both directions.

43. **A new client module loads in the page and 404s in the binary.** Adding
    `web/orientation.js` and importing it from `app.js` is not enough: three
    separate lists had to agree, and two of them fail only at runtime. The
    `//go:embed` list decides what the binary holds, and `stampIndex` keeps its
    own list twice over -- once for the content hash and once for the import
    rewrite. A module missing from the embed list is stamped correctly into a
    URL that answers 404, and because it is an ES module import, `app.js` stops
    part way through evaluation. The page loads, the target list never appears,
    and nothing in the Go tests notices.

    The embed patterns are globs now (`*.html *.css *.js`, which leaves the
    `.test.mjs` harnesses out of the binary), the two stamp lists are one
    `clientModules`, and `TestEveryClientModuleIsStamped` reads `app.js`'s own
    relative imports so a module that is imported but unlisted fails the build
    rather than the browser. `TestEveryClientModuleIsEmbedded` is the other
    half, since stamping a module and shipping it are different things.

    Found by loading the page, not by reading the code: every Go test passed
    with the module unreachable.

44. **The bezel will not turn, and `dumpsys window displays` reports the
    rotation of a display nobody is watching.** `/api/frame` answered `turn 0`
    at every rotation while the emulator was plainly in landscape. The dump
    says why, in the order it prints:

    ```
    Display: mDisplayId=1094
      init=480x1080 1dpi ... cur=480x1080     winConfig={ ... mDisplayRotation=ROTATION_0 }
    Display: mDisplayId=0 (organized)
      init=1080x2424 420dpi ... cur=1080x2424 winConfig={ ... mDisplayRotation=ROTATION_90 }
    ```

    The 1dpi display comes **first**, and it never rotates. Taking the first
    reading therefore gets a display nobody is watching, which is trap 39's
    shape a second time and in the same dump. It also settles trap 42's aside:
    the `cur=1080x480` that disagreed with `/api/ui` there was this display,
    not a stale or wrong reading of the real one.

    **Its id is not stable and is not the thing to key on** -- 1084 in one run
    and 1094 in the next. Keyed on `mDisplayId=0`, which is the display scrcpy
    streams by default.

    **The ordering is not the only decoy.** The dump carries the rotation
    *constants* alongside the state:

    ```
    mLandscapeRotation=ROTATION_90  mSeascapeRotation=ROTATION_270
    mPortraitRotation=ROTATION_0    mUpsideDownRotation=ROTATION_180
    ```

    A search for `Rotation=ROTATION_` matches all four, so a parser can report
    a constant as the current rotation and be right by luck for as long as the
    real entry happens to print first. `mDisplayRotation` is the only key no
    constant shares. `TestTheDisplayRotationIsReadOffDisplayZero` feeds it a
    dump holding nothing but those four lines and requires no answer; the
    looser key returns 1 from it.

    The turn direction was checked against the device rather than derived.
    Android reports the cutout's inset edge, and the art's punch hole has to
    land on it:

    ```
    ROTATION_90   insets=Rect(136, 0 - 0, 0)   left    hole x 2.1-5.1%
    ROTATION_270  insets=Rect(0, 0 - 136, 0)   right   hole x 94.8-97.8%
    ```

    Both measured by scanning the rendered mask's alpha in the browser. Scan a
    central band, not the whole image: the mask carries the screen's rounded
    corners (trap 40), whose arcs reach about 3.6% in and swamp a bounding-box
    read, which is what first reported the hole at dead centre.

45. **Modules are leaves, except one that is not.** Only `app.js` should
    import a module, because `stampIndex` rewrites import specifiers in
    `app.js` alone; a sibling import is fetched at its bare, unversioned URL,
    which is trap 7's stale-client shape. Anything two modules share goes in
    through `mount`'s `deps`. This is trap 43's companion: that one is about a
    module being listed, this one about where it is imported from.

    `toast.js` breaks the rule. `appcard.js`, `clipboard.js`, `corners.js`,
    `devicesettings.js`, `drawer.js`, `fold.js`, `immersive.js`, `inspect.js`,
    `flag.js`, `keyboard.js`, `newdevice.js`, `record.js`, `streamconfig.js`
    and `drop.js` each import it directly, and `app.js` does not import it at all. It works only because `toast.js` is
    stateless. If `app.js` ever imports it too, its specifier gets stamped and
    the other fourteen do not, and the page holds two instances of the module
    under two URLs. `TestEveryClientModuleIsStamped` walks imports
    transitively, so it checks that `toast.js` is *listed*, not that every file
    naming it is rewritten. The full account is the comment above
    `clientModules` in `internal/httpapi/server.go`.

46. **`/api/ui` fails with `uiautomator dump: exit status 137:` and nothing
    after the colon.** Another UiAutomation client already holds the device,
    and Android allows one at a time. Seen on emulator-5554 with a Maestro
    driver (`dev.mobile.maestro`) running under a host `maestro mcp`. The dump
    throws inside `UiAutomation.connect`:

    ```
    java.lang.IllegalStateException: UiAutomationService
      android.accessibilityservice.IAccessibilityServiceClient$Stub$Proxy@4264778already registered!
    ```

    That goes to logcat and nowhere else. Stdout and stderr were both captured
    separately and both were empty, so the exit status is all adb has to
    report. 137 is SIGKILL: the runtime kills the process after the uncaught
    exception. `adb exec-out` is no better -- it printed `Killed` and exited 0.

    `Inspect` now runs the dump as `echo loupe-pid=$$; exec uiautomator dump`.
    `exec` keeps the shell's pid, so the echoed pid is uiautomator's own, and
    on a non-zero exit `logcat -d -b crash --pid=<pid>` reads back only that
    process's crash. If it is this exception, the error is
    `ErrUIAutomationBusy` and `/api/ui` answers 409 rather than 502. The pid
    filter matters: without it a crash from any earlier dump, or from another
    tool entirely, would be read as the cause of this one.

    `TestDumpCrashIsUIAutomationConflict` feeds the crash buffer captured from
    emulator-5554 (`internal/target/testdata/`). Ending the other client is
    the fix; nothing on the loupe side can share the registration.

47. **The phone says "Disconnected" and retries forever, and the server is
    healthy.** The phone's QUIC handshake completed and `/wt` reached the
    handler; every attempt then failed in `acquire`:

    ```
    push server jar: exit status 1 (adb: error: failed to get feature set: device 'ACME00000000001' not found)
    ```

    The Acme One had dropped off adb overnight. The target list was read once
    at startup, so it stayed selectable, and a link with no `?target=` picked
    it because it was the first `device`-kind target. Nothing on the phone
    said so: a WebSocket refused before it upgrades reports only that it
    closed, and WebTransport reports a bare `WebTransportError` (trap 11). The
    reason existed only in the server log, which is why this read as a
    network fault.

    `adb track-devices -l` now drives `Manager.SyncDevices` over one
    long-lived connection. Polling `adb devices` would open a loopback
    connection every few seconds, which is trap 33's habit. A device adb
    stops listing is kept and marked `missing` rather than removed, a state
    change rebuilds the target, and `Acquire` refuses an unavailable one
    before touching adb. `/api/targets` carries `available`, `state`,
    `reason` and `lastError`. After a failed connect the client reads them
    and replaces "Disconnected" with the reason and a button to watch a
    target that can open. The retry keeps running underneath, so a device
    that comes back is picked up without a tap. The drawer lists an
    unavailable target in its own disabled Unavailable group, with its state
    in place of the version line.

    The client polls the list every 5s while the page is visible, and an open
    drawer is repainted only when a field it renders changed (`sameShown` in
    `web/targets.js`), since a repaint under a finger moves the row being
    reached for. `Targets()` sorts devices, then emulators, then the pattern,
    each by ID; `TestTheTargetListHasAFixedOrder` pins it. An unavailable
    target is not described, because `getprop` on a device adb has lost can
    only spend `describeTimeout` failing.

    The server half was verified against a test instance whose `-adb` was a
    wrapper listing a fake `FAKE0001` and dropping it from `track-devices`
    after 25s, with the pre-redesign client. The drawer half has unit tests
    only. Neither has been run on the phone, or against a real device
    unplugged mid-stream.

48. **A simulator's dot goes red and stays red while its bytes keep
    arriving.** The trace shows `decFps` 0 and `healthReason` "delivered 0 of
    N frames" at 1-2 Mbps. On Safari, `decodeErrors` also climbs about once a
    second, one per keyframe. Only a reconnect clears it: a suspend and resume,
    or a reload.

    `configureDecoder` cleared `pendingConfig`, the SPS/PPS it prepends to
    each keyframe. Every rebuild after a decoder error therefore started
    with no parameter sets. An encoder that sends them only once never
    replaced them, and simcap sends them only when they change. After one bad
    frame, every later keyframe reached a decoder that could not read it. A
    reconnect recovered because the server replays `lastCfg` to a joining
    viewer.

    WebTransport has a second route to the same state. The init message
    travels on the control stream and the config frame on its own uni stream,
    so they can arrive in either order. An init that lands second configured
    the decoder and dropped a config that had already arrived. That ordering
    is the mechanism, not something a trace has shown.

    The parameter sets now belong to the connection. `closeDecoder` keeps
    them for a rebuild; `teardownDecoder`, run only when the connection ends,
    drops them. Reproduced in the browser pane against a spare server on
    `loupe-probe` by corrupting one delta frame through a patched
    `VideoDecoder.prototype.decode`. On main the page read `decFps` 0 and
    "delivered 0 of 10 frames" for as long as it was watched. With the fix it
    logged one error and kept decoding, and health stayed `good`.

    The client now reports `decode-error` at most once a second, with the
    decoder's message and whether it held a config at the time. In the field
    the first error is the page going away. Every `Decoder failure` with a
    config held in the traces of 26 and 27 September, 8 across a simulator,
    an Acme One and Pixel_10, came 3-429ms by the client's clock before a
    `suspend`, on a stream that had been decoding until then, and none came
    mid-session. The error precedes `visibilitychange`, so at the error the
    page still reads visible and there is no earlier signal to close the
    decoder on. The report is therefore held for up to a second: a suspend
    inside that second sends it first with `beforeSuspendMs`, and otherwise
    it goes without, which is a real fault. The 43 others on 26 September,
    once a second with `haveConfig` false on fresh connections, were the
    joiner replay expiring before it was sent, fixed in #31 five minutes
    later on a server started before it.

    The decoder was the one thing the dot could not see. An idle simulator
    produces about two frames a second, under `MinProducedForRatio`, so the
    delivery ratio was never judged, and a phone rejecting every frame read
    `good` on delivery, shed and rtt alike. Health now takes the report's
    `decodeErrors` as a per-second count: any error reads `bad` for that
    second, and the next clean report reads `good`. The count is per page
    load, so a viewer's first report is only a baseline and a lower count is
    a reload starting over (`TestDecodeErrorsReadBadOnlyWhileTheyHappen`).

    The dot also lagged the stream by about four seconds each way, from two
    averages. Delivery was always judged over five reports, so a lost second
    read good until it outweighed the healthy ones before it, and a recovery
    read bad until the lost second aged out. The window now reaches back only
    until it holds `MinProducedForRatio` frames, which at a full frame rate is
    the newest report alone; a slow stream still widens it
    (`TestAFullRateStreamRecoversOnTheFirstHealthyReport`). And rtt was the
    display's running average, a 1/8 step per 500ms ping, so a burst held
    `degraded` for about four seconds after the link recovered. The client
    now sends `rttNowMs`, the median of the last second's pings, and health
    reads that when it is there.

    What remains is the report cadence: the dot is at most one stats report,
    about a second, behind the stream. Replaying the per-second records of 12
    traces from this machine through both versions, the new one recovered
    earlier 46 times and later 17. The later ones inspected were the old
    window reading `good` through seconds the phone decoded nothing
    (16:59:24-28 and 17:13:37-40 on 25 September).

49. **The video freezes and the dot goes grey, while every swipe still
    reaches the device.**

    The viewer's send loop and its read loop are two goroutines, and only
    the read loop ended the viewer. A send error returned from the writer
    and left the connection open, so `Recv` kept handing the phone's
    touches to the device while nothing went back: no frames, no stats, and
    the viewer's queue full, which dropped its events. Nothing was logged.
    On 26 September at 12:56:03 an iPhone 18 Pro simulator's session stopped
    sending (the `bitrate` record's `bytes` froze at 4049379 while `frames`
    climbed to 1085), the per-second stats records stopped, and 13 swipes
    arrived over the next 18s. It ended only when the page was backgrounded
    at 12:56:24.

    The writer now closes the connection when it stops, and logs and traces
    the reason as `kind:"viewer-stop"`
    (`TestServeViewerEndsWhenSendingStops`). Which send failed that day is
    not known, since the error was discarded. Two WebTransport errors that
    were fatal are now an abandoned frame instead. `OpenUniStream` refuses
    once the phone's stream credit runs out, which means the phone is
    behind, so `SendFrame` waits for credit until the frame's deadline.
    And `Close` fails on a stream the phone stopped with `STOP_SENDING`,
    which is one frame lost.

    The dot read grey because a report older than 2.5s reads `stale`, which
    was painted muted, the same as no session. Reports stopping mid-session
    is a stream that has stopped, so `stale` is now red.

    The first run with the writer closing the connection showed a stream
    limit still ending sessions: `viewer-stop` with `open frame stream: too
    many open streams` on a Acmechat simulator at 22:27:49 the same day, as
    the phone's rtt climbed to 476ms. That was quic-go's per-connection
    limit, which webtransport-go passes through as its own
    `StreamLimitReachedError` type, and `isStreamLimit` had matched only
    webtransport-go's (`TestIsStreamLimitMatchesBothLimits`). `viewer-stop`
    now carries `errType`.

    That was not enough. On 28 September at 07:05, ten `viewer-stop` records
    read `open frame stream: ` and nothing else. webtransport-go's
    `SessionError.Error()` is its message alone, and `wtConn.Close` closes
    with an empty one. `loupe-run.log` put `read: stream canceled with error
    code 0` before every one: the phone reset the control stream as the page
    was backgrounded, the read loop closed the session, and the writer's
    next open got that close back. `wtErr` now says which side closed a
    session and with what code, and `viewer-stop` is written once per viewer
    for the side that stopped first, as `side`, with the other side's error
    in `readErr` or `writeErr` and the bottom of a wrapped chain in
    `innerType` (`TestViewerStopNamesTheSideThatStoppedFirst`).

50. **`/api/input` answers `{"ok":true}` and nothing on the device moves,
    and neither does anything the viewer sends.**

    Android was holding two pointers down that nothing ever lifted, and
    rejects every injected DOWN after that. On 30 September the Acme One
    (Android 16, API 36) dropped `/api/input` taps and swipes, plain `adb
    shell input tap`, and a tap sent over a live scrcpy session with
    `wtprobe -tap`, each logged by `InputDispatcher` as `Inconsistent
    event ... reason: Injection on 0: Invalid DOWN event - pointers already
    down`, naming pointers 0 and 1. `input` exits 0 regardless, so the
    server answered 200. Screenshots and `/api/ui` kept working, which made
    it look like a setting that blocks shell input. It is not one: a single
    two-finger touch over scrcpy, whose downs were rejected and whose two
    ups matched the stuck pair, cleared it, and the next `input tap` went
    through.

    What left them down was not caught. A viewer that goes away
    mid-gesture never sends the up, and that is the one way this server
    can cause it, so a session now remembers each viewer's pointers that
    are down and lifts them, at their last position, when the viewer
    leaves or the session closes (`internal/session/pointers.go`;
    `TestAViewerLeavingMidTouchLiftsItsFingers`,
    `TestClosingTheSessionLiftsFingersStillDown`).

    And a rejection is no longer a success. A tap or swipe through `adb
    shell input` reads `InputDispatcher`'s errors logged since just before
    it, in the same shell, and a rejection answers 409 with code
    `touchRejected` and the dispatcher's reason
    (`TestTapReportsATouchTheDeviceRejected`,
    `TestAgentRejectedTouchIsAConflictWithACode`). On emulator-5554 the read
    straight after `input` found the rejection every time. Touches from a
    viewer go over the scrcpy socket, which says nothing about delivery, so
    a stuck pointer still shows there only as a touch that does nothing.

51. **The dot swings between green, yellow and red on cellular, the video
    sits under 10fps, and the bitrate controller never acts.**

    The link carried a fraction of the bitrate, and the window fell between
    the two gates that step down. On 20 October avd:Pixel_10 over DERP
    (rtt 190-530ms), 13:54:53 to 13:55:12:

    ```
    produced       6 -> 265    about 14 a second
    sent          64 -> 139    about 5 a second
    shedAwaitKey  23 -> 106
    goodput       340-667 kbps against 4000 kbps
    shedFrac      0.008-0.079  (down gate: 0.20)
    awaitFrac     0.28-0.44    (starved gate: 0.50)
    decFps        0-9
    ```

    Every window read `upWindow`, then reset as `inconclusive`, so the
    encoder stayed at 4 Mbps. The person reloaded the page and the new
    session starved the same way: decFps 5-6, then 0, still at 4 Mbps. A
    simulator on cellular on 21 October did the same at 0.8-1.1 Mbps of 4
    Mbps, awaitFrac 0.35-0.49, with no change all session.

    Goodput was the target of both step-down gates but never a trigger, and
    the shed gate does not count keyframe waits (trap 27). So a link
    carrying an eighth of the bitrate read as healthy as long as the waits
    stayed under half the window.

    `GateUndercarried` fires when goodput is under half the current rate
    and awaitFrac is at least 0.20, and aims at goodput, floored at 250
    kbps. A static screen also reads goodput far under the bitrate, but
    with awaitFrac 0, so `TestAdapterIgnoresAnIdleScreen` still holds.
    Stepping up now also needs awaitFrac at most 0.05, because after a step
    down the pacer can go clean while the viewer still waits on keyframes.
    `TestALinkCarryingAFractionOfTheBitrateStepsDown` feeds both sessions'
    numbers to a relaunch and a live adapter, and
    `TestNoStepUpWhileTheViewerWaitsOnKeyframes` pins the probe.

    That covered a static screen with no waits, not a near-idle one with a
    few. On 26 October a simulator on a direct phone link sent 26 frames in
    12s, 7 of them while a newly attached viewer awaited its keyframe:
    awaitFrac 0.27, goodput 66 kbps of 4 Mbps, cut to the floor as
    undercarried, and the size ladder took the picture to 168×360. Another
    window, 22 frames in 10s, was cut as starved. Goodput over a handful of
    small frames measures the frames, not the link. Every gate now needs the
    window to average 6 frames a second first (`GateSparse` otherwise);
    `TestAdapterIgnoresASparseWindowWithKeyframeWaits` feeds both shapes.

52. **Safari's video stops about forty seconds into a WebTransport session,
    and the page keeps reporting as if nothing were wrong.**

    Safari never raises the QUIC connection's flow-control limit. It
    advertises `initial_max_data` 16777216 and sends no `MAX_DATA` frame
    after that, so once 16 MiB of stream data has gone out on the
    connection the server can send nothing more. That is WebKit bug
    319818, "Flow control never refills". The WebTransport session limit
    is not what runs out: Safari advertises 8 MiB for it (setting
    `0x2b61`) and raises it with `WT_MAX_DATA` every 4 MiB, to 20.97MB
    by the time the connection stalls.

    On 11 October, iPhone Safari on iOS 27.2 against a synthetic spare
    server on the LAN, `send.bytes` froze at 16.76MB, 16.70MB and 16.58MB
    in three sessions, each 40-43s in, with `lastErr` `open: context
    deadline exceeded` and `awaitKey` climbing. A fourth froze at 14.27MB
    after five frames were reset mid-flight; a reset frame's bytes count
    against the connection limit and not towards `send.bytes`. The
    client kept sending stats with `decFps` 0 and no decode error. With
    quic-go's frame logging added to a debug build, the run that froze
    at 16.58MB logged Safari's transport parameters with
    `initial_max_data=16777216`, no `MAX_DATA` received for the whole
    connection, and our `DATA_BLOCKED 16777216` in the same second
    `send.bytes` stopped. Every `WT_MAX_DATA` and `WT_MAX_STREAMS`
    capsule stopped at that moment too, since Safari had nothing new to
    acknowledge.

    `wtprobe` does not show it: quic-go's client raises the window, and
    a 75s run delivered 30.6MB without a stall. WebSocket does not show
    it either, which made Picture in Picture look like the cause, since
    the PiP run that survived happened to be on WebSocket. Each new
    connection starts with a fresh 16 MiB.

    At synthetic's 3.3 Mbps that is about 40s. A device screen sends far
    less while it is still: in the production traces, 49 WebTransport
    plateaus of 5s or more with one viewer, none of them in the
    16.3-16.8MB band. So this is not yet shown to be behind trap 49.

    The server cannot raise a window the peer owns, so it moves the
    session to a fresh connection before the old one runs out. `wtConn`
    counts what each connection has spent of Safari's window, a reset
    frame's bytes included, and at 12 MiB or 6000 streams the server sends
    `MsgRotate` (0x09, `{bytes, streams, reason}`). `web/rotate.js` opens
    a second `/wt` without falling back to WebSocket, holds its config,
    and promotes it on its first keyframe once no finger is down,
    skipping any frame whose PTS the old connection already delivered.
    Promoting on the config cost about a second of video per swap,
    because the keyframe comes about 0.8s after it.

    A connection past 15.5 MiB or 7400 streams that has delivered nothing
    for 2s is closed with code 2, on a ticker of its own: the control
    stream's write blocks without a deadline once the window is spent, so
    a check inside the writer loop never ran. The close capsule goes out
    in the same spent window and is cancelled unsent, so the page reads a
    close with no code, or code 0, after 12 MiB of frames as the same
    thing, and reconnects at once rather than backing off.

    Safari also stops granting stream credit well short of either limit,
    with its control stream still answering. A connection refused a new
    stream for 1s while the client sent within the last 2.5s is asked to
    rotate with reason `credit`, and closed at 6s.

    On the phone, on the LAN: on 11 October five rotations at 12 MiB each
    kept `decFps` at 55-63. On 12 October the backstop closed two frozen
    connections 2.1-2.3s after they froze, at 16.76MB, and once the page
    read the codeless close the reconnect began 4-8ms after it. On an
    internet path at 500 kbps that afternoon the credit stall came every
    12-24s, at 329-910 streams, and each of 15 credit rotations worked
    with a 3.1-3.9s gap in the video; the clock then started at the
    second refusal and waited 2s. Starting it at the first and moving
    after 1s has not been run on the phone. Why Safari stops granting
    credit is not known.

    `TestDecidePeerWindow`, `TestViewerAsksForRotationOnceNearTheLimit`,
    `TestViewerClosesAnExhaustedConnectionWhoseWriterIsStuck` and
    `TestViewerRotatesThenClosesACreditStarvedConnection` pin the server;
    `web/rotate.test.mjs` the promotion and `closedExhausted`.

53. **`/api/ui?compose=1` names only a handful of nodes on the first call
    after the agent attaches, and the next call names them all.**

    The inspector fills Compose's slot tables for a screen that is already
    open by forcing a recomposition, so a reply that comes back before that
    finishes holds part of the tree. In the spike on emulator-5554 with
    acmechat the first `GetComposables` reply had 95 nodes and the next
    489. `composetree.Stable` re-asks every 150ms until two replies agree
    on the node count; don't drop the loop to save latency.
    `TestStable*` in `internal/composetree` pins it.

54. **`loupe mcp`'s channel messages never reach the session, with no
    error anywhere.** Claude Code registers no channel for a stdio server that
    negotiated MCP protocol 2026-07-28, and drops events for an unregistered
    channel in silence (channels-reference). The go-sdk negotiates the newest
    version both sides know, so `NewServer` sets `SupportedProtocolVersions`
    to stop at 2025-11-25; the documented workaround,
    `MCP_PROTOCOL_NEGOTIATION=legacy`, would have to be set in every user's
    environment. Don't add newer versions to that list until channels
    register on them. `TestChannelCapabilityIsDeclared` checks the
    negotiated version against a go-sdk client. Not yet confirmed against
    Claude Code itself: if its tools vanish from `/mcp`, the cap broke
    negotiation, so revert it and document the env var instead.

## Open observation: a 4.87s held sample in a Pixel_10 recording

Not a numbered trap: the cause is not known. A recording of Pixel_10 from
the Home Screen (97 frames in 9.48s, through a spare server on 15 October)
showed the encoder's first second as ten 100ms samples, which matches the
`repeat-previous-frame-after` the pinned jar sets, then one sample of 4.87s
while the Home Screen sat at rest, then 16-40ms samples from the launch's
transition. Why the repeat stopped for those 4.87s is not explained. The
facts are in CLAUDE.md's Recording section. If a recording of a still screen
shows a long hold, read its `ffprobe -show_packets` before assuming the
recorder dropped frames: the samples are the encoder's own.
