// Tests for the Event log page's Gestures / Logcat switch (eventlog.js
// mountLogPage). Run: node web/logpage.test.mjs
import assert from 'node:assert/strict';
import { installFakeDom, tick, deferred } from './fakedom.js';

installFakeDom();
const { mountLogPage } = await import('./eventlog.js');

let failures = 0;
const pending = [];
function test(name, fn) {
  pending.push((async () => {
    try { await fn(); console.log(`  PASS ${name}`); }
    catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
  })());
}

function build(fetchImpl) {
  const page = document.createElement('div');
  const list = document.createElement('div');
  const calls = [];
  const api = mountLogPage(page, {
    targetId: () => 'emulator-5554',
    fetch: (url) => { calls.push(url); return fetchImpl(url); },
  });
  api.setList(list);
  return { page, list, api, calls };
}
const ok = (text) => async () => ({ ok: true, text: async () => text });
const seg = (page, value) => page.querySelectorAll('.settingSegment').find((b) => b.dataset.value === value);
const out = (page) => page.querySelector('.logcatText');

test('the switch is hidden off Android and the gesture list shows', () => {
  const { page, list, api } = build(ok(''));
  api.setAndroid(false);
  assert.equal(page.querySelector('.logcatSwitch').hidden, true);
  assert.equal(list.parentNode.hidden, false);
  assert.equal(page.querySelector('.logcatBody').hidden, true);
});

test('on Android Logcat fetches 300 lines for the target, Loading... first', async () => {
  const d = deferred();
  const { page, api, calls } = build(() => d.promise);
  api.setAndroid(true);
  assert.equal(page.querySelector('.logcatSwitch').hidden, false);
  seg(page, 'logcat').dispatch('click');
  assert.deepEqual(calls, ['/api/logcat?target=emulator-5554&lines=300']);
  assert.equal(out(page).textContent, 'Loading…');
  d.resolve({ ok: true, text: async () => 'line one\nline two' });
  await tick(2);
  assert.equal(out(page).textContent, 'line one\nline two');
});

test('Reload fetches again', async () => {
  const { page, api, calls } = build(ok('x'));
  api.setAndroid(true);
  seg(page, 'logcat').dispatch('click');
  await tick(2);
  page.querySelector('.logcatReload').dispatch('click');
  assert.equal(calls.length, 2);
});

test('an error or a non-2xx answer reads "Could not read logcat."', async () => {
  for (const impl of [async () => { throw new Error('net'); }, async () => ({ ok: false, text: async () => 'no' })]) {
    const { page, api } = build(impl);
    api.setAndroid(true);
    seg(page, 'logcat').dispatch('click');
    await tick(2);
    assert.equal(out(page).textContent, 'Could not read logcat.');
  }
});

test('losing Android returns to the gestures view', () => {
  const { page, list, api } = build(ok('x'));
  api.setAndroid(true);
  seg(page, 'logcat').dispatch('click');
  api.setAndroid(false);
  assert.equal(page.querySelector('.logcatBody').hidden, true);
  assert.equal(list.parentNode.hidden, false);
});

await Promise.all(pending);
console.log(failures ? `\n${failures} failing` : '\nall passing');
process.exit(failures ? 1 : 0);
