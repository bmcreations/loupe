// Tests for the client's token handling. Run: node web/transport.test.mjs
//
// This exists because the failure it guards is invisible. WebTransport sends
// no cookies, so the token has to be put on the /wt URL by hand; if that
// lookup returns nothing, the connect is rejected before any handler runs and
// the client falls back to WebSocket without reporting why. That is trap 11's
// shape, and the whole point of cmd/wtprobe was not having to guess at it.
import assert from 'node:assert/strict';

// transport.js reads document.cookie at call time, not at module scope, so a
// stub is enough to import it under node.
globalThis.document = { cookie: '' };

const { tokenParam } = await import('./transport.js');

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

test('reads the token from a lone cookie', () => {
  document.cookie = 'loupe_token=abc123';
  assert.equal(tokenParam(), '&token=abc123');
});

test('finds it among other cookies', () => {
  document.cookie = 'other=1; loupe_token=abc123; more=2';
  assert.equal(tokenParam(), '&token=abc123');
});

test('stops at the delimiter rather than swallowing the rest', () => {
  document.cookie = 'loupe_token=abc123; x=1';
  assert.equal(tokenParam(), '&token=abc123');
});

test('returns nothing when auth is off', () => {
  // -no-auth sets no cookie, and appending a bare &token= would make the
  // server reject a request it would otherwise have allowed.
  document.cookie = '';
  assert.equal(tokenParam(), '');
});

test('does not match a cookie that merely ends in the name', () => {
  document.cookie = 'xloupe_token=nope';
  assert.equal(tokenParam(), '');
});

test('does not match a cookie that merely contains the name', () => {
  document.cookie = 'not_loupe_token=nope';
  assert.equal(tokenParam(), '');
});

test('escapes what it puts in the query string', () => {
  document.cookie = 'loupe_token=a+b/c';
  assert.equal(tokenParam(), '&token=a%2Bb%2Fc');
});

console.log(failures === 0 ? '\nALL PASS' : `\n${failures} FAILED`);
process.exit(failures === 0 ? 0 : 1);
