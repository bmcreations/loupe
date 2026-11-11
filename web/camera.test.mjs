// Tests for camera.js. Run: node web/camera.test.mjs
//
// Pure functions are pinned directly; the mount() loop is driven against
// web/fakedom.js with the camera source, fetch and pacing injected, so the
// getUserMedia + canvas half never has to run. The loop reacts to what the
// server says a frame did (live / staged / 503), so those are the cases that
// matter most here.
import assert from 'node:assert/strict';
import {
  offered, fitDimensions, facingName, statusText, openErrorKind, mount,
} from './camera.js';

const { installFakeDom, tick, jsonResponse } = await import('./fakedom.js');

let failures = 0;
const pending = [];
function test(name, fn) {
  const run = (async () => {
    try { await fn(); console.log(`  PASS ${name}`); }
    catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.stack || err.message}`); }
  })();
  pending.push(run);
}

// --- pure functions ---

test('offered: only an Android emulator', () => {
  assert.equal(offered({ os: 'android', kind: 'emulator' }), true);
  assert.equal(offered({ os: 'android', kind: 'device' }), false);
  assert.equal(offered({ os: 'ios', kind: 'simulator' }), false);
  assert.equal(offered(null), false);
});

test('fitDimensions: caps to the box, keeps aspect, never upscales', () => {
  assert.deepEqual(fitDimensions(1920, 1080, 1280, 720), { w: 1280, h: 720 });
  assert.deepEqual(fitDimensions(640, 480, 1280, 720), { w: 640, h: 480 });
  assert.deepEqual(fitDimensions(0, 0, 1280, 720), { w: 0, h: 0 });
});

test('facingName maps getUserMedia facing to a word', () => {
  assert.equal(facingName('user'), 'front');
  assert.equal(facingName('environment'), 'back');
});

test('statusText covers each state', () => {
  assert.match(statusText({ kind: 'live', facing: 'environment' }), /back camera/);
  assert.match(statusText({ kind: 'insecure' }), /https/);
  assert.match(statusText({ kind: 'notLive' }), /Handset Camera/);
  assert.equal(statusText({ kind: 'unavailable', detail: 'busy' }), 'busy');
});

test('openErrorKind maps DOMException names', () => {
  assert.equal(openErrorKind({ name: 'NotAllowedError' }), 'denied');
  assert.equal(openErrorKind({ name: 'NotFoundError' }), 'nocamera');
  assert.equal(openErrorKind({ name: 'AbortError' }), 'error');
});

// --- mount() loop ---

function harness(opts = {}) {
  installFakeDom();
  const host = document.createElement('div');
  const posts = [];
  let stops = 0;
  const openCamera = opts.openCamera || (async (facing) => ({
    facing,
    grab: async () => ({ jpeg: facing }),
    stop: () => { stops += 1; },
  }));
  const fetchImpl = opts.fetch || (async () => jsonResponse(200, { ok: true, live: true }));
  const m = mount({
    doc: document,
    host,
    fetch: (url, init) => { posts.push({ url, init }); return fetchImpl(url, init); },
    targetId: () => opts.target || 'emulator-5554',
    report: () => {},
    toast: document.createElement('div'),
    isSecure: () => (opts.secure === undefined ? true : opts.secure),
    openCamera,
    sleep: () => new Promise((r) => setTimeout(r, 0)),
  });
  return { m, host, posts, stops: () => stops, note: () => host.querySelector('.sheetNote').textContent };
}

test('insecure context: start refuses and explains, no capture', async () => {
  const h = harness({ secure: false });
  h.m.start();
  await tick(2);
  assert.equal(h.m.active(), false);
  assert.equal(h.posts.length, 0);
  assert.match(h.note(), /https/);
});

test('live feed: posts JPEG frames to the target and shows Live', async () => {
  const h = harness();
  h.m.start();
  await tick(3);
  assert.ok(h.posts.length >= 1, 'at least one frame posted');
  assert.match(h.posts[0].url, /\/api\/camera\?target=emulator-5554/);
  assert.equal(h.posts[0].init.method, 'POST');
  assert.equal(h.posts[0].init.headers['Content-Type'], 'image/jpeg');
  assert.match(h.note(), /Live/);
  assert.equal(h.m.active(), true);
  h.m.stop();
});

test('staged (live:false): stops after the probe frame with guidance', async () => {
  const h = harness({ fetch: async () => jsonResponse(200, { ok: true, live: false }) });
  h.m.start();
  await tick(4);
  assert.equal(h.m.active(), false);
  assert.match(h.note(), /isn.t using Handset Camera/);
  assert.ok(h.stops() >= 1, 'camera source released');
});

test('503: stops and surfaces the server message', async () => {
  const h = harness({ fetch: async () => jsonResponse(503, { error: 'Handset Camera is being fed by another process on this Mac' }) });
  h.m.start();
  await tick(4);
  assert.equal(h.m.active(), false);
  assert.match(h.note(), /another process/);
});

test('getUserMedia denied: stops with a permission message', async () => {
  const h = harness({ openCamera: async () => { const e = new Error('no'); e.name = 'NotAllowedError'; throw e; } });
  h.m.start();
  await tick(3);
  assert.equal(h.m.active(), false);
  assert.match(h.note(), /denied/i);
});

test('onTargetSwitch stops capture when the viewer moves away', async () => {
  const h = harness();
  h.m.start();
  await tick(2);
  assert.equal(h.m.active(), true);
  h.m.onTargetSwitch('emulator-9999');
  assert.equal(h.m.active(), false);
});

test('default facing is back; the selector reflects it', () => {
  const h = harness();
  const back = h.host.querySelectorAll('.camFacingBtn')[0];
  const front = h.host.querySelectorAll('.camFacingBtn')[1];
  assert.equal(back.getAttribute('aria-pressed'), 'true');
  assert.equal(front.getAttribute('aria-pressed'), 'false');
});

await Promise.all(pending);
console.log(failures ? `\n${failures} failing` : '\nall passing');
process.exit(failures ? 1 : 0);
