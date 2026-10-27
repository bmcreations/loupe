// Transports.
//
// Two ways to carry the same stream, behind one interface so the rest of the
// client never branches on which is in use.
//
// WebSocket is TCP: reliable, ordered, and therefore unable to abandon a frame
// once written. A frame that stops mattering is still delivered, ahead of the
// frames behind it.
//
// WebTransport is QUIC. Each access unit gets its own unidirectional stream,
// which gives three things TCP cannot:
//   - frames are delivered independently, so a slow one cannot block the rest;
//   - each frame is still whole or absent, so there is no partial-frame
//     corruption to detect;
//   - the sender can reset a stream, so a frame that goes stale mid-flight
//     stops consuming the link.
//
// Both hand the caller the same two events: control messages (the existing
// binary envelope) and decoded frames.

const MSG_FRAME = 0x02;
const WS_FRAME_HEADER_LEN = 18;
const WT_FRAME_HEADER_LEN = 13;
const FLAG_KEY = 1 << 0;
const FLAG_CONFIG = 1 << 1;

/**
 * @param {object}   opts
 * @param {string}   opts.target     target id
 * @param {function} opts.onControl  (Uint8Array) for non-frame envelope messages
 * @param {function} opts.onFrame    ({ptsMicros, key, config, payload})
 * @param {function} opts.onOpen
 * @param {function} opts.onClose    ({code?, reason?, received}) on WebTransport, () on WebSocket
 * @param {boolean}  opts.preferWebTransport
 * @param {boolean}  opts.start      an explicit Start: lifts the server's reopen guard once
 * @param {function} opts.endedFromClose  ({code, reason}) -> reason | null; decided by the
 *                   caller, since modules here do not import each other (trap 45)
 * @param {boolean}  opts.noFallback  fail rather than fall back to WebSocket
 */
// The boot probe in index.html installs __loupeLog before this module is even
// fetched, so it is always here. Reporting from inside the handshake is the
// only way to see which await a browser stalls on: a stalled promise raises
// nothing, logs nothing, and looks identical to a client that never ran.
const report = (rec) => { try { window.__loupeLog && window.__loupeLog(rec); } catch (e) {} };

// How long the WebTransport handshake gets before the connection is abandoned
// for WebSocket. Every step of it can stall rather than reject -- Safari has
// been seen to leave `ready` pending indefinitely -- and an await that never
// settles takes the fallback down with it.
const WT_HANDSHAKE_TIMEOUT_MS = 6000;

// How long each stream-capability probe gets. Shorter than the handshake
// timeout so every probe has reported before the session is torn down for the
// WebSocket fallback.
const WT_PROBE_TIMEOUT_MS = 4000;

// probeStreams reports which stream directions this browser actually gets.
//
// Safari leaves createBidirectionalStream() pending indefinitely on a session
// whose `ready` resolved in under 200ms. From the server that looks the same as
// a client that cannot open any stream, so each direction is timed separately
// and reported on its own record. Nothing here blocks the handshake.
function probeStreams(wt, uniReader) {
  const probe = (name, run) => {
    const start = performance.now();
    let settled = false;
    const done = (rec) => {
      if (settled) return;
      settled = true;
      rec.event = 'wt-probe';
      rec.probe = name;
      rec.ms = Math.round(performance.now() - start);
      report(rec);
    };
    setTimeout(() => done({ result: 'timeout' }), WT_PROBE_TIMEOUT_MS);
    run().then(
      () => done({ result: 'ok' }),
      (e) => done({ result: 'error', message: String((e && e.message) || e) }),
    );
  };

  probe('client-uni', async () => {
    const s = await wt.createUnidirectionalStream();
    const w = s.getWriter();
    await w.write(new Uint8Array(4));
    await w.close();
  });

  // The server opens one of each immediately after the upgrade. Reading the
  // incoming streams here is what proves the session carries traffic at all.
  //
  // The unidirectional reader is the one readFrames goes on to use, because
  // taking a second reader on the same stream throws. The server's probe
  // payload is shorter than a frame header, so nothing downstream sees it.
  probe('server-uni', async () => {
    const { done } = await uniReader.read();
    if (done) throw new Error('closed');
  });
  probe('server-bidi', async () => {
    const { done } = await wt.incomingBidirectionalStreams.getReader().read();
    if (done) throw new Error('closed');
  });
}

export async function connect(opts) {
  if (opts.preferWebTransport && typeof WebTransport !== 'undefined') {
    try {
      return await connectWebTransport(opts);
    } catch (err) {
      // A rotation (rotate.js) wants a second WebTransport connection or
      // nothing: a WebSocket beside the WebTransport one would be a different
      // transport for the rest of the session.
      if (opts.noFallback) throw err;
      // Falling back rather than failing: WebTransport needs HTTP/3 reachable
      // over UDP, which a proxy or a restrictive network can quietly prevent
      // even when the page itself loaded fine.
      // A session the server closed to say the target ended is an answer, not
      // an unreachable network: WebSocket would only be refused the same way.
      if (err && err.closeInfo && opts.endedFromClose && opts.endedFromClose(err.closeInfo)) throw err;
      console.warn('webtransport unavailable, falling back to websocket:', err);
    }
  }
  return connectWebSocket(opts);
}

