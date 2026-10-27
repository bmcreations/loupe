// Inspect mode (design spec "Inspect", client Task 9; Tree and 3D tabs from
// nodes 4:3 and 7:78). Toggled from the
// overflow menu (corners.js's optional deps.inspect: {label, run()}). While
// on, a transparent #inspectLayer sits over the canvas and takes pointer
// events instead of the device, so a tap selects a node rather than reaching
// Android. Boxes and the selected node's card come from
// GET /api/ui?interactive=1 (internal/httpapi/agent.go's uiResponse), fetched
// when inspect turns on and again on Refresh.
//
// Split the way jitter.js/orientation.js are: pure functions with no DOM in
// them (testable against the hand-built fixture in
// testdata/client/ui-settings.json -- see that file's own "_note" for why it
// is hand-built), plus mount() for the DOM half.

import { showToast } from './toast.js';

// nodeAt(nodes, x, y) -> node | null
//
// nodes is a target.Node[] as /api/ui?interactive=1 returns it under
// "nodes" -- which is `null`, not `[]`, when target.Prune(ui.Root) collapses
// to nothing (a server fix for that is in flight; this treats null as
// simply "nothing to hit" rather than throwing). x/y are device pixels, the
// same space Bounds and Center are already in.
//
// The deepest containing node wins: among a node's children, later siblings
// are drawn on top (Android's own paint order), so a tie at one level goes
// to the last child whose bounds contain the point, and the search then
// recurses into THAT child looking for something deeper still. A node with
// no containing child is the answer at its own level.
export function nodeAt(nodes, x, y) {
  if (!nodes) return null;
  let hit = null;
  for (const n of nodes) {
    if (n && containsPoint(n.bounds, x, y)) hit = n;
  }
  if (!hit) return null;
  return nodeAt(hit.children, x, y) || hit;
}

// Inclusive on both edges: a device tap reads in integer pixels and so do
// bounds, so a point sitting exactly on a shared edge between two adjacent
// nodes should still hit whichever one is checked (the later sibling wins
// via nodeAt's own last-match rule above), not fall through to neither.
export function containsPoint(bounds, x, y) {
  if (!bounds || bounds.length !== 4) return false;
  const [x1, y1, x2, y2] = bounds;
  return x >= x1 && x <= x2 && y >= y1 && y <= y2;
}

// treeIsStale(tree, streamW, streamH) -> bool
//
// tree is the whole /api/ui response ({width, height, nodes, ...}, or
// null/undefined before the first fetch). streamW/streamH is the stream size
// in effect right now (the pane's info.codedWidth/Height).
//
// Compares ORIENTATION, not exact pixel dimensions. MaxSize (or a fresh
// session simply negotiating a different one on a plain reconnect) rescales
// the coded stream without touching the device's own rotation, and the
// tree's bounds are in DEVICE pixels regardless of what the stream happens
// to be coded at -- so treating every dimension mismatch as staleness marked
// every MsgInit stale, reconnect included. What actually invalidates the
// tree is a rotation (trap 42), and a rotation is exactly what swaps
// portrait for landscape, so that swap is the one thing worth comparing.
// A square device (width === height) can rotate without this ever reading
// stale, since treePortrait and streamPortrait are both computed with >=
// and so agree at every rotation. Left alone per review round 3: no
// fixture or real device here is square, and detecting that rotation would
// need a signal this function doesn't have (the actual angle, not just the
// two sizes it's given).
export function treeIsStale(tree, streamW, streamH) {
  if (!tree || !streamW || !streamH) return false;
  const treePortrait = tree.height >= tree.width;
  const streamPortrait = streamH >= streamW;
  return treePortrait !== streamPortrait;
}

// toCss(bounds, deviceW, deviceH, rect) -> {left, top, width, height}
//
// Maps a device-pixel bounds box onto the CSS box #inspectLayer's boxes are
// drawn into. `rect` describes the CANVAS's own box, in the LAYER's own
// coordinate frame: {width, height, left = 0, top = 0}. left/top default to
// 0 for an unframed device, where the canvas fills #screenWrap and so does
// the layer -- they share an origin. Framed (trap 40's geometry, one layer
// up), #screen sits inset inside #screenWrap at the display rect's offset
// while #inspectLayer -- like every other layer over the stage -- fills the
// WHOLE bezel box, so a box drawn for device (0,0) has to land at the
// canvas's offset within the layer, not at the layer's own origin.
//
// Independent x/y scale factors rather than one shared one: fitScreen keeps
// the canvas at the device's own aspect ratio, so in practice they agree,
// but nothing here assumes that.
export function toCss(bounds, deviceW, deviceH, rect) {
  const [x1, y1, x2, y2] = bounds;
  const sx = deviceW > 0 ? rect.width / deviceW : 0;
  const sy = deviceH > 0 ? rect.height / deviceH : 0;
  const originLeft = rect.left || 0;
  const originTop = rect.top || 0;
  return {
    left: originLeft + x1 * sx,
    top: originTop + y1 * sy,
    width: (x2 - x1) * sx,
    height: (y2 - y1) * sy,
  };
}

// flatten collects every node in the tree, depth-first, for drawing one box
// per node without a second recursive walk at render time.
function flatten(nodes, out) {
  if (!nodes) return out;
  for (const n of nodes) {
    if (!n) continue;
    out.push(n);
    flatten(n.children, out);
  }
  return out;
}

