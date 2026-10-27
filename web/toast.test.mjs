// Tests for the shared toast helper. Run: node web/toast.test.mjs
//
// Added for client Task 9's review (MEDIUM-9): corners.js, app.js (for
// devicesettings) and inspect.js each used to run their own setTimeout
// against the same #toast element, so two toasts racing each other resolved
// to whichever module's private timer happened to fire first rather than to
// whichever call was most recent. showToast owns the one timer now.
import assert from 'node:assert/strict';
import { installFakeDom } from './fakedom.js';

installFakeDom();
const { showToast } = await import('./toast.js');

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

test('shows the message and un-hides the element', () => {
  const el = document.createElement('div');
  el.hidden = true;
  showToast(el, 'hello');
  assert.equal(el.hidden, false);
  assert.equal(el.textContent, 'hello');
});

test('a second call before the first timer fires replaces the message rather than racing it', () => {
  const el = document.createElement('div');
  showToast(el, 'first');
  showToast(el, 'second');
  assert.equal(el.textContent, 'second');
});

test('sets data-corner only when a corner is given, and leaves it alone otherwise', () => {
  const el = document.createElement('div');
  showToast(el, 'x', 'more');
  assert.equal(el.dataset.corner, 'more');
  showToast(el, 'y');
  assert.equal(el.dataset.corner, 'more', 'an omitted corner must not clear a previous one');
});

test('a null element is a no-op, not a throw', () => {
  assert.doesNotThrow(() => showToast(null, 'x'));
});

test('a second toast cancels the first\'s timer, so its own full 2500ms is honoured (review round 3, N19)', () => {
  // showToast's own module-level `timer` variable is what's under test:
  // without `clearTimeout(timer)` before scheduling a new one, the FIRST
  // call's timer still fires 2500ms after the first call, hiding the
  // second toast early rather than at 2500ms after the second call.
  // Instrument setTimeout/clearTimeout rather than waiting 2.5 real
  // seconds: this proves the second call actually asked to cancel the
  // first's specific timer id, which is the mechanism, not just an
  // eventual outcome a slow poll could get right by accident.
  const realSetTimeout = globalThis.setTimeout;
  const realClearTimeout = globalThis.clearTimeout;
  const scheduled = [];
  const cleared = [];
  let nextId = 1;
  globalThis.setTimeout = (fn, ms) => { const id = nextId++; scheduled.push({ id, fn, ms }); return id; };
  globalThis.clearTimeout = (id) => { cleared.push(id); };
  try {
    const el = document.createElement('div');
    showToast(el, 'first');
    assert.equal(scheduled.length, 1, 'first call schedules one timer');
    const firstId = scheduled[0].id;
    showToast(el, 'second');
    assert.equal(scheduled.length, 2, 'second call schedules its own timer');
    assert.ok(cleared.includes(firstId), 'the first call\'s timer must be cancelled before the second is scheduled -- otherwise it still fires 2500ms after the FIRST call and hides the second toast early');
    // Firing only the (uncancelled) second timer must hide the element;
    // the test would still pass a mutant that forgot to clear if firing
    // the stale first timer here also hid it, so this only proves the
    // second timer is the one still live and does its job.
    scheduled[1].fn();
    assert.equal(el.hidden, true);
  } finally {
    globalThis.setTimeout = realSetTimeout;
    globalThis.clearTimeout = realClearTimeout;
  }
});

console.log(failures ? `\n${failures} failing` : '\nall passing');
process.exit(failures ? 1 : 0);