// ---------------------------------------------------------------------------
// WebSocket
// ---------------------------------------------------------------------------
// PROTOCOL is the session protocol range this client speaks, sent as ?proto=
// on /ws and /wt (session.NegotiateProtocol). A server that shares no version
// with it ends the connection with the reason unsupportedProtocol.
export const PROTOCOL = '1';

function connectWebSocket({ target, start, onControl, onFrame, onOpen, onClose }) {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const ws = new WebSocket(`${proto}//${location.host}/ws?target=${encodeURIComponent(target)}&proto=${PROTOCOL}${start ? '&start=1' : ''}`);
  ws.binaryType = 'arraybuffer';

  ws.onopen = () => onOpen && onOpen();
  ws.onclose = () => onClose && onClose();
  ws.onerror = () => {};
  ws.onmessage = (ev) => {
    const buf = new Uint8Array(ev.data);
    if (buf[0] !== MSG_FRAME) { onControl(buf); return; }
    const view = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
    onFrame({
      ptsMicros: Number(view.getBigUint64(2)),
      key: (buf[1] & FLAG_KEY) !== 0,
      config: (buf[1] & FLAG_CONFIG) !== 0,
      payload: buf.subarray(WS_FRAME_HEADER_LEN),
      bytes: buf.byteLength,
    });
  };

  return {
    name: 'websocket',
    send(bytes) {
      if (ws.readyState === WebSocket.OPEN) ws.send(bytes);
    },
    get open() { return ws.readyState === WebSocket.OPEN; },
    close() { try { ws.close(); } catch (_) {} },
  };
}

// ---------------------------------------------------------------------------
// WebTransport
// ---------------------------------------------------------------------------
async function connectWebTransport({ target, start, onControl, onFrame, onOpen, onClose }) {
  // WebTransport sends no cookies, so the token that authenticated this page
  // has to travel on the URL. The cookie is readable by script for exactly
  // this reason; the WebSocket path needs none of it, because its handshake is
  // ordinary HTTP and the browser attaches the cookie itself.
  const url = `https://${location.host}/wt?target=${encodeURIComponent(target)}&proto=${PROTOCOL}${start ? '&start=1' : ''}${tokenParam()}`;

  // WebTransport validates the server certificate through the normal chain,
  // which is what happens in real use. For a development server with a
  // self-signed certificate there is no chain to validate against, so the spec
  // provides serverCertificateHashes: pin one exact certificate instead. It
  // only accepts short-lived ECDSA P-256 certificates, deliberately, so this
  // cannot be used to pin a long-lived one in production.
  const opts = {};
  const pin = new URLSearchParams(location.search).get('certhash');
  if (pin) {
    opts.serverCertificateHashes = [{
      algorithm: 'sha-256',
      value: Uint8Array.from(atob(pin), (c) => c.charCodeAt(0)),
    }];
  }

  const wt = new WebTransport(url, opts);

  // Taken once and shared: the capability probe reads the server's first
  // unidirectional stream from it, and readFrames continues on the same
  // reader afterwards.
  const uniReader = wt.incomingUnidirectionalStreams.getReader();

  // Each step reports before it is awaited, so the last 'wt-stage' record in
  // the trace names the step that stalled.
  let stage = 'construct';
  const step = async (name, promise) => {
    stage = name;
    report({ event: 'wt-stage', stage: name });
    return await promise();
  };

  let expired = false;
  let timer = 0;
  const deadline = new Promise((_, reject) => {
    timer = setTimeout(() => {
      expired = true;
      reject(new Error(`webtransport handshake stalled at ${stage}`));
    }, WT_HANDSHAKE_TIMEOUT_MS);
  });

  let control, writer;
  try {
    await Promise.race([deadline, (async () => {
      await step('ready', () => wt.ready);

      // Probe before the control stream, not after: if the control stream is
      // what stalls, this is the only chance to learn anything about the
      // session before it is abandoned.
      probeStreams(wt, uniReader);

      // Control is one bidirectional stream carrying length-prefixed messages,
      // the same envelope the WebSocket transport sends.
      control = await step('bidi', () => wt.createBidirectionalStream());
      writer = control.writable.getWriter();

      // Announce the control stream immediately. QUIC opens streams lazily, so
      // until something is written the server has no idea this stream exists
      // and will sit waiting to accept it.
      const hello = new Uint8Array(5);
      new DataView(hello.buffer).setUint32(0, 1);
      hello[4] = 0x15; // MsgHello
      await step('hello', () => writer.write(hello));
    })()]);
  } catch (err) {
    // Cost: every handshake failure with wt.closed still pending (the common
    // unreachable-network case) waits the full 150ms before falling back.
    // A server that ended the target closes the session with a code and a
    // reason, and that lands here as a failed step. Give it a moment to
    // resolve so the caller can tell it from a network that never answered.
    const info = await Promise.race([
      wt.closed.then((i) => i, () => null),
      new Promise((r) => setTimeout(() => r(null), 150)),
    ]);
    if (info && typeof info.closeCode === 'number') {
      err.closeInfo = { code: info.closeCode, reason: info.reason || '' };
    }
    // Close the session on the way out. Without this a handshake that is
    // merely slow rather than dead completes after the fallback is already
    // streaming, and the server holds a second viewer open.
    try { wt.close(); } catch (_) {}
    report({ event: 'wt-failed', stage, expired, message: String(err && err.message || err) });
    throw err;
  } finally {
    clearTimeout(timer);
  }
  report({ event: 'wt-stage', stage: 'open' });

  let closed = false;
  // received counts frame bytes, so app.js can recognise the server's
  // backstop close when its code never arrives (rotate.js closedExhausted).
  let received = 0;
  const markClosed = (info) => {
    if (closed) return;
    closed = true;
    const out = info ? { ...info } : {};
    out.received = received;
    onClose && onClose(out);
  };
  // closed resolves with the server's code and reason. Code 2 is the server
  // closing a connection whose flow-control window Safari will not refill
  // (peerwindow.go); app.js reconnects at once for it.
  const closeInfo = (i) => (i && typeof i.closeCode === 'number'
    ? { code: i.closeCode, reason: i.reason || '' } : undefined);
  wt.closed.then((i) => markClosed(closeInfo(i)), () => markClosed());

  // A reader that fails usually does so because the session closed, and its
  // close info is the one thing that says why. Wait for it, briefly.
  const readerFailed = () => {
    wt.closed.then((i) => markClosed(closeInfo(i)), () => markClosed());
    setTimeout(() => markClosed(), 500);
  };
  readControl(control.readable, onControl).catch(readerFailed);
  readFrames(uniReader, (f) => { received += f.bytes; onFrame(f); }).catch(readerFailed);

  onOpen && onOpen();

  return {
    name: 'webtransport',
    send(bytes) {
      if (closed) return;
      const body = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes);
      const framed = new Uint8Array(4 + body.length);
      new DataView(framed.buffer).setUint32(0, body.length);
      framed.set(body, 4);
      writer.write(framed).catch(() => {});
    },
    get open() { return !closed; },
    close() { try { wt.close(); } catch (_) {} },
  };
}

