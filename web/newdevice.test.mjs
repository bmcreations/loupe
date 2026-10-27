// Tests for the New device sheet's pure logic. Run:
// node web/newdevice.test.mjs
//
// Profiles are shaped as /api/avds/profiles returns them
// (internal/httpapi/avds.go's AVDProfile).
import assert from 'node:assert/strict';

import { readFileSync } from 'node:fs';
const {
  profileRowModel, createError, platformsOf, platformById, defaultOs, devicesFor, defaultDevice,
  reconcile, sanitizeName, dedupeName, osVersion, prefillName, validateName, jobView, submitModel,
  emptyOsText, withLegacy, visibleFailure, awaitListed, badRules, loadOptions, canCreateFrom, createOutcome, postCreate, watchCreated,
} = await import('./newdevice.js');
const fx = JSON.parse(readFileSync(new URL('../testdata/client/deviceoptions.json', import.meta.url)));
const A = (o) => platformById(o, 'android');
const IOS = (o) => platformById(o, 'ios');
const A36 = 'system-images;android-36.1;google_apis_playstore;arm64-v8a';
const A34 = 'system-images;android-34;google_apis_playstore;arm64-v8a';
const R = (v) => `com.apple.CoreSimulator.SimRuntime.iOS-${v}`;
const T = (n) => `com.apple.CoreSimulator.SimDeviceType.${n}`;
const jsonRes = (status, body) => ({ ok: status >= 200 && status < 300, status, json: async () => body });

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

test('a profile with no AVD yet is offered under its own name', () => {
  assert.deepEqual(profileRowModel({ name: 'medium_phone', label: 'Medium Phone', exists: false }),
    { name: 'medium_phone', label: 'Medium Phone', icon: 'phone', detail: 'medium_phone', disabled: false });
});

test('a profile whose AVD exists is dimmed, since the CLI cannot make a second', () => {
  const row = profileRowModel({ name: 'small_phone', label: 'Small Phone', exists: true });
  assert.equal(row.disabled, true);
  assert.equal(row.detail, 'small_phone · already on this Mac');
});

test('tablets and desktops get the wide icon', () => {
  assert.equal(profileRowModel({ name: 'medium_tablet' }).icon, 'wide');
  assert.equal(profileRowModel({ name: 'large_desktop' }).icon, 'wide');
});

test('a profile without a label falls back to its name', () => {
  assert.equal(profileRowModel({ name: 'small_desktop' }).label, 'small_desktop');
});

test('409 codes read as what to do, not as the status', () => {
  assert.equal(createError(409, { error: 'x', code: 'creating' }), 'Another device is still being created');
  assert.equal(createError(409, { error: 'x', code: 'avdExists' }), 'That device is already on this Mac');
});

test('404 says the CLI is missing', () => {
  assert.match(createError(404, null), /android CLI/);
});

test("anything else shows the server's own error, or the status", () => {
  assert.equal(createError(502, { error: 'list profiles: exit 1' }), 'list profiles: exit 1');
  assert.match(createError(500, null), /500/);
});

// --- the form's pure parts ---------------------------------------------

test('the newest OS is the first the server lists', () => {
  assert.equal(defaultOs(A(fx.two)), A36);
  assert.equal(defaultOs(IOS(fx.two)), R('27-1'));
  assert.equal(defaultOs(IOS(fx.noIosRuntimes)), '');
});

test('devices group as Phone, Foldable, Tablet and empty groups are hidden', () => {
  const g = devicesFor(A(fx.two), A36);
  assert.deepEqual(g.map((x) => x.title), ['Phone', 'Foldable', 'Tablet']);
  const g34 = devicesFor(A(fx.two), A34);
  assert.deepEqual(g34.map((x) => x.title), ['Phone', 'Tablet']);
});

test('a sparse pair leaves iOS 27.1 with the Duo alone', () => {
  const g = devicesFor(IOS(fx.two), R('27-1'));
  assert.deepEqual(g.map((x) => [x.group, x.devices.map((d) => d.label)]), [['foldable', ['iPhone Duo']]]);
});

test('an OS with no pairs entry offers nothing, and a server with no pairs offers all', () => {
  assert.deepEqual(devicesFor(A(fx.two), 'nope'), []);
  assert.equal(devicesFor({ devices: A(fx.two).devices }, 'x').length, 3);
});

test('the default device is the first phone, else the first device there is', () => {
  assert.equal(defaultDevice(devicesFor(A(fx.two), A36)), 'pixel_9');
  assert.equal(defaultDevice(devicesFor(IOS(fx.two), R('27-1'))), T('iPhone-Duo'));
  assert.equal(defaultDevice([]), '');
});

