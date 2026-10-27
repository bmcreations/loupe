// Tests for appcard.js's pure functions. Run: node web/appcard.test.mjs
import assert from 'node:assert/strict';

const { initials, displayName, formatApp, shouldRefetch } = await import('./appcard.js');

let failures = 0;
// test() has to accommodate the mount()-level tests added for client Task 9's
// review (corners.js/appcard.js/devicesettings.js/inspect.js all fetch, so
// their mount() tests are async): a bare `try { fn(); }` never awaits an
// async fn, so a rejected assertion inside one used to be silently missed
// and the run still printed PASS -- caught only because the review's own
// probe.mjs disagreed with a suite that claimed everything was green. Each
// test's promise is collected and the run waits for all of them before
// deciding pass/fail, rather than awaiting inline, so a slow async test does
// not block sync ones from logging in order.
const pending = [];
function test(name, fn) {
  pending.push((async () => {
    try { await fn(); console.log(`  PASS ${name}`); }
    catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
  })());
}

// --- initials ---------------------------------------------------------------

test('initials takes the last dotted segment', () => {
  assert.equal(initials('com.android.settings'), 'SE');
});

test('initials uppercases', () => {
  assert.equal(initials('com.example.myApp'), 'MY');
});

test('initials falls back to ? for empty input', () => {
  assert.equal(initials(''), '?');
  assert.equal(initials(null), '?');
  assert.equal(initials(undefined), '?');
});

test('initials falls back to ? for a package with no letters', () => {
  assert.equal(initials('123.456'), '?');
  assert.equal(initials('...'), '?');
});

test('initials strips non-letters from the last segment', () => {
  assert.equal(initials('com.example.app2'), 'AP');
});

// --- formatApp ----------------------------------------------------------

test('formatApp returns null with no package', () => {
  assert.equal(formatApp(null), null);
  assert.equal(formatApp(undefined), null);
  assert.equal(formatApp({}), null);
});

test('formatApp titles on a readable name, keeps the package under it, and folds the developer fields', () => {
  const app = {
    package: 'com.android.settings',
    activity: '.Settings',
    versionName: '14',
    versionCode: 34,
    pid: 1234,
    apkPath: '/system/priv-app/Settings/Settings.apk',
  };
  const got = formatApp(app);
  assert.equal(got.title, 'Settings');
  assert.equal(got.subtitle, 'com.android.settings');
  assert.deepEqual(got.rows, [
    { label: 'Screen', value: 'Settings' },
    { label: 'Version', value: '14 (34)' },
  ]);
  assert.deepEqual(got.details, [
    { label: 'Activity', value: '.Settings' },
    { label: 'PID', value: '1234' },
    { label: 'APK', value: '/system/priv-app/Settings/Settings.apk' },
  ]);
});

test('formatApp prefers a label the server sends', () => {
  const got = formatApp({ package: 'com.android.chrome', label: 'Chrome Beta' });
  assert.equal(got.title, 'Chrome Beta');
  assert.equal(got.subtitle, 'com.android.chrome');
});

test('formatApp shows a fully qualified activity by its class name', () => {
  const got = formatApp({ package: 'com.android.chrome', activity: 'com.google.android.apps.chrome.Main' });
  assert.deepEqual(got.rows, [{ label: 'Screen', value: 'Main' }]);
  assert.deepEqual(got.details, [{ label: 'Activity', value: 'com.google.android.apps.chrome.Main' }]);
});

test('formatApp shows a version name or code alone when only one arrives', () => {
  assert.deepEqual(formatApp({ package: 'a.b', versionName: '1.2' }).rows, [{ label: 'Version', value: '1.2' }]);
  assert.deepEqual(formatApp({ package: 'a.b', versionCode: 7 }).rows, [{ label: 'Version', value: '(7)' }]);
});

test('formatApp never shows a value the device did not send', () => {
  // Every field but Package is "best effort" per app.go's own comment.
  // An absent or zero field is left out, not rendered as "" or "0".
  const got = formatApp({ package: 'com.example.app', versionCode: 0, pid: 0 });
  assert.equal(got.title, 'App');
  assert.deepEqual(got.rows, []);
  assert.deepEqual(got.details, []);
});

test('displayName falls back to the package when its last segment has no letters', () => {
  assert.equal(displayName('com.example.123'), 'com.example.123');
});

test('initials reads a display name as well as a package', () => {
  assert.equal(initials('Settings'), 'SE');
  assert.equal(initials('com.android.settings'), 'SE');
});

test('formatApp omits an empty-string field', () => {
  const got = formatApp({ package: 'com.example.app', activity: '', apkPath: '' });
  assert.deepEqual(got.rows, []);
  assert.deepEqual(got.details, []);
});

// --- shouldRefetch --------------------------------------------------------

test('shouldRefetch is true before the first fetch', () => {
  assert.equal(shouldRefetch(null, 1000), true);
  assert.equal(shouldRefetch(undefined, 1000), true);
});

test('shouldRefetch rate-limits to the minimum interval', () => {
  assert.equal(shouldRefetch(1000, 1500, 2000), false);
  assert.equal(shouldRefetch(1000, 2999, 2000), false);
});

test('shouldRefetch allows a refetch at exactly the interval boundary', () => {
  assert.equal(shouldRefetch(1000, 3000, 2000), true);
});

test('shouldRefetch defaults its interval to 2000ms', () => {
  assert.equal(shouldRefetch(1000, 2999), false);
  assert.equal(shouldRefetch(1000, 3000), true);
});

// --- mount() -----------------------------------------------------------
//
// Against web/fakedom.js's fake DOM. Added for client Task 9's review: the
// pure-function tests above never exercised a 501 hiding/re-showing the
// section, or a response racing a target switch (trap 41) -- both of which
// mutated clean in the first pass.

