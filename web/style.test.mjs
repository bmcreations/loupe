// Guards style.css's colour discipline. Run: node web/style.test.mjs
//
// Three rules, each a way the restyle could quietly undo itself:
//   1. No colour literal outside the token blocks.
//   2. The two dark blocks (System-dark under the media query, and forced
//      [data-theme="dark"]) declare the same tokens with the same values.
//   3. Light and dark declare the same set of tokens.
// Regex over the file, not a CSS parser: the only nesting is the one @media
// wrapper this test looks for.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const here = path.dirname(fileURLToPath(import.meta.url));
const css = readFileSync(path.join(here, 'style.css'), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

// The first rule whose selector matches, braces balanced.
function block(selectorRe, from = css) {
  const m = selectorRe.exec(from);
  if (!m) return null;
  let i = m.index + m[0].length, depth = 1;
  const start = i;
  while (depth && i < from.length) {
    if (from[i] === '{') depth++;
    else if (from[i] === '}') depth--;
    i++;
  }
  return { start: m.index, end: i, body: from.slice(start, i - 1) };
}
function decls(body) {
  const out = {};
  for (const m of body.matchAll(/(--[\w-]+)\s*:\s*([^;]+);/g)) out[m[1]] = m[2].trim();
  return out;
}

const shared = block(/^:root\s*\{/m);
const light = block(/:root\[data-theme="light"\],\s*:root\s*\{/);
const media = block(/@media\s*\(prefers-color-scheme:\s*dark\)\s*\{/);
const systemDark = media && block(/:root:not\(\[data-theme="light"\]\)\s*\{/, media.body);
const forcedDark = block(/:root\[data-theme="dark"\]\s*\{/);
const lightScheme = block(/:root\[data-theme="light"\]\s*\{/);

test('the token blocks exist', () => {
  assert.ok(shared, 'shared: a plain :root { ... } first');
  assert.ok(light, 'light: :root[data-theme="light"], :root { ... }');
  assert.ok(systemDark, 'system dark: @media (prefers-color-scheme: dark) { :root:not([data-theme="light"]) { ... } }');
  assert.ok(forcedDark, 'forced dark: :root[data-theme="dark"] { ... }');
});

test('both dark blocks are identical', () => {
  const sys = decls(systemDark.body);
  const forced = decls(forcedDark.body);
  assert.deepEqual(sys, forced);
});

test('light and dark declare the same tokens', () => {
  assert.deepEqual(Object.keys(decls(light.body)).sort(), Object.keys(decls(forcedDark.body)).sort());
});

test('no colour literal outside the token blocks', () => {
  let rest = css;
  const blocks = [shared, light, media, forcedDark, lightScheme].filter(Boolean);
  for (const b of blocks.sort((a, b) => b.start - a.start)) {
    rest = rest.slice(0, b.start) + rest.slice(b.end);
  }
  const hits = [...rest.matchAll(/#[0-9a-fA-F]{3,8}\b|rgba?\(\s*\d/g)].map((m) => m[0]);
  assert.deepEqual(hits, []);
});

test('no font-size literal outside the type ramp', () => {
  const hits = [...css.matchAll(/font-size:\s*(\d+(\.\d+)?px)/g)].map((m) => m[1]);
  assert.deepEqual(hits, []);
});

// A swiped row slides over its actions, so every background it can take must
// end on an opaque fill. The pressed tint alone let the actions show through.
test('a swipe row stays opaque while pressed', () => {
  const b = block(/\.pickerSwipe > \.pickerRow:not\(:disabled\):active[^{]*\{/);
  assert.ok(b, 'no pressed rule for .pickerSwipe > .pickerRow');
  assert.match(b.body, /background:[^;]*var\(--fill-pressed\)[^;]*,\s*var\(--background-secondary\)\s*;/);
});

// Every pane is a clone of one template, so nothing inside it has an id:
// a rule that still names one styles no pane at all.
test('no rule selects a pane part by id', () => {
  const ids = ['screenWrap', 'screen', 'frameArt', 'frameMask', 'frameCutout', 'frameCutoutPath',
    'touchLayer', 'cursorLayer', 'inspectLayer', 'flagLayer', 'statsOverlay'];
  for (const id of ids) {
    assert.doesNotMatch(css, new RegExp(`#${id}(?![\\w-])`), `#${id} is a pane part`);
  }
});

// Motion animates transform and opacity only (agent-team's style): a
// height, padding or colour animation repaints the layout every frame.
test('transitions and keyframes move only transform and opacity', () => {
  const bad = [];
  for (const m of css.matchAll(/transition:\s*([^;]+);/g)) {
    for (const part of m[1].split(',')) {
      const prop = part.trim().split(/\s+/)[0];
      if (!['transform', 'opacity', 'none'].includes(prop)) bad.push(`transition ${prop}`);
    }
  }
  for (const m of css.matchAll(/@keyframes\s+([\w-]+)\s*\{((?:[^{}]*\{[^}]*\})*)\s*\}/g)) {
    for (const d of m[2].matchAll(/([\w-]+)\s*:/g)) {
      if (!['transform', 'opacity'].includes(d[1])) bad.push(`@keyframes ${m[1]} ${d[1]}`);
    }
  }
  assert.deepEqual(bad, []);
});

if (failures) { console.log(`${failures} failed`); process.exit(1); }
