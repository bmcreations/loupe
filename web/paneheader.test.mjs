// Tests for paneheader.js. Run: node web/paneheader.test.mjs
import assert from 'node:assert/strict';
import { FakeElement, installFakeDom } from './fakedom.js';

installFakeDom();
const { model, mount } = await import('./paneheader.js');

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

const NOW = 10000;
function fakePane(over = {}) {
  return {
    id: 'p1',
    targetId: 'emulator-5554',
    targetLabel: 'Pixel 10',
    status: 'live',
    info: { codedWidth: 1080, codedHeight: 2424 },
    stats: () => ({ fps: 59.6, health: 'good', statsReceivedAt: NOW - 100 }),
    ...over,
  };
}

console.log('paneheader');

test('model names the index, label, dot and resolution with fps', () => {
  const m = model(fakePane(), { index: 2, focused: false, now: NOW });
  assert.equal(m.badge, '⌃2');
  assert.equal(m.label, 'Pixel 10');
  assert.equal(m.dot, 'good');
  assert.equal(m.detail, '1080×2424 · 60 fps');
  assert.equal(m.focused, false);
});

test('model says what the pane is doing before frames arrive', () => {
  const m = model(fakePane({ status: null, info: null, stats: () => ({}) }), { index: 1, focused: true, now: NOW });
  assert.equal(m.dot, '');
  assert.equal(m.detail, 'connecting');
  assert.equal(m.focused, true);
  const err = model(fakePane({ status: 'error', info: null, stats: () => ({}) }), { index: 1, now: NOW });
  assert.equal(err.detail, 'disconnected');
});

test('model falls back to the target id when the list has not named it', () => {
  assert.equal(model(fakePane({ targetLabel: '' }), { index: 1, now: NOW }).label, 'emulator-5554');
});

test('mount paints the model and close calls onClose', () => {
  const host = new FakeElement('div');
  let closed = 0;
  const h = mount(host, { onClose: () => closed++ });
  h.paint(model(fakePane(), { index: 3, focused: true, now: NOW }));
  const q = (c) => host.querySelector(c);
  assert.equal(q('.paneBadge').textContent, '⌃3');
  assert.equal(q('.paneName').textContent, 'Pixel 10');
  assert.equal(q('.paneDetail').textContent, '1080×2424 · 60 fps');
  assert.equal(q('.paneDot').className, 'paneDot dot good');
  assert.ok(host.classList.contains('focused'));
  assert.equal(q('.paneClose').getAttribute('aria-label'), 'Close Pixel 10');
  q('.paneClose').dispatch('click');
  assert.equal(closed, 1);
});

test('paint skips writes when nothing changed', () => {
  const host = new FakeElement('div');
  const h = mount(host, { onClose() {} });
  const m = model(fakePane(), { index: 1, now: NOW });
  h.paint(m);
  host.querySelector('.paneName').textContent = 'touched';
  h.paint({ ...m });
  assert.equal(host.querySelector('.paneName').textContent, 'touched');
});

if (failures) { console.log(`FAIL ${failures}`); process.exit(1); }
console.log('PASS');
