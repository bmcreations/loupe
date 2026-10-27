// Tests for the bezel's orientation gate. Run: node web/orientation.test.mjs
//
// The geometry here is emulator-5554's own, read from /api/frame: a 1198x2531
// skin whose display rect is 1080x2424 at (59,55). The stream sizes are the
// ones cmd/rotprobe measured either side of a rotation, 480x1080 portrait and
// 1080x480 landscape.
//
// This lives in its own module because web/app.js touches the DOM at module
// scope and has no harness (trap 21), and because the wrong answer here is
// quiet: a portrait bezel around a landscape picture still renders, it just
// squashes the screen into a box a third of its width.
import assert from 'node:assert/strict';

const { frameNeedsRecheck, frameSuitsStream, frameWantedFrom, orientationOf, turnedArt } = await import('./orientation.js');

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

const pixelSkin = {
  width: 1198, height: 2531,
  display: { x: 59, y: 55, width: 1080, height: 2424 },
  source: 'skin',
};

test('a portrait skin suits the portrait stream it was fetched for', () => {
  assert.equal(frameSuitsStream(pixelSkin, 480, 1080), true);
});

test('a portrait skin does not suit the stream after a rotation', () => {
  // The whole point: scrcpy keeps streaming and the picture turns over, so
  // without this the 1080x480 canvas is fitted to a 1080x2424 display rect.
  assert.equal(frameSuitsStream(pixelSkin, 1080, 480), false);
});

test('a landscape frame suits a landscape stream', () => {
  const landscape = { width: 2531, height: 1198, display: { x: 55, y: 59, width: 2424, height: 1080 } };
  assert.equal(frameSuitsStream(landscape, 1080, 480), true);
});

test('it reads the display rect, not the outer bezel', () => {
  // A bezel can be squarer than its screen. The canvas is placed on the
  // display rect, so that is the rect whose orientation has to agree.
  const wideBezel = { width: 1200, height: 1200, display: { x: 60, y: 0, width: 1080, height: 2424 } };
  assert.equal(frameSuitsStream(wideBezel, 1080, 480), false);
  assert.equal(frameSuitsStream(wideBezel, 480, 1080), true);
});

test('a square display rect suits either orientation', () => {
  const square = { width: 1200, height: 1200, display: { x: 60, y: 60, width: 1080, height: 1080 } };
  assert.equal(frameSuitsStream(square, 1080, 480), true);
  assert.equal(frameSuitsStream(square, 480, 1080), true);
});

test('a square stream suits either frame', () => {
  assert.equal(frameSuitsStream(pixelSkin, 1080, 1080), true);
});

test('no frame is not a suitable frame', () => {
  assert.equal(frameSuitsStream(null, 480, 1080), false);
  assert.equal(frameSuitsStream(undefined, 480, 1080), false);
});

test('a frame with no display rect is rejected rather than assumed to fit', () => {
  // /api/frame always sends one, so this is a malformed response; drawing a
  // bezel positioned off a missing rect is worse than drawing none.
  assert.equal(frameSuitsStream({ width: 1198, height: 2531 }, 480, 1080), false);
  assert.equal(frameSuitsStream({ display: {} }, 480, 1080), false);
});

test('a canvas that has not been sized yet is rejected', () => {
  // fitScreen runs from a ResizeObserver and can fire before the first init,
  // when canvas.width is 0. Answering "suits" there would show the bezel and
  // then have to take it away.
  assert.equal(frameSuitsStream(pixelSkin, 0, 0), false);
  assert.equal(frameSuitsStream(pixelSkin, 480, 0), false);
  assert.equal(frameSuitsStream(pixelSkin, NaN, 1080), false);
});

test('orientationOf names the three cases', () => {
  assert.equal(orientationOf(1080, 480), 'landscape');
  assert.equal(orientationOf(480, 1080), 'portrait');
  assert.equal(orientationOf(1080, 1080), 'square');
});

