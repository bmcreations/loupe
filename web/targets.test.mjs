// Tests for targets.js: fetchTargets' shape validation and latestOnly's
// ordering guarantee. Run: node web/targets.test.mjs
//
// These are the two regression cases from the review of the drawer
// follow-ups. A stale response landing after a newer one must not win
// (latestOnly's `current` flag), a malformed response must be rejected
// (fetchTargets), and -- the bug that shipped -- a call whose OWN response
// arrives fine must not be told to do nothing just because a later call
// started before it finished. That last one is why fetchTargets and
// latestOnly are two separate functions: loadTargets in app.js needs its
// own outcome regardless of `current`, and only a caller that folds the two
// together can get that wrong.
import assert from 'node:assert/strict';
import {
  fetchTargets, latestOnly,
  isAvailable, isRunning, chooseTarget, alternative, failureReason, sameShown,
} from './targets.js';
import { isRunning as drawerIsRunning } from './drawer.js';

let failures = 0;
function test(name, fn) {
  const result = fn();
  if (result && typeof result.then === 'function') {
    return result
      .then(() => console.log(`  PASS ${name}`))
      .catch((err) => { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); });
  }
  console.log(`  PASS ${name}`);
  return Promise.resolve();
}

function okResponse(body) {
  return { ok: true, json: async () => body };
}

const tests = [];
function it(name, fn) { tests.push([name, fn]); }

it('fetchTargets returns the list on a normal response', async () => {
  const list = [{ id: 'synthetic', kind: 'synthetic', label: 'Synthetic', active: true }];
  const got = await fetchTargets(async () => okResponse(list));
  assert.deepEqual(got, { list, failed: false });
});

it('fetchTargets rejects a non-OK response', async () => {
  const got = await fetchTargets(async () => ({ ok: false, json: async () => { throw new Error('unused'); } }));
  assert.deepEqual(got, { list: null, failed: true });
});

it('fetchTargets rejects a non-array body', async () => {
  const got = await fetchTargets(async () => okResponse({ id: 'not-a-list' }));
  assert.deepEqual(got, { list: null, failed: true });
});

it('fetchTargets rejects a body that is an array of the wrong thing (still a shape failure, not a crash)', async () => {
  const got = await fetchTargets(async () => okResponse(null));
  assert.deepEqual(got, { list: null, failed: true });
});

it('fetchTargets treats a thrown fetch (network error) as failed', async () => {
  const got = await fetchTargets(async () => { throw new TypeError('Failed to fetch'); });
  assert.deepEqual(got, { list: null, failed: true });
});

it('latestOnly marks a stale response as not current once a later call has started', async () => {
  // Two overlapping calls, first-started resolves last (a slow initial
  // fetch racing a fast one triggered by a click during it).
  let n = 0;
  const gate = [deferred(), deferred()];
  const wrapped = latestOnly(async () => {
    const i = n++;
    await gate[i].promise;
    return i;
  });
  const first = wrapped();  // ticket 1, starts first
  const second = wrapped(); // ticket 2, starts second -- supersedes ticket 1
  gate[1].resolve(); // second's underlying work finishes first
  const secondResult = await second;
  gate[0].resolve(); // first's underlying work finishes after
  const firstResult = await first;

  assert.equal(secondResult.current, true);
  assert.equal(secondResult.result, 1);
  assert.equal(firstResult.current, false, 'the earlier-started call must read as not current once a later call has started');
  assert.equal(firstResult.result, 0, 'latestOnly must still hand back the caller\'s OWN result -- this is the bug: a caller must be able to see it, even when told it is not current');
});

it('latestOnly reports current:true for a call with nothing racing it', async () => {
  const wrapped = latestOnly(async () => 'ok');
  const { current, result } = await wrapped();
  assert.equal(current, true);
  assert.equal(result, 'ok');
});

