// Tests for the stage geometry. Run: node web/layout.test.mjs
//
// Cases come from testdata/client/layout.json, shared with a future KMP
// client the way orientation.test.mjs's golden geometry is. Stage and
// stream sizes are real, measured numbers (390x844 and 390x657 iPhone
// viewports, the pixel_10 skin at 1198x2531, a 2560x1600 tablet -- see
// CLAUDE.md's Device frames section). Most corner sizes are small test
// values chosen to make both branches of fitStage demonstrable at those real
// dimensions; one case reads the CSS defaults (--corner-w/-h/-gap) out of
// style.css to pin what a 402x874 iPhone gets. The corner cluster's width and height are
// independent (a gutter footprint vs. a below-the-device band footprint --
// see layout.js), which is why fixtures carry both rather than one `size`.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fitStage, placeArt, gridCells } from './layout.js';
import { turnedArt } from './orientation.js';

const fixtures = JSON.parse(
  readFileSync(new URL('../testdata/client/layout.json', import.meta.url)),
);

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

// JSON has no Infinity literal, so a fixture that needs the real number
// (pinning that positive() checks isFinite, not just > 0) spells it as the
// string "Infinity" and gets it swapped in here rather than losing the
// distinction it exists to test.
function resolveCorners(corners) {
  if (!corners) return corners;
  const resolve = (v) => (v === 'Infinity' ? Infinity : v);
  return { width: resolve(corners.width), height: resolve(corners.height), gap: resolve(corners.gap) };
}

for (const c of fixtures.fitStage) {
  test(`fitStage: ${c.name}`, () => {
    const got = fitStage(c.stage, c.box, resolveCorners(c.corners));
    assert.deepEqual(got, c.expect);
  });
}

for (const c of fixtures.placeArt) {
  test(`placeArt: ${c.name}`, () => {
    const got = placeArt(c.x, c.y, c.w, c.h, c.scale, c.turn, turnedArt);
    assert.deepEqual(got, c.expect);
  });
}

test('fitStage with no stage or box returns null rather than throwing', () => {
  assert.equal(fitStage(null, { width: 100, height: 100 }, { size: 0, gap: 0 }), null);
  assert.equal(fitStage({ width: 100, height: 100 }, null, { size: 0, gap: 0 }), null);
  assert.equal(fitStage({ width: 100, height: 100 }, { width: NaN, height: 100 }, { size: 0, gap: 0 }), null);
});

test('placeArt reuses the passed-in turnedArt rather than its own copy', () => {
  let calls = 0;
  const fakeTurnedArt = (turn, w, h) => {
    calls++;
    return { width: w, height: h, transform: `turn${turn}` };
  };
  const got = placeArt(10, 20, 100, 200, 2, 3, fakeTurnedArt);
  assert.equal(calls, 1);
  assert.deepEqual(got, { left: 20, top: 40, width: 200, height: 400, transform: 'turn3' });
});

test('placeArt turns back.webp about its layout, not its own larger image', () => {
  // pixel_10: back.webp is 1205x2535 on a 1198x2531 layout. One quarter turn
  // has to slide back by the layout's scaled height, 1265.5 -> 1266, not the
  // image's 1267.5 -> 1268, or the hole lands off the canvas.
  const got = placeArt(0, 0, 1205, 2535, 0.5, 1, turnedArt, 1198, 2531);
  assert.equal(got.transform, 'translate(1266px, 0px) rotate(90deg)');
  assert.equal(got.width, 603);
  assert.equal(got.height, 1268);
});

test('fitStage sanitises corners.height on its own, independently of width', () => {
  // A stage too narrow for the gutter fit, so only the band's availH decides
  // the scale. Width and height differ on purpose: a fixture that sets them
  // equal cannot tell height falling back to width from height being read.
  const stage = { width: 100, height: 400 };
  const box = { width: 50, height: 300 };
  const zero = fitStage(stage, box, { width: 44, height: 0, gap: 12 });
  assert.equal(zero.band, true);
  for (const h of [-500, NaN, Infinity, undefined]) {
    assert.deepEqual(fitStage(stage, box, { width: 44, height: h, gap: 12 }), zero,
      `corners.height ${h} must reserve nothing, not a negative band or width's value`);
  }
  assert.ok(fitStage(stage, box, { width: 44, height: 44, gap: 12 }).scale < zero.scale,
    'a real height must shrink the band fit, or the check above proves nothing');
});

// A 402x874 iPhone's stage is 382x808 inside its padding. With the nav L
// (100x100, gap 12) and a banner slot it was 382x774 and a 1080x2344 stream
// drew at 305x662; the one-row nav and no slot give 347x754.
test('the CSS corner defaults fit a 402x874 iPhone at 347x754', () => {
  const css = readFileSync(new URL('./style.css', import.meta.url), 'utf8');
  const px = (name) => Number(css.match(new RegExp(`${name}:\\s*(\\d+)px;`))[1]);
  const corners = { width: px('--corner-w'), height: px('--corner-h'), gap: px('--corner-gap') };
  const fit = fitStage({ width: 382, height: 808 }, { width: 1080, height: 2344 }, corners);
  assert.deepEqual([fit.width, fit.height, fit.band], [347, 754, true]);
});

// Side by side (Figma 59:178, 59:761, 59:1374): two and three panes share
// one row, four go two by two, each cell the same size.
test('gridCells is the box for one pane and one row for two or three', () => {
  const box = { x: 10, y: 20, width: 1000, height: 600 };
  assert.deepEqual(gridCells(1, box, 12), [box]);
  const two = gridCells(2, box, 12);
  assert.deepEqual(two, [
    { x: 10, y: 20, width: 494, height: 600 },
    { x: 516, y: 20, width: 494, height: 600 },
  ]);
  const three = gridCells(3, box, 12);
  assert.equal(three.length, 3);
  assert.equal(three[2].x + three[2].width, 1010);
  assert.ok(three.every((c) => c.height === 600 && c.y === 20));
});

test('gridCells puts four panes two by two', () => {
  const cells = gridCells(4, { x: 0, y: 0, width: 1000, height: 600 }, 12);
  assert.deepEqual(cells.map((c) => [c.x, c.y, c.width, c.height]), [
    [0, 0, 494, 294], [506, 0, 494, 294],
    [0, 306, 494, 294], [506, 306, 494, 294],
  ]);
});

// 59:761: three panes at 1440 with both panels docked are about 372 wide.
test('three panes in a 1440 window are about a third of the stage each', () => {
  const cells = gridCells(3, { x: 0, y: 0, width: 1120, height: 800 }, 12);
  assert.ok(cells.every((c) => c.width > 360 && c.width < 380), JSON.stringify(cells));
});

test('gridCells returns nothing for no panes or a box with no size', () => {
  assert.deepEqual(gridCells(0, { x: 0, y: 0, width: 100, height: 100 }, 12), []);
  assert.deepEqual(gridCells(2, { x: 0, y: 0, width: 0, height: 100 }, 12), []);
});

console.log(failures ? `\n${failures} failing` : '\nall passing');
process.exit(failures ? 1 : 0);
