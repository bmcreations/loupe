// One target's stream: its connection, decoder and render loop, the canvas's
// pointer input, and the modules that draw over it or hold its session.
//
// This is what app.js held in its one global `state` for the device on
// screen, moved here so a page can hold more than one. It imports nothing
// from web/ (trap 45): the transport, the jitter buffers, the rotator and the
// overlay come in through deps.lib, the per-pane modules through deps.modules,
// and every page reaction leaves through an on* callback.
//
// deps:
//   id, targetId
//   els: { root, canvas, overlay, touchLayer, cursorLayer, extLayer, inspectLayer, flagLayer,
//          logList, logBadge, m: {fps, dec, rtt, jit, q, drop, buf, trans, shed, rate} }
//   lib: { connectTransport, ArrivalJitter, JitterBuffer, createRotator, closedExhausted, MSG_ROTATE,
//          secondRtt, mountOverlay, reconnectingState, shutDownState, connectingState,
//          afterClose, afterResume, endedFromClose }
//   modules: { touches, cursor, overlays, control, inspect, eventlog, record, flag }, each that module's mount.
//     The pane mounts each once, with getters bound to itself; pane.mods holds what they return.
//   moduleDeps: the page things those mounts need:
//     doc, fetch, toast, inspectSheet, pillRec, storage
//     isFront(pane)                   whether this pane is the focused one (record's pill)
//     postInput(pane, body)           one /api/input as this pane's viewer (inspect)
//     takeOver(pane)                  sends MsgTakeOver on the pane (control); throws when it cannot
//     onHeldByOther(pane, on)         optional; another viewer started or stopped holding the pane's device
//     onInspectToggle(pane, on)       inspect turned on or off
//     beforeFlag(pane), focusFlagNote()  flag mode: leave zoom first; whether to focus the note
//     logIsOpen()                     whether the event log's page is the one shown
//   createDecoder(init) -> { configure, decode(chunkInit), close, state, decodeQueueSize }
//   raf(fn), cancelRaf(handle), now(), setTimeout, clearTimeout
//   hidden() -> bool, visibility() -> string, pipActive() -> bool
//   preferWebTransport, build
//   report(rec)                       the page's report; pane.report wraps it
//   label(targetId) -> string         the name the overlay shows for a target
//   openPicker()                      the ended screen's Pick another device
//   onInit(pane, info)                a session's init, for the modules that read it
//   onShape(pane, {width, height})    after every init; app.js refits the screen
//   onStatus(pane, status)            null | 'live' | 'error', as setStatus had them
//   onStats(pane)                     a fresh MsgStats verdict arrived
//   onClosed(pane, info)              the current connection closed
//   onConnectFailed(pane, targetId)   a failed connect or close; app.js asks why
//   onControlMessage(pane, type, body) every control message the pane does not handle
//   onTouchEnd(pane)                  a press on the canvas was released

// Protocol. Must stay in lockstep with internal/session/protocol.go.
const MSG_INIT = 0x01;
const MSG_STATS = 0x03;
const MSG_PONG = 0x04;
const MSG_ENDED = 0x0A;
const MSG_PING = 0x13;
const MSG_CLIENT_STATS = 0x14;
const MSG_TOUCH = 0x10;
const MSG_SCROLL = 0x12;
const ACTION_DOWN = 0, ACTION_UP = 1, ACTION_MOVE = 2, ACTION_CANCEL = 3;

// ROTATE_GRACE_MS is how long a standby has to promote after the connection
// it was replacing is closed under it, before a plain reconnect.
const ROTATE_GRACE_MS = 3000;

const noop = () => {};

