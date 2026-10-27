// Tests for devicesettings.js's pure functions. Run: node web/devicesettings.test.mjs
import assert from 'node:assert/strict';

const { labelFor, optionLabel, clampRange, parseSettingsError, applyResponse } = await import('./devicesettings.js');

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

// --- labelFor -----------------------------------------------------------

test('labelFor knows every field settingFields declares', () => {
  assert.equal(labelFor('darkMode'), 'Dark mode');
  assert.equal(labelFor('fontScale'), 'Font scale');
  assert.equal(labelFor('reduceMotion'), 'Reduce motion');
  assert.equal(labelFor('layoutBounds'), 'Layout bounds');
  assert.equal(labelFor('talkBack'), 'TalkBack');
  assert.equal(labelFor('posture'), 'Posture');
});

test('optionLabel names each posture and falls back to the raw value', () => {
  assert.equal(optionLabel('posture', 'closed'), 'Closed');
  assert.equal(optionLabel('posture', 'halfOpened'), 'Half open');
  assert.equal(optionLabel('posture', 'opened'), 'Open');
  assert.equal(optionLabel('posture', 'tent'), 'tent');
  assert.equal(optionLabel('darkMode', 'x'), 'x');
});

test('labelFor falls back to the raw id for an unknown field', () => {
  assert.equal(labelFor('someFutureSetting'), 'someFutureSetting');
});

// --- clampRange -----------------------------------------------------------

test('clampRange passes a value already inside the range', () => {
  assert.equal(clampRange(1.2, 0.85, 2.0), 1.2);
});

test('clampRange bounds a value below the minimum', () => {
  assert.equal(clampRange(0.5, 0.85, 2.0), 0.85);
});

test('clampRange bounds a value above the maximum', () => {
  assert.equal(clampRange(3.0, 0.85, 2.0), 2.0);
});

test('clampRange treats NaN as the minimum, not a pass-through', () => {
  assert.equal(clampRange(NaN, 0.85, 2.0), 0.85);
});

test('clampRange treats a non-number as the minimum', () => {
  assert.equal(clampRange('1.5', 0.85, 2.0), 0.85);
  assert.equal(clampRange(undefined, 0.85, 2.0), 0.85);
});

// --- parseSettingsError ---------------------------------------------------

test('parseSettingsError reads a plain 400 body', () => {
  const got = parseSettingsError(400, { error: 'unknown setting "bogus"' });
  assert.equal(got.status, 400);
  assert.equal(got.message, 'unknown setting "bogus"');
  assert.deepEqual(got.failed, []);
  assert.deepEqual(got.notRestored, []);
  assert.equal(got.settings, null);
});

test('parseSettingsError reads a 502 the same way', () => {
  const got = parseSettingsError(502, { error: 'device unreachable' });
  assert.equal(got.message, 'device unreachable');
  assert.equal(got.settings, null);
});

test('parseSettingsError reads a full 409 body', () => {
  const settings = [{ id: 'darkMode', type: 'bool', value: true, supported: true }];
  const got = parseSettingsError(409, {
    error: 'apply failed',
    failed: ['fontScale'],
    notRestored: ['reduceMotion'],
    settings,
  });
  assert.equal(got.status, 409);
  assert.deepEqual(got.failed, ['fontScale']);
  assert.deepEqual(got.notRestored, ['reduceMotion']);
  assert.deepEqual(got.settings, settings);
});

test('parseSettingsError handles a 409 whose re-read itself failed', () => {
  // settings.go's putSettings only adds "settings" when its own detached
  // re-read succeeds -- absent means "not confirmed", not "empty".
  const got = parseSettingsError(409, { error: 'apply failed', failed: ['fontScale'] });
  assert.equal(got.settings, null);
  assert.deepEqual(got.notRestored, []);
});

test('parseSettingsError falls back to a generic message with no body', () => {
  const got = parseSettingsError(400, null);
  assert.equal(got.message, 'settings: 400');
});

// --- applyResponse ----------------------------------------------------

test('applyResponse replaces current with a real incoming array', () => {
  const current = [{ id: 'darkMode', value: false }];
  const incoming = [{ id: 'darkMode', value: true }];
  assert.deepEqual(applyResponse(current, incoming), incoming);
});

test('applyResponse keeps current when incoming is null -- nothing confirmed', () => {
  const current = [{ id: 'darkMode', value: false }];
  assert.deepEqual(applyResponse(current, null), current);
  assert.deepEqual(applyResponse(current, undefined), current);
});

