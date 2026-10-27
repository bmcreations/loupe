// Tests for doubletap.js. Run: node web/doubletap.test.mjs
//
// The detector is pure and driven with explicit times. The mount tests use
// fakedom elements and dispatch pointer events at them, so a test decides
// what the event target was and when it happened.
import assert from 'node:assert/strict';
import { FakeElement, installFakeDom } from './fakedom.js';

installFakeDom();
const { GAP_MS, SLOP_PX, createDetector, mount } = await import('./doubletap.js');

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

function setup(immersive = false, opts = {}) {
  const stage = new FakeElement('main');
  const wrap = new FakeElement('div');
  const canvas = new FakeElement('canvas');
  const touchLayer = new FakeElement('div');
  const corner = new FakeElement('button');
  stage.appendChild(wrap);
  wrap.appendChild(canvas);
  wrap.appendChild(touchLayer);
  stage.appendChild(corner);
  let t = 1000;
  const state = { on: immersive, calls: [] };
  mount(stage, {
    ...(opts.isBezel ? { isBezel: opts.isBezel } : { wrap }),
    isImmersive: () => state.on,
    enter: (via) => { state.on = true; state.calls.push(['enter', via]); },
    exit: (via) => { state.on = false; state.calls.push(['exit', via]); },
    now: () => t,
  });
  const ptr = (type, target, o = {}) =>
    stage.dispatch(type, { target, pointerId: 1, pointerType: 'touch', clientX: 100, clientY: 100, isPrimary: true, ...o });
  const tap = (target, o = {}, hold = 40) => {
    ptr('pointerdown', target, o);
    t += hold;
    ptr('pointerup', target, o);
  };
  const wait = (ms) => { t += ms; };
  return { stage, wrap, canvas, touchLayer, corner, state, ptr, tap, wait };
}

test('detector: two close taps are a double, a third starts over', () => {
  const d = createDetector();
  assert.equal(d.tap({ t: 0, x: 0, y: 0, type: 'touch', zone: 'a' }), false);
  assert.equal(d.tap({ t: 100, x: 5, y: 5, type: 'touch', zone: 'a' }), true);
  assert.equal(d.tap({ t: 150, x: 5, y: 5, type: 'touch', zone: 'a' }), false);
});

test('detector: slow, far, other-type and other-zone pairs are not doubles', () => {
  const mk = (b) => {
    const d = createDetector();
    d.tap({ t: 0, x: 0, y: 0, type: 'touch', zone: 'a' });
    return d.tap({ t: 100, x: 0, y: 0, type: 'touch', zone: 'a', ...b });
  };
  assert.equal(mk({ t: GAP_MS + 1 }), false);
  assert.equal(mk({ x: SLOP_PX + 1 }), false);
  assert.equal(mk({ type: 'mouse' }), false);
  assert.equal(mk({ zone: 'b' }), false);
});

test('enters from the bezel (wrap)', () => {
  const s = setup();
  s.tap(s.wrap); s.wait(100); s.tap(s.wrap);
  assert.deepEqual(s.state.calls, [['enter', 'double-tap']]);
});

test('enters from the blank stage', () => {
  const s = setup();
  s.tap(s.stage); s.wait(100); s.tap(s.stage);
  assert.deepEqual(s.state.calls, [['enter', 'double-tap']]);
});

test('exits from the blank stage in immersive', () => {
  const s = setup(true);
  s.tap(s.stage); s.wait(100); s.tap(s.stage);
  assert.deepEqual(s.state.calls, [['exit', 'double-tap']]);
});

test('canvas double-tap does nothing, in or out of immersive', () => {
  for (const on of [false, true]) {
    const s = setup(on);
    s.tap(s.canvas); s.wait(100); s.tap(s.canvas);
    assert.deepEqual(s.state.calls, []);
  }
});

test('canvas events are never preventDefaulted', () => {
  const s = setup();
  let prevented = 0;
  const o = { preventDefault: () => prevented++ };
  s.tap(s.canvas, o); s.wait(100); s.tap(s.canvas, o);
  s.tap(s.stage, o); s.wait(100); s.tap(s.stage, o);
  assert.equal(prevented, 0);
});

test('overlays and controls are ignored', () => {
  const s = setup();
  for (const el of [s.touchLayer, s.corner]) {
    s.tap(el); s.wait(100); s.tap(el);
  }
  assert.deepEqual(s.state.calls, []);
});

test('a bezel tap then a canvas tap is not a double', () => {
  const s = setup();
  s.tap(s.wrap); s.wait(100); s.tap(s.canvas);
  assert.deepEqual(s.state.calls, []);
});

test('slow taps are ignored', () => {
  const s = setup();
  s.tap(s.stage); s.wait(GAP_MS + 50); s.tap(s.stage);
  assert.deepEqual(s.state.calls, []);
});

test('far-apart taps are ignored', () => {
  const s = setup();
  s.tap(s.stage); s.wait(100); s.tap(s.stage, { clientX: 100 + SLOP_PX + 10 });
  assert.deepEqual(s.state.calls, []);
});

test('a drag is not a tap', () => {
  const s = setup();
  s.tap(s.stage); s.wait(100);
  s.ptr('pointerdown', s.stage);
  s.ptr('pointermove', s.stage, { clientX: 140 });
  s.wait(40);
  s.ptr('pointerup', s.stage, { clientX: 140 });
  assert.deepEqual(s.state.calls, []);
});

test('a drag between two taps resets the first', () => {
  const s = setup();
  s.tap(s.stage); s.wait(50);
  s.ptr('pointerdown', s.stage);
  s.ptr('pointermove', s.stage, { clientX: 140 });
  s.ptr('pointerup', s.stage, { clientX: 140 });
  s.wait(50); s.tap(s.stage);
  assert.deepEqual(s.state.calls, []);
});

test('a multi-touch is ignored', () => {
  const s = setup();
  s.tap(s.stage); s.wait(100);
  s.ptr('pointerdown', s.stage, { pointerId: 1 });
  s.ptr('pointerdown', s.stage, { pointerId: 2, isPrimary: false });
  s.wait(40);
  s.ptr('pointerup', s.stage, { pointerId: 2, isPrimary: false });
  s.ptr('pointerup', s.stage, { pointerId: 1 });
  assert.deepEqual(s.state.calls, []);
});

test('isBezel takes any pane\'s bezel in place of one wrap', () => {
  const other = new FakeElement('div');
  const { state, tap, wait } = setup(false, { isBezel: (t) => t === other });
  tap(other); wait(100); tap(other);
  assert.deepEqual(state.calls, [['enter', 'double-tap']]);
});

test('a long press is not a tap', () => {
  const s = setup();
  s.tap(s.stage, {}, 40); s.wait(50); s.tap(s.stage, {}, 600);
  assert.deepEqual(s.state.calls, []);
});

test('pointercancel drops the tap', () => {
  const s = setup();
  s.tap(s.stage); s.wait(50);
  s.ptr('pointerdown', s.stage);
  s.ptr('pointercancel', s.stage);
  s.wait(40);
  s.ptr('pointerup', s.stage);
  assert.deepEqual(s.state.calls, []);
});

test('after a double, the next two taps make another', () => {
  const s = setup();
  s.tap(s.stage); s.wait(100); s.tap(s.stage);
  s.wait(1000);
  s.tap(s.stage); s.wait(100); s.tap(s.stage);
  assert.deepEqual(s.state.calls, [['enter', 'double-tap'], ['exit', 'double-tap']]);
});

if (failures) { console.log(`${failures} failed`); process.exit(1); }
console.log('\nall passing');
