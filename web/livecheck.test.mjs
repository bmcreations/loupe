// Tests for livecheck.js. Run: node web/livecheck.test.mjs
//
// check() is pure, so every rule is pinned with a hand-built snapshot: one
// that passes, and one per rule that breaks only that rule and must fail with
// only that rule's message.
import assert from 'node:assert/strict';

const { check, collect } = await import('./livecheck.js');

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

// 390x844 portrait, left hand: the device centred in the stage, the nav stack
// in the bottom-left gutter and the overflow button in the bottom-right.
function base() {
  const screen = { x: 60, y: 80, w: 270, h: 600 };
  return {
    hand: 'left',
    frame: 'skin',
    viewport: { w: 390, h: 844 },
    stream: { w: 1080, h: 2424 },
    rects: {
      screen,
      mask: { ...screen },
      art: { x: 45, y: 66, w: 300, h: 628 },
      pill: { x: 120, y: 12, w: 150, h: 36 },
      nav: { x: 8, y: 688, w: 44, h: 148 },
      more: { x: 338, y: 792, w: 44, h: 44 },
      stage: { x: 0, y: 60, w: 390, h: 784 },
    },
  };
}

function only(failures, re) {
  assert.equal(failures.length, 1, `want exactly one failure, got ${JSON.stringify(failures)}`);
  assert.match(failures[0], re);
}

test('a correctly laid out page passes', () => {
  assert.deepEqual(check(base()), []);
});

test('the mirrored right-hand layout passes', () => {
  const s = base();
  s.hand = 'right';
  s.rects.nav = { x: 338, y: 688, w: 44, h: 148 };
  s.rects.more = { x: 8, y: 792, w: 44, h: 44 };
  assert.deepEqual(check(s), []);
});

// --- mask (trap 40) ------------------------------------------------------

test('a skin mask stretched to the frame box fails', () => {
  const s = base();
  s.rects.mask = { ...s.rects.art };
  only(check(s), /^mask: .* does not match screen/);
});

test('a skin mask one pixel off the screen fails', () => {
  const s = base();
  s.rects.mask = { ...s.rects.screen, x: s.rects.screen.x + 1 };
  only(check(s), /^mask:/);
});

test('sub-pixel disagreement between mask and screen is not a failure', () => {
  const s = base();
  s.rects.mask = { ...s.rects.screen, w: s.rects.screen.w + 0.3 };
  assert.deepEqual(check(s), []);
});

test('a skin frame shown with no mask fails', () => {
  const s = base();
  s.rects.mask = null;
  only(check(s), /^mask: skin frame shown without a mask/);
});

test('with no skin frame shown the mask is not checked', () => {
  for (const frame of [null, 'derived']) {
    const s = base();
    s.frame = frame;
    s.rects.mask = { ...s.rects.art };
    assert.deepEqual(check(s), [], `frame ${frame}`);
  }
});

// --- screen inside stage (trap 18) ---------------------------------------

test('a screen running off the bottom of the stage fails', () => {
  const s = base();
  s.rects.stage = { x: 0, y: 60, w: 390, h: 600 };
  only(check(s), /overflows stage .* vertically/);
});

test('a screen running off the top of the stage fails', () => {
  const s = base();
  s.rects.stage = { x: 0, y: 100, w: 390, h: 744 };
  only(check(s), /overflows stage .* vertically/);
});

test('a screen running off the left of the stage fails', () => {
  const s = base();
  s.rects.stage = { x: 70, y: 60, w: 320, h: 784 };
  only(check(s), /overflows stage .* horizontally/);
});

test('a screen running off the right of the stage fails', () => {
  const s = base();
  s.rects.stage = { x: 0, y: 60, w: 320, h: 784 };
  only(check(s), /overflows stage .* horizontally/);
});

// --- corners do not cover the screen -------------------------------------

test('a nav stack over the screen fails', () => {
  const s = base();
  s.rects.nav = { x: 50, y: 600, w: 44, h: 148 };
  only(check(s), /^nav: .* overlaps screen/);
});

test('an overflow button over the screen fails', () => {
  const s = base();
  s.rects.more = { x: 300, y: 650, w: 44, h: 44 };
  only(check(s), /^more: .* overlaps screen/);
});

test('a corner sharing the screen\'s columns but below it does not fail', () => {
  const s = base();
  s.rects.more = { x: 200, y: 700, w: 44, h: 44 };
  assert.deepEqual(check(s), []);
});

test('corners that only touch the screen\'s edge do not fail', () => {
  const s = base();
  s.rects.nav = { x: 16, y: 600, w: 44, h: 148 }; // right edge at 60, the screen's left
  assert.deepEqual(check(s), []);
});

