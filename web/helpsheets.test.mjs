// Tests for Switch server's pure parts. Run: node web/helpsheets.test.mjs
import assert from 'node:assert/strict';
import { serverOrigin, readServers, rememberServer, SERVERS_KEY } from './helpsheets.js';

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

function memStorage(init = {}) {
  const m = new Map(Object.entries(init));
  return { getItem: (k) => (m.has(k) ? m.get(k) : null), setItem: (k, v) => m.set(k, String(v)) };
}

const here = { port: '18456', host: 'mac.ts.net:18456', origin: 'https://mac.ts.net:18456' };

test('a bare host keeps this page’s port and goes over https', () => {
  assert.equal(serverOrigin('other.ts.net', here), 'https://other.ts.net:18456');
  assert.equal(serverOrigin('  100.1.2.3 ', here), 'https://100.1.2.3:18456');
});

test('a typed port or scheme is kept, and a path is dropped', () => {
  assert.equal(serverOrigin('other.ts.net:9000', here), 'https://other.ts.net:9000');
  assert.equal(serverOrigin('http://localhost:18472/?token=x', here), 'http://localhost:18472');
  assert.equal(serverOrigin('https://other.ts.net/', here), 'https://other.ts.net:18456');
});

test('nothing usable names no origin', () => {
  assert.equal(serverOrigin('', here), '');
  assert.equal(serverOrigin('   ', here), '');
  assert.equal(serverOrigin('ftp://x', here), '');
  assert.equal(serverOrigin('http://', here), '');
  // Node parses this hostname as given and Chrome percent-encodes "not a
  // host!" into one, so the check has to be ours.
  assert.equal(serverOrigin('bad!host', here), '');
  assert.equal(serverOrigin('[::1]:9000', here), 'https://[::1]:9000');
});

test('rememberServer keeps the newest five, each once, newest first', () => {
  const st = memStorage();
  for (const o of ['a', 'b', 'c', 'd', 'e', 'f', 'c']) rememberServer(st, `https://${o}`);
  assert.deepEqual(readServers(st), ['https://c', 'https://f', 'https://e', 'https://d', 'https://b']);
});

test('a corrupt or foreign list reads as empty, and storage that throws is survived', () => {
  assert.deepEqual(readServers(memStorage({ [SERVERS_KEY]: '{' })), []);
  assert.deepEqual(readServers(memStorage({ [SERVERS_KEY]: '{"a":1}' })), []);
  const throwing = { getItem() { throw new Error('denied'); }, setItem() { throw new Error('denied'); } };
  assert.deepEqual(rememberServer(throwing, 'https://a'), ['https://a']);
});

console.log(failures ? `\n${failures} failing` : '\nall passing');
process.exit(failures ? 1 : 0);