test('applyResponse never invents a value: an empty current stays empty', () => {
  assert.deepEqual(applyResponse(null, null), []);
  assert.deepEqual(applyResponse(undefined, undefined), []);
});

test('applyResponse accepts an empty incoming array as a real, confirmed read', () => {
  const current = [{ id: 'darkMode', value: false }];
  assert.deepEqual(applyResponse(current, []), []);
});

// review round 5, finding 3: W4-W7 survived because every prior test's
// `incoming` happened to carry the same value `current` already had, so a
// mutation that stopped consulting `pendingIds` and just returned `incoming`
// outright produced an identical result and nothing failed. This pins the
// one thing pendingIds exists for: while an id is pending, the row must
// show `current`'s own value even when `incoming` disagrees with it.
test('applyResponse: a pending id keeps current\'s own value, not a disagreeing incoming one (review round 5, finding 3)', () => {
  const current = [{ id: 'darkMode', value: true, supported: true }];
  const incoming = [{ id: 'darkMode', value: false, supported: true }]; // a GET that disagrees with current
  const got = applyResponse(current, incoming, new Set(['darkMode']));
  assert.deepEqual(got, current, 'pending must hold the row at current, not adopt a disagreeing incoming value');
});

test('applyResponse: an id not in pendingIds still takes the incoming value', () => {
  const current = [{ id: 'darkMode', value: true }, { id: 'fontScale', value: 1 }];
  const incoming = [{ id: 'darkMode', value: false }, { id: 'fontScale', value: 1.5 }];
  const got = applyResponse(current, incoming, new Set(['darkMode']));
  assert.deepEqual(got, [{ id: 'darkMode', value: true }, { id: 'fontScale', value: 1.5 }]);
});

// --- mount() -----------------------------------------------------------
//
// Against web/fakedom.js's fake DOM. Added for client Task 9's review, which
// found the mount() half untested: a 501 that never cleared on a target
// switch, a PUT body shape nothing checked, and a control left disabled
// forever after a write all mutated clean in the first pass.

const { installFakeDom, tick, deferred, jsonResponse } = await import('./fakedom.js');
const { mount } = await import('./devicesettings.js');

function container() {
  installFakeDom();
  return document.createElement('div');
}

const darkMode = { id: 'darkMode', type: 'bool', value: false, supported: true };
const fontScale = { id: 'fontScale', type: 'range', value: 1.0, min: 0.85, max: 2.0, supported: true };

test('a 501 hides the section and renders nothing', async () => {
  const c = container();
  let hidden = 0;
  const m = mount(c, { targetId: () => 'synthetic', fetchImpl: async () => jsonResponse(501, { error: 'no settings' }), onUnavailable: () => { hidden++; } });
  await m.refresh();
  assert.equal(hidden, 1);
  assert.equal(c.children.length, 0);
});

test('a 501 does not latch forever: a target switch to one that answers 200 shows the section again', async () => {
  const c = container();
  let hidden = 0, shown = 0;
  let tid = 'synthetic';
  const fetchImpl = async () => (tid === 'synthetic' ? jsonResponse(501, { error: 'x' }) : jsonResponse(200, { settings: [darkMode] }));
  const m = mount(c, { targetId: () => tid, fetchImpl, onUnavailable: () => { hidden++; }, onAvailable: () => { shown++; } });
  await m.refresh();
  assert.equal(hidden, 1);
  tid = 'emulator-5554';
  await m.refresh();
  assert.equal(shown, 1, 'the new target must get its own attempt, not inherit the previous target\'s 501');
  assert.equal(c.querySelectorAll('.settingRow').length, 1);
});

test('GET renders one row per setting, labeled and reflecting supported/value', async () => {
  const c = container();
  const m = mount(c, { targetId: () => 'x', fetchImpl: async () => jsonResponse(200, { settings: [darkMode, fontScale] }) });
  await m.refresh();
  const rows = c.querySelectorAll('.settingRow');
  assert.equal(rows.length, 2);
  assert.equal(c.querySelectorAll('.settingLabel')[0].textContent, 'Dark mode');
});

