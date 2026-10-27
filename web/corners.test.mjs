// Tests for the corner clusters. Run: node web/corners.test.mjs
//
// navKeys, errorMessage and shouldDismissMenu are pure and pinned directly.
// mount() itself is exercised too, against web/fakedom.js's fake DOM --
// added for client Task 9's review, which found that a pure-function suite
// alone left the a11y wiring (menu roles, arrow-key navigation, focus
// return) and the nav-L visibility calls completely unmutation-tested: 18 of
// 28 mutations the reviewer tried against these modules survived, several of
// them in corners.js's own mount().
import assert from 'node:assert/strict';

const { navKeys, errorMessage, shouldDismissMenu, pageLink } = await import('./corners.js');

let failures = 0;
// test() has to accommodate the mount()-level tests added for client Task 9's
// review (corners.js/appcard.js/devicesettings.js/inspect.js all fetch, so
// their mount() tests are async): a bare `try { fn(); }` never awaits an
// async fn, so a rejected assertion inside one used to be silently missed
// and the run still printed PASS -- caught only because the review's own
// probe.mjs disagreed with a suite that claimed everything was green. Each
// test's promise is collected and the run waits for all of them before
// deciding pass/fail, rather than awaiting inline, so a slow async test does
// not block sync ones from logging in order.
const pending = [];
function test(name, fn) {
  pending.push((async () => {
    try { await fn(); console.log(`  PASS ${name}`); }
    catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
  })());
}

// --- navKeys -----------------------------------------------------------

test('android gets back/home/recents at upright/elbow/floor', () => {
  const keys = navKeys('android');
  assert.deepEqual(keys.map((k) => k.slot), ['upright', 'elbow', 'floor']);
  assert.deepEqual(keys.map((k) => k.button), ['back', 'home', 'recents']);
});

test('ios gets appSwitcher/home/lock at upright/elbow/floor', () => {
  const keys = navKeys('ios');
  assert.deepEqual(keys.map((k) => k.slot), ['upright', 'elbow', 'floor']);
  assert.deepEqual(keys.map((k) => k.button), ['appSwitcher', 'home', 'lock']);
});

test('both OS layouts put home at the elbow', () => {
  const androidElbow = navKeys('android').find((k) => k.slot === 'elbow');
  const iosElbow = navKeys('ios').find((k) => k.slot === 'elbow');
  assert.equal(androidElbow.button, 'home');
  assert.equal(iosElbow.button, 'home');
});

test('ios has no back -- app switcher takes the upright slot instead', () => {
  const keys = navKeys('ios');
  assert.equal(keys.some((k) => k.button === 'back'), false);
});

test('android keys carry the MsgKey keycode: back 4, home 3, recents 187', () => {
  const byButton = Object.fromEntries(navKeys('android').map((k) => [k.button, k.keycode]));
  assert.equal(byButton.back, 4);
  assert.equal(byButton.home, 3);
  assert.equal(byButton.recents, 187);
});

test('ios keys carry no keycode -- they go through /api/input, not MsgKey', () => {
  for (const key of navKeys('ios')) {
    assert.equal('keycode' in key, false);
  }
});

test('synthetic has no OS and gets an empty L', () => {
  assert.deepEqual(navKeys('synthetic'), []);
});

test('an unrecognised OS gets an empty L rather than throwing', () => {
  assert.deepEqual(navKeys('palmos'), []);
});

test('no target selected (undefined os) gets an empty L', () => {
  assert.deepEqual(navKeys(undefined), []);
});

// --- errorMessage --------------------------------------------------------

test('errorMessage reads an Error object\'s message', () => {
  assert.equal(errorMessage(new Error('rotate: 501 not implemented')), 'rotate: 501 not implemented');
});

test('errorMessage stringifies a plain string throw', () => {
  assert.equal(errorMessage('boom'), 'boom');
});

test('errorMessage stringifies a non-Error, non-string throw', () => {
  assert.equal(errorMessage(404), '404');
});

test('errorMessage falls back to String() for an Error with an empty message', () => {
  assert.equal(errorMessage(new Error('')), String(new Error('')));
});

// --- shouldDismissMenu ---------------------------------------------------

function fakeEl(contains) {
  return { contains };
}

test('a closed menu is never dismissed', () => {
  const menuEl = fakeEl(() => false);
  const toggleEl = fakeEl(() => false);
  assert.equal(
    shouldDismissMenu({ open: false, menuEl, toggleEl, target: {} }),
    false,
  );
});

