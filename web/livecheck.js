// Layout self-check, for running against a live page rather than a fake DOM.
// app.js exposes both halves on window (__loupeLayout, __loupeLayoutCheck) so
// a browser session can ask the page whether it is laid out correctly instead
// of someone reading numbers off a screenshot.
//
// This is a leaf: it imports nothing from its siblings (trap 43), so it can be
// loaded by a test without a DOM and cannot drag another module's state in.
//
// collect(doc) reads the page; check(snapshot) is pure and holds the rules,
// so the rules are tested with hand-built snapshots and collect() is kept to
// reading geometry and nothing else.
//
// A snapshot is
//
//   {hand, frame, immersive, viewport:{w,h}, stream:{w,h},
//    rects:{screen, mask, art, pill, nav, more, stage,
//           topbar, handle, control, clip, rec, inspect}}
//
// Each rect is {x, y, w, h} in CSS px, viewport-absolute, or null when the
// element is absent or not rendered -- a hidden nav stack (a target with no
// control) is not a nav stack on the wrong side. frame is 'skin' or
// 'derived' while a bezel is actually on screen, and null otherwise: the
// bezel toggle can be on while the frame is hidden for not suiting the
// stream's orientation, and only a shown frame puts a mask beside the canvas.

// Sub-pixel slack. Layout rounds to device pixels and a turned mask's
// bounding box carries float noise from the transform; anything a person can
// see is a whole pixel or more.
const EPS = 0.5;

function rectOf(node) {
  if (!node || node.hidden) return null;
  if (typeof node.closest === 'function' && node.closest('[hidden]')) return null;
  const r = node.getBoundingClientRect();
  if (!r || (r.width === 0 && r.height === 0)) return null;
  return { x: r.left, y: r.top, w: r.width, h: r.height };
}

// paneParts reads one .pane root: its parts carry data-r roles, not ids,
// since every pane is a clone of the same template.
function paneParts(root) {
  const role = (r) => root.querySelector(`[data-r="${r}"]`);
  const wrap = role('wrap');
  let frame = null;
  if (wrap && wrap.classList.contains('framed')) {
    frame = wrap.classList.contains('skin') ? 'skin' : 'derived';
  }
  return { root, wrap, canvas: role('canvas'), mask: role('frameMask'), art: role('frameArt'), frame };
}

export function collect(doc) {
  const byId = (id) => doc.getElementById(id);
  const roots = doc.querySelectorAll ? Array.from(doc.querySelectorAll('.pane')) : [];
  const parts = roots.map(paneParts);
  // The top-level rects are the focused pane's, so one pane reads as it did
  // before there could be several.
  const top = parts.find((p) => p.root.classList.contains('focused')) || parts[0] || {};
  // The banners carry a class, not an id: each pane has its own.
  const byClass = (c) => (top.root && top.root.querySelector ? top.root.querySelector('.' + c) : null)
    || (doc.querySelector ? doc.querySelector('.' + c) : null);
  const { canvas, mask, frame } = top;
  const view = doc.defaultView;

  return {
    hand: doc.documentElement.classList.contains('hand-right') ? 'right' : 'left',
    frame,
    immersive: doc.documentElement.classList.contains('immersive'),
    docked: doc.documentElement.classList.contains('docked'),
    viewport: { w: view ? view.innerWidth : 0, h: view ? view.innerHeight : 0 },
    stream: { w: canvas ? canvas.width : 0, h: canvas ? canvas.height : 0 },
    rects: {
      screen: rectOf(canvas),
      mask: frame === 'skin' ? rectOf(mask) : null,
      art: rectOf(top.art),
      pill: rectOf(byId('pill')),
      nav: rectOf(byId('cornerNav')),
      more: rectOf(byId('cornerMore')),
      stage: rectOf(byId('stage')),
      topbar: rectOf(byId('topbar')),
      handle: rectOf(byId('immersiveHandle')),
      control: rectOf(byClass('controlBanner')),
      clip: rectOf(byClass('clipOffer')),
      rec: rectOf(byClass('recBanner')),
      inspect: rectOf(byId('inspectSheet')),
      sheet: rectOf(byId('sheet')),
      drawer: rectOf(byId('drawer')),
      leftPanelBtn: rectOf(byId('leftPanelBtn')),
      rightPanelBtn: rectOf(byId('rightPanelBtn')),
      over: rectOf(byId('newDevice')) || rectOf(byId('connectHelp')) || rectOf(byId('switchServer')) || rectOf(byId('addServer')),
    },
    // Every pane, in page order. check() reads them only when there are two
    // or more; with one, the top level above is that pane.
    panes: parts.map((p, i) => ({
      index: i + 1,
      focused: p === top,
      frame: p.frame,
      cell: rectOf(p.root),
      screen: rectOf(p.canvas),
      mask: p.frame === 'skin' ? rectOf(p.mask) : null,
    })),
    // The 3D tab covers the device on purpose (node 7:78).
    inspectFull: !!(byId('inspectSheet') && byId('inspectSheet').classList
      && byId('inspectSheet').classList.contains('full')),
  };
}