test('toggling a checkbox sends exactly one PUT with {changes:{id:value}} and disables the control immediately', async () => {
  const c = container();
  const calls = [];
  const m = mount(c, {
    targetId: () => 'x',
    fetchImpl: async (url, opts) => {
      if (opts && opts.method === 'PUT') { calls.push(JSON.parse(opts.body)); return jsonResponse(200, { settings: [{ ...darkMode, value: true }] }); }
      return jsonResponse(200, { settings: [darkMode] });
    },
  });
  await m.refresh();
  const input = c.querySelectorAll('.settingSwitch')[0];
  input.checked = true;
  input.dispatch('change');
  assert.equal(input.disabled, true, 'must disable synchronously, not wait for the PUT to resolve (review HIGH-4)');
  await tick(2);
  assert.equal(calls.length, 1);
  assert.deepEqual(calls[0], { changes: { darkMode: true } });
});

// settings-put telemetry: one record per PUT with the ids and the outcome,
// and never the value written.
async function putReports(putResponse) {
  const c = container();
  const reports = [];
  const m = mount(c, {
    targetId: () => 'x',
    report: (r) => reports.push(r),
    fetchImpl: async (url, opts) => {
      if (opts && opts.method === 'PUT') return putResponse();
      return jsonResponse(200, { settings: [darkMode] });
    },
  });
  await m.refresh();
  const input = c.querySelectorAll('.settingSwitch')[0];
  input.checked = true;
  input.dispatch('change');
  await tick(2);
  return reports.filter((r) => r.event === 'settings-put');
}

test('a successful PUT reports settings-put with its ids and outcome ok', async () => {
  const got = await putReports(() => jsonResponse(200, { settings: [{ ...darkMode, value: true }] }));
  assert.deepEqual(got, [{ event: 'settings-put', ids: ['darkMode'], outcome: 'ok', status: 200 }]);
});

test('a rejected PUT reports settings-put with outcome error and the status', async () => {
  const got = await putReports(() => jsonResponse(400, { error: 'unknown setting "darkMode"' }));
  assert.deepEqual(got, [{ event: 'settings-put', ids: ['darkMode'], outcome: 'error', status: 400 }]);
});

test('a PUT that throws reports settings-put with outcome error', async () => {
  const got = await putReports(() => { throw new Error('offline'); });
  assert.equal(got.length, 1);
  assert.equal(got[0].outcome, 'error');
  assert.deepEqual(got[0].ids, ['darkMode']);
});

test('a 501 PUT reports settings-put with outcome unavailable', async () => {
  const got = await putReports(() => jsonResponse(501, { error: 'no settings' }));
  assert.deepEqual(got, [{ event: 'settings-put', ids: ['darkMode'], outcome: 'unavailable', status: 501 }]);
});

test('after a successful write, the re-rendered control is enabled again, not stuck disabled', async () => {
  const c = container();
  const m = mount(c, {
    targetId: () => 'x',
    fetchImpl: async (url, opts) => {
      if (opts && opts.method === 'PUT') return jsonResponse(200, { settings: [{ ...darkMode, value: true }] });
      return jsonResponse(200, { settings: [darkMode] });
    },
  });
  await m.refresh();
  c.querySelectorAll('.settingSwitch')[0].dispatch('change');
  await tick(2);
  const input = c.querySelectorAll('.settingSwitch')[0]; // render() replaced the element
  assert.equal(input.disabled, false, 'render() must run with pending already cleared, or the control stays disabled forever (review HIGH-4)');
  assert.equal(input.checked, true);
});

test('two quick toggles before the first PUT resolves send only one PUT', async () => {
  const c = container();
  const calls = [];
  const d = deferred();
  const m = mount(c, {
    targetId: () => 'x',
    fetchImpl: async (url, opts) => {
      if (opts && opts.method === 'PUT') { calls.push(1); await d.promise; return jsonResponse(200, { settings: [{ ...darkMode, value: true }] }); }
      return jsonResponse(200, { settings: [darkMode] });
    },
  });
  await m.refresh();
  const input = c.querySelectorAll('.settingSwitch')[0];
  input.checked = true;
  input.dispatch('change');
  input.dispatch('change'); // fired again before the control's own disabled=true would stop a real pointer
  d.resolve();
  await tick(2);
  assert.equal(calls.length, 1, 'a write already in flight for this id must not start a second one');
});

test('a slider PUT fires on change (release), not on every input tick', async () => {
  const c = container();
  const calls = [];
  const m = mount(c, {
    targetId: () => 'x',
    fetchImpl: async (url, opts) => {
      if (opts && opts.method === 'PUT') { calls.push(1); return jsonResponse(200, { settings: [fontScale] }); }
      return jsonResponse(200, { settings: [fontScale] });
    },
  });
  await m.refresh();
  const input = c.querySelectorAll('.settingRange')[0];
  input.value = '1.2';
  input.dispatch('input');
  input.value = '1.4';
  input.dispatch('input');
  assert.equal(calls.length, 0, 'dragging must not write yet');
  input.dispatch('change');
  await tick(2);
  assert.equal(calls.length, 1, 'release must write exactly once');
});

