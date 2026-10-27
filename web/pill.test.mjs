import { test } from 'node:test';
import assert from 'node:assert/strict';
import { paintPill } from './pill.js';

// Minimal stand-ins for the DOM nodes paintPill touches -- pill.js only ever
// sets textContent/className/title, so a plain object is enough and keeps
// this test out of the no-harness app.js (see CLAUDE.md trap 21/38).
function elements() {
  return {
    pill: { title: '' },
    pillName: { textContent: '' },
    pillDot: { className: '', title: '' },
  };
}

test('paintPill sets the pill\'s own title to the full label, not just pillName\'s text', () => {
  const el = elements();
  const long = 'Acme One (ACME00000000001)';
  paintPill(el, { label: long, dot: { state: 'good', label: 'live' } });
  assert.equal(el.pillName.textContent, long, 'pillName carries the full text -- CSS truncates it visually, the DOM keeps it whole');
  assert.equal(el.pill.title, long, 'title must carry the same full label so a truncated pill still reveals it on hover/long-press (owner iPhone report: a long device name crowded the Tools button)');
});

test('paintPill clears the title for a null/undefined label the same way it clears the text', () => {
  const el = elements();
  el.pill.title = 'Acme One (ACME00000000001)';
  paintPill(el, {});
  assert.equal(el.pillName.textContent, '');
  assert.equal(el.pill.title, '', 'an empty title, not "undefined" or a stale previous label');
});

test('paintPill does not throw when el.pill is absent', () => {
  const el = { pillName: { textContent: '' }, pillDot: { className: '', title: '' } };
  assert.doesNotThrow(() => paintPill(el, { label: 'x', dot: null }));
});

test('paintPill still paints the idle dot look when dot is falsy', () => {
  const el = elements();
  paintPill(el, { label: 'Target', dot: null });
  assert.equal(el.pillDot.className, 'dot');
  assert.equal(el.pillDot.title, 'idle');
});

test('headless shows the tag and names it in the title', () => {
  const el = elements();
  el.pillTag = { hidden: true };
  paintPill(el, { label: 'Pixel 10', headless: true });
  assert.equal(el.pillTag.hidden, false);
  assert.equal(el.pill.title, 'Pixel 10 (headless)');
  paintPill(el, { label: 'Pixel 10' });
  assert.equal(el.pillTag.hidden, true);
  assert.equal(el.pill.title, 'Pixel 10');
});