it('latestOnly: the initial load is not told to do nothing merely because a click during it started a second, faster fetch (the shipped bug)', async () => {
  // This is loadTargets' own shape, not just latestOnly in isolation:
  // the caller must act on its own {list, failed}, using `current` only to
  // decide whether it also gets to write the shared cache.
  const gate = [deferred(), deferred()];
  let call = 0;
  const fetchTargetsLatest = latestOnly(async () => {
    const i = call++;
    await gate[i].promise;
    return i === 0 ? { list: [{ id: 'a' }], failed: false } : { list: [{ id: 'b' }], failed: false };
  });

  const initialLoad = fetchTargetsLatest(); // the page's own load, ticket 1
  const pillClick = fetchTargetsLatest();   // a click during it, ticket 2 -- resolves first
  gate[1].resolve();
  const clickOutcome = await pillClick;
  gate[0].resolve();
  const loadOutcome = await initialLoad;

  assert.equal(clickOutcome.current, true);
  // The bug: loadTargets used to treat `!current` as "do nothing at all",
  // including skipping connect()/the failure overlay. The fix is that its
  // own result is right here regardless of `current` -- a caller must use
  // it rather than bail out.
  assert.equal(loadOutcome.current, false);
  assert.deepEqual(loadOutcome.result, { list: [{ id: 'a' }], failed: false });
});

const acmeGone = {
  id: 'ACME00000000001', kind: 'device', label: 'Acme One',
  available: false, state: 'missing',
  reason: 'Acme One is not connected: adb no longer lists it',
  lastError: 'push server jar: exit status 1',
};
const emulator = { id: 'emulator-5554', kind: 'emulator', label: 'Pixel 10', available: true };
const synthetic = { id: 'synthetic', kind: 'synthetic', label: 'Test pattern', available: true };

// The case that was reported as "unable to connect": a link with no target,
// the Acme One listed first and gone from adb. Picking it meant a jar push that
// failed every retry behind a bare "Disconnected".
it('a fresh link does not open a device adb has lost', () => {
  assert.equal(chooseTarget([acmeGone, emulator, synthetic], null), 'emulator-5554');
});

it('a fresh link still prefers a real device when it is there', () => {
  const acme = { ...acmeGone, available: true, state: 'device', reason: undefined };
  assert.equal(chooseTarget([synthetic, emulator, acme], null), 'ACME00000000001');
});

// Switching the viewer to another screen without asking would hide the
// answer to "where did my device go".
it('a target the viewer asked for is kept even when it cannot open', () => {
  assert.equal(chooseTarget([acmeGone, emulator], 'ACME00000000001'), 'ACME00000000001');
});

it('an unknown requested target falls back', () => {
  assert.equal(chooseTarget([acmeGone, emulator], 'gone-for-good'), 'emulator-5554');
});

// With several panes open, a fallback opens some other device beside the
// ones asked for: on a page that named a simulator the server could not
// list, it was a physical iPhone. A strict pick finds nothing instead.
it('a strict pick does not fall back from an unknown target', () => {
  assert.equal(chooseTarget([acmeGone, emulator], 'gone-for-good', { strict: true }), null);
  assert.equal(chooseTarget([acmeGone, emulator], 'ACME00000000001', { strict: true }), 'ACME00000000001');
});

// A page saved before AVDs had ids of their own asks for the serial. Falling
// back would open the Acme One in its place, since devices rank first.
it('a saved emulator serial finds the AVD running as it', () => {
  const acme = { ...acmeGone, available: true, state: 'device', reason: undefined };
  const avd = { id: 'avd:Pixel_10', kind: 'emulator', label: 'Pixel 10 (emulator)', available: true, serial: 'emulator-5554' };
  assert.equal(chooseTarget([acme, avd], 'emulator-5554'), 'avd:Pixel_10');
});

// Opening the page with no target named should show what is already on. The
// server lists AVDs alphabetically, so a shut-down one can come first, and
// picking it booted a second emulator beside the running one.
it('a fresh link opens a running emulator over a shut-down one listed first', () => {
  const asleep = { id: 'avd:A_first', kind: 'emulator', label: 'A', available: true, bootsOnOpen: true };
  const awake = { id: 'avd:Pixel_10', kind: 'emulator', label: 'Pixel 10', available: true };
  assert.equal(chooseTarget([asleep, awake, synthetic], null), 'avd:Pixel_10');
});