test('a pointerdown inside the menu does not dismiss it', () => {
  const target = {};
  const menuEl = fakeEl((t) => t === target);
  const toggleEl = fakeEl(() => false);
  assert.equal(
    shouldDismissMenu({ open: true, menuEl, toggleEl, target }),
    false,
  );
});

test('a pointerdown on the toggle button does not dismiss it (it handles its own toggle)', () => {
  const target = {};
  const menuEl = fakeEl(() => false);
  const toggleEl = fakeEl((t) => t === target);
  assert.equal(
    shouldDismissMenu({ open: true, menuEl, toggleEl, target }),
    false,
  );
});

test('a pointerdown outside both dismisses an open menu -- this is what lets a tap on the canvas close it', () => {
  const menuEl = fakeEl(() => false);
  const toggleEl = fakeEl(() => false);
  assert.equal(
    shouldDismissMenu({ open: true, menuEl, toggleEl, target: {} }),
    true,
  );
});

test('a missing menuEl/toggleEl does not throw -- an open menu with nothing to check against still dismisses', () => {
  assert.equal(
    shouldDismissMenu({ open: true, menuEl: null, toggleEl: null, target: {} }),
    true,
  );
});

// --- mount() -----------------------------------------------------------

const { installFakeDom, tick } = await import('./fakedom.js');
const { mount } = await import('./corners.js');

// buildDom() reproduces index.html's #cornerNav/#cornerMore markup closely
// enough for mount() to find everything it queries for (querySelector('#id'
// / '.cornerBtn[data-slot]' / '[role="menuitem"]')).
function buildDom() {
  const reset = installFakeDom();
  const doc = globalThis.document;
  const nav = doc.createElement('nav');
  nav.setAttribute('id', 'cornerNav');
  nav.hidden = true;
  for (const slot of ['upright', 'elbow', 'floor']) {
    const btn = doc.createElement('button');
    btn.className = 'cornerBtn';
    btn.dataset.slot = slot;
    btn.setAttribute('data-slot', slot);
    btn.hidden = true;
    nav.appendChild(btn);
  }
  const more = doc.createElement('div');
  more.setAttribute('id', 'cornerMore');
  const moreBtn = doc.createElement('button');
  moreBtn.setAttribute('id', 'moreBtn');
  moreBtn.setAttribute('aria-haspopup', 'true');
  moreBtn.setAttribute('aria-expanded', 'false'); // index.html ships this; mount() only ever toggles it
  more.appendChild(moreBtn);
  const moreMenu = doc.createElement('div');
  moreMenu.setAttribute('id', 'moreMenu');
  moreMenu.hidden = true;
  more.appendChild(moreMenu);
  const toast = doc.createElement('div');
  toast.hidden = true;
  return { reset, nav, more, moreBtn, moreMenu, toast };
}

test('setOS + setControl both have to be true for the L to show', () => {
  const { nav, more } = buildDom();
  const api = mount(nav, more, { sendAndroidKey: async () => {}, pressIOSButton: async () => {}, screenshot: async () => {}, rotate: async () => {} });
  api.setOS('android');
  assert.equal(nav.hidden, true, 'os set but no control: still hidden');
  api.setControl(true);
  assert.equal(nav.hidden, false, 'os + control: visible');
  api.setControl(false);
  assert.equal(nav.hidden, true, 'control revoked (disconnect/switch): hidden again');
});

test('setOS(undefined) at mount time leaves the L hidden with no keys', () => {
  const { nav, more } = buildDom();
  const api = mount(nav, more, {});
  api.setControl(true);
  assert.equal(nav.hidden, true, 'no OS yet -- navKeys() is empty');
});

test('the toggle button carries aria-controls/aria-expanded and the menu carries role=menu', () => {
  const { nav, more, moreBtn, moreMenu } = buildDom();
  mount(nav, more, {});
  assert.equal(moreMenu.getAttribute('role'), 'menu');
  assert.equal(moreBtn.getAttribute('aria-controls'), 'moreMenu');
  assert.equal(moreBtn.getAttribute('aria-expanded'), 'false');
  moreBtn.dispatch('click');
  assert.equal(moreBtn.getAttribute('aria-expanded'), 'true');
});

test('menu items carry role=menuitem, one per action, inspect included only when deps.inspect is given', () => {
  const { nav, more, moreMenu } = buildDom();
  mount(nav, more, { inspect: { label: 'Inspect', run() {} } });
  const items = moreMenu.querySelectorAll('[role="menuitem"]');
  // Top to bottom: the menu opens upwards, so the first item is the last row.
  assert.deepEqual(items.map((i) => i.textContent), ['Inspect', 'Rotate', 'Screenshot']);
});

