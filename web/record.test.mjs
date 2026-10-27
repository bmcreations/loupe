import assert from 'node:assert/strict';
import { test } from 'node:test';
import { adoptable, elapsed, endMessage, progress, mount } from './record.js';
import { fakeDocument, FakeElement, jsonResponse, tick } from './fakedom.js';

test('elapsed reads m:ss from milliseconds, flooring the seconds', () => {
  assert.equal(elapsed(0), '0:00');
  assert.equal(elapsed(999), '0:00');
  assert.equal(elapsed(65_400), '1:05');
  assert.equal(elapsed(600_000), '10:00');
});

test('elapsed never reads negative, from a clock that stepped back', () => {
  assert.equal(elapsed(-500), '0:00');
});

test('each end reason has its own message, and an unknown one a plain one', () => {
  assert.equal(endMessage('stopped'), 'Recording ready');
  assert.equal(endMessage('limit'), 'Recording reached its limit');
  assert.equal(endMessage('resized'), 'Stopped when the screen rotated');
  assert.equal(endMessage('fellBehind'), 'Stopped: the disk fell behind');
  assert.equal(endMessage('sessionEnded'), 'Stopped when the stream ended');
  assert.equal(endMessage('writeError'), 'Stopped: could not write the file');
  assert.equal(endMessage('somethingNew'), 'Recording stopped');
  assert.equal(endMessage(undefined), 'Recording stopped');
});

test('a recording with no frames says so, whatever its reason', () => {
  assert.equal(endMessage('stopped', 0), 'Nothing was recorded');
  assert.equal(endMessage('sessionEnded', 0), 'Nothing was recorded');
  assert.equal(endMessage('stopped', 3), 'Recording ready');
  assert.equal(endMessage('stopped', undefined), 'Recording ready');
});

test('progress shows megabytes once the size is known', () => {
  assert.equal(progress(0, 0), 'Preparing…');
  assert.equal(progress(5_000_000, 20_000_000), 'Preparing… 5 of 20 MB');
  assert.equal(progress(1_234_567, 0), 'Preparing…');
});

test('adoptable takes only the in-progress recording this tab stored, on this target', () => {
  const mine = { id: 'a', target: 't1', inProgress: true };
  const list = [
    { id: 'b', target: 't1', inProgress: true },
    mine,
    { id: 'a', target: 't2', inProgress: true },
  ];
  assert.equal(adoptable(list, 't1', 'a'), mine);
  assert.equal(adoptable(list, 't1', 'zz'), null);
  assert.equal(adoptable(list, 't1', ''), null);
  assert.equal(adoptable([{ ...mine, inProgress: false }], 't1', 'a'), null);
  assert.equal(adoptable(null, 't1', 'a'), null);
});

function recMount(seed = {}, target = 'emulator-5554', extra = {}) {
  const store = new Map(Object.entries(seed));
  const sessionStorage = {
    getItem: (k) => (store.has(k) ? store.get(k) : null),
    setItem: (k, v) => { store.set(k, String(v)); },
    removeItem: (k) => { store.delete(k); },
  };
  const win = {
    sessionStorage, performance: { now: () => 0 },
    setInterval: () => 1, clearInterval() {},
    URL: { revokeObjectURL() {} }, navigator: {},
  };
  const doc = Object.create(fakeDocument);
  doc.defaultView = win;
  const host = new FakeElement('div');
  const state = { target };
  const calls = [];
  const fetch = async (url, init) => {
    calls.push(url);
    if (init && init.method === 'POST') return jsonResponse(201, { id: 'r1' });
    return jsonResponse(200, [{ id: 'old', target, inProgress: true, started: new Date().toISOString() }]);
  };
  const pillRec = new FakeElement('span');
  pillRec.hidden = true;
  const rec = mount({ doc, host, fetch, targetId: () => state.target, report() {}, toast: new FakeElement('div'), pillRec, ...extra });
  return { rec, store, host, state, calls, pillRec };
}

test('the stored id is per target, and the banner goes in the host', async () => {
  const { rec, store, host } = recMount();
  assert.ok(host.querySelector('.recBanner'));
  assert.equal(host.querySelector('.recBanner').id, undefined);
  await rec.toggle();
  assert.equal(store.get('loupe.recordId.emulator-5554'), 'r1');
  assert.equal(store.has('loupe.recordId'), false);
});

test('a legacy loupe.recordId is adopted once and removed', async () => {
  const { rec, store } = recMount({ 'loupe.recordId': 'old' });
  await rec.sync();
  await tick(2);
  assert.equal(store.has('loupe.recordId'), false);
  assert.equal(store.get('loupe.recordId.emulator-5554'), 'old');
  assert.equal(rec.recording(), true);
});

test('the id is read for the target in front at the moment of use', async () => {
  const { rec, store, state } = recMount({ 'loupe.recordId.emulator-5554': 'old' });
  state.target = 'emulator-5556';
  await rec.sync();
  await tick(2);
  assert.equal(rec.recording(), false);
  assert.equal(store.get('loupe.recordId.emulator-5554'), 'old');
});

test('only the pane in front shows its recording on the pill', async () => {
  const front = { on: false };
  const { rec, pillRec } = recMount({}, 'emulator-5554', { isFront: () => front.on });
  await rec.toggle();
  assert.equal(rec.recording(), true);
  assert.equal(pillRec.hidden, true);
  front.on = true;
  rec.repaint();
  assert.equal(pillRec.hidden, false);
  front.on = false;
  rec.repaint();
  assert.equal(pillRec.hidden, true);
});
