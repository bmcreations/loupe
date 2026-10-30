// Tests for agent cursors' pure parts. Run: node web/cursor.test.mjs
import assert from 'node:assert/strict';

const { cursorMove, agentColor, agentLabel, PALETTE, glideMs, easeInOut, arcPoint, GLIDE_MIN_MS, GLIDE_MAX_MS, IDLE_MS, PARK_MS, FADE_MS, mount } = await import('./cursor.js');

let failures = 0;
function test(name, fn) {
  try { fn(); console.log(`  PASS ${name}`); }
  catch (err) { failures++; console.log(`  FAIL ${name}\n        ${err.message}`); }
}

test('an agent tap moves that agent\'s cursor', () => {
  assert.deepEqual(cursorMove({ source: 'agent', agent: 'claude', kind: 'tap', from: [0.5, 0.25] }),
    { name: 'claude', kind: 'tap', from: [0.5, 0.25] });
});

test('a viewer\'s input draws no cursor', () => {
  assert.equal(cursorMove({ source: 'viewer', viewer: 3, kind: 'tap', from: [0.5, 0.5] }), null);
});

test('a refused or failed action draws no cursor', () => {
  assert.equal(cursorMove({ source: 'agent', kind: 'tap', from: [0.5, 0.5], err: 'humanHeld' }), null);
});

test('input with no position draws no cursor', () => {
  assert.equal(cursorMove({ source: 'agent', kind: 'text' }), null);
  assert.equal(cursorMove({ source: 'agent', kind: 'key', from: null }), null);
});

test('a swipe carries its end point and a clamped duration', () => {
  const m = cursorMove({ source: 'agent', agent: 'a', kind: 'swipe', from: [0, 0], to: [1, 1], ms: 5000 });
  assert.deepEqual(m, { name: 'a', kind: 'swipe', from: [0, 0], to: [1, 1], ms: 900 });
  assert.equal(cursorMove({ source: 'agent', kind: 'swipe', from: [0, 0] }), null);
});

test('a long press holds for a clamped time', () => {
  assert.equal(cursorMove({ source: 'agent', kind: 'longPress', from: [0, 0], ms: 50 }).ms, 300);
});

test('an agent with no name is still drawn', () => {
  const m = cursorMove({ source: 'agent', kind: 'tap', from: [0, 0] });
  assert.equal(m.name, '');
  assert.equal(agentLabel(m.name), 'agent');
});

test('a name always gets the same colour from the palette', () => {
  assert.equal(agentColor('claude'), agentColor('claude'));
  assert.ok(PALETTE.includes(agentColor('claude')));
  assert.ok(PALETTE.includes(agentColor('')));
});

test('a long name is shortened for the label', () => {
  const label = agentLabel('x'.repeat(64));
  assert.equal(label.length, 24);
  assert.ok(label.endsWith('…'));
  assert.equal(agentLabel('  claude  '), 'claude');
});

test('an agent move points the cursor without an action', () => {
  assert.deepEqual(cursorMove({ source: 'agent', agent: 'claude', kind: 'move', from: [0.2, 0.8] }),
    { name: 'claude', kind: 'move', from: [0.2, 0.8] });
});

test('a longer trip takes longer, within bounds', () => {
  assert.equal(glideMs(0), 0);
  assert.equal(glideMs(1), 0);
  assert.ok(glideMs(10) >= GLIDE_MIN_MS);
  assert.ok(glideMs(100) < glideMs(400));
  assert.equal(glideMs(1e6), GLIDE_MAX_MS);
});

test('the ease starts and ends at rest', () => {
  assert.equal(easeInOut(0), 0);
  assert.equal(easeInOut(1), 1);
  assert.equal(easeInOut(0.5), 0.5);
  assert.ok(easeInOut(0.1) < 0.1);
});

test('the arc starts and ends on its points and bows off the line', () => {
  const a = { x: 0, y: 100 }, b = { x: 200, y: 100 };
  assert.deepEqual(arcPoint(a, b, 0), a);
  assert.deepEqual(arcPoint(a, b, 1), b);
  const mid = arcPoint(a, b, 0.5);
  assert.equal(mid.x, 100);
  assert.ok(mid.y < 100, `mid.y ${mid.y} should bow upward`);
  assert.deepEqual(arcPoint(a, a, 0.5), a);
});