// A password field's node.text is whatever the device's edit buffer holds --
// the actual password, in the clear, over an endpoint client Task 9 exposes
// unauthenticated behind the shared token like every other agent route. The
// user's own call on this: redact it. A mask says a password field is
// present and focused without saying what is in it, which is the one thing
// worth showing here; the field's class/desc/bounds still identify it.
const PASSWORD_MASK = '••••••••';

export function describe(node) {
  const lines = [];
  if (node.password) lines.push(`text: ${PASSWORD_MASK}`);
  else if (node.text) lines.push(`text: ${node.text}`);
  if (node.desc) lines.push(`desc: ${node.desc}`);
  if (node.class) lines.push(`class: ${node.class}`);
  if (node.composable) lines.push(`composable: ${node.composable}`);
  if (node.source) lines.push(`source: ${node.source}`);
  if (node.id) lines.push(`id: ${node.id}`);
  const flags = ['clickable', 'scrollable', 'checkable', 'checked', 'selected', 'focused', 'password', 'disabled']
    .filter((f) => node[f]);
  if (flags.length) lines.push(`flags: ${flags.join(', ')}`);
  if (node.bounds) lines.push(`bounds: [${node.bounds.join(', ')}]`);
  if (node.center) lines.push(`center: [${node.center.join(', ')}]`);
  return lines;
}

// errorMessage(res) -> the server's own `error` string when the body is
// JSON shaped like every agent-route failure ({"error": "..."} --
// internal/httpapi/agent.go's writeJSON calls), falling back to a plain
// status line when it isn't. Showing the raw response body dumped the whole
// JSON object (braces, quotes, key and all) into the toast; this shows only
// what the device or the server actually said (review MEDIUM-9).
async function errorMessage(res) {
  let body = null;
  try { body = await res.json(); } catch { /* not JSON; fall through */ }
  if (body && typeof body.error === 'string') return body.error;
  return `ui: ${res.status}`;
}

// --- The Tree tab's model (design spec "Inspect", node 4:3) -------------
//
// Rows are keyed by their index path ("0.2.1"), which is stable for one
// fetched tree and means nothing across two, so `expanded` is reset on
// every fetch.

// pathTo(nodes, target) -> [{key, node}] from a root down to target, or []
// when target is not in the tree.
export function pathTo(nodes, target, prefix = '') {
  if (!nodes || !target) return [];
  for (let i = 0; i < nodes.length; i++) {
    const n = nodes[i];
    if (!n) continue;
    const key = prefix ? `${prefix}.${i}` : String(i);
    if (n === target) return [{ key, node: n }];
    const below = pathTo(n.children, target, key);
    if (below.length) return [{ key, node: n }, ...below];
  }
  return [];
}

// pickTarget(nodes, x, y) -> the node a tap on the stream selects in the
// Tree tab: the smallest clickable node under the point, or the smallest
// node of any kind when nothing there is clickable.
//
// Not nodeAt's deepest-in-the-last-sibling: on the full tree a launcher's
// Home Screen ends with a View named "Home" covering the whole display, a
// later sibling of the icons, so on Hinge_Pixel_Fold a tap on Photos
// selected "Home". Smallest-area is what a finger means, and preferring a
// clickable node turns a tap on a row's TextView into the row Tap it
// should send.
export function pickTarget(nodes, x, y) {
  let best = null;
  let bestArea = Infinity;
  let bestClickable = false;
  const visit = (list) => {
    for (const n of list || []) {
      if (!n) continue;
      if (n.bounds && containsPoint(n.bounds, x, y)) {
        const area = (n.bounds[2] - n.bounds[0]) * (n.bounds[3] - n.bounds[1]);
        const clickable = !!n.clickable;
        if ((clickable && !bestClickable) || (clickable === bestClickable && area <= bestArea)) {
          best = n;
          bestArea = area;
          bestClickable = clickable;
        }
      }
      visit(n.children);
    }
  };
  visit(nodes);
  return best;
}

// initialExpanded(nodes) -> the keys open when a tree first arrives. A
// Compose screen starts with seven single-child wrappers before anything
// branches, so chains of one child are opened down to the first node with
// several, which is opened too and nothing below it.
export function initialExpanded(nodes) {
  const open = new Set();
  let level = nodes || [];
  let prefix = '';
  for (;;) {
    const live = level.filter(Boolean);
    if (live.length !== 1) break;
    const i = level.indexOf(live[0]);
    const key = prefix ? `${prefix}.${i}` : String(i);
    const kids = live[0].children || [];
    if (!kids.length) break;
    open.add(key);
    level = kids;
    prefix = key;
  }
  return open;
}

// visibleRows(nodes, expanded) -> [{key, depth, node, hasChildren, open}]
// for every row whose ancestors are all open. Only these are built, since
// a Settings tree runs to hundreds of nodes.
export function visibleRows(nodes, expanded, depth = 0, prefix = '', out = []) {
  if (!nodes) return out;
  for (let i = 0; i < nodes.length; i++) {
    const n = nodes[i];
    if (!n) continue;
    const key = prefix ? `${prefix}.${i}` : String(i);
    const hasChildren = !!(n.children && n.children.some(Boolean));
    const open = hasChildren && expanded.has(key);
    out.push({ key, depth, node: n, hasChildren, open });
    if (open) visibleRows(n.children, expanded, depth + 1, key, out);
  }
  return out;
}