test('the menu is grouped, and the clipboard group hides with its items', () => {
  const { nav, more, moreBtn, moreMenu } = buildDom();
  let offered = false;
  mount(nav, more, {
    fullScreen() {},
    inspect: { label: 'Inspect', run() {} },
    touches: { pref: () => false, setPref() {} },
    clipboard: { offered: () => offered, paste() {}, copy() {} },
  });
  const groups = moreMenu.querySelectorAll('[role="group"]');
  const labels = (g) => g.querySelectorAll('.moreMenuItem').map((i) => i.textContent);
  assert.deepEqual(groups.map(labels), [
    ['Copy from device', 'Paste to device'],
    ['Show touches', 'Inspect'],
    ['Full screen', 'Rotate', 'Screenshot'],
  ]);
  moreBtn.dispatch('click');
  assert.equal(groups[0].hidden, true);
  moreBtn.dispatch('click');
  offered = true;
  moreBtn.dispatch('click');
  assert.equal(groups[0].hidden, false);
});

test('pressing Paste to device fades the menu before its click raises the Paste callout', () => {
  const { nav, more, moreBtn, moreMenu } = buildDom();
  let pastes = 0;
  mount(nav, more, { clipboard: { offered: () => true, paste() { pastes++; }, copy() {} } });
  moreBtn.dispatch('click');
  const items = moreMenu.querySelectorAll('[role="menuitem"]');
  const paste = items.find((i) => i.textContent === 'Paste to device');
  const shot = items.find((i) => i.textContent === 'Screenshot');
  shot.dispatch('pointerdown');
  assert.equal(moreMenu.classList.contains('pressing'), false, 'only an item that raises a callout fades');
  paste.dispatch('pointerdown');
  assert.equal(moreMenu.classList.contains('pressing'), true, 'faded on press, a frame before the click');
  paste.dispatch('pointercancel');
  assert.equal(moreMenu.classList.contains('pressing'), false, 'a cancelled press brings it back');
  paste.dispatch('pointerdown');
  paste.dispatch('click');
  assert.equal(pastes, 1);
  assert.equal(moreMenu.hidden, true);
  moreBtn.dispatch('click');
  assert.equal(moreMenu.classList.contains('pressing'), false, 'reopening is not left faded');
});

test('opening the menu focuses the row nearest the button, the bottom one', () => {
  const { nav, more, moreBtn, moreMenu } = buildDom();
  mount(nav, more, {});
  moreBtn.dispatch('click');
  const items = moreMenu.querySelectorAll('[role="menuitem"]');
  assert.equal(document.activeElement, items[items.length - 1]);
  assert.equal(document.activeElement.textContent, 'Screenshot');
});

test('ArrowDown/ArrowUp move focus among menu items and wrap at both ends', () => {
  const { nav, more, moreBtn, moreMenu } = buildDom();
  mount(nav, more, {});
  moreBtn.dispatch('click');
  const items = moreMenu.querySelectorAll('[role="menuitem"]');
  const last = items.length - 1;
  document.dispatch('keydown', { key: 'ArrowDown' }); // wrap forward past the last
  assert.equal(document.activeElement, items[0]);
  document.dispatch('keydown', { key: 'ArrowUp' }); // wrap backward past the first
  assert.equal(document.activeElement, items[last]);
  document.dispatch('keydown', { key: 'ArrowUp' });
  assert.equal(document.activeElement, items[last - 1]);
});

test('Home/End jump to the first/last menu item', () => {
  const { nav, more, moreBtn, moreMenu } = buildDom();
  mount(nav, more, {});
  moreBtn.dispatch('click');
  const items = moreMenu.querySelectorAll('[role="menuitem"]');
  document.dispatch('keydown', { key: 'End' });
  assert.equal(document.activeElement, items[items.length - 1]);
  document.dispatch('keydown', { key: 'Home' });
  assert.equal(document.activeElement, items[0]);
});

test('Escape from a focused menu item closes the menu and returns focus to the toggle', () => {
  const { nav, more, moreBtn, moreMenu } = buildDom();
  mount(nav, more, {});
  moreBtn.dispatch('click');
  document.dispatch('keydown', { key: 'ArrowDown' }); // focus moves onto an item
  document.dispatch('keydown', { key: 'Escape' });
  assert.equal(moreMenu.hidden, true);
  assert.equal(document.activeElement, moreBtn);
});