test('changing OS resets a device the new OS lacks and keeps one it has', () => {
  assert.deepEqual(reconcile(IOS(fx.two), R('27-1'), T('iPhone-17')), { os: R('27-1'), device: T('iPhone-Duo') });
  assert.deepEqual(reconcile(IOS(fx.two), R('27-0'), T('iPad-Pro-13')), { os: R('27-0'), device: T('iPad-Pro-13') });
  assert.deepEqual(reconcile(A(fx.two), 'gone', 'gone'), { os: A36, device: 'pixel_9' });
  assert.deepEqual(reconcile(IOS(fx.noIosRuntimes), '', ''), { os: '', device: '' });
});

test('OS versions come from the image or runtime id', () => {
  assert.equal(osVersion('android', { id: A36 }), '36.1');
  assert.equal(osVersion('android', { id: A34 }), '34');
  assert.equal(osVersion('ios', { id: R('27-0') }), '27.0');
  assert.equal(osVersion('ios', { id: 'weird', label: 'iOS 28.2' }), '28.2');
});

test('Android names are prefilled from the device and API level', () => {
  assert.equal(prefillName(fx.two, 'android', A34, 'pixel_9'), 'Pixel_9_API_34');
  assert.equal(prefillName(fx.two, 'android', A34, 'pixel_tablet'), 'Pixel_Tablet_API_34');
});

test('a prefill that collides gets _2, _3 on Android and a space and a number on iOS', () => {
  // Pixel_9_API_36.1 exists in the fixture.
  assert.equal(prefillName(fx.two, 'android', A36, 'pixel_9'), 'Pixel_9_API_36.1_2');
  assert.equal(prefillName(fx.two, 'ios', R('27-0'), T('iPhone-17')), 'iPhone 17 iOS 27.0 2');
  assert.equal(prefillName(fx.two, 'ios', R('26-4'), T('iPhone-17')), 'iPhone 17 iOS 26.4');
  assert.equal(dedupeName('A', { android: ['A', 'A_2'] }, 'android'), 'A_3');
  assert.equal(dedupeName('A', { ios: ['a', 'A 2'] }, 'ios'), 'A 3');
});

test('names collide without regard to case, on either platform and across them', () => {
  assert.equal(dedupeName('pixel_10', { android: ['Pixel_10'] }, 'android'), 'pixel_10_2');
  assert.equal(dedupeName('LOUPE-PROBE', { ios: ['loupe-probe'] }, 'android'), 'LOUPE-PROBE_2');
  const o = { ...fx.two, existingNames: { android: [], ios: ['pixel_9_api_34'] } };
  assert.equal(prefillName(o, 'android', A34, 'pixel_9'), 'Pixel_9_API_34_2');
});

test('sanitizeName turns what the rule rejects into underscores and cuts to length', () => {
  assert.equal(sanitizeName('Pixel 9 (beta)', '^[A-Za-z0-9._-]{1,64}$'), 'Pixel_9__beta_');
  assert.equal(sanitizeName('x'.repeat(80), '^[A-Za-z0-9._-]{1,64}$').length, 64);
  assert.equal(sanitizeName('a b', '^[A-Za-z0-9 ]{1,64}$'), 'a b');
  assert.equal(sanitizeName('a b', undefined), 'a b');
  // iOS's rule starts with a letter or digit and allows spaces and parentheses
  const ios = fx.two.platforms[1].nameRule;
  assert.equal(sanitizeName('iPhone 17 iOS 27.0 (2)', ios), 'iPhone 17 iOS 27.0 (2)');
  assert.equal(sanitizeName('  x', ios), 'x');
  assert.equal(sanitizeName('a'.repeat(80), ios).length, 64);
  assert.equal(sanitizeName('a b', '('), 'a b'); // an invalid rule is ignored, not thrown
});

test('names are checked against the rule and the existing names', () => {
  assert.equal(validateName(fx.two, 'android', 'Fine_1.0'), '');
  assert.match(validateName(fx.two, 'android', 'has space'), /letters, numbers/);
  assert.match(validateName(fx.two, 'android', '   '), /Enter a name/);
  assert.match(validateName(fx.two, 'android', 'Pixel_10'), /already exists/);
  assert.match(validateName(fx.two, 'android', 'pixel_10'), /already exists/);
  assert.match(validateName(fx.two, 'ios', 'PIXEL_10'), /already exists/);
  assert.match(validateName(fx.two, 'android', 'LOUPE-PROBE'), /already exists/);
  assert.match(validateName(fx.two, 'android', '_lead'), /letters, numbers/);
  assert.equal(validateName(fx.two, 'ios', 'has space'), '');
  assert.match(validateName(fx.two, 'ios', 'LOUPE-PROBE'), /already exists/);
});

