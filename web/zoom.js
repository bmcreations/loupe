// Zoom mode (Mac VideoView.swift:128-168, StageView.swift:109-124).
//
// Entered from the More menu. The focused pane's screen is scaled and panned
// with a CSS transform on its .screenWrap: one finger pans, two pinch, a
// double tap toggles fit and 2x. While the mode is on no touch on the pane
// reaches the device: the stage's capture-phase listeners take every pointer
// and wheel event aimed at the zoomed pane and stop it there, before pane.js's
// canvas handlers, the touch markers or the immersive double tap see it. The
// device frame is dropped while zoomed (app.js layoutScreen reads the
// .zoomed class on the wrap), so the wrap is the canvas alone.
//
// The state is {scale, x, y}: the picture is drawn as
// translate(x, y) scale(scale) about the wrap's top-left corner, with w x h
// the wrap's unscaled size. The helpers are pure; mount() owns the DOM.

export const MIN_SCALE = 1;
export const MAX_SCALE = 5;
export const FIT = Object.freeze({ scale: 1, x: 0, y: 0 });

// clampScale holds a scale to 1..5. Anything that is not a number is fit.
export function clampScale(s) {
  if (!Number.isFinite(s)) return MIN_SCALE;
  return Math.min(MAX_SCALE, Math.max(MIN_SCALE, s));
}

// clampPan moves the picture back until it covers the w x h viewport.
export function clampPan(state, w, h) {
  const s = clampScale(state.scale);
  const x = Math.min(0, Math.max(w * (1 - s), state.x));
  const y = Math.min(0, Math.max(h * (1 - s), state.y));
  // -0 would print as "-0px" in the transform; keep the state clean.
  return { scale: s, x: x + 0, y: y + 0 };
}

// zoomAbout sets the scale so the content point under the viewport point
// (cx, cy) stays under it, then keeps the picture covering the viewport.
export function zoomAbout(state, scale, cx, cy, w, h) {
  const next = clampScale(scale);
  const px = (cx - state.x) / state.scale;
  const py = (cy - state.y) / state.scale;
  return clampPan({ scale: next, x: cx - px * next, y: cy - py * next }, w, h);
}

// panBy drags the picture by (dx, dy) viewport pixels, stopping at its edges.
export function panBy(state, dx, dy, w, h) {
  return clampPan({ scale: state.scale, x: state.x + dx, y: state.y + dy }, w, h);
}

// toggleAbout is the double tap: fit goes to 2x about the tapped point, and a
// zoomed picture goes back to fit.
export function toggleAbout(state, cx, cy, w, h) {
  if (state.scale > MIN_SCALE) return { ...FIT };
  return zoomAbout(state, 2, cx, cy, w, h);
}

// toDevice maps a viewport point back through the transform: the point of the
// unscaled picture that is drawn under (x, y).
export function toDevice(state, x, y) {
  return { x: (x - state.x) / state.scale, y: (y - state.y) / state.scale };
}

// badgeText is the capsule's label, a whole-number percentage.
export function badgeText(scale) {
  return `${Math.round(scale * 100)}%`;
}

const TAP_MS = 300;
const TAP_SLOP = 10;
const DOUBLE_SLOP = 30;
const WHEEL_ZOOM = 0.01;

