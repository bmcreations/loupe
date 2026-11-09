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

const { tokenParam, FrameReorder, readFrames } = await import('./transport.js');

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

for (const [name, fn] of asyncTests) {
  try { await fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

console.log(failures === 0 ? '\nALL PASS' : `\n${failures} FAILED`);
process.exit(failures === 0 ? 0 : 1);
