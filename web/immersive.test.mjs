// Tests for immersive.js. Run: node web/immersive.test.mjs
//
// The hold timer is driven through fake timers, so a test decides when the
// hold elapses instead of sleeping for it.
import assert from 'node:assert/strict';
import { FakeElement, installFakeDom } from './fakedom.js';

installFakeDom();
const { KEY, HOLD_MS, readImmersive, centreFit, mount } = await import('./immersive.js');

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

function fakeStorage(initial = {}) {
  const data = { ...initial };
  return { data, getItem: (k) => (k in data ? data[k] : null), setItem: (k, v) => { data[k] = v; } };
}

function fakeTimers() {
  let next = 1;
  const pending = new Map();
  return {
    pending,
    setTimeout: (fn, ms) => { const id = next++; pending.set(id, { fn, ms }); return id; },
    clearTimeout: (id) => { pending.delete(id); },
    fire() { for (const [id, t] of [...pending]) { pending.delete(id); t.fn(); } },
  };
}

function setup(initial) {
  const root = new FakeElement('html');
  const handle = new FakeElement('button');
  const storage = fakeStorage(initial);
  const timers = fakeTimers();
  const reports = [];
  const changes = [];
  const toast = new FakeElement('div');
  toast.hidden = true;
  const im = mount(root, handle, {
    storage, timers, toast,
    report: (r) => reports.push(r),
    onChange: (on) => changes.push(on),
  });
  const press = (type) => handle.dispatch(type, { preventDefault() {} });
  return { root, handle, storage, timers, reports, changes, toast, im, press };
}

test('off by default, and a storage that throws reads as off', () => {
  assert.equal(readImmersive(fakeStorage()), false);
  assert.equal(readImmersive({ getItem() { throw new Error('denied'); } }), false);
});

test('a stored preference is applied at mount without a report', () => {
  const s = setup({ [KEY]: '1' });
  assert.equal(s.im.isOn(), true);
  assert.equal(s.root.classList.contains('immersive'), true);
  assert.equal(s.handle.hidden, false);
  assert.deepEqual(s.reports, []);
});

test('entering sets the class, shows the handle, persists and reports', () => {
  const s = setup();
  assert.equal(s.handle.hidden, true);
  s.im.set(true);
  assert.equal(s.root.classList.contains('immersive'), true);
  assert.equal(s.handle.hidden, false);
  assert.equal(s.storage.data[KEY], '1');
  assert.deepEqual(s.reports, [{ event: 'immersive', on: true, via: 'menu' }]);
  assert.deepEqual(s.changes, [true]);
});

test('setting the state it is already in does nothing', () => {
  const s = setup();
  s.im.set(false);
  assert.deepEqual(s.reports, []);
  assert.deepEqual(s.changes, []);
});

test('a hold on the handle leaves', () => {
  const s = setup({ [KEY]: '1' });
  s.press('pointerdown');
  assert.equal([...s.timers.pending.values()][0].ms, HOLD_MS);
  s.timers.fire();
  assert.equal(s.im.isOn(), false);
  assert.equal(s.handle.hidden, true);
  assert.deepEqual(s.reports, [{ event: 'immersive', on: false, via: 'hold' }]);
  s.press('pointerup');
  assert.equal(s.toast.hidden, true, 'the release after a hold should not also show the hint');
});

test('a short tap on the handle stays and says how to leave', () => {
  const s = setup({ [KEY]: '1' });
  s.press('pointerdown');
  s.press('pointerup');
  assert.equal(s.timers.pending.size, 0);
  assert.equal(s.im.isOn(), true);
  assert.equal(s.toast.hidden, false);
  assert.match(s.toast.textContent, /Hold/);
});

test('a finger sliding off the handle cancels the hold', () => {
  const s = setup({ [KEY]: '1' });
  s.press('pointerdown');
  s.press('pointerleave');
  assert.equal(s.timers.pending.size, 0);
  assert.equal(s.im.isOn(), true);
});

test('centreFit splits the letterbox evenly on both axes', () => {
  const fit = centreFit({ width: 390, height: 763 }, { width: 496, height: 1080 });
  assert.equal(fit.height, 763);
  assert.equal(fit.width, Math.floor(496 * (763 / 1080)));
  assert.equal(fit.y, 0);
  assert.equal(fit.x, Math.round((390 - fit.width) / 2));
});

test('centreFit refuses an empty stage or box', () => {
  assert.equal(centreFit({ width: 0, height: 10 }, { width: 1, height: 1 }), null);
  assert.equal(centreFit({ width: 10, height: 10 }, null), null);
});

if (failures) {
  console.log(`\n${failures} failing`);
  process.exit(1);
}
console.log('\nall passing');
