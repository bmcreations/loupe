// Tests for the Add server sheet's pure logic. Run:
// node web/servers.test.mjs
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const { validateServerForm, serverError, postServer, fetchServers, deleteServer, serverRowModel, serverRowHTML } = await import('./servers.js');

let failures = 0;
const pending = [];
function test(name, fn) {
  const fail = (err) => { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); };
  try {
    const r = fn();
    if (r && typeof r.then === 'function') pending.push(r.then(() => console.log(`  PASS ${name}`), fail));
    else console.log(`  PASS ${name}`);
  } catch (err) { fail(err); }
}
const res = (status, body) => ({ ok: status >= 200 && status < 300, status, json: async () => { if (body === undefined) throw new Error('no body'); return body; } });

test('validateServerForm takes an https URL and a token, and trims', () => {
  const v = validateServerForm({ url: ' https://mac-mini.local:18456/ ', token: ' t ', name: ' mini ' });
  assert.deepEqual(v, { ok: true, value: { url: 'https://mac-mini.local:18456', token: 't', name: 'mini' } });
});
test('validateServerForm leaves name out when blank and assumes https for a bare host', () => {
  assert.deepEqual(validateServerForm({ url: 'mac-mini.local:18456', token: 't' }).value, { url: 'https://mac-mini.local:18456', token: 't' });
});
test('validateServerForm refuses http, a missing token, a missing URL and a slash in the name', () => {
  assert.equal(validateServerForm({ url: 'http://x.local', token: 't' }).field, 'url');
  assert.match(validateServerForm({ url: 'http://x.local', token: 't' }).error, /https/);
  assert.equal(validateServerForm({ url: '', token: 't' }).field, 'url');
  assert.equal(validateServerForm({ url: 'https://x.local', token: ' ' }).field, 'token');
  assert.equal(validateServerForm({ url: 'https://x.local', token: 't', name: 'a/b' }).field, 'name');
});
test('serverError: a 401 on add is the peer refusing the token, whatever the body says', () => {
  assert.equal(serverError(401, { error: 'unauthorized' }), 'The server refused the token.');
  assert.equal(serverError(401, null), 'The server refused the token.');
});
test('serverError shows the server’s message, else the status', () => {
  assert.equal(serverError(409, { error: 'name "mini" is taken' }), 'name "mini" is taken');
  assert.equal(serverError(502, null), 'Could not add that server (502)');
  assert.equal(serverError(500, null, { removing: true }), 'Could not remove that server (500)');
});

test('postServer sends the body as JSON to /api/servers', async () => {
  let seen;
  const r = await postServer(async (url, init) => { seen = { url, init }; return res(201, { name: 'mini', state: 'up' }); }, { url: 'https://x', token: 't' });
  assert.equal(seen.url, '/api/servers');
  assert.equal(seen.init.method, 'POST');
  assert.deepEqual(JSON.parse(seen.init.body), { url: 'https://x', token: 't' });
  assert.equal(r.ok, true);
  assert.equal(r.warning, '');
});
test('postServer surfaces the add response’s warning', async () => {
  const r = await postServer(async () => res(201, { name: 'mini', warning: 'streaming may not work' }), {});
  assert.equal(r.ok, true);
  assert.equal(r.warning, 'streaming may not work');
});
test('postServer: a 401 is the peer’s refusal and carries no page-level signal', async () => {
  const r = await postServer(async () => res(401, { error: 'unauthorized' }), {});
  assert.deepEqual(r, { ok: false, status: 401, message: 'The server refused the token.' });
});
test('postServer shows the server’s error on other failures, including a non-JSON body', async () => {
  const a = await postServer(async () => res(502, { error: 'certificate changed', code: 'certChanged' }), {});
  assert.equal(a.message, 'certificate changed');
  const b = await postServer(async () => res(500), {});
  assert.equal(b.message, 'Could not add that server (500)');
});
test('postServer reports a network failure as status 0', async () => {
  const r = await postServer(async () => { throw new Error('offline'); }, {});
  assert.equal(r.ok, false);
  assert.equal(r.status, 0);
});

test('fetchServers returns the list, and fails on a non-array, an error or a throw', async () => {
  assert.deepEqual((await fetchServers(async () => res(200, [{ name: 'a' }]))).list, [{ name: 'a' }]);
  assert.equal((await fetchServers(async () => res(200, {}))).failed, true);
  assert.equal((await fetchServers(async () => res(404, {}))).status, 404);
  assert.equal((await fetchServers(async () => { throw new Error('x'); })).failed, true);
});
test('deleteServer sends DELETE with the name encoded', async () => {
  let seen;
  const r = await deleteServer(async (url, init) => { seen = { url, init }; return res(204); }, 'my mini');
  assert.equal(seen.url, '/api/servers?name=my%20mini');
  assert.equal(seen.init.method, 'DELETE');
  assert.equal(r.ok, true);
});
test('deleteServer shows the server’s message when it fails', async () => {
  assert.equal((await deleteServer(async () => res(404, { error: 'no server named x' }), 'x')).message, 'no server named x');
});

test('serverRowModel: up shows count and round trip, down says unreachable', () => {
  const up = serverRowModel({ name: 'mini', url: 'https://mac-mini.local:18456', state: 'up', targets: 4, rttMs: 2 });
  assert.equal(up.up, true);
  assert.equal(up.detail, 'Up · 4 devices · 2 ms');
  assert.equal(up.host, 'mac-mini.local:18456');
  const down = serverRowModel({ name: 'lab', url: 'https://lab:1', state: 'down', targets: 1, rttMs: 0 });
  assert.equal(down.detail, 'Unreachable · 1 device');
});
test('serverRowHTML escapes the name and carries a Remove button', () => {
  const html = serverRowHTML(serverRowModel({ name: '<i>', url: 'https://x', state: 'up', targets: 0 }));
  assert.match(html, /data-remove="&lt;i&gt;"/);
  assert.doesNotMatch(html, /<i>/);
});

// The module list: servers.js is shipped, stamped and imported only by app.js.
test('servers.js is imported by app.js and listed in clientModules', () => {
  const app = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
  assert.match(app, /from '\.\/servers\.js'/);
  const go = readFileSync(new URL('../internal/httpapi/server.go', import.meta.url), 'utf8');
  assert.match(go, /var clientModules = \[\]string\{[^}]*"servers\.js"/);
  assert.equal(/from '\.\/servers\.js'/.test(readFileSync(new URL('./drawer.js', import.meta.url), 'utf8')), false, 'no module but app.js imports it (trap 45)');
});
test('the page has an #addServer sheet element', () => {
  assert.match(readFileSync(new URL('./index.html', import.meta.url), 'utf8'), /<aside id="addServer"/);
});
test('no client module redirects on a 401 from the add (nothing here navigates)', () => {
  const src = readFileSync(new URL('./servers.js', import.meta.url), 'utf8');
  assert.doesNotMatch(src, /location\.(assign|href|replace)/);
});

await Promise.all(pending);
console.log(failures ? `\n${failures} failing` : '\nall passing');
process.exit(failures ? 1 : 0);
