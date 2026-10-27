// Tests for the event log: decode against the golden protocol hex, EventLog's
// gap/merge/dedupe rules, formatRow, and mount()'s DOM wiring. Run:
// node web/eventlog.test.mjs
//
// decodeEvent is asserted against testdata/protocol/*.hex directly -- the
// same fixtures internal/session's own golden test decodes -- rather than
// hand-copied bytes, so the two cannot silently disagree (see CLAUDE.md's
// instruction on this task). formatRow is fixture-driven from
// testdata/client/eventlog.json, the pattern drawer.test.mjs and
// health.test.mjs use. EventLog itself is a small stateful class, so its
// rules are pinned with hand-written sequences.
//
// mount() is the DOM-touching part app.js used to own inline (untestable --
// trap 21, app.js has no harness). It is unit-tested here with a minimal,
// hand-rolled fake DOM -- a few objects implementing just the
// createElement/appendChild/innerHTML/textContent surface mount() actually
// uses -- built fresh for this file rather than imported from anywhere else
// (Task 9, branch feat/web-tools-panels, is building its own fakedom.js for
// a different module; the two get reconciled at merge, not shared now).
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

import { decodeEvent, formatRow, EventLog, mount } from './eventlog.js';

const fixtures = JSON.parse(
  readFileSync(new URL('../testdata/client/eventlog.json', import.meta.url)),
);

// test() queues rather than runs: several tests here are async (they drive
// mount()'s fetch races with deferred()), and a synchronous "fn(); log PASS"
// runner does not wait for those -- it logs PASS immediately, the real
// assertions run later as an unawaited microtask, and a late failure becomes
// an unhandled rejection that this file's own process.exit(0) can race and
// swallow entirely. runAll() below awaits every test in order instead, sync
// or async alike, so a rejected assertion is what actually fails the run.
let failures = 0;
const queued = [];
function test(name, fn) {
  queued.push({ name, fn });
}
async function runAll() {
  for (const { name, fn } of queued) {
    try { await fn(); console.log(`  PASS ${name}`); }
    catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
  }
}

// readHexFixture parses testdata/protocol/*.hex: comment lines start with
// '#', and the remaining non-blank lines are hex, concatenated in order.
// The first byte is the message type; the rest is the body decodeEvent
// takes (handleControl in app.js strips the type byte the same way before
// calling any decoder).
function readHexFixture(name) {
  const text = readFileSync(new URL(`../testdata/protocol/${name}`, import.meta.url), 'utf8');
  const hex = text
    .split('\n')
    .map((l) => l.trim())
    .filter((l) => l && !l.startsWith('#'))
    .join('');
  const bytes = new Uint8Array(hex.length / 2);
  for (let i = 0; i < bytes.length; i++) bytes[i] = parseInt(hex.substr(i * 2, 2), 16);
  return { type: bytes[0], body: bytes.subarray(1) };
}

test('decodeEvent: plain tap (event.hex)', () => {
  const { type, body } = readHexFixture('event.hex');
  assert.equal(type, 0x06);
  assert.deepEqual(decodeEvent(body), {
    seq: 1, t: 1727177400000, source: 'viewer', viewer: 2, kind: 'tap', from: [0.512, 0.734],
  });
});

test('decodeEvent: every optional field at once (event-full.hex)', () => {
  const { body } = readHexFixture('event-full.hex');
  assert.deepEqual(decodeEvent(body), {
    seq: 4, t: 1727177401500, source: 'agent', kind: 'swipe',
    to: [0.9, 0.8], ms: 250, pointers: 2, scale: 1.5, rotation: 0,
    scroll: [-1.5, 0.25], err: 'context deadline exceeded',
  });
});

test('decodeEvent: rotation 0 decodes present, not omitted', () => {
  const { body } = readHexFixture('event-full.hex');
  const ev = decodeEvent(body);
  assert.equal('rotation' in ev, true);
  assert.equal(ev.rotation, 0);
});

test('decodeEvent: redacted text (event-redacted.hex)', () => {
  const { body } = readHexFixture('event-redacted.hex');
  assert.deepEqual(decodeEvent(body), {
    seq: 2, t: 1727177400500, source: 'agent', kind: 'text',
    text: '•••', redacted: true, textLen: 8,
  });
});

test('decodeEvent: button (event-button.hex)', () => {
  const { body } = readHexFixture('event-button.hex');
  assert.deepEqual(decodeEvent(body), {
    seq: 3, t: 1727177401000, source: 'viewer', viewer: 2, kind: 'button', button: 'home',
  });
});

for (const c of fixtures.formatRow) {
  test(`formatRow: ${c.name}`, () => {
    const got = formatRow(...c.args);
    assert.deepEqual(got, c.expect);
  });
}

// --- EventLog -------------------------------------------------------------

function tap(seq) {
  return { seq, t: 1727177400000 + seq, source: 'viewer', viewer: 1, kind: 'tap', from: [0.1, 0.1] };
}

test('EventLog.add: consecutive events report no gap', () => {
  const log = new EventLog();
  assert.equal(log.add(tap(1)), null);
  assert.equal(log.add(tap(2)), null);
  assert.deepEqual(log.rows().map((e) => e.seq), [2, 1]);
});

test('EventLog.add: a single-seq gap is detected (E25)', () => {
  // The narrowest possible gap -- exactly one seq missing. A wider gap
  // (1 -> 5, tested below) does not distinguish `> lastSeq + 1` from the
  // off-by-one `> lastSeq + 2`, which is exactly why this survived
  // unnoticed in round 1: every existing gap test skipped at least two
  // seqs.
  const log = new EventLog();
  log.add(tap(1));
  const gap = log.add(tap(3));
  assert.deepEqual(gap, { after: 1, before: 3 });
});

test('EventLog.add: a skipped seq reports exactly one gap', () => {
  const log = new EventLog();
  log.add(tap(1));
  const result = log.add(tap(5));
  assert.deepEqual(result, { after: 1, before: 5 });
  const rows = log.rows();
  // newest first: the new event, then the gap marker, then the first tap.
  assert.equal(rows.length, 3);
  assert.equal(rows[0].seq, 5);
  assert.deepEqual(rows[1], { gap: true, after: 1, before: 5, lost: false });
  assert.equal(rows[2].seq, 1);
});

test('EventLog.add: re-adding the same seq does not duplicate the gap', () => {
  const log = new EventLog();
  log.add(tap(1));
  log.add(tap(5));
  const again = log.add(tap(5));
  assert.equal(again, null); // seen already -- not a fresh gap, not a fresh event
  assert.equal(log.rows().filter((r) => r.gap).length, 1);
});

test('EventLog: a gap reported by add() and rediscovered by a merge is not recorded twice (E1)', () => {
  const log = new EventLog();
  // add() records the gap; merge() replays a page that still has not
  // filled it (events only from 5 on). Both see the same unfilled range,
  // which exercises _pushGap's own idempotency guard rather than add()'s
  // seen-event check -- a poller re-running the same /api/events request
  // must not be what grows the log.
  log.add(tap(1));
  log.add(tap(5)); // gap 1->5 recorded by add()
  log.merge({ session: 's1', oldest: 1, latest: 5, events: [tap(5)] });
  log.merge({ session: 's1', oldest: 1, latest: 5, events: [tap(5)] });
  const gaps = log.rows().filter((r) => r.gap);
  assert.equal(gaps.length, 1);
});