export function createPane(deps) {
  const { id, els, lib } = deps;
  const el = els;
  const m = els.m || {};
  const now = deps.now || (() => performance.now());
  const setTimeout = deps.setTimeout || globalThis.setTimeout;
  const clearTimeout = deps.clearTimeout || globalThis.clearTimeout;
  const raf = deps.raf || ((fn) => requestAnimationFrame(fn));
  const cancelRaf = deps.cancelRaf || ((h) => cancelAnimationFrame(h));
  const hidden = deps.hidden || (() => false);
  const visibility = deps.visibility || (() => (hidden() ? 'hidden' : 'visible'));
  const pipActive = deps.pipActive || (() => false);
  const label = deps.label || ((t) => t);
  const onInit = deps.onInit || noop;
  const onShape = deps.onShape || noop;
  const onStatus = deps.onStatus || noop;
  const onStats = deps.onStats || noop;
  const onClosed = deps.onClosed || noop;
  const onConnectFailed = deps.onConnectFailed || noop;
  const onControlMessage = deps.onControlMessage || noop;
  const onTouchEnd = deps.onTouchEnd || noop;
  const { connectTransport, closedExhausted, MSG_ROTATE, secondRtt, reconnectingState, shutDownState,
    connectingState, afterClose, afterResume, endedFromClose } = lib;

  // Set by close(). A closed pane never connects again.
  let closed = false;
  // The per-pane modules' handles, mounted once the pane object exists.
  let mods = null;

  // -------------------------------------------------------------------------
  // State
  // -------------------------------------------------------------------------
  let ws = null;
  let decoder = null;
  let info = null;
  let targetId = deps.targetId || null;
  // ?transport=ws forces the WebSocket path, for measuring the two against
  // each other on the same link.
  const preferWT = !!deps.preferWebTransport;
  let transportName = '';

  // Parameter sets arrive as their own message. In Annex B mode the decoder
  // wants them in-band ahead of the picture they describe, so hold them and
  // prepend to every keyframe rather than submitting a picture-less chunk.
  //
  // They belong to the connection, not to the decoder: a decoder rebuilt
  // after an error, or configured by an init that arrived after them, still
  // needs them, and an encoder that sends them once -- simcap, and scrcpy
  // until its next relaunch -- will not send them again. Only a connection
  // teardown clears them; the next connection's joiner replay brings them back.
  let pendingConfig = null;
  // Rate limit for the decode-error report: one per second is enough to read
  // the cause, and a stream failing every keyframe would otherwise log 60/s.
  let lastDecodeErrorReport = 0;
  // A decode-error waiting to see whether the page hides. iOS fails the
  // decoder just before the page goes hidden, and before visibilitychange
  // fires, so at the error the page still reads visible. suspendStream sends
  // it with the gap to the suspend; otherwise the timer sends it untagged.
  let pendingDecodeError = null;
  let decodeErrorTimer = 0;
  let sawKeyframe = false;

  let rafHandle = 0;
  // A hidden page in PiP presents from a timer instead of rAF; this says
  // which one rafHandle is.
  let rafIsTimer = false;
  // The page hid while PiP was showing the stream, so the stream was kept.
  let pipCarried = false;

  let reconnectDelay = 500;
  let reconnectAttempt = 0;
  let reconnectTimer = 0;
  let manualClose = false;
  // Guards connect() against the two resume signals firing together. Both are
  // needed (see the visibility handlers) and either one alone can arrive first.
  let connecting = false;
  // A connect() that arrived while another was in flight. Dropping it outright
  // meant a target switch made mid-connect finished on the old target.
  let connectPending = false;
  // Which transport is current. Every callback a transport makes checks it, so
  // one that has been replaced can neither tear down its successor's decoder
  // nor schedule a reconnect of its own. manualClose alone could not say this:
  // it is one flag shared by every connection, and connect() clears it before
  // the old transport's close event has fired.
  let connGen = 0;

  // Metrics
  let renderedThisSecond = 0;
  let fps = 0;
  // decodedThisSecond counts frames the decoder emitted, before the
  // presentation clock skips any. The gap between this and rendered fps is how
  // much of the stream arrived too clumped to be shown as distinct frames.
  let decodedThisSecond = 0;
  let decFps = 0;
  let bytesThisSecond = 0;
  let kbps = 0;
  let rtt = 0;
  let dropped = 0;
  let decodeErrors = 0;
  let shedRate = 0;

  // status is what the dot is showing. It was only ever a class on an element
  // before; the connection details need to read it back, and deriving it from
  // the class name would make the dot the source of truth for the state rather
  // than a view of it.
  let status = null;

  // The raw material for health.js's dotFor: the server's per-second verdict
  // plus when it arrived. receivedAt is performance.now(), never Date.now()
  // (health.js's own doc comment says why): it is what dotFor ages against to
  // read 'stale' after 2.5s with no fresh MsgStats.
  let health = null;
  let healthReason = '';
  let statsReceivedAt = NaN;
  // Whether the target runs with no window (a Peek, or a headless AVD).
  // Written by the page from the targets list; nothing here reads it.
  let targetHeadless = false;

  // frame is the device bezel, or null when it is off or unavailable.
  // frameFor is the target the cached frame describes, so switching devices
  // does not draw a Pixel's bezel around an Acme One. app.js's device-frame code
  // fetches and lays them out; the pane only holds them for its target.
  let frame = null;
  let frameFor = null;
  // Why the server ended this target's session ('shutDown' or 'deleted'),
  // or null. Set by MsgEnded or a WebTransport close with code 3, read by
  // onClose, and cleared by every connect and target switch.
  let ended = null;
  let endedVia = '';
  // The next connect is an explicit Start, and sends start=1.
  let startNext = false;

  // Round trips since the last stats report, for rttNowMs.
  const rttThisSecond = [];

  const ctx = el.canvas && el.canvas.getContext
    ? el.canvas.getContext('2d', { alpha: false, desynchronized: true })
    : null;

  // report is the page's report with this pane and its target filled in, so
  // every record in the trace says which stream it came from.
  function report(rec) {
    const out = { ...rec };
    if (out.pane === undefined) out.pane = id;
    if (out.target === undefined) out.target = targetId;
    try { deps.report(out); } catch (_) {}
  }

  // states.js builds each state and paints it; this is the pane's instance.
  const overlay = lib.mountOverlay(el.overlay, { report });

  function hideOverlay() { overlay.hide(); }

  function setStatus(kind) {
    status = kind;
    onStatus(pane, kind);
  }

  // scheduleReconnect is the backoff both failure paths share, and the
  // Reconnecting state counting down to it. Retry now skips the wait.
  function scheduleReconnect(error) {
    clearTimeout(reconnectTimer);
    const until = now() + reconnectDelay;
    reconnectAttempt++;
    overlay.show(reconnectingState({ remainingMs: reconnectDelay, error, attempt: reconnectAttempt }), { retryNow: retryNow }, until);
    reconnectTimer = setTimeout(connect, reconnectDelay);
    // Back off so a server that is down does not get hammered, but cap it
    // low enough that a laptop waking from sleep recovers quickly.
    reconnectDelay = Math.min(reconnectDelay * 1.6, 5000);
  }

  // showEnded is the screen for a target the server ended on purpose. Start it
  // connects once with start=1, which lifts the server's reopen guard for a
  // shut down; reconnecting on its own would be refused.
  function showEnded() {
    clearTimeout(reconnectTimer);
    const reason = ended;
    report({ event: 'ended', target: targetId, reason, via: endedVia });
    overlay.show(shutDownState({ label: label(targetId), reason }), {
      start: () => {
        ended = null;
        endedVia = '';
        startNext = true;
        reconnectDelay = 500;
        reconnectAttempt = 0;
        overlay.show(connectingState({ label: label(targetId) }));
        connect();
      },
      pick: deps.openPicker || noop,
      reload: () => location.reload(),
    });
  }

  function retryNow() {
    // An attempt already running would only queue a second one behind it.
    if (connecting) return;
    clearTimeout(reconnectTimer);
    reconnectDelay = 500;
    connect();
  }

  // -------------------------------------------------------------------------
  // Transport
  //
  // Which transport carries the stream is decided in transport.js. Everything
  // below deals in decoded frames and control messages and never knows the
  // difference.
  // -------------------------------------------------------------------------
  async function connect() {
    if (closed) return;
    if (!targetId) return;
    // An ended target is reopened only by Start or a target switch, which clear
    // ended first. A queued or timer-driven connect must not.
    if (ended) { connectPending = false; return; }
    if (connecting) {
      connectPending = true;
      return;
    }
    connecting = true;
    const gen = ++connGen;
    const current = () => gen === connGen;
    rotator.reset();
    report({ event: 'connect', target: targetId, preferWT });
    clearTimeout(reconnectTimer);
    manualClose = false;
    const start = startNext;
    startNext = false;
    setStatus(null);

    try {
      const conn = await connectTransport({
        target: targetId,
        preferWebTransport: preferWT,
        start,
        endedFromClose,
        onControl: (buf) => { if (current()) handleControl(buf); },
        onFrame: (f) => { if (current()) { rotator.noteFrame(f); handleFrame(f); } },
        onOpen: () => {
          if (!current()) return;
          // The gap across a reconnect is not transit variation, and a fresh
          // session restarts its timestamps.
          arrival.reset();
          // The overlay stays and the backoff is not reset until handleInit.
          // The server accepts WebTransport before it opens the target, so a
          // transport that is open says nothing yet: a shut-down AVD is still
          // booting, and a refused target closes without ever sending init.
        },
        onClose: (info) => { if (current()) connClosed(info); },
      });
      // Superseded while the handshake was running: a target switch, or a
      // suspend. Its callbacks are already inert; close it so the server does not
      // keep a viewer for a connection nothing reads.
      if (!current()) {
        try { conn.close(); } catch (_) {}
        return;
      }
      // Whatever this replaces is closed here rather than trusted to be closed
      // already: a backoff retry or a resume can start a connect beside a live
      // transport, and the server then counts two viewers for one tab, both
      // feeding the same decoder.
      const prev = ws;
      ws = conn;
      if (prev && prev !== conn) { try { prev.close(); } catch (_) {} }
      transportName = ws.name;
      if (m.trans) m.trans.textContent = ws.name === 'webtransport' ? 'wt' : 'ws';
    } catch (err) {
      if (!current()) return;
      setStatus('error');
      const why = endedFromClose(err && err.closeInfo);
      if (why) {
        ended = why;
        endedVia = 'close';
        showEnded();
        return;
      }
      scheduleReconnect(String(err && err.message ? err.message : err));
      onConnectFailed(pane, targetId);
    } finally {
      connecting = false;
      if (connectPending) {
        connectPending = false;
        if (!ended) connect();
      }
    }
  }

  // connClosed handles the current connection closing, whether connect() opened
  // it or a rotation promoted it.
  function connClosed(info) {
    const why = endedFromClose(info);
    if (why && !ended) { ended = why; endedVia = 'close'; }
    setStatus('error');
    teardownDecoder();
    // The banner described that session; the next one sends its own. The nav
    // row did too, and the page clears it (client Task 7 leftover).
    mods.control.clear();
    onClosed(pane, info);
    const next = afterClose({ ended, manualClose });
    if (next === 'none') return;
    if (next === 'ended') { showEnded(); return; }
    // Code 2 is the server closing a connection Safari's flow control had
    // stopped (peerwindow.go, trap 52), most likely after a rotation failed.
    // The server is fine, so there is nothing to back off from.
    if (closedExhausted(info)) {
      // A rotation already under way gets a moment to promote first: the
      // credit stall closes at 6s, and a standby's first keyframe has taken
      // 2.3s on the phone. connect() would drop the standby and start over.
      const wait = rotator.pending ? ROTATE_GRACE_MS : 0;
      report({ event: 'wt-exhausted', pending: wait > 0, code: info.code, received: info.received });
      clearTimeout(reconnectTimer);
      reconnectTimer = setTimeout(connect, wait);
      return;
    }
    scheduleReconnect();
    onConnectFailed(pane, targetId);
  }

  // The rotator moves a WebTransport session to a fresh connection when the
  // server says this one is nearing a limit Safari will not raise (rotate.js,
  // trap 52).
  const rotator = lib.createRotator({
    open: (cb) => connectTransport({
      target: targetId,
      preferWebTransport: true,
      noFallback: true,
      onControl: cb.onControl,
      onFrame: cb.onFrame,
      onClose: cb.onClose,
    }),
    pointerDown: () => pointerIds.size > 0,
    promote: (conn, initBody) => {
      clearTimeout(reconnectTimer);
      const gen = ++connGen;
      const prev = ws;
      ws = conn;
      if (prev && prev !== conn) { try { prev.close(); } catch (_) {} }
      handleInit(initBody);
      return () => gen === connGen;
    },
    control: (buf) => handleControl(buf),
    frame: (f) => handleFrame(f),
    closed: (info) => connClosed(info),
    report,
    now,
  });

  // reconnect is the target switch's path, and the only one besides connect
  // and suspend that takes a new generation (trap 41).
  function reconnect(immediate) {
    clearTimeout(reconnectTimer);
    ended = null;
    endedVia = '';
    startNext = false;
    rotator.reset();
    connGen++;
    manualClose = true;
    if (ws) { try { ws.close(); } catch (_) {} }
    teardownDecoder();
    // An accessibility tree fetched for the target being left is meaningless
    // once the video and every /api/ui-derived box on screen belongs to a
    // different device (trap 41, one layer up); turning inspect off is
    // simpler and more honest than trying to keep it running against a target
    // nobody asked it to watch (review HIGH-3).
    mods.inspect.off();
    // A marker still fading belongs to the device being left, and so does
    // who is driving it.
    mods.touches.clear();
    mods.cursor.clear();
    mods.overlays.clear();
    mods.control.clear();
    setTimeout(connect, immediate ? 0 : 200);
  }

  function handleControl(buf) {
    switch (buf[0]) {
      case MSG_INIT: return handleInit(buf.subarray(1));
      case MSG_STATS: return handleStats(buf.subarray(1));
      case MSG_PONG: return handlePong(buf);
      case MSG_ENDED: {
        let reason = '';
        try { reason = JSON.parse(new TextDecoder().decode(buf.subarray(1))).reason; } catch (_) {}
        ended = reason || 'shutDown';
        endedVia = 'msg';
        return;
      }
      case MSG_ROTATE:
        if (ws && ws.name === 'webtransport') rotator.start(buf.subarray(1));
        return;
      // MsgEvent, MsgClipboard and MSG_CONTROL belong to the modules.
      default: return onControlMessage(pane, buf[0], buf.subarray(1));
    }
  }

  function handleInit(body) {
    const next = JSON.parse(new TextDecoder().decode(body));
    // !decoder is the case this missed. onClose tears the decoder down but
    // leaves info, so a reconnect to the same encoder compared identical,
    // skipped configureDecoder and left nothing to submit to. Assigning
    // canvas.width just above clears the canvas, so the result is a black screen
    // that never recovers while the byte counter climbs -- the tell is `queue`
    // reading `--` with kbps in the thousands.
    const changed = !decoder ||
      !info ||
      info.codec !== next.codec ||
      info.codedWidth !== next.codedWidth ||
      info.codedHeight !== next.codedHeight;

    info = next;
    // The ceiling is a server setting: the operator knows whether this is a LAN
    // or a cellular link, and the client cannot. An older server sends nothing,
    // so keep the compiled-in default rather than collapsing the buffer to zero.
    if (Number.isFinite(info.maxBufferMs)) jitterBuf.maxBufferMs = info.maxBufferMs;
    // Only when something changed: assigning clears the canvas even at the same
    // size, and a rotation (rotate.js) promotes a new connection onto a decoder
    // that is still showing the picture.
    if (changed) {
      el.canvas.width = info.codedWidth;
      el.canvas.height = info.codedHeight;
    }
    // The pane's own modules read the session's init first: the page's nav
    // row and keyboard ask pane.hasControl(), which the control module answers.
    mods.control.onInit(info);
    mods.touches.onInit(info);
    // A rotation changes codedWidth/Height without necessarily changing the
    // accessibility tree inspect.js is still holding (trap 42) -- notifyResize
    // is what makes a stale tree's boxes stop drawing rather than sitting
    // against a device pixel space the canvas no longer has.
    mods.inspect.notifyResize(info.codedWidth, info.codedHeight);
    // noteInit resets the log's last seq to 0 before its own catch-up fetch,
    // so a fresh connection never reads as "after 300" against a session it
    // has not seen yet -- unless this is a same-session reconnect, in which
    // case the log keeps the entries and dedupes rather than doubling the
    // ring (see eventlog.js's EventLog.noteInit doc comment).
    mods.eventlog.noteInit();
    // The nav row, the fold and the keyboard are the page's.
    onInit(pane, info);
    setStatus('live');
    reconnectDelay = 500;
    reconnectAttempt = 0;
    hideOverlay();

    // Every init refits: the nav row the init turns on changes the box.
    onShape(pane, { width: info.codedWidth, height: info.codedHeight });
    if (changed) configureDecoder(info);
  }

  // -------------------------------------------------------------------------
  // Decoder
  // -------------------------------------------------------------------------
  function configureDecoder(cfg) {
    closeDecoder();

    const d = deps.createDecoder({
      output: (frame) => {
        decodedThisSecond++;
        // Hand the frame to the jitter buffer rather than drawing it now. See
        // the presentation clock below for why arrival time is the wrong time.
        enqueueFrame(frame);
      },
      error: (err) => {
        decodeErrors++;
        console.warn('decoder error', err);
        reportDecodeError('callback', err);
        // Rebuild and wait for the next keyframe rather than showing corruption.
        sawKeyframe = false;
        try { d.close(); } catch (_) {}
        // Not across a suspend: the error usually lands just before one, and a
        // rebuild then would open a decoder on a hidden page for nothing.
        const gen = connGen;
        setTimeout(() => {
          if (gen === connGen && info) configureDecoder(info);
        }, 100);
      },
    });

    // No `description` means Annex B, which is what both scrcpy and ffmpeg emit.
    // optimizeForLatency asks the decoder not to buffer pictures before output;
    // without it, hardware decoders commonly hold several frames and add 30ms or
    // more that no amount of network tuning will recover.
    d.configure({
      codec: cfg.codec,
      optimizeForLatency: true,
    });

    decoder = d;
    sawKeyframe = false;
  }

  // closeDecoder keeps pendingConfig; see its comment in the state for why only
  // teardownDecoder, on a connection ending, may drop it.
  function closeDecoder() {
    if (decoder) {
      try { if (decoder.state !== 'closed') decoder.close(); } catch (_) {}
      decoder = null;
    }
    flushQueue();
    sawKeyframe = false;
  }

  function teardownDecoder() {
    closeDecoder();
    pendingConfig = null;
  }

  function reportDecodeError(where, err) {
    const t = now();
    if (t - lastDecodeErrorReport < 1000) return;
    lastDecodeErrorReport = t;
    flushDecodeError(null);
    pendingDecodeError = {
      rec: {
        event: 'decode-error',
        where,
        err: String((err && err.message) || err),
        haveConfig: !!pendingConfig,
        count: decodeErrors,
        visibility: visibility(),
        // Under PiP no suspend follows a hidden page's error, so without this
        // it would read as a stream fault.
        ...(pipActive() ? { pip: true } : {}),
      },
      at: t,
    };
    // Every "Decoder failure" in the traces of 26-28 September came 3-429ms
    // before a suspend, on a stream decoding normally until then. A second is
    // enough to see the suspend, and short enough that a real fault still
    // arrives within one stats report of the second it happened.
    clearTimeout(decodeErrorTimer);
    decodeErrorTimer = setTimeout(() => flushDecodeError(null), 1000);
  }

  // flushDecodeError sends the held decode-error. suspendAt is the time of a
  // suspend that followed it, which marks it as the page going away rather
  // than the stream failing; null means no suspend came.
  function flushDecodeError(suspendAt) {
    clearTimeout(decodeErrorTimer);
    const pending = pendingDecodeError;
    if (!pending) return;
    pendingDecodeError = null;
    const rec = pending.rec;
    if (suspendAt !== null) rec.beforeSuspendMs = Math.round(suspendAt - pending.at);
    report(rec);
  }

  function handleFrame(f) {
    bytesThisSecond += f.bytes;

    if (f.config) {
      pendingConfig = f.payload.slice();
      return;
    }

    // Against the frame's own timestamp, not against the previous gap: the
    // source is not constant rate, so an idle screen is not jitter. Below the
    // config guard deliberately -- scrcpy reports pts 0 for a parameter-set
    // frame and device uptime for every media frame, so the pair straddling one
    // reads as the whole uptime. See ArrivalJitter in jitter.js.
    arrival.observe(now(), f.ptsMicros / 1000);

    if (!decoder || decoder.state !== 'configured') return;

    // Everything before the first keyframe decodes into garbage.
    if (!sawKeyframe && !f.key) {
      dropped++;
      return;
    }

    let data = f.payload;
    if (f.key && pendingConfig) {
      data = new Uint8Array(pendingConfig.length + f.payload.length);
      data.set(pendingConfig, 0);
      data.set(f.payload, pendingConfig.length);
    }

    try {
      // The EncodedVideoChunk is built by createDecoder's decode, so this
      // runs without WebCodecs under test.
      decoder.decode({
        type: f.key ? 'key' : 'delta',
        timestamp: f.ptsMicros,
        data,
      });
      if (f.key) sawKeyframe = true;
    } catch (err) {
      decodeErrors++;
      sawKeyframe = false;
      reportDecodeError('decode', err);
    }
  }

  // -------------------------------------------------------------------------
  // Presentation
  //
  // The jitter buffer itself lives in jitter.js so it can be tested without a
  // DOM. See web/jitter.test.mjs.
  // -------------------------------------------------------------------------

  const arrival = new lib.ArrivalJitter();

  const jitterBuf = new lib.JitterBuffer({
    // Replaced from the init message. This is only what applies before one
    // arrives, which is no frames at all.
    maxBufferMs: 50,
    dispose: (item) => item.frame.close(),
  });

  function enqueueFrame(frame) {
    // timestamp is the microsecond PTS supplied in the EncodedVideoChunk.
    jitterBuf.enqueue({ frame, ptsMs: frame.timestamp / 1000 }, now());
    startPresenting();
  }

  function flushQueue() { jitterBuf.flush(); }

  function startPresenting() {
    if (rafHandle) return;
    // A hidden page gets no rAF, and under PiP it is still being watched, so
    // the same loop runs from a timer until the page shows again.
    // Unexercised on iOS: Safari on 27.2 keeps document.hidden false while the
    // page is in PiP, so rAF keeps running and this branch never fired on the
    // phone. It is kept for a browser that does hide the page.
    if (hidden() && pipActive()) {
      rafHandle = setTimeout(present, 8);
      rafIsTimer = true;
      return;
    }
    rafHandle = raf(present);
    rafIsTimer = false;
  }

  function present() {
    rafHandle = 0;
    jitterBuf.tick(arrival.value);

    const item = jitterBuf.due(now());
    if (item) {
      try {
        ctx.drawImage(item.frame, 0, 0, el.canvas.width, el.canvas.height);
        renderedThisSecond++;
      } finally {
        item.frame.close();
      }
    }

    if (jitterBuf.held || (ws && ws.open)) {
      startPresenting();
    }
  }

  // -------------------------------------------------------------------------
  // Metrics
  // -------------------------------------------------------------------------
  // Report the metrics only this side can see, so the server can join them with
  // its own counters into a single trace record.
  function sendClientStats() {
    if (!ws || !ws.open) return;
    const body = JSON.stringify({
      fps,
      decFps,
      rttMs: Math.round(rtt * 10) / 10,
      // The last second's pings alone, which is what the server judges the
      // dot on; rttMs above is smoothed for the readout and lags a recovery.
      rttNowMs: takeSecondRtt(),
      jitterMs: Math.round(arrival.value * 10) / 10,
      kbps,
      dropped,
      decodeErrors,
      decodeQueue: decoder ? decoder.decodeQueueSize : -1,
      bufferMs: Math.round(jitterBuf.bufferMs * 10) / 10,
      // What frames actually waited, which bufferMs alone cannot say: the clock
      // is anchored against the transit floor, not recomputed from bufferMs.
      holdMs: jitterBuf.drainHolds(),
      interactive: jitterBuf.interactive,
      held: jitterBuf.held,
      overruns: jitterBuf.overruns,
      skipped: jitterBuf.skipped,
      codec: info ? info.codec : null,
      size: info ? `${info.codedWidth}x${info.codedHeight}` : null,
      visible: !hidden(),
      build: deps.build,
      transport: transportName,
    });
    const bytes = new TextEncoder().encode(body);
    const buf = new Uint8Array(1 + bytes.length);
    buf[0] = MSG_CLIENT_STATS;
    buf.set(bytes, 1);
    ws.send(buf);
  }

  function sendPing() {
    if (!ws || !ws.open) return;
    const buf = new ArrayBuffer(9);
    const dv = new DataView(buf);
    dv.setUint8(0, MSG_PING);
    // performance.now() is sub-millisecond and monotonic; wall clock is neither.
    dv.setBigUint64(1, BigInt(Math.round(now() * 1000)));
    ws.send(buf);
  }

  // Server-side shedding is reported separately from client-side drops so the
  // two causes stay legible: shed means the server protected latency by refusing
  // to send, dropped means frames arrived that this client could not use.
  function handleStats(body) {
    try {
      const s = JSON.parse(new TextDecoder().decode(body));
      shedRate = s.shedRate || 0;
      health = s.health;
      healthReason = s.healthReason || '';
      statsReceivedAt = now();
      onStats(pane);
    } catch (_) {}
  }

  function handlePong(buf) {
    const dv = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
    const sentMicros = Number(dv.getBigUint64(1));
    const sample = now() - sentMicros / 1000;
    // Smooth so a single scheduling hiccup does not dominate the readout.
    rtt = rtt ? rtt + (sample - rtt) / 8 : sample;
    rttThisSecond.push(sample);
  }

  function takeSecondRtt() {
    const v = secondRtt(rttThisSecond);
    rttThisSecond.length = 0;
    return v === undefined ? undefined : Math.round(v * 10) / 10;
  }

  function grade(elem, value, warnAt, badAt) {
    if (!elem) return;
    elem.classList.toggle('warn', value >= warnAt && value < badAt);
    elem.classList.toggle('bad', value >= badAt);
  }

  // tick is the 1s metrics step. app.js's one interval calls it for every
  // pane, then repaints what reads the result.
  function tick() {
    fps = renderedThisSecond;
    renderedThisSecond = 0;
    decFps = decodedThisSecond;
    decodedThisSecond = 0;
    kbps = Math.round(bytesThisSecond * 8 / 1000);
    bytesThisSecond = 0;

    if (m.fps) {
      m.fps.textContent = fps;
      m.dec.textContent = decFps;
      m.dec.classList.toggle('warn', decFps > 0 && decFps < 55);
      m.dec.classList.toggle('bad', decFps > 0 && decFps < 40);
      m.rtt.textContent = rtt ? rtt.toFixed(1) + 'ms' : '--';
      m.jit.textContent = arrival.value.toFixed(1) + 'ms';
      m.q.textContent = decoder ? decoder.decodeQueueSize : '--';
      m.drop.textContent = dropped + (decodeErrors ? '/' + decodeErrors : '');
      m.buf.textContent = jitterBuf.bufferMs.toFixed(0) + 'ms';
      grade(m.buf, jitterBuf.bufferMs, 35, 48);
      m.shed.textContent = shedRate;
      grade(m.shed, shedRate, 5, 20);
      m.rate.textContent = kbps;

      // 55fps is the threshold where a 60fps stream starts reading as uneven.
      m.fps.classList.toggle('warn', fps > 0 && fps < 55);
      m.fps.classList.toggle('bad', fps > 0 && fps < 40);
      grade(m.rtt, rtt, 60, 120);
      grade(m.jit, arrival.value, 4, 10);
    }

    sendClientStats();
  }

  // -------------------------------------------------------------------------
  // Suspend and resume (trap 21)
  // -------------------------------------------------------------------------
  // iOS does not merely throttle a backgrounded or locked page, it freezes it:
  // rAF stops, timers stop, and a reconnect scheduled from onClose may never run
  // -- or never be scheduled at all, if the freeze lands first. A locked phone
  // therefore sat 56 minutes without one connect attempt and came back only on a
  // manual reload. visibilitychange still fires *before* the freeze, so both the
  // release and the resume are done from app.js's handlers and nothing
  // afterwards is assumed to run.
  function suspendStream() {
    if (!hidden()) return;
    // PiP is still showing the stream. Leaving PiP while hidden calls this
    // again (see pip's onChange). Unexercised on iOS, like the timer in
    // startPresenting: 27.2 never hides a page in PiP, so pipCarried stayed
    // false on the phone.
    if (pipActive()) {
      pipCarried = true;
      startPresenting();
      return;
    }
    // Drop the transport deliberately. Nothing can be painted while hidden, and
    // a server writing into a client that has stopped reading backs the
    // connection up until it dies of its own idle timeout -- which is how the
    // stream was lost in the first place, 114 seconds of frames after the last
    // one the phone could show.
    //
    // A decode-error still held is from the page going away, not the stream:
    // iOS fails the decoder just before it hides the page. Sent first, so it
    // precedes the suspend in the trace as it did on the phone.
    flushDecodeError(now());
    report({ event: 'suspend', open: !!(ws && ws.open) });
    clearTimeout(reconnectTimer);
    connGen++;
    connectPending = false;
    manualClose = true;
    rotator.reset();
    if (ws) { try { ws.close(); } catch (_) {} }
    teardownDecoder();
  }

  function resumeStream() {
    if (hidden()) return;
    report({ event: 'resume', open: !!(ws && ws.open), ...(pipCarried ? { pip: true } : {}) });
    // A timer-driven present() is replaced by rAF on its next schedule.
    if (rafIsTimer && rafHandle) { clearTimeout(rafHandle); rafHandle = 0; startPresenting(); }
    const carried = pipCarried;
    pipCarried = false;
    const what = afterResume({ ended, open: !!(ws && ws.open) });
    // The server ended this target: the overlay stays until Start or a pick.
    if (what === 'stay') return;
    if (what === 'refresh') {
      // PiP kept decoding and painting the whole time, so there is no gap to
      // recover from and no keyframe to wait for.
      if (carried) return;
      sawKeyframe = false;
      arrival.reset();
      // The presentation clock is meaningless after rAF was suspended.
      flushQueue();
      return;
    }
    // Coming to the foreground is the only resume signal that survives a freeze,
    // so treat it as one: clear any pending backoff and reconnect now.
    clearTimeout(reconnectTimer);
    reconnectDelay = 500;
    reconnectAttempt = 0;
    connect();
  }

  // -------------------------------------------------------------------------
  // Input
  // -------------------------------------------------------------------------
  // Every listener sends through send(), which reads the current transport at
  // call time, so a switch or a rotation never leaves input on a closed one.

  // Browser pointer ids are arbitrary and can be large; scrcpy wants stable small
  // ids per live finger. This map owns that translation.
  const pointerIds = new Map();
  let nextPointerId = 1;

  function pointerIdFor(ev) {
    if (!pointerIds.has(ev.pointerId)) pointerIds.set(ev.pointerId, nextPointerId++);
    return pointerIds.get(ev.pointerId);
  }

  function toStreamCoords(ev) {
    const rect = el.canvas.getBoundingClientRect();
    // Clamp: a drag that leaves the canvas should pin to the edge rather than
    // inject an off-screen coordinate the device will reject.
    const x = Math.min(Math.max(ev.clientX - rect.left, 0), rect.width);
    const y = Math.min(Math.max(ev.clientY - rect.top, 0), rect.height);
    return {
      x: Math.round(x / rect.width * el.canvas.width),
      y: Math.round(y / rect.height * el.canvas.height),
    };
  }

  function sendTouch(action, ev) {
    if (!ws || !ws.open) return;
    if (info && !info.control) return;

    const { x, y } = toStreamCoords(ev);
    const pid = pointerIdFor(ev);
    const pressure = ev.pressure > 0 ? ev.pressure : 1;

    const buf = new ArrayBuffer(16);
    const dv = new DataView(buf);
    dv.setUint8(0, MSG_TOUCH);
    dv.setUint8(1, action);
    dv.setUint32(2, pid);
    dv.setInt32(6, x);
    dv.setInt32(10, y);
    dv.setUint16(14, Math.round(Math.min(pressure, 1) * 0xffff));
    send(buf);

    if (action === ACTION_UP || action === ACTION_CANCEL) {
      pointerIds.delete(ev.pointerId);
    }
  }

  el.canvas.addEventListener('pointerdown', (ev) => {
    ev.preventDefault();
    // Capture so a finger that slides off the canvas still reports moves and a
    // release, instead of leaving a stuck pointer on the device.
    try { el.canvas.setPointerCapture(ev.pointerId); } catch (_) {}
    sendTouch(ACTION_DOWN, ev);
    setInteractive(true);
  });

  el.canvas.addEventListener('pointermove', (ev) => {
    if (!pointerIds.has(ev.pointerId)) return;
    ev.preventDefault();
    // Coalesced events preserve the true motion path, which is what makes fling
    // velocity match the device instead of arriving as a few coarse jumps.
    const events = ev.getCoalescedEvents ? ev.getCoalescedEvents() : [ev];
    for (const e of events) sendTouch(ACTION_MOVE, e.pointerId !== undefined ? e : ev);
  });

  // While a finger is down the jitter buffer's cushion is capped (jitter.js), so
  // the frames answering a touch are drawn sooner. It stays capped briefly after
  // the last finger lifts, so the start of a fling is as live as the drag was.
  let interactiveTimer = 0;
  function setInteractive(on) {
    clearTimeout(interactiveTimer);
    if (on) jitterBuf.interactive = true;
    else interactiveTimer = setTimeout(() => { jitterBuf.interactive = false; }, 300);
  }

  function endPointer(action) {
    return (ev) => {
      if (!pointerIds.has(ev.pointerId)) return;
      ev.preventDefault();
      sendTouch(action, ev);
      if (!pointerIds.size) setInteractive(false);
      try { el.canvas.releasePointerCapture(ev.pointerId); } catch (_) {}
    };
  }

  el.canvas.addEventListener('pointerup', (ev) => {
    endPointer(ACTION_UP)(ev);
    // A completed tap or swipe is the one gesture likely to change the
    // foreground app; the page refetches it (appcard.js's own throttle).
    onTouchEnd(pane);
  });
  el.canvas.addEventListener('pointercancel', endPointer(ACTION_CANCEL));

  el.canvas.addEventListener('wheel', (ev) => {
    if (!ws || !ws.open) return;
    if (info && !info.control) return;
    ev.preventDefault();
    const { x, y } = toStreamCoords(ev);
    const buf = new ArrayBuffer(17);
    const dv = new DataView(buf);
    dv.setUint8(0, MSG_SCROLL);
    dv.setInt32(1, x);
    dv.setInt32(5, y);
    dv.setFloat32(9, -ev.deltaX / 120);
    dv.setFloat32(13, -ev.deltaY / 120);
    send(buf);
  }, { passive: false });

  function send(bytes) {
    if (!ws || !ws.open) return false;
    ws.send(bytes);
    return true;
  }

  // close ends the pane for good: no reconnect, the decoder closed, the render
  // loop cancelled.
  function close() {
    if (closed) return;
    closed = true;
    clearTimeout(reconnectTimer);
    clearTimeout(decodeErrorTimer);
    clearTimeout(interactiveTimer);
    connGen++;
    connectPending = false;
    manualClose = true;
    rotator.reset();
    if (ws) { try { ws.close(); } catch (_) {} }
    teardownDecoder();
    if (rafHandle) {
      if (rafIsTimer) clearTimeout(rafHandle); else cancelRaf(rafHandle);
      rafHandle = 0;
    }
  }

  const pane = {
    id,
    els,
    // Each per-pane module's handle: { touches, cursor, control, inspect, eventlog, record }.
    get mods() { return mods; },
    get targetId() { return targetId; },
    get info() { return info; },
    get status() { return status; },
    get transportName() { return transportName; },
    get overlay() { return overlay; },
    get open() { return !!(ws && ws.open); },
    get connecting() { return connecting; },
    get hasDecoder() { return !!decoder; },
    get generation() { return connGen; },
    get ended() { return ended; },
    get targetHeadless() { return targetHeadless; },
    set targetHeadless(v) { targetHeadless = !!v; },
    get frame() { return frame; },
    set frame(v) { frame = v; },
    get frameFor() { return frameFor; },
    set frameFor(v) { frameFor = v; },
    stats() {
      return {
        fps, decFps, kbps, rtt, dropped, decodeErrors, shedRate, health, healthReason,
        statsReceivedAt,
        jitter: arrival.value,
        bufferMs: jitterBuf.bufferMs,
        queue: decoder ? decoder.decodeQueueSize : null,
      };
    },
    connect({ explicitStart = false } = {}) {
      if (closed) return;
      if (explicitStart) startNext = true;
      connect();
    },
    // switchTarget moves the pane to another target: the old one's verdict is
    // cleared rather than carried forward (trap 38's shape one layer up), and
    // reconnect takes a new generation so the old transport's close is inert.
    switchTarget(next) {
      if (closed) return;
      // Stops this page's own recording on the old target, never an agent's.
      mods.record.onTargetSwitch(next);
      mods.flag.onTargetSwitch();
      targetId = next;
      health = undefined;
      healthReason = '';
      statsReceivedAt = NaN;
      reconnect(true);
    },
    send,
    // Whether the current session takes input, and the headers that mark this
    // pane's own /api calls as its viewer's: both from this connection's init.
    hasControl: () => mods.control.hasControl(),
    viewerHeaders: () => mods.control.headers(),
    retryNow,
    suspend: suspendStream,
    resume: resumeStream,
    tick,
    ping: sendPing,
    close,
    report,
  };

  // The per-pane modules, each mounted once against this pane. Their reports
  // go through the pane's report, so they carry pane and target too.
  const mdeps = deps.moduleDeps || {};
  const mount = deps.modules;
  mods = {
    touches: mount.touches(el.touchLayer, {
      canvas: el.canvas,
      storage: mdeps.storage,
      report,
    }),
    // Not behind "Show touches": they are how a person sees that an agent is
    // driving, not a debugging aid.
    cursor: mount.cursor(el.cursorLayer, { canvas: el.canvas }),
    // What an extension put over the screen; the server resends it after
    // every init, so a reconnect redraws it.
    overlays: mount.overlays(el.extLayer, { canvas: el.canvas, storage: mdeps.storage }),
    // The banner goes in the pane's root, which also carries the data-agent
    // that glows the device (style.css).
    control: mount.control({
      doc: mdeps.doc,
      host: el.root,
      report,
      takeOver: () => mdeps.takeOver(pane),
      onHeldByOther: (on) => { if (mdeps.onHeldByOther) mdeps.onHeldByOther(pane, on); },
    }),
    inspect: mount.inspect(el.inspectLayer, {
      targetId: () => pane.targetId,
      screenRect: () => el.canvas.getBoundingClientRect(),
      sheet: mdeps.inspectSheet,
      tab: 'tree',
      postInput: (body) => mdeps.postInput(pane, body),
      onToggle: (on) => { if (mdeps.onInspectToggle) mdeps.onInspectToggle(pane, on); },
      toast: mdeps.toast,
      report,
    }),
    eventlog: mount.eventlog(el.logList, {
      getTarget: () => pane.targetId,
      getGeneration: () => pane.generation,
      // Rendering into logList is wasted unless its page is the one shown.
      isOpen: () => !!(mdeps.logIsOpen && mdeps.logIsOpen()),
      badgeEl: el.logBadge,
    }),
    flag: mount.flag(el.flagLayer, {
      doc: mdeps.doc,
      host: el.root,
      targetId: () => pane.targetId,
      pictureRect: () => el.canvas.getBoundingClientRect(),
      fetch: mdeps.fetch,
      headers: () => pane.viewerHeaders(),
      beforeEnter: () => { if (mdeps.beforeFlag) mdeps.beforeFlag(pane); },
      focusNote: () => !mdeps.focusFlagNote || mdeps.focusFlagNote(),
      toast: mdeps.toast,
      report,
    }),
    record: mount.record({
      doc: mdeps.doc,
      host: el.root,
      isFront: () => !mdeps.isFront || mdeps.isFront(pane),
      fetch: mdeps.fetch,
      targetId: () => pane.targetId,
      pillRec: mdeps.pillRec,
      report,
      toast: mdeps.toast,
    }),
  };
  return pane;
}
