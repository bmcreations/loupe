// Tests for zoom mode's pure helpers and its mount(). Run: node web/zoom.test.mjs
import assert from 'node:assert/strict';
import { installFakeDom } from './fakedom.js';

installFakeDom();
const {
  clampScale, clampPan, zoomAbout, panBy, toggleAbout, toDevice, badgeText, FIT, MAX_SCALE, mount,
} = await import('./zoom.js');

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}
const near = (a, b, eps = 1e-9) => assert.ok(Math.abs(a - b) < eps, `${a} != ${b}`);

const W = 400, H = 800;

test('clampScale holds 1..5 and reads junk as fit', () => {
  assert.equal(clampScale(0.2), 1);
  assert.equal(clampScale(3), 3);
  assert.equal(clampScale(9), MAX_SCALE);
  assert.equal(clampScale(NaN), 1);
  assert.equal(clampScale(undefined), 1);
});

test('badgeText is a whole-number percentage', () => {
  assert.equal(badgeText(1), '100%');
  assert.equal(badgeText(2), '200%');
  assert.equal(badgeText(1.256), '126%');
  assert.equal(badgeText(5), '500%');
});

test('clampPan keeps the picture covering the viewport', () => {
  assert.deepEqual(clampPan({ scale: 2, x: 50, y: 50 }, W, H), { scale: 2, x: 0, y: 0 });
  assert.deepEqual(clampPan({ scale: 2, x: -999, y: -999 }, W, H), { scale: 2, x: -400, y: -800 });
  assert.deepEqual(clampPan({ scale: 1, x: 30, y: -30 }, W, H), { scale: 1, x: 0, y: 0 });
});

test('zoomAbout keeps the point under the fingers fixed', () => {
  const s = zoomAbout(FIT, 2, 300, 600, W, H);
  assert.equal(s.scale, 2);
  // The content point that was under (300,600) at fit is still there.
  const c = toDevice(s, 300, 600);
  near(c.x, 300); near(c.y, 600);
});

test('zoomAbout at a corner clamps so the picture still covers', () => {
  const s = zoomAbout(FIT, 2, 0, 0, W, H);
  assert.deepEqual(s, { scale: 2, x: 0, y: 0 });
  const e = zoomAbout(FIT, 2, W, H, W, H);
  assert.deepEqual(e, { scale: 2, x: -400, y: -800 });
});

test('zoomAbout clamps the scale and returns to fit at 1', () => {
  assert.equal(zoomAbout(FIT, 50, 100, 100, W, H).scale, 5);
  const back = zoomAbout({ scale: 3, x: -300, y: -500 }, 1, 100, 100, W, H);
  assert.deepEqual(back, FIT);
});

test('zoomAbout from an already-zoomed state keeps the anchor', () => {
  const a = zoomAbout(FIT, 2, 200, 400, W, H);
  const b = zoomAbout(a, 3, 250, 300, W, H);
  const before = toDevice(a, 250, 300);
  const after = toDevice(b, 250, 300);
  near(before.x, after.x); near(before.y, after.y);
});

test('toDevice maps a viewport point back through the transform', () => {
  const s = { scale: 2, x: -100, y: -200 };
  assert.deepEqual(toDevice(s, 0, 0), { x: 50, y: 100 });
  assert.deepEqual(toDevice(s, 100, 200), { x: 100, y: 200 });
  assert.deepEqual(toDevice(FIT, 12, 34), { x: 12, y: 34 });
});

test('panBy moves the picture and stops at its edges', () => {
  const z = zoomAbout(FIT, 2, 200, 400, W, H); // x -200, y -400
  assert.deepEqual(panBy(z, 50, -30, W, H), { scale: 2, x: -150, y: -430 });
  assert.deepEqual(panBy(z, 500, 500, W, H), { scale: 2, x: 0, y: 0 });
  assert.deepEqual(panBy(FIT, 40, 40, W, H), FIT);
});

test('toggleAbout goes from fit to 2x about the point, and back to fit', () => {
  const z = toggleAbout(FIT, 100, 100, W, H);
  assert.equal(z.scale, 2);
  assert.deepEqual(toggleAbout(z, 100, 100, W, H), FIT);
});

// --- mount -----------------------------------------------------------------

function build() {
  const doc = document;
  const root = doc.createElement('html');
  const stage = doc.createElement('div');
  const wrap = doc.createElement('div');
  const canvas = doc.createElement('canvas');
  canvas.width = 720; canvas.height = 1560;
  stage.appendChild(wrap);
  const pane = { id: 1, targetId: 'a', els: { wrap, canvas } };
  const calls = { relayout: 0, change: [] };
  let focused = pane;
  const z = mount({
    doc, root, stage,
    pane: () => focused,
    relayout: () => { calls.relayout++; },
    onChange: (on) => calls.change.push(on),
  });
  return { z, root, stage, wrap, pane, calls, setFocused: (p) => { focused = p; } };
}

test('enter marks the page and the pane zoomed, shows a 100% badge, and relays out', () => {
  const { z, root, stage, wrap, calls } = build();
  assert.equal(z.active, false);
  z.enter();
  assert.equal(z.active, true);
  assert.ok(root.classList.contains('zoomed'));
  assert.ok(wrap.classList.contains('zoomed'));
  const badge = stage.querySelector('.zoomBadge');
  assert.ok(badge);
  assert.equal(badge.textContent, '100%');
  assert.equal(calls.relayout, 1);
  assert.deepEqual(calls.change, [true]);
});

test('tapping the badge leaves zoom mode and clears the transform', () => {
  const { z, root, stage, wrap } = build();
  z.enter();
  stage.querySelector('.zoomBadge').dispatch('click');
  assert.equal(z.active, false);
  assert.equal(root.classList.contains('zoomed'), false);
  assert.equal(wrap.classList.contains('zoomed'), false);
  assert.equal(wrap.style.transform, '');
  assert.equal(stage.querySelector('.zoomBadge'), null);
});

test('sync exits when focus moves to another pane, the canvas reshapes, or the target changes', () => {
  for (const change of [
    (b) => b.setFocused({ id: 2, targetId: 'b', els: b.pane.els }),
    (b) => { b.pane.els.canvas.width = 1560; },
    (b) => { b.pane.targetId = 'other'; },
  ]) {
    const b = build();
    b.z.enter();
    b.z.sync();
    assert.equal(b.z.active, true, 'unchanged pane stays zoomed');
    change(b);
    b.z.sync();
    assert.equal(b.z.active, false);
  }
});

test('exit when not active is a no-op', () => {
  const { z, calls } = build();
  z.exit();
  assert.deepEqual(calls.change, []);
});

test('enter with no pane does nothing', () => {
  const doc = document;
  const z = mount({ doc, root: doc.createElement('html'), stage: doc.createElement('div'), pane: () => null, relayout() {}, onChange() {} });
  z.enter();
  assert.equal(z.active, false);
});

console.log(failures ? `\n${failures} failing` : '\nall passing');
process.exit(failures ? 1 : 0);