test('a write in flight for A is discarded, not re-rendered, after a switch to B (trap 41)', async () => {
  const c = container();
  const d = deferred();
  let tid = 'A';
  // B's GET answers with a setting A never had (reduceMotion, not darkMode),
  // so a mutant that lets A's stale write through is caught on CONTENT, not
  // just row count -- A's write response also produces exactly one row.
  const bSetting = { id: 'reduceMotion', type: 'bool', supported: true, value: true };
  const m = mount(c, {
    targetId: () => tid,
    fetchImpl: async (url, opts) => {
      if (opts && opts.method === 'PUT') { await d.promise; return jsonResponse(200, { settings: [{ ...darkMode, value: true }] }); }
      if (String(url).includes('target=B')) return jsonResponse(200, { settings: [bSetting] });
      return jsonResponse(200, { settings: [darkMode] });
    },
  });
  await m.refresh();
  const input = c.querySelectorAll('.settingSwitch')[0];
  input.checked = true;
  input.dispatch('change');
  tid = 'B';
  await m.refresh(); // bumps gen -- B's own row now lives in `container`
  d.resolve();
  await tick(2);
  // The re-render B's refresh produced must survive; A's stale write must
  // not have painted its response over it.
  assert.equal(c.querySelectorAll('.settingRow').length, 1);
  assert.equal(c.querySelectorAll('.settingLabel')[0].textContent, labelFor('reduceMotion'), 'A\'s stale write must not overwrite B\'s already-rendered row');
});

test('a 400 with no settings body reverts the rendered control to its pre-write value', async () => {
  const c = container();
  const m = mount(c, {
    targetId: () => 'A',
    fetchImpl: async (url, opts) => {
      if (opts && opts.method === 'PUT') return jsonResponse(400, { error: 'unknown setting "darkMode"' });
      return jsonResponse(200, { settings: [darkMode] }); // value: false
    },
  });
  await m.refresh();
  const input = c.querySelectorAll('.settingSwitch')[0];
  input.checked = true;
  input.dispatch('change');
  await tick(2);
  const rerendered = c.querySelectorAll('.settingSwitch')[0];
  assert.equal(rerendered.checked, false, 'a 400 with no settings body must revert the visible control, not leave the optimistic value');
  assert.equal(rerendered.disabled, false, 'the control must be re-enabled after the error render');
});

test('a network error (fetch throws) re-renders the control back to the device\'s value (review round 5, finding 1)', async () => {
  const c = container();
  const m = mount(c, {
    targetId: () => 'A',
    fetchImpl: async (url, opts) => {
      if (opts && opts.method === 'PUT') throw new Error('network down');
      return jsonResponse(200, { settings: [darkMode] });
    },
  });
  await m.refresh();
  c.querySelectorAll('.settingSwitch')[0].dispatch('change');
  await tick(2);
  // The catch branch calls render(), same as every other error status
  // (review round 5, finding 1) -- not the closure-captured revert(), which
  // would touch this original element even after a refresh had already
  // replaced it elsewhere. Query fresh, the same way the 400/409 tests
  // above already do, since a live control after any error path is a new
  // element.
  const after = c.querySelectorAll('.settingSwitch')[0];
  assert.equal(after.checked, false, 'a thrown fetch must show the device\'s real value, not the optimistic toggle');
  assert.equal(after.disabled, false, 'the control must be re-enabled after the error');
});

test('a 409 with a re-read settings array shows the device\'s real value, not the optimistic one', async () => {
  const c = container();
  const m = mount(c, {
    targetId: () => 'A',
    fetchImpl: async (url, opts) => {
      if (opts && opts.method === 'PUT') {
        return jsonResponse(409, {
          error: 'apply failed',
          failed: ['darkMode'],
          settings: [{ ...darkMode, value: false }],
        });
      }
      return jsonResponse(200, { settings: [darkMode] });
    },
  });
  await m.refresh();
  const input = c.querySelectorAll('.settingSwitch')[0];
  input.checked = true;
  input.dispatch('change');
  await tick(2);
  const rerendered = c.querySelectorAll('.settingSwitch')[0];
  assert.equal(rerendered.checked, false, 'the 409 re-read is the device\'s real state and must win over the optimistic toggle');
});