test('EventLog._pushGap: the same (epoch, after, before) is recorded only once (E1 direct)', () => {
  // The E1 test above (add() then two merges replaying the same trailing
  // event) never actually re-invokes _pushGap for the same range: merge()'s
  // runner already advances past the gap on the first call (through the
  // lost-rollover branch or the per-event loop), so a second identical page
  // recomputes nothing and the loop-level guards (`ev.seq > runner + 1`,
  // `page.oldest > runner + 1`) are what keep it from calling _pushGap
  // again -- not the idempotency guard inside _pushGap itself. That guard
  // only matters when two callers independently compute the same range, so
  // it is pinned directly here, the same way other tests in this file poke
  // private state (log.epoch, log.lastSeq, log.entries) rather than forcing
  // a contrived call path.
  const log = new EventLog();
  log._pushGap(0, 1, 5, false);
  log._pushGap(0, 1, 5, false); // identical triple, called again
  const gaps = log.rows().filter((r) => r.gap);
  assert.equal(gaps.length, 1);
  assert.deepEqual(gaps[0], { gap: true, after: 1, before: 5, lost: false });
});

test('EventLog.merge: fills a gap and removes the marker', () => {
  const log = new EventLog();
  log.add(tap(1));
  log.add(tap(5)); // gap 1->5 recorded
  log.merge({ session: 's1', oldest: 1, latest: 5, events: [tap(2), tap(3), tap(4)] });
  const rows = log.rows();
  assert.deepEqual(rows.map((e) => e.gap ? 'gap' : e.seq), [5, 4, 3, 2, 1]);
});

test('EventLog.merge: drops events already seen (dedupe)', () => {
  const log = new EventLog();
  log.add(tap(1));
  log.add(tap(2));
  log.merge({ session: 's1', oldest: 1, latest: 2, events: [tap(1), tap(2)] });
  assert.equal(log.rows().length, 2);
});

test('EventLog.merge: keeps events in seq order regardless of arrival order', () => {
  const log = new EventLog();
  log.merge({ session: 's1', oldest: 1, latest: 3, events: [tap(1), tap(2), tap(3)] });
  assert.deepEqual(log.rows().map((e) => e.seq), [3, 2, 1]);
});

test('EventLog.merge: advances lastSeq on its own, not only via add() (E10)', () => {
  const log = new EventLog();
  log.merge({ session: 's1', oldest: 1, latest: 3, events: [tap(1), tap(2), tap(3)] });
  assert.equal(log.lastSeq, 3);
});

test('EventLog.add: lastSeq is the max seen, not the last call\'s seq (E16)', () => {
  const log = new EventLog();
  log.add(tap(5));
  log.add(tap(3)); // an out-of-order arrival must not roll lastSeq backwards
  assert.equal(log.lastSeq, 5);
});

test('EventLog: default limit is 200 (E24)', () => {
  const log = new EventLog();
  for (let s = 1; s <= 201; s++) log.add(tap(s));
  assert.equal(log.rows().length, 200);
  assert.equal(log.rows()[log.rows().length - 1].seq, 2); // seq 1 trimmed off
});

test('noteInit (same target) resets lastSeq so the catch-up fetch asks for everything, not "after 300"', () => {
  const log = new EventLog();
  for (let s = 1; s <= 300; s++) log.add(tap(s));
  assert.equal(log.lastSeq, 300);
  log.noteInit();
  assert.equal(log.lastSeq, 0);
});

test('a same-session reconnect keeps entries rather than clearing them (E26 direction)', () => {
  // A benign resume: the log already has a live event the /api/events
  // catch-up page does not repeat (still within its own window but not
  // part of this particular page, e.g. it arrived on the live socket
  // moments before the reconnect). If noteInit() cleared the log outright
  // -- the simpler alternative design -- this event would vanish even
  // though nothing about the session changed and it was never lost.
  const log = new EventLog();
  log.merge({ session: 's1', oldest: 1, latest: 2, events: [tap(1), tap(2)] });
  log.add(tap(3)); // a live event the next catch-up page below will not repeat
  log.noteInit();
  log.merge({ session: 's1', oldest: 1, latest: 2, events: [tap(1), tap(2)] });
  assert.deepEqual(log.rows().map((e) => e.seq), [3, 2, 1]);
});

test('two noteInit+merge rounds on the same session do not duplicate rows (item 1 / race.mjs #1)', () => {
  const log = new EventLog();
  log.noteInit();
  log.merge({ session: 'S', oldest: 1, latest: 3, events: [tap(1), tap(2), tap(3)] });
  assert.equal(log.rows().length, 3);
  log.noteInit();
  log.merge({ session: 'S', oldest: 1, latest: 3, events: [tap(1), tap(2), tap(3)] });
  assert.equal(log.rows().length, 3);
  assert.equal(log.lastSeq, 3);

  // A third round, this time with the page reporting a couple of new
  // events too -- the ordinary shape of "reconnect, then catch up on what
  // was missed" -- must add exactly the new ones.
  log.noteInit();
  log.merge({ session: 'S', oldest: 1, latest: 5, events: [tap(1), tap(2), tap(3), tap(4), tap(5)] });
  assert.equal(log.rows().length, 5);
});

test('noteInit with a different target clears the log; the same target does not', () => {
  const log = new EventLog();
  log.noteInit('device-a');
  log.merge({ session: 's1', oldest: 1, latest: 2, events: [tap(1), tap(2)] });
  assert.equal(log.rows().length, 2);

  log.noteInit('device-a'); // same target: an ordinary reconnect
  assert.equal(log.rows().length, 2);
  assert.equal(log.session, 's1');

  log.noteInit('device-b'); // a different target: the log is per-target
  assert.equal(log.rows().length, 0);
  assert.equal(log.lastSeq, 0);
  assert.equal(log.session, null);
});

test('a session change (EventPage.session) starts a new epoch, not a merge into the old one', () => {
  const log = new EventLog();
  log.merge({ session: 'session-a', oldest: 1, latest: 1, events: [tap(1)] });
  // A fresh session restarts seq at 1 too -- session-b's seq 1 is not the
  // same event as session-a's seq 1, and must not be deduped against it.
  log.merge({ session: 'session-b', oldest: 1, latest: 1, events: [tap(1)] });
  assert.equal(log.rows().length, 2);
});

test('ring rollover surfaces exactly one lost gap marker, not a loop of fetches', () => {
  const log = new EventLog();
  // The log last saw seq 1; the ring has since wrapped and now starts at 200.
  log.add(tap(1));
  log.merge({ session: 's1', oldest: 200, latest: 202, events: [tap(200), tap(201), tap(202)] });
  const rows = log.rows();
  const gaps = rows.filter((r) => r.gap);
  assert.equal(gaps.length, 1);
  assert.deepEqual(gaps[0], { gap: true, after: 1, before: 200, lost: true });
  assert.deepEqual(rows.map((e) => e.gap ? 'gap' : e.seq), [202, 201, 200, 'gap', 1]);
});

test('a lost (rollover) gap marker is never removed, even if every intervening seq later arrives (E14)', () => {
  const log = new EventLog();
  log.add(tap(1));
  log.merge({ session: 's1', oldest: 10, latest: 10, events: [tap(10)] }); // rollover: lost gap 1->10
  assert.equal(log.rows().filter((r) => r.gap && r.lost).length, 1);
  // Contrived, but exactly what _removeFilledGaps's `e.lost` check guards:
  // even if seqs 2..9 somehow show up later (a page replaying more of the
  // ring than this log expected), a *lost* marker must stay -- those events
  // were already reported gone, and un-reporting that would be a second lie.
  for (let s = 2; s <= 9; s++) log.add(tap(s));
  assert.equal(log.rows().filter((r) => r.gap && r.lost).length, 1);
});

test('EventLog trims to its limit, oldest first', () => {
  const log = new EventLog(3);
  for (let s = 1; s <= 5; s++) log.add(tap(s));
  assert.deepEqual(log.rows().map((e) => e.seq), [5, 4, 3]);
});

// --- mount() ---------------------------------------------------------------
//
// A minimal fake DOM: only what mount()'s buildRow/render actually touch.
// Not shared with any other module's tests (see the file header).
function fakeEl(tag) {
  return {
    tag,
    className: '',
    textContent: '',
    _html: '',
    children: [],
    get innerHTML() { return this._html; },
    set innerHTML(v) { this._html = v; if (v === '') this.children = []; },
    appendChild(node) { this.children.push(node); return node; },
  };
}