// mount(deps) owns the mode.
//
// deps:
//   doc, root (the <html>, which gets .zoomed), stage (the element the
//     capture listeners and the badge go on).
//   pane() -> the focused pane ({els: {wrap, canvas}, targetId}) or null.
//   relayout() -> app.js's fitScreen; called after the mode flips, because
//     the frame drops out of the fit while zoomed.
//   onChange(active) -> optional; the page's own bookkeeping.
//
// Returns {enter(), exit(), sync(), active}. sync() is called from fitScreen
// and on a focus change: it leaves zoom mode when the pane is no longer the
// focused one, was reshaped (a rotation) or moved to another target, and
// otherwise re-clamps the picture to the wrap's possibly new size.
export function mount(deps) {
  const doc = deps.doc;
  let active = false;
  let pane = null;
  let shape = '';
  let targetId = null;
  let state = { ...FIT };
  let badge = null;
  const pointers = new Map(); // pointerId -> {x, y}
  let lastTap = null; // {t, x, y}
  let downAt = null; // {id, t, x, y, moved}

  const shapeOf = (p) => `${p.els.canvas.width}x${p.els.canvas.height}`;

  function size() {
    const w = pane.els.wrap;
    return { w: w.offsetWidth || w.clientWidth || 0, h: w.offsetHeight || w.clientHeight || 0 };
  }

  // The wrap's untransformed top-left in client coordinates: its rect moves
  // with the transform (origin 0 0), so take the translation back out.
  function origin() {
    const r = pane.els.wrap.getBoundingClientRect();
    return { left: r.left - state.x, top: r.top - state.y };
  }

  function apply() {
    const wrap = pane.els.wrap;
    wrap.style.transformOrigin = '0 0';
    wrap.style.transform = state.scale === 1 && state.x === 0 && state.y === 0
      ? ''
      : `translate(${state.x}px, ${state.y}px) scale(${state.scale})`;
    // The picture grows past the box it was fitted to; clip it back to that
    // box. clip-path lives in the wrap's own (scaled) coordinates, so the
    // box is mapped in through the transform.
    const { w, h } = size();
    const s = state.scale;
    wrap.style.clipPath = s === 1 || !w || !h ? ''
      : `inset(${-state.y / s}px ${w - (w - state.x) / s}px ${h - (h - state.y) / s}px ${-state.x / s}px)`;
    if (badge) badge.textContent = badgeText(state.scale);
  }

  function set(next) {
    state = next;
    apply();
  }

  function inPane(target) {
    const wrap = pane && pane.els.wrap;
    return !!(wrap && target && (target === wrap || (wrap.contains && wrap.contains(target))));
  }

  function takeEvent(ev) {
    ev.stopPropagation();
    if (ev.preventDefault) ev.preventDefault();
  }

  function onDown(ev) {
    if (!active || !inPane(ev.target)) return;
    takeEvent(ev);
    pointers.set(ev.pointerId, { x: ev.clientX, y: ev.clientY });
    downAt = pointers.size === 1
      ? { id: ev.pointerId, t: Date.now(), x: ev.clientX, y: ev.clientY, moved: false }
      : null;
    if (pointers.size > 1) lastTap = null;
  }

  function onMove(ev) {
    if (!active) return;
    const prev = pointers.get(ev.pointerId);
    if (!prev) return;
    takeEvent(ev);
    const { w, h } = size();
    if (pointers.size >= 2) {
      const [a, b] = Array.from(pointers.values());
      const before = { a: { ...a }, b: { ...b } };
      pointers.set(ev.pointerId, { x: ev.clientX, y: ev.clientY });
      const [a2, b2] = Array.from(pointers.values());
      const dBefore = Math.hypot(before.a.x - before.b.x, before.a.y - before.b.y);
      const dAfter = Math.hypot(a2.x - b2.x, a2.y - b2.y);
      const o = origin();
      const midBefore = { x: (before.a.x + before.b.x) / 2 - o.left, y: (before.a.y + before.b.y) / 2 - o.top };
      const midAfter = { x: (a2.x + b2.x) / 2 - o.left, y: (a2.y + b2.y) / 2 - o.top };
      let next = state;
      if (dBefore > 0) next = zoomAbout(next, next.scale * dAfter / dBefore, midBefore.x, midBefore.y, w, h);
      next = panBy(next, midAfter.x - midBefore.x, midAfter.y - midBefore.y, w, h);
      set(next);
      return;
    }
    pointers.set(ev.pointerId, { x: ev.clientX, y: ev.clientY });
    if (downAt && downAt.id === ev.pointerId && Math.hypot(ev.clientX - downAt.x, ev.clientY - downAt.y) > TAP_SLOP) {
      downAt.moved = true;
    }
    set(panBy(state, ev.clientX - prev.x, ev.clientY - prev.y, w, h));
  }

  function onUp(ev) {
    if (!active) return;
    if (!pointers.has(ev.pointerId)) return;
    takeEvent(ev);
    pointers.delete(ev.pointerId);
    const tap = downAt && downAt.id === ev.pointerId && !downAt.moved && ev.type === 'pointerup'
      && Date.now() - downAt.t <= TAP_MS;
    if (!pointers.size) downAt = null;
    if (!tap) return;
    const now = Date.now();
    if (lastTap && now - lastTap.t <= TAP_MS && Math.hypot(ev.clientX - lastTap.x, ev.clientY - lastTap.y) <= DOUBLE_SLOP) {
      lastTap = null;
      const o = origin();
      const { w, h } = size();
      set(toggleAbout(state, ev.clientX - o.left, ev.clientY - o.top, w, h));
    } else {
      lastTap = { t: now, x: ev.clientX, y: ev.clientY };
    }
  }

  // The wheel would scroll the device: here it pans, and with ctrl (a
  // trackpad pinch) it zooms.
  function onWheel(ev) {
    if (!active || !inPane(ev.target)) return;
    takeEvent(ev);
    const { w, h } = size();
    if (ev.ctrlKey) {
      const o = origin();
      set(zoomAbout(state, state.scale * Math.exp(-ev.deltaY * WHEEL_ZOOM * 3), ev.clientX - o.left, ev.clientY - o.top, w, h));
    } else {
      set(panBy(state, -ev.deltaX, -ev.deltaY, w, h));
    }
  }

  for (const [type, fn] of [['pointerdown', onDown], ['pointermove', onMove], ['pointerup', onUp], ['pointercancel', onUp]]) {
    deps.stage.addEventListener(type, fn, true);
  }
  deps.stage.addEventListener('wheel', onWheel, { capture: true, passive: false });

  function enter() {
    if (active) return;
    const p = deps.pane();
    if (!p) return;
    pane = p;
    shape = shapeOf(p);
    targetId = p.targetId;
    state = { ...FIT };
    pointers.clear();
    lastTap = null;
    downAt = null;
    active = true;
    deps.root.classList.add('zoomed');
    p.els.wrap.classList.add('zoomed');
    badge = doc.createElement('button');
    badge.type = 'button';
    badge.className = 'zoomBadge';
    badge.setAttribute('aria-label', 'Exit zoom');
    badge.textContent = badgeText(1);
    badge.addEventListener('click', () => exit());
    deps.stage.appendChild(badge);
    deps.relayout();
    apply();
    if (deps.onChange) deps.onChange(true);
  }

  function exit() {
    if (!active) return;
    active = false;
    const p = pane;
    pane = null;
    pointers.clear();
    deps.root.classList.remove('zoomed');
    if (p) {
      p.els.wrap.classList.remove('zoomed');
      p.els.wrap.style.transform = '';
      p.els.wrap.style.transformOrigin = '';
      p.els.wrap.style.clipPath = '';
    }
    if (badge) { if (badge.parentNode) badge.parentNode.removeChild(badge); badge = null; }
    deps.relayout();
    if (deps.onChange) deps.onChange(false);
  }

  function sync() {
    if (!active) return;
    if (deps.pane() !== pane || shapeOf(pane) !== shape || pane.targetId !== targetId) { exit(); return; }
    const { w, h } = size();
    if (w && h) set(clampPan(state, w, h));
  }

  return { enter, exit, sync, get active() { return active; } };
}
