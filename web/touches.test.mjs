// Tests for the touch markers. Run: node web/touches.test.mjs
//
// remoteMark, clampMs and the preference are pure and pinned directly;
// mount() is driven against web/fakedom.js, with a timer and a raf the test
// fires by hand so nothing waits on the clock.
import assert from 'node:assert/strict';

const { remoteMark, clampMs, readPref, writePref, mount, LIFT_MS } = await import('./touches.js');
const { installFakeDom } = await import('./fakedom.js');

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

function memStorage(init = {}) {
  const m = new Map(Object.entries(init));
  return { getItem: (k) => (m.has(k) ? m.get(k) : null), setItem: (k, v) => m.set(k, String(v)) };
}

// --- remoteMark ----------------------------------------------------------

test('an agent tap is drawn as an agent tap', () => {
  assert.deepEqual(remoteMark({ source: 'agent', kind: 'tap', from: [0.5, 0.25] }, 3),
    { kind: 'tap', who: 'agent', from: [0.5, 0.25] });
});

test("this viewer's own echoed tap is skipped, another viewer's is not", () => {
  const ev = { source: 'viewer', viewer: 3, kind: 'tap', from: [0.1, 0.1] };
  assert.equal(remoteMark(ev, 3), null);
  assert.equal(remoteMark(ev, 4).who, 'viewer');
});

test('with no id from init, every viewer event is drawn', () => {
  // An older server sends no viewer id: drawing this phone's own tap twice
  // is the lesser fault than drawing nobody's.
  assert.ok(remoteMark({ source: 'viewer', viewer: 3, kind: 'tap', from: [0, 0] }, 0));
});

test('an event with no position or an error draws nothing', () => {
  assert.equal(remoteMark({ source: 'agent', kind: 'tap' }, 0), null);
  assert.equal(remoteMark({ source: 'agent', kind: 'tap', from: [0, 0], err: 'no device' }, 0), null);
  assert.equal(remoteMark({ source: 'agent', kind: 'swipe', from: [0, 0] }, 0), null);
});

test('keys, text and buttons draw nothing', () => {
  for (const kind of ['key', 'text', 'button', 'rotate', 'scroll']) {
    assert.equal(remoteMark({ source: 'agent', kind, from: [0, 0] }, 0), null, kind);
  }
});

test('swipe, hold and pinch carry a clamped duration', () => {
  assert.equal(remoteMark({ source: 'agent', kind: 'swipe', from: [0, 0], to: [1, 1], ms: 5000 }, 0).ms, 900);
  assert.equal(remoteMark({ source: 'agent', kind: 'longPress', from: [0, 0] }, 0).ms, 300);
  const p = remoteMark({ source: 'agent', kind: 'pinch', from: [0.5, 0.5], scale: 2, ms: 400 }, 0);
  assert.deepEqual([p.kind, p.scale, p.ms], ['pinch', 2, 400]);
  assert.equal(remoteMark({ source: 'agent', kind: 'pinch', from: [0.5, 0.5] }, 0), null, 'no scale');
});

test('clampMs falls back to the floor for junk', () => {
  assert.equal(clampMs(undefined, 100, 200), 100);
  assert.equal(clampMs(-5, 100, 200), 100);
  assert.equal(clampMs(150, 100, 200), 150);
});

// --- preference ----------------------------------------------------------

test('the preference is off unless stored on, and survives no storage', () => {
  assert.equal(readPref(memStorage()), false);
  const s = memStorage();
  writePref(s, true);
  assert.equal(readPref(s), true);
  assert.equal(readPref(null), false);
  writePref(null, true); // must not throw
});

// --- mount() -------------------------------------------------------------

function setup(on = true) {
  installFakeDom();
  const doc = globalThis.document;
  const layer = doc.createElement('div');
  const canvas = doc.createElement('canvas');
  // The layer covers a bezel; the canvas sits 50,40 inside it at 400x800.
  layer._rect = { left: 10, top: 20, width: 500, height: 880 };
  canvas._rect = { left: 60, top: 60, width: 400, height: 800 };
  const timers = [];
  const frames = [];
  const reports = [];
  const api = mount(layer, {
    canvas,
    storage: memStorage(on ? { 'loupe.showTouches': '1' } : {}),
    report: (r) => reports.push(r),
    setTimeout: (fn, ms) => timers.push({ fn, ms }),
    raf: (fn) => frames.push(fn),
  });
  const runTimers = () => { while (timers.length) timers.shift().fn(); };
  const runFrames = () => { while (frames.length) frames.shift()(); };
  return { layer, canvas, api, timers, frames, reports, runTimers, runFrames };
}

const translate = (d) => d.style.transform;

