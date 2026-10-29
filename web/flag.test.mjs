import assert from 'node:assert/strict';
import { test } from 'node:test';
import { fractionAt, regionOf, requestBody, flagShortcut, mount, MAX_NOTE } from './flag.js';
import { fakeDocument, FakeElement, jsonResponse, tick } from './fakedom.js';

test('fractionAt maps a point against the picture rect and holds it to the picture', () => {
  const rect = { left: 50, top: 10, width: 200, height: 400 };
  assert.deepEqual(fractionAt(rect, 150, 210), [0.5, 0.5]);
  assert.deepEqual(fractionAt(rect, 0, 1000), [0, 1]);
  assert.deepEqual(fractionAt({ left: 0, top: 0, width: 0, height: 0 }, 5, 5), [0, 0]);
});

test('regionOf orders the corners and refuses a box with no area', () => {
  assert.deepEqual(regionOf([0.75, 0.75], [0.25, 0.25]), [0.25, 0.25, 0.75, 0.75]);
  assert.equal(regionOf([0.5, 0.2], [0.5, 0.8]), null);
  assert.equal(regionOf([0.2, 0.5], [0.8, 0.5]), null);
});

test('requestBody trims the note and leaves out what was not given', () => {
  assert.deepEqual(requestBody({ note: '  odd  ', region: [0, 0.1, 0.5, 0.9], watch: true }),
    { note: 'odd', region: [0, 0.1, 0.5, 0.9], watch: true });
  assert.deepEqual(requestBody({ note: '   ' }), {});
  assert.equal(Array.from(requestBody({ note: 'x'.repeat(MAX_NOTE + 5) }).note).length, MAX_NOTE);
});

test('flagShortcut is Alt+Meta+F or Alt+Ctrl+F by key position', () => {
  assert.equal(flagShortcut({ altKey: true, metaKey: true, code: 'KeyF' }), true);
  assert.equal(flagShortcut({ altKey: true, ctrlKey: true, code: 'KeyF' }), true);
  assert.equal(flagShortcut({ altKey: true, code: 'KeyF' }), false);
  assert.equal(flagShortcut({ metaKey: true, code: 'KeyF' }), false);
  assert.equal(flagShortcut({ altKey: true, metaKey: true, shiftKey: true, code: 'KeyF' }), false);
  assert.equal(flagShortcut({ altKey: true, metaKey: true, code: 'KeyG' }), false);
});

// A 300x500 layer with the picture letterboxed inside it: 200 wide, 400 tall,
// 50 in from the left and 20 down.
function setup({ post = async () => jsonResponse(201, { id: 7, watching: true }), headers = { 'X-Loupe-Viewer': '3' } } = {}) {
  const layer = new FakeElement('div');
  layer._rect = { left: 0, top: 0, width: 300, height: 500 };
  const host = new FakeElement('div');
  const toast = new FakeElement('div');
  toast.hidden = true;
  const picture = { left: 50, top: 20, width: 200, height: 400 };
  const calls = [];
  const states = [];
  const flag = mount(layer, {
    doc: fakeDocument,
    host,
    targetId: () => 'emulator-5554',
    pictureRect: () => picture,
    fetch: async (url, init) => { calls.push({ url, init, body: init.body ? JSON.parse(init.body) : null }); return post(url, init); },
    headers: () => headers,
    toast,
    report() {},
    onToggle: (on) => states.push(on),
  });
  const q = (sel) => host.querySelector(sel) || layer.querySelector(sel);
  const press = (type, x, y, id = 1) => layer.dispatch(type, { clientX: x, clientY: y, pointerId: id, button: 0 });
  const drag = (x0, y0, x1, y1, id = 1) => { press('pointerdown', x0, y0, id); press('pointermove', x1, y1, id); press('pointerup', x1, y1, id); };
  return { flag, layer, host, toast, calls, states, q, press, drag };
}

test('a drag draws a box in picture fractions, through the letterboxing', () => {
  const s = setup();
  s.flag.run();
  assert.equal(s.layer.hidden, false);
  // Left of the picture is clamped to its edge; (150, 220) is 0.5, 0.5.
  s.drag(150, 220, 250, 420);
  const box = s.q('.flagBox');
  assert.equal(box.hidden, false);
  assert.equal(box.style.left, '50%');
  assert.equal(box.style.top, '50%');
  assert.equal(box.style.width, '50%');
  assert.equal(box.style.height, '50%');
  // The picture clip is the canvas rect as percentages of the layer.
  const pic = s.q('.flagPic');
  assert.equal(pic.style.left, `${(50 / 300) * 100}%`);
  assert.equal(pic.style.width, `${(200 / 300) * 100}%`);
  assert.equal(s.q('.flagHint').hidden, true);
});

test('a drag that leaves the picture is held to its edge', async () => {
  const s = setup();
  s.flag.run();
  s.drag(0, 0, 150, 220);
  s.q('.flagSend').dispatch('click');
  await tick();
  assert.deepEqual(s.calls[0].body.region, [0, 0, 0.5, 0.5]);
});

