// Tests for the adaptive jitter buffer. Run: node web/jitter.test.mjs
import { JitterBuffer, ArrivalJitter } from './jitter.js';
import assert from 'node:assert/strict';

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

const FRAME_MS = 1000 / 60;

/**
 * Drive a buffer through `seconds` of arrivals and presentation.
 *
 * `at` continues a previous run's timeline. PTS and the wall clock are
 * monotonic within a real session, so restarting them between phases would
 * hand the buffer a backwards time jump it never actually sees.
 */
function simulate(buf, { seconds = 3, jitterMs = 20, burstAt = null, burstSize = 0, at = { now: 0, pts: 0 } } = {}) {
  let now = at.now, pts = at.pts, presented = 0;
  const totalFrames = Math.round(seconds * 60);

  for (let i = 0; i < totalFrames; i++) {
    // Arrivals clump: several frames land together, then a gap.
    const clump = (i % 3 === 0) ? 3 : 0;
    for (let c = 0; c < clump; c++) {
      buf.enqueue({ ptsMs: pts }, now);
      pts += FRAME_MS;
    }
    if (burstAt !== null && i === burstAt) {
      for (let b = 0; b < burstSize; b++) {
        buf.enqueue({ ptsMs: pts }, now);
        pts += FRAME_MS;
      }
    }
    // One animation frame per display refresh.
    now += FRAME_MS;
    buf.tick(jitterMs);
    if (buf.due(now)) presented++;
  }
  return { presented, totalFrames, at: { now, pts } };
}

test('presents frames under clumped arrival', () => {
  const buf = new JitterBuffer();
  const { presented } = simulate(buf, { seconds: 3 });
  assert.ok(presented > 100, `presented only ${presented} frames in 3s`);
});

test('a startup burst does not latch the clock', () => {
  // The original bug: a burst overflowed the queue, the guard cleared the
  // baseline, and nothing was ever presented again.
  const buf = new JitterBuffer({ queueMax: 8 });
  const { presented } = simulate(buf, { seconds: 3, burstAt: 1, burstSize: 60 });
  assert.ok(presented > 100, `latched after burst: presented ${presented}`);
  assert.equal(buf.baseline === null, false, 'clock left unanchored');
});

test('overruns stop once the queue drains', () => {
  const buf = new JitterBuffer({ queueMax: 8 });
  const phase1 = simulate(buf, { seconds: 1, burstAt: 1, burstSize: 60 });
  const after = buf.overruns;
  simulate(buf, { seconds: 2, at: phase1.at });
  assert.equal(buf.overruns, after, `overruns still climbing: ${after} -> ${buf.overruns}`);
});

test('queue depth stays bounded', () => {
  const buf = new JitterBuffer({ queueMax: 24 });
  let maxHeld = 0;
  const orig = buf.enqueue.bind(buf);
  buf.enqueue = (item, now) => { orig(item, now); maxHeld = Math.max(maxHeld, buf.held); };
  simulate(buf, { seconds: 3, burstAt: 10, burstSize: 100 });
  assert.ok(maxHeld <= 24, `queue grew to ${maxHeld}`);
});

test('buffer tracks jitter and respects the ceiling', () => {
  const buf = new JitterBuffer({ maxBufferMs: 50 });
  for (let i = 0; i < 2000; i++) buf.tick(200); // absurd jitter
  assert.ok(buf.bufferMs <= 50.001, `buffer exceeded ceiling: ${buf.bufferMs}`);
  for (let i = 0; i < 2000; i++) buf.tick(0);   // perfect link
  assert.ok(buf.bufferMs < 1, `buffer did not collapse on a clean link: ${buf.bufferMs}`);
});

test('discarded frames are disposed exactly once', () => {
  const disposed = new Set();
  let doubleDispose = false;
  const buf = new JitterBuffer({
    queueMax: 8,
    dispose: (item) => {
      if (disposed.has(item.id)) doubleDispose = true;
      disposed.add(item.id);
    },
  });
  let now = 0, pts = 0, id = 0;
  const presentedIds = new Set();
  for (let i = 0; i < 300; i++) {
    for (let c = 0; c < 3; c++) {
      buf.enqueue({ ptsMs: pts, id: id++ }, now);
      pts += FRAME_MS;
    }
    now += FRAME_MS;
    const item = buf.due(now);
    if (item) presentedIds.add(item.id);
  }
  assert.equal(doubleDispose, false, 'an item was disposed twice');
  for (const pid of presentedIds) {
    assert.equal(disposed.has(pid), false, `presented frame ${pid} was also disposed`);
  }
});

