// Tests for inspect.js's pure functions. Run: node web/inspect.test.mjs
//
// Fixture is testdata/client/ui-settings.json, a hand-built Settings-home
// tree (see its own "_note" -- not a real capture, one is owed). It has a
// "Network & internet" row inside a scrollable container, and a gap between
// it and "Connected devices" below.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const { nodeAt, treeIsStale, toCss, containsPoint, describe: describeNode } = await import('./inspect.js');

const webDir = path.dirname(fileURLToPath(import.meta.url));
const fixturePath = path.join(webDir, '..', 'testdata', 'client', 'ui-settings.json');
const fixture = JSON.parse(readFileSync(fixturePath, 'utf8'));

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

// --- nodeAt -----------------------------------------------------------------

test('nodeAt finds the deepest node containing the point', () => {
  // (300, 320) is inside the "Network & internet" row's whole bounds
  // [0,260,1080,460] and inside its title TextView's bounds
  // [168,290,760,360] -- the title should win, not the row it sits in.
  const hit = nodeAt(fixture.nodes, 300, 320);
  assert.equal(hit.text, 'Network & internet');
});

test('nodeAt falls back to the row when no child contains the point', () => {
  // (900, 400) is inside the row [0,260,1080,460] but to the right of both
  // text children (which end at x=760) -- the row is the deepest hit.
  const hit = nodeAt(fixture.nodes, 900, 400);
  assert.equal(hit.id, 'com.android.settings:id/network_and_internet_row');
});

test('nodeAt returns the container itself in the gap between rows', () => {
  // (300, 500) is between the two rows (460..540) but still inside the
  // scrollable container's own bounds [0,220,1080,2424].
  const hit = nodeAt(fixture.nodes, 300, 500);
  assert.equal(hit.id, 'com.android.settings:id/list_container');
});

test('nodeAt returns null outside every node', () => {
  assert.equal(nodeAt(fixture.nodes, 5000, 5000), null);
});

test('nodeAt treats null nodes as empty rather than throwing', () => {
  // target.Prune can collapse to nothing, and /api/ui then answers "nodes":
  // null rather than []. A server fix for this is in flight; the client
  // side of it is this being a no-op instead of a TypeError.
  assert.equal(nodeAt(null, 100, 100), null);
});

test('nodeAt treats a null child list the same way, one level down', () => {
  const nodes = [{ bounds: [0, 0, 100, 100], children: null }];
  assert.equal(nodeAt(nodes, 50, 50), nodes[0]);
});

test('a later sibling wins a tie, Android paint order', () => {
  const nodes = [
    { id: 'under', bounds: [0, 0, 100, 100] },
    { id: 'over', bounds: [0, 0, 100, 100] },
  ];
  assert.equal(nodeAt(nodes, 50, 50).id, 'over');
});

// --- treeIsStale --------------------------------------------------------

test('treeIsStale is false for a matching size', () => {
  assert.equal(treeIsStale(fixture, fixture.width, fixture.height), false);
});

test('treeIsStale is true after a rotation changes the stream size', () => {
  assert.equal(treeIsStale(fixture, fixture.height, fixture.width), true);
});

test('treeIsStale is false with no tree yet -- nothing to be stale', () => {
  assert.equal(treeIsStale(null, 1080, 2424), false);
  assert.equal(treeIsStale(undefined, 1080, 2424), false);
});

// --- toCss ----------------------------------------------------------------

test('toCss scales a device-pixel box onto the canvas rect', () => {
  const rect = { width: 540, height: 1212 }; // half of 1080x2424
  const box = toCss([168, 290, 760, 360], 1080, 2424, rect);
  assert.deepEqual(box, { left: 84, top: 145, width: 296, height: 35 });
});

test('toCss uses independent x/y scale factors', () => {
  const rect = { width: 100, height: 50 };
  const box = toCss([0, 0, 10, 10], 10, 10, rect);
  // 10x scale on x, 5x on y -- a square device box becomes a non-square CSS
  // one, which is the point of not assuming one shared scale factor.
  assert.deepEqual(box, { left: 0, top: 0, width: 100, height: 50 });
});

test('toCss on a zero device size does not divide by zero', () => {
  const box = toCss([0, 0, 10, 10], 0, 0, { width: 100, height: 100 });
  assert.deepEqual(box, { left: 0, top: 0, width: 0, height: 0 });
});

test('toCss offsets by the canvas rect\'s own left/top -- the framed case (review HIGH-2)', () => {
  // pixel_10's display rect at 0.25 scale: layout (59,55) 1080x2424 ->
  // canvas box left=14.75 top=13.75 width=270 height=606 inside the wider
  // #inspectLayer, which covers the whole bezel. Device (0,0) must land at
  // the canvas's own origin, not the layer's.
  const rect = { left: 14.75, top: 13.75, width: 270, height: 606 };
  const origin = toCss([0, 0, 0, 0], 1080, 2424, rect);
  assert.equal(origin.left, 14.75);
  assert.equal(origin.top, 13.75);
  // Device (90, 90) at 0.25 scale is 22.5px into the canvas on both axes.
  const p = toCss([90, 90, 90, 90], 1080, 2424, rect);
  assert.equal(p.left, 14.75 + 22.5);
  assert.equal(p.top, 13.75 + 22.5);
});

