// Tests for the viewer's full-screen states. Run: node web/states.test.mjs
//
// The trace keys are the point of the first test. showOverlay reported its
// title to /api/log as the overlay event, and existing kind:"client"
// records group on those strings, so the rewrite keeps them word for word
// even where the screen's title changed.
import assert from 'node:assert/strict';
import {
  retryText, shortHost, connectingState, reconnectingState, unreachableState,
  cantStreamState, noDevicesState, unsupportedState,
  shutDownState, afterClose, afterResume, endedFromClose,
} from './states.js';

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

test('trace keys are the titles the old showOverlay reported', () => {
  assert.equal(unsupportedState().trace, 'WebCodecs not available');
  assert.equal(unreachableState({ hostname: 'x' }).trace, 'Cannot reach the server');
  assert.equal(noDevicesState().trace, 'No targets');
  assert.equal(cantStreamState({ label: 'Pixel 10' }).trace, 'Cannot stream Pixel 10');
  assert.equal(reconnectingState({ remainingMs: 500 }).trace, 'Disconnected');
  assert.equal(reconnectingState({ remainingMs: 500, error: 'boom' }).trace, 'Cannot connect');
});

test('every state has a key, an icon and a title', () => {
  const all = [connectingState(), reconnectingState({ remainingMs: 1 }), unreachableState(),
    cantStreamState({ label: 'a' }), noDevicesState(), unsupportedState()];
  for (const s of all) {
    assert.ok(s.key && s.icon && s.title && s.body, JSON.stringify(s));
    assert.ok(Array.isArray(s.actions));
  }
  assert.equal(new Set(all.map((s) => s.key)).size, all.length);
});

test('retry countdown rounds up and never reads 0 s while waiting', () => {
  assert.equal(retryText(1), 'next in 1 s');
  assert.equal(retryText(1000), 'next in 1 s');
  assert.equal(retryText(1001), 'next in 2 s');
  assert.equal(retryText(0), 'retrying now');
  assert.equal(retryText(-50), 'retrying now');
  assert.equal(retryText(NaN), 'retrying now');
});

test('the body names the attempt and follows the remaining time', () => {
  const s = reconnectingState({ remainingMs: 4000, attempt: 3 });
  assert.equal(s.body, 'Attempt 3, next in 4 s');
  assert.equal(s.bodyAt(1200), 'Attempt 3, next in 2 s');
  assert.equal(s.bodyAt(0), 'Attempt 3, retrying now');
  assert.equal(reconnectingState({ remainingMs: 4000 }).body, 'Attempt 1, next in 4 s');
  assert.equal(s.actions[0].id, 'retryNow');
  assert.equal(s.actions[0].label, 'Retry now');
});

test('a connect error goes in the mono detail, a plain drop has none', () => {
  assert.equal(reconnectingState({ remainingMs: 1, error: 'WebTransportError' }).detail, 'WebTransportError');
  assert.equal(reconnectingState({ remainingMs: 1 }).detail, '');
});

test('the server is named by its machine, an address whole', () => {
  assert.equal(shortHost('my-mac.tailnet-0000.ts.net'), 'my-mac');
  assert.equal(shortHost('100.64.0.10'), '100.64.0.10');
  assert.equal(shortHost('::1'), '::1');
  assert.equal(shortHost(''), 'The server');
});

test('connecting says booting only for a target that boots on open', () => {
  assert.match(connectingState({ label: 'Pixel_10', kind: 'emulator', booting: true }).body, /emulator/);
  assert.match(connectingState({ label: 'iPhone 17', kind: 'simulator', booting: true }).body, /simulator/);
  assert.equal(connectingState({ label: 'Acme One', kind: 'device' }).body, 'Opening the stream.');
  assert.equal(connectingState({ label: 'Acme One' }).title, 'Connecting to Acme One');
});

test('cannot stream keeps the server reason verbatim and offers the picker', () => {
  const s = cantStreamState({ label: 'iPhone', reason: 'CoreDeviceError 9001: Remote control requires iOS 27.0' });
  assert.equal(s.detail, 'CoreDeviceError 9001: Remote control requires iOS 27.0');
  assert.deepEqual(s.actions.map((a) => a.id), ['pick']);
});

test('No devices leads with New device… when the server can create one', () => {
  assert.deepEqual(noDevicesState({ canCreate: true }).actions.map((a) => a.id), ['newDevice', 'connectHelp']);
  assert.ok(noDevicesState({ canCreate: true }).actions[0].primary);
  assert.deepEqual(noDevicesState().actions.map((a) => a.id), ['retry', 'connectHelp']);
});

test('Can’t reach offers Switch server beside Try again', () => {
  assert.deepEqual(unreachableState({ hostname: 'h' }).actions.map((a) => a.id), ['retry', 'switchServer']);
});

test('shutDownState offers Start it and Pick for a shut down', () => {
  const s = shutDownState({ label: 'Pixel 10', reason: 'shutDown' });
  assert.equal(s.key, 'shutDown');
  assert.equal(s.trace, 'Pixel 10 is shut down');
  assert.deepEqual(s.actions.map((a) => a.id), ['start', 'pick']);
});

test('shutDownState offers only Pick for a deleted target', () => {
  const s = shutDownState({ label: 'Pixel 10', reason: 'deleted' });
  assert.equal(s.key, 'shutDown');
  assert.equal(s.trace, 'Pixel 10 was deleted');
  assert.deepEqual(s.actions.map((a) => a.id), ['pick']);
});

test('afterClose: ended wins, then a manual close, else reconnect', () => {
  assert.equal(afterClose({ ended: 'shutDown', manualClose: false }), 'ended');
  assert.equal(afterClose({ ended: 'deleted', manualClose: true }), 'ended');
  assert.equal(afterClose({ ended: null, manualClose: true }), 'none');
  assert.equal(afterClose({ ended: null, manualClose: false }), 'reconnect');
});

test('afterResume: an ended target stays, else refresh a live stream or connect', () => {
  assert.equal(afterResume({ ended: 'shutDown', open: false }), 'stay');
  assert.equal(afterResume({ ended: 'deleted', open: true }), 'stay');
  assert.equal(afterResume({ ended: null, open: true }), 'refresh');
  assert.equal(afterResume({ ended: null, open: false }), 'connect');
});

test('endedFromClose reads only WebTransport code 3', () => {
  assert.equal(endedFromClose({ code: 3, reason: 'deleted' }), 'deleted');
  assert.equal(endedFromClose({ code: 3, reason: '' }), 'shutDown');
  assert.equal(endedFromClose({ code: 3 }), 'shutDown');
  assert.equal(endedFromClose({ code: 1, reason: 'deleted' }), null);
  assert.equal(endedFromClose({}), null);
  assert.equal(endedFromClose(undefined), null);
});

console.log(failures ? `\n${failures} failing` : '\nall passing');
process.exit(failures ? 1 : 0);

test('shutDownState offers only Reload for an unsupported protocol', () => {
  const s = shutDownState({ label: 'Pixel 10', reason: 'unsupportedProtocol' });
  assert.equal(s.title, 'This page is out of date');
  assert.deepEqual(s.actions.map((a) => a.id), ['reload']);
});
