import assert from 'node:assert/strict';
import { test } from 'node:test';
import { route, offered, carriesFiles, endpoint, message } from './drop.js';

const f = (name, type = '') => ({ name, type, size: 1 });

test('photos and videos go to the library on any target that has one', () => {
  for (const id of ['sim:X', 'avd:Pixel_10', 'emulator-5554', 'ACME0']) {
    assert.equal(route(f('a.png', 'image/png'), id), 'media');
    assert.equal(route(f('IMG_1.HEIC'), id), 'media');
    assert.equal(route(f('clip.mov', 'video/quicktime'), id), 'media');
  }
});

test('an apk installs on Android and is refused on a simulator', () => {
  assert.equal(route(f('app-debug.apk'), 'avd:Pixel_10'), 'install');
  assert.equal(route(f('x', 'application/vnd.android.package-archive'), 'emulator-5554'), 'install');
  assert.equal(route(f('app-debug.apk'), 'sim:X'), null);
});

test('anything else, or a target with no library, is refused before upload', () => {
  assert.equal(route(f('notes.txt', 'text/plain'), 'sim:X'), null);
  assert.equal(route(f('a.png', 'image/png'), 'synthetic'), null);
  assert.equal(route(f('a.png', 'image/png'), 'ios:00008140'), null);
  assert.equal(offered('sim-new:loupe'), false);
  assert.equal(offered(''), false);
});

test('only a drag holding files counts', () => {
  assert.equal(carriesFiles({ types: ['Files'] }), true);
  assert.equal(carriesFiles({ types: ['text/plain', 'text/uri-list'] }), false);
  assert.equal(carriesFiles(null), false);
});

test('endpoints escape the target and allow test-only APKs', () => {
  assert.equal(endpoint('media', 'sim:A B'), '/api/media?target=sim%3AA%20B');
  assert.equal(endpoint('install', 'avd:P'), '/api/install?target=avd%3AP&allowTest=1');
});

test('messages name the package and the server reason', () => {
  assert.equal(message('install', true, { package: 'dev.x' }), 'Installed dev.x');
  assert.equal(message('media', true, {}), 'Added to Photos');
  assert.equal(message('media', false, { error: 'not a photo' }), "Couldn't add to Photos: not a photo");
  assert.equal(message('install', false, { code: 'INSTALL_FAILED_X' }), "Couldn't install: INSTALL_FAILED_X");
});