test('toCss with y1 not 0 still offsets correctly (not just an x1=y1=0 coincidence)', () => {
  const rect = { left: 10, top: 20, width: 1080, height: 2424 };
  const box = toCss([100, 500, 300, 700], 1080, 2424, rect);
  assert.deepEqual(box, { left: 110, top: 520, width: 200, height: 200 });
});

test('toCss offsets top by the y scale, not the x scale, when they differ and y1 is not 0 (review round 3, M5)', () => {
  // The test above has sx === sy === 1 (rect matches the device size
  // exactly), so `top: originTop + y1 * sx` would produce the identical
  // number and pass it too. 10x on x, 5x on y, plus a nonzero top/y1, is
  // what actually distinguishes originTop + y1*sy from originTop + y1*sx.
  const rect = { left: 3, top: 7, width: 100, height: 50 };
  const box = toCss([0, 4, 10, 10], 10, 10, rect);
  // sx = 100/10 = 10, sy = 50/10 = 5. top = 7 + 4*5 = 27 (not 7 + 4*10 = 47).
  assert.equal(box.top, 27);
});

// --- treeIsStale, height-only variant -------------------------------------

test('treeIsStale reads height against width, not height against height (M11-class survivor)', () => {
  // A landscape tree (2424x1080, w>h) against a landscape stream of a
  // DIFFERENT size (900x500, also w>h): same orientation, must read fresh.
  // A mutant comparing tree.height to streamH alone (or width to streamW
  // alone) instead of deriving each side's own orientation would get this
  // wrong -- 1080 != 500 looks like a change if you compare the raw numbers
  // instead of each side's own aspect.
  const landscapeTree = { width: 2424, height: 1080 };
  assert.equal(treeIsStale(landscapeTree, 900, 500), false);
});

// --- containsPoint --------------------------------------------------------

test('containsPoint is inclusive on all four edges', () => {
  const bounds = [10, 20, 110, 220];
  assert.equal(containsPoint(bounds, 10, 20), true, 'top-left corner');
  assert.equal(containsPoint(bounds, 110, 220), true, 'bottom-right corner');
  assert.equal(containsPoint(bounds, 10, 120), true, 'left edge, mid-height');
  assert.equal(containsPoint(bounds, 60, 20), true, 'top edge, mid-width');
});

test('containsPoint excludes points just outside each edge', () => {
  const bounds = [10, 20, 110, 220];
  assert.equal(containsPoint(bounds, 9, 120), false);
  assert.equal(containsPoint(bounds, 111, 120), false);
  assert.equal(containsPoint(bounds, 60, 19), false);
  assert.equal(containsPoint(bounds, 60, 221), false);
});

// --- describe / password redaction (review MEDIUM-5) ----------------------

test('describe redacts a password node\'s text -- never the real value', () => {
  const node = nodeAt(fixture.nodes, 540, 956); // the Wi-Fi password EditText
  assert.equal(node.id, 'com.android.settings:id/password_field');
  assert.equal(node.text, 'hunter2', 'fixture sanity: the raw node still carries the real text');
  const lines = describeNode(node);
  assert.ok(lines.some((l) => l.startsWith('text: ') && l.includes('•')), 'must show a mask');
  assert.ok(!lines.some((l) => l.includes('hunter2')), 'must never show the real password');
  // Round 4, M14: the previous version of this test asserted only the
  // `text:` line, so a mutation that dropped 'password' from describe's own
  // `flags` list (a separate array, independent of the redaction branch
  // above it) passed unnoticed -- the flags line silently stopped saying
  // this field was a password field at all, while still masking its text.
  // Both must hold independently.
  assert.ok(lines.some((l) => l.startsWith('flags:') && l.includes('password')), 'must report the password flag itself, not just redact the text');
});

test('describe redacts text using the password flag alone -- not text content or field type (M14, round 4)', () => {
  // A node carrying `password: true` with no other flag set at all: proves
  // the redaction branch reads `node.password` specifically, not some
  // stand-in like `node.focused` or `node.class === 'EditText'` that the
  // fixture node above also happens to have set.
  const node = { class: 'EditText', text: 'hunter2', password: true };
  const lines = describeNode(node);
  assert.ok(lines.some((l) => l.startsWith('text: ') && l.includes('•')));
  assert.ok(!lines.some((l) => l.includes('hunter2')));
  assert.ok(lines.some((l) => l.startsWith('flags:') && l.includes('password')));
});

