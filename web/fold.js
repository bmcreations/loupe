// Fold button: a fourth button at the inner end of the nav row that sets a foldable's
// posture without opening the Tools sheet.
//
// It is the same `posture` setting the sheet shows (internal/target/
// posture.go), read with GET /api/settings and written with PUT, so the two
// never disagree about what the device is in. The button exists only while
// the target's settings list a supported posture: a phone that cannot fold
// gets no row, and the stack keeps its three-button height.
//
// A foldable emulator offers closed, half open and open. An iPhone Duo
// offers closed and open, folded by simcap the way Device Hub's hinge slider
// does (internal/target/simposture.go). Any other simulator answers
// /api/settings with 501.

import { showToast } from './toast.js';

const LABELS = { closed: 'Closed', halfOpened: 'Half open', opened: 'Open' };

// postureOf(body) -> {value, options} for a supported posture setting in a
// GET or PUT /api/settings body, or null when there is none to drive.
export function postureOf(body) {
  const list = body && Array.isArray(body.settings) ? body.settings : [];
  const s = list.find((x) => x && x.id === 'posture');
  if (!s || !s.supported || !Array.isArray(s.options) || s.options.length < 2) return null;
  return { value: typeof s.value === 'string' ? s.value : '', options: s.options };
}

export function postureLabel(value) {
  return LABELS[value] || value;
}

// Icons: a phone shut, bent and flat, 24x24 to match .cornerBtn svg.
const ICONS = {
  closed: '<rect x="8" y="3" width="8" height="18" rx="2"/>',
  halfOpened: '<path d="M4 18 L12 12 L12 3"/><path d="M12 12 L20 18"/>',
  opened: '<rect x="3" y="5" width="18" height="14" rx="2"/><path d="M12 5v14"/>',
};
// foldMotion(from, to) says how the bezel moves for a posture change.
//
// The simulator and the emulator draw nothing between postures: a hinge
// sweep on loupe-duo kept the inner panel lit and unchanged down to 10
// degrees and switched panels at 0, so the stream has no fold to show.
// Device Hub's fold is its own 3D model, and this is the same idea at the
// scale of the page.
//
// `flip`: the lit panel changes (closed against anything else), so the
// page folds the inner panel shut at its hinge, or open, around the wait
// for the other panel's picture (see hingeBook). `tilt`: open and half open
// share a panel, so the box only leans and comes back. `dir` is which way.
export function foldMotion(from, to) {
  if (!from || !to || from === to) return null;
  if (from === 'closed' || to === 'closed') {
    return { kind: 'flip', dir: to === 'closed' ? 'closing' : 'opening' };
  }
  return { kind: 'tilt', dir: to === 'opened' ? 'opening' : 'closing' };
}

// The longest the fold waits after the PUT has answered. The PUT returns
// once the lit panel has followed, and the new panel's first frame lands a
// few frames later; this only covers a stream that never reshapes.
const SETTLE_MS = 1200;

const FOLD_MS = 650;
const REVEAL_MS = 240;
const FOLD_EASE = 'cubic-bezier(.45,0,.2,1)';

// slabTransform(book, to) is the CSS transform that takes a book laid over
// rect `book`, with its left leaf shut, and puts the shut half on rect `to`.
// Shut, the leaves cover the book's right half; translateX(-w/4) centres them,
// then the scale and translate match `to`'s height and centre. The function
// list is the same at every value so the browser interpolates between them.
export function slabTransform(book, to) {
  const s = to.height / book.height;
  const dx = to.left + to.width / 2 - (book.left + book.width / 2);
  const dy = to.top + to.height / 2 - (book.top + book.height / 2);
  return `translate(${round(dx)}px, ${round(dy)}px) scale(${round(s, 4)}) translateX(${round(-book.width / 4)}px)`;
}
export const FLAT = 'translate(0px, 0px) scale(1) translateX(0px)';

function round(v, places = 2) {
  const k = 10 ** places;
  return Math.round(v * k) / k;
}