test('a suffix never pushes a name past the rule’s length', () => {
  const rule = A(fx.two).nameRule;
  const long = 'a'.repeat(64);
  const out = dedupeName(long, { android: [long] }, 'android', rule);
  assert.equal(out.length, 64);
  assert.ok(new RegExp(rule).test(out));
  assert.equal(out.slice(-2), '_2');
  const out3 = dedupeName(long, { android: [long, out] }, 'android', rule);
  assert.equal(out3.slice(-2), '_3');
  assert.equal(out3.length, 64);
  const iosRule = IOS(fx.two).nameRule;
  const l2 = 'b'.repeat(64);
  assert.equal(dedupeName(l2, { ios: [l2] }, 'ios', iosRule).length, 64);
  // short names are untouched
  assert.equal(dedupeName('A', { android: ['a'] }, 'android', rule), 'A_2');
});

test('an Android image with no numeric API uses its id segment, never the label', () => {
  const canary = 'system-images;android-CANARY;google_apis;arm64-v8a';
  assert.equal(osVersion('android', { id: canary, label: 'Android 16 Canary 2' }), 'CANARY');
  assert.equal(osVersion('android', { id: 'odd', label: 'Android 16' }), '');
  const opts = JSON.parse(JSON.stringify(fx.two));
  const a = opts.platforms.find((p) => p.id === 'android');
  a.os.push({ id: canary, label: 'Android 16 Canary 2' });
  a.pairs[canary] = a.pairs[A34];
  assert.equal(prefillName(opts, 'android', canary, 'pixel_9'), 'Pixel_9_API_CANARY');
  a.os.push({ id: 'odd', label: 'Android 16' });
  a.pairs.odd = a.pairs[A34];
  assert.equal(prefillName(opts, 'android', 'odd', 'pixel_9'), 'Pixel_9');
});

test('a nameRule the browser cannot compile is reported', () => {
  assert.deepEqual(badRules(fx.two), []);
  const o = { platforms: [{ id: 'android', nameRule: '(' }, { id: 'ios', nameRule: '^a$' }] };
  assert.deepEqual(badRules(o), ['android']);
});

test('a failure shows only for a job this page started, once', () => {
  const job = fx.failed.job;
  assert.equal(visibleFailure(job, new Set(), new Set()), null); // someone else's, or an old one
  assert.equal(visibleFailure(job, new Set([job.id]), new Set()).error, job.error);
  assert.equal(visibleFailure(job, new Set([job.id]), new Set([job.id])), null);
  assert.equal(visibleFailure(fx.creating.job, new Set([fx.creating.job.id]), new Set()), null);
  assert.equal(visibleFailure(undefined, new Set(), new Set()), null);
});

test('the job reads as none, creating, failed or done', () => {
  assert.equal(jobView(fx.two.job).kind, 'none');
  assert.equal(jobView(fx.creating.job).kind, 'creating');
  assert.deepEqual(jobView(fx.failed.job), { kind: 'failed', id: 'avd:Pixel_9_API_36.1_2', name: 'Pixel_9_API_36.1_2', error: 'Error: no space left on device' });
  assert.equal(jobView(fx.done.job).createdId, 'sim:5C1D7C5E-0000-4000-8000-000000000001');
});

test('Create is enabled for a valid selection and says what it will POST', () => {
  const sel = { platform: 'android', os: A34, device: 'pixel_9', name: 'Mine' };
  assert.deepEqual(submitModel(fx.two, sel), { ok: true, error: '', body: { platform: 'android', os: A34, device: 'pixel_9', name: 'Mine' } });
});

test('Create is disabled for a bad name, no OS, or a create already running', () => {
  const sel = { platform: 'android', os: A34, device: 'pixel_9', name: 'bad name' };
  const bad = submitModel(fx.two, sel);
  assert.equal(bad.ok, false);
  assert.ok(bad.error);
  assert.equal(submitModel(fx.noIosRuntimes, { platform: 'ios', os: '', device: '', name: 'x' }).ok, false);
  assert.equal(submitModel(fx.creating, { ...sel, name: 'Fine' }).ok, false);
  assert.equal(submitModel(fx.failed, { ...sel, name: 'Fine' }).ok, true);
});

test('an empty platform says what to install', () => {
  assert.match(emptyOsText('android'), /Android Studio’s SDK Manager/);
  assert.match(emptyOsText('ios'), /Xcode › Settings › Components/);
});