// A transform is only correct if the art lands where the bezel expects it, so
// these read the geometry back rather than comparing the string. apply() is
// the CSS rule for `translate(a, b) rotate(d)` about a top-left origin: the
// rotation runs first and the translate slides the result.
function apply(transform, x, y) {
  if (!transform) return [x, y];
  const m = /^translate\((-?[\d.]+)px, (-?[\d.]+)px\) rotate\((\d+)deg\)$/.exec(transform);
  assert.ok(m, `unparseable transform: ${transform}`);
  const [tx, ty, deg] = [Number(m[1]), Number(m[2]), Number(m[3])];
  const rad = (deg * Math.PI) / 180;
  const [c, s] = [Math.round(Math.cos(rad)), Math.round(Math.sin(rad))];
  return [x * c - y * s + tx, x * s + y * c + ty];
}

// The box the turned art covers, from its four corners.
function covered(quarters, w, h) {
  const { transform } = turnedArt(quarters, w, h);
  const pts = [[0, 0], [w, 0], [w, h], [0, h]].map(([x, y]) => apply(transform, x, y));
  const xs = pts.map((p) => p[0]);
  const ys = pts.map((p) => p[1]);
  return { x: Math.min(...xs), y: Math.min(...ys), w: Math.max(...xs) - Math.min(...xs), h: Math.max(...ys) - Math.min(...ys) };
}

// mask.webp is exactly the display, so its grid is its own box, and a turn
// has to leave it starting at the display rect's origin. Art that lands at a
// negative coordinate has swung off the top or the left of the bezel, which is
// what rotating about a corner does if nothing puts it back.
test('turned art on its own grid starts where it started and swaps its axes', () => {
  for (const q of [0, 1, 2, 3]) {
    const box = covered(q, 1080, 2424);
    assert.deepEqual([box.x, box.y], [0, 0], `turn ${q} moved the origin`);
    const [wantW, wantH] = q % 2 === 0 ? [1080, 2424] : [2424, 1080];
    assert.deepEqual([box.w, box.h], [wantW, wantH], `turn ${q} covered the wrong box`);
  }
});

// back.webp on pixel_10 is 1205x2535 against a 1198x2531 layout, and its
// transparent hole is x 59..1139, y 55..2479 in its own pixels (trap 40
// measured both). The display rects are what /api/frame answered on
// emulator-5554 at each turn, and what TestTheBezelTurnsTheSameWayTheDeviceDid
// pins on the server: it turns about the layout, so the art has to as well or
// the hole and the canvas disagree. Pivoting on the image put the hole 7 art px
// low at three turns and 4 px right at one, measured in a browser.
test('the art hole lands on the display rect the server turned', () => {
  const hole = { x0: 59, y0: 55, x1: 1139, y1: 2479 };
  const served = {
    0: { x: 59, y: 55, width: 1080, height: 2424 },
    1: { x: 52, y: 59, width: 2424, height: 1080 },
    2: { x: 59, y: 52, width: 1080, height: 2424 },
    3: { x: 55, y: 59, width: 2424, height: 1080 },
  };
  for (const q of [0, 1, 2, 3]) {
    const { transform } = turnedArt(q, 1205, 2535, 1198, 2531);
    const a = apply(transform, hole.x0, hole.y0);
    const b = apply(transform, hole.x1, hole.y1);
    const got = {
      x: Math.min(a[0], b[0]), y: Math.min(a[1], b[1]),
      width: Math.abs(a[0] - b[0]), height: Math.abs(a[1] - b[1]),
    };
    assert.deepEqual(got, served[q], `turn ${q}`);
  }
});

// The overhang is outside the layout, and turned on the grid it has to stay
// outside it -- past the top or left edge, where the wrap clips it -- rather
// than pushing the layout's own pixels off the far side.
test('turned art keeps the whole layout in view', () => {
  for (const q of [0, 1, 2, 3]) {
    const { transform } = turnedArt(q, 1205, 2535, 1198, 2531);
    const corners = [[0, 0], [1198, 0], [1198, 2531], [0, 2531]].map(([x, y]) => apply(transform, x, y));
    const [w, h] = q % 2 === 0 ? [1198, 2531] : [2531, 1198];
    assert.deepEqual([Math.min(...corners.map((p) => p[0])), Math.min(...corners.map((p) => p[1]))], [0, 0], `turn ${q}`);
    assert.deepEqual([Math.max(...corners.map((p) => p[0])), Math.max(...corners.map((p) => p[1]))], [w, h], `turn ${q}`);
  }
});