// hingeBook(canvas, stage, wrap) draws a flip as the phone folding at its
// hinge: the inner panel's picture split down the middle, the left half
// turning about the centre line onto the right, with the outer panel on its
// back, so the fold lands with the outer panel facing out. With a device
// frame shown the halves carry the bezel too, and the book is the frame box
// rather than the canvas.
//
// close(): the inner panel is on the canvas now. Fold it shut and hold.
// open():  the outer panel is on the canvas now. Hold its picture.
// arrive(): the other panel's picture has replaced it. Closing, the shut
//   half takes on that picture and moves onto the canvas rect; opening,
//   the book unfolds from the outer panel to the inner one. Resolves when
//   the book matches what the canvas shows, so the box can come back.
// leave(): fade the book out and remove it, from any state.
//
// Every panel the hinge has been tried on splits across its width: the
// Duo's inner panel streams upright as 2853x2007, and a foldable emulator
// unfolds side to side. A phone folding top to bottom would need rotateX.
function hingeBook(canvas, stage, wrap) {
  let book = null, slab = null, dir = null, from = null, run = 0;

  const rectOf = (node) => {
    const r = node.getBoundingClientRect(), st = stage.getBoundingClientRect();
    return { left: r.left - st.left, top: r.top - st.top, width: r.width, height: r.height };
  };
  const place = (node, r) => {
    Object.assign(node.style, {
      left: `${r.left}px`, top: `${r.top}px`, width: `${r.width}px`, height: `${r.height}px`,
    });
    node.style.setProperty('--r', getComputedStyle(canvas).borderTopLeftRadius || '0px');
  };
  const div = (cls, ...kids) => {
    const d = document.createElement('div');
    d.className = cls;
    for (const k of kids) d.appendChild(k);
    return d;
  };
  const framed = () => !!wrap && wrap.classList.contains('framed');
  // The box is what is on screen: the bezel and the canvas inside it when
  // a frame is shown, the canvas alone otherwise.
  const boxOf = () => (framed() ? wrap : canvas);
  const copy = () => {
    const c = document.createElement('canvas');
    c.width = canvas.width; c.height = canvas.height;
    c.getContext('2d').drawImage(canvas, 0, 0);
    return c;
  };
  // picture() is the box as it looks now, an element of the box's size in
  // CSS px: the canvas copied, and with a frame the bezel's layers cloned
  // around it with the positions and turns fitScreen gave them.
  function picture() {
    const r = rectOf(boxOf());
    let p;
    if (!framed()) {
      p = copy();
    } else {
      p = div('foldPic');
      if (wrap.classList.contains('derived')) p.classList.add('derived');
      p.style.borderRadius = getComputedStyle(wrap).borderTopLeftRadius || '';
      for (const node of wrap.children) {
        if (node.hidden) continue;
        if (node === canvas) {
          const c = copy();
          c.style.cssText = canvas.style.cssText;
          c.style.position = 'absolute';
          p.appendChild(c);
        } else if (node.classList.contains('frameLayer')) {
          const k = node.cloneNode(true);
          k.removeAttribute('id');
          for (const el of k.querySelectorAll('[id]')) el.removeAttribute('id');
          for (const path of k.querySelectorAll('path')) path.setAttribute('fill', '#000');
          p.appendChild(k);
        }
      }
    }
    p.style.width = `${r.width}px`;
    p.style.height = `${r.height}px`;
    p.dataset.w = r.width;
    p.dataset.h = r.height;
    return p;
  }
  // fit stretches a picture onto a face of w x h CSS px.
  const fit = (pic, w, h) => {
    pic.style.position = 'absolute';
    pic.style.left = pic.style.top = '0px';
    pic.style.transformOrigin = '0 0';
    pic.style.transform = `scale(${round(w / pic.dataset.w, 4)}, ${round(h / pic.dataset.h, 4)})`;
    return pic;
  };
  const face = (cls, pic, x = 0) => {
    const f = div(`foldFace ${cls}`);
    if (pic) {
      pic.style.position = 'absolute';
      pic.style.top = '0px';
      pic.style.left = `${-x}px`;
      f.appendChild(pic);
    }
    f.appendChild(div('foldShade'));
    return f;
  };
  const shade = (f) => f.querySelector('.foldShade');

  // build lays the inner panel's halves over its box, with `back` (a
  // picture of the outer panel, or nothing) on the left leaf's back.
  function build(back) {
    const r = rectOf(boxOf()), half = r.width / 2;
    const front = face('front', picture(), 0);
    const right = face('', picture(), half);
    const rear = face('back', back && fit(back, half, r.height));
    const leaf = div('foldLeaf l', front, rear);
    // The turning leaf goes last so it paints over the still one: the book
    // is flat, not a 3D scene, so DOM order is what puts one on top.
    const b = div('foldBook', div('foldLeaf r', right), leaf);
    b.classList.toggle('framed', framed());
    b.style.perspective = `${Math.round(Math.max(r.width, r.height) * 2.5)}px`;
    place(b, r);
    if (framed()) {
      // The shell on the leaf's back follows the phone's outer curve, the
      // screen's own radius widened by the bezel.
      const c = rectOf(canvas);
      const inner = parseFloat(getComputedStyle(canvas).borderTopLeftRadius) || 0;
      b.style.setProperty('--r', `${Math.round(inner + (r.width - c.width) / 2)}px`);
    }
    stage.appendChild(b);
    return { b, r, leaf, right, front, rear };
  }

  // swing turns the left leaf from flat to shut, or back with `reverse`,
  // shading each face as it turns away from the light.
  function swing(p, reverse) {
    const o = { duration: FOLD_MS, easing: FOLD_EASE, fill: 'forwards', direction: reverse ? 'reverse' : 'normal' };
    p.leaf.animate([{ transform: 'rotateY(0deg)' }, { transform: 'rotateY(180deg)' }], o);
    shade(p.front).animate([{ opacity: 0 }, { opacity: 0.6, offset: 0.5 }, { opacity: 0.6 }], o);
    shade(p.rear).animate([{ opacity: 0.35 }, { opacity: 0.35, offset: 0.5 }, { opacity: 0 }], o);
    shade(p.right).animate([{ opacity: 0 }, { opacity: 0.4 }], o);
  }

  // painted resolves once the canvas holds a picture again. Resizing a
  // canvas clears it, and the reshape is seen before the next frame is
  // drawn into it; a picture with no colour in it at all is taken as not
  // yet drawn, for up to half a second.
  async function painted(mine) {
    const probe = document.createElement('canvas');
    probe.width = probe.height = 8;
    const pc = probe.getContext('2d', { willReadFrequently: true });
    for (let i = 0; i < 30 && mine === run; i++) {
      await new Promise((res) => requestAnimationFrame(res));
      pc.drawImage(canvas, 0, 0, 8, 8);
      const px = pc.getImageData(0, 0, 8, 8).data;
      let sum = 0;
      for (let j = 0; j < px.length; j += 4) sum += px[j] + px[j + 1] + px[j + 2];
      if (sum > 8 * 8 * 3 * 4) return;
    }
  }

  function close() {
    leave(0);
    ++run;
    dir = 'closing';
    book = build(null);
    book.framed = framed();
    swing(book, false);
    // A picture that lands mid-fold waits for the fold to finish.
    book.shut = book.b.animate([{ transform: FLAT }, { transform: slabTransform(book.r, book.r) }],
      { duration: FOLD_MS, easing: FOLD_EASE, fill: 'forwards' }).finished.catch(() => {});
  }

  function open() {
    leave(0);
    ++run;
    dir = 'opening';
    from = rectOf(boxOf());
    slab = div('foldSlab', picture());
    place(slab, from);
    stage.appendChild(slab);
  }

  // bezelled waits, for up to a second, for the other panel's bezel when
  // the fold started with one: it is fetched after the stream reshapes, and
  // until its art has loaded the box is the canvas alone.
  async function bezelled(mine, want) {
    const ready = () => framed() && [...wrap.querySelectorAll('img.frameLayer')]
      .every((img) => img.hidden || (img.complete && img.naturalWidth > 0));
    for (let i = 0; want && i < 60 && mine === run && !ready(); i++) {
      await new Promise((res) => requestAnimationFrame(res));
    }
  }

  async function arrive() {
    const mine = run;
    await painted(mine);
    if (mine !== run) return false;
    if (dir === 'closing' && book) {
      await book.shut;
      await bezelled(mine, book.framed);
      if (mine !== run) return false;
      const to = rectOf(boxOf());
      book.rear.prepend(fit(picture(), book.r.width / 2, book.r.height));
      const anim = book.b.animate(
        [{ transform: slabTransform(book.r, book.r) }, { transform: slabTransform(book.r, to) }],
        { duration: REVEAL_MS, easing: 'ease-out', fill: 'forwards' });
      await anim.finished.catch(() => {});
    } else if (dir === 'opening' && slab) {
      const pic = slab.firstChild;
      await bezelled(mine, pic.classList.contains('foldPic'));
      if (mine !== run) return false;
      book = build(pic);
      slab.remove(); slab = null;
      swing(book, true);
      const anim = book.b.animate([{ transform: slabTransform(book.r, from) }, { transform: FLAT }],
        { duration: FOLD_MS, easing: FOLD_EASE, fill: 'forwards' });
      await anim.finished.catch(() => {});
    } else {
      return false;
    }
    return mine === run;
  }

  function leave(ms = REVEAL_MS) {
    run++;
    for (const node of [book && book.b, slab]) {
      if (!node) continue;
      if (!ms) { node.remove(); continue; }
      node.animate([{ opacity: 1 }, { opacity: 0 }], { duration: ms, easing: 'ease-out', fill: 'forwards' })
        .finished.catch(() => {}).then(() => node.remove());
    }
    book = null; slab = null; dir = null;
  }

  return { close, open, arrive, leave };
}