test('describe reports a disabled node\'s flag', () => {
  const node = nodeAt(fixture.nodes, 900, 1200); // the disabled "Forget network" row (right of its text child)
  assert.equal(node.id, 'com.android.settings:id/forget_network_row');
  assert.equal(node.disabled, true, 'fixture sanity');
  const lines = describeNode(node);
  assert.ok(lines.some((l) => l.startsWith('flags:') && l.includes('disabled')));
});

// --- mount() ----------------------------------------------------------
//
// Against web/fakedom.js's fake DOM. Added for client Task 9's review: the
// pure functions above were tested, but nothing exercised toCss/nodeAt
// wired together through an actual click, or a fetch racing a target switch
// (trap 41) -- both mutated clean in the first pass.

const { installFakeDom, tick, deferred, jsonResponse } = await import('./fakedom.js');
const { mount } = await import('./inspect.js');

function layerAndCard() {
  installFakeDom();
  const layer = document.createElement('div');
  const card = document.createElement('div');
  return { layer, card };
}

test('run() fetches and turns boxes on; a second run() toggles off and clears them', async () => {
  const { layer, card } = layerAndCard();
  const m = mount(layer, {
    targetId: () => 'x',
    screenRect: () => ({ left: 0, top: 0, width: 1080, height: 2424 }),
    fetchImpl: async () => jsonResponse(200, fixture),
    card,
  });
  m.run();
  await tick(2);
  assert.equal(m.isOn(), true);
  assert.equal(layer.hidden, false);
  assert.ok(layer.querySelectorAll('.inspectBox').length > 0);
  m.run();
  assert.equal(m.isOn(), false);
  assert.equal(layer.querySelectorAll('.inspectBox').length, 0);
});

test('a 501 shows the toast message and leaves inspect off', async () => {
  const { layer, card } = layerAndCard();
  const toast = document.createElement('div');
  const m = mount(layer, {
    targetId: () => 'x',
    screenRect: () => ({ left: 0, top: 0, width: 1080, height: 2424 }),
    fetchImpl: async () => jsonResponse(501, { error: 'no inspector on this target' }),
    card, toast,
  });
  m.run();
  await tick(2);
  assert.equal(m.isOn(), false);
  assert.equal(toast.hidden, false);
  assert.equal(toast.textContent, 'no inspector on this target');
});

test('a click on the layer selects the right node through a framed canvas offset (review HIGH-2, tightened in round 3: N14/N16/F6)', async () => {
  const { layer, card } = layerAndCard();
  // pixel_10 at 0.25 scale, same geometry as the toCss test above.
  const rect = { left: 14.75, top: 13.75, width: 270, height: 606 };
  const m = mount(layer, {
    targetId: () => 'x',
    screenRect: () => rect,
    fetchImpl: async () => jsonResponse(200, fixture),
    card,
  });
  // The layer covers the whole bezel (inset:0 in #screenWrap), so its own
  // rect is offset from the viewport independently of the canvas rect --
  // give it an origin that differs from the canvas rect's, so a handler
  // that mixes the two offsets up is exercised, not one that happens to
  // agree with the canvas rect by coincidence.
  layer._rect = { left: 5, top: 7, width: 200, height: 200 };
  m.run();
  await tick(2);
  // Round 3 finding N14/N16/F6: the previous version of this test clicked
  // the wide "Network & internet" TITLE (bounds height 70) and computed
  // clientX/Y by adding BOTH the canvas offset AND the layer's own (5,7)
  // offset -- ev.clientX/Y are viewport-absolute already, so that extra +5/+7
  // was never correct math, only close enough that the wide title still
  // contained the resulting point. A handler with the offset handling
  // removed entirely (using ev.clientX/Y raw, or adding the layer offset it
  // should not) passed the same way.
  //
  // The "Mobile, Wi-Fi, hotspot" SUBTITLE is a tighter target: bounds
  // [168,366,760,420], 54 device px tall. Correct math is clientX =
  // canvas.left + deviceX*scale (no layer term at all -- canvasRect()
  // folds the layer's own box in only for drawing boxes IN the layer's
  // coordinate frame; a click event's clientX/Y need no such folding,
  // since they already share the canvas rect's viewport-absolute frame).
  // Device center (464, 393) at 0.25 scale: clientX = 14.75 + 464*0.25 =
  // 130.75, clientY = 13.75 + 393*0.25 = 112.
  layer.dispatch('click', { clientX: 130.75, clientY: 112 });
  const dts = card.querySelectorAll('dt');
  assert.ok(dts.length > 0, 'the click must have selected a node and rendered its card');
  assert.ok(card.textContent.includes('Mobile, Wi-Fi, hotspot'), 'the framed offset must be subtracted before hit-testing, or this lands on the parent row (no text: line) instead of the subtitle leaf');

  // The failure mode this guards against, made concrete: adding the
  // layer's own (5,7) offset on top of the correct math -- exactly what
  // the old test's comment computed -- shifts the device-space point by
  // (20, 28) (at 0.25 scale), which clears this leaf's 54px height and
  // falls back to the parent row. Proven directly against nodeAt, since a
  // wrong onLayerClick and a right one are otherwise only reachable by
  // mutating the shipped file:
  const wronglyOffsetPoint = { x: 464 + 5 / 0.25, y: 393 + 7 / 0.25 };
  const missed = nodeAt(fixture.nodes, wronglyOffsetPoint.x, wronglyOffsetPoint.y);
  assert.ok(!describeNode(missed).some((l) => l.includes('Mobile, Wi-Fi, hotspot')), 'sanity: the +5/+7 error this test rejects really does miss the subtitle leaf');
});

