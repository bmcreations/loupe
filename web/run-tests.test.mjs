// Tests for run-tests.mjs itself: the runner's own failure modes, which
// never show up in an individual suite's PASS/FAIL output and so need
// covering separately. Run: node web/run-tests.test.mjs
//
// Each case spawns run-tests.mjs against a throwaway directory rather than
// against web/ itself, so a hanging fixture suite or an empty directory
// never depends on -- or perturbs -- the real suites.
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const runnerPath = fileURLToPath(new URL('./run-tests.mjs', import.meta.url));

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

function run(dir, timeoutMs) {
  const args = [runnerPath, dir];
  if (timeoutMs !== undefined) args.push(String(timeoutMs));
  return spawnSync(process.execPath, args, { encoding: 'utf8' });
}

test('an empty directory (no *.test.mjs files) exits 1, not 0', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'loupe-run-tests-empty-'));
  try {
    const result = run(dir);
    assert.notEqual(result.status, 0);
    assert.match(result.stdout, /no \*\.test\.mjs suites found/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('a directory with only non-matching files also exits 1', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'loupe-run-tests-nomatch-'));
  try {
    writeFileSync(path.join(dir, 'not-a-suite.mjs'), 'export {};\n');
    writeFileSync(path.join(dir, 'readme.md'), '# nope\n');
    const result = run(dir);
    assert.notEqual(result.status, 0);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('a directory with one passing suite exits 0', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'loupe-run-tests-pass-'));
  try {
    writeFileSync(path.join(dir, 'ok.test.mjs'), 'process.exit(0);\n');
    const result = run(dir);
    assert.equal(result.status, 0);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('a directory with one failing suite exits 1 and still runs the rest', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'loupe-run-tests-fail-'));
  try {
    writeFileSync(path.join(dir, 'a-fails.test.mjs'), 'process.exit(1);\n');
    writeFileSync(path.join(dir, 'b-passes.test.mjs'), 'process.exit(0);\n');
    const result = run(dir);
    assert.notEqual(result.status, 0);
    assert.match(result.stdout, /a-fails\.test\.mjs/);
    assert.match(result.stdout, /b-passes\.test\.mjs/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('a suite that hangs past its timeout is killed and counted as a failure, not left to hang the run', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'loupe-run-tests-hang-'));
  try {
    // A bare unsettled top-level await has no pending timer, so Node's own
    // deadlock detection would exit it with code 13 on its own (proven by
    // hand against this exact snippet) -- which would pass this test for
    // the wrong reason, without the runner's timeout ever firing. An
    // interval is a real pending handle, so the process only ends when
    // something kills it, which is what makes this fixture actually hang.
    writeFileSync(path.join(dir, 'hangs.test.mjs'), 'setInterval(() => {}, 1000);\n');
    const start = Date.now();
    const result = run(dir, 1000);
    const elapsed = Date.now() - start;
    assert.notEqual(result.status, 0);
    assert.match(result.stdout, /TIMEOUT/);
    // Loose upper bound: proves the runner did not fall back to its own
    // 30s default despite the hang, without pinning an exact duration.
    assert.ok(elapsed < 10000, `expected the 1000ms timeout to bound the run, took ${elapsed}ms`);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

console.log(failures ? `\n${failures} failing` : '\nall passing');
process.exit(failures ? 1 : 0);