// --- nav follows the hand ------------------------------------------------

test('a nav stack on the left under a right hand fails', () => {
  const s = base();
  s.hand = 'right';
  s.rects.more = { x: 8, y: 792, w: 44, h: 44 };
  only(check(s), /^nav: on the left, hand is right/);
});

test('a nav stack on the right under a left hand fails', () => {
  const s = base();
  s.rects.nav = { x: 338, y: 688, w: 44, h: 148 };
  s.rects.more = { x: 8, y: 792, w: 44, h: 44 };
  only(check(s), /^nav: on the right, hand is left/);
});

test('a hidden nav stack is not on the wrong side', () => {
  const s = base();
  s.hand = 'right';
  s.rects.nav = null;
  s.rects.more = { x: 8, y: 792, w: 44, h: 44 };
  assert.deepEqual(check(s), []);
});

// --- collect -------------------------------------------------------------

function fakeNode(rect, { hidden = false, classes = [], width, height } = {}) {
  return {
    hidden,
    width,
    height,
    classList: { contains: (c) => classes.includes(c) },
    closest: () => null,
    getBoundingClientRect: () => ({ left: rect.x, top: rect.y, width: rect.w, height: rect.h }),
  };
}

// A pane root: its parts are found by data-r role, as app.js clones them.
function fakePane(rect, roles, classes = []) {
  const root = fakeNode(rect, { classes });
  root.querySelector = (sel) => {
    const m = /^\[data-r="?(\w+)"?\]$/.exec(sel);
    if (m) return roles[m[1]] || null;
    if (sel.startsWith('.')) return roles[sel.slice(1)] || null;
    return null;
  };
  return root;
}

function fakeDoc(nodes, rootClasses = [], panes = null) {
  // A page with no panes listed has the one pane the nodes describe.
  const list = panes || [fakePane({ x: 0, y: 0, w: 0, h: 0 }, {
    wrap: nodes.screenWrap, canvas: nodes.screen, frameMask: nodes.frameMask, frameArt: nodes.frameArt,
  }, ['focused'])];
  return {
    documentElement: { classList: { contains: (c) => rootClasses.includes(c) } },
    defaultView: { innerWidth: 844, innerHeight: 390 },
    getElementById: (id) => nodes[id] || null,
    querySelector: (sel) => nodes[sel.slice(1)] || null,
    querySelectorAll: (sel) => (sel === '.pane' ? list : []),
  };
}

test('collect reads hand, frame, stream and rects off the page', () => {
  const s = base();
  const doc = fakeDoc({
    screenWrap: fakeNode(s.rects.screen, { classes: ['framed', 'skin'] }),
    screen: fakeNode(s.rects.screen, { width: 1080, height: 2424 }),
    frameMask: fakeNode(s.rects.mask),
    frameArt: fakeNode(s.rects.art),
    pill: fakeNode(s.rects.pill),
    cornerNav: fakeNode(s.rects.nav, { hidden: true }),
    cornerMore: fakeNode(s.rects.more),
    stage: fakeNode(s.rects.stage),
  }, ['hand-right']);
  const got = collect(doc);
  assert.equal(got.hand, 'right');
  assert.equal(got.frame, 'skin');
  assert.deepEqual(got.viewport, { w: 844, h: 390 });
  assert.deepEqual(got.stream, { w: 1080, h: 2424 });
  assert.deepEqual(got.rects.screen, s.rects.screen);
  assert.deepEqual(got.rects.mask, s.rects.mask);
  assert.equal(got.rects.nav, null, 'a hidden element reads as absent');
  assert.deepEqual(got.rects.more, s.rects.more);
});

test('collect reports no frame, and no mask, when the wrap is not framed', () => {
  const s = base();
  const doc = fakeDoc({
    screenWrap: fakeNode(s.rects.screen, { classes: ['skin'] }),
    screen: fakeNode(s.rects.screen, { width: 1080, height: 2424 }),
    frameMask: fakeNode(s.rects.mask),
    stage: fakeNode(s.rects.stage),
  });
  const got = collect(doc);
  assert.equal(got.hand, 'left');
  assert.equal(got.frame, null);
  assert.equal(got.rects.mask, null);
  assert.equal(got.rects.pill, null);
});


// 390x844 with the top bar and corners gone: the stage is the viewport less
// the safe areas, and the stream is a contain fit centred in it.
function immersed() {
  const stage = { x: 0, y: 47, w: 390, h: 763 };
  const screen = { x: 20, y: 47, w: 350, h: 763 };
  return {
    hand: 'left', frame: null, immersive: true,
    viewport: { w: 390, h: 844 }, stream: { w: 496, h: 1080 },
    rects: { screen, mask: null, art: null, pill: null, nav: null, more: null, stage },
  };
}

