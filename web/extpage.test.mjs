// Tests for the extension page sheet. Run: node web/extpage.test.mjs
//
// The sheet's whole job is to put the right path in its frame, and to take
// it out again on close so the page stops running.
import assert from 'node:assert/strict';
import { installFakeDom, FakeElement } from './fakedom.js';

const reset = installFakeDom();
const { mount, framePath } = await import('./extpage.js');

let failures = 0;
function test(name, fn) {
  reset();
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

const SLIPWAY = { name: 'slipway', label: 'Slipway', page: '/api/ext/slipway/' };

function sheet(opts = {}) {
  const root = new FakeElement('aside');
  root.hidden = true;
  const keys = new FakeElement('window');
  const closed = [];
  const reports = [];
  const s = mount(root, { onClose: () => closed.push(true), report: (r) => reports.push(r), keyTarget: keys, ...opts });
  return { root, keys, closed, reports, s, title: root.querySelector('#extPageTitle') };
}
const frameOf = (t) => t.root.querySelector('iframe');

test('framePath keeps a path under /api/ext/ and refuses anything else', () => {
  assert.equal(framePath('/api/ext/slipway/'), '/api/ext/slipway/');
  for (const p of ['javascript:alert(1)', 'https://example.com/', '/api/targets', '', undefined]) {
    assert.equal(framePath(p), null, String(p));
  }
});

test('open shows the sheet with the page in its frame, titled by label', () => {
  const t = sheet();
  assert.equal(t.s.open(SLIPWAY), true);
  assert.equal(t.root.hidden, false);
  assert.equal(t.s.isOpen(), true);
  assert.equal(frameOf(t).src, '/api/ext/slipway/');
  assert.equal(frameOf(t).getAttribute('title'), 'Slipway');
  assert.equal(t.title.textContent, 'Slipway');
  assert.equal(frameOf(t).hasAttribute('sandbox'), false, 'a sandboxed frame gets origin null and auth.go refuses its cookie');
  assert.deepEqual(t.reports, [{ event: 'extpage-open', name: 'slipway' }]);
});

test('open refuses a page outside /api/ext/ and stays hidden', () => {
  const t = sheet();
  assert.equal(t.s.open({ ...SLIPWAY, page: 'javascript:alert(1)' }), false);
  assert.equal(t.root.hidden, true);
  assert.equal(t.s.isOpen(), false);
  assert.equal(frameOf(t), null, 'no frame for a refused page');
});

test('Done closes the sheet and removes the frame', () => {
  const t = sheet();
  t.s.open(SLIPWAY);
  t.root.querySelector('.pickerDone').dispatch('click');
  assert.equal(t.root.hidden, true);
  assert.equal(t.s.isOpen(), false);
  assert.equal(frameOf(t), null, 'a frame left behind keeps the page running');
  assert.equal(t.closed.length, 1);
});

test('Escape typed inside the page closes the sheet too', () => {
  const t = sheet();
  t.s.open(SLIPWAY);
  const win = new FakeElement('window');
  frameOf(t).contentWindow = win;
  frameOf(t).dispatch('load', { target: frameOf(t) });
  for (const fn of win.listeners.keydown) fn({ key: 'Escape', stopPropagation() {}, preventDefault() {} });
  assert.equal(t.s.isOpen(), false);
});

test('Escape closes it, and only while it is open', () => {
  const t = sheet();
  t.s.open(SLIPWAY);
  let stopped = false;
  for (const fn of t.keys.listeners.keydown) fn({ key: 'Escape', stopPropagation() { stopped = true; }, preventDefault() {} });
  assert.equal(t.s.isOpen(), false);
  assert.ok(stopped, 'Escape must stop at the sheet, or the picker under it closes too');
  assert.equal((t.keys.listeners.keydown || []).length, 0, 'the key listener outlived the sheet');
  t.s.close();
  assert.equal(t.closed.length, 1, 'a second close must not call onClose again');
});

test('opening another page while open puts it in a new frame', () => {
  const t = sheet();
  t.s.open(SLIPWAY);
  const first = frameOf(t);
  t.s.open({ name: 'docs', label: 'Docs', page: '/api/ext/docs/' });
  assert.notEqual(frameOf(t), first, 'a src change on a loaded frame adds a history entry');
  assert.equal(t.root.querySelectorAll('iframe').length, 1);
  assert.equal(frameOf(t).src, '/api/ext/docs/');
  assert.equal(t.title.textContent, 'Docs');
  assert.equal(t.keys.listeners.keydown.length, 1);
});

test('reopening after a close loads the page again', () => {
  const t = sheet();
  t.s.open(SLIPWAY);
  t.s.close();
  t.s.open(SLIPWAY);
  assert.equal(frameOf(t).src, '/api/ext/slipway/');
  assert.equal(t.root.hidden, false);
});

console.log(failures ? `\n${failures} failing` : '\nall passing');
process.exit(failures ? 1 : 0);