test('a click without a drag clears the box and brings the hint back', () => {
  const s = setup();
  s.flag.run();
  s.drag(100, 100, 200, 300);
  assert.equal(s.q('.flagBox').hidden, false);
  assert.equal(s.q('.flagBtn').hidden, false, 'Clear Box shows with a box');
  s.press('pointerdown', 120, 120);
  s.press('pointerup', 121, 121);
  assert.equal(s.q('.flagBox').hidden, true);
  assert.equal(s.q('.flagHint').hidden, false);
  assert.equal(s.q('.flagBtn').hidden, true, 'Clear Box hides with no box');
});

test('Clear Box removes the box', () => {
  const s = setup();
  s.flag.run();
  s.drag(100, 100, 200, 300);
  s.q('.flagBtn').dispatch('click');
  assert.equal(s.q('.flagBox').hidden, true);
});

test('Send posts the trimmed note, region and watch as the viewer, then closes with a toast', async () => {
  const s = setup({ post: async () => jsonResponse(201, { id: 7, watching: false }) });
  s.flag.run();
  s.drag(50, 20, 150, 220);
  const note = s.q('.flagNote');
  note.value = '  button is clipped  ';
  s.q('.flagSend').dispatch('click');
  await tick();
  assert.equal(s.calls.length, 1);
  assert.equal(s.calls[0].url, '/api/flags?target=emulator-5554');
  assert.equal(s.calls[0].init.method, 'POST');
  assert.equal(s.calls[0].init.headers['X-Loupe-Viewer'], '3');
  assert.deepEqual(s.calls[0].body, { note: 'button is clipped', region: [0, 0, 0.5, 0.5] });
  assert.equal(s.flag.on(), false);
  assert.equal(s.layer.hidden, true);
  assert.equal(s.toast.textContent, 'Flag sent');
  assert.deepEqual(s.states, [true, false]);
});

test('Enter sends, Shift+Enter does not', async () => {
  const s = setup();
  s.flag.run();
  let prevented = false;
  s.q('.flagNote').dispatch('keydown', { key: 'Enter', shiftKey: true, preventDefault() { prevented = true; } });
  await tick();
  assert.equal(s.calls.length, 0);
  assert.equal(prevented, false);
  s.q('.flagNote').dispatch('keydown', { key: 'Enter', preventDefault() { prevented = true; } });
  await tick();
  assert.equal(s.calls.length, 1);
  assert.equal(prevented, true);
});

test('Escape and Cancel leave flag mode without posting', () => {
  const s = setup();
  s.flag.run();
  fakeDocument.dispatch('keydown', { key: 'Escape' });
  assert.equal(s.flag.on(), false);
  s.flag.run();
  const cancel = s.host.all().find((e) => e.textContent === 'Cancel');
  cancel.dispatch('click');
  assert.equal(s.flag.on(), false);
  assert.equal(s.calls.length, 0);
});

test('a failed POST keeps the panel and the box and shows the error', async () => {
  const s = setup({ post: async () => jsonResponse(409, { error: 'no such target' }) });
  s.flag.run();
  s.drag(100, 100, 200, 300);
  s.q('.flagSend').dispatch('click');
  await tick();
  assert.equal(s.flag.on(), true);
  assert.equal(s.q('.flagPanel').hidden, false);
  assert.equal(s.q('.flagError').hidden, false);
  assert.match(s.q('.flagError').textContent, /no such target/);
  assert.equal(s.q('.flagBox').hidden, false);
  assert.equal(s.q('.flagSend').disabled, false, 'Send works again');
});

test('Send and Watch leaves flag mode and shows the pill; Stop posts /api/flags/end', async () => {
  const s = setup();
  s.flag.run();
  const check = s.q('input');
  check.checked = true;
  check.dispatch('change');
  assert.equal(s.q('.flagSend').textContent, 'Send and Watch');
  s.q('.flagSend').dispatch('click');
  await tick();
  assert.equal(s.calls[0].body.watch, true);
  assert.equal(s.flag.on(), false, 'touches go to the device again');
  assert.equal(s.layer.hidden, true);
  assert.equal(s.q('.flagPill').hidden, false);
  assert.equal(s.flag.watching(), true);
  assert.equal(s.toast.hidden, true, 'no toast while the watch runs');

  s.q('.flagStop').dispatch('click');
  await tick();
  assert.equal(s.calls[1].url, '/api/flags/end?id=7');
  assert.equal(s.calls[1].init.method, 'POST');
  assert.equal(s.calls[1].init.headers['X-Loupe-Viewer'], '3');
  assert.equal(s.q('.flagPill').hidden, true);
  assert.equal(s.flag.watching(), false);
});

test('a failed Stop keeps the pill so it can be tried again', async () => {
  let n = 0;
  const s = setup({ post: async () => (n++ === 0 ? jsonResponse(201, { id: 9, watching: true }) : jsonResponse(500, { error: 'boom' })) });
  s.flag.run();
  s.q('input').checked = true;
  s.q('.flagSend').dispatch('click');
  await tick();
  s.q('.flagStop').dispatch('click');
  await tick();
  assert.equal(s.q('.flagPill').hidden, false);
  assert.equal(s.flag.watching(), true);
  assert.match(s.toast.textContent, /boom/);
});

test('a drag does nothing while flag mode is off', () => {
  const s = setup();
  s.drag(100, 100, 200, 300);
  assert.equal(s.q('.flagBox').hidden, true);
});