test('a slow fetch for A is discarded after off() -- e.g. a target switch (trap 41)', async () => {
  const { layer, card } = layerAndCard();
  const d = deferred();
  const m = mount(layer, {
    targetId: () => 'A',
    screenRect: () => ({ left: 0, top: 0, width: 1080, height: 2424 }),
    fetchImpl: async () => { await d.promise; return jsonResponse(200, fixture); },
    card,
  });
  m.run();
  m.off(); // app.js's reconnect() calls this on every target switch
  d.resolve();
  await tick(2);
  assert.equal(m.isOn(), false, 'A\'s slow tree must not turn inspect back on after the switch');
  assert.equal(layer.querySelectorAll('.inspectBox').length, 0);
});

test('a rotation racing the FIRST fetch is not drawn in the stale (pre-rotation) orientation (review round 3, finding 2)', async () => {
  // Before this fix, notifyResize returned early while `tree` was still
  // null (nothing fetched yet), so a rotation landing during the very
  // first fetch never bumped `gen` -- treeIsStale(null, ...) is false by
  // definition, there being nothing yet to compare against. The fetch then
  // resolved with a tree describing the device's PRE-rotation orientation,
  // and nothing had invalidated it, so it was drawn as if it still matched.
  const { layer, card } = layerAndCard();
  let resolveFetch;
  const held = new Promise((res) => { resolveFetch = res; });
  const m = mount(layer, {
    targetId: () => 'x',
    screenRect: () => ({ left: 0, top: 0, width: 1080, height: 2424 }),
    fetchImpl: async () => { await held; return jsonResponse(200, fixture); },
    card,
  });
  m.run();
  await tick(2); // fetch is in flight, `tree` is still null
  m.notifyResize(fixture.height, fixture.width); // portrait -> landscape, mid-fetch
  resolveFetch();
  await tick(3);
  assert.equal(layer.querySelectorAll('.inspectBox').length, 0, 'must not draw boxes in the orientation the device was in before the rotation');
  assert.ok(layer.querySelector('.inspectRefresh'), 'must land on the stale/refresh state instead, not a silently-empty one');
});

test('notifyResize marks the tree stale on a rotation and shows a Refresh control', async () => {
  const { layer, card } = layerAndCard();
  const m = mount(layer, {
    targetId: () => 'x',
    screenRect: () => ({ left: 0, top: 0, width: 1080, height: 2424 }),
    fetchImpl: async () => jsonResponse(200, fixture),
    card,
  });
  m.run();
  await tick(2);
  m.notifyResize(fixture.height, fixture.width); // portrait -> landscape
  const refresh = layer.querySelector('.inspectRefresh');
  assert.ok(refresh, 'a visible Refresh control must appear once stale (review MEDIUM-6)');
  assert.equal(layer.querySelectorAll('.inspectBox').length, 0, 'stale boxes are not drawn against the wrong orientation');
});

test('a click on the layer while stale selects nothing -- a stale tree is not trustworthy to tap (M28-class survivor)', async () => {
  const { layer, card } = layerAndCard();
  const m = mount(layer, {
    targetId: () => 'x',
    screenRect: () => ({ left: 0, top: 0, width: 1080, height: 2424 }),
    fetchImpl: async () => jsonResponse(200, fixture),
    card,
  });
  m.run();
  await tick(2);
  m.notifyResize(fixture.height, fixture.width); // portrait -> landscape: now stale
  // (90, 300) is inside the fixture's root ScrollView [0,220,1080,2424] --
  // proven to select something when the tree is fresh, in the test below.
  layer.dispatch('click', { clientX: 90, clientY: 300 });
  await tick(2);
  assert.equal(card.querySelectorAll('dt').length, 0, 'a click while stale must not select or render a node');
  assert.equal(card.innerHTML, '', 'review round 3 F1: read innerHTML directly too, not only through querySelectorAll/textContent');
});