const fmt = (r) => `${r.x},${r.y} ${r.w}x${r.h}`;

function intersects(a, b) {
  const w = Math.min(a.x + a.w, b.x + b.w) - Math.max(a.x, b.x);
  const h = Math.min(a.y + a.h, b.y + b.h) - Math.max(a.y, b.y);
  return w > EPS && h > EPS;
}

// check(snapshot) -> failure strings; an empty array is a pass.
export function check(snap) {
  const out = [];
  const { screen, mask, stage, nav, more } = snap.rects || {};

  if (!screen) {
    out.push('screen: not rendered');
    return out;
  }

  // Trap 40: mask.webp is exactly the display, so it goes on the canvas's
  // rect and nowhere else. Stretched to the frame box it looks deliberate.
  if (snap.frame === 'skin') {
    if (!mask) {
      out.push('mask: skin frame shown without a mask');
    } else if (['x', 'y', 'w', 'h'].some((k) => Math.abs(mask[k] - screen[k]) > EPS)) {
      out.push(`mask: ${fmt(mask)} does not match screen ${fmt(screen)}`);
    }
  }

  // Trap 18: the device screen has to fit, on both axes.
  if (!stage) {
    out.push('stage: not rendered');
  } else {
    if (screen.x < stage.x - EPS || screen.x + screen.w > stage.x + stage.w + EPS) {
      out.push(`screen: ${fmt(screen)} overflows stage ${fmt(stage)} horizontally`);
    }
    if (screen.y < stage.y - EPS || screen.y + screen.h > stage.y + stage.h + EPS) {
      out.push(`screen: ${fmt(screen)} overflows stage ${fmt(stage)} vertically`);
    }
  }

  // Inspect puts its sheet under the device, which is refitted into the
  // space above it rather than drawn beneath.
  const sheet = snap.rects.inspect;
  if (sheet && (snap.docked || !snap.inspectFull) && intersects(screen, sheet)) out.push(`inspect: sheet ${fmt(sheet)} overlaps screen ${fmt(screen)}`);

  if (nav && intersects(screen, nav)) out.push(`nav: ${fmt(nav)} overlaps screen ${fmt(screen)}`);
  if (more && intersects(screen, more)) out.push(`more: ${fmt(more)} overlaps screen ${fmt(screen)}`);

  // The top banners go under the top bar, over the top of the device, and
  // under the immersive handle when the bar is gone. Over the bar they
  // covered the pill, and the clipboard offer, a button, took the pill's taps.
  // The stage kept a slot above the device for them until that slot was
  // found to cost the device 5% of its width.
  const { topbar, handle, control, clip, rec } = snap.rects;
  for (const [name, r] of [['control', control], ['clip', clip], ['rec', rec]]) {
    if (!r) continue;
    if (topbar && intersects(r, topbar)) out.push(`${name}: ${fmt(r)} overlaps topbar ${fmt(topbar)}`);
    if (handle && intersects(r, handle)) out.push(`${name}: ${fmt(r)} overlaps handle ${fmt(handle)}`);
  }

  // Immersive mode is the screen alone, as large as the stage allows: no
  // controls and no bezel on screen, and a contain fit centred on the stage,
  // so it meets the stage's edges on one axis and splits the rest evenly.
  if (snap.immersive) {
    const { pill } = snap.rects;
    const shown = [['pill', pill], ['nav', nav], ['more', more]];
    // Docked, the panels are fixtures beside the device, and full screen
    // hides them with everything else.
    if (snap.docked) {
      shown.push(['sheet', snap.rects.sheet], ['drawer', snap.rects.drawer], ['inspect', snap.rects.inspect]);
    }
    for (const [name, r] of shown) {
      if (r) out.push(`immersive: ${name} is still shown at ${fmt(r)}`);
    }
    if (snap.frame) out.push(`immersive: a ${snap.frame} frame is still shown`);
    if (stage) {
      const fillsW = Math.abs(screen.w - stage.w) <= 1;
      const fillsH = Math.abs(screen.h - stage.h) <= 1;
      if (!fillsW && !fillsH) {
        out.push(`immersive: screen ${fmt(screen)} does not fill stage ${fmt(stage)} on either axis`);
      }
      const dx = (screen.x - stage.x) - (stage.x + stage.w - screen.x - screen.w);
      const dy = (screen.y - stage.y) - (stage.y + stage.h - screen.y - screen.h);
      if (Math.abs(dx) > 1 || Math.abs(dy) > 1) {
        out.push(`immersive: screen ${fmt(screen)} is not centred in stage ${fmt(stage)}`);
      }
    }
  }

  // The panel toggles collapse docked panels, and below the breakpoint
  // there are none: the picker and Tools are sheets.
  if (!snap.docked) {
    for (const name of ['leftPanelBtn', 'rightPanelBtn']) {
      if (snap.rects[name]) out.push(`${name}: shown at ${fmt(snap.rects[name])} while not docked`);
    }
  }

  // Docked, the picker is a panel on the left and Tools one on the right,
  // beside the device rather than over it, so none of them may cover the
  // screen or a corner. Below the breakpoint they are bottom sheets that
  // cover the device on purpose.
  const panels = snap.docked
    ? [['sheet', snap.rects.sheet], ['drawer', snap.rects.drawer], ['over', snap.rects.over]].filter(([, r]) => r)
    : [];
  for (const [name, r] of panels) {
    if (intersects(screen, r)) out.push(`${name}: panel ${fmt(r)} overlaps screen ${fmt(screen)}`);
    for (const [cn, c] of [['nav', nav], ['more', more]]) {
      if (c && intersects(c, r)) out.push(`${cn}: ${fmt(c)} is under the ${name} panel ${fmt(r)}`);
    }
  }

  // Inspect takes Tools' slot on the right, and the CSS hides Tools while
  // it is on, so any panel still under it is one hidden behind the other.
  // The New device sheet covers the picker on purpose.
  if (snap.docked && snap.rects.inspect) {
    for (const [name, r] of panels) {
      if (intersects(snap.rects.inspect, r)) out.push(`inspect: panel ${fmt(snap.rects.inspect)} is under the ${name} panel ${fmt(r)}`);
    }
  }

  // Side by side, every pane's screen stays in its own cell, the cells do
  // not overlap, and no corner or docked panel covers any of them. The
  // focused pane is the top level, which the rules above already checked.
  const many = Array.isArray(snap.panes) && snap.panes.length > 1 ? snap.panes : [];
  for (const p of many) {
    const name = `pane ${p.index}`;
    if (!p.screen) { out.push(`${name}: screen not rendered`); continue; }
    if (p.cell && (p.screen.x < p.cell.x - EPS || p.screen.y < p.cell.y - EPS
      || p.screen.x + p.screen.w > p.cell.x + p.cell.w + EPS
      || p.screen.y + p.screen.h > p.cell.y + p.cell.h + EPS)) {
      out.push(`${name}: screen ${fmt(p.screen)} overflows its pane ${fmt(p.cell)}`);
    }
    for (const q of many) {
      if (q.index >= p.index || !q.cell || !p.cell) continue;
      if (intersects(p.cell, q.cell)) out.push(`${name}: ${fmt(p.cell)} overlaps pane ${q.index} ${fmt(q.cell)}`);
    }
    if (p.focused) continue;
    for (const [cn, c] of [['nav', nav], ['more', more]]) {
      if (c && intersects(p.screen, c)) out.push(`${name}: ${cn} ${fmt(c)} overlaps screen ${fmt(p.screen)}`);
    }
    for (const [pn, r] of panels) {
      if (intersects(p.screen, r)) out.push(`${name}: ${pn} panel ${fmt(r)} overlaps screen ${fmt(p.screen)}`);
    }
  }

  if (nav) {
    // The middle of the space the device has, between the panels docked on
    // either side. At 900px the two panels are unequal in what they leave,
    // and the viewport's middle would put a nav row at either inner edge on
    // the wrong side.
    const vw = snap.viewport ? snap.viewport.w : 0;
    let left = stage ? stage.x : 0;
    let right = stage ? stage.x + stage.w : vw;
    const docked = snap.docked && snap.rects.inspect ? [...panels, ['inspect', snap.rects.inspect]] : panels;
    for (const [, r] of docked) {
      if (r.x + r.w / 2 < vw / 2) left = Math.max(left, r.x + r.w);
      else right = Math.min(right, r.x);
    }
    const mid = (left + right) / 2;
    const side = nav.x + nav.w / 2 < mid ? 'left' : 'right';
    if (side !== snap.hand) out.push(`nav: on the ${side}, hand is ${snap.hand}`);
  }

  return out;
}
