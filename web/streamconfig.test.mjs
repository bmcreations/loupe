// Tests for the Stream group's pure helpers. Run: node web/streamconfig.test.mjs
// mount() is DOM wiring and has no harness here (trap 21).
import assert from 'node:assert/strict';
import { SETTINGS, formatValue, optionsFor, fieldsByName, putOutcome } from './streamconfig.js';

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

test('the three settings are the ones /api/config names', () => {
  assert.deepEqual(SETTINGS.map((s) => s.name), ['maxSize', 'bitRate', 'maxFps']);
});

test('formatValue reads like Settings', () => {
  assert.equal(formatValue('maxSize', 0), 'Native');
  assert.equal(formatValue('maxSize', 1080), '1080');
  assert.equal(formatValue('bitRate', 8000000), '8 Mbps');
  assert.equal(formatValue('bitRate', 1500000), '1.5 Mbps');
  assert.equal(formatValue('bitRate', 500000), '500 kbps');
  assert.equal(formatValue('maxFps', 60), '60 fps');
  assert.equal(formatValue('maxFps', undefined), '');
});

test('optionsFor adds a hand-set value so it still shows as checked', () => {
  const s = SETTINGS.find((x) => x.name === 'maxSize');
  assert.deepEqual(optionsFor(s, 1280), [0, 720, 1080, 1280, 1440, 1920]);
  assert.deepEqual(optionsFor(s, 1080), [0, 720, 1080, 1440, 1920]);
});

test('fieldsByName keys the response fields and survives a missing list', () => {
  const got = fieldsByName({ fields: [{ name: 'maxFps', value: 60, pinned: false }] });
  assert.equal(got.maxFps.value, 60);
  assert.deepEqual(fieldsByName(null), {});
});

test('putOutcome: a pinned setting is not reported as a change', () => {
  assert.equal(putOutcome(200, { pinned: ['bitRate'] }, 'bitRate'), 'pinned');
  assert.equal(putOutcome(200, { pinned: [] }, 'bitRate'), 'ok');
  assert.equal(putOutcome(400, null, 'bitRate'), 'error');
  assert.equal(putOutcome(0, null, 'bitRate'), 'error');
});

console.log(failures ? `\n${failures} failing` : '\nall passing');
process.exit(failures ? 1 : 0);