// withFakeDocument installs the fake document for the duration of fn,
// including every microtask fn awaits internally -- restoring it as soon as
// fn() *returns* (rather than once its returned promise settles) would pull
// the fake document out from under an async fn's own continuations, which
// silently broke every async mount() test that called document.createElement
// after an await (masked by test()'s own bug above: nothing was awaiting the
// rejection that produced). fn may be sync or async; either way the finally
// only runs once whatever fn() returned has settled.
async function withFakeDocument(fn) {
  const prev = globalThis.document;
  globalThis.document = { createElement: (tag) => fakeEl(tag) };
  try { return await fn(); } finally { globalThis.document = prev; }
}

// deferred() gives a test control over exactly when a fake fetch resolves,
// which is the only way to reproduce "A's late page lands after B has
// already taken over" without a real network.
function deferred() {
  let resolve, reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

function fakeResponse(body) {
  return { ok: true, json: async () => body };
}

// flushMicrotasks yields the given number of microtask turns. A promise
// chain through an async fetchImpl that itself returns another promise (as
// opposed to a plain value) needs more turns to settle than one does --
// each `return somePromise` inside an async function is its own adoption
// tick -- so tests that build a fetch out of deferred() use this rather
// than guessing an exact count of bare `await Promise.resolve()` calls.
async function flushMicrotasks(n = 6) {
  for (let i = 0; i < n; i++) await Promise.resolve();
}

test('mount: a live event is decoded, added, and rendered (A3/A8 direction)', () => {
  return withFakeDocument(() => {
    const list = fakeEl('div');
    const m = mount(list, { getTarget: () => 't1', isOpen: () => true, fetch: async () => fakeResponse({ session: 's', oldest: 0, latest: 0, events: [] }) });
    const body = new TextEncoder().encode(JSON.stringify(tap(1)));
    m.handleEvent(body);
    assert.equal(m.log.rows().length, 1);
    assert.equal(list.children.length, 1); // rendered, not just recorded
  });
});

test('mount: render() is a no-op while the section is closed, but the log still records (item 4)', () => {
  return withFakeDocument(() => {
    const list = fakeEl('div');
    let open = false;
    const m = mount(list, { getTarget: () => 't1', isOpen: () => open, fetch: async () => fakeResponse({ session: 's', oldest: 0, latest: 0, events: [] }) });
    m.handleEvent(new TextEncoder().encode(JSON.stringify(tap(1))));
    assert.equal(list.children.length, 0); // nothing painted while closed
    assert.equal(m.log.rows().length, 1); // but the event was not lost
    open = true;
    m.render(); // the caller is expected to call this on the open transition
    assert.equal(list.children.length, 1);
  });
});

test('mount: the badge tracks the row count independent of isOpen (item 4)', () => {
  return withFakeDocument(() => {
    const list = fakeEl('div');
    const badge = { textContent: '' };
    const m = mount(list, { getTarget: () => 't1', isOpen: () => false, badgeEl: badge, fetch: async () => fakeResponse({ session: 's', oldest: 0, latest: 0, events: [] }) });
    m.handleEvent(new TextEncoder().encode(JSON.stringify(tap(1))));
    // Skips straight to seq 4: a gap marker lands in rows() alongside the
    // two real events. The badge must count only the events -- a viewer
    // reading "3 events" for two taps and a hole would be misleading.
    m.handleEvent(new TextEncoder().encode(JSON.stringify(tap(4))));
    assert.equal(m.log.rows().filter((r) => r.gap).length, 1); // sanity: a gap really is in rows()
    assert.equal(badge.textContent, '2');
  });
});

test('mount: an unreadable row does not blank the rest of the list (item 3)', () => {
  return withFakeDocument(() => {
    const list = fakeEl('div');
    const m = mount(list, { getTarget: () => 't1', isOpen: () => true, fetch: async () => fakeResponse({ session: 's', oldest: 0, latest: 0, events: [] }) });
    m.log.add(tap(1));
    // A malformed entry: kind formatRow's whatFor has no case-specific
    // guard for. default: falls through cleanly, so force a genuinely
    // throwing shape instead -- an event with a kind that indexes into a
    // field which is simply absent, past what any known case defends.
    m.log.entries.push({ epoch: 0, seq: 2, ev: { t: 1, source: 'agent', kind: 'swipe', get to() { throw new Error('boom'); }, from: [0, 0] } });
    m.render();
    // Two real events plus nothing blank: the throwing row degrades to an
    // error placeholder instead of stopping the loop.
    assert.equal(list.children.length, 2);
  });
});

test('mount: fetchCatchUp is single-flight -- N gaps in flight coalesce into one fetch (item 4)', () => {
  return withFakeDocument(async () => {
    const list = fakeEl('div');
    let fetchCount = 0;
    const gate = deferred();
    const m = mount(list, {
      getTarget: () => 't1',
      isOpen: () => true,
      fetch: async () => { fetchCount++; return gate.promise; },
    });
    // Three gaps while the first fetch is still pending.
    m.handleEvent(new TextEncoder().encode(JSON.stringify(tap(5))));
    m.handleEvent(new TextEncoder().encode(JSON.stringify(tap(9))));
    m.handleEvent(new TextEncoder().encode(JSON.stringify(tap(13))));
    await Promise.resolve(); // let the microtask queue settle the async calls
    assert.equal(fetchCount, 1);
    gate.resolve(fakeResponse({ session: 's', oldest: 1, latest: 13, events: [] }));
  });
});

test('mount: an agent tap/swipe with no coordinates renders without throwing (item 3, via fixture shape)', () => {
  return withFakeDocument(() => {
    const list = fakeEl('div');
    const m = mount(list, { getTarget: () => 't1', isOpen: () => true, fetch: async () => fakeResponse({ session: 's', oldest: 0, latest: 0, events: [] }) });
    const ev = { seq: 1, t: 0, source: 'agent', kind: 'tap', err: 'target: screen size unknown' };
    m.handleEvent(new TextEncoder().encode(JSON.stringify(ev)));
    assert.equal(list.children.length, 1);
    assert.equal(list.children[0].children[1].textContent, 'Tap (position unknown)');
  });
});

// Round 2's single-flight guard was one boolean shared across every
// connection: noteInit() mid-fetch (a switch, or -- P3 below -- a
// same-target reconnect) found fetchInFlight already true, its own
// fetchCatchUp returned immediately, and the old response was then dropped
// by the currency check, so *nothing ever fetched for the new connection at
// all*. Live: switching A->B mid-fetch left B's log permanently empty, with
// only one request ever going out, for target=A (reviewer probe P2b).
// Single-flight is now keyed on (target, generation), so a switch's own
// fetchCatchUp is not blocked by the superseded one still in flight.
test('mount: switching target mid-fetch is not blocked by the stale fetch, and starts its own (P2b)', async () => {
  await withFakeDocument(async () => {
    const list = fakeEl('div');
    let target = 't-a';
    let gen = 1;
    const urls = [];
    const gates = {};
    const m = mount(list, {
      getTarget: () => target,
      getGeneration: () => gen,
      isOpen: () => true,
      fetch: async (url) => { urls.push(url); const g = deferred(); gates[url] = g; return g.promise; },
    });
    m.noteInit(); // t-a's fetch starts, still in flight
    target = 't-b';
    gen = 2;
    m.noteInit(); // switch: must start its own fetch, not be blocked by t-a's
    await flushMicrotasks();
    assert.equal(urls.length, 2);
    assert.match(urls[0], /target=t-a/);
    assert.match(urls[1], /target=t-b/);

    // t-a's late page resolves after the switch and must not populate
    // t-b's (now current) log.
    gates[urls[0]].resolve(fakeResponse({ session: 'SA', oldest: 1, latest: 5, events: [1, 2, 3, 4, 5].map(tap) }));
    await flushMicrotasks();
    assert.equal(m.log.rows().length, 0);

    // t-b's own fetch resolving is what actually populates the log --
    // this is the part round 2 never reached.
    gates[urls[1]].resolve(fakeResponse({ session: 'SB', oldest: 1, latest: 2, events: [tap(1), tap(2)] }));
    await flushMicrotasks();
    assert.equal(m.log.rows().length, 2);
  });
});

// Same bug, same-target shape: a reconnect (not a switch) mid-fetch. app.js
// bumps connGen on every connect(), including a resume, so the generation
// getter changes even though the target does not. Live, this left the log
// empty after any reconnect that happened to land while a previous catch-up
// was still in flight (reviewer probe P3). This also pins M6 -- the
// currency check has to compare generation, not just target, or the second
// (current) fetch's own late-arriving stale sibling would be treated as
// still current.
test('mount: a same-target reconnect mid-fetch starts its own fetch and is not corrupted by the stale one (P3, M6)', async () => {
  await withFakeDocument(async () => {
    const list = fakeEl('div');
    let gen = 1;
    const gates = [];
    const urls = [];
    const m = mount(list, {
      getTarget: () => 't1',
      getGeneration: () => gen,
      isOpen: () => true,
      fetch: async (url) => { urls.push(url); const g = deferred(); gates.push(g); return g.promise; },
    });
    m.noteInit(); // reconnect #1, generation 1, still in flight
    gen = 2;
    m.noteInit(); // reconnect #2 before #1 settled -- same target throughout
    await flushMicrotasks();
    assert.equal(urls.length, 2); // not blocked by #1 still being in flight

    gates[1].resolve(fakeResponse({ session: 'S', oldest: 1, latest: 2, events: [tap(1), tap(2)] }));
    await flushMicrotasks();
    assert.equal(m.log.rows().length, 2);

    // #1's page resolves after #2's -- same target, stale generation. Its
    // payload is deliberately different (a different session, different
    // seqs) so that a wrongful merge is visible rather than masked by
    // dedupe against #2's identical-looking page.
    gates[0].resolve(fakeResponse({ session: 'STALE', oldest: 9, latest: 10, events: [tap(9), tap(10)] }));
    await flushMicrotasks();
    assert.equal(m.log.rows().length, 2); // #1's page must not have landed
    assert.deepEqual(m.log.rows().map((r) => r.seq), [2, 1]);
  });
});

// M7: the currency check has two independent halves (target and
// generation); a mutation that drops the target half alone would pass every
// test above, because every switch in them also changes generation. Hold
// generation constant and switch only the target to isolate it.
test('mount: the currency check catches a target switch even when the generation getter does not change (M7)', async () => {
  await withFakeDocument(async () => {
    const list = fakeEl('div');
    let target = 't-a';
    const gate = deferred();
    const m = mount(list, {
      getTarget: () => target,
      getGeneration: () => 1,
      isOpen: () => true,
      fetch: async () => gate.promise,
    });
    m.noteInit();
    target = 't-b';
    gate.resolve(fakeResponse({ session: 'SA', oldest: 1, latest: 3, events: [tap(1), tap(2), tap(3)] }));
    await flushMicrotasks();
    assert.equal(m.log.rows().length, 0);
  });
});

// The switch above lands before the network call even resolves, so the
// check right after that first await already catches it -- it never
// isolates the *second* guard, immediately before merge(), which is what
// actually stands between a parsed page and the log. This test forces the
// switch into the gap between "the fetch resolved" and "the JSON parsed",
// where only that second guard is what a removed one would let through:
// res.json() is itself async (fakeResponse), so a deferred body gives a
// place to switch target after fetchImpl's own await has already resumed.
test('mount: a stale response is dropped even if the switch lands after the fetch settles, before the JSON parses', async () => {
  await withFakeDocument(async () => {
    const list = fakeEl('div');
    let target = 't-a';
    const body = deferred();
    const m = mount(list, {
      getTarget: () => target,
      getGeneration: () => (target === 't-a' ? 1 : 2),
      isOpen: () => true,
      fetch: async () => ({ ok: true, json: async () => body.promise }),
    });
    m.noteInit(); // t-a's fetch starts; fetchImpl resolves immediately
                   // (it is not itself deferred here -- only its .json() is),
                   // so the first stillCurrent() check passes while target
                   // is still t-a.
    await flushMicrotasks();
    target = 't-b'; // the switch now lands strictly between the two checks
    body.resolve({ session: 'SA', oldest: 1, latest: 5, events: [1, 2, 3, 4, 5].map(tap) });
    await flushMicrotasks();
    assert.equal(m.log.rows().length, 0);
  });
});

test('mount: a fresh fetch after a target switch is not blocked by the superseded one (item 2, continued)', async () => {
  await withFakeDocument(async () => {
    const list = fakeEl('div');
    let target = 't-a';
    let gen = 1;
    const responses = [];
    const m = mount(list, {
      getTarget: () => target,
      getGeneration: () => gen,
      isOpen: () => true,
      fetch: async () => { const r = deferred(); responses.push(r); return r.promise; },
    });
    m.noteInit(); // t-a fetch #1, in flight
    responses[0].resolve(fakeResponse({ session: 'SA', oldest: 1, latest: 2, events: [tap(1), tap(2)] }));
    await flushMicrotasks();
    assert.equal(m.log.rows().length, 2);

    target = 't-b';
    gen = 2;
    m.noteInit(); // t-b fetch #2 -- the previous one already settled and left
                   // its (target, generation) key, so this is not blocked.
    assert.equal(responses.length, 2);
    responses[1].resolve(fakeResponse({ session: 'SB', oldest: 0, latest: 0, events: [] }));
    await flushMicrotasks();
    // t-b starts from a clean log: t-a's rows are gone (target switch), and
    // the empty SB page did not resurrect them.
    assert.equal(m.log.rows().length, 0);
  });
});

test('mount: formatRow failed styling reaches the row (A9 direction)', () => {
  return withFakeDocument(() => {
    const list = fakeEl('div');
    const m = mount(list, { getTarget: () => 't1', isOpen: () => true, fetch: async () => fakeResponse({ session: 's', oldest: 0, latest: 0, events: [] }) });
    const ev = { seq: 1, t: 0, source: 'agent', kind: 'tap', from: [0.5, 0.5], err: 'boom' };
    m.handleEvent(new TextEncoder().encode(JSON.stringify(ev)));
    assert.equal(list.children[0].className, 'logRow logFailed');
  });
});

test('mount: row text uses textContent, not innerHTML (A4 direction)', () => {
  return withFakeDocument(() => {
    const list = fakeEl('div');
    const m = mount(list, { getTarget: () => 't1', isOpen: () => true, fetch: async () => fakeResponse({ session: 's', oldest: 0, latest: 0, events: [] }) });
    m.handleEvent(new TextEncoder().encode(JSON.stringify(tap(1))));
    const whatNode = list.children[0].children[1];
    assert.equal(whatNode.textContent, 'Tap 0.1, 0.1');
  });
});

test('mount: render() clears the list before repainting, so it does not grow on every call (A10 direction)', () => {
  return withFakeDocument(() => {
    const list = fakeEl('div');
    const m = mount(list, { getTarget: () => 't1', isOpen: () => true, fetch: async () => fakeResponse({ session: 's', oldest: 0, latest: 0, events: [] }) });
    m.handleEvent(new TextEncoder().encode(JSON.stringify(tap(1))));
    m.render();
    m.render();
    assert.equal(list.children.length, 1);
  });
});

test('mount: noteInit resets lastSeq before the catch-up fetch, so it asks after=0 not a stale number (A7 direction)', async () => {
  await withFakeDocument(async () => {
    const list = fakeEl('div');
    const urls = [];
    const m = mount(list, {
      getTarget: () => 't1',
      isOpen: () => true,
      fetch: async (url) => { urls.push(url); return fakeResponse({ session: 's', oldest: 0, latest: 0, events: [] }); },
    });
    m.log.add(tap(1));
    m.log.add(tap(2));
    m.log.lastSeq = 2;
    m.noteInit();
    await Promise.resolve();
    await Promise.resolve();
    assert.equal(urls[0], '/api/events?target=t1&after=0');
  });
});

test('mount: noteInit triggers the catch-up fetch (A1/A2 direction)', async () => {
  await withFakeDocument(async () => {
    const list = fakeEl('div');
    let called = 0;
    const m = mount(list, {
      getTarget: () => 't1',
      isOpen: () => true,
      fetch: async () => { called++; return fakeResponse({ session: 's', oldest: 1, latest: 2, events: [tap(1), tap(2)] }); },
    });
    m.noteInit();
    await Promise.resolve();
    await Promise.resolve();
    assert.equal(called, 1);
    assert.equal(m.log.rows().length, 2); // the fetch result was actually merged (A6 direction)
  });
});

// Round 2 pinned this test against `after=5` -- log.lastSeq, which add()
// already advances past the very seq that revealed the gap. That request
// asks the ring for everything *after* the range that needs filling, so the
// gap marker could never close: live, it asked after=6 and the marker sat
// there forever even though the ring held every missing seq. Fixed to fetch
// from the gap's own `after` (openGapAfter()), and extended to check the
// marker actually clears once the page lands, not just the URL (round 3,
// item 3 / P5).
test('mount: a gap-triggered fetch asks from the gap\'s own after, and the marker clears once the page lands (P5)', async () => {
  await withFakeDocument(async () => {
    const list = fakeEl('div');
    const urls = [];
    const m = mount(list, {
      getTarget: () => 't1',
      isOpen: () => true,
      fetch: async (url) => {
        urls.push(url);
        return fakeResponse({ session: 's', oldest: 2, latest: 4, events: [tap(2), tap(3), tap(4)] });
      },
    });
    m.handleEvent(new TextEncoder().encode(JSON.stringify(tap(1))));
    m.handleEvent(new TextEncoder().encode(JSON.stringify(tap(5)))); // gap 1->5; lastSeq is now 5
    assert.equal(m.log.rows().filter((r) => r.gap).length, 1); // sanity: the marker exists before the fetch lands
    await flushMicrotasks();
    assert.equal(urls[0], '/api/events?target=t1&after=1'); // the gap's after, not lastSeq (5)
    assert.equal(m.log.rows().filter((r) => r.gap).length, 0); // filled: the marker is gone
    assert.deepEqual(m.log.rows().map((r) => r.seq), [5, 4, 3, 2, 1]);
  });
});

test('mount: the single-flight key includes target, not just generation -- two targets sharing a generation value do not block each other (item1)', async () => {
  await withFakeDocument(async () => {
    const list = fakeEl('div');
    let target = 't-a';
    const gen = 1; // held constant on purpose: both targets report generation 1
    const urls = [];
    const gates = {};
    const m = mount(list, {
      getTarget: () => target,
      getGeneration: () => gen,
      isOpen: () => true,
      fetch: async (url) => { urls.push(url); const g = deferred(); gates[url] = g; return g.promise; },
    });
    m.noteInit(); // t-a's fetch starts, still in flight, key would be "1" if gen alone were used
    target = 't-b';
    m.noteInit(); // a different target, same generation -- must not be coalesced away
    await flushMicrotasks();
    assert.equal(urls.length, 2);
    assert.match(urls[0], /target=t-a/);
    assert.match(urls[1], /target=t-b/);
    gates[urls[1]].resolve(fakeResponse({ session: 'SB', oldest: 1, latest: 1, events: [tap(1)] }));
    await flushMicrotasks();
    assert.equal(m.log.rows().length, 1);
  });
});

// --- provisional-epoch semantics (item 2: server restart vs. resume) ------
//
// noteInit() on a same-target reconnect cannot know yet whether the session
// survived or the server restarted underneath it -- that only comes back
// with the catch-up page. These pin the two outcomes and the case where the
// catch-up never answers at all, directly against EventLog (no mount()
// wiring involved, since this is core state, not DOM).

test('EventLog: a same-target reconnect across a server restart keeps the old rows separate from the new ones (P4)', () => {
  const log = new EventLog();
  log.merge({ session: 'OLD', oldest: 1, latest: 3, events: [tap(1), tap(2), tap(3)] });
  log.noteInit(); // reconnect, same target -- opens a provisional epoch
  // The new session's live events arrive over the wire before the catch-up
  // page lands.
  for (let s = 1; s <= 5; s++) log.add(tap(s));
  // The catch-up now lands and reports a *different* session: a restart,
  // not a resume (trap 21's path -- a phone locked past idleGrace).
  log.merge({ session: 'NEW', oldest: 1, latest: 5, events: [1, 2, 3, 4, 5].map(tap) });
  const rows = log.rows();
  assert.equal(rows.length, 8); // NEW1..5 once each, OLD1..3 -- nothing mixed, nothing duplicated
  assert.deepEqual(rows.slice(0, 5).map((r) => r.seq), [5, 4, 3, 2, 1]); // NEW, newest first, on top
  assert.deepEqual(rows.slice(5).map((r) => r.seq), [3, 2, 1]); // OLD, below, untouched
});

test('EventLog: if the catch-up never answers, provisional rows stay separate rather than guessing (P4b)', () => {
  const log = new EventLog();
  log.merge({ session: 'OLD', oldest: 1, latest: 3, events: [tap(1), tap(2), tap(3)] });
  log.noteInit();
  for (let s = 1; s <= 4; s++) log.add(tap(s)); // NEW4..1 arrive; merge() never runs
  const rows = log.rows();
  assert.equal(rows.length, 7); // still separate: nothing mixed, nothing lost
  assert.deepEqual(rows.slice(0, 4).map((r) => r.seq), [4, 3, 2, 1]);
  assert.deepEqual(rows.slice(4).map((r) => r.seq), [3, 2, 1]);
});

test('EventLog.merge: promoting a provisional epoch to a new session keeps the live lastSeq, does not reset it (E6b)', () => {
  const log = new EventLog();
  log.merge({ session: 'OLD', oldest: 1, latest: 1, events: [tap(1)] });
  log.noteInit();
  log.add(tap(1));
  log.add(tap(2));
  log.add(tap(3)); // live NEW events already advanced lastSeq to 3
  log.merge({ session: 'NEW', oldest: 1, latest: 3, events: [] }); // restart resolves; the page itself reports nothing new
  assert.equal(log.lastSeq, 3); // must not have been reset to 0 by the session change
});

test('EventLog.rows: sorts by epoch before seq -- a lower seq in a newer epoch still sorts above an older epoch\'s higher seq (E13)', () => {
  const log = new EventLog();
  log.merge({ session: 'OLD', oldest: 1, latest: 9, events: [tap(9)] }); // epoch 0, seq 9
  log.noteInit();
  log.add(tap(1)); // provisional epoch, seq 1 -- lower seq, newer epoch
  log.merge({ session: 'NEW', oldest: 1, latest: 1, events: [tap(1)] }); // restart: provisional epoch becomes permanent
  const rows = log.rows();
  assert.equal(rows[0].seq, 1); // the newer epoch's seq 1 sorts above the older epoch's seq 9
  assert.equal(rows[1].seq, 9);
});

test('EventLog.merge: detects a gap inside a single page\'s events, not just at its edges (E9)', () => {
  const log = new EventLog();
  log.merge({ session: 's1', oldest: 1, latest: 5, events: [tap(1), tap(2), tap(5)] }); // 3, 4 missing mid-page
  const gaps = log.rows().filter((r) => r.gap);
  assert.equal(gaps.length, 1);
  assert.deepEqual(gaps[0], { gap: true, after: 2, before: 5, lost: false });
});

test('EventLog.openGapAfter: only the current epoch\'s own gap counts, not a stale one left behind by a restart (item3)', () => {
  const log = new EventLog();
  // An old epoch's gap that was never filled before a restart moved on.
  log._pushGap(0, 1, 5, false);
  log.epoch = 1;
  // The current epoch's own gap, further along.
  log._pushGap(1, 10, 15, false);
  assert.equal(log.openGapAfter(), 10); // not 1 -- epoch 0's gap belongs to a connection that is gone
});

// --- round 4: a second MsgInit before the first catch-up lands -------------
//
// The server sends MsgInit twice at the start of every session -- a
// provisional codec, then the resolved one once SPS arrives -- and a
// rotation resends it too. noteInit() opens a provisional epoch and, until
// this round, unconditionally did so again on the *next* call even if the
// previous provisional was still unresolved: the second call took the
// still-open provisional as its own "prior" epoch, reset lastSeq to 0, and
// stranded the first provisional's rows under an epoch nothing would ever
// fold or promote again. Live, that produced a spurious "gap 1-1" (lastSeq
// reset made the very next live event look like it skipped seq 1) and tap
// seq 1 rendered twice (once under the stranded epoch, once again when the
// catch-up page -- resolved against the wrong "prior" -- replayed it into
// the current one). These reproduce the real wire order (init, event, init,
// more events, catch-up) rather than a same-tick approximation.

test('EventLog: a second MsgInit before the first catch-up lands does not strand or duplicate rows (server restart)', () => {
  const log = new EventLog();
  log.merge({ session: 'OLD', oldest: 1, latest: 3, events: [tap(1), tap(2), tap(3)] });
  log.noteInit(); // init #1 (provisional codec) -- opens epoch 1, provisional
  log.add(tap(1)); // a live event lands in the still-open provisional epoch
  log.noteInit(); // init #2 (resolved codec) -- must retire epoch 1 into epoch 2, not strand it
  for (let s = 2; s <= 5; s++) log.add(tap(s)); // more live events, after the second init
  assert.equal(log.rows().filter((r) => r.gap).length, 0); // no spurious gap from a wrongly-reset lastSeq
  // The catch-up page for the FIRST init's fetch lands now, reporting a
  // different session: a real restart, not a resume.
  log.merge({ session: 'NEW', oldest: 1, latest: 5, events: [1, 2, 3, 4, 5].map(tap) });
  const rows = log.rows();
  assert.equal(rows.length, 8); // NEW1..5 once each, OLD1..3 -- nothing stranded, nothing duplicated
  assert.deepEqual(rows.slice(0, 5).map((r) => r.seq), [5, 4, 3, 2, 1]);
  assert.deepEqual(rows.slice(5).map((r) => r.seq), [3, 2, 1]);
});

test('EventLog: two MsgInits before catch-up lands folds cleanly on a resume (same session)', () => {
  const log = new EventLog();
  log.merge({ session: 'S', oldest: 1, latest: 3, events: [tap(1), tap(2), tap(3)] });
  log.noteInit(); // init #1
  log.noteInit(); // init #2 (e.g. a rotation's resend), before catch-up lands
  // The catch-up page for the first fetch lands, reporting the SAME session:
  // a resume, not a restart.
  log.merge({ session: 'S', oldest: 1, latest: 3, events: [tap(1), tap(2), tap(3)] });
  const rows = log.rows();
  assert.equal(rows.length, 3); // folded back to one copy each, not stranded under a dead epoch
  assert.deepEqual(rows.map((r) => r.seq), [3, 2, 1]);
});

test('mount: a reconnect followed by a rotation-style init before catch-up lands does not duplicate or leave a spurious gap', async () => {
  await withFakeDocument(async () => {
    const list = fakeEl('div');
    const gates = [];
    const m = mount(list, {
      getTarget: () => 't1',
      getGeneration: () => 1,
      isOpen: () => true,
      fetch: async () => { const g = deferred(); gates.push(g); return g.promise; },
    });
    // getTarget() already names 't1' before any of this: the target was
    // selected and connected once already, which is what makes the next
    // noteInit() an ordinary reconnect rather than the log's very first
    // target switch (a switch always clears cleanly and proves nothing
    // about the retire path). Then the reviewer's exact live repro, in the
    // real wire order: init, one live event, a second init (the codec
    // resend, or a rotation) before the first init's catch-up has
    // resolved, then more live events land on top of that. Carrying
    // lastSeq forward through the retire must keep this gap-free
    // throughout, not just once catch-up lands.
    m.noteInit(); // establishes target 't1' for the first time (switch/clear)
    m.noteInit(); // init #1 of the reconnect -- fetch #1 starts and is left pending
    m.handleEvent(new TextEncoder().encode(JSON.stringify(tap(1))));
    m.noteInit(); // init #2 -- retires the still-open provisional from init #1
    assert.equal(m.log.rows().filter((r) => r.gap).length, 0); // no spurious gap from a reset lastSeq
    for (let s = 2; s <= 5; s++) m.handleEvent(new TextEncoder().encode(JSON.stringify(tap(s))));
    assert.equal(m.log.rows().filter((r) => r.gap).length, 0);
    gates[0].resolve(fakeResponse({ session: 'S1', oldest: 1, latest: 1, events: [tap(1)] }));
    await flushMicrotasks();
    const rows = m.log.rows();
    assert.equal(rows.filter((r) => !r.gap).length, 5); // seq 1..5 once each -- no duplicate seq 1
    assert.deepEqual(rows.map((r) => r.seq), [5, 4, 3, 2, 1]);
    assert.equal(rows.filter((r) => r.gap).length, 0); // nothing left open at the end either
  });
});

// --- round 4: surviving mutants -----------------------------------------

test('mount: the in-flight key is released after a fetch completes, so a later gap on the same connection still fetches (N21)', async () => {
  await withFakeDocument(async () => {
    const list = fakeEl('div');
    const urls = [];
    const m = mount(list, {
      getTarget: () => 't1',
      getGeneration: () => 1,
      isOpen: () => true,
      fetch: async (url) => { urls.push(url); return fakeResponse({ session: 's', oldest: 1, latest: 1, events: [tap(1)] }); },
    });
    m.noteInit(); // fetch #1: after=0
    await flushMicrotasks();
    assert.equal(urls.length, 1);
    // A later gap on the SAME connection (same target, same generation) must
    // still trigger its own fetch -- if the in-flight key were never
    // released, this would be silently swallowed forever.
    m.handleEvent(new TextEncoder().encode(JSON.stringify(tap(1)))); // dup of what catch-up already gave, no gap
    m.handleEvent(new TextEncoder().encode(JSON.stringify(tap(5)))); // gap 1->5
    await flushMicrotasks();
    assert.equal(urls.length, 2); // the gap-triggered fetch actually went out
    assert.match(urls[1], /after=1/);
  });
});

test('EventLog.openGapAfter: returns the lowest open gap, not the highest (N17)', () => {
  const log = new EventLog();
  log._pushGap(0, 10, 15, false); // a later, higher gap
  log._pushGap(0, 1, 5, false); // an earlier, lower gap
  assert.equal(log.openGapAfter(), 1); // the lowest, so catch-up makes forward progress from the earliest hole
});

test('EventLog.merge: a fold moves the provisional epoch\'s live rows into priorEpoch without duplicating them (N1)', () => {
  const log = new EventLog();
  log.merge({ session: 'S', oldest: 1, latest: 1, events: [tap(1)] }); // epoch 0
  log.noteInit(); // provisional epoch 1
  log.add(tap(2)); // a live event lands in the provisional epoch before catch-up resolves
  // Catch-up reports the SAME session: a resume. The fold must move seq 2
  // (recorded under the provisional epoch) back into epoch 0 alongside seq
  // 1, not leave two separate epochs each claiming to be current.
  log.merge({ session: 'S', oldest: 1, latest: 2, events: [tap(1), tap(2)] });
  const rows = log.rows();
  assert.equal(rows.length, 2); // seq 1 and seq 2, once each -- not stranded, not duplicated
  assert.deepEqual(rows.map((r) => r.seq), [2, 1]);
  assert.equal(log.epoch, 0); // reverted to the one continuing epoch
});

test('EventLog.merge: a gap that falls entirely below the ring\'s oldest is marked lost, so openGapAfter stops returning it (minor item 3)', () => {
  const log = new EventLog();
  log.add(tap(1));
  log.add(tap(10)); // gap 1->10 recorded, not yet lost
  assert.equal(log.openGapAfter(), 1);
  // A later catch-up page lands after the ring has rolled well past the
  // gap's range -- the events that would have filled 2..9 are gone for
  // good, not just not-yet-fetched.
  log.merge({ session: 's1', oldest: 20, latest: 20, events: [tap(20)] });
  assert.equal(log.openGapAfter(), null); // the old gap no longer counts as open
  const gap = log.rows().find((r) => r.gap && r.after === 1 && r.before === 10);
  assert.ok(gap);
  assert.equal(gap.lost, true); // marked lost, not silently dropped -- still visible as unrecoverable
});

// --- round 4: surviving mutants from the second review --------------------

test('EventLog: retiring an open provisional carries its gap marker and lastSeq forward under the new epoch, not stuck behind it', () => {
  const log = new EventLog();
  log.merge({ session: 'OLD', oldest: 1, latest: 1, events: [tap(1)] }); // epoch 0, lastSeq 1
  log.noteInit(); // opens provisional epoch 1, lastSeq resets to 0
  const gap = log.add(tap(5)); // skips ahead under the provisional -- gap(0,5) recorded in epoch 1
  assert.ok(gap);
  assert.equal(log.openGapAfter(), 0);
  log.noteInit(); // retire: a second init before catch-up landed
  assert.equal(log.epoch, 2);
  // The gap and the event both have to move forward to the new provisional
  // epoch, not stay tagged with the epoch that was just retired -- openGapAfter
  // only ever looks at entries in the CURRENT epoch, so a marker left behind
  // is invisible forever and no fetch will ever be asked to fill it.
  assert.equal(log.openGapAfter(), 0);
  assert.equal(log.lastSeq, 5); // carried forward from the moved event, not reset to 0
});

test('EventLog.merge: a fold recomputes lastSeq from the merged entries and dedupes an event that already existed under priorEpoch (N3/N4)', () => {
  const log = new EventLog();
  log.merge({ session: 'S', oldest: 1, latest: 3, events: [tap(1), tap(2), tap(3)] }); // epoch 0, lastSeq 3
  log.noteInit(); // provisional epoch 1, lastSeq resets to 0
  log.add(tap(2)); // a live "replay" of a seq priorEpoch already has, recorded under the provisional
  // Catch-up reports the same session: a resume, so this folds.
  log.merge({ session: 'S', oldest: 1, latest: 3, events: [tap(1), tap(2), tap(3)] });
  const rows = log.rows();
  assert.equal(rows.length, 3); // seq 1,2,3 once each -- the provisional's seq 2 collapsed into priorEpoch's, not duplicated
  assert.equal(log.lastSeq, 3); // recomputed as the true max across the fold, not left at the provisional's smaller value
});

test('EventLog.merge: a fold dedupes a gap marker that already existed under priorEpoch (N5)', () => {
  const log = new EventLog();
  log.merge({ session: 'S', oldest: 1, latest: 5, events: [tap(5)] }); // epoch 0: gap(0,5) recorded, lastSeq 5
  log.noteInit(); // provisional epoch 1, lastSeq resets to 0
  log.add(tap(5)); // the same skip happens again under the provisional -- gap(0,5) recorded in epoch 1 too
  // Catch-up reports the same session: a resume, so this folds. Both
  // epochs' gap(0,5) markers remap to the same priorEpoch key.
  log.merge({ session: 'S', oldest: 1, latest: 5, events: [tap(5)] });
  const gapRows = log.rows().filter((r) => r.gap && r.after === 0 && r.before === 5);
  assert.equal(gapRows.length, 1); // deduped to one marker, not two identical ones stacked on the same range
});

// --- remaining pinned survivors --------------------------------------------

test('mount: the source field is written via textContent, not innerHTML (M21)', () => {
  return withFakeDocument(() => {
    const list = fakeEl('div');
    const m = mount(list, { getTarget: () => 't1', isOpen: () => true, fetch: async () => fakeResponse({ session: 's', oldest: 0, latest: 0, events: [] }) });
    m.handleEvent(new TextEncoder().encode(JSON.stringify(tap(1))));
    const sourceNode = list.children[0].children[2];
    assert.equal(sourceNode.textContent, 'viewer 1');
    assert.equal(sourceNode.innerHTML, ''); // untouched -- textContent is what carries the value
  });
});

test('mount: default limit is 200, matching EventLog\'s own default (E24b)', () => {
  return withFakeDocument(() => {
    const list = fakeEl('div');
    const m = mount(list, { getTarget: () => 't1', isOpen: () => true, fetch: async () => fakeResponse({ session: 's', oldest: 0, latest: 0, events: [] }) });
    for (let s = 1; s <= 201; s++) m.handleEvent(new TextEncoder().encode(JSON.stringify(tap(s))));
    assert.equal(m.log.rows().length, 200);
  });
});

test('mount: a successful catch-up merge triggers a render, not just a log update (M20)', async () => {
  await withFakeDocument(async () => {
    const list = fakeEl('div');
    const m = mount(list, {
      getTarget: () => 't1',
      isOpen: () => true,
      fetch: async () => fakeResponse({ session: 's', oldest: 1, latest: 2, events: [tap(1), tap(2)] }),
    });
    m.noteInit();
    await flushMicrotasks();
    assert.equal(list.children.length, 2); // the DOM actually reflects the merged page, not just m.log
  });
});

// --- round 6: long-session resume corruption -------------------------------
//
// A phone locked past idleGrace after 200+ taps reaches this: the live
// phase alone already exceeds the client's 200-row limit (well under the
// server ring's 256), so by the time a same-target reconnect's catch-up
// page lands, the fold has to reconcile a provisional epoch's live rows
// against a page that repeats seqs this client already trimmed away.

test('EventLog/mount: a resume after more than `limit` live events keeps the newest N seqs unbroken, even with a live tap racing the catch-up (case 6)', async () => {
  await withFakeDocument(async () => {
    const list = fakeEl('div');
    const badge = fakeEl('span');
    let gen = 1;
    const gates = [];
    const m = mount(list, {
      getTarget: () => 't1',
      getGeneration: () => gen,
      isOpen: () => true,
      badgeEl: badge,
      fetch: async () => { const g = deferred(); gates.push(g); return g.promise; },
    });
    m.noteInit(); // fetch #1, epoch 0
    gates[0].resolve(fakeResponse({ session: 'S1', oldest: 0, latest: 0, events: [] }));
    await flushMicrotasks();
    for (let s = 1; s <= 300; s++) m.handleEvent(new TextEncoder().encode(JSON.stringify(tap(s))));
    gen++;
    m.noteInit(); // reconnect: opens a provisional epoch, fetch #2 dispatched with after=0
    // A live event lands before the catch-up resolves: seq 301 jumps ahead
    // of the provisional's own lastSeq (0), so add() opens a gap(0, 301).
    m.handleEvent(new TextEncoder().encode(JSON.stringify(tap(301))));
    gates[1].resolve(fakeResponse({
      session: 'S1', oldest: 45, latest: 301,
      events: Array.from({ length: 301 - 45 + 1 }, (_, i) => tap(45 + i)),
    }));
    await flushMicrotasks();
    const rows = m.log.rows();
    assert.equal(rows.filter((r) => r.gap).length, 0); // the live tap's gap resolved away, not stranded open or unmarked
    // Not asserting an exact row count: whether a gap marker briefly shares
    // the trim budget with real events is a design choice (see _trim's own
    // comment), and a valid implementation can land at 199 or 200 rows here
    // depending on it. What has to hold regardless: the seqs kept are the
    // newest ones, contiguous from 301 down -- not a spliced-together
    // 45-300 with a hole in the middle, and not missing the seq right below
    // 301 to a transient gap marker's own eviction.
    const seqs = rows.map((r) => r.seq);
    assert.ok(seqs.length >= 199 && seqs.length <= 200, `expected 199 or 200 rows, got ${seqs.length}`);
    assert.deepEqual(seqs, Array.from({ length: seqs.length }, (_, i) => 301 - i));
    assert.equal(badge.textContent, String(seqs.length));
  });
});

test('EventLog/mount: the same resume with no live tap during the delay leaves no holes either (case 6b)', async () => {
  await withFakeDocument(async () => {
    const list = fakeEl('div');
    let gen = 1;
    const gates = [];
    const m = mount(list, {
      getTarget: () => 't1',
      getGeneration: () => gen,
      isOpen: () => true,
      fetch: async () => { const g = deferred(); gates.push(g); return g.promise; },
    });
    m.noteInit();
    gates[0].resolve(fakeResponse({ session: 'S1', oldest: 0, latest: 0, events: [] }));
    await flushMicrotasks();
    for (let s = 1; s <= 300; s++) m.handleEvent(new TextEncoder().encode(JSON.stringify(tap(s))));
    gen++;
    m.noteInit();
    gates[1].resolve(fakeResponse({
      session: 'S1', oldest: 45, latest: 300,
      events: Array.from({ length: 300 - 45 + 1 }, (_, i) => tap(45 + i)),
    }));
    await flushMicrotasks();
    const seqs = m.log.rows().filter((r) => !r.gap).map((r) => r.seq);
    assert.equal(seqs.length, 200);
    assert.equal(new Set(seqs).size, seqs.length); // no duplicates
    assert.deepEqual(seqs.slice().sort((a, b) => a - b), Array.from({ length: 200 }, (_, i) => 101 + i)); // 101..300, unbroken
  });
});

test('EventLog: a gap opened right at the row limit survives long enough to be filled, not evicted by the very add() that created it', async () => {
  await withFakeDocument(async () => {
    const list = fakeEl('div');
    const gates = [];
    const m = mount(list, {
      getTarget: () => 'A',
      getGeneration: () => 1,
      isOpen: () => true,
      fetch: async (url) => { const g = deferred(); g.url = url; gates.push(g); return g.promise; },
    });
    m.noteInit();
    gates[0].resolve(fakeResponse({ session: 'S1', oldest: 0, latest: 0, events: [] }));
    await flushMicrotasks();
    for (let s = 1; s <= 250; s++) m.handleEvent(new TextEncoder().encode(JSON.stringify(tap(s))));
    // The log is already at its 200-row limit (1..250 trimmed to 51..250).
    // seq 255 jumps ahead of lastSeq(250), opening a gap(250, 255) in the
    // very add() call that also has to trim -- if the gap loses that trim
    // to make room for itself, it is gone before the fetch it triggers can
    // ever fill it.
    m.handleEvent(new TextEncoder().encode(JSON.stringify(tap(255))));
    await flushMicrotasks();
    assert.equal(m.log.openGapAfter(), 250); // still open, not silently evicted
    const fetchUrl = gates.at(-1).url;
    assert.match(fetchUrl, /after=250/); // not after=255 -- that would skip 251-254 forever
    gates.at(-1).resolve(fakeResponse({
      session: 'S1', oldest: 1, latest: 255,
      events: Array.from({ length: 255 - 250 }, (_, i) => tap(251 + i)),
    }));
    await flushMicrotasks();
    assert.equal(m.log.openGapAfter(), null);
    const rows = m.log.rows().filter((r) => !r.gap).map((r) => r.seq);
    assert.deepEqual(new Set(range(rows.length === 0 ? 0 : Math.min(...rows), Math.max(...rows))).size >= 0, true);
    for (let s = 251; s <= 254; s++) assert.ok(rows.includes(s), `seq ${s} missing after catch-up`);
  });
});

test('EventLog: a gap opened below the limit survives being pushed toward it by later live events, until its catch-up lands', () => {
  const log = new EventLog(200);
  log.noteInit('A');
  log.merge({ session: 'S1', oldest: 0, latest: 0, events: [] });
  for (let s = 1; s <= 100; s++) log.add(tap(s));
  log.add(tap(105)); // gap(100, 105), well under the 200 limit
  assert.equal(log.openGapAfter(), 100);
  // 105 more live events (106..210) push total real rows past the limit,
  // trimming down to the newest 200 -- the still-open gap has to survive
  // that trim the same way a real event at its position would, not be
  // evicted ahead of them because it is a marker rather than a tap.
  for (let s = 106; s <= 210; s++) log.add(tap(s));
  assert.equal(log.openGapAfter(), 100); // still open
  assert.ok(log.rows().some((r) => r.gap && r.after === 100 && r.before === 105 && !r.lost));
  log.merge({ session: 'S1', oldest: 1, latest: 210, events: [101, 102, 103, 104].map(tap) });
  assert.equal(log.openGapAfter(), null); // filled once the catch-up lands
});

function range(a, b) { return Array.from({ length: Math.max(0, b - a + 1) }, (_, i) => a + i); }

test('mount: a 500 catch-up still releases the in-flight key, so a later gap on the same connection still fetches (R1)', async () => {
  await withFakeDocument(async () => {
    const list = fakeEl('div');
    const urls = [];
    const m = mount(list, {
      getTarget: () => 't1',
      getGeneration: () => 1,
      isOpen: () => true,
      fetch: async (url) => {
        urls.push(url);
        if (urls.length === 1) return { ok: false }; // catch-up fails, e.g. a 500
        return fakeResponse({ session: 's', oldest: 1, latest: 5, events: [tap(1), tap(5)] });
      },
    });
    m.noteInit(); // fetch #1: fails
    await flushMicrotasks();
    assert.equal(urls.length, 1);
    m.handleEvent(new TextEncoder().encode(JSON.stringify(tap(1))));
    m.handleEvent(new TextEncoder().encode(JSON.stringify(tap(5)))); // gap 1->5
    await flushMicrotasks();
    // If the in-flight key were only released on success (e.g. inside the
    // `!res.ok` early return rather than in `finally`), this second fetch
    // would find the key still held and silently do nothing, forever.
    assert.equal(urls.length, 2);
    assert.match(urls[1], /after=1/);
  });
});

test('EventLog.merge: a gap whose `before` exactly equals the ring\'s oldest is marked lost too, not just one strictly less (L2, boundary)', () => {
  const log = new EventLog();
  log.add(tap(1));
  log.add(tap(10)); // gap 1->10 recorded, not yet lost
  assert.equal(log.openGapAfter(), 1);
  // oldest === before (10): nothing strictly between 1 and 10 can still be
  // in the ring either way, so `<=` must mark this lost the same as a page
  // whose oldest is further past it -- `<` alone would miss this exact
  // boundary and leave openGapAfter looping on an unreachable range.
  log.merge({ session: 's1', oldest: 10, latest: 10, events: [tap(10)] });
  assert.equal(log.openGapAfter(), null);
  const gap = log.rows().find((r) => r.gap && r.after === 1 && r.before === 10);
  assert.ok(gap);
  assert.equal(gap.lost, true);
});

await runAll();
console.log(failures ? `\n${failures} failing` : '\nall passing');
process.exit(failures ? 1 : 0);
