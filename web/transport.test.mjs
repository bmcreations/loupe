// Tests for the client's token handling. Run: node web/transport.test.mjs
//
// This exists because the failure it guards is invisible. WebTransport sends
// no cookies, so the token has to be put on the /wt URL by hand; if that
// lookup returns nothing, the connect is rejected before any handler runs and
// the client falls back to WebSocket without reporting why. That is trap 11's
// shape, and the whole point of cmd/wtprobe was not having to guess at it.
import assert from 'node:assert/strict';

// transport.js reads document.cookie at call time, not at module scope, so a
// stub is enough to import it under node.
globalThis.document = { cookie: '' };

const { tokenParam, FrameReorder, readFrames, encodeFrameAck, connect } = await import('./transport.js');

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

test('reads the token from a lone cookie', () => {
  document.cookie = 'loupe_token=abc123';
  assert.equal(tokenParam(), '&token=abc123');
});

test('finds it among other cookies', () => {
  document.cookie = 'other=1; loupe_token=abc123; more=2';
  assert.equal(tokenParam(), '&token=abc123');
});

test('stops at the delimiter rather than swallowing the rest', () => {
  document.cookie = 'loupe_token=abc123; x=1';
  assert.equal(tokenParam(), '&token=abc123');
});

test('returns nothing when auth is off', () => {
  // -no-auth sets no cookie, and appending a bare &token= would make the
  // server reject a request it would otherwise have allowed.
  document.cookie = '';
  assert.equal(tokenParam(), '');
});

test('does not match a cookie that merely ends in the name', () => {
  document.cookie = 'xloupe_token=nope';
  assert.equal(tokenParam(), '');
});

test('does not match a cookie that merely contains the name', () => {
  document.cookie = 'not_loupe_token=nope';
  assert.equal(tokenParam(), '');
});

test('escapes what it puts in the query string', () => {
  document.cookie = 'loupe_token=a+b/c';
  assert.equal(tokenParam(), '&token=a%2Bb%2Fc');
});

// FrameReorder and readFrames. Each WebTransport frame arrives on its own
// stream, so frames finish in any order. These pin the order the decoder sees.

const fr = (id, kind = 'delta') => ({ id, key: kind === 'key', config: kind === 'config' });
const ids = (frames) => frames.map((f) => f.id);

test('passes frames that arrive in order straight through', () => {
  const r = new FrameReorder();
  assert.deepEqual(ids(r.accept(fr(1, 'config'))), [1]);
  assert.deepEqual(ids(r.accept(fr(2, 'key'))), [2]);
  assert.deepEqual(ids(r.accept(fr(3))), [3]);
  assert.equal(r.hasGap, false);
});

test('holds a delta that overtakes the keyframe sent before it', () => {
  const r = new FrameReorder();
  r.accept(fr(1, 'config'));
  assert.deepEqual(ids(r.accept(fr(3))), []);
  assert.deepEqual(ids(r.accept(fr(4))), []);
  assert.equal(r.hasGap, true);
  assert.deepEqual(ids(r.accept(fr(2, 'key'))), [2, 3, 4]);
  assert.equal(r.hasGap, false);
});

test('drops a frame whose turn has passed', () => {
  const r = new FrameReorder();
  r.accept(fr(1, 'key'));
  r.accept(fr(3));
  r.skipGap();
  assert.deepEqual(ids(r.accept(fr(2))), []);
  assert.equal(r.late, 1);
});

test('after a reset, resumes at the next keyframe and skips the deltas before it', () => {
  const r = new FrameReorder();
  r.accept(fr(1, 'key'));
  r.reset(); // frame 2 was abandoned
  // Deltas the server sent before it noticed: they predict from frame 2.
  assert.deepEqual(ids(r.accept(fr(3))), []);
  assert.deepEqual(ids(r.accept(fr(5, 'key'))), [5]);
  assert.equal(r.resyncs, 1);
  assert.equal(r.hasGap, false);
  assert.deepEqual(ids(r.accept(fr(6))), [6]);
});

