// Agent cursors: an arrow with the agent's name, drawn where an agent's
// /api/input lands, the way a multiplayer design tool shows a collaborator.
// Always on, unlike touches.js's markers: those are a debugging aid a person
// opts into, and this is how they see that someone else is driving.
//
// One cursor per agent name. It travels like a mouse from where it last was
// to the next point -- on a slight arc, easing in and out, longer for a
// longer trip -- ripples on a tap, follows a swipe, and fades once the agent
// has been quiet for IDLE_MS. A move (MsgCursor, /api/input's move action)
// travels without rippling, so an agent can point before it taps. Positions
// are the event's fractions of the screen, so they land on the canvas at
// whatever size it is drawn.

// How long a cursor stays after its agent's last action.
export const IDLE_MS = 2500;
// The shortest and longest trip to a new point, before the action plays.
export const GLIDE_MIN_MS = 160;
export const GLIDE_MAX_MS = 650;
// The fade at the end; style.css's .agentCursor.gone matches it.
export const FADE_MS = 300;

// RGB triplets, so style.css can use them at any alpha. The first is the
// amber touches.js uses for agent touches, so one agent looks the same in
// both.
export const PALETTE = ['255, 176, 32', '139, 92, 246', '236, 72, 153', '59, 130, 246'];

// agentColor(name) -> one of PALETTE, the same for a name every time.
export function agentColor(name) {
  let h = 0;
  for (const ch of String(name || '')) h = (h * 31 + ch.codePointAt(0)) >>> 0;
  return PALETTE[h % PALETTE.length];
}

// agentLabel(name) -> what the pill says. X-Loupe-Agent allows 64 bytes,
// which is too wide for a label that sits over the screen.
export function agentLabel(name) {
  const s = String(name || '').trim();
  if (!s) return 'agent';
  return s.length > 24 ? `${s.slice(0, 23)}…` : s;
}

// glideMs(px) -> how long a trip of px pixels takes. It grows with the log
// of the distance, as a hand's does (Fitts), so a long trip is not slow and a
// short one is not a snap. Under 2px there is no trip: the cursor is there.
export function glideMs(px) {
  if (!(px >= 2)) return 0;
  const ms = GLIDE_MIN_MS + 90 * Math.log2(1 + px / 40);
  return Math.round(Math.min(ms, GLIDE_MAX_MS));
}

// easeInOut(u) -> a cubic ease for u in [0,1]: a hand speeds up, then
// settles on the target.
export function easeInOut(u) {
  return u < 0.5 ? 4 * u * u * u : 1 - (-2 * u + 2) ** 3 / 2;
}

// arcPoint(a, b, u) -> the point u of the way along a shallow curve from a to
// b. The control point sits off the midpoint by an eighth of the distance,
// always bowing upward, the way a wrist swings a mouse.
export function arcPoint(a, b, u) {
  const dx = b.x - a.x;
  const dy = b.y - a.y;
  const d = Math.hypot(dx, dy);
  if (d === 0) return { x: b.x, y: b.y };
  const side = dx >= 0 ? -1 : 1;
  const cx = (a.x + b.x) / 2 + side * (-dy / d) * d / 8;
  const cy = (a.y + b.y) / 2 + side * (dx / d) * d / 8;
  const v = 1 - u;
  return {
    x: v * v * a.x + 2 * v * u * cx + u * u * b.x,
    y: v * v * a.y + 2 * v * u * cy + u * u * b.y,
  };
}

function clampMs(ms, lo, hi) {
  const n = Number(ms);
  if (!Number.isFinite(n)) return lo;
  return Math.min(Math.max(n, lo), hi);
}

// cursorMove(ev) -> what the cursor does for a decoded MsgEvent, or null.
//
// Only agent input with a position moves a cursor. A viewer's input is a
// person's own finger, which touches.js shows, and a refused or failed
// action never reached the screen.
export function cursorMove(ev) {
  if (!ev || ev.source !== 'agent' || ev.err) return null;
  if (!Array.isArray(ev.from)) return null;
  const name = ev.agent || '';
  const from = ev.from;
  switch (ev.kind) {
    case 'move':
      return { name, kind: 'move', from };
    case 'tap':
      return { name, kind: 'tap', from };
    case 'longPress':
      return { name, kind: 'hold', from, ms: clampMs(ev.ms, 300, 1200) };
    case 'swipe':
      if (!Array.isArray(ev.to)) return null;
      return { name, kind: 'swipe', from, to: ev.to, ms: clampMs(ev.ms, 150, 900) };
    case 'pinch':
      return { name, kind: 'tap', from };
    default:
      return null;
  }
}

// The arrow's tip is at the element's origin, so placing the element places
// the tip.
const ARROW = '<svg class="agentArrow" width="18" height="22" viewBox="0 0 18 22" aria-hidden="true">'
  + '<path d="M1.5 1.5 L16 12.2 L9.4 13.2 L6 20.2 Z" /></svg>';

