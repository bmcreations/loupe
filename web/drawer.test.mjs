// Tests for the drawer's pure logic: groupTargets and rowModel. Run:
// node web/drawer.test.mjs
//
// Cases live in testdata/client/drawer.json, keyed by function name, the
// same fixture-driven shape health.test.mjs uses. Field names in the fixture
// targets are copied from internal/httpapi/manager.go's TargetInfo and
// internal/target/describe.go's Description, not invented.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const { groupTargets, rowModel, rowHTML, handlePick, pickIntent, refocusTarget, rowActions, swipeDecide, shouldDefer, actionRefusal, swipeStart, readActionResult, agentLine, agentActions, onServer, peerNames, serverFilterOf, serverOptions, readServerFilter, writeServerFilter, serversHTML, addGroupHTML } = await import('./drawer.js');

const fixtures = JSON.parse(readFileSync(new URL('../testdata/client/drawer.json', import.meta.url)));

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

for (const c of fixtures.rowModel) {
  test(`rowModel: ${c.name}`, () => {
    const got = rowModel(...c.args);
    assert.deepEqual(got, c.expect);
  });
}

for (const c of fixtures.groupTargets) {
  test(`groupTargets: ${c.name}`, () => {
    const got = groupTargets(...c.args);
    assert.deepEqual(got, c.expect);
  });
}

// rowHTML: a hostile label/id/version must not reach the DOM unescaped.
// This is what kills mutations that drop or weaken escapeHTML -- surviving
// mutations 12 and 13 from an earlier mutation pass were exactly this: no
// test exercised rowHTML with attacker-controlled strings.
test('rowHTML escapes a hostile label, id and version', () => {
  const html = rowHTML({
    id: '"><img src=x onerror=alert(1)>',
    label: '<script>alert(1)</script>',
    icon: 'phone',
    tile: '"><b>t</b>',
    detail: '"><svg onload=alert(1)>',
    tone: '"><i>x</i>',
    disabled: false,
  });
  for (const raw of ['<script>alert(1)</script>', '"><img src=x onerror=alert(1)>', '"><svg onload=alert(1)>', '"><b>t</b>', '"><i>x</i>']) {
    assert.ok(!html.includes(raw), `rowHTML output contains unescaped input: ${raw}\n${html}`);
  }
  assert.ok(html.includes('&lt;script&gt;'), 'label was not escaped');
  assert.ok(html.includes('&quot;&gt;&lt;img'), 'id was not escaped');
});

// handlePick: factored out of mount() so this needs no DOM at all. Pins both
// halves -- the drawer closes, and the pick is still reported -- and their
// order, since onPick drives a reconnect and must not run against a drawer
// that still thinks it is open.
test('handlePick closes the drawer and then reports the pick', () => {
  const calls = [];
  handlePick('target-1', {
    close: () => calls.push('close'),
    onPick: (id) => calls.push(`pick:${id}`),
  });
  assert.deepEqual(calls, ['close', 'pick:target-1']);
});

// The streaming row is tappable now (it has a check, not a disabled look),
// and picking it again would reconnect for nothing.
test('handlePick on the streaming row only closes', () => {
  const calls = [];
  handlePick('target-1', {
    close: () => calls.push('close'),
    onPick: (id) => calls.push(`pick:${id}`),
    currentId: 'target-1',
  });
  assert.deepEqual(calls, ['close']);
});

test('handlePick still closes when there is no onPick to call', () => {
  let closed = false;
  handlePick('target-1', { close: () => { closed = true; } });
  assert.equal(closed, true);
});

// refocusTarget: the pure decision behind refresh()'s "put focus back on
// the same row if it survived" step.
test('refocusTarget keeps the same row when its id survives the refresh', () => {
  assert.equal(refocusTarget('target-1', ['target-0', 'target-1', 'target-2']), 'target-1');
});

test('refocusTarget returns null when the previously-focused row is gone', () => {
  assert.equal(refocusTarget('target-1', ['target-0', 'target-2']), null);
});

test('refocusTarget returns null when nothing on a row was focused', () => {
  assert.equal(refocusTarget(null, ['target-0', 'target-1']), null);
});

test('refocusTarget returns null against an empty list', () => {
  assert.equal(refocusTarget('target-1', []), null);
});