test('without a reset, a keyframe waits behind the gap', () => {
  // A missing frame with no reset is still in flight; jumping to the
  // keyframe would make it late for nothing.
  const r = new FrameReorder();
  r.accept(fr(1, 'key'));
  assert.deepEqual(ids(r.accept(fr(3, 'key'))), []);
  assert.deepEqual(ids(r.accept(fr(2))), [2, 3]);
});

test('counts what it did for the client report', () => {
  const r = new FrameReorder();
  r.accept(fr(1, 'key'));
  r.accept(fr(3));
  r.accept(fr(2));
  r.reset();
  r.accept(fr(5));
  r.accept(fr(6, 'key'));
  r.accept(fr(8));
  r.skipGap();
  assert.deepEqual(r.stats(), { reordered: 3, resets: 1, resyncs: 1, gapSkips: 1, late: 0, maxHeld: 1 });
  assert.equal(r.stats().maxHeld, 0);
});

test('gives up on a gap once too much is held behind it', () => {
  const r = new FrameReorder({ maxPending: 3 });
  r.accept(fr(1, 'key'));
  r.accept(fr(3));
  r.accept(fr(4));
  r.accept(fr(5));
  assert.deepEqual(ids(r.accept(fr(6))), [3, 4, 5, 6]);
});

// Fake WebTransport streams. A stream's chunks are released when the test
// says so, which is how a later frame is made to finish first.
function header(id, flags) {
  const b = new Uint8Array(13 + 2);
  const v = new DataView(b.buffer);
  v.setUint32(0, id);
  v.setBigUint64(4, BigInt(id * 1000));
  b[12] = flags;
  return b;
}
function fakeStream() {
  let settle;
  const p = new Promise((res, rej) => { settle = { res, rej }; });
  let sent = false;
  return {
    finish(bytes) { settle.res(bytes); },
    fail() { settle.rej(new Error('reset')); },
    getReader() {
      return {
        async read() {
          if (sent) return { done: true };
          const value = await p;
          sent = true;
          return { value, done: false };
        },
      };
    },
  };
}
function fakeStreams(list) {
  let i = 0;
  return { async read() { return i < list.length ? { value: list[i++], done: false } : { done: true }; } };
}
const tick = () => new Promise((r) => setTimeout(r, 0));

const asyncTests = [];
function atest(name, fn) { asyncTests.push([name, fn]); }

atest('readFrames hands the caller frames in id order, not finish order', async () => {
  const s = [fakeStream(), fakeStream(), fakeStream()];
  const got = [];
  readFrames(fakeStreams(s), (f) => got.push(f.id), () => false, 10_000);
  await tick();
  s[2].finish(header(3, 0));
  s[1].finish(header(2, 0));
  await tick();
  assert.deepEqual(got, []);
  s[0].finish(header(1, 1));
  await tick();
  assert.deepEqual(got, [1, 2, 3]);
});

atest('readFrames skips to the keyframe after a reset stream', async () => {
  const s = [fakeStream(), fakeStream(), fakeStream(), fakeStream()];
  const got = [];
  readFrames(fakeStreams(s), (f) => got.push(f.id), () => false, 10_000);
  await tick();
  s[0].finish(header(1, 1));
  s[1].fail();
  s[2].finish(header(3, 0));
  await tick();
  assert.deepEqual(got, [1]);
  s[3].finish(header(4, 1));
  await tick();
  assert.deepEqual(got, [1, 4]);
});

atest('readFrames gives up on a gap after the wait', async () => {
  const s = [fakeStream(), fakeStream(), fakeStream()];
  const got = [];
  readFrames(fakeStreams(s), (f) => got.push(f.id), () => false, 20);
  await tick();
  s[0].finish(header(1, 1));
  s[2].finish(header(3, 0));
  await tick();
  assert.deepEqual(got, [1]);
  await new Promise((r) => setTimeout(r, 40));
  assert.deepEqual(got, [1, 3]);
  s[1].finish(header(2, 0)); // let the fake settle so nothing is left pending
});

atest('readFrames ignores a stream too short to hold a frame', async () => {
  const s = [fakeStream(), fakeStream()];
  const got = [];
  readFrames(fakeStreams(s), (f) => got.push(f.id), () => false, 10_000);
  await tick();
  s[0].finish(new Uint8Array(4));
  s[1].finish(header(1, 1));
  await tick();
  assert.deepEqual(got, [1]);
});

