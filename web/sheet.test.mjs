// Tests for the Tools sheet's settle logic. Run: node web/sheet.test.mjs
//
// mount() is DOM wiring and, like app.js, has no harness here (trap 21) --
// only the pure settle() function is pinned against fixtures, the same
// pattern layout.test.mjs and orientation.test.mjs use.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { settle, estimateVelocity, detentHeightPx, DETENTS, SECTION_ORDER, HARDWARE_KEYS } from './sheet.js';

const fixtures = JSON.parse(
  readFileSync(new URL('../testdata/client/sheet.json', import.meta.url)),
);

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

for (const c of fixtures.settle) {
  test(`settle: ${c.name}`, () => {
    const got = settle(c.startDetent, c.dragDy, c.velocityY, c.vh);
    assert.equal(got, c.expect);
  });
}

for (const c of fixtures.velocity) {
  test(`estimateVelocity: ${c.name}`, () => {
    const got = estimateVelocity(c.samples, c.now);
    assert.equal(got, c.expect);
  });
}

test('DETENTS carries half and full as fractions of the viewport', () => {
  assert.equal(DETENTS.half, 0.5);
  assert.equal(DETENTS.full, 0.92);
});

// detentHeightPx is the pure half of mount()'s resize handling: the height a
// detent resolves to at a given viewport height, with no DOM involved. A
// resize/rotation reapplies this from a fresh measurement (mount() wires the
// visualViewport/window resize listener; that wiring itself has no harness,
// same as the rest of mount(), but the arithmetic it depends on is pinned
// here). Fixtures pin an actual rotation: 390x844 portrait to 844x390
// landscape, the exact case that left the sheet off-screen before this was
// fixed.
test('detentHeightPx: full in 390x844 portrait', () => {
  assert.equal(detentHeightPx(844, 'full'), 776);
});

test('detentHeightPx: half in 390x844 portrait', () => {
  assert.equal(detentHeightPx(844, 'half'), 422);
});

test('detentHeightPx: full after rotating to 844x390 landscape is far smaller, not left at the portrait value', () => {
  assert.equal(detentHeightPx(390, 'full'), 359);
});

test('detentHeightPx: half after rotating to 844x390 landscape', () => {
  assert.equal(detentHeightPx(390, 'half'), 195);
});

test('detentHeightPx: a zero viewport height (not yet measured) gives zero, not NaN or a stale height', () => {
  assert.equal(detentHeightPx(0, 'full'), 0);
});

test('detentHeightPx: an unrecognized detent gives zero rather than throwing', () => {
  assert.equal(detentHeightPx(844, 'nonsense'), 0);
});

// Added for client Task 9's review round 3 (N24): the pure settle()/
// detentHeightPx() tests above never touch showSection/hideSection, so a
// mutant that swapped their bodies (showSection setting hidden=true,
// hideSection setting hidden=false) passed clean. mount() itself is still
// otherwise untested (trap 21, this file's own top comment) -- this covers
// only the two functions app.js's Task 9 fixes actually call.
test('Hardware and Control lead the sections, and Hardware is Vol -, Vol +, Power', () => {
  assert.deepEqual(SECTION_ORDER.slice(0, 2), ['hardware', 'control']);
  assert.deepEqual(HARDWARE_KEYS.map((k) => [k.label, k.keycode]), [['Vol \u2212', 25], ['Vol +', 24], ['Power', 26]]);
});