test('immersive: a centred contain fit with nothing else shown passes', () => {
  assert.deepEqual(check(immersed()), []);
});

test('immersive: a control left on screen fails', () => {
  const snap = immersed();
  snap.rects.more = { x: 338, y: 758, w: 44, h: 44 };
  const out = check(snap);
  assert.ok(out.some((f) => /immersive: more is still shown/.test(f)), JSON.stringify(out));
});

test('immersive: a bezel left on fails', () => {
  const snap = immersed();
  snap.frame = 'derived';
  only(check(snap), /immersive: a derived frame is still shown/);
});

test('immersive: a screen that fills neither axis fails', () => {
  const snap = immersed();
  snap.rects.screen = { x: 45, y: 97, w: 300, h: 663 };
  only(check(snap), /does not fill stage/);
});

test('immersive: a top-aligned screen fails', () => {
  const snap = immersed();
  snap.rects.stage = { x: 0, y: 0, w: 390, h: 1000 };
  snap.rects.screen = { x: 0, y: 0, w: 390, h: 849 };
  only(check(snap), /is not centred/);
});

// The top banners (control.js's "driving", clipboard.js's offer) sit under
// the top bar, not on it: over the pill they hid the target's name, and the
// offer, a button, took the pill's taps.
test('a banner below the top bar passes', () => {
  const s = base();
  s.rects.topbar = { x: 0, y: 0, w: 390, h: 60 };
  s.rects.topbar = { x: 0, y: 0, w: 390, h: 46 };
  s.rects.control = { x: 110, y: 51, w: 170, h: 28 };
  s.rects.clip = { x: 100, y: 51, w: 190, h: 28 };
  assert.deepEqual(check(s), []);
});

test('a banner over the top bar fails', () => {
  const s = base();
  s.rects.topbar = { x: 0, y: 0, w: 390, h: 60 };
  s.rects.control = { x: 110, y: 12, w: 170, h: 30 };
  only(check(s), /control: .* overlaps topbar/);
});

test('a banner over the device screen passes', () => {
  const s = base();
  s.rects.control = { x: 110, y: 70, w: 170, h: 30 };
  assert.deepEqual(check(s), []);
});

test('immersive: a banner over the video is expected', () => {
  const s = immersed();
  s.rects.control = { x: 110, y: 83, w: 170, h: 30 };
  assert.deepEqual(check(s), []);
});

test('a clipboard offer over the immersive handle fails', () => {
  const s = immersed();
  s.rects.handle = { x: 147, y: 47, w: 96, h: 28 };
  s.rects.clip = { x: 100, y: 59, w: 190, h: 34 };
  only(check(s), /clip: .* overlaps handle/);
});

test('a record banner over the top bar fails, and over the screen passes', () => {
  const s = base();
  s.rects.topbar = { x: 0, y: 0, w: 390, h: 60 };
  s.rects.rec = { x: 100, y: 12, w: 190, h: 34 };
  only(check(s), /rec: .* overlaps topbar/);
  const t = base();
  t.rects.rec = { x: 100, y: 70, w: 190, h: 34 };
  assert.deepEqual(check(t), []);
});

test('collect reads the top bar, the handle and the banners by class', () => {
  const r = { x: 1, y: 2, w: 3, h: 4 };
  const got = collect(fakeDoc({
    topbar: fakeNode(r), immersiveHandle: fakeNode(r),
    controlBanner: fakeNode(r), clipOffer: fakeNode(r, { hidden: true }), recBanner: fakeNode(r),
  }));
  assert.deepEqual(got.rects.topbar, r);
  assert.deepEqual(got.rects.handle, r);
  assert.deepEqual(got.rects.control, r);
  assert.equal(got.rects.clip, null);
  assert.deepEqual(got.rects.rec, r);
});

// --- inspect --------------------------------------------------------------

// Inspect's sheet sits under the device, and the device is refitted above it.
test('a device above the inspect sheet passes', () => {
  const s = base();
  s.rects.nav = null;
  s.rects.more = null;
  s.rects.screen = { x: 125, y: 70, w: 140, h: 300 };
  s.rects.mask = { ...s.rects.screen };
  s.rects.inspect = { x: 0, y: 380, w: 390, h: 464 };
  assert.deepEqual(check(s), []);
});

test('a device drawn under the inspect sheet fails', () => {
  const s = base();
  s.rects.inspect = { x: 0, y: 380, w: 390, h: 464 };
  only(check(s), /inspect: sheet .* overlaps screen/);
});

