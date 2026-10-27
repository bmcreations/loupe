// Tests for the fold button. Run: node web/fold.test.mjs
import assert from 'node:assert/strict';

const { installFakeDom, tick, jsonResponse, deferred } = await import('./fakedom.js');
const { mount, postureOf, postureLabel, foldMotion, slabTransform, FLAT } = await import('./fold.js');

let failures = 0;
// One at a time: every test shares the fake document's <html> class list.
const tests = [];
function test(name, fn) { tests.push([name, fn]); }

const duo = { id: 'posture', type: 'enum', value: 'opened', options: ['closed', 'opened'], supported: true };
const fold = { ...duo, options: ['closed', 'halfOpened', 'opened'] };

test('postureOf reads a supported posture with two options or more', () => {
  assert.deepEqual(postureOf({ settings: [{ id: 'darkMode', value: true, supported: true }, duo] }),
    { value: 'opened', options: ['closed', 'opened'] });
  assert.equal(postureOf({ settings: [{ ...duo, supported: false }] }), null);
  assert.equal(postureOf({ settings: [{ ...duo, options: ['opened'] }] }), null);
  assert.equal(postureOf({ settings: [] }), null);
  assert.equal(postureOf(null), null);
});

test('postureLabel names the three postures', () => {
  assert.deepEqual(['closed', 'halfOpened', 'opened'].map(postureLabel), ['Closed', 'Half open', 'Open']);
});

// rig mounts against the fake DOM with a fetch that answers from `answers`
// in order and records each request.
function rig(answers, opts = {}) {
  installFakeDom();
  document.documentElement.classList.remove('has-fold');
  const nav = document.createElement('nav');
  const wrap = document.createElement('div');
  const calls = [];
  let shapes = 0, changed = 0;
  const f = async (url, init = {}) => {
    calls.push({ url, method: init.method || 'GET', body: init.body });
    const a = answers.shift();
    if (a instanceof Error) throw a;
    return a && a.promise ? a.promise : a;
  };
  const handle = mount(nav, {
    fetch: f, targetId: () => 'sim:duo',
    onShape: () => shapes++, onChanged: () => changed++,
    ...(opts.surface ? { surface: opts.surface } : { wrap }),
  });
  const btn = nav.children[0], menu = nav.children[1];
  return { handle, btn, menu, wrap, calls, shapes: () => shapes, changed: () => changed };
}

test('a target with no posture keeps the stack at three rows', async () => {
  const r = rig([jsonResponse(501, { error: 'no device settings' })]);
  await r.handle.refresh(); await tick();
  assert.equal(r.btn.hidden, true);
  assert.equal(document.documentElement.classList.contains('has-fold'), false);
  assert.equal(r.shapes(), 0);
});

test('a Duo shows the button, grows the stack and lists closed and open', async () => {
  const r = rig([jsonResponse(200, { settings: [duo] })]);
  await r.handle.refresh(); await tick();
  assert.equal(r.btn.hidden, false);
  assert.equal(document.documentElement.classList.contains('has-fold'), true);
  assert.equal(r.shapes(), 1);
  assert.equal(r.btn.getAttribute('aria-label'), 'Fold: Open');
  r.btn.dispatch('click');
  assert.equal(r.menu.hidden, false);
  assert.equal(r.menu.children.length, 2);
  assert.deepEqual(r.menu.children.map((c) => c.getAttribute('aria-checked')), ['false', 'true']);
});

test('choosing closed PUTs the posture and tells the sheet', async () => {
  const r = rig([jsonResponse(200, { settings: [fold] }), jsonResponse(200, { settings: [{ ...fold, value: 'closed' }] })]);
  await r.handle.refresh(); await tick();
  r.btn.dispatch('click');
  r.menu.children[0].dispatch('click');
  await tick(3);
  assert.equal(r.calls[1].method, 'PUT');
  assert.equal(r.calls[1].url, '/api/settings?target=sim%3Aduo');
  assert.deepEqual(JSON.parse(r.calls[1].body), { changes: { posture: 'closed' } });
  assert.equal(r.btn.getAttribute('aria-label'), 'Fold: Closed');
  assert.equal(r.menu.hidden, true);
  assert.equal(r.changed(), 1);
});

test('foldMotion flips across the closed panel and tilts between the open ones', () => {
  assert.deepEqual(foldMotion('opened', 'closed'), { kind: 'flip', dir: 'closing' });
  assert.deepEqual(foldMotion('closed', 'opened'), { kind: 'flip', dir: 'opening' });
  assert.deepEqual(foldMotion('closed', 'halfOpened'), { kind: 'flip', dir: 'opening' });
  assert.deepEqual(foldMotion('opened', 'halfOpened'), { kind: 'tilt', dir: 'closing' });
  assert.deepEqual(foldMotion('halfOpened', 'opened'), { kind: 'tilt', dir: 'opening' });
  assert.equal(foldMotion('opened', 'opened'), null);
  assert.equal(foldMotion('', 'closed'), null);
});