// The emulator reports ROTATION_90 with the punch hole on the left of a
// 2424x1080 display, measured from mDisplayCutout as Rect(0, 944 - 136, 1080).
// mask.webp is exactly the display, 1080x2424, and carries that hole near its
// own top-left. Three turns clockwise is the only one of the four that puts
// it back at the bottom-left, and getting this backwards is a bezel that looks
// deliberate and is mirrored.
test('a mask turns the way the device did', () => {
  const hole = [84, 83]; // near the top-left of the portrait mask
  const [x, y] = apply(turnedArt(3, 1080, 2424).transform, ...hole);
  assert.ok(x < 2424 / 2, `hole should be on the left, got x=${x}`);
  assert.ok(y > 1080 / 2, `hole should be low, got y=${y}`);
  // One turn the other way is the mirror image, which is the mistake this pins.
  const [mx, my] = apply(turnedArt(1, 1080, 2424).transform, ...hole);
  assert.ok(mx > 2424 / 2 && my < 1080 / 2, 'one turn clockwise should be the opposite corner');
});

test('a turn count is normalised, and a missing size never turns', () => {
  assert.equal(turnedArt(-1, 10, 20).transform, turnedArt(3, 10, 20).transform);
  assert.equal(turnedArt(5, 10, 20).transform, turnedArt(1, 10, 20).transform);
  for (const bad of [0, -5, NaN, undefined]) {
    assert.equal(turnedArt(1, bad, 20).transform, '', `turned art on width ${bad}`);
    assert.equal(turnedArt(1, 10, bad).transform, '', `turned art on height ${bad}`);
  }
  // An omitted grid is the box, which is the mask's case; a bad one never turns.
  for (const bad of [0, -5, NaN]) {
    assert.equal(turnedArt(1, 10, 20, bad, 20).transform, '', `turned art on grid width ${bad}`);
    assert.equal(turnedArt(1, 10, 20, 10, bad).transform, '', `turned art on grid height ${bad}`);
  }
});

// A flip between the two landscapes keeps the stream at 1080x480, so the only
// way to see it is to ask /api/frame again while a turned skin is shown.
const turnedSkin = {
  width: 2531, height: 1198,
  display: { x: 55, y: 59, width: 2424, height: 1080 },
  source: 'skin', rotation: 1,
};

test('a turned skin around a landscape stream is rechecked', () => {
  assert.equal(frameNeedsRecheck(turnedSkin, 1080, 480), true);
});

test('a portrait stream has only one upright, so it is never rechecked', () => {
  assert.equal(frameNeedsRecheck(pixelSkin, 480, 1080), false);
});

test('a derived frame does not turn, so it is never rechecked', () => {
  assert.equal(frameNeedsRecheck({ ...turnedSkin, source: 'derived' }, 1080, 480), false);
});

test('a frame that does not suit the stream yet waits for the rotation fetch instead', () => {
  assert.equal(frameNeedsRecheck(pixelSkin, 1080, 480), false);
});

test('no frame or no stream size is never rechecked', () => {
  assert.equal(frameNeedsRecheck(null, 1080, 480), false);
  assert.equal(frameNeedsRecheck(turnedSkin, 0, 480), false);
  assert.equal(frameNeedsRecheck(turnedSkin, 1080, NaN), false);
});

test('the device frame defaults on when no preference is saved', () => {
  assert.equal(frameWantedFrom(null), true);
  assert.equal(frameWantedFrom(undefined), true);
});

test('a saved preference wins over the default', () => {
  assert.equal(frameWantedFrom('1'), true);
  assert.equal(frameWantedFrom('0'), false);
});

console.log(failures ? `\n${failures} failing` : '\nall passing');
process.exit(failures ? 1 : 0);