test('sanity: the same click point used above does select a node once the tree is fresh (not stale)', async () => {
  const { layer, card } = layerAndCard();
  const m = mount(layer, {
    targetId: () => 'x',
    screenRect: () => ({ left: 0, top: 0, width: 1080, height: 2424 }),
    fetchImpl: async () => jsonResponse(200, fixture),
    card,
  });
  m.run();
  await tick(2);
  layer.dispatch('click', { clientX: 90, clientY: 300 });
  await tick(2);
  assert.ok(card.querySelectorAll('dt').length > 0, 'the click point must hit a real node when the tree is not stale');
  assert.ok(card.textContent.includes('network_and_internet_row') || card.textContent.length > 0);
  assert.ok(card.innerHTML.includes('<dt>'), 'review round 3 F1: read innerHTML directly, not only through textContent/querySelectorAll');
});

test('the Refresh control shown while stale re-fetches when clicked (review round 3, N26)', async () => {
  const { layer, card } = layerAndCard();
  let fetches = 0;
  // The device is landscape by the time Refresh is clicked (that is what
  // "stale" means here), so the SECOND fetch has to actually describe
  // landscape -- refetching the same portrait fixture would correctly stay
  // stale per this round's finding 2 fix, which would make this test
  // indistinguishable from one where Refresh's click handler is never
  // wired at all (both show 0 boxes). fetches counts whether the click
  // reached fetchImpl a second time; that is what N26 is actually about.
  const landscape = { ...fixture, width: fixture.height, height: fixture.width };
  const m = mount(layer, {
    targetId: () => 'x',
    screenRect: () => ({ left: 0, top: 0, width: 1080, height: 2424 }),
    fetchImpl: async () => { fetches++; return jsonResponse(200, fetches === 1 ? fixture : landscape); },
    card,
  });
  m.run();
  await tick(2);
  assert.equal(fetches, 1);
  m.notifyResize(fixture.height, fixture.width); // portrait -> landscape: now stale
  const refresh = layer.querySelector('.inspectRefresh');
  assert.ok(refresh, 'sanity: the stale notice and its Refresh button are showing');
  refresh.dispatch('click');
  await tick(2);
  assert.equal(fetches, 2, 'clicking Refresh must trigger a new fetch, not just be present in the DOM');
  assert.equal(layer.querySelectorAll('.inspectBox').length > 0, true, 'a fresh (matching-orientation) tree draws boxes again once Refresh resolves');
});

test('destroy() removes the layer\'s click listener -- a stray click afterwards does nothing (review round 3, N27)', async () => {
  const { layer, card } = layerAndCard();
  const m = mount(layer, {
    targetId: () => 'x',
    screenRect: () => ({ left: 0, top: 0, width: 1080, height: 2424 }),
    fetchImpl: async () => jsonResponse(200, fixture),
    card,
  });
  m.run();
  await tick(2);
  layer.dispatch('click', { clientX: 90, clientY: 300 });
  await tick(2);
  assert.ok(card.querySelectorAll('dt').length > 0, 'sanity: a click before destroy() does select a node');
  m.destroy();
  card.innerHTML = ''; // fakedom's innerHTML setter clears children, same as a real element
  layer.dispatch('click', { clientX: 90, clientY: 300 });
  await tick(2);
  assert.equal(card.querySelectorAll('dt').length, 0, 'a click after destroy() must not still be wired to the (possibly torn-down) layer');
  assert.equal(card.innerHTML, '', 'review round 3 F1: read innerHTML directly too, not only through querySelectorAll');
});

test('relayout() redraws boxes at a new canvas rect without a fetch', async () => {
  const { layer, card } = layerAndCard();
  let fetches = 0;
  let rect = { left: 0, top: 0, width: 1080, height: 2424 };
  const m = mount(layer, {
    targetId: () => 'x',
    screenRect: () => rect,
    fetchImpl: async () => { fetches++; return jsonResponse(200, fixture); },
    card,
  });
  m.run();
  await tick(2);
  assert.equal(fetches, 1);
  // The root box (bounds [0,0,W,H]) has left/top 0 at any scale, so its
  // width is what proves a rescale happened; a box further down the tree
  // (its left) is checked too, since a mutant scaling only one axis should
  // not pass by accident.
  const before = layer.querySelectorAll('.inspectBox').map((b) => [b.style.left, b.style.width]);
  rect = { left: 0, top: 0, width: 540, height: 1212 }; // resized to half
  m.relayout();
  assert.equal(fetches, 1, 'relayout must not re-fetch');
  const after = layer.querySelectorAll('.inspectBox').map((b) => [b.style.left, b.style.width]);
  assert.equal(after[0][1], '540px', 'the root box\'s width must halve with the canvas');
  assert.notEqual(before[0][1], after[0][1], 'boxes must resize to match the new canvas rect');
  const movedLeft = before.some((b, i) => b[0] !== after[i][0]);
  assert.ok(movedLeft, 'at least one non-root box must also move on its left edge');
});

// --- review round 4 ----------------------------------------------------