test('activating a menu item closes the menu and returns focus to the toggle, not to body', async () => {
  const { nav, more, moreBtn, moreMenu } = buildDom();
  let ran = false;
  mount(nav, more, { inspect: { label: 'Inspect', run: () => { ran = true; } } });
  moreBtn.dispatch('click');
  const items = moreMenu.querySelectorAll('[role="menuitem"]');
  const inspectItem = items.find((i) => i.textContent === 'Inspect');
  inspectItem.dispatch('click');
  assert.equal(ran, true);
  assert.equal(moreMenu.hidden, true);
  assert.equal(document.activeElement, moreBtn, 'focus must not fall to body once the item is hidden');
});

test('a failed nav-key press shows the server/thrown message via the shared toast, not raw JSON', async () => {
  const { nav, more, toast } = buildDom();
  const api = mount(nav, more, {
    sendAndroidKey: async () => { throw new Error('no open session'); },
    toast,
  });
  api.setOS('android');
  api.setControl(true);
  const backBtn = nav.children.find((b) => b.dataset.slot === 'upright');
  backBtn.onclick();
  await tick(2);
  assert.equal(toast.hidden, false);
  assert.equal(toast.textContent, 'no open session');
});

function fakeKeyboard(offered) {
  const k = {
    up: false, opened: 0, closed: 0, listeners: [],
    offered: () => offered,
    isOpen: () => k.up,
    open() { k.up = true; k.opened++; },
    close() { k.up = false; k.closed++; },
    onChange(fn) { k.listeners.push(fn); },
  };
  return k;
}

function withKbdBtn(dom) {
  const kbdBtn = globalThis.document.createElement('button');
  kbdBtn.setAttribute('id', 'kbdBtn');
  kbdBtn.hidden = true;
  dom.more.appendChild(kbdBtn);
  return { ...dom, kbdBtn };
}

test('the keyboard button shows only while the target takes typing, re-read on setOS and setControl', () => {
  const { nav, more, kbdBtn } = withKbdBtn(buildDom());
  let offered = false;
  const k = fakeKeyboard(false);
  k.offered = () => offered;
  const api = mount(nav, more, { keyboard: k });
  assert.equal(kbdBtn.hidden, true);
  offered = true;
  api.setControl(true);
  assert.equal(kbdBtn.hidden, false);
  offered = false;
  api.setOS('');
  assert.equal(kbdBtn.hidden, true);
});

test('the keyboard button raises the keyboard, and the next tap dismisses it', () => {
  const { nav, more, kbdBtn } = withKbdBtn(buildDom());
  const k = fakeKeyboard(true);
  mount(nav, more, { keyboard: k });
  kbdBtn.dispatch('click');
  assert.equal(k.opened, 1);
  assert.equal(kbdBtn.getAttribute('aria-pressed'), 'true');
  kbdBtn.dispatch('click');
  assert.equal(k.closed, 1);
  assert.equal(kbdBtn.getAttribute('aria-pressed'), 'false');
});

test('a keyboard dismissed some other way unpresses the button', () => {
  const { nav, more, kbdBtn } = withKbdBtn(buildDom());
  const k = fakeKeyboard(true);
  mount(nav, more, { keyboard: k });
  kbdBtn.dispatch('click');
  k.up = false;
  for (const fn of k.listeners) fn(false);
  assert.equal(kbdBtn.getAttribute('aria-pressed'), 'false');
});

test('losing control takes the keyboard down', () => {
  const { nav, more, kbdBtn } = withKbdBtn(buildDom());
  let offered = true;
  const k = fakeKeyboard(true);
  k.offered = () => offered;
  const api = mount(nav, more, { keyboard: k });
  kbdBtn.dispatch('click');
  offered = false;
  api.setControl(false);
  assert.equal(k.up, false);
  assert.equal(kbdBtn.hidden, true);
});

test('the overflow menu no longer carries keyboard items', () => {
  const { nav, more, moreBtn, moreMenu } = withKbdBtn(buildDom());
  mount(nav, more, { keyboard: fakeKeyboard(true) });
  moreBtn.dispatch('click');
  const labels = moreMenu.querySelectorAll('.moreMenuItem').map((b) => b.textContent);
  assert.equal(labels.some((l) => /keyboard/i.test(l)), false);
});

