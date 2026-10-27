// Tests for the handedness preference. Run: node web/handedness.test.mjs
//
// storage is a fake implementing only getItem/setItem, not a real Storage,
// so a test can make either call throw without a browser. classList is
// likewise a fake recording toggle() calls, since applyHand never reads DOM
// state back, only writes it.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const { KEY, readHand, writeHand, applyHand, safeStorage } = await import('./handedness.js');

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

test('default with nothing stored is left', () => {
  const storage = fakeStorage();
  assert.equal(readHand(storage), 'left');
});

test('round trip: writing right and reading back gives right', () => {
  const storage = fakeStorage();
  writeHand(storage, 'right');
  assert.equal(readHand(storage), 'right');
  assert.equal(storage.data[KEY], 'right');
});

test('round trip: writing left and reading back gives left', () => {
  const storage = fakeStorage({ [KEY]: 'right' });
  writeHand(storage, 'left');
  assert.equal(readHand(storage), 'left');
});

test('a junk stored value reads as left, not thrown', () => {
  const storage = fakeStorage({ [KEY]: 'sideways' });
  assert.equal(readHand(storage), 'left');
});

test('a non-string junk value reads as left', () => {
  const storage = fakeStorage({ [KEY]: 42 });
  assert.equal(readHand(storage), 'left');
});

test('writeHand with a junk hand value normalises to left rather than storing garbage', () => {
  const storage = fakeStorage();
  writeHand(storage, 'sideways');
  assert.equal(storage.data[KEY], 'left');
});

test('a storage whose getItem throws (private mode) reads as left, not thrown', () => {
  const storage = {
    getItem() { throw new DOMException('denied', 'SecurityError'); },
    setItem() {},
  };
  assert.equal(readHand(storage), 'left');
});

test('a storage whose setItem throws (quota, private mode) does not throw out of writeHand', () => {
  const storage = {
    getItem: () => null,
    setItem() { throw new DOMException('denied', 'QuotaExceededError'); },
  };
  assert.doesNotThrow(() => writeHand(storage, 'right'));
});

function fakeClassList() {
  const calls = [];
  const classes = new Set();
  return {
    calls,
    classes,
    toggle(name, force) {
      calls.push([name, force]);
      if (force) classes.add(name); else classes.delete(name);
    },
  };
}

test('applyHand adds hand-right for right', () => {
  const root = { classList: fakeClassList() };
  applyHand(root, 'right');
  assert.deepEqual(root.classList.calls, [['hand-right', true]]);
  assert.ok(root.classList.classes.has('hand-right'));
});

test('applyHand removes hand-right for left', () => {
  const root = { classList: fakeClassList() };
  applyHand(root, 'left');
  assert.deepEqual(root.classList.calls, [['hand-right', false]]);
  assert.ok(!root.classList.classes.has('hand-right'));
});

test('applyHand treats any non-right value as left, including junk', () => {
  const root = { classList: fakeClassList() };
  applyHand(root, 'sideways');
  assert.deepEqual(root.classList.calls, [['hand-right', false]]);
});

test('safeStorage returns the real localStorage when reading it does not throw', () => {
  const fakeLocalStorage = fakeStorage();
  const win = { localStorage: fakeLocalStorage };
  assert.equal(safeStorage(win), fakeLocalStorage);
});

test('safeStorage returns null when merely reading window.localStorage throws (Safari, storage fully blocked)', () => {
  const win = {
    get localStorage() { throw new DOMException('denied', 'SecurityError'); },
  };
  assert.equal(safeStorage(win), null);
});

test('safeStorage normalises a falsy localStorage (present but disabled) to null, not passed through as-is', () => {
  // '' rather than null/undefined: assert.equal treats null and undefined as
  // interchangeable, which would let a `|| null` normalisation go untested.
  const win = { localStorage: '' };
  assert.strictEqual(safeStorage(win), null);
});

test('safeStorage returns null with no window-like object at all', () => {
  assert.equal(safeStorage(null), null);
});

test('safeStorage feeding a throwing getter straight into readHand still reads left, not thrown', () => {
  const win = {
    get localStorage() { throw new DOMException('denied', 'SecurityError'); },
  };
  assert.equal(readHand(safeStorage(win)), 'left');
});

// index.html's inline head script applies handedness before first paint
// (before app.js, a module, has even started evaluating), and it is
// necessarily a hand-written duplicate of readHand/applyHand -- a classic
// script cannot import a module. Nothing enforces the two agree, so this
// reads the script's own text out of index.html and pins its key, its
// 'right' value and its 'hand-right' class against the same constants and
// literals this file already tests against handedness.js. Changing the head
// script to check for 'left' (still a plausible typo -- readHand's own
// default is 'left') would apply the class to everyone instead of no one,
// and previously passed nothing at all.
const indexHtmlPath = path.join(path.dirname(fileURLToPath(import.meta.url)), 'index.html');
const indexHtml = readFileSync(indexHtmlPath, 'utf8');

// The boot probe script is a separate, byte-for-byte-pinned classic script
// (see trap 21/38's telemetry requirements); this match is scoped to the
// handedness script specifically by anchoring on the localStorage.getItem
// call it alone makes, rather than matching "the first <script> in <head>"
// and silently starting to check the wrong one if the two are reordered.
const headScriptMatch = indexHtml.match(
  /<script>([^]*?localStorage\.getItem[^]*?)<\/script>/
);

test('index.html has an inline head script that reads handedness before first paint', () => {
  assert.ok(headScriptMatch, 'no <script> in index.html calls localStorage.getItem');
});

const headScript = headScriptMatch ? headScriptMatch[1] : '';

test('the head script reads the same storage key handedness.js does', () => {
  const m = headScript.match(/getItem\(\s*['"]([^'"]+)['"]\s*\)/);
  assert.ok(m, 'head script does not call getItem with a literal key');
  assert.equal(m[1], KEY);
});

test('the head script compares against the same right-handed value readHand does', () => {
  const m = headScript.match(/getItem\([^)]*\)\s*===\s*['"]([^'"]+)['"]/);
  assert.ok(m, 'head script does not compare getItem(...) === <literal>');
  assert.equal(m[1], 'right');
});

test('the head script adds the same class applyHand does', () => {
  const m = headScript.match(/classList\.add\(\s*['"]([^'"]+)['"]\s*\)/);
  assert.ok(m, 'head script does not call classList.add with a literal class name');
  assert.equal(m[1], 'hand-right');
});

console.log(failures ? `\n${failures} failing` : '\nall passing');
process.exit(failures ? 1 : 0);
