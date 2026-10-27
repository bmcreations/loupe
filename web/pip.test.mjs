// Tests for pip.js. Run: node web/pip.test.mjs
//
// Only the canvas handling is tested: whether PiP itself starts is Safari's
// business and is checked on a phone. fakedom has no <video>, so the element
// pip.js creates gets the two PiP methods it feature-checks for.
import assert from 'node:assert/strict';
import { FakeElement, installFakeDom } from './fakedom.js';

installFakeDom();
const realCreate = document.createElement;
document.createElement = (tag) => {
  const node = realCreate(tag);
  if (tag === 'video') {
    node.requestPictureInPicture = () => Promise.resolve();
    node.play = () => Promise.resolve();
  }
  return node;
};
document.pictureInPictureEnabled = true;
const { mount } = await import('./pip.js');

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

function canvas(name) {
  const c = new FakeElement('canvas');
  c.captureStream = () => ({ from: name });
  return c;
}

console.log('pip');

test('retarget swaps the video onto the new canvas once attached', () => {
  const pip = mount(canvas('a'), { report() {}, onChange() {} });
  assert.equal(pip.supported, true);
  pip.attach();
  pip.retarget(canvas('b'));
  const video = document.body.children.find((n) => n.tagName === 'VIDEO' || n.tag === 'video');
  assert.deepEqual(video.srcObject, { from: 'b' });
});

test('retarget before attach waits for attach to capture the new canvas', () => {
  const before = document.body.children.length;
  const pip = mount(canvas('a'), { report() {}, onChange() {} });
  pip.retarget(canvas('c'));
  assert.equal(document.body.children.length, before);
  pip.attach();
  const video = document.body.children[document.body.children.length - 1];
  assert.deepEqual(video.srcObject, { from: 'c' });
});

if (failures) { console.log(`FAIL ${failures}`); process.exit(1); }
console.log('PASS');
