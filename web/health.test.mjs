// Tests for the health dot's verdict mapping. Run: node web/health.test.mjs
//
// Cases are read from testdata/client/health.json so a later Kotlin client
// can run the same vectors. dotFor is pure: it takes `now` as a parameter
// rather than reading Date.now() itself, so a test does not need fake timers
// to hit the 2500ms stale boundary from both sides.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const { dotFor, secondRtt } = await import('./health.js');

const cases = JSON.parse(readFileSync(new URL('../testdata/client/health.json', import.meta.url)));

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

// JSON has no NaN literal, so a fixture that needs the real value (pinning
// that dotFor checks Number.isFinite, not just != null) spells receivedAt as
// the string "NaN" and gets it swapped in here.
function resolveInput(input) {
  if (!input || input.receivedAt !== 'NaN') return input;
  return { ...input, receivedAt: NaN };
}

for (const c of cases) {
  test(c.name, () => {
    const got = dotFor(resolveInput(c.input), c.now);
    assert.deepEqual(got, c.expect);
  });
}

// The server judges health on the last second's rtt, so a burst clears as
// soon as a clean second is reported. The median keeps one late pong from
// making a second read slow; no pongs at all is no reading.
test('secondRtt is the median of the second\'s pongs', () => {
  assert.equal(secondRtt([12, 300, 14]), 14);
  assert.equal(secondRtt([10, 20]), 15);
  assert.equal(secondRtt([42]), 42);
  assert.equal(secondRtt([]), undefined);
});

console.log(failures ? `\n${failures} failing` : '\nall passing');
process.exit(failures ? 1 : 0);
