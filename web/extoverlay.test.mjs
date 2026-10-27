// Tests for extension overlays' pure parts. Run: node web/extoverlay.test.mjs
import assert from 'node:assert/strict';

const { parseOverlays, rectStyle, imageStyle, modeTitle, MODES } = await import('./extoverlay.js');

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

test('a MsgOverlay body yields its overlays', () => {
  const got = parseOverlays(JSON.stringify({ overlays: [
    { id: 'spec', source: 'spec-compare', rect: [0, 0, 1, 1], opacity: 0.5, image: '/api/overlays/image?x' },
  ] }));
  assert.equal(got.length, 1);
  assert.equal(got[0].id, 'spec');
});

test('an empty set clears', () => {
  assert.deepEqual(parseOverlays('{"overlays":[]}'), []);
  assert.deepEqual(parseOverlays('{}'), []);
});

test('an overlay with no usable rect is dropped, not drawn at 0,0', () => {
  const got = parseOverlays(JSON.stringify({ overlays: [
    { id: 'a', rect: [0, 0, 1] },
    { id: 'b', rect: [0.5, 0.5, 0.1, 0.1] },
    { id: 'c', rect: [0, 0, 'x', 1] },
    { rect: [0, 0, 1, 1] },
    { id: 'ok', rect: [0, 0.25, 0.5, 0.5] },
  ] }));
  assert.deepEqual(got.map((o) => o.id), ['ok']);
});

test('a body that is not JSON throws', () => {
  assert.throws(() => parseOverlays('nope'));
  assert.throws(() => parseOverlays('null'));
});

test('a rect becomes percent placement', () => {
  assert.deepEqual(rectStyle([0, 0.25, 0.5, 0.5]), { left: '0%', top: '25%', width: '50%', height: '25%' });
});

test('fade draws the reference at the opacity, clamped', () => {
  assert.equal(imageStyle('fade', 0.3, 0.5).opacity, '0.3');
  assert.equal(imageStyle('fade', 4, 0.5).opacity, '1');
  assert.equal(imageStyle('fade', NaN, 0.5).opacity, '0.5');
});

test('swipe shows the reference left of the divider, fully opaque', () => {
  const s = imageStyle('swipe', 0.2, 0.25);
  assert.equal(s.opacity, '1');
  assert.equal(s.clipPath, 'inset(0 75% 0 0)');
  assert.equal(imageStyle('swipe', 1, 2).clipPath, 'inset(0 0% 0 0)');
});

test('difference blends at full opacity', () => {
  const s = imageStyle('difference', 0.2, 0.5);
  assert.equal(s.mixBlendMode, 'difference');
  assert.equal(s.opacity, '1');
});

test('the modes are the three the design names', () => {
  assert.deepEqual(MODES, ['fade', 'swipe', 'difference']);
});

test('each mode has the title the segmented control shows', () => {
  assert.deepEqual(MODES.map(modeTitle), ['Fade', 'Swipe', 'Difference']);
});

if (failures) { console.log(`\n${failures} failing`); process.exit(1); }
console.log('\nall passing');