// canDraw says whether this page can draw the hinge: a canvas and a stage
// to draw over, the Web Animations API, and nobody asking for less motion.
function canDraw(canvas, stage) {
  if (!canvas || !stage || typeof canvas.getContext !== 'function') return false;
  if (typeof canvas.animate !== 'function') return false;
  const m = typeof matchMedia === 'function' && matchMedia('(prefers-reduced-motion: reduce)');
  return !(m && m.matches);
}

const CHECK = '<svg class="moreMenuCheck" viewBox="0 0 24 24" aria-hidden="true"><path d="M5 12.5l4.5 4.5L19 7.5"/></svg>';

function icon(value) {
  return `<svg viewBox="0 0 24 24" aria-hidden="true">${ICONS[value] || ICONS.opened}</svg>`;
}

// mount(nav, deps) builds the button and its menu inside #cornerNav.
//
// deps:
//   fetch      -- defaults to globalThis.fetch
//   targetId() -- the target the button acts on now
//   report(o)  -- client telemetry
//   onShape()  -- called after the row appears or goes, so the page refits
//   toast      -- the shared #toast element, shown through toast.js
//   wrap       -- #screenWrap, hidden or turned while the fold goes through
//   canvas     -- the stream's canvas, whose picture the hinge folds
//   stage      -- #stage, which the hinge draws in over the canvas
//   surface()  -- instead of the three above: {wrap, canvas, stage} of the
//                 focused pane, read when a fold starts, so the motion lands
//                 on whichever pane is in front
//
// Returns {refresh(), close(), reshaped()}: refresh re-reads the posture for
// the current target, close shuts the menu, and reshaped tells it the
// stream changed size, which is the other panel's picture arriving.
export function mount(nav, deps = {}) {
  const f = deps.fetch || ((...a) => globalThis.fetch(...a));
  const root = document.documentElement;

  const btn = document.createElement('button');
  btn.className = 'cornerBtn';
  btn.dataset.slot = 'fold';
  btn.hidden = true;
  btn.setAttribute('aria-haspopup', 'true');
  btn.setAttribute('aria-expanded', 'false');
  const menu = document.createElement('div');
  // One posture is always current, so the checkmark column is always shown.
  menu.className = 'moreMenu foldMenu checked';
  menu.setAttribute('role', 'menu');
  menu.hidden = true;
  nav.appendChild(btn);
  nav.appendChild(menu);

  let state = null; // {value, options} for `forTarget`
  let forTarget = null;
  let generation = 0;
  let writing = false;

  // The page's half of a fold. turnAway starts it; turnBack finishes it.
  // A flip hides the box behind the hinge (hingeBook) until turnBack,
  // which waits for the stream to reshape or for SETTLE_MS after the PUT,
  // so the box comes back showing the new panel rather than the old one's
  // last frame. Without a hinge to draw, the box dims instead.
  const surface = deps.surface || (() => ({ wrap: deps.wrap, canvas: deps.canvas, stage: deps.stage }));
  // One hinge per box, made the first time a fold runs on it.
  const hinges = new WeakMap();
  const wired = new WeakSet();
  function hingeFor(s) {
    if (!canDraw(s.canvas, s.stage)) return null;
    if (!hinges.has(s.wrap)) hinges.set(s.wrap, hingeBook(s.canvas, s.stage, s.wrap));
    return hinges.get(s.wrap);
  }
  function wire(wrap) {
    if (wired.has(wrap)) return;
    wired.add(wrap);
    wrap.addEventListener('animationend', (ev) => {
      if (ev.target !== wrap || (motion && motion.wrap === wrap) || !wrap.classList.contains('fold-back')) return;
      wrap.classList.remove('fold-back', 'fold-flip', 'fold-tilt');
      delete wrap.dataset.fold;
    });
  }
  let motion = null;
  let settle = 0;
  function turnAway(m) {
    const s = surface() || {};
    const wrap = s.wrap || null;
    if (!wrap || !m) return;
    clearTimeout(settle);
    wire(wrap);
    const hinge = hingeFor(s);
    motion = { ...m, wrap, hinge };
    wrap.classList.remove('fold-back', 'fold-flip', 'fold-tilt');
    wrap.dataset.fold = m.dir;
    wrap.classList.add('fold-away', `fold-${m.kind}`);
    if (hinge && m.kind === 'flip') {
      if (m.dir === 'closing') hinge.close(); else hinge.open();
    }
  }
  function turnBack() {
    clearTimeout(settle);
    if (!motion) return;
    const { wrap, hinge } = motion;
    motion = null;
    wrap.classList.remove('fold-away');
    wrap.classList.add('fold-back');
    if (hinge) hinge.leave();
  }
  function settleSoon(ms) {
    clearTimeout(settle);
    if (motion && !motion.arriving) settle = setTimeout(turnBack, ms);
  }

  function show(next, target) {
    const had = !btn.hidden;
    state = next;
    forTarget = target;
    btn.hidden = !next;
    root.classList.toggle('has-fold', !!next);
    if (next) {
      btn.innerHTML = icon(next.value);
      btn.setAttribute('aria-label', `Fold: ${postureLabel(next.value)}`);
    } else {
      close();
    }
    if (had !== !!next && deps.onShape) deps.onShape();
  }

  function fail(message) {
    if (deps.toast) showToast(deps.toast, message);
  }

  function close() {
    menu.hidden = true;
    btn.setAttribute('aria-expanded', 'false');
  }

  function open() {
    if (!state) return;
    menu.innerHTML = '';
    for (const value of state.options) {
      const item = document.createElement('button');
      item.className = 'moreMenuItem';
      item.setAttribute('role', 'menuitemradio');
      item.setAttribute('aria-checked', String(value === state.value));
      // Laid out as corners.js lays out the More menu: a checkmark on the
      // current posture, the label, then the icon trailing.
      item.innerHTML = `${CHECK}<span>${postureLabel(value)}</span>${icon(value)}`;
      item.addEventListener('click', (ev) => { ev.stopPropagation(); choose(value); });
      menu.appendChild(item);
    }
    menu.hidden = false;
    btn.setAttribute('aria-expanded', 'true');
  }

  async function refresh() {
    const target = deps.targetId ? deps.targetId() : null;
    const mine = ++generation;
    if (!target) { show(null, null); return; }
    try {
      const res = await f(`/api/settings?target=${encodeURIComponent(target)}`);
      if (mine !== generation) return;
      show(res.ok ? postureOf(await res.json()) : null, target);
    } catch {
      if (mine === generation) show(null, null);
    }
  }

  async function choose(value) {
    close();
    if (!state || writing || value === state.value) return;
    const target = forTarget;
    const from = state.value;
    writing = true;
    show({ ...state, value }, target); // show it now; a failure puts it back
    const m = foldMotion(from, value);
    turnAway(m);
    let outcome = 'error', status = 0;
    try {
      const res = await f(`/api/settings?target=${encodeURIComponent(target || '')}`, {
        method: 'PUT',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ changes: { posture: value } }),
      });
      status = res.status;
      const body = await res.json().catch(() => null);
      if (forTarget !== target) { outcome = 'superseded'; turnBack(); return; }
      if (res.ok) {
        outcome = 'ok';
        show(postureOf(body) || { ...state, value }, target);
        if (deps.onChanged) deps.onChanged();
        // A tilt has nothing to wait for. A flip waits for the new panel.
        if (m && m.kind === 'flip') settleSoon(SETTLE_MS); else turnBack();
      } else {
        turnBack();
        show({ ...state, value: from }, target);
        fail((body && body.error) || `Fold failed (${res.status})`);
      }
    } catch (err) {
      turnBack();
      if (forTarget === target) show({ ...state, value: from }, target);
      fail('Fold failed');
    } finally {
      writing = false;
      if (deps.report) deps.report({ event: 'fold', from, to: value, outcome, status });
    }
  }

  btn.addEventListener('click', (ev) => {
    ev.stopPropagation();
    if (menu.hidden) open(); else close();
  });
  document.addEventListener('click', (ev) => {
    if (!menu.hidden && !menu.contains(ev.target)) close();
  });

  // The new panel's picture has arrived, while the PUT may still be
  // waiting on the lit-panel check: finish the fold on it now. Before the
  // PUT went out nothing is folding, so this does nothing.
  function reshaped() {
    if (!motion || motion.kind !== 'flip' || motion.arriving) return;
    if (!motion.hinge) { settleSoon(0); return; }
    const m = motion;
    m.arriving = true;
    clearTimeout(settle);
    m.hinge.arrive().then(() => { if (motion === m) turnBack(); });
  }

  return { refresh, close, reshaped };
}
