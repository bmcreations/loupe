import assert from 'node:assert/strict';
import { test } from 'node:test';
import { preview, offered, cancelled, mount } from './clipboard.js';
import { fakeDocument, FakeElement } from './fakedom.js';

test('preview flattens whitespace and cuts with an ellipsis', () => {
  assert.equal(preview('a\n\n  b\tc'), 'a b c');
  assert.equal(preview('abcdef', 4), 'abc…');
  assert.equal(preview('abcd', 4), 'abcd');
});

test('preview never splits a character outside the BMP', () => {
  assert.equal(preview('😀😀😀', 2), '😀…');
});

test('clipboard is offered for any real target with control', () => {
  assert.equal(offered('sim:X', true), true);
  assert.equal(offered('avd:Pixel_10', true), true);
  assert.equal(offered('synthetic', true), false);
  assert.equal(offered('sim:X', false), false);
  assert.equal(offered('', true), false);
});

test('a paste the user dismissed reads as cancelled, not as a failure', () => {
  const denied = new Error('The request is not allowed by the user agent');
  denied.name = 'NotAllowedError';
  assert.equal(cancelled(denied), true);
  const aborted = new Error('aborted');
  aborted.name = 'AbortError';
  assert.equal(cancelled(aborted), true);
  assert.equal(cancelled(new Error('502 bad gateway')), false);
  assert.equal(cancelled(undefined), false);
});

test('the offer banner goes into deps.host, without an id', () => {
  const host = new FakeElement('div');
  const before = fakeDocument.body.children.length;
  mount({ doc: fakeDocument, host, targetId: () => 'sim:X', hasControl: () => true, report() {} });
  const banner = host.querySelector('.clipOffer');
  assert.ok(banner);
  assert.equal(banner.hidden, true);
  assert.equal(banner.id, undefined);
  assert.equal(fakeDocument.body.children.length, before);
});