test('boxes are positioned at the layer-relative canvas offset, not just scaled (review round 4, N14/F6)', async () => {
  const { layer, card } = layerAndCard();
  // The layer covers the whole bezel (framed mode, trap 40); the canvas sits
  // inset within it. screenRect() is viewport-absolute, so mount() must
  // subtract the LAYER's own offset, not assume the layer starts at (0,0).
  layer._rect = { left: 20, top: 40, width: 1080, height: 2424 };
  const rect = { left: 79, top: 95, width: 1080, height: 2424 }; // canvas offset (59,55) from the layer
  const m = mount(layer, {
    targetId: () => 'x',
    screenRect: () => rect,
    fetchImpl: async () => jsonResponse(200, fixture),
    card,
  });
  m.run();
  await tick(2);
  const boxes = layer.querySelectorAll('.inspectBox');
  assert.ok(boxes.length > 0, 'sanity: boxes were drawn');
  const root = boxes[0];
  // The top-level node (the ScrollView) has bounds [0,220,1080,2424], so at
  // scale 1 its box must sit at the canvas's offset within the layer --
  // 79-20=59, 95-40=55 -- plus its own device-pixel origin (0,220).
  assert.equal(root.style.left, '59px', 'left must be the canvas offset relative to the LAYER, not the viewport');
  assert.equal(root.style.top, '275px', 'top must be the canvas offset (55) plus the node\'s own y1 (220), relative to the LAYER not the viewport');
});

test('hit testing is correct on x against a node that is narrow in x (review round 4, N16)', async () => {
  const { layer, card } = layerAndCard();
  // Same framed geometry shape as the HIGH-2/round-3 test, but left and top
  // are deliberately far apart (14.75 vs 300) rather than nearly equal, so a
  // mutation that confuses the two axes -- using canvas.top for the x term,
  // or dropping the x offset while keeping the y one -- moves the computed
  // point far enough to miss, rather than landing inside a wide target by
  // coincidence. The chevron fixture node is 40 device-px wide (bounds
  // [1000,290,1040,430]) and 140 tall, so it is narrow specifically on the
  // axis this test needs to pin.
  const rect = { left: 14.75, top: 300, width: 270, height: 606 };
  const m = mount(layer, {
    targetId: () => 'x',
    screenRect: () => rect,
    fetchImpl: async () => jsonResponse(200, fixture),
    card,
  });
  layer._rect = { left: 5, top: 7, width: 200, height: 200 };
  m.run();
  await tick(2);
  // Device center (1020, 360) at 0.25 scale: clientX = 14.75 + 1020*0.25 =
  // 269.75, clientY = 300 + 360*0.25 = 390.
  layer.dispatch('click', { clientX: 269.75, clientY: 390 });
  const dts = card.querySelectorAll('dt');
  assert.ok(dts.length > 0, 'the click must have selected a node and rendered its card');
  assert.ok(card.textContent.includes('network_and_internet_chevron'), 'a click on the narrow-in-x chevron must resolve x independently of y');

  // The failure mode: using clientY's offset (300) for the x term too shifts
  // device x from 1020 down to (269.75-300)/0.25, which is negative and
  // nowhere near the chevron -- confirmed directly against nodeAt rather
  // than only through a mutated file.
  const confusedAxes = nodeAt(fixture.nodes, (269.75 - 300) / 0.25, (390 - 300) / 0.25);
  assert.ok(!(confusedAxes && describeNode(confusedAxes).some((l) => l.includes('network_and_internet_chevron'))), 'sanity: using top for the x offset really does miss the chevron');
});

test('notifyResize while inspect is off is remembered, so turning it on afterwards fetches and draws (review round 4, R6)', async () => {
  const { layer, card } = layerAndCard();
  let fetches = 0;
  let size = { w: 1080, h: 2424 }; // portrait
  const m = mount(layer, {
    targetId: () => 'x',
    screenRect: () => ({ left: 0, top: 0, width: size.w, height: size.h }),
    fetchImpl: async () => { fetches++; return jsonResponse(200, fixture); },
    card,
  });
  // Rotate to landscape while off -- never turned on, never fetched yet.
  size = { w: 2424, h: 1080 };
  m.notifyResize(size.w, size.h);
  assert.equal(fetches, 0, 'sanity: no fetch happens while off');
  // Now turn on (run() toggles on when off, same as the "run() fetches and
  // turns boxes on" test above). The fixture (portrait) must be recognised
  // as stale against the landscape size that was recorded while off, not
  // treated as fresh just because it is the first fetch.
  m.run();
  await tick(2);
  assert.equal(fetches, 1, 'turning on must fetch');
  const stale = layer.querySelectorAll('.inspectStale');
  assert.ok(stale.length > 0, 'the size recorded while off must still be honoured: the portrait fixture is stale against the landscape size');
});

// --- the Tree tab's model ---------------------------------------------------

