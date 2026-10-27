// Tests for the peek window. Run: node web/peek.test.mjs
//
// The bezel is pixel_10's skin, 1198x2531, the one /api/frame answers for
// emulator-5554. The screen is a 14in MacBook Pro's available area at its
// default scale, 1512x944 less the menu bar.
import assert from 'node:assert/strict';

const { isPeek, peekURL, windowName, fitWindow, mount } = await import('./peek.js');

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

const PIXEL10 = { width: 1198, height: 2531 };
const MAC = { width: 1512, height: 919 };

function fakeWin({ search = '', hover = true, open } = {}) {
  const calls = { open: [], resize: [] };
  const win = {
    location: { search, origin: 'https://mac.test:18456' },
    outerWidth: 1200, innerWidth: 1200, outerHeight: 900, innerHeight: 872,
    screen: { availWidth: MAC.width, availHeight: MAC.height },
    matchMedia: () => ({ matches: hover }),
    open: (...a) => { calls.open.push(a); return open === undefined ? { focus() {} } : open; },
    resizeTo: (w, h) => calls.resize.push([w, h]),
  };
  return { win, calls };
}

test('isPeek reads the query parameter alone', () => {
  assert.equal(isPeek('?peek=1'), true);
  assert.equal(isPeek('?target=avd:x&peek'), true);
  assert.equal(isPeek('?peeking=1'), false);
  assert.equal(isPeek(''), false);
});

test('peekURL carries the target and the flag', () => {
  assert.equal(peekURL('https://h', 'avd:Pixel_10'), 'https://h/?target=avd%3APixel_10&peek=1');
});

test('windowName is one window per target and a valid name', () => {
  assert.equal(windowName('sim:AB-12'), 'loupe-peek-sim_AB-12');
  assert.notEqual(windowName('avd:a'), windowName('avd:b'));
});

test('fitWindow keeps the bezel shape inside the screen share', () => {
  const s = fitWindow(PIXEL10, MAC, { width: 0, height: 28 });
  assert.ok(s.height <= MAC.height * 0.85 + 1, `height ${s.height}`);
  const ratio = s.width / (s.height - 28);
  assert.ok(Math.abs(ratio - PIXEL10.width / PIXEL10.height) < 0.01, `ratio ${ratio}`);
});

test('fitWindow refuses sizes it cannot use', () => {
  assert.equal(fitWindow({ width: 0, height: 1 }, MAC), null);
  assert.equal(fitWindow(PIXEL10, { width: 0, height: 0 }), null);
  assert.equal(fitWindow(PIXEL10, MAC, { width: 5000, height: 0 }), null);
});

test('the item is offered on a Mac pointer only, and never inside a peek', () => {
  assert.equal(mount(fakeWin().win, { targetId: () => 'a', report() {} }).offered(), true);
  assert.equal(mount(fakeWin({ hover: false }).win, { targetId: () => 'a', report() {} }).offered(), false);
  assert.equal(mount(fakeWin({ search: '?peek=1' }).win, { targetId: () => 'a', report() {} }).offered(), false);
});

test('open names the window after the target and reports', () => {
  const { win, calls } = fakeWin();
  const recs = [];
  mount(win, { targetId: () => 'avd:Pixel_10', report: (r) => recs.push(r) }).open();
  assert.equal(calls.open.length, 1);
  assert.equal(calls.open[0][1], 'loupe-peek-avd_Pixel_10');
  assert.match(calls.open[0][2], /^popup,width=\d+,height=\d+$/);
  assert.deepEqual(recs, [{ event: 'peek-open', target: 'avd:Pixel_10', outcome: 'ok' }]);
});

test('a blocked pop-up throws and is reported', () => {
  const { win } = fakeWin({ open: null });
  const recs = [];
  assert.throws(() => mount(win, { targetId: () => 't', report: (r) => recs.push(r) }).open(), /blocked/);
  assert.equal(recs[0].outcome, 'blocked');
});

test('sizeTo resizes once per shape, again on a rotation', () => {
  const { win, calls } = fakeWin({ search: '?peek=1' });
  const p = mount(win, { targetId: () => 't', report() {} });
  p.sizeTo(PIXEL10, { width: 24, height: 124 });
  p.sizeTo(PIXEL10, { width: 24, height: 124 });
  p.sizeTo({ width: 1198, height: 2531 }, { width: 30, height: 130 });
  assert.equal(calls.resize.length, 1);
  p.sizeTo({ width: 2531, height: 1198 }, { width: 24, height: 124 });
  assert.equal(calls.resize.length, 2);
  const [w, h] = calls.resize[1];
  assert.ok(w > h, `landscape ${w}x${h}`);
});

test('sizeTo does nothing outside a peek', () => {
  const { win, calls } = fakeWin();
  mount(win, { targetId: () => 't', report() {} }).sizeTo(PIXEL10, {});
  assert.equal(calls.resize.length, 0);
});

if (failures) { console.log(`\n${failures} failed`); process.exit(1); }
console.log('\nall passed');