// Swipe actions. rowActions is the same rule as devicelife.go's deviceActions.
const base = { kind: 'emulator', label: 'x' };
for (const [name, t, want] of [
  ['a running avd offers both', { ...base, id: 'avd:a' }, ['shutdown', 'delete']],
  ['a shut-down avd offers delete only', { ...base, id: 'avd:a', bootsOnOpen: true }, ['delete']],
  ['a running simulator offers both', { ...base, id: 'sim:U' }, ['shutdown', 'delete']],
  ['a pending simulator offers nothing', { ...base, id: 'sim-new:n' }, []],
  ['a creating avd offers nothing', { ...base, id: 'avd:a', state: 'creating' }, []],
  ['an unavailable avd offers nothing', { ...base, id: 'avd:a', available: false }, []],
  ['an adb serial offers nothing', { ...base, id: 'emulator-5554' }, []],
  ['synthetic offers nothing', { ...base, id: 'synthetic', kind: 'synthetic' }, []],
  ['a physical iPhone offers nothing', { ...base, id: 'ios:U' }, []],
]) {
  test(`rowActions: ${name}`, () => assert.deepEqual(rowActions(t), want));
}

for (const [name, args, want] of [
  ['a small move stays unlocked', { dx: -4, dy: 1 }, { locked: null, offset: 0, open: false, swallowClick: false }],
  ['a horizontal drag locks x and swallows the click', { dx: -30, dy: 4 }, { locked: 'x', offset: -30, open: false, swallowClick: true }],
  ['a vertical drag locks y', { dx: -5, dy: 20 }, { locked: 'y', offset: 0, open: false, swallowClick: false }],
  ['a long drag clamps and opens', { dx: -200, dy: 0, locked: 'x' }, { locked: 'x', offset: -152, open: true, swallowClick: true }],
  ['a short drag does not open', { dx: -60, dy: 0, locked: 'x' }, { locked: 'x', offset: -60, open: false, swallowClick: true }],
  ['dragging right from open stays open', { dx: 40, dy: 0, locked: 'x', wasOpen: true }, { locked: 'x', offset: -112, open: true, swallowClick: true }],
  ['dragging far right from open closes', { dx: 100, dy: 0, locked: 'x', wasOpen: true }, { locked: 'x', offset: -52, open: false, swallowClick: true }],
  ['a long first move from unlocked clamps to the open width', { dx: -300, dy: 0 }, { locked: 'x', offset: -152, open: true, swallowClick: true }],
  ['a locked y stays y whatever dx does', { dx: -90, dy: 0, locked: 'y' }, { locked: 'y', offset: 0, open: false, swallowClick: false }],
]) {
  test(`swipeDecide: ${name}`, () => assert.deepEqual(swipeDecide({ openWidth: 152, ...args }), want));
}