test('profilesFallback alone is the profile list, and beside iOS adds Android as a legacy entry', () => {
  assert.equal(withLegacy(fx.fallback).kind, 'fallback');
  const r = withLegacy(fx.fallbackWithIos);
  assert.equal(r.kind, 'options');
  assert.deepEqual(platformsOf(r.opts).map((p) => [p.id, !!p.legacy]), [['android', true], ['ios', false]]);
  assert.equal(withLegacy(fx.two).opts, fx.two);
});

test('a one-platform server and an empty one', () => {
  assert.equal(platformsOf(fx.androidOnly).length, 1);
  assert.equal(platformsOf(fx.fallback).length, 0);
  assert.equal(platformById(fx.androidOnly, 'ios').id, 'android');
});

test('canCreate is true for options with a platform and for the fallback, not for a 404', () => {
  assert.equal(canCreateFrom(200, fx.two), true);
  assert.equal(canCreateFrom(200, fx.fallback), true);
  assert.equal(canCreateFrom(200, { platforms: [] }), false);
  assert.equal(canCreateFrom(404, { error: 'x' }), false);
  assert.equal(canCreateFrom(200, null), false);
});

test('outcomes are ok, exists, creating or error', () => {
  assert.equal(createOutcome(202, { id: 'x' }), 'ok');
  assert.equal(createOutcome(409, { code: 'avdExists' }), 'exists');
  assert.equal(createOutcome(409, { code: 'creating' }), 'creating');
  assert.equal(createOutcome(400, { error: 'bad' }), 'error');
  assert.equal(createOutcome(500, null), 'error');
});

// --- fetch round trips -------------------------------------------------

