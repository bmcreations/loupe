// Touch markers drawn over the stream: a dot under each of this phone's
// fingers while it is down, and a replay of every tap, hold, swipe and pinch
// another viewer or an agent sends. Off by default; the overflow menu's
// "Show touches" switch turns it on, and the choice is kept per phone.
//
// Drawn in the page, not on the device. Android's own "Show taps" paints into
// the framebuffer, so it would reach every viewer and every agent screenshot
// and outlive a session that died before switching it back; an iOS simulator
// has no such setting at all. A screen recording of this phone captures the
// page, which is what the markers are for.
//
// This phone's own touches come from its pointer events, so they carry no
// delay. Everyone else's arrive as MsgEvent once the gesture has finished,
// and are replayed from its from/to/ms, so they are an account of what
// happened rather than a live trail. Every viewer is sent every event, its
// own included; init's `viewer` names this one so its own taps are not drawn
// twice.
//
// remoteMark, clampMs and readPref/writePref have no DOM in them and are
// pinned by touches.test.mjs; mount() takes the layer and the canvas and
// is driven there against fakedom.js.

const KEY = 'loupe.showTouches';

export function readPref(storage) {
  try {
    return storage.getItem(KEY) === '1';
  } catch {
    return false;
  }
}

export function writePref(storage, on) {
  try {
    storage.setItem(KEY, on ? '1' : '0');
  } catch {
    // ignored: the preference just does not stick this session
  }
}

export function clampMs(ms, lo, hi) {
  const n = Number(ms);
  if (!Number.isFinite(n) || n <= 0) return lo;
  return Math.min(Math.max(n, lo), hi);
}

// remoteMark(ev, self) -> what to draw for a decoded MsgEvent, or null.
//
// self is init's `viewer`, 0 when the server did not send one. An event
// with no position -- an agent tap on a target whose screen size could not
// be read publishes none -- draws nothing rather than a guess.
export function remoteMark(ev, self) {
  if (!ev || !Array.isArray(ev.from)) return null;
  if (ev.err) return null;
  if (ev.source === 'viewer' && self && ev.viewer === self) return null;
  const who = ev.source === 'agent' ? 'agent' : 'viewer';
  const from = ev.from;
  switch (ev.kind) {
    case 'tap':
      return { kind: 'tap', who, from };
    case 'longPress':
      return { kind: 'hold', who, from, ms: clampMs(ev.ms, 300, 1200) };
    case 'swipe':
      if (!Array.isArray(ev.to)) return null;
      return { kind: 'swipe', who, from, to: ev.to, ms: clampMs(ev.ms, 150, 900) };
    case 'pinch':
      if (!(ev.scale > 0)) return null;
      return { kind: 'pinch', who, from, scale: ev.scale, ms: clampMs(ev.ms, 250, 900) };
    default:
      return null;
  }
}

// How long a lifted dot takes to fade; style.css's touch-lift matches it.
export const LIFT_MS = 450;