test('flush leaves the buffer reusable', () => {
  const buf = new JitterBuffer();
  const warm = simulate(buf, { seconds: 1 });
  buf.flush();
  assert.equal(buf.held, 0);
  assert.equal(buf.baseline, null);
  const { presented } = simulate(buf, { seconds: 2, at: { now: 1000, pts: 1000 } });
  assert.ok(presented > 60, `did not resume after flush: ${presented}`);
});

test('a late clock is pulled forward instead of stalling', () => {
  const buf = new JitterBuffer();
  buf.enqueue({ ptsMs: 0 }, 0);
  // Anchor the clock far in the future, as a drifted one would be.
  buf.baseline = 10000;
  let got = null;
  for (let i = 0; i < 20 && !got; i++) got = buf.due(i * FRAME_MS);
  assert.ok(got, 'stalled on a clock anchored in the future');
});


// The 5G trace that motivated the setting reported arrival jitter of 28-347ms
// against a buffer pinned at exactly 50.0 in 28 of 32 rows: the ceiling, not
// the measurement, was deciding. Raising it has to actually let the buffer
// track, or the setting does nothing.
test('the ceiling, not the slew, is what bounds growth', () => {
  const tight = new JitterBuffer({ maxBufferMs: 50 });
  const loose = new JitterBuffer({ maxBufferMs: 300 });
  // 200ms of jitter wants 300ms of buffer, which only one of these can reach.
  simulate(tight, { seconds: 6, jitterMs: 200 });
  simulate(loose, { seconds: 6, jitterMs: 200 });

  assert.ok(tight.bufferMs <= 50 + 1e-9, `tight exceeded its cap: ${tight.bufferMs}`);
  assert.ok(tight.bufferMs > 49, `tight should be pinned at its cap, got ${tight.bufferMs}`);
  assert.ok(loose.bufferMs > 150, `loose should track past 50, got ${loose.bufferMs}`);
});

// A ceiling of 0 is a legal setting and means draw on arrival. It must not
// produce a negative buffer or stall presentation.
test('a zero ceiling still presents frames', () => {
  const buf = new JitterBuffer({ maxBufferMs: 0 });
  const { presented } = simulate(buf, { seconds: 3, jitterMs: 200 });
  assert.ok(buf.bufferMs >= 0, `negative buffer: ${buf.bufferMs}`);
  // Frames land three to a clump, and with no cushion only the newest of each
  // is drawn -- what drawing on arrival does. What must not happen is a stall.
  assert.ok(presented >= 55, `presented ${presented} frames with a zero ceiling`);
});

// Frames at a steady 60fps with a fixed transit, after a first frame that took
// `firstMs`. Presents on every ms tick and returns how long each frame waited.
function holdsAfterSlowFirst(buf, { firstMs, transitMs = 20, frames = 180, interactiveAt = null }) {
  const holds = [];
  buf.enqueue({ ptsMs: 0, at: firstMs }, firstMs);
  let t = firstMs;
  for (let i = 1; i < frames; i++) {
    const pts = i * FRAME_MS;
    const arr = pts + transitMs;
    if (interactiveAt !== null && i === interactiveAt) buf.interactive = true;
    for (; t < arr; t++) {
      const it = buf.due(t);
      if (it) holds.push(t - it.at);
    }
    buf.enqueue({ ptsMs: pts, at: arr }, arr);
  }
  return holds;
}

test('a slow first frame does not set the latency for the session', () => {
  const buf = new JitterBuffer({ maxBufferMs: 50, initialBufferMs: 16 });
  const holds = holdsAfterSlowFirst(buf, { firstMs: 150 });
  const tail = holds.slice(-60).sort((a, b) => a - b);
  const p50 = tail[tail.length >> 1];
  // Anchored on the keyframe, this was 146ms for every frame.
  assert.ok(p50 <= 20, `held ${p50}ms against a 16ms buffer`);
});

test('the clock does not step when transit is steady', () => {
  const buf = new JitterBuffer({ maxBufferMs: 50, initialBufferMs: 16 });
  const holds = holdsAfterSlowFirst(buf, { firstMs: 20 });
  const tail = holds.slice(-120);
  const spread = Math.max(...tail) - Math.min(...tail);
  assert.ok(spread <= 2, `hold wandered by ${spread}ms on a steady link`);
});

test('a finger down caps the cushion', () => {
  const buf = new JitterBuffer({ maxBufferMs: 50, initialBufferMs: 50, interactiveBufferMs: 16 });
  const holds = holdsAfterSlowFirst(buf, { firstMs: 20, interactiveAt: 60 });
  const before = holds.slice(40, 55).sort((a, b) => a - b);
  const after = holds.slice(-60).sort((a, b) => a - b);
  assert.ok(before[before.length >> 1] >= 45, `held ${before[before.length >> 1]}ms before the touch`);
  assert.ok(after[after.length >> 1] <= 18, `held ${after[after.length >> 1]}ms with a finger down`);
});