it('a fresh link opens a booted simulator before booting an AVD', () => {
  const asleep = { id: 'avd:Pixel_10', kind: 'emulator', label: 'Pixel 10', available: true, bootsOnOpen: true };
  const sim = { id: 'sim:A', kind: 'simulator', label: 'iPhone 17', available: true };
  assert.equal(chooseTarget([asleep, sim, synthetic], null), 'sim:A');
});

it('with nothing running a fresh link still boots an emulator over the pattern', () => {
  const asleep = { id: 'avd:Pixel_10', kind: 'emulator', label: 'Pixel 10', available: true, bootsOnOpen: true };
  assert.equal(chooseTarget([synthetic, asleep], null), 'avd:Pixel_10');
});

it('targets.js and drawer.js agree on what is running', () => {
  const cases = [
    { kind: 'emulator' }, { kind: 'emulator', bootsOnOpen: true }, { kind: 'device', available: false },
    { kind: 'emulator', state: 'creating' }, { kind: 'synthetic' }, { kind: 'simulator' }, null,
  ];
  for (const t of cases) assert.equal(!!drawerIsRunning(t), !!(t && isRunning(t)), JSON.stringify(t));
});

it('with nothing available the first choice still opens, to show why', () => {
  assert.equal(chooseTarget([acmeGone], null), 'ACME00000000001');
  assert.equal(chooseTarget([], null), null);
});

it('the alternative is never the target that failed, nor an unavailable one', () => {
  assert.equal(alternative([acmeGone, emulator, synthetic], 'ACME00000000001'), 'emulator-5554');
  assert.equal(alternative([acmeGone, emulator], 'emulator-5554'), null);
});

it('the device state outranks a stale last error', () => {
  assert.equal(failureReason([acmeGone], 'ACME00000000001'), acmeGone.reason);
  assert.equal(failureReason([{ ...emulator, lastError: 'boom' }], 'emulator-5554'), 'boom');
  assert.equal(failureReason([emulator], 'emulator-5554'), null);
  assert.equal(failureReason(null, 'x'), null);
});

// A server from before availability tracking sends no field at all.
it('a target with no available field is available', () => {
  assert.equal(isAvailable({ id: 'x' }), true);
  assert.equal(isAvailable(acmeGone), false);
});

it('a poll only repaints when what the drawer shows changed', () => {
  const back = { ...acmeGone, available: true, state: 'device', reason: undefined };
  assert.ok(sameShown([acmeGone, emulator], [{ ...acmeGone, lastError: 'other' }, emulator]));
  assert.ok(!sameShown([acmeGone, emulator], [back, emulator]));
  const claim = { agent: 'claude', session: 's1', idleMs: 0 };
  assert.ok(sameShown([{ ...emulator, claim }], [{ ...emulator, claim: { ...claim, idleMs: 9000 } }]), 'idleMs alone repaints');
  assert.ok(!sameShown([emulator], [{ ...emulator, claim }]), 'a new claim does not repaint');
  assert.ok(!sameShown([emulator], [{ ...emulator, handsOff: true }]), 'hands off does not repaint');
  assert.ok(!sameShown([emulator], [{ ...emulator, drivenBy: 'claude' }]), 'driving does not repaint');
  assert.ok(!sameShown([emulator], [emulator, synthetic]));
  assert.ok(!sameShown(null, [emulator]));
});

function deferred() {
  let resolve;
  const promise = new Promise((r) => { resolve = r; });
  return { promise, resolve };
}

for (const [name, fn] of tests) {
  await test(name, fn);
}

if (failures) {
  console.log(`\n${failures} of ${tests.length} failing`);
  process.exit(1);
}
console.log(`\nall ${tests.length} passing`);

test('a simulator row names Simulator once', async () => {
  const { rowModel } = await import('./drawer.js');
  const row = rowModel({ id: 'sim:A', kind: 'simulator', transport: 'simulator', label: 'X', os: 'iOS', osVersion: '27.0' }, null, 0);
  assert.equal(row.detail.split('Simulator').length - 1, 1, row.detail);
});