async function atest(name, fn) {
  try { await fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

await atest('loadOptions tells options, the profile fallback, a 404 and a dead server apart', async () => {
  assert.equal((await loadOptions(async () => jsonRes(200, fx.two))).kind, 'options');
  assert.equal((await loadOptions(async () => jsonRes(200, fx.fallback))).kind, 'fallback');
  const none = await loadOptions(async () => jsonRes(404, { error: 'no' }));
  assert.equal(none.kind, 'none');
  assert.equal(none.status, 404);
  assert.equal((await loadOptions(async () => { throw new Error('down'); })).kind, 'error');
});

await atest('postCreate sends the four fields and returns the pending id', async () => {
  let call;
  const r = await postCreate(async (url, init) => { call = { url, init }; return jsonRes(202, { id: 'sim-new:Mine' }); },
    { platform: 'ios', os: 'o', device: 'd', name: 'Mine' });
  assert.equal(call.url, '/api/devices');
  assert.equal(call.init.method, 'POST');
  assert.deepEqual(JSON.parse(call.init.body), { platform: 'ios', os: 'o', device: 'd', name: 'Mine' });
  assert.deepEqual(r, { outcome: 'ok', id: 'sim-new:Mine', message: '' });
});

await atest('postCreate turns refusals into the toast text and the outcome', async () => {
  const exists = await postCreate(async () => jsonRes(409, { error: 'x', code: 'avdExists' }), {});
  assert.deepEqual([exists.outcome, exists.message], ['exists', 'That device is already on this Mac']);
  const busy = await postCreate(async () => jsonRes(409, { error: 'x', code: 'creating' }), {});
  assert.deepEqual([busy.outcome, busy.message], ['creating', 'Another device is still being created']);
  const bad = await postCreate(async () => jsonRes(400, { error: 'device not offered for that OS' }), {});
  assert.deepEqual([bad.outcome, bad.message], ['error', 'device not offered for that OS']);
  const down = await postCreate(async () => { throw new Error('down'); }, {});
  assert.equal(down.outcome, 'error');
});

// A sleep that yields to timers, and a short maxMs, so a watcher that
// regresses into polling forever fails an assertion instead of hanging.
const tick = () => new Promise((r) => setTimeout(r, 0));
const W = { sleep: tick, maxMs: 200 };
const settle = () => new Promise((r) => setTimeout(r, 60));

await atest('watchCreated waits for job.createdId and hands it over once', async () => {
  const seq = [fx.creating, fx.creating, fx.done];
  const got = [];
  watchCreated({
    fetchImpl: async () => jsonRes(200, seq.shift() || fx.done),
    id: 'sim-new:Phone A', name: 'Phone A', ...W, onResolved: (id) => got.push(id),
  });
  await settle();
  assert.deepEqual(got, ['sim:5C1D7C5E-0000-4000-8000-000000000001']);
});

await atest('watchCreated gives up on a failed job, a 401, 403 or 404 and a cancel', async () => {
  const got = [];
  let fetches = 0;
  const failedJob = { ...fx.failed, job: { ...fx.failed.job, id: 'sim-new:X', name: 'X' } };
  watchCreated({ fetchImpl: async () => { fetches++; return jsonRes(200, failedJob); }, id: 'sim-new:X', name: 'X', ...W, onResolved: (i) => got.push(i) });
  await settle();
  assert.equal(fetches, 1);
  for (const status of [401, 403, 404]) {
    let n = 0;
    watchCreated({ fetchImpl: async () => { n++; return jsonRes(status, {}); }, id: 'x', name: 'x', ...W, onResolved: (i) => got.push(i) });
    await settle();
    assert.equal(n, 1, `status ${status}`);
  }
  let n = 0;
  const cancel = watchCreated({ fetchImpl: async () => { n++; return jsonRes(200, fx.creating); }, id: 'sim-new:z', name: 'z',
    maxMs: 200, sleep: () => new Promise((r) => setTimeout(r, 5)), onResolved: (i) => got.push(i) });
  cancel();
  await settle();
  assert.equal(n, 0);
  assert.deepEqual(got, []);
});

await atest('watchCreated ignores another create’s done job', async () => {
  const got = [];
  let n = 0;
  watchCreated({ fetchImpl: async () => { n++; return jsonRes(200, n < 3 ? fx.done : { ...fx.done, job: { ...fx.done.job, id: 'sim-new:Mine', name: 'Mine' } }); },
    id: 'sim-new:Mine', name: 'Mine', ...W, onResolved: (i) => got.push(i) });
  await settle();
  assert.equal(n, 3);
  assert.equal(got.length, 1);
});

await atest('watchCreated ignores a stale done job of the same name until the new create runs', async () => {
  const stale = fx.done.job; // same id and createdId as before Create was pressed
  const before = { id: stale.id, createdId: stale.createdId };
  const fresh = 'sim:NEW-UDID';
  const got = [];
  let n = 0;
  const running = { ...fx.done, job: { id: stale.id, platform: 'ios', name: stale.name, state: 'creating' } };
  const seq = [fx.done, fx.done, running, { ...fx.done, job: { ...stale, createdId: stale.createdId } }];
  watchCreated({ fetchImpl: async () => { n++; return jsonRes(200, seq.shift() || fx.done); },
    id: stale.id, name: stale.name, before, ...W, onResolved: (i) => got.push(i) });
  await settle();
  // the stale job was skipped twice, then the creating job proved this create ran
  assert.equal(n, 4);
  assert.deepEqual(got, [stale.createdId]);
  // a different createdId resolves at once, with no creating seen
  const got2 = [];
  let n2 = 0;
  watchCreated({ fetchImpl: async () => { n2++; return jsonRes(200, { ...fx.done, job: { ...stale, createdId: fresh } }); },
    id: stale.id, name: stale.name, before, ...W, onResolved: (i) => got2.push(i) });
  await settle();
  assert.equal(n2, 1);
  assert.deepEqual(got2, [fresh]);
  // and never when the stale job is all there is
  const got3 = [];
  let n3 = 0;
  const cancel = watchCreated({ fetchImpl: async () => { n3++; return jsonRes(200, fx.done); },
    id: stale.id, name: stale.name, before, maxMs: 100, sleep: tick, onResolved: (i) => got3.push(i) });
  await new Promise((r) => setTimeout(r, 150));
  cancel();
  assert.ok(n3 > 1);
  assert.deepEqual(got3, []);
});

await atest('awaitListed retries the refresh until the id is listed, within bounds', async () => {
  let polls = 0;
  const listed = new Set();
  const r1 = await awaitListed({ poll: async () => { polls++; if (polls === 3) listed.add('sim:a'); },
    isListed: () => listed.has('sim:a'), stillHere: () => true, sleep: tick });
  assert.equal(r1, 'listed');
  assert.equal(polls, 3);
  let p2 = 0;
  const r2 = await awaitListed({ poll: async () => { p2++; }, isListed: () => false, stillHere: () => true, attempts: 4, sleep: tick });
  assert.equal(r2, 'unlisted');
  assert.equal(p2, 4);
  let here = true;
  const r3 = await awaitListed({ poll: async () => { here = false; }, isListed: () => true, stillHere: () => here, sleep: tick });
  assert.equal(r3, 'moved');
  assert.equal(await awaitListed({ poll: async () => { throw new Error('no'); }, isListed: () => true, stillHere: () => false, sleep: tick }), 'moved');
});

if (failures) {
  console.log(`\n${failures} failed`);
  process.exit(1);
}