test('a 400 shows a toast through the real dep shape app.js passes -- an element, not a wrapper function (review round 3, finding 1)', async () => {
  const c = container();
  const toastEl = document.createElement('div');
  toastEl.hidden = true;
  const m = mount(c, {
    targetId: () => 'A',
    toast: toastEl, // the shape app.js actually passes: the #toast element itself
    fetchImpl: async (url, opts) => {
      if (opts && opts.method === 'PUT') return jsonResponse(400, { error: 'unknown setting "darkMode"' });
      return jsonResponse(200, { settings: [darkMode] });
    },
  });
  await m.refresh();
  c.querySelectorAll('.settingSwitch')[0].dispatch('change');
  await tick(2);
  // devicesettings.js calls showToast(deps.toast, message) internally. If it
  // were still handed a closure (the pre-review-3 bug) or if showToast were
  // handed the wrong argument, this element would never be touched: it sets
  // properties ON a function object, which is not this assertion's target.
  assert.equal(toastEl.hidden, false, 'a real element dep must actually be shown, not silently no-op');
  assert.equal(toastEl.textContent, 'unknown setting "darkMode"');
});

test('a write on A in flight, then a switch to B, leaves B\'s control enabled -- pending must not survive the switch (review round 3, finding 3)', async () => {
  const c = container();
  const d = deferred();
  let tid = 'A';
  const bSetting = { id: 'darkMode', type: 'bool', supported: true, value: false };
  const m = mount(c, {
    targetId: () => tid,
    fetchImpl: async (url, opts) => {
      if (opts && opts.method === 'PUT') { await d.promise; return jsonResponse(200, { settings: [{ ...darkMode, value: true }] }); }
      if (String(url).includes('target=B')) return jsonResponse(200, { settings: [bSetting] });
      return jsonResponse(200, { settings: [darkMode] });
    },
  });
  await m.refresh(); // A
  c.querySelectorAll('.settingSwitch')[0].dispatch('change'); // A's darkMode write starts, never resolves yet
  tid = 'B';
  await m.refresh(); // bumps gen -- B's own darkMode row now lives in `container`
  const bInput = c.querySelectorAll('.settingSwitch')[0];
  assert.equal(bInput.disabled, false, 'B\'s own control must not inherit A\'s in-flight write as "pending" for the same id');
  assert.equal(bInput.checked, false, 'B\'s row must show B\'s own value, not A\'s');
  d.resolve(); // let A's stale write resolve after the switch
  await tick(2);
  const bInputAfter = c.querySelectorAll('.settingSwitch')[0];
  assert.equal(bInputAfter.disabled, false, 'A\'s belated write resolving must not touch B\'s control at all');
  assert.equal(bInputAfter.checked, false, 'A\'s belated write must not paint its value over B\'s row');
});

// --- review round 4 --------------------------------------------------
//
// The round 3 fix keyed `pending` by `gen + ':' + id`, but `renderRow`
// computed its own key through `pendingKey(id)`, which still returned the
// bare id -- so the disabled check the write-side key was built for never
// matched anything, and `refresh()` bumped `gen` on every call including a
// refresh of the *same* target. These three tests are the reviewer's own
// reproductions, kept as regressions against a `targetId:id` key instead.

test('A -> B -> A while a write is in flight: A\'s PUT must still land once back on A (review round 4, bug 1)', async () => {
  const c = container();
  const d = deferred();
  let tid = 'A';
  const m = mount(c, {
    targetId: () => tid,
    fetchImpl: async (url, opts) => {
      if (opts && opts.method === 'PUT') { await d.promise; return jsonResponse(200, { settings: [{ ...darkMode, value: true }] }); }
      if (String(url).includes('target=B')) return jsonResponse(200, { settings: [{ id: 'darkMode', type: 'bool', supported: true, value: false }] });
      return jsonResponse(200, { settings: [darkMode] }); // A, value: false
    },
  });
  await m.refresh(); // A
  c.querySelectorAll('.settingSwitch')[0].dispatch('change'); // A's write starts (never resolved yet)
  tid = 'B';
  await m.refresh(); // A -> B
  tid = 'A';
  await m.refresh(); // B -> A: back where the write started, before it resolves
  d.resolve(); // A's PUT finally lands
  await tick(2);
  const input = c.querySelectorAll('.settingSwitch')[0];
  assert.equal(input.checked, true, 'A\'s own write must be accepted once we are back on A, not dropped as if it were still stale');
  assert.equal(input.disabled, false, 'the control must be enabled again once the write lands');
});