test('holds are reported and reset', () => {
  const buf = new JitterBuffer({ maxBufferMs: 50, initialBufferMs: 16 });
  holdsAfterSlowFirst(buf, { firstMs: 20, frames: 30 });
  const h = buf.drainHolds();
  assert.ok(h && h.p50 >= 0 && h.max >= h.p50, JSON.stringify(h));
  assert.equal(buf.drainHolds(), null);
});


// --- ArrivalJitter -----------------------------------------------------------

test('arrival jitter sits near zero on a steady stream', () => {
  const j = new ArrivalJitter();
  for (let i = 0; i < 100; i++) j.observe(i * 16.7, i * 16.7);
  assert.ok(j.value < 0.1, `expected ~0, got ${j.value}`);
});

test('arrival jitter ignores an idle screen', () => {
  const j = new ArrivalJitter();
  // 12fps for two seconds, then the screen goes idle for 25s (trap 20), then
  // it resumes. Nothing about the link changed.
  let t = 0;
  for (let i = 0; i < 24; i++, t += 83) j.observe(t, t);
  const beforeGap = j.value;

  t += 25000;
  j.observe(t, t);
  for (let i = 0; i < 12; i++, t += 83) j.observe(t, t);

  assert.ok(j.value < beforeGap + 1,
    `an idle gap must not register as jitter: ${beforeGap} -> ${j.value}`);
});

test('arrival jitter still sees real transit variation', () => {
  const j = new ArrivalJitter();
  // The sender is steady at 60fps; arrivals are not.
  let arrival = 0;
  for (let i = 0; i < 100; i++) {
    arrival += (i % 2 === 0) ? 4 : 30;
    j.observe(arrival, i * 16.7);
  }
  assert.ok(j.value > 5, `expected real jitter to register, got ${j.value}`);
});

test('arrival jitter re-anchors when the encoder restarts', () => {
  const j = new ArrivalJitter();
  let t = 0;
  for (let i = 0; i < 60; i++, t += 16.7) j.observe(t, t);
  const before = j.value;

  // A relaunch, or a reconnect onto a fresh session: PTS restarts at zero
  // while the local clock keeps going.
  t += 500;
  j.observe(t, 0);
  for (let i = 1; i < 30; i++, t += 16.7) j.observe(t, i * 16.7);

  assert.ok(j.value < before + 1,
    `a PTS restart must not register as jitter: ${before} -> ${j.value}`);
});


test('a parameter-set frame does not poison the estimate', () => {
  const j = new ArrivalJitter();
  // Measured on emulator-5554: the config frame reports pts 0 and the very
  // next media frame reports absolute device uptime.
  const uptimeMs = 351876184.098;
  let t = 0;
  j.observe(t, 0);
  t += 20;
  j.observe(t, uptimeMs);
  for (let i = 1; i < 60; i++) { t += 16.7; j.observe(t, uptimeMs + i * 16.7); }

  assert.ok(j.value < 5,
    `a config frame's zero pts must not read as transit variation: ${j.value}`);
});

test('a target switch does not register as jitter', () => {
  const j = new ArrivalJitter();
  // Forwards, not backwards: swapping emulator-5554 for an Acme One moves pts
  // from one device's uptime to another's, and the Acme One's is larger.
  const emulatorMs = 351876184.098;
  const acmeMs = 2663591420.0;
  let t = 0;
  for (let i = 0; i < 30; i++, t += 16.7) j.observe(t, emulatorMs + i * 16.7);
  const before = j.value;

  t += 400;
  for (let i = 0; i < 30; i++, t += 16.7) j.observe(t, acmeMs + i * 16.7);

  assert.ok(j.value < before + 1,
    `a target switch must not register as jitter: ${before} -> ${j.value}`);
});

test('a transit change inside the plausible range still registers', () => {
  const j = new ArrivalJitter();
  let t = 0;
  let pts = 0;
  for (let i = 0; i < 30; i++, t += 16.7, pts += 16.7) j.observe(t, pts);

  // Trap 25 measured the phone reporting 500-2000ms across a network
  // handover. That is the link, not a discontinuity, and must survive.
  t += 2000; pts += 16.7;
  j.observe(t, pts);

  assert.ok(j.value > 50,
    `a two-second transit change must still register: ${j.value}`);
});


console.log(failures === 0 ? '\nALL PASS' : `\n${failures} FAILED`);
process.exit(failures === 0 ? 0 : 1);