// readControl reassembles length-prefixed messages from the control stream.
async function readControl(readable, onControl) {
  const reader = readable.getReader();
  let buf = new Uint8Array(0);

  for (;;) {
    const { value, done } = await reader.read();
    if (done) return;
    if (value && value.length) {
      const merged = new Uint8Array(buf.length + value.length);
      merged.set(buf, 0);
      merged.set(value, buf.length);
      buf = merged;
    }
    // A single read can carry several messages, or part of one.
    for (;;) {
      if (buf.length < 4) break;
      const n = new DataView(buf.buffer, buf.byteOffset, buf.byteLength).getUint32(0);
      if (buf.length < 4 + n) break;
      onControl(buf.subarray(4, 4 + n));
      buf = buf.subarray(4 + n);
    }
  }
}

// readFrames consumes one unidirectional stream per access unit.
//
// Each stream is drained in its own task rather than in sequence. Serializing
// them would reintroduce exactly the head-of-line blocking this transport
// exists to avoid.
async function readFrames(streams, onFrame) {
  for (;;) {
    const { value: stream, done } = await streams.read();
    if (done) return;
    drainFrame(stream, onFrame).catch(() => {
      // A reset stream is the server abandoning a stale frame on purpose.
      // Nothing to recover: the next frame arrives on its own stream.
    });
  }
}

async function drainFrame(stream, onFrame) {
  const reader = stream.getReader();
  const chunks = [];
  let total = 0;
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    if (value && value.length) {
      chunks.push(value);
      total += value.length;
    }
  }
  if (total < WT_FRAME_HEADER_LEN) return;

  const buf = new Uint8Array(total);
  let at = 0;
  for (const c of chunks) { buf.set(c, at); at += c.length; }

  const view = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  const flags = buf[12];
  onFrame({
    ptsMicros: Number(view.getBigUint64(4)),
    key: (flags & FLAG_KEY) !== 0,
    config: (flags & FLAG_CONFIG) !== 0,
    payload: buf.subarray(WT_FRAME_HEADER_LEN),
    bytes: total,
  });
}

// tokenParam returns "&token=..." when the page holds a loupe token, or "".
export function tokenParam() {
  const m = document.cookie.match(/(?:^|;\s*)loupe_token=([^;]+)/);
  return m ? `&token=${encodeURIComponent(m[1])}` : '';
}