test('Show touches is a checkmark item on every target, its state re-read each open', () => {
  const { nav, more, moreBtn, moreMenu } = buildDom();
  let on = false;
  mount(nav, more, { touches: { pref: () => on, setPref: (v) => { on = v; } } });
  moreBtn.dispatch('click');
  const sw = moreMenu.querySelectorAll('[role="menuitemcheckbox"]')[0];
  assert.equal(sw.textContent, 'Show touches');
  assert.equal(sw.getAttribute('aria-checked'), 'false');
  assert.equal(moreMenu.classList.contains('checked'), false);
  sw.dispatch('click');
  assert.equal(on, true);
  moreBtn.dispatch('click');
  assert.equal(sw.getAttribute('aria-checked'), 'true');
  assert.equal(moreMenu.classList.contains('checked'), true, 'one checkmark indents every label');
});

test('an overflow menu item with hidden set is not displayed', async () => {
  // .moreMenuItem sets display: flex, which beats the browser's own [hidden]
  // rule; only an author rule that wins over it keeps a hidden item out of
  // the menu.
  const { readFile } = await import('node:fs/promises');
  const css = await readFile(new URL('./style.css', import.meta.url), 'utf8');
  assert.match(css, /^\[hidden\]\s*\{\s*display:\s*none\s*!important;\s*\}/m);
});

// --- Copy Link and Zoom ----------------------------------------------------

test('pageLink is the page URL for one target with no token', () => {
  assert.equal(pageLink('https://mac.local:18456', 'emulator-5554'), 'https://mac.local:18456/?target=emulator-5554');
  assert.equal(pageLink('https://mac.local:18456', 'avd:Pixel 10'), 'https://mac.local:18456/?target=avd%3APixel%2010');
  assert.equal(pageLink('https://mac.local:18456', ''), null);
  assert.equal(pageLink('https://mac.local:18456', null), null);
});

test('Copy Link writes the focused target\'s link and flashes "Link copied"', async () => {
  const { nav, more, moreMenu, toast } = buildDom();
  const written = [];
  mount(nav, more, { toast, link: { url: () => 'https://h/?target=a', write: async (s) => { written.push(s); } } });
  const item = moreMenu.querySelectorAll('[role="menuitem"]').find((i) => i.textContent === 'Copy Link');
  assert.ok(item, 'Copy Link is in the menu');
  item.dispatch('click');
  await new Promise((r) => setTimeout(r, 0));
  assert.deepEqual(written, ['https://h/?target=a']);
  assert.equal(toast.textContent, 'Link copied');
});

test('Copy Link with no target shows no menu item action and reports the failure', async () => {
  const { nav, more, moreMenu, toast } = buildDom();
  const written = [];
  mount(nav, more, { toast, link: { url: () => null, write: async (s) => { written.push(s); } } });
  const item = moreMenu.querySelectorAll('[role="menuitem"]').find((i) => i.textContent === 'Copy Link');
  item.dispatch('click');
  await new Promise((r) => setTimeout(r, 0));
  assert.deepEqual(written, []);
  assert.equal(toast.textContent, 'No device to link to');
});

test('a Zoom item enters zoom mode', () => {
  const { nav, more, moreMenu } = buildDom();
  let entered = 0;
  mount(nav, more, { zoom: { enter() { entered++; } } });
  const item = moreMenu.querySelectorAll('[role="menuitem"]').find((i) => i.textContent === 'Zoom');
  assert.ok(item, 'Zoom is in the menu');
  item.dispatch('click');
  assert.equal(entered, 1);
});

await Promise.all(pending);
console.log(failures ? `\n${failures} test(s) failing` : '\nall tests passing');
process.exit(failures ? 1 : 0);

test('a lost connection hides the keyboard button and leaves the keyboard up', () => {
  const { nav, more, kbdBtn } = withKbdBtn(buildDom());
  const k = fakeKeyboard(true);
  let control = true;
  k.offered = () => control;
  k.takes = () => true;
  const api = mount(nav, more, { keyboard: k });
  api.setControl(true);
  k.up = true;
  control = false;
  api.setControl(false);
  assert.equal(kbdBtn.hidden, true);
  assert.equal(k.closed, 0);
});

test('a target that does not take typing closes an open keyboard', () => {
  const { nav, more } = withKbdBtn(buildDom());
  const k = fakeKeyboard(false);
  k.takes = () => false;
  const api = mount(nav, more, { keyboard: k });
  k.up = true;
  api.setControl(true);
  assert.equal(k.closed, 1);
});
