// Runs every web/*.test.mjs as a child process and exits non-zero if any of
// them fail. Node has no built-in multi-file runner here (no build step, no
// dependency), so this is that runner: one line per suite, one summary line,
// and the same "each test file prints PASS/FAIL and exits 1 on failure"
// contract the existing suites already follow.
//
// Two failure modes that never touch an individual suite's PASS/FAIL output
// still have to fail the whole run, or `make test` would read a broken
// invocation as success:
//
//   - Zero suites found. A typo'd path, a directory that lost its tests, or
//     this script pointed at the wrong place all look identical to "nothing
//     to do", and silently exiting 0 would make `make test` pass while
//     testing nothing at all.
//   - A suite that hangs. A test holding an open timer or a listening socket
//     blocks spawnSync forever with no output, which would wedge `make test`
//     rather than fail it. Each suite gets a wall-clock budget; a suite that
//     runs past it is killed and counted as a failure rather than left to
//     hang the whole run.
//
// dir and timeoutMs are optional positional args (both defaulting to the
// normal case below) so a test of this runner can point it at a directory of
// its own -- an empty one for the zero-suites case, one with a
// deliberately-hanging suite and a short timeout for the hang case -- without
// waiting on this script's own real 30s budget.
import { readdirSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const webDir = path.dirname(fileURLToPath(import.meta.url));
const targetDir = process.argv[2] ? path.resolve(process.argv[2]) : webDir;
const timeoutMs = process.argv[3] ? Number(process.argv[3]) : 30000;

const files = readdirSync(targetDir)
  .filter((name) => name.endsWith('.test.mjs'))
  .sort();

if (files.length === 0) {
  console.log(`no *.test.mjs suites found in ${targetDir}`);
  process.exit(1);
}

let failed = 0;
for (const file of files) {
  console.log(`\n== ${file} ==`);
  const result = spawnSync(process.execPath, [path.join(targetDir, file)], {
    stdio: 'inherit',
    timeout: timeoutMs,
  });
  if (result.error && result.error.code === 'ETIMEDOUT') {
    console.log(`  TIMEOUT after ${timeoutMs}ms`);
  }
  if (result.status !== 0) failed++;
}

console.log(
  failed
    ? `\n${failed} of ${files.length} suites failing`
    : `\nall ${files.length} suites passing`,
);
process.exit(failed ? 1 : 0);