test('the full-screen 3D sheet may cover the device', () => {
  const s = base();
  s.rects.inspect = { x: 0, y: 0, w: 390, h: 844 };
  s.inspectFull = true;
  assert.deepEqual(check(s), []);
});

// --- docked panels -------------------------------------------------------

// 1280x800, right hand: the picker docked as a 360px panel on the left and
// Tools as one on the right. The stage keeps the viewport's width and pads
// both panels out, and the corners sit at the panels' inner edges.
function docked() {
  const screen = { x: 480, y: 70, w: 320, h: 718 };
  return {
    hand: 'right',
    frame: null,
    docked: true,
    viewport: { w: 1280, h: 800 },
    stream: { w: 1080, h: 2424 },
    rects: {
      screen,
      pill: { x: 565, y: 12, w: 150, h: 36 },
      nav: { x: 860, y: 640, w: 44, h: 148 },
      more: { x: 376, y: 744, w: 44, h: 44 },
      stage: { x: 0, y: 60, w: 1280, h: 740 },
      drawer: { x: 0, y: 60, w: 360, h: 740 },
      sheet: { x: 920, y: 60, w: 360, h: 740 },
    },
  };
}

test('both docked panels beside the device pass', () => {
  assert.deepEqual(check(docked()), []);
});

test('at 900px a right-hand nav at the inner edge of Tools is on the right', () => {
  // Its centre, 512, is right of the viewport's middle only by a little,
  // and the middle between the panels, 450, is what decides.
  const s = docked();
  s.viewport = { w: 900, h: 800 };
  s.rects.stage = { x: 0, y: 60, w: 900, h: 740 };
  s.rects.drawer = { x: 0, y: 60, w: 252, h: 740 };
  s.rects.sheet = { x: 648, y: 60, w: 252, h: 740 };
  s.rects.screen = { x: 330, y: 140, w: 240, h: 600 };
  s.rects.pill = { x: 375, y: 12, w: 150, h: 36 };
  s.rects.nav = { x: 588, y: 640, w: 44, h: 148 };
  s.rects.more = { x: 268, y: 744, w: 44, h: 44 };
  assert.deepEqual(check(s), []);
});

test('at 900px a left-hand nav at the inner edge of the picker is on the left', () => {
  const s = docked();
  s.hand = 'left';
  s.viewport = { w: 900, h: 800 };
  s.rects.stage = { x: 0, y: 60, w: 900, h: 740 };
  s.rects.drawer = { x: 0, y: 60, w: 252, h: 740 };
  s.rects.sheet = { x: 648, y: 60, w: 252, h: 740 };
  s.rects.screen = { x: 330, y: 140, w: 240, h: 600 };
  s.rects.pill = { x: 375, y: 12, w: 150, h: 36 };
  s.rects.nav = { x: 268, y: 640, w: 44, h: 148 };
  s.rects.more = { x: 588, y: 744, w: 44, h: 44 };
  assert.deepEqual(check(s), []);
});

test('a docked panel over the screen fails', () => {
  const s = docked();
  s.rects.sheet = { x: 700, y: 60, w: 360, h: 740 };
  s.rects.nav = { x: 1200, y: 640, w: 44, h: 148 };
  const out = check(s);
  assert.ok(out.some((f) => /^sheet: panel .* overlaps screen/.test(f)), JSON.stringify(out));
});

test('a corner left under the picker fails', () => {
  const s = docked();
  s.rects.more = { x: 16, y: 744, w: 44, h: 44 };
  only(check(s), /^more: .* is under the drawer panel/);
});

test('inspect in the right slot beside the picker passes', () => {
  const s = docked();
  s.rects.sheet = null;
  s.rects.inspect = { x: 920, y: 60, w: 360, h: 740 };
  assert.deepEqual(check(s), []);
});

test('inspect under the picker fails', () => {
  const s = docked();
  s.rects.sheet = null;
  s.rects.inspect = { x: 0, y: 60, w: 360, h: 740 };
  only(check(s), /^inspect: panel .* is under the drawer panel/);
});

test('below the breakpoint a sheet may cover the device', () => {
  const s = base();
  s.rects.sheet = { x: 0, y: 400, w: 390, h: 444 };
  assert.deepEqual(check(s), []);
});

test('docked, the 3D tab is a panel too and may not cover the device', () => {
  const s = docked();
  s.rects.sheet = null;
  s.rects.inspect = { x: 560, y: 52, w: 480, h: 748 };
  s.inspectFull = true;
  const out = check(s);
  assert.ok(out.some((f) => /inspect: sheet .* overlaps screen/.test(f)), JSON.stringify(out));
});