// childText(node) -> the first text or description among node's children,
// skipping password fields. A LinearLayout row has no text of its own; the
// TextView inside it names it.
function childText(node) {
  for (const c of node.children || []) {
    if (!c) continue;
    if (!c.password && c.text) return c.text;
    if (c.desc) return c.desc;
  }
  return '';
}

// rowLabel(node) -> {cls, text}. A node's own text is quoted, which is
// what it holds; an unquoted label is borrowed from a descendant or is a
// content description, which is what it is called.
//
// It borrows from a direct child only. Borrowing from any depth labelled
// every layout down the launcher's chain "At a glance" on
// Hinge_Pixel_Fold.
//
// A node /api/ui?compose=1 named after its composable shows that name in
// place of the class: on a Compose screen nearly every class is "View".
export function rowLabel(node) {
  const cls = node.composable || node.class || 'View';
  if (node.password) return { cls, text: `"${PASSWORD_MASK}"` };
  if (node.text) return { cls, text: `"${node.text}"` };
  if (node.desc) return { cls, text: node.desc };
  if (node.value) return { cls, text: node.value };
  return { cls, text: childText(node) };
}

// crumb(path) -> the last two classes on the way to the node, the way the
// detail bar shows where it sits.
export function crumb(path) {
  return path.slice(-2).map((p) => rowLabel(p.node).cls).join(' › ');
}

export function flagsOf(node) {
  return ['clickable', 'scrollable', 'checkable', 'checked', 'selected', 'focused', 'password', 'disabled', 'occluded']
    .filter((f) => node[f]);
}

// geometryText(node, depth) -> "x 10  y 20  100×50 px", the Mac detail bar's
// line. depth is the node's depth in the tree and is shown only when given
// (the 3D tab); no bounds gives an empty string.
export function geometryText(node, depth) {
  const b = node && node.bounds;
  if (!b || b.length !== 4) return '';
  let s = `x ${b[0]}  y ${b[1]}  ${b[2] - b[0]}×${b[3] - b[1]} px`;
  if (depth != null) s += `   depth ${depth}`;
  return s;
}

// tapRequest(node) -> the /api/input body that taps node, which is both
// what Tap it sends and what Copy puts on the clipboard.
export function tapRequest(node) {
  return { action: 'tap', x: node.center[0], y: node.center[1] };
}

// The 3D tab (node 7:78): every node with an area is a plane, pushed out by
// its depth in the tree. These bound what a drag and a pinch can do to it.
export const ORBIT_LIMIT = 70; // degrees either way, past which planes go edge-on
export const SPACING_MIN = 0;
export const SPACING_MAX = 80; // CSS px between depths
export const SPACING_DEFAULT = 20;
export const DEFAULT_ORBIT = { x: 12, y: -32 };
// A Settings screen runs to a few hundred nodes; past this the deepest are
// dropped rather than building thousands of 3D layers on a phone.
export const MAX_PLANES = 600;

// layers3d(nodes) -> {planes: [{key, depth, node}], maxDepth}, in paint
// order, skipping nodes with no area. Keys match visibleRows', so a plane
// and its row name the same node.
export function layers3d(nodes) {
  const planes = [];
  let maxDepth = 0;
  const walk = (level, depth, prefix) => {
    if (!level) return;
    for (let i = 0; i < level.length; i++) {
      const n = level[i];
      if (!n) continue;
      const key = prefix ? `${prefix}.${i}` : String(i);
      const b = n.bounds;
      if (b && b[2] > b[0] && b[3] > b[1]) {
        planes.push({ key, depth, node: n });
        if (depth > maxDepth) maxDepth = depth;
      }
      walk(n.children, depth + 1, key);
    }
  };
  walk(nodes, 0, '');
  if (planes.length > MAX_PLANES) {
    const kept = planes.slice().sort((a, b) => a.depth - b.depth).slice(0, MAX_PLANES);
    const keep = new Set(kept);
    const out = planes.filter((p) => keep.has(p));
    return { planes: out, maxDepth: out.reduce((m, p) => Math.max(m, p.depth), 0) };
  }
  return { planes, maxDepth };
}

const clamp = (v, lo, hi) => Math.min(hi, Math.max(lo, v));

// orbit(o, dx, dy) -> the orientation after a drag of dx, dy CSS px. Left
// and right turn about the vertical axis, up and down about the horizontal.
export function orbit(o, dx, dy) {
  return {
    x: clamp(o.x - dy * 0.4, -ORBIT_LIMIT, ORBIT_LIMIT),
    y: clamp(o.y + dx * 0.4, -ORBIT_LIMIT, ORBIT_LIMIT),
  };
}

// pinchSpacing(start, d0, d1) -> the spacing after two fingers that began
// d0 apart move to d1 apart.
export function pinchSpacing(start, d0, d1) {
  if (!(d0 > 0)) return start;
  // From flat, a spread still has to open the layers up.
  return clamp(Math.round(Math.max(start, 4) * (d1 / d0)), SPACING_MIN, SPACING_MAX);
}