// A layer and canvas at the origin, 1000px square, with a clock and frame
// queue the test steps by hand.
function rig() {
  let t = 0;
  let frames = [];
  const timers = [];
  const el = () => { const e = ({
    className: '', style: { setProperty() {} }, innerHTML: '', children: [],
    classes: new Set(),
    classList: {
      add(...c) { c.forEach((x) => this.owner.classes.add(x)); },
      remove(...c) { c.forEach((x) => this.owner.classes.delete(x)); },
    },
    appendChild(c) { this.children.push(c); }, remove() { this.removed = true; },
  }); e.classList.owner = e; return e; };
  const layer = Object.assign(el(), {
    ownerDocument: { createElement: el },
    getBoundingClientRect: () => ({ left: 0, top: 0, width: 1000, height: 1000 }),
  });
  const canvas = { getBoundingClientRect: () => ({ left: 0, top: 0, width: 1000, height: 1000 }) };
  const m = mount(layer, {
    canvas,
    now: () => t,
    raf: (fn) => frames.push(fn),
    setTimeout: (fn, ms) => timers.push({ fn, at: t + ms }) ,
    clearTimeout() {},
    reducedMotion: () => false,
  });
  const cursor = () => layer.children[0];
  const pos = () => cursor().style.transform.match(/-?\d+/g).map(Number);
  const advance = (ms) => {
    t += ms;
    const run = frames; frames = [];
    run.forEach((f) => f());
  };
  // fire runs every timer due by the clock, in order, including ones
  // they set.
  const fire = (ms) => {
    const end = t + ms;
    for (;;) {
      const due = timers.filter((x) => !x.done && x.at <= end).sort((a, b) => a.at - b.at)[0];
      if (!due) break;
      t = Math.max(t, due.at);
      due.done = true;
      due.fn();
    }
    t = end;
  };
  return { m, pos, advance, timers, cursor, fire };
}

test('a move travels the cursor over time and lands on the point', () => {
  const r = rig();
  r.m.onEvent({ source: 'agent', agent: 'a', kind: 'move', from: [0.1, 0.1] });
  assert.deepEqual(r.pos(), [100, 100]);
  r.m.onEvent({ source: 'agent', agent: 'a', kind: 'move', from: [0.9, 0.9] });
  r.advance(50);
  const [x] = r.pos();
  assert.ok(x > 100 && x < 900, `mid-trip x ${x}`);
  for (let i = 0; i < 40; i++) r.advance(50);
  assert.deepEqual(r.pos(), [900, 900]);
});

test('a move does not ripple; a tap on the same point ripples at once', () => {
  const r = rig();
  r.m.onEvent({ source: 'agent', agent: 'a', kind: 'move', from: [0.5, 0.5] });
  assert.equal(r.timers.filter((x) => x.at === 0).length, 0);
  r.m.onEvent({ source: 'agent', agent: 'a', kind: 'tap', from: [0.5, 0.5] });
  const now = r.timers.filter((x) => x.at === 0);
  assert.equal(now.length, 1, 'the ripple is due with no glide');
  now[0].fn();
  assert.ok(r.cursor().children.some((c) => c.className === 'agentRipple'));
});

test('a quiet cursor parks, then fades only after PARK_MS', () => {
  const r = rig();
  r.m.onEvent({ source: 'agent', agent: 'a', kind: 'tap', from: [0.5, 0.5] });
  r.fire(IDLE_MS);
  assert.ok(r.cursor().classes.has('parked'), 'parked after IDLE_MS');
  assert.ok(!r.cursor().classes.has('gone'));
  r.fire(PARK_MS - 1);
  assert.ok(!r.cursor().classes.has('gone'), 'still parked just before PARK_MS');
  r.fire(1 + FADE_MS);
  assert.ok(r.cursor().classes.has('gone') && r.cursor().removed, 'faded and removed');
});

test('an action wakes a parked cursor where it is', () => {
  const r = rig();
  r.m.onEvent({ source: 'agent', agent: 'a', kind: 'tap', from: [0.5, 0.5] });
  r.fire(IDLE_MS + 5000);
  assert.ok(r.cursor().classes.has('parked'));
  r.m.onEvent({ source: 'agent', agent: 'a', kind: 'move', from: [0.2, 0.2] });
  assert.ok(!r.cursor().classes.has('parked'), 'lit again');
});

if (failures) { console.log(`\n${failures} failing`); process.exit(1); }
console.log('\nall passing');