test('off by default: the layer is hidden and a finger draws nothing', () => {
  const { layer, canvas } = setup(false);
  assert.equal(layer.hidden, true);
  canvas.dispatch('pointerdown', { pointerId: 1, clientX: 100, clientY: 100 });
  assert.equal(layer.children.length, 0);
});

test("a finger's dot follows it and fades out on lift", () => {
  const { layer, canvas, timers, runTimers } = setup();
  assert.equal(layer.hidden, false);
  canvas.dispatch('pointerdown', { pointerId: 1, clientX: 110, clientY: 120 });
  assert.equal(layer.children.length, 1);
  const d = layer.children[0];
  assert.ok(d.classList.contains('self'));
  assert.equal(translate(d), 'translate(100px, 100px)', 'client point minus the layer origin');
  canvas.dispatch('pointermove', { pointerId: 1, clientX: 210, clientY: 320 });
  assert.equal(translate(d), 'translate(200px, 300px)');
  canvas.dispatch('pointerup', { pointerId: 1 });
  timers.shift().fn();
  assert.ok(d.classList.contains('lift'));
  assert.equal(timers[0].ms, LIFT_MS);
  runTimers();
  assert.equal(layer.children.length, 0, 'removed once the fade has run');
});

test('a dot never leaves the canvas, even with a bezel around it', () => {
  const { layer, canvas } = setup();
  canvas.dispatch('pointerdown', { pointerId: 1, clientX: 0, clientY: 2000 });
  assert.equal(translate(layer.children[0]), 'translate(50px, 840px)');
});

test('two fingers get two dots, each lifting on its own', () => {
  const { layer, canvas } = setup();
  canvas.dispatch('pointerdown', { pointerId: 1, clientX: 100, clientY: 100 });
  canvas.dispatch('pointerdown', { pointerId: 2, clientX: 200, clientY: 200 });
  assert.equal(layer.children.length, 2);
  canvas.dispatch('pointerup', { pointerId: 1 });
  canvas.dispatch('pointermove', { pointerId: 2, clientX: 300, clientY: 300 });
  assert.equal(translate(layer.children[1]), 'translate(290px, 280px)');
});

test('an agent tap lands at its fraction of the canvas, not the layer', () => {
  const { layer, api } = setup();
  api.onInit({ viewer: 7 });
  api.onEvent({ source: 'agent', kind: 'tap', from: [0.5, 0.5] });
  const d = layer.children[0];
  assert.ok(d.classList.contains('agent'));
  assert.equal(translate(d), 'translate(250px, 440px)');
});

test("this viewer's own echo draws nothing once init has named it", () => {
  const { layer, api } = setup();
  api.onInit({ viewer: 7 });
  api.onEvent({ source: 'viewer', viewer: 7, kind: 'tap', from: [0.5, 0.5] });
  assert.equal(layer.children.length, 0);
  api.onEvent({ source: 'viewer', viewer: 8, kind: 'tap', from: [0.5, 0.5] });
  assert.ok(layer.children[0].classList.contains('viewer'));
});

test('a swipe travels to its end on the next frame, then lifts after its duration', () => {
  const { layer, api, timers, runFrames } = setup();
  api.onEvent({ source: 'agent', kind: 'swipe', from: [0, 0], to: [1, 1], ms: 300 });
  const d = layer.children[0];
  assert.equal(translate(d), 'translate(50px, 40px)');
  runFrames();
  assert.equal(translate(d), 'translate(450px, 840px)');
  assert.match(d.style.transition, /300ms/);
  assert.equal(timers[0].ms, 300, 'lift waits for the travel');
});

test('a pinch draws two fingers moving apart for a zoom in', () => {
  const { layer, api, runFrames } = setup();
  api.onEvent({ source: 'agent', kind: 'pinch', from: [0.5, 0.5], scale: 2, ms: 400 });
  assert.equal(layer.children.length, 2);
  const dist = () => {
    const [a, b] = layer.children.map((d) => translate(d).match(/-?\d+/g).map(Number));
    return Math.hypot(a[0] - b[0], a[1] - b[1]);
  };
  const before = dist();
  runFrames();
  assert.ok(dist() > before * 1.5, `spread ${before} -> ${dist()}`);
});

test('turning it off clears what is drawn and is reported', () => {
  const { layer, canvas, api, reports } = setup();
  canvas.dispatch('pointerdown', { pointerId: 1, clientX: 100, clientY: 100 });
  api.setPref(false);
  assert.equal(layer.hidden, true);
  assert.equal(layer.children.length, 0);
  assert.deepEqual(reports.at(-1), { event: 'touches-pref', on: false });
  api.onEvent({ source: 'agent', kind: 'tap', from: [0.5, 0.5] });
  assert.equal(layer.children.length, 0, 'events are ignored while off');
});

if (failures) {
  console.log(`\n${failures} failed`);
  process.exit(1);
}
