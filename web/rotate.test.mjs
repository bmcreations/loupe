// Tests for moving a WebTransport session to a fresh connection (trap 52).
// Run: node web/rotate.test.mjs
//
// The rotator is driven with fake connections: open() hands back the
// callbacks it was given, so a test can deliver init, frames and a close on
// the standby the way transport.js would.
import assert from 'node:assert/strict';

const { createRotator, closedExhausted, EXHAUSTED_RECEIVED, MSG_INIT } = await import('./rotate.js');

let failures = 0;
const queue = [];
function test(name, fn) { queue.push([name, fn]); }

function rig({ pointer = false, openFails = false } = {}) {
  const r = {
    t: 1000, pointer, reports: [], frames: [], controls: [], closes: [],
    promoted: [], current: true, opened: null, conns: [],
  };
  r.rot = createRotator({
    open: async (cb) => {
      if (openFails) throw new Error('handshake stalled at ready');
      const conn = { closed: false, close() { this.closed = true; } };
      r.opened = cb;
      r.conns.push(conn);
      return conn;
    },
    pointerDown: () => r.pointer,
    promote: (conn, init) => {
      r.promoted.push({ conn, init: new TextDecoder().decode(init) });
      return () => r.current;
    },
    control: (buf) => r.controls.push(buf[0]),
    frame: (f) => r.frames.push(f.ptsMicros),
    closed: (info) => r.closes.push(info),
    report: (rec) => r.reports.push(rec),
    now: () => r.t,
  });
  return r;
}

const rotateBody = new TextEncoder().encode('{"bytes":12582912,"streams":1800}');
const init = () => Uint8Array.from([MSG_INIT, ...new TextEncoder().encode('{"codec":"avc1"}')]);
const frame = (pts, key = false) => ({ ptsMicros: pts, key, config: false, bytes: 10 });
const config = () => ({ ptsMicros: 0, key: false, config: true, bytes: 10 });
const events = (r) => r.reports.map((x) => x.event);

test('promotes on the standby\'s first keyframe after init', async () => {
  const r = rig();
  r.rot.noteFrame(frame(100));
  await r.rot.start(rotateBody);
  r.opened.onControl(init());
  r.opened.onFrame(frame(116));            // not a keyframe: still the old one's
  assert.equal(r.promoted.length, 0);
  r.t += 500;
  r.opened.onFrame(config());
  assert.equal(r.promoted.length, 0, 'the config arrives before the keyframe and cannot start the picture');
  r.opened.onFrame(frame(1100, true));
  assert.equal(r.promoted.length, 1);
  assert.deepEqual(r.frames, [0, 1100], 'the held config goes ahead of the keyframe');
  assert.equal(r.promoted[0].init, '{"codec":"avc1"}');
  assert.equal(r.rot.pending, false);
  assert.deepEqual(events(r), ['rotate-start', 'rotate-open', 'rotate-promote']);
  assert.equal(r.reports[0].bytes, 12582912);
});

test('a frame before init does not promote', async () => {
  const r = rig();
  await r.rot.start(rotateBody);
  r.opened.onFrame(frame(200, true));
  assert.equal(r.promoted.length, 0);
});

test('waits for no finger down, then promotes on the next keyframe', async () => {
  const r = rig({ pointer: true });
  await r.rot.start(rotateBody);
  r.opened.onControl(init());
  r.opened.onFrame(frame(200, true));
  assert.equal(r.promoted.length, 0);
  r.pointer = false;
  r.opened.onFrame(frame(216));
  assert.equal(r.promoted.length, 0, 'a delta frame cannot start the new connection');
  r.opened.onFrame(frame(1200, true));
  assert.equal(r.promoted.length, 1);
  assert.equal(r.reports.at(-1).waitedForPointer, true);
});

test('skips frames the old connection already delivered', async () => {
  const r = rig();
  r.rot.noteFrame(frame(300));
  await r.rot.start(rotateBody);
  r.opened.onControl(init());
  r.opened.onFrame(frame(250, true));      // promotes, already delivered
  r.opened.onFrame(frame(300));            // already delivered
  r.opened.onFrame(frame(316));
  r.opened.onFrame(frame(310));            // after the first new one, nothing is skipped
  assert.deepEqual(r.frames, [316, 310]);
});

test('after promotion the connection routes through app.js while current', async () => {
  const r = rig();
  await r.rot.start(rotateBody);
  r.opened.onControl(init());
  r.opened.onFrame(frame(400, true));
  r.opened.onControl(Uint8Array.of(0x03));
  r.opened.onClose({ code: 2 });
  assert.deepEqual(r.controls, [0x03]);
  assert.deepEqual(r.closes, [{ code: 2 }]);
  r.current = false;                       // replaced by a later connect
  r.opened.onFrame(frame(500));
  r.opened.onClose({});
  assert.deepEqual(r.frames, [400]);
  assert.equal(r.closes.length, 1);
});

test('a standby that closes before promotion is dropped and reported', async () => {
  const r = rig();
  await r.rot.start(rotateBody);
  r.opened.onClose({});
  assert.equal(r.rot.pending, false);
  assert.equal(r.closes.length, 0, 'the current connection did not close');
  assert.equal(r.reports.at(-1).event, 'rotate-failed');
  assert.equal(r.reports.at(-1).stage, 'closed');
});

test('a failed open is reported and leaves no standby', async () => {
  const r = rig({ openFails: true });
  await r.rot.start(rotateBody);
  assert.equal(r.rot.pending, false);
  assert.equal(r.reports.at(-1).stage, 'open');
});

test('a second MsgRotate while one is pending is ignored', async () => {
  const r = rig();
  await r.rot.start(rotateBody);
  await r.rot.start(rotateBody);
  assert.equal(r.conns.length, 1);
});

test('reset closes the standby and ignores its later callbacks', async () => {
  const r = rig();
  await r.rot.start(rotateBody);
  r.rot.reset();
  assert.equal(r.conns[0].closed, true);
  r.opened.onControl(init());
  r.opened.onFrame(frame(600, true));
  assert.equal(r.promoted.length, 0);
});

// 12 October: the server's code 2 never reached the phone, because its close
// capsule sat inside the spent window. Safari reported a close with no code
// after 14-ish MB of frames.
test('closedExhausted reads the backstop with or without its code', () => {
  assert.equal(closedExhausted({ code: 2, received: 0 }), true);
  assert.equal(closedExhausted({ received: EXHAUSTED_RECEIVED }), true);
  assert.equal(closedExhausted({ code: 0, received: EXHAUSTED_RECEIVED }), true);
  assert.equal(closedExhausted({ received: EXHAUSTED_RECEIVED - 1 }), false);
  // A server that said why, and said something else, is believed.
  assert.equal(closedExhausted({ code: 1, received: EXHAUSTED_RECEIVED }), false);
  assert.equal(closedExhausted({}), false);
  assert.equal(closedExhausted(undefined), false);
});

for (const [name, fn] of queue) {
  try { await fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}
if (failures) { console.log(`${failures} failed`); process.exit(1); }