test('reopening the sheet on the same target mid-write does not re-enable the control (review round 4, bug 2)', async () => {
  const c = container();
  const d = deferred();
  const m = mount(c, {
    targetId: () => 'A',
    fetchImpl: async (url, opts) => {
      if (opts && opts.method === 'PUT') { await d.promise; return jsonResponse(200, { settings: [{ ...darkMode, value: true }] }); }
      return jsonResponse(200, { settings: [darkMode] }); // value: false throughout
    },
  });
  await m.refresh();
  c.querySelectorAll('.settingSwitch')[0].dispatch('change'); // write starts, control disabled
  await m.refresh(); // same target -- reopening the sheet, or a reconnect
  const input = c.querySelectorAll('.settingSwitch')[0];
  assert.equal(input.disabled, true, 'a same-target refresh must not re-enable a control with a write still in flight');
  assert.equal(input.checked, false, 'the GET landed before the write\'s outcome and must not show its value over the write\'s own eventual render');
  d.resolve();
  await tick(2);
  const after = c.querySelectorAll('.settingSwitch')[0];
  assert.equal(after.checked, true, 'the write\'s own response, once it lands, must still win');
  assert.equal(after.disabled, false);
});

test('a refresh GET racing an in-flight write does not send a second PUT for the same control (review round 4, bug 2)', async () => {
  const c = container();
  const d = deferred();
  let puts = 0;
  const m = mount(c, {
    targetId: () => 'A',
    fetchImpl: async (url, opts) => {
      if (opts && opts.method === 'PUT') { puts++; await d.promise; return jsonResponse(200, { settings: [{ ...darkMode, value: true }] }); }
      return jsonResponse(200, { settings: [darkMode] });
    },
  });
  await m.refresh();
  const input = c.querySelectorAll('.settingSwitch')[0];
  input.checked = true;
  input.dispatch('change'); // one PUT starts
  await m.refresh(); // re-render happens here; if it re-enabled the control a second click could fire
  // Simulate the second click the bug would have allowed: the control from
  // the refreshed render is the one a real pointer would find and click.
  const afterRefresh = c.querySelectorAll('.settingSwitch')[0];
  if (!afterRefresh.disabled) { afterRefresh.checked = true; afterRefresh.dispatch('change'); }
  d.resolve();
  await tick(2);
  assert.equal(puts, 1, 'a refresh mid-write must not leave the control clickable again, which would send a second concurrent PUT');
});

// --- review round 5 ----------------------------------------------------
//
// One bug left in 69b2292: the `catch` branch called the closure-captured
// `revert()` instead of going through `render()` the way every other error
// status does. If a refresh() GET resolved while the PUT was still in
// flight, that GET's render() had already replaced every row -- so
// `revert()` acted on a detached input from the row `render()` just threw
// away, and the live control (the new row) never had its disabled state
// touched. It stayed disabled until the next refresh happened to also
// touch that same id.

test('a network failure on the PUT does not strand the control disabled when a refresh GET already re-rendered it (review round 5, finding 1)', async () => {
  const c = container();
  const d = deferred();
  const m = mount(c, {
    targetId: () => 'A',
    fetchImpl: async (url, opts) => {
      if (opts && opts.method === 'PUT') { await d.promise; throw new Error('network down'); }
      return jsonResponse(200, { settings: [darkMode] }); // value: false throughout
    },
  });
  await m.refresh();
  c.querySelectorAll('.settingSwitch')[0].dispatch('change'); // write starts, disabled, never resolved yet
  await m.refresh(); // a GET lands first and re-renders every row while the PUT is still in flight
  const beforeReject = c.querySelectorAll('.settingSwitch')[0];
  assert.equal(beforeReject.disabled, true, 'still pending -- the refresh must not have re-enabled it');
  d.resolve(); // the PUT now rejects
  await tick(2);
  const after = c.querySelectorAll('.settingSwitch')[0];
  assert.equal(after.disabled, false, 'a failed PUT must leave the live control enabled, not the one render() already discarded');
  assert.equal(after.checked, false, 'and showing the device\'s real value');
});