// mount(layer, deps) wires #inspectLayer and, when deps.sheet is given, the
// Inspect sheet under the device.
//
// deps:
//   targetId() -- the currently selected target id.
//   fetchImpl -- defaults to window.fetch.
//   screenRect() -- the CANVAS's current CSS box, as a getBoundingClientRect
//     would report it -- {width, height, left, top}, viewport-absolute. Read
//     fresh on every render and every click rather than cached, so a resize
//     or a refit between fetches is reflected without a stale-tree fetch.
//   sheet -- the #inspectSheet element. mount builds its header, tabs, tree,
//     detail bar and buttons into it.
//   card -- where the Tapped tab's node details render. Without a sheet it
//     is the whole UI, which is how the tests drive it.
//   tab -- the tab inspect opens on: 'tree' or 'tapped' (the default).
//   postInput(body) -- sends one /api/input as this viewer. Tap it uses it.
//   writeClipboard(text) -- defaults to navigator.clipboard.writeText.
//   onToggle(on) -- called after inspect turns on or off, so app.js can
//     refit the device into the space the sheet leaves.
//   toast -- shown on a failed fetch (no Inspector on this target, or the
//     device round trip failing), so a click on Inspect against synthetic
//     reports its own unavailability rather than doing nothing.
//   report(rec) -- app.js's report(), same contract as corners.js's deps.
//
// Returns {label: 'Inspect', run(), off(), isOn(), notifyResize(), destroy()}
// -- run() is what corners.js's deps.inspect.run() calls (it toggles), off()
// is what app.js calls on a target switch (trap 41: a tree fetched for the
// target being left behind must not paint over the one being switched to),
// and notifyResize() is what app.js calls whenever the stream's own size
// changes, so a rotation while inspecting clears the boxes rather than
// drawing them against a device pixel space the canvas no longer has.
// COMPOSE_KEY persists the sheet's Compose names toggle per browser.
const COMPOSE_KEY = 'loupe.inspect.compose';