const { pathTo, pickTarget, initialExpanded, visibleRows, rowLabel, crumb, tapRequest } = await import('./inspect.js');
const scroll = fixture.nodes[0];
const network = scroll.children[0];

test('pathTo walks from the root to the node, keyed by index path', () => {
  const p = pathTo(fixture.nodes, network.children[1]);
  assert.deepEqual(p.map((x) => x.key), ['0', '0.0', '0.0.1']);
  assert.deepEqual(pathTo(fixture.nodes, { class: 'Nowhere' }), []);
});

test('a tap on a row\'s text selects the clickable row, not the TextView', () => {
  assert.equal(pickTarget(fixture.nodes, 464, 325), network);
});

test('a tap with nothing clickable above it selects the deepest hit', () => {
  const wifi = scroll.children[2];
  assert.equal(pickTarget(fixture.nodes, 464, 880), wifi.children[0]);
});

// The shape Hinge_Pixel_Fold's launcher returned: icons under a ScrollView,
// then a later sibling "Home" View covering the whole display.
const homeScreen = [{ class: 'FrameLayout', bounds: [0, 0, 2208, 1840], children: [
  { class: 'ScrollView', bounds: [0, 0, 2208, 1840], children: [
    { class: 'TextView', text: 'Photos', clickable: true, bounds: [604, 1064, 814, 1332] },
  ] },
  { class: 'View', desc: 'Home', bounds: [0, 74, 2208, 1840] },
] }];

test('a tap on an icon selects it, not a later full-screen sibling', () => {
  assert.equal(pickTarget(homeScreen, 650, 1150).text, 'Photos');
});

test('with nothing clickable under it, a tap selects the smallest node', () => {
  assert.equal(pickTarget(homeScreen, 100, 1500).class, 'View');
});

test('a tap outside every node selects nothing', () => {
  assert.equal(pickTarget(homeScreen, 5000, 5000), null);
});

test('the first tree opens single-child chains down to where it branches', () => {
  const open = initialExpanded(fixture.nodes);
  assert.deepEqual([...open], ['0']);
  const rows = visibleRows(fixture.nodes, open);
  assert.deepEqual(rows.map((r) => r.key), ['0', '0.0', '0.1', '0.2', '0.3']);
  assert.equal(rows[1].depth, 1);
  assert.equal(rows[1].hasChildren, true);
  assert.equal(rows[1].open, false);
});

test('a row without text borrows its first descendant\'s, unquoted', () => {
  assert.deepEqual(rowLabel(network), { cls: 'LinearLayout', text: 'Network & internet' });
  assert.deepEqual(rowLabel(network.children[0]), { cls: 'TextView', text: '"Network & internet"' });
});

test('a layout whose text is two levels down borrows nothing', () => {
  const chain = { class: 'FrameLayout', children: [{ class: 'LinearLayout', children: [{ class: 'TextView', text: 'At a glance' }] }] };
  assert.deepEqual(rowLabel(chain), { cls: 'FrameLayout', text: '' });
  assert.deepEqual(rowLabel(chain.children[0]), { cls: 'LinearLayout', text: 'At a glance' });
});

test('a password field is masked in its row and never lent to its parent', () => {
  const wifi = scroll.children[2];
  const field = wifi.children[1];
  assert.ok(field.password, 'fixture: the EditText is a password field');
  assert.doesNotMatch(rowLabel(field).text, /hunter2/);
  assert.equal(rowLabel(wifi).text, 'Wi-Fi password');
});

test('the crumb names the last two classes and Tap it taps the centre', () => {
  assert.equal(crumb(pathTo(fixture.nodes, network.children[0])), 'LinearLayout › TextView');
  assert.deepEqual(tapRequest(network), { action: 'tap', x: 540, y: 360 });
});

const { geometryText } = await import('./inspect.js');

test('geometryText is x y w×h px, with depth only in 3D, and empty without bounds', () => {
  assert.equal(geometryText({ bounds: [10, 20, 110, 70] }), 'x 10  y 20  100×50 px');
  assert.equal(geometryText({ bounds: [10, 20, 110, 70] }, 3), 'x 10  y 20  100×50 px   depth 3');
  assert.equal(geometryText({ bounds: [10, 20, 110, 70] }, 0), 'x 10  y 20  100×50 px   depth 0');
  assert.equal(geometryText({}), '');
  assert.equal(geometryText({ bounds: [1, 2] }, 1), '');
});

const { layers3d, orbit, pinchSpacing, ORBIT_LIMIT, SPACING_MAX, MAX_PLANES } = await import('./inspect.js');

test('layers3d gives each node with an area a plane at its depth, keyed like the rows', () => {
  const nodes = [{ bounds: [0, 0, 100, 200], children: [
    { bounds: [0, 0, 100, 50] },
    { bounds: [0, 0, 0, 0], children: [{ bounds: [10, 10, 20, 20] }] },
  ] }];
  const { planes, maxDepth } = layers3d(nodes);
  assert.deepEqual(planes.map((p) => [p.key, p.depth]), [['0', 0], ['0.0', 1], ['0.1.0', 2]]);
  assert.equal(maxDepth, 2);
});