// --- frame acks -----------------------------------------------------------
const ACK_PTS = 0x0102030405060708n;
const ACK_BYTES = [0x1A, 1, 2, 3, 4, 5, 6, 7, 8];

atest('encodeFrameAck is the type byte and the pts as u64 big-endian', async () => {
  assert.deepEqual([...encodeFrameAck(ACK_PTS)], ACK_BYTES);
  assert.deepEqual([...encodeFrameAck(258)], [0x1A, 0, 0, 0, 0, 0, 0, 1, 2]);
});

atest('sendFrameAck on the WebSocket sends the encoded ack', async () => {
  const sent = [];
  class FakeWS {
    static OPEN = 1;
    constructor() { this.readyState = 1; }
    send(b) { sent.push([...new Uint8Array(b)]); }
    close() {}
  }
  globalThis.WebSocket = FakeWS;
  globalThis.location = { protocol: 'https:', host: 'h', search: '' };
  const conn = await connect({ target: 't', preferWebTransport: false, onControl() {}, onFrame() {} });
  conn.sendFrameAck(ACK_PTS);
  assert.deepEqual(sent, [ACK_BYTES]);
});

atest('sendFrameAck on WebTransport writes it, length-prefixed, to the control stream', async () => {
  const written = [];
  const never = new Promise(() => {});
  const stream = () => ({ getReader: () => ({ read: () => never }) });
  class FakeWT {
    constructor() {
      this.ready = Promise.resolve();
      this.closed = never;
      this.incomingUnidirectionalStreams = stream();
      this.incomingBidirectionalStreams = stream();
    }
    createBidirectionalStream() {
      return Promise.resolve({
        readable: stream(),
        writable: { getWriter: () => ({ write: (b) => { written.push([...b]); return Promise.resolve(); } }) },
      });
    }
    close() {}
  }
  globalThis.WebTransport = FakeWT;
  globalThis.location = { protocol: 'https:', host: 'h', search: '' };
  const conn = await connect({ target: 't', preferWebTransport: true, onControl() {}, onFrame() {}, onOpen() {} });
  assert.equal(conn.name, 'webtransport');
  conn.sendFrameAck(ACK_PTS);
  assert.deepEqual(written.at(-1), [0, 0, 0, 9, ...ACK_BYTES]);
  delete globalThis.WebTransport;
});

atest('the pts a frame arrives with is the pts the ack carries, on /ws and /wt', async () => {
  // /ws: type, flags, pts u64 at 2, sentAt u64 at 10, payload from 18.
  const ws = new Uint8Array(18 + 1);
  ws[0] = 0x02;
  new DataView(ws.buffer).setBigUint64(2, 123456789n);
  let got;
  class FakeWS {
    static OPEN = 1;
    constructor() { this.readyState = 1; setTimeout(() => this.onmessage({ data: ws.buffer }), 0); }
    send() {}
    close() {}
  }
  globalThis.WebSocket = FakeWS;
  globalThis.location = { protocol: 'https:', host: 'h', search: '' };
  await connect({ target: 't', preferWebTransport: false, onControl() {}, onFrame: (f) => { got = f; } });
  await tick();
  assert.deepEqual([...encodeFrameAck(got.ptsMicros)].slice(1), [0, 0, 0, 0, 0x07, 0x5B, 0xCD, 0x15]);
  // /wt: id u32, pts u64 at 4, flags at 12 (header() above).
  const wtFrame = await readOne(header(1, 1));
  assert.equal(wtFrame.ptsMicros, 1000);
  assert.deepEqual([...encodeFrameAck(wtFrame.ptsMicros)].slice(1), [0, 0, 0, 0, 0, 0, 0x03, 0xE8]);
});

async function readOne(bytes) {
  const s = fakeStream();
  let f;
  readFrames(fakeStreams([s]), (x) => { f = x; }, () => false, 10_000, new FrameReorder({ maxPending: 10 }));
  await tick();
  s.finish(bytes);
  await tick();
  return f;
}

for (const [name, fn] of asyncTests) {
  try { await fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

console.log(failures === 0 ? '\nALL PASS' : `\n${failures} FAILED`);
process.exit(failures === 0 ? 0 : 1);