// mount(layer, deps) -> {onEvent(ev), clear()}
//
// deps:
//   canvas -- the stream's canvas; its box is where cursors are placed.
//   setTimeout, clearTimeout, raf, now -- default to the window's.
//   reducedMotion -- () -> true to jump instead of travel; defaults to the
//     prefers-reduced-motion query. style.css's own reduced-motion rule
//     cannot reach this, since the travel is drawn here, not by a transition.
export function mount(layer, deps) {
  const doc = layer.ownerDocument || globalThis.document;
  const later = deps.setTimeout || ((fn, ms) => setTimeout(fn, ms));
  const cancel = deps.clearTimeout || ((id) => clearTimeout(id));
  const raf = deps.raf || ((fn) => requestAnimationFrame(fn));
  const now = deps.now || (() => performance.now());
  const reduced = deps.reducedMotion
    || (() => !!globalThis.matchMedia?.('(prefers-reduced-motion: reduce)').matches);
  const cursors = new Map();

  function canvasBox() {
    const c = deps.canvas.getBoundingClientRect();
    const l = layer.getBoundingClientRect();
    return { left: c.left - l.left, top: c.top - l.top, width: c.width, height: c.height };
  }

  function at(f, box) {
    return { x: box.left + f[0] * box.width, y: box.top + f[1] * box.height };
  }

  function put(c, p) {
    c.pos = p;
    c.el.style.transform = `translate(${Math.round(p.x)}px, ${Math.round(p.y)}px)`;
  }

  // travel(c, to, ms, arc) moves c to `to` over ms, starting from wherever
  // it is now -- mid-trip included, so a tap that lands while a move is
  // still travelling carries on from there rather than jumping back. arc
  // is a hand's trip to a point; without it the path is straight and even,
  // a finger's swipe.
  function travel(c, to, ms, arc) {
    const trip = ++c.trip;
    if (ms <= 0 || reduced()) {
      put(c, to);
      return;
    }
    const from = c.pos;
    const t0 = now();
    const step = () => {
      if (c.trip !== trip) return;
      const u = Math.min(1, (now() - t0) / ms);
      put(c, arc ? arcPoint(from, to, easeInOut(u))
        : { x: from.x + (to.x - from.x) * u, y: from.y + (to.y - from.y) * u });
      if (u < 1) raf(step);
    };
    raf(step);
  }

  function cursorFor(name, p) {
    let c = cursors.get(name);
    if (c) {
      cancel(c.timer);
      c.el.classList.remove('gone');
      return { c, fresh: false };
    }
    const el = doc.createElement('div');
    el.className = 'agentCursor';
    el.style.setProperty('--cursor', agentColor(name));
    el.style.transition = `opacity ${FADE_MS}ms`;
    el.innerHTML = ARROW;
    const pill = doc.createElement('span');
    pill.className = 'agentName';
    pill.textContent = agentLabel(name);
    el.appendChild(pill);
    c = { el, timer: 0, trip: 0, pos: p, ripple: 0 };
    cursors.set(name, c);
    put(c, p);
    layer.appendChild(el);
    return { c, fresh: true };
  }

  function ripple(c) {
    const r = doc.createElement('span');
    r.className = 'agentRipple';
    c.el.appendChild(r);
    later(() => r.remove(), 600);
  }

  function idle(name, c, afterMs) {
    c.timer = later(() => {
      c.el.classList.add('gone');
      c.timer = later(() => {
        c.el.remove();
        cursors.delete(name);
      }, FADE_MS);
    }, afterMs + IDLE_MS);
  }

  function clear() {
    for (const c of cursors.values()) {
      cancel(c.timer);
      cancel(c.ripple);
      c.trip++;
      c.el.remove();
    }
    cursors.clear();
  }

  return {
    // Live events and cursor moves only, as touches.js: the /api/events
    // catch-up is history.
    onEvent(ev) {
      const m = cursorMove(ev);
      if (!m) return;
      const box = canvasBox();
      const from = at(m.from, box);
      const { c, fresh } = cursorFor(m.name, from);
      // A cursor already on screen travels to the new point first; a new
      // one appears there. A tap on the point a move already reached plays
      // at once.
      const glide = fresh || reduced() ? 0 : glideMs(Math.hypot(from.x - c.pos.x, from.y - c.pos.y));
      travel(c, from, glide, true);
      // An action due at the end of an earlier trip would play at the wrong
      // place once this one starts.
      cancel(c.ripple);
      switch (m.kind) {
        case 'move':
          idle(m.name, c, glide);
          return;
        case 'tap':
          c.ripple = later(() => ripple(c), glide);
          idle(m.name, c, glide);
          return;
        case 'hold':
          c.ripple = later(() => ripple(c), glide);
          idle(m.name, c, glide + m.ms);
          return;
        case 'swipe':
          c.ripple = later(() => {
            ripple(c);
            travel(c, at(m.to, canvasBox()), m.ms, false);
          }, glide);
          idle(m.name, c, glide + m.ms);
          return;
      }
    },
    clear,
  };
}