test('rowHTML wraps a row with actions and lists exactly those buttons', () => {
  const html = rowHTML({ id: 'avd:a', label: 'a', icon: 'phone', tile: 'android', detail: '', tone: '', actions: ['delete'] });
  assert.ok(html.includes('class="pickerSwipe"'));
  assert.equal((html.match(/data-act="/g) || []).length, 1);
  assert.ok(html.includes('data-act="delete"') && !html.includes('data-act="shutdown"'));
  const both = rowHTML({ id: 'avd:a', label: 'a', icon: 'phone', tile: 'android', actions: ['shutdown', 'delete'] });
  assert.equal((both.match(/data-act="/g) || []).length, 2);
  assert.ok(both.includes('aria-hidden="true"') && both.includes('tabindex="-1"'));
});

test('rowHTML leaves a row with no actions bare', () => {
  const html = rowHTML({ id: 'R5', label: 'a', icon: 'phone', tile: 'android', actions: [] });
  assert.ok(!html.includes('pickerSwipe') && !html.includes('data-act'));
});

test('shouldDefer holds a refresh under a finger or an open row', () => {
  assert.equal(shouldDefer({ pointerDown: true }), true);
  assert.equal(shouldDefer({ openId: 'avd:a' }), true);
  assert.equal(shouldDefer({}), false);
});

for (const [name, status, body, want] of [
  ['recording', 409, { code: 'recording', error: 'recording in progress' }, 'Stop the recording first'],
  ['busy uses the server message', 409, { code: 'busy', error: 'Pixel_10 is already shutting down' }, 'Pixel_10 is already shutting down'],
  ['humanHeld uses the server message', 409, { code: 'humanHeld', error: 'someone is driving', retryAfterMs: 3000 }, 'someone is driving'],
  ['notSupported uses the server message', 422, { code: 'notSupported', error: 'cannot shut this down' }, 'cannot shut this down'],
  ['no body falls back to the status', 500, null, 'Could not complete that (500)'],
]) {
  test(`actionRefusal: ${name}`, () => assert.equal(actionRefusal(status, body), want));
}

test('shouldDefer holds while an action is busy', () => {
  assert.equal(shouldDefer({ busy: 1 }), true);
  assert.equal(shouldDefer({ busy: 0 }), false);
});

for (const [name, args, want] of [
  ['busy ignores everything', { openId: 'a', swipeId: 'a', busy: 1 }, 'ignore'],
  ['busy ignores a touch outside the open row, so it is not closed', { openId: 'a', swipeId: 'b', busy: 1 }, 'ignore'],
  ['a touch outside the open row closes it', { openId: 'a', swipeId: 'b' }, 'closeOpen'],
  ['a touch outside any row closes the open one', { openId: 'a', swipeId: null }, 'closeOpen'],
  ['a touch on an action is left to the button', { openId: 'a', swipeId: 'a', onAction: true }, 'ignore'],
  ['a touch on the open row drags', { openId: 'a', swipeId: 'a' }, 'drag'],
  ['a touch on a closed row drags', { swipeId: 'b' }, 'drag'],
  ['a touch on a bare row ignores', { swipeId: null }, 'ignore'],
]) {
  test(`swipeStart: ${name}`, () => assert.equal(swipeStart(args), want));
}

const resp = (status, body) => ({
  ok: status >= 200 && status < 300,
  status,
  json: async () => { if (body === undefined) throw new Error('parsed a body'); return body; },
});
test('readActionResult: 204 is ok and its body is never parsed', async () => {
  assert.deepEqual(await readActionResult(resp(204)), { ok: true, outcome: 'ok', message: '' });
});
test('readActionResult: 200 shutdown is ok without parsing', async () => {
  assert.equal((await readActionResult(resp(200))).ok, true);
});
test('readActionResult: a refusal carries its code and message', async () => {
  assert.deepEqual(await readActionResult(resp(409, { code: 'busy', error: 'already shutting down' })),
    { ok: false, outcome: 'busy', message: 'already shutting down' });
});
test('readActionResult: a refusal with no body falls back to the status', async () => {
  assert.deepEqual(await readActionResult(resp(502)),
    { ok: false, outcome: '502', message: 'Could not complete that (502)' });
});

// Agents. agentLine is the row's second status line, agentActions the swipe
// actions about agents. Field names are TargetInfo's claim, handsOff and
// drivenBy in internal/httpapi/manager.go.
const claim = { target: 'avd:a', agent: 'loupe: xctest touch', session: 'local_1', since: '2026-10-17T13:00:00Z', idleMs: 0 };
test('agentLine: nothing to say leaves the line empty', () => {
  assert.deepEqual(agentLine({ id: 'avd:a' }), { agent: '', agentTone: '', agentLink: '' });
});
test('agentLine: an agent driving with no claim', () => {
  assert.deepEqual(agentLine({ id: 'avd:a', drivenBy: 'claude' }), { agent: 'claude is driving', agentTone: 'driving', agentLink: '' });
});
test('agentLine: a claim at rest names its holder', () => {
  assert.equal(agentLine({ id: 'avd:a', claim }).agent, 'Claimed by loupe: xctest touch');
});
test('agentLine: a claim whose holder is driving says driving', () => {
  assert.equal(agentLine({ id: 'avd:a', claim, drivenBy: 'loupe: xctest touch' }).agentTone, 'driving');
});
test('agentLine: a claim with only a session names the session', () => {
  assert.equal(agentLine({ id: 'avd:a', claim: { ...claim, agent: '' } }).agent, 'Claimed by local_1');
});
test('agentLine: an https link is kept, anything else dropped', () => {
  assert.equal(agentLine({ id: 'avd:a', claim: { ...claim, link: 'https://claude.ai/x' } }).agentLink, 'https://claude.ai/x');
  assert.equal(agentLine({ id: 'avd:a', claim: { ...claim, link: 'javascript:alert(1)' } }).agentLink, '');
});
test('agentLine: hands off outranks a claim', () => {
  assert.deepEqual(agentLine({ id: 'avd:a', claim, handsOff: true }),
    { agent: 'Hands off · agents are refused', agentTone: 'handsoff', agentLink: '' });
});
test('agentActions: any listed target can be marked hands off', () => {
  assert.deepEqual(agentActions({ id: 'R5' }), ['handsoff']);
});
test('agentActions: a hands-off row offers Allow Agents', () => {
  assert.deepEqual(agentActions({ id: 'R5', handsOff: true }), ['allow']);
});
test('agentActions: a claimed row offers Release first', () => {
  assert.deepEqual(agentActions({ id: 'R5', claim }), ['release', 'handsoff']);
});
test('agentActions: creation rows offer nothing', () => {
  assert.deepEqual(agentActions({ id: 'sim-new:x' }), []);
  assert.deepEqual(agentActions({ id: 'avd:x', state: 'creating' }), []);
  assert.deepEqual(agentActions({ id: 'avd:x', state: 'createFailed' }), []);
});
test('rowHTML draws the agent line, and the link outside the row button', () => {
  const html = rowHTML({ id: 'avd:a', label: 'a', icon: 'phone', tile: 'android', actions: ['handsoff'],
    agent: 'Claimed by <x>', agentTone: 'claimed', agentLink: 'https://claude.ai/s?a=1&b=2' });
  assert.match(html, /pickerRowAgent--claimed">Claimed by &lt;x&gt;</);
  assert.match(html, /<\/div>\s*<a class="pickerAgentLink" href="https:\/\/claude.ai\/s\?a=1&amp;b=2"/);
  assert.match(html, /data-act="handsoff"[^>]*>Hands Off</);
});

// Open panes: with openIds the drawer badges each open row with its pane's
// 1-based index and offers Open beside on a streamable row that is not open.
const paneList = [
  { id: 'avd:a', label: 'A', kind: 'emulator', os: 'android', osVersion: '15' },
  { id: 'sim:b', label: 'B', kind: 'simulator', os: 'ios', osVersion: '18' },
  { id: 'avd:c', label: 'C', kind: 'emulator', os: 'android', available: false, state: 'offline' },
];
test('groupTargets with no openIds adds no badge or beside', () => {
  for (const g of groupTargets(paneList, 'avd:a', '', 0)) for (const r of g.rows) {
    assert.equal('badge' in r, false);
    assert.equal('beside' in r, false);
  }
});
test('groupTargets badges open rows by pane index and offers beside on the rest', () => {
  const rows = groupTargets(paneList, 'sim:b', '', 0, ['avd:a', 'sim:b']).flatMap((g) => g.rows);
  const by = Object.fromEntries(rows.map((r) => [r.id, r]));
  assert.equal(by['avd:a'].badge, 1);
  assert.equal(by['sim:b'].badge, 2);
  assert.equal('beside' in by['avd:a'], false);
  assert.equal('badge' in by['avd:c'], false);
  assert.equal('beside' in by['avd:c'], false, 'a row that cannot open is not offered');
});
test('groupTargets offers beside on a closed streamable row', () => {
  const rows = groupTargets(paneList, 'avd:a', '', 0, ['avd:a']).flatMap((g) => g.rows);
  assert.equal(rows.find((r) => r.id === 'sim:b').beside, true);
});
test('groupTargets puts every open row under Running, in pane order', () => {
  const g = groupTargets(paneList, 'avd:a', '', 0, ['sim:b', 'avd:a']);
  assert.deepEqual(g[0].rows.map((r) => r.id), ['sim:b', 'avd:a']);
});
test('rowHTML draws the badge and an Open beside button outside the row button', () => {
  const html = rowHTML({ id: 'sim:b', label: 'B', icon: 'phone', tile: 'ios', beside: true, badge: 3 });
  assert.match(html, /pickerBadge[^>]*>⌃3</);
  assert.match(rowHTML({ id: 'x', label: 'X', icon: 'phone', badge: 2 }, { paneKey: 'Alt+' }), /pickerBadge[^>]*>Alt\+2</);
  assert.match(html, /<\/button>\s*<button type="button" class="pickerBeside" data-beside="sim:b"[^>]*>Open beside</);
});
test('rowHTML draws no Open beside without the offer', () => {
  assert.doesNotMatch(rowHTML({ id: 'x', label: 'x', icon: 'phone', tile: 'ios' }), /pickerBeside|pickerBadge/);
});
test('pickIntent: a cmd or ctrl click on an offered row opens beside', () => {
  assert.equal(pickIntent({ metaKey: true }, true), 'beside');
  assert.equal(pickIntent({ ctrlKey: true }, true), 'beside');
  assert.equal(pickIntent({}, true), 'pick');
  assert.equal(pickIntent({ metaKey: true }, false), 'pick', 'a row not offered just picks');
});

// ---- Peer rows and the server filter ----
const peerList = [
  { id: 'sim:own', kind: 'simulator', label: 'iPhone 17', available: true },
  { id: 'avd:own', kind: 'emulator', label: 'Pixel 9', available: true },
  { id: 'mini/sim:abc', server: 'mini', kind: 'simulator', label: 'iPhone 17', available: true },
  { id: 'lab/avd:p', server: 'lab', kind: 'emulator', label: 'Pixel 9', available: false, reason: 'serverDown' },
];
test('onServer: All passes everything, This server only its own, a name only that peer', () => {
  assert.deepEqual(peerList.filter((t) => onServer(t, 'all')).length, 4);
  assert.deepEqual(peerList.filter((t) => onServer(t, undefined)).length, 4);
  assert.deepEqual(peerList.filter((t) => onServer(t, 'local')).map((t) => t.id), ['sim:own', 'avd:own']);
  assert.deepEqual(peerList.filter((t) => onServer(t, 'mini')).map((t) => t.id), ['mini/sim:abc']);
});
test('groupTargets filters by server and keeps peers in their platform group', () => {
  const ids = (f) => groupTargets(peerList, null, '', 0, undefined, f).flatMap((g) => g.rows.map((r) => r.id));
  assert.deepEqual(ids('lab'), ['lab/avd:p']);
  assert.deepEqual(ids('local').sort(), ['avd:own', 'sim:own']);
  assert.equal(ids('all').length, 4);
  const g = groupTargets(peerList, null, '', 0, undefined, 'mini');
  assert.deepEqual(g.map((x) => x.title), ['Running']);
});
test('groupTargets: a search matches a peer by server name', () => {
  const ids = groupTargets(peerList, null, 'mini', 0).flatMap((g) => g.rows.map((r) => r.id));
  assert.deepEqual(ids, ['mini/sim:abc']);
});
test('peerNames and serverOptions list peers only when one exists', () => {
  assert.deepEqual(peerNames(peerList), ['mini', 'lab']);
  assert.deepEqual(serverOptions(peerList.slice(0, 2)), []);
  assert.deepEqual(serverOptions(peerList).map((o) => o.label), ['All', 'This server', 'mini', 'lab']);
  assert.deepEqual(serverOptions(peerList).map((o) => o.value), ['all', 'local', 'mini', 'lab']);
});
test('serverFilterOf falls back to All for a vanished peer or no peers, keeps valid choices', () => {
  assert.equal(serverFilterOf('mini', peerList), 'mini');
  assert.equal(serverFilterOf('local', peerList), 'local');
  assert.equal(serverFilterOf('gone', peerList), 'all');
  assert.equal(serverFilterOf('mini', peerList.slice(0, 2)), 'all');
  assert.equal(serverFilterOf(null, peerList), 'all');
});
test('the saved filter round-trips and survives a storage that throws', () => {
  const mem = new Map();
  const storage = { getItem: (k) => (mem.has(k) ? mem.get(k) : null), setItem: (k, v) => mem.set(k, v) };
  assert.equal(readServerFilter(storage), 'all');
  writeServerFilter(storage, 'mini');
  assert.equal(readServerFilter(storage), 'mini');
  const broken = { getItem() { throw new Error('denied'); }, setItem() { throw new Error('denied'); } };
  assert.equal(readServerFilter(broken), 'all');
  writeServerFilter(broken, 'mini');
});
test('rowModel: a peer row carries its server, and a local one has no server key', () => {
  assert.equal(rowModel(peerList[2], null, 0).server, 'mini');
  assert.equal('server' in rowModel(peerList[0], null, 0), false);
});
test('rowModel: serverDown reads as the server being unreachable', () => {
  const m = rowModel(peerList[3], null, 0);
  assert.equal(m.disabled, true);
  assert.equal(m.tone, 'warn');
  assert.match(m.detail, /lab is unreachable/);
});
test('rowHTML shows the server name beside a peer label, escaped', () => {
  const html = rowHTML(rowModel({ ...peerList[2], server: '<b>x' }, null, 0));
  assert.match(html, /pickerRowServer">&lt;b&gt;x</);
  assert.doesNotMatch(rowHTML(rowModel(peerList[0], null, 0)), /pickerRowServer/);
});
test('serversHTML marks the active choice and draws nothing without options', () => {
  assert.equal(serversHTML([], 'all'), '');
  const html = serversHTML(serverOptions(peerList), 'mini');
  assert.match(html, /data-server="mini"[^>]*aria-checked="true"/);
  assert.match(html, /data-server="all"[^>]*aria-checked="false"/);
});
test('addGroupHTML offers Add server alone, both rows, or nothing', () => {
  assert.equal(addGroupHTML({}), '');
  assert.match(addGroupHTML({ server: true }), /data-action="server"/);
  assert.doesNotMatch(addGroupHTML({ server: true }), /data-action="new"/);
  assert.match(addGroupHTML({ newDevice: true, server: true }), /data-action="new"[\s\S]*data-action="server"/);
});

await Promise.all(pending);
console.log(failures ? `\n${failures} failing` : '\nall passing');
process.exit(failures ? 1 : 0);
