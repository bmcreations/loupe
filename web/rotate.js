// Moving a WebTransport session to a fresh connection before Safari's
// flow-control limits stop it (trap 52).
//
// Safari grants a QUIC connection 16 MiB of stream data and never raises it,
// so a connection that has carried that much delivers nothing more. The server
// counts what each connection has spent and sends MsgRotate before the limit.
// This opens a second connection beside the first, waits for its first
// keyframe, and then makes it the current one, so the picture carries on from
// that keyframe instead of freezing.
//
// It lives here and not in app.js because app.js touches the DOM at module
// scope and has no harness (trap 21). Everything with a side effect comes in
// through deps:
//
//   open(callbacks)  -> Promise<conn>   a WebTransport connection, no fallback
//   pointerDown()    -> bool            a finger is on the canvas
//   promote(conn, initBody) -> isCurrent
//                                       make conn the session's connection and
//                                       apply its init; returns the predicate
//                                       pane.js gates that connection's
//                                       callbacks on from then on
//   control(buf), frame(f), closed(info)
//                                       pane.js's handlers for a current
//                                       connection
//   report(rec), now()
//
// The standby is never promoted mid-gesture: the server lifts a departing
// viewer's fingers, which would end a drag the person is still making. It waits
// for the next keyframe with no finger down instead, about a second later.
//
// It does not switch back or retry. A standby that fails is closed and the old
// connection carries on; if it then reaches the limit, the server closes it
// with code 2 and app.js reconnects (peerwindow.go's backstop).

export const MSG_INIT = 0x01;
export const MSG_ROTATE = 0x09;

// EXHAUSTED_RECEIVED is how many frame bytes a WebTransport connection must
// have delivered before a close without a code is read as the server's
// backstop (peerwindow.go, trap 52). The server's code 2 cannot reach the
// page: its close capsule goes on the CONNECT stream, inside the same spent
// window, and on the phone on 12 October it was cancelled unsent both times.
// The server closes at 15.5 MiB spent; frames reset in flight are spent and
// never received, so this sits lower, at the rotation threshold.
export const EXHAUSTED_RECEIVED = 12 << 20;

// closedExhausted reports whether a connection's close info means the server
// closed it for Safari's flow-control limit, so the page should reconnect at
// once rather than back off.
export function closedExhausted(info) {
  if (!info) return false;
  if (info.code === 2) return true;
  // No code, or 0, which is what a session torn down without a capsule may
  // read as. Any other code is the server saying why, and is believed.
  return !info.code && (info.received || 0) >= EXHAUSTED_RECEIVED;
}

export function createRotator(deps) {
  let standby = null;
  // The newest timestamp the current connection delivered, and when. Both
  // connections are viewers of the same session and receive the same frames,
  // so the standby's first keyframe can be one the current connection already
  // delivered; frames at or before this are skipped after a promotion.
  let lastPts = -1;
  let lastFrameAt = 0;

  function abort(why) {
    const s = standby;
    if (!s) return;
    standby = null;
    if (s.conn) { try { s.conn.close(); } catch (_) {} }
    if (why) deps.report({ event: 'rotate-failed', stage: why, ms: Math.round(deps.now() - s.startedAt) });
  }

  // Only a keyframe promotes. The server sends the codec config the moment a
  // viewer attaches and the keyframe up to a second later, and promoting on
  // the config closed the old connection with nothing to replace it: about
  // 0.8s of no video at every rotation on the phone. The config is held and
  // delivered ahead of the keyframe instead.
  function tryPromote(s, f) {
    if (!s.conn || !s.init || !f.key) return false;
    if (deps.pointerDown()) {
      s.waitedForPointer = true;
      return false;
    }
    standby = null;
    s.isCurrent = deps.promote(s.conn, s.init);
    s.promoted = true;
    const now = deps.now();
    deps.report({
      event: 'rotate-promote',
      ms: Math.round(now - s.startedAt),
      gapMs: lastFrameAt ? Math.round(now - lastFrameAt) : null,
      waitedForPointer: !!s.waitedForPointer,
    });
    return true;
  }

  // deliver hands a promoted connection's frame on, skipping the ones the old
  // connection already delivered. Once one is newer, nothing more is skipped.
  function deliver(s, f) {
    if (s.skipping) {
      if (!f.config && f.ptsMicros <= lastPts) return;
      if (!f.config) s.skipping = false;
    }
    noteFrame(f);
    deps.frame(f);
  }

  function noteFrame(f) {
    if (f.config) return;
    if (f.ptsMicros > lastPts) lastPts = f.ptsMicros;
    lastFrameAt = deps.now();
  }

  async function start(body) {
    if (standby) return;
    const s = {
      conn: null, init: null, config: null, promoted: false, isCurrent: null,
      skipping: true, waitedForPointer: false, startedAt: deps.now(),
    };
    standby = s;
    let asked = {};
    try { asked = JSON.parse(new TextDecoder().decode(body)); } catch (_) {}
    deps.report({ event: 'rotate-start', bytes: asked.bytes, streams: asked.streams });

    let conn;
    try {
      conn = await deps.open({
        onControl: (buf) => {
          if (s.promoted) { if (s.isCurrent()) deps.control(buf); return; }
          if (standby !== s) return;
          // Only init matters before the promotion. Stats and events from a
          // connection nobody is watching yet are the old connection's too.
          if (buf[0] === MSG_INIT) s.init = buf.slice(1);
        },
        onFrame: (f) => {
          if (s.promoted) { if (s.isCurrent()) deliver(s, f); return; }
          if (standby !== s) return;
          if (f.config) { s.config = f; return; }
          if (!tryPromote(s, f)) return;
          if (s.config) deliver(s, s.config);
          deliver(s, f);
        },
        onClose: (info) => {
          if (s.promoted) { if (s.isCurrent()) deps.closed(info); return; }
          if (standby === s) abort('closed');
        },
      });
    } catch (err) {
      if (standby === s) {
        standby = null;
        deps.report({ event: 'rotate-failed', stage: 'open', message: String((err && err.message) || err),
          ms: Math.round(deps.now() - s.startedAt) });
      }
      return;
    }
    if (standby !== s) {
      // Reset while the handshake ran: a reconnect or a target switch.
      try { conn.close(); } catch (_) {}
      return;
    }
    s.conn = conn;
    deps.report({ event: 'rotate-open', ms: Math.round(deps.now() - s.startedAt) });
  }

  return {
    start,
    // noteFrame records a frame the current connection delivered.
    noteFrame,
    // reset drops any standby, for a reconnect or a target switch, and forgets
    // the old session's timestamps.
    reset() {
      abort(null);
      lastPts = -1;
      lastFrameAt = 0;
    },
    get pending() { return standby !== null; },
  };
}