const { installFakeDom, tick, deferred, jsonResponse } = await import('./fakedom.js');
const { mount } = await import('./appcard.js');

function container() {
  installFakeDom();
  return document.createElement('div');
}

test('a 501 hides the section and renders nothing', async () => {
  const c = container();
  let hidden = 0;
  const m = mount(c, { targetId: () => 'synthetic', fetchImpl: async () => jsonResponse(501, { error: 'no app inspector' }), onUnavailable: () => { hidden++; } });
  await m.refresh();
  assert.equal(hidden, 1);
  assert.equal(c.children.length, 0);
});

test('a 501 does not latch forever: switching to a target that answers 200 shows the section again', async () => {
  const c = container();
  let hidden = 0, shown = 0;
  let tid = 'synthetic';
  const fetchImpl = async () => (tid === 'synthetic' ? jsonResponse(501, { error: 'x' }) : jsonResponse(200, { package: 'com.example.app' }));
  const m = mount(c, { targetId: () => tid, fetchImpl, onUnavailable: () => { hidden++; }, onAvailable: () => { shown++; } });
  await m.refresh();
  assert.equal(hidden, 1);
  tid = 'emulator-5554';
  await m.refresh();
  assert.equal(shown, 1, 'a fresh target must get its own attempt, not inherit the previous one\'s 501');
  assert.equal(c.querySelectorAll('.appSubtitle')[0].textContent, 'com.example.app');
});

test('a slow response for A does not paint after a switch to B (trap 41)', async () => {
  const c = container();
  let tid = 'A';
  const dA = deferred();
  const fetchImpl = async (url) => {
    if (String(url).includes('target=A')) { await dA.promise; return jsonResponse(200, { package: 'com.a.app' }); }
    return jsonResponse(200, { package: 'com.b.app' });
  };
  const m = mount(c, { targetId: () => tid, fetchImpl });
  const pA = m.refresh();
  tid = 'B';
  await m.refresh();
  dA.resolve();
  await pA;
  await tick(2);
  assert.equal(c.querySelectorAll('.appSubtitle')[0].textContent, 'com.b.app', 'A\'s slow response must not overwrite B\'s already-rendered card');
});

test('a slow BODY PARSE for A (the fetch itself already resolved) does not paint after a switch to B (trap 41, second guard)', async () => {
  const c = container();
  let tid = 'A';
  const dJson = deferred();
  const fetchImpl = async (url) => {
    if (String(url).includes('target=A')) {
      return { status: 200, ok: true, json: async () => { await dJson.promise; return { package: 'com.a.app' }; } };
    }
    return jsonResponse(200, { package: 'com.b.app' });
  };
  const m = mount(c, { targetId: () => tid, fetchImpl });
  const pA = m.refresh(); // fetchImpl for A resolves immediately; res.json() is what's slow
  await tick(1); // let refresh() get past the outer fetch and start awaiting res.json()
  tid = 'B';
  await m.refresh();
  dJson.resolve();
  await pA;
  await tick(2);
  assert.equal(c.querySelectorAll('.appSubtitle')[0].textContent, 'com.b.app', 'A\'s slow body parse must not overwrite B\'s already-rendered card');
});

test('a long, unbroken value gets appRows for CSS wrapping and keeps the full text in title, not truncated in the DOM (owner iPhone report: values overflowing the sheet)', async () => {
  const c = container();
  // The real emulator-5554 shape, from a read-only GET /api/app there, plus
  // an activity name long enough to need wrapping rather than fitting.
  const app = {
    package: 'com.acmechat.app.android',
    activity: 'com.android.searchlauncher.SomeVeryLongActivityNameThatWouldOverflowTheSheetIfNotWrapped',
    versionName: '2026.9.3',
    versionCode: 4614,
    pid: 14348,
    apkPath: '/data/app/~~9ky_ES6FyLII_oRNtN-usg==/com.acmechat.app.android-ZCYbirXKxXNWsovsKUCcoA==/base.apk',
  };
  const m = mount(c, { targetId: () => 'emulator-5554', fetchImpl: async () => jsonResponse(200, app) });
  await m.refresh();

  const dl = c.querySelector('dl');
  assert.ok(dl.className.split(' ').includes('appRows'), 'appRows must be on the row list so style.css can left-align/wrap its values');
  assert.ok(dl.className.split(' ').includes('sheetRows'), 'still a sheetRows grid, just with the appRows modifier');

  const dds = Array.from(c.querySelectorAll('dd'));
  const activityDd = dds.find((dd) => dd.textContent === app.activity);
  assert.ok(activityDd, 'the long activity value is rendered in full, not truncated in the DOM -- wrapping is CSS-only');
  assert.equal(activityDd.title, app.activity, 'title carries the exact value for a hover/long-press even if the wrap still looks awkward at a given width');

  const apkDd = dds.find((dd) => dd.textContent === app.apkPath);
  assert.ok(apkDd, 'the APK path is rendered in full');
  assert.equal(apkDd.title, app.apkPath);

  // Version, Version code and PID were reported as showing no visible value.
  // Pin that all three are present in formatted rows for a normal
  // best-effort response -- i.e. that was a rendering/overflow bug, not
  // missing data (formatApp already omits a genuinely absent/zero field, per
  // the tests above).
  const texts = dds.map((dd) => dd.textContent);
  assert.ok(texts.includes('2026.9.3 (4614)'), 'Version name and code must render as one row');
  assert.ok(texts.includes('14348'), 'PID must render');
});

await Promise.all(pending);
console.log(failures ? `\n${failures} failing` : '\nall passing');
process.exit(failures ? 1 : 0);
