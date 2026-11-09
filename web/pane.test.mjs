// Tests for one pane: a target's connection, decoder and render loop.
// Run: node web/pane.test.mjs
//
// The transport, the decoder, timers and raf are fakes the test drives by
// hand, so nothing waits on the network or the clock. The rotator, the jitter
// buffers and states.js's pure helpers are the real ones: pane.js gets them
// through deps.lib, as app.js hands them over.
import assert from 'node:assert/strict';

const { createPane } = await import('./pane.js');
const { installFakeDom, FakeElement, tick } = await import('./fakedom.js');
const { JitterBuffer, ArrivalJitter } = await import('./jitter.js');
const { createRotator, closedExhausted, MSG_ROTATE } = await import('./rotate.js');
const { secondRtt } = await import('./health.js');
const states = await import('./states.js');
const { mount: mountControl } = await import('./control.js');
const { fakeDocument } = await import('./fakedom.js');

installFakeDom();

let failures = 0;
async function test(name, fn) {
  try { await fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.stack}`); }
}

const MSG_INIT = 0x01;
const enc = new TextEncoder();
function control(type, obj) {
  const body = enc.encode(JSON.stringify(obj));
  const buf = new Uint8Array(1 + body.length);
  buf[0] = type;
  buf.set(body, 1);
  return buf;
}
const INIT = { codec: 'avc1.42E01F', codedWidth: 1080, codedHeight: 2400, control: true };
function frame(key = true, pts = 1000) {
  return { bytes: 10, key, config: false, payload: new Uint8Array([0, 0, 0, 1, 0x65]), ptsMicros: pts };
}

// fakeMount stands in for one per-pane module's mount: every method the pane
// or the page calls is logged as "name.method".
const MODULE_METHODS = ['onInit', 'onEvent', 'clear', 'off', 'notifyResize', 'noteInit', 'handleEvent',
  'handleMessage', 'onTargetSwitch', 'relayout', 'sync'];
function fakeMount(name, log) {
  return (...args) => {
    const api = { deps: args.at(-1), headers: () => ({}), hasControl: () => false };
    for (const m of MODULE_METHODS) api[m] = () => log.push(`${name}.${m}`);
    return api;
  };
}
function fakeModules(log, real = {}) {
  const mods = {};
  for (const n of ['touches', 'cursor', 'overlays', 'control', 'inspect', 'eventlog', 'record', 'flag']) mods[n] = real[n] || fakeMount(n, log);
  return mods;
}

// harness builds the fakes shared by one or more panes in a test.
function harness({ transportName = 'websocket' } = {}) {
  const calls = [];
  const timers = [];
  let nextTimer = 1;
  const rafs = [];
  const decoders = [];
  const reports = [];
  let hidden = false;
  let clock = 0;

  const connectTransport = (opts) => {
    const conn = {
      name: transportName, open: true, sent: [], closed: false,
      send(b) { this.sent.push(b); },
      acks: [],
      sendFrameAck(pts) { this.acks.push(pts); },
      close() { this.closed = true; this.open = false; },
    };
    calls.push({ opts, conn });
    return Promise.resolve(conn);
  };

  const fakeOverlay = () => {
    let shown = null;
    return {
      show(s) { shown = s.key; },
      hide() { shown = null; },
      state() { return shown; },
    };
  };

  const lib = {
    connectTransport, ArrivalJitter, JitterBuffer, createRotator, closedExhausted, MSG_ROTATE,
    secondRtt, mountOverlay: fakeOverlay,
    reconnectingState: states.reconnectingState,
    shutDownState: states.shutDownState,
    connectingState: states.connectingState,
    afterClose: states.afterClose,
    afterResume: states.afterResume,
    endedFromClose: states.endedFromClose,
  };

  function els() {
    const m = {};
    for (const k of ['fps', 'dec', 'rtt', 'jit', 'q', 'drop', 'buf', 'trans', 'shed', 'rate']) m[k] = new FakeElement('span');
    const canvas = new FakeElement('canvas');
    canvas.getContext = () => ({ drawImage() {} });
    return {
      canvas, root: new FakeElement('div'),
      overlay: new FakeElement('div'), m,
    };
  }

  // make builds a pane. Its control module is control.js's real mount unless
  // modules says otherwise; the rest are fakes logging into moduleCalls.
  const moduleCalls = [];
  function make(id, targetId, { modules, label } = {}) {
    return createPane({
      id, targetId, els: els(), lib, label,
      modules: modules || fakeModules(moduleCalls, { control: mountControl }),
      moduleDeps: {
        doc: fakeDocument,
        fetch: () => Promise.reject(new Error('no fetch in tests')),
        toast: new FakeElement('div'),
        inspectSheet: new FakeElement('div'),
        pillRec: new FakeElement('span'),
        postInput: async () => {},
        takeOver: () => {},
        storage: null,
        onInspectToggle: () => {},
        logIsOpen: () => false,
      },
      createDecoder: (init) => {
        const d = {
          init, state: 'unconfigured', decodes: 0, decodeQueueSize: 0,
          configure() { this.state = 'configured'; },
          decode() { this.decodes++; },
          close() { this.state = 'closed'; },
        };
        decoders.push({ pane: id, d });
        return d;
      },
      raf: (fn) => { rafs.push(fn); return rafs.length; },
      cancelRaf: () => {},
      now: () => clock,
      setTimeout: (fn, ms) => { const t = { id: nextTimer++, fn, ms }; timers.push(t); return t.id; },
      clearTimeout: (handle) => {
        const i = timers.findIndex((t) => t.id === handle);
        if (i >= 0) timers.splice(i, 1);
      },
      hidden: () => hidden,
      preferWebTransport: true,
      report: (rec) => reports.push(rec),
      onInit: () => {},
      onStatus: () => {},
      onShape: () => {},
      onControlMessage: () => {},
    });
  }

  // runTimers fires every queued timer, including ones queued while running,
  // and lets each connect's awaited handshake settle.
  async function runTimers() {
    for (let n = 0; timers.length && n < 50; n++) {
      const t = timers.shift();
      t.fn();
      await tick();
    }
  }

  return {
    calls, timers, decoders, reports, moduleCalls, make, runTimers, rafs,
    setHidden(v) { hidden = v; },
    setNow(ms) { clock = ms; },
  };
}

// live connects a pane and delivers its init, so it is streaming.
async function live(h, pane, at = h.calls.length) {
  pane.connect();
  await tick();
  h.calls[at].opts.onControl(control(MSG_INIT, INIT));
  return h.calls[at];
}

// decodeAndPresent hands the decoder's output callback a frame stamped pts, as
// WebCodecs does, then runs the render loop.
function decoded(h, pts) {
  const frame = { timestamp: pts, closed: false, close() { this.closed = true; } };
  h.decoders.at(-1).d.init.output(frame);
  return frame;
}
function present(h) {
  const fns = h.rafs.splice(0);
  for (const fn of fns) fn(0);
}

await test('present acks each frame once, after it is drawn', async () => {
  const h = harness();
  const pane = h.make('p1', 'emulator-5554');
  const call = await live(h, pane);
  let t = 0;
  for (const pts of [1000, 17667, 34333]) {
    call.opts.onFrame(frame(pts === 1000, pts));
    decoded(h, pts);
    assert.equal(call.conn.acks.includes(pts), false, 'not acked on receive or decode');
    t += 100;
    h.setNow(t);
    present(h);
    present(h);
  }
  assert.deepEqual(call.conn.acks, [1000, 17667, 34333]);
  for (let i = 0; i < 5; i++) present(h);
  assert.equal(call.conn.acks.length, 3, 'a presented frame is not acked again');
});

await test('on the remote track every decoded frame is drawn and acked at once', async () => {
  const h = harness();
  const pane = h.make('p1', 'emulator-5554');
  const call = await live(h, pane);
  call.opts.onControl(control(MSG_INIT, { ...INIT, track: 'remote' }));
  // A clump of three that the main track would thin to the newest.
  const frames = [1000, 2000, 3000].map((pts) => { call.opts.onFrame(frame(pts === 1000, pts)); return decoded(h, pts); });
  assert.deepEqual(call.conn.acks, [1000, 2000, 3000], 'acked on decode, with no render tick');
  assert.ok(frames.every((f) => f.closed));
  // A later init without the field puts it back on the buffered path.
  call.opts.onControl(control(MSG_INIT, INIT));
  call.opts.onFrame(frame(false, 4000));
  decoded(h, 4000);
  assert.deepEqual(call.conn.acks, [1000, 2000, 3000], 'main track waits for the render loop');
});

await test('a frame the jitter buffer skips is never acked', async () => {
  const h = harness();
  const pane = h.make('p1', 'emulator-5554');
  const call = await live(h, pane);
  // Three frames due at once: only the newest is drawn, the others are
  // discarded undrawn, and the server's ack of the newest pops them.
  const frames = [1000, 2000, 3000].map((pts) => { call.opts.onFrame(frame(pts === 1000, pts)); return decoded(h, pts); });
  h.setNow(100);
  for (let i = 0; i < 20; i++) present(h);
  assert.deepEqual(call.conn.acks, [3000]);
  assert.ok(frames.every((f) => f.closed));
});

await test('a frame still queued when the pane switches target is never acked', async () => {
  const h = harness();
  const pane = h.make('p1', 'emulator-5554');
  const call = await live(h, pane);
  call.opts.onFrame(frame(true, 1000));
  decoded(h, 1000);
  pane.switchTarget('sim:A');
  h.setNow(100);
  for (let i = 0; i < 20; i++) present(h);
  assert.deepEqual(call.conn.acks, []);
});

await test("stats() carries the server's queue numbers only once they arrive", async () => {
  const h = harness();
  const pane = h.make('p1', 'emulator-5554');
  const call = await live(h, pane);
  assert.ok(Number.isNaN(pane.stats().queueMs));
  call.opts.onControl(control(0x03, { shedRate: 0, health: 'good' }));
  assert.ok(Number.isNaN(pane.stats().queueMs), 'a viewer that has not acked gets neither');
  call.opts.onControl(control(0x03, { shedRate: 0, health: 'good', queueMs: 41.5, backlog: 3 }));
  assert.equal(pane.stats().queueMs, 41.5);
  assert.equal(pane.stats().backlog, 3);
});

await test('connect opens one transport for its target', async () => {
  const h = harness();
  const pane = h.make('p1', 'emulator-5554');
  pane.connect();
  await tick();
  assert.equal(h.calls.length, 1);
  assert.equal(h.calls[0].opts.target, 'emulator-5554');
  assert.equal(pane.transportName, 'websocket');
});

await test("switchTarget ignores the old connection's close", async () => {
  const h = harness();
  const pane = h.make('p1', 'emulator-5554');
  await live(h, pane);
  pane.switchTarget('sim:A');
  h.calls[0].opts.onClose({});
  await h.runTimers();
  assert.equal(h.calls.length, 2);
  assert.equal(h.calls[1].opts.target, 'sim:A');
  assert.equal(pane.targetId, 'sim:A');
  assert.equal(h.timers.length, 0, 'no reconnect pending');
  assert.notEqual(pane.status, 'reconnecting');
  assert.notEqual(pane.status, 'error');
});

await test('an unexpected close schedules exactly one reconnect', async () => {
  const h = harness();
  const pane = h.make('p1', 'emulator-5554');
  await live(h, pane);
  h.calls[0].opts.onClose({});
  assert.equal(h.timers.length, 1);
  await h.runTimers();
  assert.equal(h.calls.length, 2);
  assert.equal(h.timers.length, 0);
});

await test('suspend then resume opens one new transport', async () => {
  const h = harness();
  const pane = h.make('p1', 'emulator-5554');
  await live(h, pane);
  h.setHidden(true);
  pane.suspend();
  assert.equal(h.calls[0].conn.closed, true);
  assert.equal(h.timers.length, 0, 'no timer queued');
  // The suspended transport's close event arrives after; it is inert.
  h.calls[0].opts.onClose({});
  assert.equal(h.timers.length, 0);
  h.setHidden(false);
  pane.resume();
  await tick();
  assert.equal(h.calls.length, 2);
  assert.equal(h.reports.filter((r) => r.event === 'suspend').length, 1);
  assert.equal(h.reports.filter((r) => r.event === 'resume').length, 1);
});

await test('rotator promote swaps the transport without a reconnect', async () => {
  const h = harness({ transportName: 'webtransport' });
  const pane = h.make('p1', 'emulator-5554');
  const first = await live(h, pane);
  first.opts.onControl(control(MSG_ROTATE, { bytes: 1, streams: 1 }));
  await tick();
  assert.equal(h.calls.length, 2, 'the rotator opened a standby');
  assert.equal(h.calls[1].opts.noFallback, true);
  const standby = h.calls[1];
  standby.opts.onControl(control(MSG_INIT, INIT));
  standby.opts.onFrame(frame(true, 5000));
  assert.equal(first.conn.closed, true, 'the old connection is closed on promote');
  // Its close event is inert: the promote took a new generation.
  first.opts.onClose({});
  assert.equal(pane.send(new Uint8Array([1])), true);
  assert.equal(standby.conn.sent.length, 1);
  assert.equal(first.conn.sent.length, 0);
  assert.equal(h.timers.length, 0, 'no reconnect timer');
  assert.ok(h.reports.some((r) => r.event === 'rotate-promote'));
});

await test('two panes do not share state', async () => {
  const h = harness();
  const p1 = h.make('p1', 'emulator-5554');
  const p2 = h.make('p2', 'sim:A');
  await live(h, p1, 0);
  const c2 = await live(h, p2, 1);
  p1.close();
  assert.equal(h.calls[0].conn.closed, true);
  assert.equal(c2.conn.closed, false);
  c2.opts.onFrame(frame(true));
  const d1 = h.decoders.find((x) => x.pane === 'p1').d;
  const d2 = h.decoders.find((x) => x.pane === 'p2').d;
  assert.equal(d1.decodes, 0);
  assert.equal(d2.decodes, 1);
  assert.equal(p2.status, 'live');
});

await test('report fills in pane and target', async () => {
  const h = harness();
  const pane = h.make('p1', 'emulator-5554');
  pane.report({ kind: 'x' });
  assert.deepEqual(h.reports.at(-1), { kind: 'x', pane: 'p1', target: 'emulator-5554' });
  pane.report({ kind: 'x', target: 'other' });
  assert.deepEqual(h.reports.at(-1), { kind: 'x', pane: 'p1', target: 'other' });
});

await test("the shut-down screen names its own pane's target", async () => {
  const h = harness();
  const asked = [];
  const label = (t) => { asked.push(t); return t; };
  const p1 = h.make('p1', 'emulator-5554', { label });
  const p2 = h.make('p2', 'sim:A', { label });
  await live(h, p1, 0);
  const c2 = await live(h, p2, 1);
  c2.opts.onClose({ code: 3, reason: 'shutDown' });
  assert.equal(p2.overlay.state(), 'shutDown');
  assert.deepEqual(asked, ['sim:A']);
});

await test('close is final', async () => {
  const h = harness();
  const pane = h.make('p1', 'emulator-5554');
  await live(h, pane);
  pane.close();
  h.calls[0].opts.onClose({});
  assert.equal(h.timers.length, 0, 'no timer queued');
  pane.connect();
  await tick();
  assert.equal(h.calls.length, 1, 'connect after close is a no-op');
  assert.equal(pane.send(new Uint8Array([1])), false);
});

// touch dispatches a press and a release on the pane's canvas.
function touch(pane, pointerId = 1) {
  const canvas = pane.els.canvas;
  canvas._rect = { left: 0, top: 0, width: 108, height: 240 };
  canvas.dispatch('pointerdown', { pointerId, clientX: 10, clientY: 20, pressure: 1 });
  canvas.dispatch('pointerup', { pointerId, clientX: 10, clientY: 20, pressure: 1 });
}
const MSG_TOUCH = 0x10;
const touchesSent = (conn) => conn.sent.filter((b) => new Uint8Array(b)[0] === MSG_TOUCH).length;

await test('pointer input after switchTarget goes to the new transport', async () => {
  const h = harness();
  const pane = h.make('p1', 'emulator-5554');
  const first = await live(h, pane);
  touch(pane);
  assert.equal(touchesSent(first.conn), 2, 'down and up reach the first transport');
  pane.switchTarget('sim:A');
  await h.runTimers();
  h.calls[1].opts.onControl(control(MSG_INIT, INIT));
  touch(pane, 2);
  assert.equal(first.conn.sent.length, 2, 'nothing more went to the old transport');
  assert.equal(touchesSent(h.calls[1].conn), 2, 'down and up reach the new transport');
});

await test("control headers use the latest init's viewer id", async () => {
  const h = harness();
  const pane = h.make('p1', 'emulator-5554');
  pane.connect();
  await tick();
  h.calls[0].opts.onControl(control(MSG_INIT, { ...INIT, viewer: 7 }));
  assert.equal(pane.viewerHeaders()['X-Loupe-Viewer'], '7');
  assert.equal(pane.hasControl(), true);
  pane.switchTarget('sim:A');
  assert.equal(pane.hasControl(), false, 'a switch drops the old session\'s control');
  await h.runTimers();
  h.calls[1].opts.onControl(control(MSG_INIT, { ...INIT, viewer: 9 }));
  assert.equal(pane.viewerHeaders()['X-Loupe-Viewer'], '9');
});

await test('switchTarget resets the per-pane modules', async () => {
  const h = harness();
  const log = [];
  const pane = h.make('p1', 'emulator-5554', { modules: fakeModules(log) });
  await live(h, pane);
  log.length = 0;
  pane.switchTarget('sim:A');
  for (const call of ['record.onTargetSwitch', 'flag.onTargetSwitch', 'inspect.off', 'touches.clear', 'cursor.clear', 'overlays.clear', 'control.clear']) {
    assert.equal(log.filter((c) => c === call).length, 1, call);
  }
  // reconnect's order: the recording stops first, then the overlays clear.
  assert.deepEqual(log, ['record.onTargetSwitch', 'flag.onTargetSwitch', 'inspect.off', 'touches.clear', 'cursor.clear', 'overlays.clear', 'control.clear']);
  assert.ok(pane.mods.record && pane.mods.eventlog, 'mods exposes each module');
});

await test('each pane mounts its own modules into its own host', async () => {
  const h = harness();
  const p1 = h.make('p1', 'emulator-5554');
  const p2 = h.make('p2', 'sim:A');
  const banner = (p) => p.els.root.querySelector('.controlBanner');
  assert.ok(banner(p1), 'p1 has a control banner under its root');
  assert.ok(banner(p2), 'p2 has a control banner under its root');
  assert.notEqual(banner(p1), banner(p2));
  assert.equal(fakeDocument.body.querySelector('.controlBanner'), null, 'none under body');
  const log = [];
  const p3 = h.make('p3', 'sim:B', { modules: fakeModules(log) });
  assert.equal(p3.mods.record.deps.host, p3.els.root, 'record mounts into the pane root');
  assert.equal(p3.mods.record.deps.targetId(), 'sim:B');
  assert.equal(p3.mods.touches.deps.canvas, p3.els.canvas);
});

if (failures) {
  console.log(`\n${failures} failed`);
  process.exit(1);
}