// mount(layer, deps) -> {pref(), setPref(on), onInit(info), onEvent(ev), clear()}
//
// deps:
//   canvas -- the stream's canvas. Its pointer events are this phone's
//     touches, and its box is where every marker is placed.
//   storage -- localStorage or null (handedness.js's safeStorage).
//   report(obj) -- client telemetry.
//   setTimeout, raf -- default to the window's; a test passes its own.
export function mount(layer, deps) {
  const doc = layer.ownerDocument || globalThis.document;
  const later = deps.setTimeout || ((fn, ms) => setTimeout(fn, ms));
  const raf = deps.raf || ((fn) => requestAnimationFrame(fn));
  let on = readPref(deps.storage);
  let self = 0;
  const fingers = new Map();

  layer.hidden = !on;

  // The canvas's box in the layer's own frame. Both rects are
  // viewport-absolute, and the layer covers the whole bezel when a frame is
  // shown, so the canvas's offset inside it is the difference of the two.
  function canvasBox() {
    const c = deps.canvas.getBoundingClientRect();
    const l = layer.getBoundingClientRect();
    return { left: c.left - l.left, top: c.top - l.top, width: c.width, height: c.height };
  }

  function atFraction(f, box) {
    return { x: box.left + f[0] * box.width, y: box.top + f[1] * box.height };
  }

  function atPointer(ev, box) {
    const l = layer.getBoundingClientRect();
    const x = Math.min(Math.max(ev.clientX - l.left, box.left), box.left + box.width);
    const y = Math.min(Math.max(ev.clientY - l.top, box.top), box.top + box.height);
    return { x, y };
  }

  function place(dot, p) {
    dot.style.transform = `translate(${Math.round(p.x)}px, ${Math.round(p.y)}px)`;
  }

  function dot(who, p) {
    const d = doc.createElement('div');
    d.className = `touchDot ${who}`;
    place(d, p);
    layer.appendChild(d);
    return d;
  }

  function lift(d, afterMs = 0) {
    later(() => {
      d.classList.add('lift');
      later(() => d.remove(), LIFT_MS);
    }, afterMs);
  }

  function travel(d, to, ms) {
    raf(() => {
      d.style.transition = `transform ${ms}ms cubic-bezier(0.2, 0.7, 0.3, 1)`;
      place(d, to);
    });
  }

  function clear() {
    fingers.clear();
    for (const c of Array.from(layer.children)) c.remove();
  }

  deps.canvas.addEventListener('pointerdown', (ev) => {
    if (!on) return;
    const old = fingers.get(ev.pointerId);
    if (old) old.remove();
    fingers.set(ev.pointerId, dot('self', atPointer(ev, canvasBox())));
  });
  deps.canvas.addEventListener('pointermove', (ev) => {
    const d = fingers.get(ev.pointerId);
    if (d) place(d, atPointer(ev, canvasBox()));
  });
  const end = (ev) => {
    const d = fingers.get(ev.pointerId);
    if (!d) return;
    fingers.delete(ev.pointerId);
    lift(d);
  };
  deps.canvas.addEventListener('pointerup', end);
  deps.canvas.addEventListener('pointercancel', end);

  function draw(m) {
    const box = canvasBox();
    const from = atFraction(m.from, box);
    switch (m.kind) {
      case 'tap':
        lift(dot(m.who, from));
        return;
      case 'hold':
        lift(dot(m.who, from), m.ms);
        return;
      case 'swipe': {
        const d = dot(m.who, from);
        travel(d, atFraction(m.to, box), m.ms);
        lift(d, m.ms);
        return;
      }
      case 'pinch': {
        // Two fingers on a diagonal through the centre, spreading for a
        // scale above 1 and closing below it. The event says how far the
        // gesture zoomed, not where each finger went, so the spread is
        // drawn at the scale the server ran it at (agent.go's pinch).
        const short = Math.min(box.width, box.height);
        const near = short * 0.12;
        const far = near * Math.min(Math.max(m.scale, 1 / m.scale), 3);
        const [r0, r1] = m.scale >= 1 ? [near, far] : [far, near];
        for (const s of [-1, 1]) {
          const at = (r) => ({ x: from.x + s * r * Math.SQRT1_2, y: from.y - s * r * Math.SQRT1_2 });
          const d = dot(m.who, at(r0));
          travel(d, at(r1), m.ms);
          lift(d, m.ms);
        }
        return;
      }
    }
  }

  return {
    pref: () => on,
    setPref(next) {
      on = !!next;
      writePref(deps.storage, on);
      layer.hidden = !on;
      if (!on) clear();
      if (deps.report) deps.report({ event: 'touches-pref', on });
    },
    onInit(info) {
      self = (info && info.viewer) || 0;
    },
    // Live events only. The /api/events catch-up is history, and replaying
    // a burst of it would draw minutes-old taps as if they were happening.
    onEvent(ev) {
      if (!on) return;
      const m = remoteMark(ev, self);
      if (m) draw(m);
    },
    clear,
  };
}
