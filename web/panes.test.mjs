// Tests for panes.js: the list of open panes, which one has focus, and the
// rules the spec puts on them (one pane per target, at most four).
//
// Run: node web/panes.test.mjs
import assert from 'node:assert/strict';
import { createPanes, parseTargets, buildSearch, paneShortcut, paneKey } from './panes.js';

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

// A pane stand-in: what the registry reads (targetId, id) and what it calls.
function fixture(max) {
  const made = [];
  const destroyed = [];
  const changes = [];
  const reg = createPanes({
    max,
    create: (targetId, id) => {
      const p = { id, targetId, switchTarget(t) { this.targetId = t; } };
      made.push(p);
      return p;
    },
    destroy: (p) => destroyed.push(p.id),
  });
  reg.onChange((ev) => changes.push(ev.kind));
  return { reg, made, destroyed, changes };
}

test('open adds a pane, focuses it and returns it', () => {
  const { reg } = fixture();
  const a = reg.open('emulator-5554');
  assert.equal(a.targetId, 'emulator-5554');
  assert.equal(reg.focused(), a);
  const b = reg.open('sim:ABC');
  assert.equal(reg.focused(), b);
  assert.deepEqual(reg.list().map((p) => p.targetId), ['emulator-5554', 'sim:ABC']);
  assert.notEqual(a.id, b.id);
});

test('open of a target already open focuses that pane instead', () => {
  const { reg, made } = fixture();
  const a = reg.open('emulator-5554');
  reg.open('sim:ABC');
  assert.equal(reg.open('emulator-5554'), a);
  assert.equal(reg.focused(), a);
  assert.equal(made.length, 2);
});

test('a fifth pane is refused', () => {
  const { reg } = fixture();
  for (const t of ['a', 'b', 'c', 'd']) assert.ok(reg.open(t));
  assert.equal(reg.open('e'), null);
  assert.equal(reg.list().length, 4);
  assert.equal(reg.full(), true);
});

test('max is configurable', () => {
  const { reg } = fixture(2);
  reg.open('a'); reg.open('b');
  assert.equal(reg.open('c'), null);
});

test('close tears the pane down and focuses its neighbour', () => {
  const { reg, destroyed } = fixture();
  const a = reg.open('a');
  const b = reg.open('b');
  const c = reg.open('c');
  reg.focus(b.id);
  reg.close(b.id);
  assert.deepEqual(destroyed, [b.id]);
  // The next pane, the one that slid into its place.
  assert.equal(reg.focused(), c);
  reg.close(c.id);
  // No next: the previous.
  assert.equal(reg.focused(), a);
  reg.close(a.id);
  assert.equal(reg.focused(), null);
  assert.deepEqual(reg.list(), []);
});

test('closing a pane without focus keeps focus where it is', () => {
  const { reg } = fixture();
  const a = reg.open('a');
  const b = reg.open('b');
  reg.close(a.id);
  assert.equal(reg.focused(), b);
});

test('focus, focusIndex and indexOf', () => {
  const { reg } = fixture();
  const a = reg.open('a');
  const b = reg.open('b');
  assert.equal(reg.focus(a.id), a);
  assert.equal(reg.focused(), a);
  assert.equal(reg.focusIndex(2), b);
  assert.equal(reg.focused(), b);
  // Past the end, nothing changes.
  assert.equal(reg.focusIndex(3), null);
  assert.equal(reg.focused(), b);
  assert.equal(reg.indexOf(a.id), 1);
  assert.equal(reg.indexOf(b.id), 2);
  assert.equal(reg.indexOf('nope'), 0);
  assert.equal(reg.byTarget('b'), b);
  assert.equal(reg.byTarget('z'), null);
});

test('replace retargets a pane in place, unless the target is open elsewhere', () => {
  const { reg } = fixture();
  const a = reg.open('a');
  const b = reg.open('b');
  assert.equal(reg.replace(a.id, 'c'), a);
  assert.equal(a.targetId, 'c');
  assert.deepEqual(reg.list().map((p) => p.targetId), ['c', 'b']);
  // b is open in its own pane: focus it rather than open it twice.
  assert.equal(reg.replace(a.id, 'b'), b);
  assert.equal(a.targetId, 'c');
  assert.equal(reg.focused(), b);
});

test('onChange fires once per change with its kind', () => {
  const { reg, changes } = fixture();
  const a = reg.open('a');
  reg.open('b');
  reg.open('a'); // focuses a
  reg.focus(a.id); // already focused: nothing
  reg.close(a.id);
  assert.deepEqual(changes, ['open', 'open', 'focus', 'close']);
});

test('each visits every pane once, in order', () => {
  const { reg } = fixture();
  reg.open('a'); reg.open('b'); reg.open('c');
  const seen = [];
  reg.each((p) => seen.push(p.targetId));
  assert.deepEqual(seen, ['a', 'b', 'c']);
});

test('parseTargets reads every ?target= in order, without repeats', () => {
  assert.deepEqual(parseTargets('?target=emulator-5554&target=sim:ABC'), ['emulator-5554', 'sim:ABC']);
  assert.deepEqual(parseTargets('?target=a&x=1&target=a'), ['a']);
  assert.deepEqual(parseTargets(''), []);
  assert.deepEqual(parseTargets('?target='), []);
});

test('buildSearch writes the targets back and keeps other params', () => {
  assert.equal(buildSearch('?token=t&target=a', ['b', 'c']), '?token=t&target=b&target=c');
  assert.equal(buildSearch('?target=a', []), '');
  assert.equal(buildSearch('', ['sim:ABC']), '?target=sim%3AABC');
});

// Safari keeps ⌘1-⌘9 for tabs and never hands them to the page, so the
// pane shortcuts are ⌃ on a Mac. Elsewhere ctrl+digit switches tabs, so Alt.
test('paneShortcut reads ⌃1-⌃4 on a Mac and ignores ⌘', () => {
  assert.equal(paneShortcut({ ctrlKey: true, code: 'Digit2' }, true), 2);
  assert.equal(paneShortcut({ metaKey: true, code: 'Digit2' }, true), 0);
  assert.equal(paneShortcut({ ctrlKey: true, shiftKey: true, code: 'Digit2' }, true), 0);
  assert.equal(paneShortcut({ ctrlKey: true, code: 'Digit5' }, true), 0);
});
test('paneShortcut reads Alt+1-4 elsewhere, by key position', () => {
  assert.equal(paneShortcut({ altKey: true, code: 'Digit3', key: '£' }, false), 3);
  assert.equal(paneShortcut({ ctrlKey: true, code: 'Digit3' }, false), 0);
});
test('paneKey names the modifier the badges show', () => {
  assert.equal(paneKey(true), '⌃');
  assert.equal(paneKey(false), 'Alt+');
});

console.log(failures ? `\n${failures} failing` : '\nall passing');
process.exit(failures ? 1 : 0);