test('docked full screen hides both panels', () => {
  const s = docked();
  s.immersive = true;
  s.rects.pill = null;
  s.rects.nav = null;
  s.rects.more = null;
  s.rects.stage = { x: 0, y: 0, w: 1280, h: 800 };
  s.rects.screen = { x: 462, y: 0, w: 356, h: 800 };
  const out = check(s);
  assert.ok(out.some((f) => /^immersive: sheet is still shown/.test(f)), JSON.stringify(out));
  assert.ok(out.some((f) => /^immersive: drawer is still shown/.test(f)), JSON.stringify(out));
  s.rects.sheet = null;
  s.rects.drawer = null;
  assert.deepEqual(check(s), []);
});

// --- several panes ---------------------------------------------------------

// Two panes docked at 1280x800, the drawer and Tools beside them, the
// corners in the band under the panes.
function twoPanes() {
  const s = docked();
  const a = { cell: { x: 380, y: 70, w: 260, h: 640 }, screen: { x: 395, y: 110, w: 230, h: 516 } };
  const b = { cell: { x: 650, y: 70, w: 260, h: 640 }, screen: { x: 665, y: 110, w: 230, h: 516 } };
  s.rects.screen = a.screen;
  s.rects.nav = { x: 860, y: 740, w: 44, h: 44 };
  s.rects.more = { x: 376, y: 744, w: 44, h: 44 };
  s.panes = [
    { index: 1, focused: true, frame: null, cell: a.cell, screen: a.screen, mask: null },
    { index: 2, focused: false, frame: null, cell: b.cell, screen: b.screen, mask: null },
  ];
  return s;
}

test('two panes side by side pass', () => {
  assert.deepEqual(check(twoPanes()), []);
});

test("a pane's screen past its own cell fails", () => {
  const s = twoPanes();
  s.panes[1].screen = { ...s.panes[1].screen, w: 250 };
  only(check(s), /pane 2: screen .* overflows its pane/);
});

test('panes that overlap fail', () => {
  const s = twoPanes();
  s.panes[1].cell = { ...s.panes[1].cell, x: 600 };
  s.panes[1].screen = { ...s.panes[1].screen, x: 615 };
  const got = check(s);
  assert.ok(got.some((f) => /pane 2: .* overlaps pane 1/.test(f)), JSON.stringify(got));
});

test('a second pane under a docked panel fails', () => {
  const s = twoPanes();
  s.rects.sheet = { x: 880, y: 60, w: 400, h: 740 };
  const got = check(s);
  assert.ok(got.some((f) => /pane 2: sheet panel .* overlaps screen/.test(f)), JSON.stringify(got));
});

test('a corner over a second pane fails', () => {
  const s = twoPanes();
  s.rects.nav = { x: 700, y: 500, w: 44, h: 148 };
  const got = check(s);
  assert.ok(got.some((f) => /pane 2: nav .* overlaps screen/.test(f)), JSON.stringify(got));
});

test('collect reads every pane, and the top level from the focused one', () => {
  const s = twoPanes();
  const mk = (p, focused) => fakePane(p.cell, {
    wrap: fakeNode(p.screen),
    canvas: fakeNode(p.screen, { width: 1080, height: 2424 }),
  }, focused ? ['focused'] : []);
  const panes = [mk(s.panes[1], false), mk(s.panes[0], true)];
  const doc = fakeDoc({ stage: fakeNode(s.rects.stage) }, ['docked'], panes);
  const got = collect(doc);
  assert.equal(got.panes.length, 2);
  assert.deepEqual(got.panes.map((p) => p.index), [1, 2]);
  assert.deepEqual(got.panes[1].cell, s.panes[0].cell);
  assert.equal(got.panes[1].focused, true);
  assert.deepEqual(got.rects.screen, s.panes[0].screen, 'the focused pane fills the top level');
});

if (failures) {
  console.log(`\n${failures} failing`);
  process.exit(1);
}
console.log('\nall passing');

// --- panel toggles ------------------------------------------------------

test('a panel toggle shown at phone width fails', () => {
  const s = base();
  s.rects.leftPanelBtn = { x: 12, y: 8, w: 44, h: 44 };
  only(check(s), /leftPanelBtn: shown .* while not docked/);
});

test('docked panel toggles pass', () => {
  const s = docked();
  s.rects.leftPanelBtn = { x: 12, y: 8, w: 44, h: 44 };
  s.rects.rightPanelBtn = { x: 1224, y: 8, w: 44, h: 44 };
  assert.deepEqual(check(s), []);
});