test('layers3d keeps the shallowest planes past the cap', () => {
  const deep = { bounds: [0, 0, 1, 1], children: [] };
  let cur = deep;
  for (let i = 0; i < MAX_PLANES + 50; i++) {
    const n = { bounds: [0, 0, 1, 1], children: [] };
    cur.children.push(n);
    cur = n;
  }
  const { planes, maxDepth } = layers3d([deep]);
  assert.equal(planes.length, MAX_PLANES);
  assert.equal(maxDepth, MAX_PLANES - 1);
});

test('layers3d treats null as no planes', () => {
  assert.deepEqual(layers3d(null), { planes: [], maxDepth: 0 });
});

test('orbit turns with the drag and stops short of edge-on', () => {
  assert.deepEqual(orbit({ x: 0, y: 0 }, 10, 0), { x: 0, y: 4 });
  assert.deepEqual(orbit({ x: 0, y: 0 }, 0, -10), { x: 4, y: 0 });
  assert.deepEqual(orbit({ x: 0, y: 0 }, 1000, 1000), { x: -ORBIT_LIMIT, y: ORBIT_LIMIT });
});

test('pinchSpacing scales with the fingers, opens from flat and stays in range', () => {
  assert.equal(pinchSpacing(20, 100, 200), 40);
  assert.equal(pinchSpacing(0, 100, 300), 12);
  assert.equal(pinchSpacing(60, 100, 1000), SPACING_MAX);
  assert.equal(pinchSpacing(20, 0, 100), 20);
});

// --- composable names (/api/ui?compose=1) ------------------------------

const composeRow = {
  class: 'View', composable: 'MessageRow', source: 'MessageList.kt:661', clickable: true,
  bounds: [53, 1674, 1027, 2125], center: [540, 1899], children: [],
};

test('rowLabel names a row after its composable, not its view class', () => {
  assert.equal(rowLabel(composeRow).cls, 'MessageRow');
  assert.equal(rowLabel({ class: 'View' }).cls, 'View');
});

test('describe lists the composable and its source right after the class', () => {
  const lines = describeNode(composeRow);
  const at = lines.indexOf('class: View');
  assert.deepEqual(lines.slice(at, at + 3), ['class: View', 'composable: MessageRow', 'source: MessageList.kt:661']);
});

test('crumb uses the composable name the row shows', () => {
  assert.equal(crumb([{ node: { class: 'ComposeView' } }, { node: composeRow }]), 'ComposeView › MessageRow');
});

function composeTree(extra = {}) {
  const cv = { class: 'ComposeView', bounds: [0, 0, 1080, 2424], children: [composeRow] };
  return { width: 1080, height: 2424, rotation: 0, nodes: [cv], ...extra };
}

function mountSheet(fetchImpl) {
  installFakeDom();
  const layer = document.createElement('div');
  const sheet = document.createElement('div');
  const m = mount(layer, {
    targetId: () => 'x',
    screenRect: () => ({ left: 0, top: 0, width: 1080, height: 2424 }),
    fetchImpl, sheet, tab: 'tree',
  });
  return { m, sheet };
}

test('the tree fetch asks for compose=1 only once the Compose toggle is on', async () => {
  const urls = [];
  const { m, sheet } = mountSheet(async (url) => { urls.push(url); return jsonResponse(200, composeTree()); });
  m.run();
  await tick(2);
  assert.ok(!urls[0].includes('compose=1'), urls[0]);
  const btn = sheet.querySelector('.inspectCompose');
  assert.equal(btn.getAttribute('aria-pressed'), 'false');
  btn.dispatch('click');
  await tick(2);
  assert.equal(btn.getAttribute('aria-pressed'), 'true');
  assert.ok(urls[1].includes('&compose=1'), urls[1]);
});

test('a composeError shows above the tree and the rows still render', async () => {
  const { m, sheet } = mountSheet(async () => jsonResponse(200, composeTree({ composeError: 'com.example: app is not debuggable' })));
  m.run();
  await tick(2);
  const note = sheet.querySelector('.inspectComposeError');
  assert.equal(note.hidden, false);
  assert.equal(note.textContent, 'com.example: app is not debuggable');
  assert.ok(sheet.querySelectorAll('.inspectRow').length > 0);
});

test('no composeError leaves the note hidden', async () => {
  const { m, sheet } = mountSheet(async () => jsonResponse(200, composeTree()));
  m.run();
  await tick(2);
  assert.equal(sheet.querySelector('.inspectComposeError').hidden, true);
});

await Promise.all(pending);
console.log(failures ? `\n${failures} failing` : '\nall passing');
process.exit(failures ? 1 : 0);