test('A -> B -> A, then a network failure on A\'s PUT: the control must end enabled and showing A\'s value (review round 5, finding 1)', async () => {
  const c = container();
  const d = deferred();
  let tid = 'A';
  const m = mount(c, {
    targetId: () => tid,
    fetchImpl: async (url, opts) => {
      if (opts && opts.method === 'PUT') { await d.promise; throw new Error('network down'); }
      if (String(url).includes('target=B')) return jsonResponse(200, { settings: [{ id: 'darkMode', type: 'bool', supported: true, value: true }] });
      return jsonResponse(200, { settings: [darkMode] }); // A, value: false
    },
  });
  await m.refresh(); // A
  c.querySelectorAll('.settingSwitch')[0].dispatch('change'); // A's write starts, never resolved yet
  tid = 'B';
  await m.refresh(); // A -> B
  tid = 'A';
  await m.refresh(); // B -> A: back where the write started, before it rejects
  d.resolve(); // A's PUT finally rejects
  await tick(2);
  const input = c.querySelectorAll('.settingSwitch')[0];
  assert.equal(input.disabled, false, 'the failed write must leave the control back on A enabled');
  assert.equal(input.checked, false, 'and showing A\'s own device value, not stuck mid-toggle');
});

// review round 5, finding 2: renderRow's disabled check for the range
// control used the same `pending.has(pendingKey(...))` guard as the
// checkbox from the start, but nothing exercised it directly the way the
// checkbox tests do.
test('renderRow disables the range control while its own write is pending (review round 5, finding 2)', async () => {
  const c = container();
  const d = deferred();
  const m = mount(c, {
    targetId: () => 'A',
    fetchImpl: async (url, opts) => {
      if (opts && opts.method === 'PUT') { await d.promise; return jsonResponse(200, { settings: [{ ...fontScale, value: 1.5 }] }); }
      return jsonResponse(200, { settings: [fontScale] });
    },
  });
  await m.refresh();
  const input = c.querySelectorAll('.settingRange')[0];
  assert.equal(input.disabled, false, 'not pending yet');
  input.value = '1.5';
  input.dispatch('change'); // one write on release; disables synchronously
  const stillMounted = c.querySelectorAll('.settingRange')[0];
  assert.equal(stillMounted.disabled, true, 'renderRow must read pending for the range control, same as the checkbox');
  await m.refresh(); // a same-target refresh mid-write must not re-enable it either
  const afterRefresh = c.querySelectorAll('.settingRange')[0];
  assert.equal(afterRefresh.disabled, true, 'a refresh while the range write is pending must leave it disabled too');
  d.resolve();
  await tick(2);
  const after = c.querySelectorAll('.settingRange')[0];
  assert.equal(after.disabled, false);
  assert.equal(after.value, '1.5');
});

// review round 5, finding 3 (mount-level half): the pure applyResponse test
// above pins the filter directly; this is the same scenario through mount()
// so a break in how write()/refresh() actually call applyResponse -- not
// just in applyResponse itself -- is caught too.
test('A -> B -> A while A\'s write is pending: A\'s row shows A\'s own value, not B\'s (review round 5, finding 3)', async () => {
  const c = container();
  const d = deferred();
  let tid = 'A';
  const m = mount(c, {
    targetId: () => tid,
    fetchImpl: async (url, opts) => {
      if (opts && opts.method === 'PUT') { await d.promise; return jsonResponse(200, { settings: [{ ...darkMode, value: true }] }); }
      if (String(url).includes('target=B')) return jsonResponse(200, { settings: [{ id: 'darkMode', type: 'bool', supported: true, value: true }] }); // B: true
      return jsonResponse(200, { settings: [darkMode] }); // A: false
    },
  });
  await m.refresh(); // A: false
  c.querySelectorAll('.settingSwitch')[0].dispatch('change'); // A's write starts, disabled, never resolved yet
  tid = 'B';
  await m.refresh(); // A -> B (B's own row, unrelated to A's pending write)
  tid = 'A';
  await m.refresh(); // B -> A: A's GET answers "false" again, same as before the write -- this alone cannot
  // distinguish "held at current" from "took incoming", since both are false here. The distinguishing
  // case is pinned directly against applyResponse above, where incoming disagrees with current.
  const input = c.querySelectorAll('.settingSwitch')[0];
  assert.equal(input.checked, false, 'A\'s row while pending must read A\'s own current value');
  assert.equal(input.disabled, true, 'still pending');
  d.resolve();
  await tick(2);
});

// --- review round 6 -----------------------------------------------------
//
// Three call sites pass `pendingIdsFor(...)` into applyResponse: the refresh
// render (W4), the write-success render (W6) and the write-error render
// (W7). Every existing test's incoming value happens to agree with current
// for the id that isn't being written, so dropping the pendingIds argument
// at any one of those three sites still passed. These use two settings, X
// (darkMode, mid-write) and Y (fontScale, whose own response carries a
// disagreeing value for X), so the filter's absence is visible.