export function mount(layer, deps) {
  let on = false;
  let tree = null; // the last-fetched /api/ui response
  let selected = null;
  let stale = false;
  let tab = deps.tab || 'tapped';
  let expanded = new Set();
  // gen guards every async continuation below: off(), a target switch (via
  // off()), and a rotation marking the tree stale all bump it, so a fetch
  // started before any of those discards its result instead of painting
  // over a state that has already moved on (trap 41, one layer up from the
  // targets.js doc comment -- there is no shared cache to race here, just
  // this one module's own in-flight request against its own later state).
  let gen = 0;
  // The live stream size, kept current by every notifyResize() call
  // regardless of whether inspect is on -- so that whenever a fetch DOES
  // resolve, refresh() can compare the tree it just got against the size
  // that is current right now, not just the size that was current when the
  // fetch started. treeIsStale(null, ...) is false by definition, so a
  // rotation landing while `tree` is still null never bumped `gen`, and the
  // response that resolves afterwards described the orientation the device
  // was in before the rotation (review round 3, finding 2).
  let streamW = 0;
  let streamH = 0;

  let composeOn = false;
  try { composeOn = localStorage.getItem(COMPOSE_KEY) === '1'; } catch { /* no storage */ }

  const ui = deps.sheet ? buildSheet(deps.sheet) : null;
  const card = ui ? ui.card : deps.card;

  function buildSheet(root) {
    root.innerHTML = '';
    const el = (tag, cls, text) => {
      const e = document.createElement(tag);
      if (cls) e.className = cls;
      if (text != null) e.textContent = text;
      return e;
    };
    const header = el('div', 'inspectHeader');
    const refreshBtn = el('button', 'inspectHeaderBtn', 'Refresh');
    refreshBtn.type = 'button';
    const title = el('h2', 'inspectTitle', 'Inspect');
    const doneBtn = el('button', 'inspectHeaderBtn inspectDone', 'Done');
    doneBtn.type = 'button';
    header.append(refreshBtn, title, doneBtn);

    const tabs = el('div', 'inspectTabs');
    tabs.setAttribute('role', 'tablist');
    const tabBtns = {};
    for (const [id, label] of [['tree', 'Tree'], ['3d', '3D'], ['tapped', 'Tapped']]) {
      const b = el('button', 'inspectTab', label);
      b.type = 'button';
      b.setAttribute('role', 'tab');
      b.dataset.tab = id;
      tabs.appendChild(b);
      tabBtns[id] = b;
    }

    const scene3d = el('div', 'inspect3d');
    const stage3d = el('div', 'inspect3dStage');
    const world = el('div', 'inspect3dWorld');
    stage3d.appendChild(world);
    const caption = el('p', 'inspect3dCaption', 'Layers spread by depth. Drag to orbit, pinch to change spacing.');
    const spacingRow = el('label', 'inspect3dSpacing');
    spacingRow.appendChild(el('span', null, 'Spacing'));
    const slider = el('input');
    slider.type = 'range';
    slider.min = String(SPACING_MIN);
    slider.max = String(SPACING_MAX);
    slider.value = String(SPACING_DEFAULT);
    const resetBtn = el('button', 'inspect3dReset', 'Reset');
    resetBtn.type = 'button';
    const sliderLine = el('span', 'inspect3dSliderLine');
    sliderLine.append(slider, resetBtn);
    spacingRow.appendChild(sliderLine);
    scene3d.append(stage3d, caption, spacingRow);

    const list = el('div', 'inspectTree');
    list.setAttribute('role', 'tree');
    const tapped = el('div', 'inspectTapped');
    const detail = el('div', 'inspectDetail');
    const detailTop = el('div', 'inspectDetailTop');
    const path = el('span', 'inspectCrumb');
    const flags = el('span', 'inspectFlags');
    detailTop.append(path, flags);
    const geom = el('div', 'inspectGeom');
    const actions = el('div', 'inspectActions');
    const tapBtn = el('button', 'inspectAction inspectTapIt', 'Tap');
    tapBtn.type = 'button';
    const copyBtn = el('button', 'inspectAction', 'Copy');
    copyBtn.title = 'Copy the /api/input request that taps this node';
    copyBtn.type = 'button';
    actions.append(tapBtn, copyBtn);
    detail.append(detailTop, geom, actions);

    // Off by default: asking for composables attaches an agent to the app
    // and adds seconds to every refresh (docs/agent-api.md, /api/ui).
    const composeBar = el('div', 'inspectComposeBar');
    const composeBtn = el('button', 'inspectCompose', 'Compose names');
    composeBtn.type = 'button';
    composeBtn.setAttribute('aria-pressed', composeOn ? 'true' : 'false');
    const composeError = el('p', 'inspectComposeError');
    composeError.hidden = true;
    composeBar.append(composeBtn, composeError);

    root.append(header, tabs, composeBar, list, scene3d, tapped, detail);

    refreshBtn.addEventListener('click', () => { if (on) refresh(); });
    composeBtn.addEventListener('click', () => {
      composeOn = !composeOn;
      composeBtn.setAttribute('aria-pressed', composeOn ? 'true' : 'false');
      try { localStorage.setItem(COMPOSE_KEY, composeOn ? '1' : '0'); } catch { /* private mode */ }
      if (on) refresh();
    });
    doneBtn.addEventListener('click', () => { if (on) setOn(false); });
    tabs.addEventListener('click', (ev) => {
      const b = ev.target && ev.target.closest ? ev.target.closest('.inspectTab') : null;
      if (!b || b.disabled) return;
      setTab(b.dataset.tab);
    });
    list.addEventListener('click', (ev) => {
      const row = ev.target && ev.target.closest ? ev.target.closest('.inspectRow') : null;
      if (!row) return;
      const r = rowsByKey.get(row.dataset.key);
      if (!r) return;
      if (ev.target.closest('.inspectTwisty')) {
        toggle(r.key);
        return;
      }
      select(r.node, 'row');
    });
    tapBtn.addEventListener('click', tapSelected);
    copyBtn.addEventListener('click', copySelected);
    slider.addEventListener('input', () => setSpacing(Number(slider.value)));
    resetBtn.addEventListener('click', reset3d);
    wire3d(stage3d);

    return { root, list, tapped, card: tapped, tabBtns, composeBar, composeError, detail, path, flags, geom, tapBtn, copyBtn, scene3d, stage3d, world, slider, resetBtn };
  }

  // rowsByKey maps each built row's key to its model, for the list's click.
  let rowsByKey = new Map();

  // The 3D tab's view, kept across tab switches and refreshes in one
  // inspect session, reset when inspect turns off.
  let view = { ...DEFAULT_ORBIT };
  let spacing = SPACING_DEFAULT;
  let planesByKey = new Map();

  function applyView() {
    if (!ui) return;
    ui.world.style.transform = `rotateX(${view.x}deg) rotateY(${view.y}deg)`;
    ui.world.style.setProperty('--spacing', String(spacing));
  }

  // Reset puts the spacing and the orbit back where a session starts, as
  // the Mac app's Reset does.
  function reset3d() {
    view = { ...DEFAULT_ORBIT };
    setSpacing(SPACING_DEFAULT);
  }

  function setSpacing(v) {
    spacing = v;
    if (ui && Number(ui.slider.value) !== v) ui.slider.value = String(v);
    applyView();
  }

  // render3d builds one plane per node, sized as the tree into the stage
  // with a contain fit, and pushed out by depth. Depth is centred so the
  // orbit turns about the middle of the stack.
  function render3d() {
    if (!ui) return;
    ui.world.innerHTML = '';
    planesByKey = new Map();
    if (!tree || tab !== '3d') return;
    const box = ui.stage3d.getBoundingClientRect();
    const tw = tree.width || 1;
    const th = tree.height || 1;
    // 0.62 leaves room for the stack to turn without leaving the stage.
    const k = Math.min(box.width / tw, box.height / th) * 0.62;
    const w = tw * k;
    const h = th * k;
    ui.world.style.width = `${w}px`;
    ui.world.style.height = `${h}px`;
    const { planes, maxDepth } = layers3d(tree.nodes);
    const mid = maxDepth / 2;
    for (const p of planes) {
      const b = p.node.bounds;
      const d = document.createElement('div');
      d.className = 'inspectPlane';
      if (p.node.clickable) d.classList.add('clickable');
      if (p.node === selected) d.classList.add('selected');
      d.dataset.key = p.key;
      d.style.left = `${b[0] * k}px`;
      d.style.top = `${b[1] * k}px`;
      d.style.width = `${(b[2] - b[0]) * k}px`;
      d.style.height = `${(b[3] - b[1]) * k}px`;
      d.style.setProperty('--z', String(p.depth - mid));
      planesByKey.set(p.key, p);
      ui.world.appendChild(d);
    }
    applyView();
  }

  // wire3d: one finger orbits, two change the spacing, and a tap that did
  // not move selects the plane under it.
  function wire3d(stage) {
    const pts = new Map();
    let start = null;
    let moved = false;
    const dist = () => {
      const [a, b] = [...pts.values()];
      return Math.hypot(a.x - b.x, a.y - b.y);
    };
    stage.addEventListener('pointerdown', (ev) => {
      pts.set(ev.pointerId, { x: ev.clientX, y: ev.clientY });
      if (stage.setPointerCapture) {
        try { stage.setPointerCapture(ev.pointerId); } catch { /* synthetic pointer */ }
      }
      if (pts.size === 1) {
        moved = false;
        start = { x: ev.clientX, y: ev.clientY, view: { ...view } };
      } else if (pts.size === 2) {
        moved = true;
        start = { d: dist(), spacing };
      }
    });
    stage.addEventListener('pointermove', (ev) => {
      if (!pts.has(ev.pointerId) || !start) return;
      pts.set(ev.pointerId, { x: ev.clientX, y: ev.clientY });
      if (pts.size === 2 && start.d != null) {
        setSpacing(pinchSpacing(start.spacing, start.d, dist()));
      } else if (pts.size === 1 && start.view) {
        const dx = ev.clientX - start.x;
        const dy = ev.clientY - start.y;
        if (!moved && Math.hypot(dx, dy) < 6) return;
        moved = true;
        view = orbit(start.view, dx, dy);
        applyView();
      }
    });
    const end = (ev) => {
      if (!pts.has(ev.pointerId)) return;
      pts.delete(ev.pointerId);
      if (ev.type === 'pointerup' && pts.size === 0 && !moved) {
        const hit = document.elementFromPoint
          ? document.elementFromPoint(ev.clientX, ev.clientY) : ev.target;
        const plane = hit && hit.closest ? hit.closest('.inspectPlane') : null;
        const p = plane ? planesByKey.get(plane.dataset.key) : null;
        if (p) select(p.node, '3d');
      }
      if (pts.size === 0) start = null;
      else if (pts.size === 1) {
        // Back to one finger: orbit on from here rather than jumping.
        const [only] = [...pts.values()];
        start = { x: only.x, y: only.y, view: { ...view } };
      }
    };
    stage.addEventListener('pointerup', end);
    stage.addEventListener('pointercancel', end);
  }

  function setTab(next) {
    if (next === tab) return;
    tab = next;
    if (deps.report) deps.report({ event: 'inspect-tab', tab });
    render();
  }

  function toggle(key) {
    if (expanded.has(key)) expanded.delete(key);
    else expanded.add(key);
    renderTree();
  }

  function clearBoxes() {
    layer.innerHTML = '';
  }

  function renderCard(node) {
    if (!card) return;
    card.innerHTML = '';
    if (!node) {
      if (ui) {
        const hint = document.createElement('p');
        hint.className = 'inspectHint';
        hint.textContent = 'Tap something on the picture to see its details.';
        card.appendChild(hint);
      }
      return;
    }
    const dl = document.createElement('dl');
    dl.className = 'inspectFacts';
    for (const line of describe(node)) {
      const dt = document.createElement('dt');
      const [k, ...rest] = line.split(': ');
      dt.textContent = k;
      const dd = document.createElement('dd');
      dd.textContent = rest.join(': ');
      dl.appendChild(dt);
      dl.appendChild(dd);
    }
    card.appendChild(dl);
  }

  function renderTree() {
    if (!ui) return;
    ui.list.innerHTML = '';
    rowsByKey = new Map();
    if (!tree) return;
    let selectedRow = null;
    for (const r of visibleRows(tree.nodes, expanded)) {
      rowsByKey.set(r.key, r);
      const row = document.createElement('div');
      row.className = 'inspectRow';
      row.dataset.key = r.key;
      row.setAttribute('role', 'treeitem');
      row.style.setProperty('--depth', String(r.depth));
      if (r.hasChildren) row.setAttribute('aria-expanded', r.open ? 'true' : 'false');
      const twisty = document.createElement('span');
      twisty.className = 'inspectTwisty';
      twisty.textContent = r.hasChildren ? (r.open ? '▾' : '▸') : '';
      const { cls, text } = rowLabel(r.node);
      const c = document.createElement('span');
      c.className = 'inspectClass';
      c.textContent = cls;
      const t = document.createElement('span');
      t.className = 'inspectText';
      t.textContent = text;
      row.append(twisty, c, t);
      if (r.node.clickable) {
        const dot = document.createElement('span');
        dot.className = 'inspectDot';
        dot.title = 'clickable';
        row.appendChild(dot);
      }
      if (r.node === selected) {
        row.classList.add('selected');
        row.setAttribute('aria-selected', 'true');
        selectedRow = row;
      }
      ui.list.appendChild(row);
    }
    if (selectedRow && selectedRow.scrollIntoView) selectedRow.scrollIntoView({ block: 'center' });
  }

  function renderDetail() {
    if (!ui) return;
    const node = selected;
    ui.detail.classList.toggle('empty', !node);
    ui.tapBtn.disabled = !node || !node.center;
    ui.copyBtn.disabled = !node || !node.center;
    if (!node) {
      ui.path.textContent = tree ? 'Select a node' : '';
      ui.flags.textContent = '';
      ui.geom.textContent = '';
      return;
    }
    const where = pathTo(tree ? tree.nodes : null, node);
    ui.path.textContent = crumb(where) || node.class || '';
    ui.flags.textContent = flagsOf(node).join(' · ');
    ui.geom.textContent = geometryText(node, tab === '3d' && where.length ? where.length - 1 : null);
  }

  function render() {
    if (ui) {
      for (const [id, b] of Object.entries(ui.tabBtns)) {
        b.setAttribute('aria-selected', id === tab ? 'true' : 'false');
      }
      ui.list.hidden = tab !== 'tree';
      ui.tapped.hidden = tab !== 'tapped';
      ui.scene3d.hidden = tab !== '3d';
      ui.composeBar.hidden = tab === '3d';
      const ce = tree && tree.composeError;
      ui.composeError.textContent = ce || '';
      ui.composeError.hidden = !ce;
      // 3D takes the whole screen (node 7:78); the device is behind it.
      ui.root.classList.toggle('full', tab === '3d');
    }
    renderTree();
    render3d();
    renderCard(selected);
    renderDetail();
    renderBoxes();
  }

  // canvasRect() -> the canvas's box, in the LAYER's own coordinate frame.
  // Both getBoundingClientRect()s are viewport-absolute, so the canvas's
  // offset WITHIN the layer is just the difference of their origins -- see
  // toCss's own comment for why that offset matters (it is 0 unframed, and
  // the display rect's own offset framed).
  function canvasRect() {
    const canvas = deps.screenRect ? deps.screenRect() : { width: 0, height: 0 };
    const layerBox = layer.getBoundingClientRect ? layer.getBoundingClientRect() : { left: 0, top: 0 };
    return {
      width: canvas.width,
      height: canvas.height,
      left: (canvas.left || 0) - (layerBox.left || 0),
      top: (canvas.top || 0) - (layerBox.top || 0),
    };
  }

  // The Tree tab outlines what can be tapped, dashed, and the selection,
  // solid. Tapped keeps the old look, a box on every node.
  function renderBoxes() {
    clearBoxes();
    if (tab === '3d' && !stale) return;
    if (stale || !tree) {
      if (stale) layer.appendChild(renderStaleNotice());
      return;
    }
    const rect = canvasRect();
    const nodes = flatten(tree.nodes, []);
    for (const n of nodes) {
      if (!n.bounds) continue;
      const isSel = n === selected;
      if (tab === 'tree' && !isSel && !n.clickable) continue;
      const css = toCss(n.bounds, tree.width, tree.height, rect);
      const box = document.createElement('div');
      box.className = 'inspectBox';
      if (tab === 'tree' && n.clickable) box.classList.add('clickable');
      else if (!isSel) box.classList.add('plain');
      if (isSel) box.classList.add('selected');
      box.style.left = `${css.left}px`;
      box.style.top = `${css.top}px`;
      box.style.width = `${css.width}px`;
      box.style.height = `${css.height}px`;
      layer.appendChild(box);
    }
  }

  // A rotation leaves the tree stale rather than clearing it outright, and
  // Refresh is how a person acts on that -- without a visible control the
  // only way out was toggling Inspect off and on, which nothing here said.
  function renderStaleNotice() {
    const msg = document.createElement('div');
    msg.className = 'inspectStale';
    const text = document.createElement('span');
    text.textContent = 'Rotated since this tree was read. ';
    msg.appendChild(text);
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'inspectRefresh';
    btn.textContent = 'Refresh';
    btn.addEventListener('click', refresh);
    msg.appendChild(btn);
    return msg;
  }

  // The full tree, not ?interactive=1: the Tree tab shows the TextViews and
  // ImageViews the pruned tree drops, and those are what name a row.
  async function fetchTree() {
    const fetchImpl = deps.fetchImpl || (typeof fetch !== 'undefined' ? fetch : null);
    const compose = composeOn ? '&compose=1' : '';
    const res = await fetchImpl(`/api/ui?target=${encodeURIComponent(deps.targetId() || '')}${compose}`);
    if (!res.ok) throw new Error(await errorMessage(res));
    return res.json();
  }

  async function refresh() {
    const myGen = ++gen;
    // Captured up front and compared again after the await: `gen` alone
    // catches off()/run()/a rotation, all of which are explicit calls into
    // this module, but a target switch is not one -- app.js's reconnect()
    // does call inspect.off() today, but nothing here should depend on every
    // caller remembering to (review HIGH-3).
    const myTarget = deps.targetId ? deps.targetId() : null;
    // A refresh keeps the selection when the same node is still there, by
    // path, so Tap it followed by Refresh does not lose the row.
    const keepKey = selected && tree ? (pathTo(tree.nodes, selected).pop() || {}).key : null;
    try {
      const t = await fetchTree();
      if (myGen !== gen) return; // superseded by off()/run()/a rotation
      if (deps.targetId && deps.targetId() !== myTarget) return; // superseded by a target switch
      if (treeIsStale(t, streamW, streamH)) {
        // The device rotated between this request going out and its
        // response landing. Discard rather than store: nothing downstream
        // should ever see a tree whose own orientation disagrees with the
        // stream it would be drawn against (review round 3, finding 2).
        stale = true;
        selected = null;
        render();
        return;
      }
      tree = t;
      stale = false;
      selected = keepKey ? nodeByKey(t.nodes, keepKey) : null;
      expanded = initialExpanded(t.nodes);
      if (selected) openPathTo(selected);
      render();
    } catch (err) {
      if (myGen !== gen) return;
      if (deps.targetId && deps.targetId() !== myTarget) return;
      const message = err && err.message ? err.message : String(err);
      if (deps.toast) showToast(deps.toast, message);
      if (deps.report) deps.report({ event: 'inspect-error', err: message });
      // A failed fetch leaves inspect off rather than on with nothing to
      // show: there is nothing useful a tap can do against no tree.
      setOn(false);
    }
  }

  function nodeByKey(nodes, key) {
    let level = nodes;
    let n = null;
    for (const i of key.split('.')) {
      n = level && level[Number(i)];
      if (!n) return null;
      level = n.children;
    }
    return n;
  }

  // openPathTo expands every ancestor of node, so a selection made on the
  // stream shows up in the list.
  function openPathTo(node) {
    const path = pathTo(tree.nodes, node);
    for (const p of path.slice(0, -1)) expanded.add(p.key);
  }

  function select(node, via) {
    selected = node;
    if (node && tree) openPathTo(node);
    render();
    if (node && deps.report) deps.report({ event: 'inspect-select', via, tab, class: node.class || '' });
  }

  function onLayerClick(ev) {
    if (!tree || stale) return;
    const rect = canvasRect();
    // clientX/Y are viewport-absolute already, same as any
    // getBoundingClientRect() origin -- so the canvas's own absolute left/top
    // (deps.screenRect(), not the layer-relative `rect` above) is what
    // clicks are measured against, not the layer's.
    const canvas = deps.screenRect ? deps.screenRect() : { width: 0, height: 0, left: 0, top: 0 };
    const px = ev.clientX - (canvas.left || 0);
    const py = ev.clientY - (canvas.top || 0);
    const sx = rect.width > 0 ? tree.width / rect.width : 0;
    const sy = rect.height > 0 ? tree.height / rect.height : 0;
    const x = px * sx;
    const y = py * sy;
    select(tab === 'tree' ? pickTarget(tree.nodes, x, y) : nodeAt(tree.nodes, x, y), 'stream');
  }

  // Tap it goes out as this viewer's own input (deps.postInput carries
  // X-Loupe-Viewer), so it is never refused as an agent's. The tree is
  // fetched again shortly after, since the tap usually changes the screen.
  async function tapSelected() {
    if (!selected || !selected.center || !deps.postInput) return;
    const body = tapRequest(selected);
    try {
      await deps.postInput(body);
      if (deps.report) deps.report({ event: 'inspect-tap', x: body.x, y: body.y });
      const myGen = gen;
      setTimeout(() => { if (on && myGen === gen) refresh(); }, 700);
    } catch (err) {
      const message = err && err.message ? err.message : String(err);
      if (deps.toast) showToast(deps.toast, message);
      if (deps.report) deps.report({ event: 'inspect-tap', err: message });
    }
  }

  // Safari allows a clipboard write only inside the tap, so this runs
  // straight from the click with nothing awaited first (see clipboard.js).
  async function copySelected() {
    if (!selected || !selected.center) return;
    const text = JSON.stringify(tapRequest(selected));
    const write = deps.writeClipboard
      || ((s) => navigator.clipboard.writeText(s));
    try {
      await write(text);
      if (deps.toast) showToast(deps.toast, 'Copied');
      if (deps.report) deps.report({ event: 'inspect-copy', ok: true });
    } catch (err) {
      if (deps.toast) showToast(deps.toast, 'Could not copy');
      if (deps.report) deps.report({ event: 'inspect-copy', ok: false });
    }
  }

  function setOn(next) {
    gen++; // invalidate any fetch started before this transition
    on = next;
    layer.hidden = !on;
    if (ui) ui.root.hidden = !on;
    else if (card) card.hidden = !on;
    if (!on) {
      clearBoxes();
      renderCard(null);
      tree = null;
      selected = null;
      stale = false;
      expanded = new Set();
      view = { ...DEFAULT_ORBIT };
      spacing = SPACING_DEFAULT;
      if (ui) {
        ui.list.innerHTML = '';
        ui.world.innerHTML = '';
        ui.slider.value = String(spacing);
        ui.root.classList.remove('full');
        renderDetail();
      }
    } else if (ui) {
      tab = deps.tab || 'tapped';
      render();
    }
    if (deps.report) deps.report({ event: 'inspect', on });
    if (deps.onToggle) deps.onToggle(on);
  }

  layer.addEventListener('click', onLayerClick);

  function run() {
    if (on) { setOn(false); return; }
    setOn(true);
    refresh();
  }

  // off() is idempotent and safe to call whether or not inspect is on --
  // app.js calls it unconditionally on every target switch (trap 41) rather
  // than tracking whether inspect happened to be running.
  function off() {
    if (on) { setOn(false); return; }
    gen++; // still discard a fetch left in flight from before the switch
  }

  function notifyResize(w, h) {
    streamW = w;
    streamH = h;
    if (!on) return;
    if (treeIsStale(tree, w, h)) {
      stale = true;
      gen++; // a fetch already in flight was requested against the old orientation
      renderBoxes();
    }
  }

  // Boxes are computed from the canvas's CSS box, so anything that moves
  // that box (fitScreen, on a stage resize or the frame art loading) has to
  // repaint them too (review LOW/MEDIUM-7). app.js calls this from the same
  // path fitScreen() already runs on.
  function relayout() {
    if (!on) return;
    renderBoxes();
    if (tab === '3d') render3d();
  }

  function destroy() {
    layer.removeEventListener('click', onLayerClick);
  }

  return {
    label: 'Inspect',
    run,
    off,
    refresh,
    isOn: () => on,
    notifyResize,
    relayout,
    destroy,
  };
}
