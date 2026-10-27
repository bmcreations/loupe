// Tests for the docked panels' collapsed state. Run: node web/panels.test.mjs
import assert from 'node:assert/strict';

const { KEY, readPanels, writePanels, toggle, applyPanels } = await import('./panels.js');

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

function fakeStorage(initial = {}) {
  const data = { ...initial };
  return {
    data,
    getItem: (k) => (k in data ? data[k] : null),
    setItem: (k, v) => { data[k] = v; },
  };
}

function fakeEl() {
  const classes = new Set();
  const attrs = {};
  return {
    classes, attrs,
    classList: { toggle: (c, on) => (on ? classes.add(c) : classes.delete(c)) },
    setAttribute: (k, v) => { attrs[k] = v; },
  };
}

test('nothing stored is both panels open', () => {
  assert.deepEqual(readPanels(fakeStorage()), { left: false, right: false });
});

test('a written state reads back', () => {
  const s = fakeStorage();
  writePanels(s, { left: true, right: false });
  assert.deepEqual(readPanels(s), { left: true, right: false });
});

test('junk, a non-boolean and a throwing or null storage read as open', () => {
  assert.deepEqual(readPanels(fakeStorage({ [KEY]: '{nope' })), { left: false, right: false });
  assert.deepEqual(readPanels(fakeStorage({ [KEY]: '{"left":"yes","right":1}' })), { left: false, right: false });
  assert.deepEqual(readPanels({ getItem() { throw new Error('blocked'); } }), { left: false, right: false });
  assert.deepEqual(readPanels(null), { left: false, right: false });
});

test('a throwing setItem does not throw', () => {
  writePanels({ setItem() { throw new Error('quota'); } }, { left: true, right: true });
});

test('toggle flips one side only', () => {
  assert.deepEqual(toggle({ left: false, right: true }, 'left'), { left: true, right: true });
  assert.deepEqual(toggle({ left: false, right: true }, 'right'), { left: false, right: false });
});

test('applyPanels sets the classes and each button reads open', () => {
  const root = fakeEl(), l = fakeEl(), r = fakeEl();
  applyPanels(root, { left: true, right: false }, { left: l, right: r });
  assert.deepEqual([...root.classes], ['left-collapsed']);
  assert.equal(l.attrs['aria-expanded'], 'false');
  assert.equal(r.attrs['aria-expanded'], 'true');
  applyPanels(root, { left: false, right: false }, { left: l, right: r });
  assert.deepEqual([...root.classes], []);
});

if (failures) { console.log(`FAIL panels (${failures})`); process.exit(1); }
console.log('PASS panels');