test('W4: a refresh GET reporting X=true does not show it while X\'s write is in flight', async () => {
  const c = container();
  const d = deferred();
  let gets = 0;
  const m = mount(c, {
    targetId: () => 'A',
    fetchImpl: async (url, opts) => {
      if (opts && opts.method === 'PUT') { await d.promise; return jsonResponse(200, { settings: [{ ...darkMode, value: true }, fontScale] }); }
      gets++;
      // The first GET (before the write starts) reports X's real value,
      // false. The second GET -- the W4 call site, mid-write -- reports
      // X=true, disagreeing with what the pending write is holding.
      return jsonResponse(200, { settings: [{ ...darkMode, value: gets > 1 }, fontScale] });
    },
  });
  await m.refresh(); // first GET: X is false
  c.querySelectorAll('.settingSwitch')[0].dispatch('change'); // X's write starts, disabled
  await m.refresh(); // W4 call site: second GET lands while X is still pending, reports X=true
  const input = c.querySelectorAll('.settingSwitch')[0];
  assert.equal(input.checked, false, 'the refresh GET\'s X=true must not show while X is pending');
  assert.equal(input.disabled, true, 'still pending');
  d.resolve();
  await tick(2);
});

test('W6: Y\'s successful PUT reporting X=true does not show it while X\'s write is in flight', async () => {
  const c = container();
  const dx = deferred();
  const m = mount(c, {
    targetId: () => 'A',
    fetchImpl: async (url, opts) => {
      if (opts && opts.method === 'PUT') {
        const body = JSON.parse(opts.body);
        if ('darkMode' in body.changes) { await dx.promise; return jsonResponse(200, { settings: [{ ...darkMode, value: true }, fontScale] }); }
        // Y's own PUT resolves immediately, and its response body -- the
        // server's full settings snapshot -- also reports X=true.
        return jsonResponse(200, { settings: [{ ...darkMode, value: true }, { ...fontScale, value: 1.5 }] });
      }
      return jsonResponse(200, { settings: [darkMode, fontScale] }); // X: false, Y: 1.0
    },
  });
  await m.refresh();
  c.querySelectorAll('.settingSwitch')[0].dispatch('change'); // X's write starts, never resolved yet
  const yInput = c.querySelectorAll('.settingRange')[0];
  yInput.value = '1.5';
  yInput.dispatch('change'); // Y's write: resolves immediately (W6 call site)
  await tick(2);
  const xInput = c.querySelectorAll('.settingSwitch')[0];
  assert.equal(xInput.checked, false, 'Y\'s success response reporting X=true must not show while X is pending');
  assert.equal(xInput.disabled, true, 'X is still pending');
  dx.resolve();
  await tick(2);
});

test('W7: Y\'s failed PUT (409) reporting X=true does not show it while X\'s write is in flight', async () => {
  const c = container();
  const dx = deferred();
  const m = mount(c, {
    targetId: () => 'A',
    fetchImpl: async (url, opts) => {
      if (opts && opts.method === 'PUT') {
        const body = JSON.parse(opts.body);
        if ('darkMode' in body.changes) { await dx.promise; return jsonResponse(200, { settings: [{ ...darkMode, value: true }, fontScale] }); }
        // Y's PUT fails with a 409 whose re-read settings array also reports
        // X=true (W7 call site).
        return jsonResponse(409, { error: 'not applied', failed: ['fontScale'], settings: [{ ...darkMode, value: true }, fontScale] });
      }
      return jsonResponse(200, { settings: [darkMode, fontScale] }); // X: false, Y: 1.0
    },
  });
  await m.refresh();
  c.querySelectorAll('.settingSwitch')[0].dispatch('change'); // X's write starts, never resolved yet
  const yInput = c.querySelectorAll('.settingRange')[0];
  yInput.value = '1.5';
  yInput.dispatch('change'); // Y's write: 409s immediately (W7 call site)
  await tick(2);
  const xInput = c.querySelectorAll('.settingSwitch')[0];
  assert.equal(xInput.checked, false, 'Y\'s 409 re-read reporting X=true must not show while X is pending');
  assert.equal(xInput.disabled, true, 'X is still pending');
  dx.resolve();
  await tick(2);
});

await Promise.all(pending);
console.log(failures ? `\n${failures} failing` : '\nall passing');
process.exit(failures ? 1 : 0);