{
  const { installFakeDom } = await import('./fakedom.js');
  const { mount } = await import('./sheet.js');

  function freshRoot() {
    installFakeDom();
    return document.createElement('div');
  }

  test('the sheet builds titled Hardware and Control sections', () => {
    const root = freshRoot();
    const handle = mount(root, { storage: null });
    assert.ok(handle.sections.hardware && handle.sections.control);
    assert.equal(root.querySelector('section[data-section="hardware"]').querySelector('h3').textContent, 'Hardware');
    assert.equal(root.querySelector('section[data-section="control"]').querySelector('h3').textContent, 'Control');
  });

  test('hideSection hides the named section and leaves others alone', () => {
    const root = freshRoot();
    const handle = mount(root, { storage: null });
    handle.hideSection('app');
    const app = root.querySelector('section[data-section="app"]');
    const stream = root.querySelector('section[data-section="stream"]');
    assert.equal(app.hidden, true);
    assert.equal(stream.hidden, false, 'hideSection must not touch a section it was not asked about');
  });

  // sheet-open/sheet-close telemetry carries the detent, and onClose is the
  // only place the detent it closed from is still known.
  test('onOpen gets the detent opened to, onClose the detent closed from', () => {
    const root = freshRoot();
    const hadWindow = 'window' in globalThis;
    const saved = globalThis.window;
    globalThis.window = { innerHeight: 844 };
    try {
      const seen = [];
      const handle = mount(root, {
        storage: null,
        onOpen: (d) => seen.push(['open', d]),
        onClose: (d) => seen.push(['close', d]),
      });
      handle.open('half');
      handle.open('full');
      handle.close();
      assert.deepEqual(seen, [['open', 'half'], ['open', 'full'], ['close', 'full']]);
    } finally {
      if (hadWindow) globalThis.window = saved; else delete globalThis.window;
    }
  });

  test('showSection reverses hideSection', () => {
    const root = freshRoot();
    const handle = mount(root, { storage: null });
    handle.hideSection('device');
    assert.equal(root.querySelector('section[data-section="device"]').hidden, true, 'sanity: hidden first');
    handle.showSection('device');
    assert.equal(root.querySelector('section[data-section="device"]').hidden, false);
  });

  test('showSection on a section that was never hidden is a no-op, not a throw', () => {
    const root = freshRoot();
    const handle = mount(root, { storage: null });
    assert.doesNotThrow(() => handle.showSection('viewer'));
    assert.equal(root.querySelector('section[data-section="viewer"]').hidden, false);
  });

  test('hideSection/showSection on an unknown id do not throw', () => {
    const root = freshRoot();
    const handle = mount(root, { storage: null });
    assert.doesNotThrow(() => handle.hideSection('nonexistent'));
    assert.doesNotThrow(() => handle.showSection('nonexistent'));
  });

  test('the groups are Hardware, Control, App, Device, Stream, Viewer and More, in that order', () => {
    const root = freshRoot();
    mount(root, {});
    const ids = root.querySelectorAll('section').map((s) => s.dataset.section);
    assert.deepEqual(ids, ['hardware', 'control', 'app', 'device', 'stream', 'viewer', 'more']);
  });

  function pageShown(root) {
    return root.querySelectorAll('.sheetPage').filter((p) => !p.hidden).map((p) => p.dataset.page);
  }

  test('a link row pushes its page and hides the groups; Back returns', () => {
    const root = freshRoot();
    const seen = [];
    const handle = mount(root, { onPage: (id, action) => seen.push([id, action]) });
    handle.addPage('log', 'Event log');
    handle.addLink('more', 'Event log', 'log');
    assert.deepEqual(pageShown(root), ['']);

    root.querySelector('.sheetLinkRow').dispatch('click');
    assert.deepEqual(pageShown(root), ['log']);
    assert.equal(handle.page, 'log');
    const back = root.querySelector('.sheetBack');
    assert.equal(back.textContent, 'Tools', 'Back names the page it returns to');

    back.dispatch('click');
    assert.deepEqual(pageShown(root), ['']);
    assert.equal(handle.page, '');
    assert.deepEqual(seen, [['log', 'push'], ['log', 'pop']]);
  });

  test('a page pushed from a page names that page on its Back button', () => {
    const root = freshRoot();
    const handle = mount(root, {});
    const conn = handle.addPage('connection', 'Connection');
    handle.addPage('detail', 'Detail');
    handle.pushPage('connection');
    handle.addLink(conn, 'Detail', 'detail');
    handle.pushPage('detail');
    const backs = root.querySelectorAll('.sheetBack');
    assert.equal(backs[1].textContent, 'Connection');
    handle.popPage();
    assert.deepEqual(pageShown(root), ['connection']);
  });

  test('closing the sheet returns it to the groups without an onPage pop', () => {
    const root = freshRoot();
    const hadWindow = 'window' in globalThis;
    const saved = globalThis.window;
    globalThis.window = { innerHeight: 844 };
    try {
      const seen = [];
      const handle = mount(root, { onPage: (id, action) => seen.push([id, action]) });
      handle.addPage('log', 'Event log');
      handle.open('half');
      handle.pushPage('log');
      handle.close();
      assert.equal(handle.page, '');
      assert.deepEqual(pageShown(root), ['']);
      assert.deepEqual(seen, [['log', 'push']]);
    } finally {
      if (hadWindow) globalThis.window = saved; else delete globalThis.window;
    }
  });

  test('pushing an unknown page does nothing', () => {
    const root = freshRoot();
    const handle = mount(root, {});
    handle.pushPage('nope');
    assert.equal(handle.page, '');
    assert.deepEqual(pageShown(root), ['']);
  });

  test('setFooter writes one footer under the group and replaces it on a second call', () => {
    const root = freshRoot();
    const handle = mount(root, {});
    handle.setFooter('stream', 'one');
    handle.setFooter('stream', 'Changes apply to the next session.');
    const footers = root.querySelector('section[data-section="stream"]').querySelectorAll('.sheetSectionFooter');
    assert.equal(footers.length, 1);
    assert.equal(footers[0].textContent, 'Changes apply to the next session.');
  });
}

console.log(failures ? `\n${failures} failing` : '\nall passing');
process.exit(failures ? 1 : 0);
