// Tests for the Connection page's rows. Run: node web/connstats.test.mjs
import assert from 'node:assert/strict';

const { connectionRows } = await import('./connstats.js');

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

const row = (rows, id) => rows.find((r) => r.id === id);

const healthy = {
  status: 'live', statusText: 'Live', transport: 'webtransport',
  rtt: 12.4, jitter: 2.1, kbps: 3400, width: 496, height: 1080, codec: 'avc1.640028',
  targetId: 'emulator-5554', decFps: 60, fps: 59, bufferMs: 22,
  dropped: 0, decodeErrors: 0, shed: 0, queue: 0,
};

test('a healthy session colours nothing and lists no problems', () => {
  const { link, video, problems } = connectionRows(healthy);
  for (const r of [...link, ...video]) assert.equal(r.grade, '', `${r.id} graded ${r.grade}`);
  assert.deepEqual(problems, []);
});

test('values carry plain units', () => {
  const { link, video } = connectionRows(healthy);
  assert.equal(row(link, 'rtt').value, '12 ms');
  assert.equal(row(link, 'jitter').value, '2.1 ms');
  assert.equal(row(link, 'rate').value, '3.4 Mbps');
  assert.equal(row(link, 'transport').value, 'WebTransport');
  assert.equal(row(video, 'size').value, '496 × 1080');
  assert.equal(row(video, 'decoded').value, '60 fps');
  assert.equal(row(video, 'buffer').value, '22 ms');
  assert.equal(connectionRows({ kbps: 480 }).link.find((r) => r.id === 'rate').value, '480 kbps');
});

test('a sub-millisecond round trip is not 0 ms', () => {
  assert.equal(row(connectionRows({ rtt: 0.4 }).link, 'rtt').value, '<1 ms');
});

test('round trip grades at the HUD thresholds, 60 and 120', () => {
  assert.equal(row(connectionRows({ rtt: 59 }).link, 'rtt').grade, '');
  assert.equal(row(connectionRows({ rtt: 60 }).link, 'rtt').grade, 'warn');
  assert.equal(row(connectionRows({ rtt: 120 }).link, 'rtt').grade, 'bad');
});

test('frame rates grade below 55 and 40, and zero is no reading', () => {
  assert.equal(row(connectionRows({ decFps: 54 }).video, 'decoded').grade, 'warn');
  assert.equal(row(connectionRows({ decFps: 39 }).video, 'decoded').grade, 'bad');
  const none = row(connectionRows({ decFps: 0 }).video, 'decoded');
  assert.equal(none.grade, '');
  assert.equal(none.value, '--');
});

test('the WebSocket fallback is called out', () => {
  assert.equal(row(connectionRows({ transport: 'websocket' }).link, 'transport').grade, 'warn');
});

test('problems appear only while they are above zero', () => {
  const { problems } = connectionRows({ ...healthy, shed: 3, dropped: 4, decodeErrors: 1, queue: 3 });
  assert.deepEqual(problems.map((r) => r.id), ['shed', 'dropped', 'decodeErrors', 'queue']);
  assert.equal(row(problems, 'shed').value, '3 / s');
  assert.equal(row(problems, 'decodeErrors').grade, 'bad');
});

test('one frame in the decoder queue is not a problem', () => {
  assert.deepEqual(connectionRows({ ...healthy, queue: 1 }).problems, []);
});

test('an empty snapshot renders dashes, not NaN or undefined', () => {
  const { link, video } = connectionRows();
  for (const r of [...link, ...video]) {
    assert.ok(!/NaN|undefined/.test(r.value), `${r.id} reads ${r.value}`);
  }
});

console.log(failures ? `\n${failures} failing` : '\nall passing');
process.exit(failures ? 1 : 0);
