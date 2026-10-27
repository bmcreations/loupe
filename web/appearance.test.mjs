// Tests for the appearance preference. Run: node web/appearance.test.mjs
//
// storage is a fake with getItem/setItem/removeItem, so a test can make any
// call throw without a browser. root is a fake with a plain dataset object.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const { KEY, readAppearance, writeAppearance, applyAppearance, themeColor, themeName } = await import('./appearance.js');

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

function fakeStorage(initial = {}) {
  const data = { ...initial };
  return {
    data,
    getItem: (k) => (k in data ? data[k] : null),
    setItem: (k, v) => { data[k] = v; },
    removeItem: (k) => { delete data[k]; },
  };
}
const throwing = {
  getItem() { throw new Error('blocked'); },
  setItem() { throw new Error('blocked'); },
  removeItem() { throw new Error('blocked'); },
};

test('nothing stored reads as system', () => {
  assert.equal(readAppearance(fakeStorage()), 'system');
});
test('light and dark read back', () => {
  assert.equal(readAppearance(fakeStorage({ [KEY]: 'light' })), 'light');
  assert.equal(readAppearance(fakeStorage({ [KEY]: 'dark' })), 'dark');
});
test('a junk value reads as system', () => {
  assert.equal(readAppearance(fakeStorage({ [KEY]: 'sepia' })), 'system');
});
test('null or throwing storage reads as system', () => {
  assert.equal(readAppearance(null), 'system');
  assert.equal(readAppearance(throwing), 'system');
});
test('writing system or junk removes the key', () => {
  const s = fakeStorage({ [KEY]: 'dark' });
  writeAppearance(s, 'system');
  assert.equal(KEY in s.data, false);
  s.data[KEY] = 'dark';
  writeAppearance(s, 'sepia');
  assert.equal(KEY in s.data, false);
});
test('writing light stores light', () => {
  const s = fakeStorage();
  writeAppearance(s, 'light');
  assert.equal(s.data[KEY], 'light');
});
test('writing to a throwing or null storage does not throw', () => {
  writeAppearance(throwing, 'dark');
  writeAppearance(null, 'dark');
});
test('apply sets data-theme for light and dark, removes it for system', () => {
  const root = { dataset: {} };
  applyAppearance(root, 'dark');
  assert.equal(root.dataset.theme, 'dark');
  applyAppearance(root, 'light');
  assert.equal(root.dataset.theme, 'light');
  applyAppearance(root, 'system');
  assert.equal('theme' in root.dataset, false);
});
test('themeColor follows the forced theme, and the system one otherwise', () => {
  assert.equal(themeColor('light', true), '#f2f3f5');
  assert.equal(themeColor('dark', false), '#0b0d10');
  assert.equal(themeColor('system', true), '#0b0d10');
  assert.equal(themeColor('system', false), '#f2f3f5');
});

// index.html's inline script repeats readAppearance/applyAppearance so the
// first paint is themed. It cannot import the module, so check it names the
// same key and only the two stored values.
const html = readFileSync(path.join(path.dirname(fileURLToPath(import.meta.url)), 'index.html'), 'utf8');
test('index.html applies the stored appearance before first paint', () => {
  const script = html.match(/\/\/ Appearance, applied before first paint[\s\S]*?\}\)\(\);/);
  assert.ok(script, 'inline appearance script not found');
  assert.ok(script[0].includes(`'${KEY}'`), `script does not read ${KEY}`);
  assert.match(script[0], /v === 'light' \|\| v === 'dark'/);
  assert.match(script[0], /dataset\.theme = v/);
});
test('index.html has a theme-color meta per scheme', () => {
  assert.match(html, /<meta name="theme-color" media="\(prefers-color-scheme: light\)" content="#f2f3f5">/);
  assert.match(html, /<meta name="theme-color" media="\(prefers-color-scheme: dark\)" content="#0b0d10">/);
});

if (failures) { console.log(`${failures} failed`); process.exit(1); }

test('themeName resolves System to the shown theme', () => {
  assert.equal(themeName('light', true), 'light');
  assert.equal(themeName('dark', false), 'dark');
  assert.equal(themeName('system', true), 'dark');
  assert.equal(themeName('system', false), 'light');
});
