import assert from 'node:assert/strict';
import { test } from 'node:test';
import { decodeControl, bannerText, viewerHeaders, heldByOther, mount } from './control.js';
import { fakeDocument, FakeElement } from './fakedom.js';

const enc = (o) => new TextEncoder().encode(JSON.stringify(o));

test('a body decodes to its state', () => {
  assert.deepEqual(decodeControl(enc({ held: true, holder: 3, agent: 'waiting', agentName: 'claude' })),
    { held: true, holder: 3, agent: 'waiting', agentName: 'claude' });
});

test('a body that does not parse reads as free and idle', () => {
  assert.deepEqual(decodeControl(new TextEncoder().encode('{nope')),
    { held: false, holder: 0, agent: 'idle', agentName: '' });
});

test('an unknown agent state reads as idle', () => {
  assert.equal(decodeControl(enc({ agent: 'plotting' })).agent, 'idle');
});

test('driving names the agent, and falls back to "An agent"', () => {
  assert.equal(bannerText({ agent: 'driving', agentName: 'claude' }, 1), 'claude is driving');
  assert.equal(bannerText({ agent: 'driving', agentName: '' }, 1), 'An agent is driving');
});

test('"waiting for you" is said only to the holder', () => {
  const st = { agent: 'waiting', agentName: 'claude', holder: 3 };
  assert.equal(bannerText(st, 3), 'claude is waiting for you');
  assert.equal(bannerText(st, 4), 'claude is waiting');
  assert.equal(bannerText(st, 0), 'claude is waiting for you');
});

test('idle shows nothing', () => {
  assert.equal(bannerText({ agent: 'idle', agentName: 'claude' }, 1), '');
});

test('the viewer header is sent only with an id', () => {
  assert.deepEqual(viewerHeaders(7), { 'X-Loupe-Viewer': '7' });
  assert.deepEqual(viewerHeaders(0), {});
});

test('mount shows, hides and reports each change once', () => {
  const doc = fakeDocument;
  const reports = [];
  const c = mount({ doc, report: (r) => reports.push(r), takeOver: () => {} });
  const banner = doc.body.children[doc.body.children.length - 1];
  const [label] = banner.children;
  assert.equal(banner.hidden, true);
  c.onInit({ viewer: 3 });
  assert.deepEqual(c.headers(), { 'X-Loupe-Viewer': '3' });
  c.handleMessage(enc({ held: true, holder: 3, agent: 'waiting', agentName: 'claude' }));
  assert.equal(banner.hidden, false);
  assert.equal(label.textContent, 'claude is waiting for you');
  assert.equal(doc.body.dataset.agent, 'waiting');
  c.handleMessage(enc({ held: false, agent: 'driving', agentName: 'claude' }));
  assert.equal(doc.body.dataset.agent, 'driving');
  c.handleMessage(enc({ held: true, holder: 3, agent: 'waiting', agentName: 'claude' }));
  c.handleMessage(enc({ held: false, agent: 'idle' }));
  assert.equal(banner.hidden, true);
  assert.equal(doc.body.dataset.agent, undefined);
  assert.deepEqual(reports.map((r) => r.state), ['waiting', 'driving', 'waiting', 'idle']);
  c.clear();
  assert.deepEqual(c.headers(), {});
});

test('Take over is offered only while an agent drives, and sends once per tap', () => {
  const doc = fakeDocument;
  const reports = [];
  let sent = 0;
  const c = mount({ doc, report: (r) => reports.push(r), takeOver: () => { sent++; } });
  const banner = doc.body.children[doc.body.children.length - 1];
  const [, button] = banner.children;
  c.onInit({ viewer: 3 });
  c.handleMessage(enc({ held: false, agent: 'driving', agentName: 'claude' }));
  assert.equal(button.hidden, false);
  assert.equal(button.textContent, 'Take over');
  button.dispatch('click');
  assert.equal(sent, 1);
  assert.equal(reports.at(-1).event, 'control-takeover');
  assert.equal(reports.at(-1).agent, 'claude');
  c.handleMessage(enc({ held: true, holder: 3, agent: 'waiting', agentName: 'claude' }));
  assert.equal(button.hidden, true);
  c.clear();
});

test('Take over that cannot send reports the error and keeps the button', () => {
  const doc = fakeDocument;
  const reports = [];
  const c = mount({ doc, report: (r) => reports.push(r), takeOver: () => { throw new Error('not connected'); } });
  const banner = doc.body.children[doc.body.children.length - 1];
  const [, button] = banner.children;
  c.handleMessage(enc({ held: false, agent: 'driving', agentName: 'claude' }));
  button.dispatch('click');
  assert.equal(reports.at(-1).event, 'control-takeover');
  assert.equal(reports.at(-1).error, 'not connected');
  assert.equal(button.hidden, false);
  c.clear();
});

test('two mounts put their banners in their own hosts', () => {
  const doc = fakeDocument;
  const a = new FakeElement('div'), b = new FakeElement('div');
  const ca = mount({ doc, host: a, report() {}, takeOver() {} });
  mount({ doc, host: b, report() {}, takeOver() {} });
  ca.handleMessage(enc({ held: false, agent: 'driving', agentName: 'claude' }));
  assert.equal(a.querySelector('.controlBanner').hidden, false);
  assert.equal(b.querySelector('.controlBanner').hidden, true);
  assert.equal(a.querySelector('.controlBanner').id, undefined);
  assert.equal(a.dataset.agent, 'driving');
  assert.equal(b.dataset.agent, undefined);
  assert.notEqual(doc.body.dataset.agent, 'driving');
});

test('hasControl follows the latest init until clear', () => {
  const c = mount({ doc: fakeDocument, host: new FakeElement('div'), report() {}, takeOver() {} });
  assert.equal(c.hasControl(), false);
  c.onInit({ viewer: 3, control: true });
  assert.equal(c.hasControl(), true);
  c.onInit({ viewer: 4, control: false });
  assert.equal(c.hasControl(), false);
  c.onInit({ viewer: 5, control: true });
  c.clear();
  assert.equal(c.hasControl(), false);
});

test('heldByOther is true only when a different viewer holds the device', () => {
  assert.equal(heldByOther({ held: true, holder: 4 }, 3), true);
  assert.equal(heldByOther({ held: true, holder: 3 }, 3), false);
  assert.equal(heldByOther({ held: false, holder: 4 }, 3), false);
  assert.equal(heldByOther({ held: true, holder: 0 }, 3), false);
  assert.equal(heldByOther({ held: true, holder: 4 }, 0), false);
});

test('onHeldByOther fires when the answer changes, and drops on clear', () => {
  const seen = [];
  const c = mount({ doc: fakeDocument, takeOver() {}, onHeldByOther: (on) => seen.push(on) });
  c.onInit({ viewer: 3, control: true });
  c.handleMessage(enc({ held: true, holder: 4, agent: 'idle' }));
  c.handleMessage(enc({ held: true, holder: 4, agent: 'driving', agentName: 'x' }));
  c.handleMessage(enc({ held: true, holder: 3, agent: 'idle' }));
  c.handleMessage(enc({ held: true, holder: 4, agent: 'idle' }));
  c.clear();
  assert.deepEqual(seen, [true, false, true, false]);
});