test('slabTransform centres the shut half and moves it onto the other panel', () => {
  const book = { left: 0, top: 100, width: 800, height: 560 };
  // Shut in place: the left half, slid right by a quarter so it is centred.
  assert.equal(slabTransform(book, book), 'translate(0px, 0px) scale(1) translateX(-200px)');
  // Onto a taller portrait rect centred lower down.
  const to = { left: 250, top: 50, width: 300, height: 700 };
  assert.equal(slabTransform(book, to), 'translate(0px, 20px) scale(1.25) translateX(-200px)');
  assert.equal(FLAT, 'translate(0px, 0px) scale(1) translateX(0px)');
});

test('a flip turns away at once and back in when the new panel arrives', async () => {
  const put = deferred();
  const r = rig([jsonResponse(200, { settings: [duo] }), put]);
  await r.handle.refresh(); await tick();
  r.btn.dispatch('click');
  r.menu.children[0].dispatch('click');
  assert.equal(r.wrap.classList.contains('fold-away'), true);
  assert.equal(r.wrap.classList.contains('fold-flip'), true);
  assert.equal(r.wrap.dataset.fold, 'closing');
  // The other panel's picture lands while the PUT still waits.
  r.handle.reshaped();
  await new Promise((res) => setTimeout(res, 5));
  assert.equal(r.wrap.classList.contains('fold-away'), false);
  assert.equal(r.wrap.classList.contains('fold-back'), true);
  r.wrap.dispatch('animationend');
  assert.equal(r.wrap.classList.contains('fold-back'), false);
  assert.equal(r.wrap.classList.contains('fold-flip'), false);
  assert.equal(r.wrap.dataset.fold, undefined);
  put.resolve(jsonResponse(200, { settings: [{ ...duo, value: 'closed' }] }));
  await tick(3);
  assert.equal(r.wrap.classList.contains('fold-away'), false, 'the answer does not turn it again');
});

test('a reshape with no fold in flight leaves the box alone', async () => {
  const r = rig([jsonResponse(200, { settings: [duo] })]);
  await r.handle.refresh(); await tick();
  r.handle.reshaped();
  await new Promise((res) => setTimeout(res, 5));
  assert.equal(r.wrap.classList.contains('fold-back'), false);
});

test('a refused fold turns straight back', async () => {
  const r = rig([jsonResponse(200, { settings: [duo] }), jsonResponse(409, { error: 'posture: still opened' })]);
  await r.handle.refresh(); await tick();
  r.btn.dispatch('click');
  r.menu.children[0].dispatch('click');
  await tick(3);
  assert.equal(r.wrap.classList.contains('fold-away'), false);
  assert.equal(r.wrap.classList.contains('fold-back'), true);
});

test('a refused fold puts the old posture back', async () => {
  const r = rig([jsonResponse(200, { settings: [duo] }), jsonResponse(409, { error: 'posture: still opened' })]);
  await r.handle.refresh(); await tick();
  r.btn.dispatch('click');
  r.menu.children[0].dispatch('click');
  assert.equal(r.btn.getAttribute('aria-label'), 'Fold: Closed', 'shown at once');
  await tick(3);
  assert.equal(r.btn.getAttribute('aria-label'), 'Fold: Open');
  assert.equal(r.changed(), 0);
});

test('switching to a target without a posture removes the row again', async () => {
  const r = rig([jsonResponse(200, { settings: [duo] }), jsonResponse(501, {})]);
  await r.handle.refresh(); await tick();
  await r.handle.refresh(); await tick();
  assert.equal(r.btn.hidden, true);
  assert.equal(document.documentElement.classList.contains('has-fold'), false);
  assert.equal(r.shapes(), 2);
});

test('the motion lands on the focused pane\'s box, read when it starts', async () => {
  const other = { wrap: null };
  const r = rig([jsonResponse(200, { settings: [duo] }), deferred()], { surface: () => ({ wrap: other.wrap }) });
  other.wrap = document.createElement('div');
  await r.handle.refresh(); await tick();
  r.btn.dispatch('click');
  r.menu.children[0].dispatch('click');
  assert.equal(other.wrap.classList.contains('fold-away'), true);
  const first = other.wrap;
  other.wrap = document.createElement('div');
  r.handle.reshaped();
  await new Promise((res) => setTimeout(res, 5));
  assert.equal(first.classList.contains('fold-back'), true);
  assert.equal(other.wrap.classList.contains('fold-back'), false);
});

for (const [name, fn] of tests) {
  try { await fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}
if (failures) { console.log(`${failures} failed`); process.exit(1); }
console.log('all passed');

