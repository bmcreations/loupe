// Loupe browser client.
//
// The job here is narrow: take H.264 access units off a WebSocket, push them
// through WebCodecs into a canvas, and send pointer events back. Everything
// else is measurement, because the only question this client exists to answer
// is whether an iPhone holds 60fps with input latency low enough to feel live.

// ---------------------------------------------------------------------------
// Protocol. Must stay in lockstep with internal/session/protocol.go.
// ---------------------------------------------------------------------------
import { JitterBuffer, ArrivalJitter } from './jitter.js';
import { connect as connectTransport } from './transport.js';
import { createPane } from './pane.js';
import { createPanes, parseTargets, buildSearch, paneShortcut, paneKey } from './panes.js';
import { model as paneHeaderModel, mount as mountPaneHeader } from './paneheader.js';
import { showToast } from './toast.js';
import { createRotator, closedExhausted, MSG_ROTATE } from './rotate.js';
import { frameNeedsRecheck, frameSuitsStream, frameWantedFrom, orientationOf, turnedArt } from './orientation.js';
import { fitStage, placeArt, gridCells } from './layout.js';
import { mount as mountSheet, HARDWARE_KEYS } from './sheet.js';
import { readHand, writeHand, applyHand, safeStorage } from './handedness.js';
import { readPanels, writePanels, toggle as togglePanel, applyPanels } from './panels.js';
import { readAppearance, writeAppearance, applyAppearance, themeColor, themeName } from './appearance.js';
import { dotFor, secondRtt } from './health.js';
import { paintPill } from './pill.js';
import { mount as mountDrawer, readActionResult } from './drawer.js';
import { mount as mountNewDevice, canCreateFrom, awaitListed } from './newdevice.js';
import { mount as mountServers } from './servers.js';
import { mountConnectHelp, mountSwitchServer, rememberServer } from './helpsheets.js';
import { fetchTargets, latestOnly, chooseTarget, failureReason, sameShown } from './targets.js';
import { mount as mountCorners, pageLink } from './corners.js';
import { mount as mountFold } from './fold.js';
import { mount as mountAppCard } from './appcard.js';
import { mount as mountDeviceSettings } from './devicesettings.js';
import { mount as mountInspect } from './inspect.js';
import { mount as mountEventLog, mountLogPage, decodeEvent } from './eventlog.js';
import { mount as mountImmersive, centreFit } from './immersive.js';
import { mount as mountDoubleTap } from './doubletap.js';
import { mount as mountKeyboard, chunkText, readStripPref } from './keyboard.js';
import { mount as mountClipboard } from './clipboard.js';
import { mount as mountDrop } from './drop.js';
import { mount as mountRecord } from './record.js';
import { mount as mountFlag, flagShortcut } from './flag.js';
import { mount as mountTouches } from './touches.js';
import { mount as mountCursors } from './cursor.js';
import { mount as mountExtOverlays } from './extoverlay.js';
import { mount as mountStreamConfig } from './streamconfig.js';
import { mount as mountControl, MSG_CONTROL, MSG_TAKE_OVER } from './control.js';
import { mount as mountPip } from './pip.js';
import { mount as mountPeek } from './peek.js';
import { mount as mountZoom } from './zoom.js';
import { collect as collectLayout, check as checkLayout } from './livecheck.js';
import { connectionRows } from './connstats.js';
import { mountOverlay, ask, connectingState, reconnectingState, unreachableState, cantStreamState, noDevicesState, unsupportedState, shutDownState, afterClose, afterResume, endedFromClose } from './states.js';

const isMac = /Mac|iPhone|iPad/.test(navigator.platform || '');

// Reported in client stats so a stale cached client is obvious in the trace
// rather than something to deduce from its behaviour.
const CLIENT_BUILD = 'jitter-pts-1';

// Report that the module reached evaluation. The boot probe in index.html logs
// a 'load' event unconditionally, so a trace with 'boot' and 'load' but no
// 'module' pins the failure to module evaluation rather than to the network.
const report = (rec) => { try { window.__loupeLog && window.__loupeLog(rec); } catch (e) {} };
report({ event: 'module', build: CLIENT_BUILD });

const MSG_EVENT = 0x06;
const MSG_CLIPBOARD = 0x07;
// An agent's move: Event JSON for the cursors only, never the log (session.MsgCursor).
const MSG_CURSOR = 0x0B;
// An extension's overlays on this target, the whole set (session.MsgOverlay).
const MSG_OVERLAY = 0x0C;

// MsgTouch (0x10) and MsgScroll (0x12) are the pane's (pane.js).
const MSG_KEY = 0x11;
// 0x15 is MsgHello, which transport.js sends itself.
const MSG_TEXT = 0x16;
const textEncoder = new TextEncoder();

const KEY_DOWN = 0, KEY_UP = 1;

// ---------------------------------------------------------------------------
// DOM
// ---------------------------------------------------------------------------
// Told when the focused pane's stream changes size, which on a foldable is
// the other panel's picture arriving. Set once the fold button is mounted.
let onReshape = null;
// pip.js's handle, set at mount; null unless ?pip=1 on a browser that can float a video.
let pip = null;
// zoom.js's handle, set at mount; fitScreen runs before that, so it is null at first.
let zoomMode = null;
// corners' Record item, repainted when the focused pane's recording flips or
// focus moves to a pane whose recording differs.
const recordListeners = [];

// peek.js's handle. Mounted here rather than with the other modules because
// layoutScreen, which runs from the first frame, sizes a peek's window.
const peek = mountPeek(window, { targetId: () => focused().targetId, report });

// The page's own elements. Each pane's canvas, bezel, layers, HUD and
// overlay are a clone of #paneTemplate (makePaneEls), found on p.els.
const el = {
  stage: document.getElementById('stage'),
  paneTemplate: document.getElementById('paneTemplate'),
  pill: document.getElementById('pill'),
  pillName: document.getElementById('pillName'),
  pillDot: document.getElementById('pillDot'),
  pillTag: document.getElementById('pillTag'),
  drawer: document.getElementById('drawer'),
  newDevice: document.getElementById('newDevice'),
  connectHelp: document.getElementById('connectHelp'),
  switchServer: document.getElementById('switchServer'),
  addServer: document.getElementById('addServer'),
  scrim: document.getElementById('scrim'),
  toolsBtn: document.getElementById('toolsBtn'),
  sheet: document.getElementById('sheet'),
  cornerNav: document.getElementById('cornerNav'),
  cornerMore: document.getElementById('cornerMore'),
  immersiveHandle: document.getElementById('immersiveHandle'),
  toast: document.getElementById('toast'),
  // The page's overlay: unsupported, unreachable, no devices. A pane's own
  // states (connecting, can't stream) are on its clone's .overlay.
  overlay: document.getElementById('overlay'),
};


// Handedness: applied before anything else paints, so the corner clusters
// never flash in the default position before swapping to a stored preference.
applyHand(document.documentElement, readHand(safeStorage()));

// Appearance: index.html already set data-theme before first paint; this
// repeats it (harmless) and keeps the status bar's theme-color in step,
// including when the phone itself switches between light and dark.
const darkQuery = window.matchMedia('(prefers-color-scheme: dark)');
function paintThemeColor(appearance) {
  const color = themeColor(appearance, darkQuery.matches);
  for (const m of document.querySelectorAll('meta[name="theme-color"]')) m.content = color;
  // The manifest's colours follow too, for a Home Screen add from here. The
  // token reaches it through the cookie, not this URL.
  const manifest = document.querySelector('link[rel="manifest"]');
  if (manifest) manifest.href = `/manifest.webmanifest?theme=${themeName(appearance, darkQuery.matches)}`;
}
let appearance = readAppearance(safeStorage());
applyAppearance(document.documentElement, appearance);
paintThemeColor(appearance);
darkQuery.addEventListener('change', () => paintThemeColor(appearance));

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------
// The page's own state. Everything about the target's stream -- the
// connection, the decoder, the metrics, the health verdict -- is the pane's
// (pane.js).
const state = {
  // The pill's label: the current target's, updated by loadTargets and by
  // the drawer's onPick.
  targetLabel: '',
  // The toggle is what the user asked for and the default is off: a bezel
  // costs about 10% of each axis, and trap 18 was the device screen not
  // fitting on iOS in the first place. The bezel itself is pane.frame.
  frameWanted: false,
};

const params = new URLSearchParams(location.search);

// The stats HUD is a page preference shown on every pane (Viewer group).
let hudOn = false;
try { hudOn = localStorage.getItem('loupe.hud') === '1'; } catch (_) {}

// makePaneEls clones #paneTemplate. The clone has no ids, so each part is
// found by its data-r role and each readout by its data-m name. The event
// log's list and count are made here too: the sheet's Log page shows the
// focused pane's.
function makePaneEls() {
  const root = el.paneTemplate.content.firstElementChild.cloneNode(true);
  const r = (name) => root.querySelector(`[data-r="${name}"]`);
  const m = {};
  for (const node of root.querySelectorAll('[data-m]')) m[node.dataset.m] = node;
  const logList = document.createElement('div');
  logList.className = 'logList';
  const logBadge = document.createElement('span');
  logBadge.className = 'logBadge';
  logBadge.textContent = '0';
  return {
    root,
    header: r('header'),
    wrap: r('wrap'),
    canvas: r('canvas'),
    frameArt: r('frameArt'),
    frameMask: r('frameMask'),
    frameCutout: r('frameCutout'),
    frameCutoutPath: r('frameCutoutPath'),
    touchLayer: r('touchLayer'),
    cursorLayer: r('cursorLayer'),
    extLayer: r('extLayer'),
    inspectLayer: r('inspectLayer'),
    flagLayer: r('flagLayer'),
    hud: r('hud'),
    overlay: r('overlay'),
    logList,
    logBadge,
    m,
  };
}

// createPaneFor builds one pane: its clone in #stage, and the target's
// connection, decoder and render loop, its canvas input, and the modules
// that draw over it or hold its session (pane.js). panes.js calls it.
function createPaneFor(targetId, id) {
  const els = makePaneEls();
  els.hud.hidden = !hudOn;
  el.stage.appendChild(els.root);
  const isFront = () => focused() === p;
  const p = createPane({
    id,
    targetId,
    els,
    lib: {
      connectTransport, ArrivalJitter, JitterBuffer, createRotator, closedExhausted, MSG_ROTATE,
      secondRtt, mountOverlay, reconnectingState, shutDownState, connectingState,
      afterClose, afterResume, endedFromClose,
    },
    modules: {
      touches: mountTouches, cursor: mountCursors, overlays: mountExtOverlays, control: mountControl,
      inspect: mountInspect, eventlog: mountEventLog, record: mountRecord, flag: mountFlag,
    },
    moduleDeps: {
      doc: document,
      fetch: (url, init) => fetch(url, init),
      toast: el.toast,
      storage: safeStorage(),
      // Inspect's sheet sits under the device, and html.inspecting gives the
      // stage a bottom padding of the sheet's height, so layoutScreen fits the
      // device into the space above it without knowing about inspect.
      inspectSheet: document.getElementById('inspectSheet'),
      pillRec: document.getElementById('pillRec'),
      // Flag mode takes the pane's pointer, so the zoom transform goes first.
      // A phone's keyboard would cover the picture the person is boxing, so
      // the note is focused only with a fine pointer.
      beforeFlag: () => { if (zoomMode) zoomMode.exit(); },
      focusFlagNote: () => !(window.matchMedia && matchMedia('(pointer: coarse)').matches),
      // The pill is the focused pane's, so only it paints the red dot.
      isFront: () => isFront(),
      postInput: (pp, body) => postViewerInput(body, pp),
      takeOver: (pp) => {
        if (!pp.open) throw new Error('not connected');
        pp.send(new Uint8Array([MSG_TAKE_OVER]).buffer);
      },
      // Another viewer started or stopped holding the pane's device: the
      // Tools sheet's Control section follows the focused pane.
      onHeldByOther: (pp, on) => {
        pp.heldByOther = on;
        if (focused() === pp) refreshToolSections();
      },
      // html.inspecting is the page's, so only the focused pane's inspect
      // sets it; focusing another pane turns the old one's off first.
      onInspectToggle: (pp, on) => {
        if (!isFront()) return;
        document.documentElement.classList.toggle('inspecting', on);
        fitScreen();
      },
      // sheetHandle is mounted further down; this runs only from a render.
      // The Log page shows the focused pane's list.
      logIsOpen: () => isFront() && sheetHandle.detent !== 'closed' && sheetHandle.page === 'log',
    },
    // The pane hands decode a plain chunk init; WebCodecs wants the chunk built.
    createDecoder: (init) => {
      const dec = new VideoDecoder(init);
      return {
        configure: (config) => dec.configure(config),
        decode: (chunk) => dec.decode(new EncodedVideoChunk(chunk)),
        close: () => dec.close(),
        get state() { return dec.state; },
        get decodeQueueSize() { return dec.decodeQueueSize; },
      };
    },
    raf: (fn) => requestAnimationFrame(fn),
    cancelRaf: (handle) => cancelAnimationFrame(handle),
    now: () => performance.now(),
    setTimeout: (fn, ms) => setTimeout(fn, ms),
    clearTimeout: (handle) => clearTimeout(handle),
    hidden: () => document.hidden,
    visibility: () => document.visibilityState,
    // PiP floats the focused pane's canvas only.
    pipActive: () => isFront() && !!(pip && pip.active()),
    // ?transport=ws forces the WebSocket path, for measuring the two against
    // each other on the same link.
    preferWebTransport: params.get('transport') !== 'ws',
    build: CLIENT_BUILD,
    report,
    label: (t) => labelFor(t),
    openPicker: () => openPicker(),
    onInit: (pp, info) => onPaneInit(pp, info),
    onShape: (pp) => onPaneShape(pp),
    onStatus: (pp) => { if (isFront()) paintConnection(); paintPaneHeader(pp); },
    onStats: (pp) => { if (isFront()) refreshPill(); paintPaneHeader(pp); },
    onClosed: (pp) => onPaneClosed(pp),
    onConnectFailed: (pp, targetId) => explainFailure(pp, targetId),
    onControlMessage: (pp, type, body) => routeControl(pp, type, body),
    // A completed tap or swipe is the one gesture likely to change the
    // foreground app; notifyActivity's own 2s throttle (design spec, appcard.js's
    // shouldRefetch) is what keeps a drag's pointerup from turning into a
    // refetch storm. The App card is the focused pane's.
    onTouchEnd: () => { if (isFront()) appCard.notifyActivity(); },
  });
  p.header = mountPaneHeader(els.header, { onClose: () => closePane(p) });
  p.mods.record.onChange(() => { if (isFront()) for (const fn of recordListeners) fn(); });
  // Any press on a pane, its header or its device, focuses it first. Capture
  // phase, so the canvas's own pointerdown (the touch it sends) comes after
  // focus has moved and the page's actions already point at this pane.
  els.root.addEventListener('pointerdown', () => panes.focus(p.id), true);
  // Both images are laid out from their natural size, which is 0 until they
  // decode, so the first fit after a target change is the one that would get
  // it wrong. The mask carries the punch hole, so it needs this as much.
  els.frameArt.addEventListener('load', fitScreen);
  els.frameMask.addEventListener('load', fitScreen);
  return p;
}

// destroyPane is close's teardown: the pane's own recording stops (never an
// agent's), its connection closes and its clone leaves the page.
function destroyPane(p) {
  p.mods.inspect.off();
  p.mods.flag.onTargetSwitch();
  p.mods.record.onTargetSwitch(null);
  p.close();
  p.els.root.remove();
}

// closePane is the header's close button: the pane's recording stops, its
// session closes and the others take its space. The last one is never closed.
function closePane(p) {
  if (panes.list().length < 2) return;
  report({ event: 'pane-close', pane: p.id, target: p.targetId, count: panes.list().length - 1 });
  panes.close(p.id);
}

// paintPaneHeader repaints p's strip; it is shown only side by side.
function paintPaneHeader(p) {
  if (!p.header) return;
  const index = panes.indexOf(p.id);
  if (!index) return;
  const isFocused = focused() === p;
  p.els.root.classList.toggle('focused', isFocused);
  p.els.header.hidden = panes.list().length < 2;
  p.header.paint(paneHeaderModel(p, { index, focused: isFocused, now: performance.now(), key: paneKey(isMac) }));
}

// The targets of panes closed by undocking, reopened on docking again.
let hiddenTargets = [];

// The open panes (panes.js). There is always at least one: the page opens
// the first below, and a pane's close button is only offered side by side.
const panes = createPanes({ create: createPaneFor, destroy: destroyPane, max: 4 });

// Page modules that act on "the device" read it through focused(), never a
// pane captured at mount: focus moves between panes.
function focused() { return panes.focused(); }

// Docked desktop opens every target the URL names, side by side; a phone
// opens the first (syncDock reopens the rest on docking). Before the target
// list arrives a pane may have no target: loadTargets picks one.
const urlTargets = parseTargets(location.search);
panes.open(urlTargets[0] || null);
hiddenTargets = urlTargets.slice(1);

// The page's overlay, for what no pane can show: unsupported, unreachable,
// no devices. hideOverlay takes it down; each pane hides its own on its first
// frame.
const pageOverlay = mountOverlay(el.overlay, { report });

function hideOverlay() { pageOverlay.hide(); }

// labelFor names a target for a pane's overlay, falling back to the label
// its pane last had from the list.
function labelFor(targetId) {
  const t = lastTargets.find((x) => x.id === targetId);
  if (t) return t.label;
  const p = panes.byTarget(targetId);
  return (p && p.targetLabel) || targetId;
}


// refreshPill paints the pill's label and dot together (pill.js owns writing
// both, so nothing can update one and forget the other). The dot comes from
// health.js's dotFor, not from pane.status: client Task 5 moves the dot
// from a 3-state connection light to the server's 5-state per-second
// verdict, so it keeps reading 'good' through a connection blip and only
// goes 'stale' once 2.5s have passed with no fresh MsgStats.
function refreshPill() {
  const p = focused();
  const st = p.stats();
  paintPill(el, {
    label: p.targetLabel || p.targetId,
    headless: !!p.targetHeadless,
    dot: dotFor(
      { health: st.health, healthReason: st.healthReason, receivedAt: st.statsReceivedAt },
      performance.now()
    ),
  });
}

// What the dot means, spelled out. The dot is three colours and no words, so
// this is the answer to "what is it actually doing" that the menu exists for.
const STATUS_TEXT = {
  live: 'streaming',
  error: 'disconnected',
  idle: 'connecting',
};

// ---------------------------------------------------------------------------
// Capability gate
// ---------------------------------------------------------------------------
if (typeof VideoDecoder === 'undefined') {
  pageOverlay.show(unsupportedState());
  throw new Error('WebCodecs unavailable');
}

// ---------------------------------------------------------------------------
// Targets and the device picker drawer
// ---------------------------------------------------------------------------

// Set from every /api/targets fetch (loadTargets, and each time the drawer
// opens), so onPick can find the label for the id it was handed without a
// second round trip.
let lastTargets = [];
let canCreate = false; // set once /api/devices/options answers; see canCreateReady

// fetchTargetsLatest is fetchTargets (the pure round trip + shape check in
// targets.js) wrapped in latestOnly, shared by loadTargets and the pill's
// refresh so both draw tickets from the same sequence -- a click during the
// initial load and the initial load itself are exactly the race this
// guards.
//
// `current` from latestOnly answers one question only: is this the most
// recently STARTED call, i.e. is it safe for this response to win the
// shared lastTargets cache and the drawer's render. It says nothing about
// whether THIS call's own request succeeded, and loadTargets below does not
// gate its own connect()-or-overlay decision on it -- seeing `current:
// false` used to be read as "a later fetch owns the outcome, do nothing",
// which stranded the page on "Connecting" forever when the later fetch was
// a pill click that only needed to refresh the drawer, not replace the
// initial connect. See targets.js for the full account.
const fetchTargetsLatest = latestOnly(() => fetchTargets(fetch));

async function loadTargets() {
  const { current, result } = await fetchTargetsLatest();
  const { list, failed } = result;
  // This call acts on its OWN {list, failed} unconditionally, whether or
  // not `current` is true -- see the comment on fetchTargetsLatest above.
  // The page must connect (or show the right overlay) off the response
  // loadTargets itself made the request for, not off whatever the most
  // recent /api/targets caller happened to be.
  if (failed) {
    pageOverlay.show(unreachableState({ hostname: location.hostname }), { retry: loadTargets, switchServer: openSwitchServer });
    return;
  }

  // A server that answered is one worth offering in Switch server later.
  rememberServer(safeStorage(), location.origin);

  if (list.length === 0) {
    await canCreateReady;
    pageOverlay.show(noDevicesState({ canCreate }), { retry: loadTargets, newDevice: openNewDevice, connectHelp: openConnectHelp });
    return;
  }

  // The shared cache and the drawer, by contrast, only take a response that
  // is still the most recent one asked for -- an older list landing after a
  // newer one must not overwrite it merely because its round trip was
  // slower.
  // A docked picker is opened by syncDock before this first list lands,
  // and its own fetch loses to this one, so it is repainted here; refresh
  // does nothing while the picker is closed.
  if (current) {
    lastTargets = list;
    drawer.refresh(list);
  }

  // A target named in the URL is kept even when it cannot open, so the
  // overlay can say what happened to it. Without one, a device adb has lost
  // is not picked over one that can stream. Only the first pane can be
  // without one: every other pane was opened on a target.
  pageOverlay.hide();
  for (const p of panes.list()) {
    const asked = p.targetId;
    // With other panes open, a pane whose target the list lacks closes. Its
    // fallback would open some other device beside them (a physical iPhone,
    // on a page naming a simulator the server could not list), or one
    // another pane already shows, and two viewers on one target garble both.
    // The last pane falls back as a single pane always has.
    const several = panes.list().length > 1;
    const chosen = chooseTarget(list, asked, { strict: several && list.length > 0 });
    if (several && asked && !chosen && list.length > 0) { panes.close(p.id); continue; }
    // Another target, or a first one when the URL named none, is a switch:
    // switchTarget takes a new generation (trap 41), and its own connect
    // follows, so this one must not start a second beside it.
    const switched = chosen !== asked;
    const holder = switched && panes.byTarget(chosen);
    if (holder && holder !== p && several) { panes.close(p.id); continue; }
    if (switched) p.switchTarget(chosen);
    const found = list.find(t => t.id === p.targetId);
    applyTarget(p, found);
    // pageshow has usually connected already, to the target the URL named,
    // before this list arrives. Connecting again replaced a working session
    // about 2s into every page load (22:28:00 and 22:28:02 on 26 September),
    // costing the viewer its keyframe and a reconnect.
    if (!switched && (p.connecting || p.open)) {
      // A Try again after the server came back finds the session already up.
      if (p.open && p.hasDecoder) p.overlay.hide();
      continue;
    }
    p.overlay.show(connectingState({ label: p.targetLabel, kind: found && found.kind, booting: found && found.bootsOnOpen }));
    if (!switched) p.connect();
  }
  syncUrl();
}

// applyTarget keeps what the target list says about p's target on the pane,
// and repaints the page's chrome when p is the focused one.
function applyTarget(p, t) {
  p.targetLabel = t ? t.label : p.targetId;
  p.targetHeadless = !!(t && t.headless);
  p.mods.record.sync();
  paintPaneHeader(p);
  if (focused() === p) {
    drawer.setCurrent(p.targetId);
    corners.setOS(t ? t.os : undefined);
    showLogOf(p);
    refreshPill();
  }
}

// syncUrl writes the open panes' targets into the URL, the focused one first
// so a reload or a phone opening the link starts on it. A phone that undocked
// keeps the closed panes' targets in `hiddenTargets` so a re-dock reopens them.
function syncUrl() {
  const f = focused();
  const ids = [];
  if (f && f.targetId) ids.push(f.targetId);
  for (const p of panes.list()) if (p.targetId && !ids.includes(p.targetId)) ids.push(p.targetId);
  for (const t of hiddenTargets) if (!ids.includes(t)) ids.push(t);
  const search = buildSearch(location.search, ids);
  if (search !== location.search) history.replaceState(null, '', location.pathname + search + location.hash);
}

// onPick does what the old <select>'s change handler did: set the focused
// pane's target, replace the URL, and reconnect. pane.switchTarget is the
// only path besides connect and suspend that touches the pane's generation
// (trap 41), so this is not a second connect path -- it drives the same one
// the initial load and the auto-reconnect timer use. A target open in another
// pane is focused there instead: two viewers on one target garble both.
function onPickTarget(id) {
  const pane = focused();
  // A pick of the current target is a no-op, except from the ended screen: a
  // deleted AVD can be recreated under the same name and picked again.
  if (id === pane.targetId && !pane.ended) return;
  const other = panes.byTarget(id);
  if (other && other !== pane) {
    panes.focus(other.id);
    return;
  }
  report({ event: 'drawer-pick', pane: pane.id, from: pane.targetId, to: id });
  const t = lastTargets.find((x) => x.id === id);
  // Clears the outgoing target's verdict rather than carrying it forward:
  // trap 38's shape one layer up. Nothing about the new target's health has
  // arrived yet, and dotFor's NaN-receivedAt branch is what reads that as
  // idle instead of stale-with-a-stale-verdict. It also stops this page's
  // recording on the old target, closes the old connection, tears down its
  // decoder, resets the pane's inspect, markers, cursors and control banner,
  // and connects again on a timer.
  pane.switchTarget(id);
  // Explicit rather than relying solely on the close event the switch
  // triggers: the nav row should not wait on an async close to disappear for
  // a target it no longer belongs to.
  corners.setControl(false);
  // App/Device settings re-fetch immediately rather than waiting for the
  // sheet's next open: each module resets its own per-target 501 state as
  // soon as targetId() reports a new id (review HIGH-1), but a section that
  // was showing A's app/settings should not keep showing them, stale, under
  // B's name for however long the sheet happens to stay open.
  appCard.refresh();
  deviceSettings.refresh();
  applyTarget(pane, t);
  syncUrl();
  pane.overlay.show(connectingState({ label: pane.targetLabel, kind: t && t.kind, booting: t && t.bootsOnOpen }));
}

function docked() { return dockQuery.matches; }

// onOpenBeside opens a target in a new pane beside the others (docked only).
// A target already open is focused where it is; a fifth is refused.
function onOpenBeside(id) {
  if (!docked()) { onPickTarget(id); return; }
  if (panes.byTarget(id)) { panes.focus(panes.byTarget(id).id); return; }
  if (panes.full()) { showToast(el.toast, 'Four devices is the most side by side', 'more'); return; }
  const t = lastTargets.find((x) => x.id === id);
  const p = panes.open(id);
  report({ event: 'pane-open', pane: p.id, target: id, count: panes.list().length });
  applyTarget(p, t);
  p.overlay.show(connectingState({ label: p.targetLabel, kind: t && t.kind, booting: t && t.bootsOnOpen }));
  p.connect();
}

// onPickerAction runs a row's swipe action. A shutdown or delete the server
// refuses (409 busy, recording or humanHeld, 422 notSupported) throws the
// message for the drawer to toast; delete asks first, since it cannot be
// undone. Either way the list is fetched again, as the poll does, because
// the row's state or presence has changed.
async function onPickerAction(id, action) {
  const t = lastTargets.find((x) => x.id === id);
  const label = t ? t.label : id;
  if (action === 'handsoff' || action === 'allow' || action === 'release') {
    await agentRowAction(id, action, t);
    return;
  }
  if (action === 'delete') {
    const running = !!t && !t.bootsOnOpen;
    const answer = await ask({
      title: `Delete ${label}?`,
      message: running ? 'It is running and shuts down first. This cannot be undone.' : 'This cannot be undone.',
      actions: [{ id: 'delete', label: 'Delete', destructive: true }],
    });
    if (answer !== 'delete') {
      report({ event: 'picker-delete', kind: t && t.kind, target: id, outcome: 'cancelled' });
      return;
    }
  }
  const event = action === 'delete' ? 'picker-delete' : 'picker-shutdown';
  const url = action === 'delete' ? '/api/devices' : '/api/devices/shutdown';
  let res;
  try {
    res = await fetch(`${url}?target=${encodeURIComponent(id)}`, {
      method: action === 'delete' ? 'DELETE' : 'POST',
      headers: focused().viewerHeaders(),
    });
  } catch (err) {
    report({ event, kind: t && t.kind, target: id, outcome: 'network' });
    throw new Error('Could not reach the server');
  }
  const result = await readActionResult(res);
  if (!result.ok) {
    report({ event, kind: t && t.kind, target: id, outcome: result.outcome });
    pollTargets();
    throw new Error(result.message);
  }
  report({ event, kind: t && t.kind, target: id, outcome: 'ok' });
  const { current, result: latest } = await fetchTargetsLatest();
  if (!latest.failed && current) takeTargets(latest.list);
}

// agentRowAction marks a row hands off, allows agents again, or ends someone
// else's claim on it. Release is forced: the person in the picker outranks
// any agent's claim, and asks first, since it lets another agent in.
async function agentRowAction(id, action, t) {
  const event = action === 'release' ? 'picker-release' : 'picker-handsoff';
  if (action === 'release') {
    const who = (t && t.claim && (t.claim.agent || t.claim.session)) || 'an agent';
    const answer = await ask({
      title: `End ${who}'s claim?`,
      message: 'Another agent can then drive this device. The one that claimed it is not told.',
      actions: [{ id: 'release', label: 'End Claim', destructive: true }],
    });
    if (answer !== 'release') {
      report({ event, target: id, outcome: 'cancelled' });
      return;
    }
  }
  const url = action === 'release'
    ? `/api/claims?target=${encodeURIComponent(id)}&force=1`
    : `/api/devices/handsoff?target=${encodeURIComponent(id)}&on=${action === 'handsoff' ? 1 : 0}`;
  let res;
  try {
    res = await fetch(url, { method: action === 'release' ? 'DELETE' : 'POST', headers: focused().viewerHeaders() });
  } catch (err) {
    report({ event, target: id, on: action === 'handsoff', outcome: 'network' });
    throw new Error('Could not reach the server');
  }
  const result = await readActionResult(res);
  report({ event, target: id, on: action === 'handsoff', outcome: result.outcome });
  if (!result.ok) throw new Error(result.message);
  const { current, result: latest } = await fetchTargetsLatest();
  if (!latest.failed && current) takeTargets(latest.list);
}

const drawer = mountDrawer(el.drawer, {
  scrim: el.scrim,
  trigger: el.pill,
  toast: el.toast,
  onPick: onPickTarget,
  onAction: onPickerAction,
  fps: () => focused().stats().decFps,
  onNewDevice: () => openNewDevice(),
  // Side by side is docked only; at phone width the picker is single pane.
  openIds: () => (docked() ? panes.list().map((p) => p.targetId) : null),
  onOpenBeside,
  paneKey: paneKey(isMac),
  onAddServer: () => openAddServer(),
  storage: safeStorage(),
});

// The New device sheet opens over the picker (z-index above it, same
// scrim), so Cancel lands back on the list it came from. A 202 means the
// server already lists the creation as a row, so the picker is refreshed
// and opened to show it -- from No devices it was not open.
let createdFrom; // the target in front when a create was accepted
const newDevice = mountNewDevice(el.newDevice, {
  report,
  toast: el.toast,
  onCreated: async () => {
    createdFrom = focused().targetId;
    if (!drawer.isOpen()) await openPicker();
    pollTargets();
  },
  // An iOS create's `sim-new:` row is replaced by the real `sim:<udid>`:
  // refresh the list so that row exists, then open it.
  // One refresh can be discarded by latestOnly when the regular poll overlaps
  // it, and does nothing while the page is hidden, so awaitListed asks again
  // a few times. The server registers the simulator before it reports the id,
  // so if the list still lacks it after that, onPickTarget copes with the id
  // alone. Not if the person has gone to another target in the meantime.
  onResolved: async (id) => {
    const from = createdFrom;
    const r = await awaitListed({
      poll: pollTargets,
      isListed: () => lastTargets.some((t) => t && t.id === id),
      stillHere: () => focused().targetId === from,
    });
    report({ event: 'newdevice-resolved', id, result: r });
    if (r === 'moved') return;
    drawer.close();
    onPickTarget(id);
  },
  onClose: () => closeScrimIfIdle(),
});

function openNewDevice() {
  el.scrim.hidden = false;
  newDevice.open();
}

// The two sheets the full-screen states open. Neither sits over the
// picker, so closing one hides the scrim unless something else holds it.
function closeScrimIfIdle() {
  if (!drawer.isOpen() && !newDevice.isOpen() && !connectHelp.isOpen() && !switchServer.isOpen() && !addServers.isOpen()) el.scrim.hidden = true;
}
const connectHelp = mountConnectHelp(el.connectHelp, {
  onClose: closeScrimIfIdle,
  onNewDevice: () => openNewDevice(),
});
function openConnectHelp() {
  el.scrim.hidden = false;
  connectHelp.open({ canCreate });
}
const switchServer = mountSwitchServer(el.switchServer, {
  storage: safeStorage(),
  report,
  onClose: closeScrimIfIdle,
});
function openSwitchServer() {
  el.scrim.hidden = false;
  switchServer.open();
}

// Add server opens over the picker like New device does. An add or remove
// changes the list, so the picker is fetched again; a 401 on the add is the
// peer refusing its token (servers.js), not this page's sign-in, and nothing
// here navigates away on it.
const addServers = mountServers(el.addServer, {
  fetch: (...a) => fetch(...a),
  report,
  confirm: async (name) => (await ask({
    title: `Remove ${name}?`,
    message: 'Its devices leave the picker. Add it again with its token to bring them back.',
    actions: [{ id: 'remove', label: 'Remove', destructive: true }],
  })) === 'remove',
  onChanged: () => pollTargets(),
  onClose: closeScrimIfIdle,
});
function openAddServer() {
  el.scrim.hidden = false;
  addServers.open();
}

// canCreate is whether this server can create a device: the options route
// answers 404 when it can create nothing, and says `profilesFallback` when
// only the old android CLI is there. Asked once, since the tools do not come
// and go while the server runs.
const canCreateReady = fetch('/api/devices/options').then(async (res) => {
  canCreate = canCreateFrom(res.status, await res.json().catch(() => null));
  drawer.setCanCreate(canCreate);
}).catch(() => {});

// A scrim tap closes the sheet on top, not the picker under it; capture on
// the scrim runs before the picker's own listener there.
el.scrim.addEventListener('click', (ev) => {
  const top = [newDevice, connectHelp, switchServer, addServers].find((s) => s.isOpen());
  if (!top) return;
  ev.stopImmediatePropagation();
  top.close();
}, true);

el.pill.addEventListener('click', () => {
  // Docked, the picker is pinned and the pill is only a label.
  if (dockQuery.matches) return;
  if (drawer.isOpen()) {
    drawer.close();
    return;
  }
  openPicker();
});

// openPicker is the pill's open, and Can't stream's "Pick another device".
async function openPicker() {
  // The drawer and the Tools sheet share one #scrim, and each owns it
  // independently -- open() shows it, close() hides it, with no notion of
  // the other. Opening one while the other is already open leaves two
  // panels live behind a scrim only one of them still thinks it controls:
  // Escape closes whichever has the capture-phase listener (the drawer),
  // which hides the scrim out from under the sheet still sitting open
  // beneath it, unreachable by a scrim click that no longer exists. Closing
  // the sheet first keeps the invariant simple -- at most one of these is
  // ever open -- rather than teaching each one to read the other's state.
  if (sheetHandle.detent !== 'closed') sheetHandle.close();
  // The overflow menu is not part of that scrim invariant (it never touches
  // #scrim -- see corners.js's module-level comment), but it still has to
  // close here: it has no scrim to be exposed by, but the drawer opening on
  // top of it would leave a dropdown floating over the panel that replaced
  // it.
  corners.closeMenu();
  // Open immediately with whatever was last fetched -- the initial
  // lastTargets from loadTargets, or an earlier open's result -- rather than
  // waiting on a fresh round trip first. A stale list open behind a scrim
  // beats a pill that visibly does nothing for up to 2s.
  drawer.open(lastTargets, focused().targetId);
  const { current, result } = await fetchTargetsLatest();
  if (result.failed || !current) return; // failed, or superseded by a later fetch -- leave the drawer showing what it has
  takeTargets(result.list);
}

// The server tracks adb live, so the list is polled to show a device that was
// plugged in, or one that dropped off, without a reload. An open drawer is
// only repainted when what it shows changed (sameShown), since a repaint
// under a finger moves the row being reached for.
async function pollTargets() {
  if (document.hidden) return;
  const { current, result } = await fetchTargetsLatest();
  if (result.failed || !current) return;
  takeTargets(result.list);
  // No devices has no Try again when New device… is offered: a device that
  // appears, made here or plugged in, is picked up by this poll instead.
  if (result.list.length > 0 && pageOverlay.state() === 'noDevices') loadTargets();
}
// Every 5s, or every 2s while a device is being created, so its row's
// elapsed time and its turn into a real device show up promptly.
let lastPoll = 0;
setInterval(() => {
  const creating = (lastTargets || []).some((t) => t && t.state === 'creating');
  if (!creating && Date.now() - lastPoll < 4900) return;
  lastPoll = Date.now();
  pollTargets();
}, 1000);

function takeTargets(list) {
  const changed = !sameShown(lastTargets, list);
  lastTargets = list;
  if (changed) drawer.refresh(list);
  // A Peek or a reboot changes the window under the same target.
  for (const p of panes.list()) {
    const cur = list.find(t => t.id === p.targetId);
    if (cur && !!cur.headless !== !!p.targetHeadless) applyTarget(p, cur);
  }
}

// explainFailure replaces a generic "Disconnected" with what the server
// knows. A browser cannot read the reason off a failed connect: a WebSocket
// refused before it upgrades reports only that it closed, and WebTransport
// reports a bare WebTransportError (trap 11). So the server keeps the reason
// and the last open error per target, and they are asked for here. The retry
// already scheduled keeps running, so a device that comes back is picked up
// without a tap.
async function explainFailure(pane, targetId) {
  const { current, result } = await fetchTargetsLatest();
  // The server ended this target while the fetch ran: its overlay stays.
  if (pane.ended) return;
  if (result.failed) {
    if (pane.targetId === targetId && !pane.els.overlay.hidden) {
      pane.overlay.show(unreachableState({ hostname: location.hostname }), { retry: () => pane.retryNow(), switchServer: openSwitchServer });
    }
    return;
  }
  const list = result.list;
  if (current) takeTargets(list);
  const reason = failureReason(list, targetId);
  // Opened in the meantime, or the viewer switched: nothing to explain.
  if (!reason || pane.els.overlay.hidden || pane.targetId !== targetId) return;
  report({ event: 'connect-failed', pane: pane.id, target: targetId, reason });
  const t = list.find(x => x.id === targetId);
  pane.overlay.show(cantStreamState({ label: t ? t.label : targetId, reason }), { pick: openPicker });
}

// routeControl takes the control messages the pane does not handle itself.
// The session's own modules are the pane's; the clipboard is the page's.
function routeControl(p, type, body) {
  switch (type) {
    case MSG_EVENT: {
      // The markers must not take the log down with them: a body the log
      // would reject is its business to report, not this call's.
      try {
        const ev = decodeEvent(body);
        p.mods.touches.onEvent(ev);
        p.mods.cursor.onEvent(ev);
      } catch (_) {}
      return p.mods.eventlog.handleEvent(body);
    }
    case MSG_CURSOR:
      try { p.mods.cursor.onEvent(decodeEvent(body)); } catch (_) {}
      return;
    case MSG_OVERLAY:
      try { p.mods.overlays.handleMessage(body); } catch (_) {}
      return;
    case MSG_CLIPBOARD: return clipboard.handleMessage(body);
    case MSG_CONTROL: return p.mods.control.handleMessage(body);
    default: return;
  }
}

// onPaneInit is the page's half of a session's init: the pane has taken the
// stream's size and status and given the init to its own modules (touches,
// control, inspect, the event log), and these are the page's that read it.
function onPaneInit(p, info) {
  paintPaneHeader(p);
  if (focused() !== p) return;
  // corners.js combines this with navKeys(os) itself (client Task 7) -- see
  // its setControl() comment for why the AND of the two lives there rather
  // than here. The keyboard button reads pane.hasControl() through
  // keyboard.offered() when setControl repaints; the pane has set it already.
  corners.setControl(info.control);
  // Each session asks again: the target may have changed, and a fold resizes
  // the stream, which arrives as a new init.
  fold.refresh();
  // The hardware keyboard is set again on every session while the phone's
  // keyboard is up, rather than trusted to have stayed on: nothing reads it
  // back, and an Android device drops it when its session ends.
  if (info.control) keyboard.onSession(p.targetId);
}

// onPaneShape runs after every init, once the overlay is down.
function onPaneShape(p) {
  fitScreen();
  refreshFrame(p);
}

// onPaneClosed is the page's half of the current connection closing.
function onPaneClosed(p) {
  if (focused() !== p) return;
  // The nav row reflects MsgInit's info.control, which only ever arrives
  // on a live connection -- nothing tells it a session ended, so
  // without this it keeps showing the buttons for a stream that no
  // longer exists (client Task 7 leftover).
  corners.setControl(false);
}

// fitScreen scales the device screen to fit entirely inside the stage.
//
// It is done here rather than in CSS because the CSS version was circular: the
// canvas asked for max-height: 100% of a row whose height the canvas itself
// was determining, so the constraint was dropped and a 20:9 phone ran off the
// bottom of the viewport. Measuring the stage and assigning pixels has no such
// loop.
//
// Both axes are honoured, so this is a contain fit: the whole device screen is
// always reachable, which is the point -- a tab bar along the bottom edge of
// the device is unusable if it is under the fold.
// place writes placeArt's result -- a piece of skin art at its own natural
// size, turned about the top-left corner of the gridW by gridH layout it is
// drawn on, which is the anchor the turn leaves fixed -- onto a node.
function place(node, x, y, w, h, gridW, gridH, scale, turn) {
  const box = placeArt(x, y, w, h, scale, turn, turnedArt, gridW, gridH);
  node.style.left = box.left + 'px';
  node.style.top = box.top + 'px';
  node.style.width = box.width + 'px';
  node.style.height = box.height + 'px';
  node.style.transformOrigin = '0 0';
  node.style.transform = box.transform;
}

// cornerReservation reads the corner clusters' footprint off the CSS custom
// properties `--corner-w`, `--corner-h` and `--corner-gap`. style.css
// declares them (in :root, alongside the corner clusters themselves), so a
// missing or unparseable value falls back to the plan's own defaults rather
// than to zero, which would silently stop reserving space if a future edit
// drops or breaks the declaration.
//
// The width and height are read separately -- and matter separately, see
// fitStage's own comment -- because the clusters are not always square: a
// foldable target's L is taller than it is wide, so what it costs beside the
// device (--corner-w) is not what it costs below it (--corner-h).
//
// `--corner-w`, `--corner-h` and `--corner-gap` are declared in px, and MUST
// stay that way. This reads them with parseFloat, which takes only the
// leading number: an unregistered custom property is never resolved by the
// engine, so `8rem` would parse as the unit-less number 8 (a silent 16x
// error at the browser's default font size) and `calc(...)` would parse as
// NaN (caught by the Number.isFinite check below, but only by accident).
// They are not registered with `@property` -- that would make
// getComputedStyle resolve any unit to px itself and remove this trap, but
// style.css's own comment on the declaration is the guard instead, so this
// stays a trap to know about rather than one that has been closed.
function cornerReservation() {
  const style = getComputedStyle(document.documentElement);
  const width = parseFloat(style.getPropertyValue('--corner-w'));
  const height = parseFloat(style.getPropertyValue('--corner-h'));
  const gap = parseFloat(style.getPropertyValue('--corner-gap'));
  return {
    width: Number.isFinite(width) ? width : 100,
    height: Number.isFinite(height) ? height : 100,
    gap: Number.isFinite(gap) ? gap : 12,
  };
}

// fitScreen recomputes the canvas's on-screen box and, when inspect mode is
// on, the boxes drawn over it: inspect.js's boxes are computed from that
// same box (toCss), so anything that moves it -- a viewport resize, a
// rotation's frame swap -- has to repaint them too, or what is drawn stops
// matching what a tap would select (review MEDIUM-7). relayout() is a no-op
// when inspect is off, so this costs nothing on the common path.
function fitScreen() {
  const list = panes.list();
  const multi = list.length > 1;
  document.documentElement.classList.toggle('multi', multi);
  if (!multi) {
    if (list[0]) layoutScreen(list[0], null);
  } else {
    const cells = paneCells(list.length);
    list.forEach((p, i) => layoutScreen(p, cells[i] || null));
  }
  if (zoomMode) zoomMode.sync();
  for (const p of list) { p.mods.inspect.relayout(); p.mods.flag.relayout(); }
}

// Side by side, each pane's header sits above its device and the corners are
// one band under all of them (Figma 59:178), so the stage's content box loses
// that band once and gridCells splits what is left.
const PANE_GAP = 16;
function paneCells(count) {
  const stageRect = el.stage.getBoundingClientRect();
  const style = getComputedStyle(el.stage);
  const padLeft = parseFloat(style.paddingLeft);
  const padTop = parseFloat(style.paddingTop);
  const c = cornerReservation();
  const box = {
    x: padLeft,
    y: padTop,
    width: stageRect.width - padLeft - parseFloat(style.paddingRight),
    height: stageRect.height - padTop - parseFloat(style.paddingBottom) - c.height - c.gap,
  };
  return gridCells(count, box, PANE_GAP);
}

function paneHeaderHeight() {
  const h = parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--pane-header-h'));
  return Number.isFinite(h) ? h : 36;
}

// Desktop and tablet landscape: the picker is a permanent panel on the left
// and Tools one on the right, with the stage between them (style.css,
// "Docked panels"). Crossing the breakpoint pins and opens both, or unpins
// and closes both, so a phone is back to one sheet at a time.
const dockQuery = window.matchMedia('(min-width: 900px)');
function syncDock() {
  const docked = dockQuery.matches;
  const root = document.documentElement;
  if (root.classList.contains('docked') === docked) return;
  root.classList.toggle('docked', docked);
  // The pill opens nothing while the picker is pinned beside the stage.
  if (docked) el.pill.removeAttribute('aria-haspopup');
  else el.pill.setAttribute('aria-haspopup', 'dialog');
  el.pill.tabIndex = docked ? -1 : 0;
  drawer.pin(docked);
  sheetHandle.pin(docked);
  if (docked) {
    corners.closeMenu();
    if (sheetHandle.detent === 'closed') sheetHandle.open('half');
    if (!drawer.isOpen()) openPicker();
    reopenHidden();
  } else {
    drawer.close();
    sheetHandle.close();
    keepFocusedOnly();
  }
  fitScreen();
}

// A phone shows one pane. Undocking keeps the focused one and closes the
// rest, keeping their targets in the URL; docking again reopens them beside
// it, the focused one still in front.
function keepFocusedOnly() {
  const f = focused();
  const rest = panes.list().filter((p) => p !== f);
  hiddenTargets = rest.map((p) => p.targetId).filter(Boolean).concat(hiddenTargets);
  for (const p of rest) panes.close(p.id);
}
function reopenHidden() {
  const ids = hiddenTargets;
  hiddenTargets = [];
  const f = focused();
  for (const id of ids) onOpenBeside(id);
  if (f) panes.focus(f.id);
}
dockQuery.addEventListener('change', syncDock);

// Collapsing a docked panel hands its width to the stage (panels.js).
const panelButtons = {
  left: document.getElementById('leftPanelBtn'),
  right: document.getElementById('rightPanelBtn'),
};
let panels = readPanels(safeStorage());
applyPanels(document.documentElement, panels, panelButtons);
for (const side of ['left', 'right']) {
  panelButtons[side].addEventListener('click', () => {
    panels = togglePanel(panels, side);
    applyPanels(document.documentElement, panels, panelButtons);
    writePanels(safeStorage(), panels);
    report({ event: 'panel', side, collapsed: panels[side] });
    fitScreen();
  });
}

// layoutScreen places p's device. With no cell (one pane) it fits the stage
// as it always has, beside or above the corners. With a cell (side by side)
// the pane's root takes the cell, its header the top of it, and the device is
// contain-fit in the rest.
function layoutScreen(p, cell) {
  const e = p.els;
  const cw = e.canvas.width, ch = e.canvas.height;
  if (!cw || !ch) return;
  // A fold's other panel arriving is a new shape; the fold button only
  // follows the focused pane.
  const shape = `${cw}x${ch}`;
  if (shape !== p.lastShape) {
    const had = p.lastShape;
    p.lastShape = shape;
    if (had && onReshape && focused() === p) onReshape();
  }

  let padLeft, padTop, availW, availH, stageRect;
  if (cell) {
    const headH = paneHeaderHeight();
    e.root.style.left = cell.x + 'px';
    e.root.style.top = cell.y + 'px';
    e.root.style.width = cell.width + 'px';
    e.root.style.height = cell.height + 'px';
    padLeft = 0;
    padTop = headH;
    availW = cell.width;
    availH = cell.height - headH;
  } else {
    for (const k of ['left', 'top', 'width', 'height']) e.root.style[k] = '';
    stageRect = el.stage.getBoundingClientRect();
    const style = getComputedStyle(el.stage);
    padLeft = parseFloat(style.paddingLeft);
    padTop = parseFloat(style.paddingTop);
    availW = stageRect.width - padLeft - parseFloat(style.paddingRight);
    availH = stageRect.height - padTop - parseFloat(style.paddingBottom);
  }
  if (availW <= 0 || availH <= 0) return;

  // Contain-fit the whole bezel when there is one, so the frame is what gets
  // measured against the viewport rather than the screen inside it. Fitting
  // the screen and then drawing a bezel around it is how trap 18 came back:
  // the part that overflows is the part nobody measured.
  // A rotation turns the picture over without ending the session, so the
  // bezel fetched for the old orientation is still here and no longer
  // describes anything. Drop it for as long as they disagree rather than
  // fitting a landscape canvas to a portrait display rect.
  //
  // Immersive mode draws no bezel: the point of it is the screen alone, as
  // large as the page allows. It reads the class rather than immersive.js's
  // handle because fitScreen runs before that module is mounted.
  const immersed = document.documentElement.classList.contains('immersive');
  const zoomed = e.wrap.classList.contains('zoomed');
  const frame = !immersed && !zoomed && frameSuitsStream(p.frame, cw, ch) ? p.frame : null;
  showFrameParts(p, frame);
  const boxW = frame ? frame.width : cw;
  const boxH = frame ? frame.height : ch;

  const fit = immersed || cell
    ? centreFit({ width: availW, height: availH }, { width: boxW, height: boxH })
    : fitStage({ width: availW, height: availH }, { width: boxW, height: boxH }, cornerReservation());
  if (!fit) return;
  const { scale, x, y, width, height } = fit;

  // A peek's window takes the bezel's shape once per shape. What the page
  // keeps beside the box is the stage's padding plus, below the device, the
  // band the corners sit in, or beside it, their columns.
  if (peek.active && frame && !cell) {
    const c = cornerReservation();
    peek.sizeTo({ width: boxW, height: boxH }, {
      width: stageRect.width - availW + (fit.band ? 0 : 2 * (c.width + c.gap)),
      height: stageRect.height - availH + (fit.band ? c.height + c.gap : 0),
    });
  }

  e.wrap.style.position = 'absolute';
  e.wrap.style.left = Math.round(padLeft + x) + 'px';
  e.wrap.style.top = Math.round(padTop + y) + 'px';
  e.wrap.style.width = width + 'px';
  e.wrap.style.height = height + 'px';

  if (!frame) {
    for (const node of [e.canvas, e.frameCutout, e.frameArt, e.frameMask]) {
      node.style.left = '';
      node.style.top = '';
      node.style.width = '';
      node.style.height = '';
      node.style.transform = '';
    }
    e.canvas.style.borderRadius = '';
    e.wrap.style.borderRadius = '';
    return;
  }

  // Skin art is authored on the layout's own pixel grid: back.webp's hole sits
  // at exactly the display rect in its own pixels, and mask.webp is exactly the
  // display's size. So both are placed by that grid and never stretched to fit
  // a box. back.webp is also larger than the layout declares -- 1205x2535
  // against 1198x2531 on pixel_10 -- and the emulator crops the difference, so
  // it is drawn at its natural size and the wrap clips it.
  //
  // A turned frame is the same art on the same grid, so the turn is a
  // transform and the natural size still governs. frame.width and the display
  // rect arrive already turned, which is why the fallbacks below swap: the
  // images have not, and an odd turn is the case where the two disagree.
  const d = frame.display;
  const turn = frame.rotation || 0;
  const odd = turn % 2 === 1;
  for (const node of [e.canvas, e.frameCutout]) {
    node.style.left = Math.round(d.x * scale) + 'px';
    node.style.top = Math.round(d.y * scale) + 'px';
    node.style.width = Math.round(d.width * scale) + 'px';
    node.style.height = Math.round(d.height * scale) + 'px';
  }

  // mask.webp is exactly the display and carries its rounded corners and its
  // punch hole, so it is anchored on the display rect and turned about that
  // corner. Stretching it onto the turned rect is trap 40 with the axes
  // swapped: a 1080x2424 mask pulled across 2424x1080 puts the hole on the
  // long edge, which reads as a bezel that was drawn for a different phone.
  //
  // Both turn about the grid they are drawn on, and the server turned the
  // display rect about the layout's size, so that is the pivot for the art and
  // not its natural size. back.webp overhangs its layout (trap 40), and
  // pivoting on the image put its hole 4-7 art px off the canvas.
  const displayW = odd ? d.height : d.width, displayH = odd ? d.width : d.height;
  const layoutW = odd ? frame.height : frame.width, layoutH = odd ? frame.width : frame.height;
  place(e.frameMask, d.x, d.y,
    e.frameMask.naturalWidth || displayW, e.frameMask.naturalHeight || displayH,
    displayW, displayH, scale, turn);
  place(e.frameArt, 0, 0,
    e.frameArt.naturalWidth || layoutW, e.frameArt.naturalHeight || layoutH,
    layoutW, layoutH, scale, turn);

  if (frame.source === 'skin') {
    // The art draws its own rounded screen over the top. The canvas is still
    // clipped to the screen's radius, because a simulator's display rect
    // reaches past the phone's outer curve, where the art is clear and would
    // otherwise show the video's square corners. An emulator skin's mask
    // covers the same corners in black, so there the clip changes nothing.
    e.canvas.style.borderRadius = Math.round(frame.cornerRadius * scale) + 'px';
    e.wrap.style.borderRadius = '';
    return;
  }
  // Concentric: the outer curve is the screen's radius plus the bezel it sits
  // inside, so the two do not read as a rounded screen on a square slab.
  e.canvas.style.borderRadius = Math.round(frame.cornerRadius * scale) + 'px';
  e.wrap.style.borderRadius = Math.round((frame.cornerRadius + d.x) * scale) + 'px';
}

// ---------------------------------------------------------------------------
// Input
// ---------------------------------------------------------------------------
// The canvas's pointer and wheel listeners are the pane's (pane.js). Keys are
// the page's: the nav row and the phone keyboard send them.

function sendKey(action, keycode) {
  const p = focused();
  if (!p.open) return;
  const buf = new ArrayBuffer(6);
  const dv = new DataView(buf);
  dv.setUint8(0, MSG_KEY);
  dv.setUint8(1, action);
  dv.setInt32(2, keycode);
  p.send(buf);
}

// The nav row's three buttons and their sendKey() wiring are built by
// corners.js, client Task 7. sendKey() itself stays here for that to call.

// ---------------------------------------------------------------------------
// Connection details
// ---------------------------------------------------------------------------
// The dot is three colours and no words. This is the same information said
// outright, plus what only the client knows: which transport actually won,
// what the round trip measures, and what the server said it was sending.

function paintConnection() {
  // el.sheet.hidden mirrors the sheet's own open/closed state (mount() sets
  // it directly), so this is available before mountSheet() runs below and
  // does not need sheetHandle itself. Skipping the writes while closed saves
  // DOM work nobody can see.
  if (!el.connGroups || el.sheet.hidden) return;

  const p = focused();
  const status = p.status || 'idle';
  const info = p.info;
  const st = p.stats();
  const groups = connectionRows({
    status,
    statusText: STATUS_TEXT[status] || status,
    // The name the transport reports, not the one that was asked for:
    // falling back to WebSocket is silent otherwise.
    transport: p.open ? p.transportName : '',
    rtt: st.rtt,
    jitter: st.jitter,
    kbps: st.kbps,
    width: info && info.codedWidth,
    height: info && info.codedHeight,
    codec: info && info.codec,
    targetId: p.targetId,
    decFps: st.decFps,
    fps: st.fps,
    bufferMs: st.bufferMs,
    dropped: st.dropped,
    decodeErrors: st.decodeErrors,
    shed: st.shedRate,
    queue: st.queue === null ? 0 : st.queue,
  });
  for (const key of ['link', 'video', 'problems']) {
    const { section, dl } = el.connGroups[key];
    const rows = groups[key];
    section.hidden = rows.length === 0;
    while (dl.children.length > rows.length * 2) dl.lastChild.remove();
    rows.forEach((r, i) => {
      let dt = dl.children[i * 2];
      let dd = dl.children[i * 2 + 1];
      if (!dt) {
        dt = dl.appendChild(document.createElement('dt'));
        dd = dl.appendChild(document.createElement('dd'));
      }
      if (dt.textContent !== r.label) dt.textContent = r.label;
      if (dd.textContent !== r.value) dd.textContent = r.value;
      dd.className = r.grade;
    });
  }
}

// ---------------------------------------------------------------------------
// Device frame
// ---------------------------------------------------------------------------
// Off by default and behind the toggle, because a bezel costs about 10% of
// each axis and trap 18 was the device screen not fitting on iOS to begin
// with. It is also strictly decoration: nothing about input or decode reads
// any of this.

async function loadFrame(targetId) {
  const res = await fetch(`/api/frame?target=${encodeURIComponent(targetId || '')}`);
  if (!res.ok) throw new Error(`frame: ${res.status}`);
  return res.json();
}

// showFrameParts is the one place the bezel is shown or hidden, because
// fitScreen decides that per frame now: applyFrame loads the art, and whether
// it is on screen depends on an orientation that can change afterwards.
function showFrameParts(p, frame) {
  const e = p.els;
  e.wrap.classList.toggle('framed', !!frame);
  e.frameArt.hidden = !frame || !frame.background;
  e.frameMask.hidden = !frame || !frame.mask;
  e.frameCutout.hidden = !frame || !frame.cutout;
}

function clearFrame(p) {
  const e = p.els;
  p.frame = null;
  e.wrap.classList.remove('framed', 'skin', 'derived');
  showFrameParts(p, null);
  fitScreen();
}

function applyFrame(p, frame) {
  const e = p.els;
  p.frame = frame;
  e.wrap.classList.toggle('skin', frame.source === 'skin');
  e.wrap.classList.toggle('derived', frame.source !== 'skin');

  const asset = (name) =>
    `/api/frame/asset?target=${encodeURIComponent(p.targetId || '')}&file=${encodeURIComponent(name)}`;

  if (frame.background) e.frameArt.src = asset(frame.background);
  if (frame.mask) e.frameMask.src = asset(frame.mask);

  // The cutout is in the display's pixel space, so it is drawn in its own
  // overlay sized to the display rect rather than to the whole frame. Scale
  // and offset stay as a transform: they came out of the device's dp units and
  // anchor, and baking them into the path would mean rewriting SVG arc
  // parameters, which is where a punch hole turns into an ellipse.
  const cut = frame.cutout;
  if (cut) {
    e.frameCutout.setAttribute('viewBox', `0 0 ${frame.display.width} ${frame.display.height}`);
    e.frameCutoutPath.setAttribute('d', cut.path);
    e.frameCutoutPath.setAttribute('transform', `translate(${cut.dx} 0) scale(${cut.scale})`);
  }

  fitScreen();
}

const frameShape = (t, w, h) => `${t}@${orientationOf(w, h)}`;

async function refreshFrame(p) {
  if (!state.frameWanted || !p.targetId) { clearFrame(p); return; }
  // The server answers for the orientation the device is in, so a rotation
  // needs a fresh frame and not just a fresh fit. Keying the cache on the
  // shape the frame describes asks again exactly once per rotation, and never
  // on the derived path, which answers portrait and is dropped by the gate
  // rather than refetched on every tick.
  const shape = frameShape;
  const e = p.els;
  if (p.frame && p.frameFor === shape(p.targetId, e.canvas.width, e.canvas.height)) return;

  const want = p.targetId;
  try {
    const frame = await loadFrame(want);
    // The target can change while the fetch is in flight, and drawing a
    // Pixel's bezel around an Acme One is worse than drawing none.
    if (!state.frameWanted || p.targetId !== want) return;
    p.frameFor = shape(want, frame.display.width, frame.display.height);
    applyFrame(p, frame);
    report({ event: 'frame', source: frame.source, w: frame.width, h: frame.height, rotation: frame.rotation || 0 });
  } catch (err) {
    // Synthetic has no device behind it and answers 501, which is the ordinary
    // case rather than a failure worth an overlay.
    report({ event: 'frame-unavailable', err: String((err && err.message) || err) });
    p.frameFor = null;
    clearFrame(p);
  }
}

// toggleFrame flips the bezel preference and persists it. The Tools sheet
// (client Task 6) wires a switch to this; there is no button here yet.
function toggleFrame() {
  state.frameWanted = !state.frameWanted;
  if (!peek.active) try { localStorage.setItem('loupe.frame', state.frameWanted ? '1' : '0'); } catch (_) {}
  for (const p of panes.list()) {
    p.frameFor = null;
    refreshFrame(p);
  }
}

// recheckFrame catches a flip between the two landscapes, which changes
// nothing the video carries, so refreshFrame's shape key never asks again and
// the punch hole stays on the edge it was on. /api/frame is one dumpsys,
// 0.11-0.17s on emulator-5554, so asking every few seconds while a turned
// skin is on screen is cheap. A failed ask keeps the frame already drawn: it
// was right a moment ago, and refreshFrame owns clearing it.
async function recheckFrame(p) {
  if (p.frameRecheckBusy || document.hidden || !state.frameWanted) return;
  const e = p.els;
  if (!frameNeedsRecheck(p.frame, e.canvas.width, e.canvas.height)) return;
  const want = p.targetId;
  const had = p.frame;
  p.frameRecheckBusy = true;
  try {
    const frame = await loadFrame(want);
    if (!state.frameWanted || p.targetId !== want || p.frame !== had) return;
    if ((frame.rotation || 0) === (had.rotation || 0)) return;
    p.frameFor = frameShape(want, frame.display.width, frame.display.height);
    applyFrame(p, frame);
    report({ event: 'frame', source: frame.source, w: frame.width, h: frame.height, rotation: frame.rotation || 0, via: 'recheck' });
  } catch (_) {
  } finally {
    p.frameRecheckBusy = false;
  }
}
setInterval(() => panes.each(recheckFrame), 3000);

state.frameWanted = true;
try { state.frameWanted = frameWantedFrom(localStorage.getItem('loupe.frame')); } catch (_) {}
// A peek is the bezel: it is shown whatever loupe.frame says, and the
// preference is left as the person set it in their main tab.
if (peek.active) state.frameWanted = true;

// ---------------------------------------------------------------------------
// Tools sheet
// ---------------------------------------------------------------------------
// mount() builds the shell (handle, scrim, section collapse); this fills the
// three sections client Task 6 owns. App, Event log and Device belong to
// later tasks and are hidden here rather than left half-built, which is the
// same rule the spec gives for a 501 endpoint: no section is better than a
// broken one.

// A labelled segmented control, one button per option, the current one
// checked. Reuses devicesettings.js's .settingSegments look so the Tools
// sheet has one segmented style, not two.
function addSegmentRow(container, label, options, current, onPick) {
  const row = document.createElement('div');
  row.className = 'sheetSwitchRow';
  const span = document.createElement('span');
  span.textContent = label;
  const group = document.createElement('div');
  group.className = 'settingSegments';
  group.setAttribute('role', 'radiogroup');
  group.setAttribute('aria-label', label);
  const mark = (value) => {
    for (const b of group.children) b.setAttribute('aria-checked', String(b.dataset.value === value));
  };
  for (const o of options) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'settingSegment';
    b.dataset.value = o.value;
    b.setAttribute('role', 'radio');
    b.textContent = o.label;
    b.addEventListener('click', () => { mark(o.value); onPick(o.value); });
    group.appendChild(b);
  }
  mark(current);
  row.appendChild(span);
  row.appendChild(group);
  container.appendChild(row);
  return group;
}

function addSwitchRow(container, label, initialChecked, onToggle) {
  const row = document.createElement('div');
  row.className = 'sheetSwitchRow';
  const span = document.createElement('span');
  span.textContent = label;
  const btn = document.createElement('button');
  btn.type = 'button';
  btn.className = 'switch';
  btn.setAttribute('role', 'switch');
  btn.setAttribute('aria-checked', initialChecked ? 'true' : 'false');
  btn.addEventListener('click', () => {
    const now = btn.getAttribute('aria-checked') !== 'true';
    btn.setAttribute('aria-checked', now ? 'true' : 'false');
    onToggle(now);
  });
  row.appendChild(span);
  row.appendChild(btn);
  container.appendChild(row);
  return btn;
}

const sheetHandle = mountSheet(el.sheet, {
  scrim: el.scrim,
  trigger: el.toolsBtn,
  onOpen: (detent) => {
    report({ event: 'sheet-open', detent, docked: dockQuery.matches });
    el.toolsBtn.setAttribute('aria-expanded', 'true');
    // Same reasoning as the drawer's open call site: the overflow menu is
    // not part of the #scrim invariant, but a menu left open behind the
    // sheet is still a dropdown floating over the panel that replaced it.
    corners.closeMenu();
    paintConnection();
    // App/Device settings are read fresh each time the sheet opens (design
    // spec: "fetched when the App section is open"), not kept live in the
    // background -- there is no MsgEvent-driven refresh to keep them current
    // while it is closed, and the device is not going anywhere between opens.
    appCard.refresh();
    deviceSettings.refresh();
    streamConfig.refresh();
    paintDeviceSwitches();
    // mount()'s own isOpen() gate means a render while closed is a no-op,
    // so the log is otherwise never painted until this fires -- render on
    // open rather than on every live event while nobody can see it.
    focused().mods.eventlog.render();
  },
  onClose: (detent) => {
    report({ event: 'sheet-close', detent, docked: dockQuery.matches });
    el.toolsBtn.setAttribute('aria-expanded', 'false');
  },
  // A pushed page is painted as it comes up: the event log renders only
  // while its page is the one shown, and the connection rows are skipped
  // while nobody can see them.
  onPage: (id, action) => {
    report({ event: 'sheet-page', page: id, action });
    if (action !== 'push') return;
    if (id === 'log') focused().mods.eventlog.render();
    if (id === 'connection') paintConnection();
  },
});

el.toolsBtn.addEventListener('click', () => {
  // Docked, Tools is always up, behind inspect's panel while that is on.
  if (dockQuery.matches) {
    focused().mods.inspect.off();
    return;
  }
  if (sheetHandle.detent === 'closed') {
    // See the matching comment on the pill's click handler: the drawer and
    // the sheet share one #scrim, so at most one of them is ever open.
    if (drawer.isOpen()) drawer.close();
    sheetHandle.open('half');
  } else {
    sheetHandle.close();
  }
});

// Connection page, under More: Link, Video and Problems as grouped lists in
// the sheet's own section style. paintConnection() (above) fills them from
// connstats.js; Problems is hidden while nothing is wrong.
const connectionPage = sheetHandle.addPage('connection', 'Connection');
{
  connectionPage.classList.add('grouped');
  el.connGroups = {};
  for (const [key, title] of [['link', 'Link'], ['video', 'Video'], ['problems', 'Problems']]) {
    const section = document.createElement('section');
    section.className = 'sheetSection';
    section.dataset.section = 'conn-' + key;
    const header = document.createElement('h3');
    header.className = 'sheetSectionHeader';
    header.textContent = title;
    const content = document.createElement('div');
    content.className = 'sheetSectionContent';
    const dl = document.createElement('dl');
    dl.className = 'sheetRows';
    content.appendChild(dl);
    section.appendChild(header);
    section.appendChild(content);
    connectionPage.appendChild(section);
    el.connGroups[key] = { section, dl };
  }
  paintConnection();
}

// The pill's dot says the link is unwell; tapping it says why. It sits
// inside the pill's button, so the drawer would open too without the stop.
el.pillDot.addEventListener('click', (ev) => {
  ev.stopPropagation();
  if (drawer.isOpen()) drawer.close();
  corners.closeMenu();
  // Closing resets the page stack, so a second tap does not push
  // Connection on top of itself.
  if (sheetHandle.detent !== 'closed') sheetHandle.close();
  sheetHandle.open('full');
  sheetHandle.pushPage('connection');
  report({ event: 'dot-open' });
});

// Viewer section: Appearance and handedness. Both already ran at startup,
// above the state block, so the rows here just reflect and change what is
// already in effect.
{
  const storage = safeStorage();
  addSegmentRow(sheetHandle.sections.viewer, 'Appearance', [
    { value: 'light', label: 'Light' },
    { value: 'dark', label: 'Dark' },
    { value: 'system', label: 'System' },
  ], appearance, (value) => {
    appearance = value;
    writeAppearance(storage, value);
    applyAppearance(document.documentElement, value);
    paintThemeColor(value);
    report({ event: 'appearance', appearance: value });
  });

  // "Controls on the right" rather than "Left-handed": the old label
  // described every viewer, checked or not, as left-handed by default
  // (readHand's default is 'left'). This one reads true only once someone
  // has actually asked for the right-hand layout, off by default, and the
  // storage key and its two values ('left'/'right') are unchanged.
  addSwitchRow(sheetHandle.sections.viewer, 'Controls on the right', readHand(storage) === 'right', (checked) => {
    const hand = checked ? 'right' : 'left';
    writeHand(storage, hand);
    applyHand(document.documentElement, hand);
    report({ event: 'hand', hand });
  });
}

// Viewer group, continued: Layout, docked only (Figma's toolbar toggle).
// Single closes every pane but the focused one. Side by side is the state
// with more than one pane open, so picking it opens the picker to choose the
// next one; the row reads Side by side only once a second pane is open.
const layoutRow = addSegmentRow(sheetHandle.sections.viewer, 'Layout', [
  { value: 'single', label: 'Single' },
  { value: 'beside', label: 'Side by side' },
], 'single', (value) => {
  if (value === 'single') {
    const keep = focused();
    for (const p of panes.list()) if (p !== keep) panes.close(p.id);
    report({ event: 'layout', layout: 'single' });
  } else if (panes.list().length < 2) {
    paintLayoutRow();
    if (sheetHandle.detent !== 'closed') sheetHandle.close();
    if (!drawer.isOpen()) openPicker();
    showToast(el.toast, `${isMac ? '⌘' : 'Ctrl'}-click a device to open it beside`, 'more');
  }
});
function paintLayoutRow() {
  const value = panes.list().length > 1 ? 'beside' : 'single';
  for (const b of layoutRow.children) b.setAttribute('aria-checked', String(b.dataset.value === value));
  layoutRow.parentElement.hidden = !docked();
}
paintLayoutRow();
panes.onChange(paintLayoutRow);
dockQuery.addEventListener('change', paintLayoutRow);

// Viewer group, continued: the stats overlay toggle (persisted under
// 'loupe.hud', off by default) and the bezel toggle (toggleFrame(),
// unchanged). The same numbers, grouped and labelled, are on the Connection
// page, so they are readable without the overlay running.
{

  addSwitchRow(sheetHandle.sections.viewer, 'Device frame', !!state.frameWanted, () => {
    toggleFrame();
  });

  addSwitchRow(sheetHandle.sections.viewer, 'Stats overlay', hudOn, (checked) => {
    hudOn = checked;
    panes.each((p) => { p.els.hud.hidden = !checked; });
    try { localStorage.setItem('loupe.hud', checked ? '1' : '0'); } catch (_) {}
  });

  // Esc, Tab, arrows, Paste and Done above the phone's keyboard. Off by
  // default: most typing needs none of them. keyboard is mounted below, and
  // this runs only from a tap.
  const stripSwitch = addSwitchRow(sheetHandle.sections.viewer, 'Key strip', readStripPref(safeStorage()), (checked) => {
    keyboard.setStripPref(checked);
  });
  // Hidden when docked, as the strip itself is (style.css).
  stripSwitch.parentElement.classList.add('keyStripRow');
}

// Event log page, under More: live MsgEvent plus the /api/events catch-up,
// newest first, painted by each pane's eventlog.js mount into its own list,
// made beside the pane. The page shows the focused pane's list and count
// (showLogOf); the count of what arrived unwatched sits beside the More row's
// chevron, where it can be seen without opening the page.
const logPage = sheetHandle.addPage('log', 'Event log');
const logLink = sheetHandle.addLink('more', 'Event log', 'log');
el.connDetail = sheetHandle.addLink('more', 'Connection', 'connection');
// Hardware and Control (Mac InspectorView.swift:44-90). Hardware is Android
// only. Control shows while another viewer holds the focused pane's device,
// and its Take Over is the banner's action. Both start hidden.
sheetHandle.hideSection('hardware');
sheetHandle.hideSection('control');
for (const k of HARDWARE_KEYS) {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'sheetAction';
  b.textContent = k.label;
  b.addEventListener('click', () => {
    pressAndroidKey(k.keycode).catch((err) => showToast(el.toast, (err && err.message) || String(err), 'more'));
  });
  sheetHandle.sections.hardware.appendChild(b);
}
const controlNote = document.createElement('p');
controlNote.className = 'sheetNote';
controlNote.textContent = 'Another viewer holds this device.';
const takeOverBtn = document.createElement('button');
takeOverBtn.type = 'button';
takeOverBtn.className = 'sheetAction';
takeOverBtn.textContent = 'Take Over';
takeOverBtn.addEventListener('click', () => {
  const p = focused();
  try {
    if (!p.open) throw new Error('not connected');
    p.send(new Uint8Array([MSG_TAKE_OVER]).buffer);
  } catch (err) {
    showToast(el.toast, (err && err.message) || String(err), 'more');
  }
  report({ event: 'control-takeover', via: 'sheet' });
});
sheetHandle.sections.control.append(controlNote, takeOverBtn);

function refreshToolSections() {
  const p = focused();
  const t = p && lastTargets.find((x) => x.id === p.targetId);
  if (t && t.os === 'android') sheetHandle.showSection('hardware');
  else sheetHandle.hideSection('hardware');
  if (p && p.heldByOther) sheetHandle.showSection('control');
  else sheetHandle.hideSection('control');
}

// The page body is the Gestures / Logcat switch; logcat is Android only.
const logView = mountLogPage(logPage, { targetId: () => focused().targetId });
function showLogOf(p) {
  const t = lastTargets.find((x) => x.id === p.targetId);
  logView.setAndroid(!!t && t.os === 'android');
  logView.setList(p.els.logList);
  refreshToolSections();
  for (const b of logLink.querySelectorAll('.logBadge')) b.remove();
  logLink.appendChild(p.els.logBadge);
}
showLogOf(focused());

// ---------------------------------------------------------------------------
// Screenshot
// ---------------------------------------------------------------------------
// The device's own framebuffer, via /api/screenshot, rather than the canvas.
// The canvas holds a MaxSize-downscaled frame that has been through a lossy
// encoder, and on a stalled link it holds whatever was painted last rather
// than what is on the device now. The canvas is the fallback, because a target
// with no adb behind it has no screencap either: Synthetic implements no
// Screenshotter and the endpoint answers 501.

function shotFilename(disposition, targetId, now) {
  const m = /filename="([^"]+)"/.exec(disposition || '');
  if (m) return m[1];
  const t = (targetId || 'screen').replace(/[^A-Za-z0-9_-]/g, '-');
  const p = (n) => String(n).padStart(2, '0');
  const stamp = `${now.getFullYear()}${p(now.getMonth() + 1)}${p(now.getDate())}-` +
    `${p(now.getHours())}${p(now.getMinutes())}${p(now.getSeconds())}`;
  return `loupe-${t}-${stamp}.png`;
}

async function deviceShot() {
  const target = focused().targetId;
  const res = await fetch(`/api/screenshot?target=${encodeURIComponent(target || '')}`);
  if (!res.ok) throw new Error(`screenshot: ${res.status} ${(await res.text()).trim().slice(0, 120)}`);
  const blob = await res.blob();
  return new File([blob], shotFilename(res.headers.get('Content-Disposition'), target, new Date()), { type: 'image/png' });
}

function canvasShot() {
  const name = shotFilename(null, focused().targetId, new Date());
  return new Promise((resolve, reject) => {
    focused().els.canvas.toBlob((blob) => {
      if (!blob) { reject(new Error('canvas produced nothing')); return; }
      resolve(new File([blob], name, { type: 'image/png' }));
    }, 'image/png');
  });
}

// deliver hands the file to the share sheet where there is one, and falls back
// to a download. Safari can reject share() outright when the fetch has already
// spent the gesture that started it, so the fallback runs on failure and not
// only on absence -- an AbortError is the user closing the sheet and is not.
async function deliver(file) {
  if (navigator.canShare && navigator.canShare({ files: [file] })) {
    try {
      await navigator.share({ files: [file], title: file.name });
      return 'share';
    } catch (err) {
      if (err && err.name === 'AbortError') return 'cancelled';
    }
  }
  const url = URL.createObjectURL(file);
  const a = document.createElement('a');
  a.href = url;
  a.download = file.name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10000);
  return 'download';
}

// takeScreenshot drives the whole capture-and-deliver flow. The overflow
// menu (corners.js) wires a button to this.
async function takeScreenshot() {
  const started = performance.now();
  let source = 'device';
  try {
    let file;
    try {
      file = await deviceShot();
    } catch (err) {
      report({ event: 'screenshot-fallback', err: String((err && err.message) || err) });
      source = 'canvas';
      file = await canvasShot();
    }
    const how = await deliver(file);
    report({ event: 'screenshot', source, how, bytes: file.size, ms: Math.round(performance.now() - started) });
  } catch (err) {
    report({ event: 'screenshot-error', source, err: String((err && err.message) || err) });
    throw err;
  }
}

// rotateDevice, pressAndroidKey and pressIOSButton back corners.js's deps
// (client Task 7).
//
// rotateDevice deliberately omits "to". internal/httpapi/agent.go's
// handleInput already resolves a nil To to "current + 1 mod 4" itself --
// reading the live rotation off the target's own Rotator.Rotation(ctx)
// (internal/target/rotate.go, dumpsys) before choosing the next one -- so a
// bare {"action":"rotate"} is not the stale request the plan wrote it as; it
// is the one request that cannot be wrong about the current orientation. The
// client has no better source: frame.rotation (handled above) only exists
// while the device frame is fetched, and the frame is off by default (trap
// 18's cost), so a client-side "current + 1" would silently guess whenever
// nothing had turned the frame on. Letting the server -- which asked the
// device -- own that read is the "no guessing" the task asked for, not a
// gap in it.
async function rotateDevice() {
  const p = focused();
  const res = await fetch(`/api/input?target=${encodeURIComponent(p.targetId || '')}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...p.viewerHeaders() },
    body: JSON.stringify({ action: 'rotate' }),
  });
  if (!res.ok) throw new Error(`rotate: ${res.status} ${(await res.text()).trim().slice(0, 120)}`);
}

// pressAndroidKey wraps sendKey (MsgKey down/up over the live stream) with a
// thrown error when there is nothing to send it to. sendKey itself is a
// silent no-op without an open transport -- the right behaviour for a touch
// or a drag, which fire far too often to report -- but corners.js needs a
// rejected promise to turn into a toast for a deliberate button press.
async function pressAndroidKey(keycode) {
  if (!focused().open) throw new Error('not connected');
  sendKey(KEY_DOWN, keycode);
  sendKey(KEY_UP, keycode);
  // Home/Back/Recents are the keys most likely to change the foreground app.
  appCard.notifyActivity();
}

// pressIOSButton goes through /api/input {"action":"button"} rather than the
// stream: scrcpy has no MsgKey wire format for an OS it never targets.
//
// A 409 with code "hubHeld" means Xcode's Device Hub holds the simulator's
// buttons and this server did not boot it. Taking them back restarts
// SpringBoard and closes its apps, and someone else may be using it, so the
// user is asked before the reclaim is sent. The server reopens the app that
// was in front, and from then on takes a hold back without asking.
async function pressIOSButton(name) {
  const p = focused();
  const post = (body) => fetch(`/api/input?target=${encodeURIComponent(p.targetId || '')}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...p.viewerHeaders() },
    body: JSON.stringify(body),
  });
  let res = await post({ action: 'button', button: name });
  if (res.status === 409) {
    const body = await res.clone().json().catch(() => ({}));
    if (body.code === 'hubHeld') {
      const ok = await ask({
        title: 'Xcode\u2019s Device Hub is holding this simulator\u2019s buttons',
        message: 'Taking them back restarts SpringBoard. The app in front reopens; its other apps close.',
        actions: [{ id: 'reclaim', label: 'Take back buttons', destructive: true }],
      }) === 'reclaim';
      report({ event: 'hub-reclaim', button: name, asked: true, agreed: ok });
      if (!ok) return;
      const rc = await post({ action: 'reclaim' });
      if (!rc.ok) throw new Error(`reclaim: ${rc.status} ${(await rc.text()).trim().slice(0, 120)}`);
      res = await post({ action: 'button', button: name });
    }
  }
  if (!res.ok) throw new Error(`button: ${res.status} ${(await res.text()).trim().slice(0, 120)}`);
}

// Tools sheet panels, client Task 9. Each is mounted regardless of whether
// the current target actually supports it -- GET answering 501 is what
// hides the section (onUnavailable -> sheetHandle.hideSection), same pattern
// as Screenshotter/Framer/Inspector being optional server-side. onAvailable
// is the reverse: a target switched TO can support what the one switched
// FROM did not, so a section 501 hid stays hidden only for as long as the
// target that earned it is still selected (review HIGH-1) -- each module
// tracks that per-target itself and calls back here once a read actually
// succeeds. Card, panel and layer are built once here rather than per
// target; a switch just changes what deps.targetId() answers on the next
// fetch, and app.js's own target-switch handling (see onPickTarget, above)
// is what tells each module to re-fetch instead of leaving a stale render
// up under the new target's name.
sheetHandle.hideSection('app');

// The Device group also holds Show touches, which works on every target, so a 501 from /api/settings hides only the settings' own
// box, never the group.
el.deviceSettingsBox = document.createElement('div');
el.deviceSettingsBox.className = 'settingRows';
el.deviceSettingsBox.hidden = true;
sheetHandle.sections.device.appendChild(el.deviceSettingsBox);

const appCard = mountAppCard(sheetHandle.sections.app, {
  targetId: () => focused().targetId,
  onUnavailable: () => sheetHandle.hideSection('app'),
  onAvailable: () => sheetHandle.showSection('app'),
  report,
});

const deviceSettings = mountDeviceSettings(el.deviceSettingsBox, {
  targetId: () => focused().targetId,
  onUnavailable: () => { el.deviceSettingsBox.hidden = true; },
  onAvailable: () => { el.deviceSettingsBox.hidden = false; },
  report,
  // The element itself, not a wrapper closure -- every module that shows a
  // toast takes the same shape (review round 3, finding 1: this used to
  // pass showToast(el.toast, message, 'more') as `toast`, and
  // devicesettings.js called showToast(deps.toast, ...) on it, setting
  // properties on a function and showing nothing).
  toast: el.toast,
});

// postViewerInput sends one /api/input as a pane's viewer: its viewerHeaders()
// carries X-Loupe-Viewer, so the server counts it as the person's input and
// never refuses it as an agent's.
async function postViewerInput(body, p = focused()) {
  const res = await fetch(`/api/input?target=${encodeURIComponent(p.targetId || '')}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...p.viewerHeaders() },
    body: JSON.stringify(body),
  });
  if (!res.ok) throw new Error(`${body.action}: ${res.status} ${(await res.text()).trim().slice(0, 120)}`);
}

// Phone keyboard: see keyboard.js. hasControl() is the focused pane's MsgInit
// info.control, the same gate the nav row uses.
const keyboard = mountKeyboard({
  doc: document,
  targetId: () => focused().targetId,
  hasControl: () => focused().hasControl(),
  postInput: postViewerInput,
  // Typing goes over the session, not /api/input: on the same connection as
  // touches, so it stays in order with them, and with none of /api/input's
  // per-request cost -- a focused-field read for redaction that took 2.1s on
  // an emulator. The server withholds the text from its event instead.
  send: (body) => {
    const p = focused();
    if (!p.open) throw new Error('not connected');
    if (body.action === 'text') {
      for (const piece of chunkText(body.text)) {
        const bytes = textEncoder.encode(piece);
        const buf = new Uint8Array(1 + bytes.length);
        buf[0] = MSG_TEXT;
        buf.set(bytes, 1);
        p.send(buf.buffer);
      }
    } else if (body.action === 'key') {
      sendKey(KEY_DOWN, body.keycode);
      sendKey(KEY_UP, body.keycode);
    }
    // Typing is the likeliest thing to change what is on screen next.
    appCard.notifyActivity();
  },
  toast: el.toast,
  report,
  storage: safeStorage(),
  strip: document.getElementById('keyStrip'),
  // A desktop has a keyboard of its own; the strip is for a phone's.
  stripAllowed: () => !dockQuery.matches,
  // clipboard is mounted below; this runs only from a strip tap.
  paste: () => clipboard.paste(),
});

// Clipboard sharing: see clipboard.js.
const clipboard = mountClipboard({
  doc: document,
  clipboard: navigator.clipboard,
  ClipboardItem: window.ClipboardItem,
  targetId: () => focused().targetId,
  hasControl: () => focused().hasControl(),
  fetch: (url, init) => fetch(url, init),
  toast: el.toast,
  report,
});

// Dropping a photo, video or APK on the viewer: see drop.js.
mountDrop({
  doc: document,
  zone: el.stage,
  targetId: () => focused().targetId,
  fetch: (url, init) => fetch(url, init),
  toast: el.toast,
  report,
});

// Touch markers (touches.js) and agent cursors (cursor.js) are the pane's.
// The Show touches row and the More menu set the focused pane's.
const touches = {
  pref: () => focused().mods.touches.pref(),
  setPref: (on) => focused().mods.touches.setPref(on),
};

// Stream group: Max size, Bitrate and Frame rate from /api/config. See
// streamconfig.js.
const streamConfig = mountStreamConfig(sheetHandle, {
  report,
  toast: el.toast,
});

// Device group switches. The overflow menu keeps its own copy of Show
// touches, so it is re-read on each sheet open. The phone keyboard is not
// here: it is the button above the overflow, since raising iOS's keyboard
// under a half-open sheet would cover the device.
const deviceSwitches = {
  touches: addSwitchRow(sheetHandle.sections.device, 'Show touches', touches.pref(), (on) => {
    touches.setPref(on);
  }),
};
for (const btn of Object.values(deviceSwitches)) {
  sheetHandle.sections.device.insertBefore(btn.parentElement, el.deviceSettingsBox);
}

function paintDeviceSwitches() {
  deviceSwitches.touches.setAttribute('aria-checked', touches.pref() ? 'true' : 'false');
}

// The control banner (control.js) and screen recording (record.js) are the
// pane's: pane.viewerHeaders() marks this viewer's own /api/input calls as
// the person's, so they pass while an agent is refused.

// Picture in Picture, a spike behind ?pip=1: see pip.js.
const pipWanted = new URLSearchParams(location.search).has('pip');
const pipMount = pipWanted ? mountPip(focused().els.canvas, {
  report,
  onChange: (on) => {
    if (on) return;
    // Left PiP with the page still in the background: release the stream
    // now, as hiding the page would have. PiP shows the focused pane.
    if (document.hidden) focused().suspend();
  },
}) : null;
pip = pipMount && pipMount.supported ? pipMount : null;
if (pipWanted) report({ event: 'pip-offered', supported: !!pip });
if (pip) pip.attach();

zoomMode = mountZoom({
  doc: document,
  root: document.documentElement,
  stage: el.stage,
  pane: focused,
  relayout: fitScreen,
  // Zoom takes the screen: the corners and a sheet would be over it.
  onChange: (on) => { if (on) corners.closeMenu(); },
});

const corners = mountCorners(el.cornerNav, el.cornerMore, {
  sendAndroidKey: pressAndroidKey,
  pressIOSButton,
  screenshot: takeScreenshot,
  rotate: rotateDevice,
  zoom: { enter: () => zoomMode.enter() },
  link: {
    url: () => pageLink(location.origin, focused().targetId),
    write: (text) => navigator.clipboard.writeText(text),
  },
  inspect: {
    label: focused().mods.inspect.label,
    // Inspect's boxes are drawn for the untransformed picture.
    run: () => { zoomMode.exit(); focused().mods.inspect.run(); },
  },
  flag: { run: () => focused().mods.flag.run() },
  // Called at click time, by which point immersive below is mounted.
  fullScreen: () => immersive.set(true),
  ...(pip ? { pip: { label: () => pip.label(), run: () => pip.toggle() } } : {}),
  peek,
  keyboard,
  clipboard,
  // The menu's label follows the focused pane's recording: every pane's
  // record fans into recordListeners while it is in front, and a focus change
  // fires them too.
  record: {
    label: () => focused().mods.record.label(),
    toggle: () => focused().mods.record.toggle(),
    onChange: (fn) => recordListeners.push(fn),
  },
  touches,
  toast: el.toast,
  report,
  closeOtherPanels: () => {
    if (drawer.isOpen()) drawer.close();
    if (sheetHandle.detent !== 'closed') sheetHandle.close();
  },
});

// The fold button sits on top of the nav stack while the target has a
// posture to change: a foldable emulator or an iPhone Duo.
const fold = mountFold(el.cornerNav, {
  targetId: () => focused().targetId,
  report,
  onShape: fitScreen,
  // The Tools sheet shows the same setting.
  onChanged: () => deviceSettings.refresh(),
  toast: el.toast,
  // Read when a fold starts, so it moves the focused pane's device.
  surface: () => ({ wrap: focused().els.wrap, canvas: focused().els.canvas, stage: el.stage }),
});
onReshape = fold.reshaped;

// Immersive mode hides every control, so nothing may be left open behind it:
// a sheet or a drawer would otherwise sit over the stream with the button
// that closes it gone.
const immersive = mountImmersive(document.documentElement, el.immersiveHandle, {
  storage: safeStorage(),
  toast: el.toast,
  report,
  onChange: (on) => {
    if (on) {
      corners.closeMenu();
      if (drawer.isOpen()) drawer.close();
      if (sheetHandle.detent !== 'closed') sheetHandle.close();
    }
    fitScreen();
  },
});

// Double-tap on the bezel or the blank stage enters immersive; on the blank
// stage it leaves. Nothing on the canvas or a control is touched.
mountDoubleTap(el.stage, {
  isBezel: (t) => panes.list().some((p) => p.els.wrap === t),
  isImmersive: immersive.isOn,
  enter: immersive.enter,
  exit: immersive.exit,
});

// ---------------------------------------------------------------------------
// Focus
// ---------------------------------------------------------------------------
// focusMoved points the page at the newly focused pane: the target switch's
// work minus the reconnect, since that pane's session is already its own.
// The old pane's inspect goes off, as html.inspecting is the page's.
let frontPane = focused();
function focusMoved() {
  const p = focused();
  if (!p || p === frontPane) return;
  const old = frontPane;
  frontPane = p;
  if (old && panes.indexOf(old.id)) old.mods.inspect.off();
  zoomMode.sync();
  document.documentElement.classList.remove('inspecting');
  const t = lastTargets.find((x) => x.id === p.targetId);
  corners.setOS(t ? t.os : undefined);
  corners.setControl(!!(p.info && p.info.control));
  drawer.setCurrent(p.targetId);
  fold.refresh();
  appCard.refresh();
  deviceSettings.refresh();
  if (p.info && p.info.control) keyboard.onSession(p.targetId);
  p.mods.record.repaint();
  for (const fn of recordListeners) fn();
  if (pip) pip.retarget(p.els.canvas);
  showLogOf(p);
  el.cornerNav.setAttribute('aria-label', `${p.targetLabel || p.targetId || 'Device'} navigation`);
  refreshPill();
  paintConnection();
  report({ event: 'pane-focus', pane: p.id, target: p.targetId });
}

panes.onChange(({ kind }) => {
  focusMoved();
  panes.each(paintPaneHeader);
  if (kind !== 'focus') fitScreen();
  syncUrl();
  drawer.repaint();
});

// Alt+Meta+F (Alt+Ctrl+F off a Mac) flags the focused pane for an agent.
document.addEventListener('keydown', (e) => {
  if (!flagShortcut(e)) return;
  const t = e.target;
  if (t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName || ''))) return;
  e.preventDefault();
  focused().mods.flag.toggle();
});

// ⌃1-⌃4 (Alt elsewhere) focuses pane N while several are open. Ignored in a
// text field, where the shortcut belongs to the field.
document.addEventListener('keydown', (e) => {
  const n = paneShortcut(e, isMac);
  if (!n || panes.list().length < 2) return;
  const t = e.target;
  if (t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName || ''))) return;
  if (!panes.focusIndex(n)) return;
  e.preventDefault();
});

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------
// The pane counts, paints its readouts and sends its client stats; these are
// the one interval per concern that step it.
setInterval(() => {
  panes.each((p) => { p.tick(); paintPaneHeader(p); });
  paintConnection();
  refreshPill();
}, 1000);

setInterval(() => panes.each((p) => p.ping()), 500);

// iOS does not merely throttle a backgrounded or locked page, it freezes it:
// rAF stops, timers stop, and a reconnect scheduled from onClose may never run
// -- or never be scheduled at all, if the freeze lands first. A locked phone
// therefore sat 56 minutes without one connect attempt and came back only on a
// manual reload. visibilitychange still fires *before* the freeze, so both the
// release and the resume are done from these handlers and nothing afterwards
// is assumed to run. The pane's suspend and resume are the guarded pair
// (trap 21).

document.addEventListener('visibilitychange', () => {
  if (document.hidden) panes.each((p) => p.suspend()); else panes.each((p) => p.resume());
});
// A page restored from the back/forward cache can come back without firing
// visibilitychange at all, so pageshow is the only signal some resumes
// produce. Both funnel through the same guarded pair.
window.addEventListener('pageshow', () => panes.each((p) => p.resume()));
window.addEventListener('pagehide', () => panes.each((p) => p.suspend()));

// Refit on anything that can change the available box: rotation, Safari's
// toolbars sliding in and out (which move the visual viewport without firing a
// resize on window in every case), and the nav bar appearing once a target
// reports that it accepts control.
if (typeof ResizeObserver !== 'undefined') {
  new ResizeObserver(fitScreen).observe(el.stage);
} else {
  window.addEventListener('resize', fitScreen);
}

window.addEventListener('orientationchange', () => setTimeout(fitScreen, 100));
if (window.visualViewport) {
  // The phone's keyboard moves the visual viewport and nothing else: iOS
  // fires no window resize for it, so sizeToScreen is called from here too.
  window.visualViewport.addEventListener('resize', () => { sizeToScreen(); fitScreen(); });
}
syncDock();
fitScreen();

// Layout self-check, for a browser session to evaluate against the live page:
// __loupeLayoutCheck() returning [] is the pass. See livecheck.js for the rules.
window.__loupeLayout = () => collectLayout(document);
window.__loupeLayoutCheck = () => checkLayout(collectLayout(document));

// What the phone's viewport actually is, once after load and after each turn.
// A Home Screen launch sizes the page differently from a Safari tab, and the
// gap left below the stage there is only measurable on the phone. insets are
// read off a probe padded by env(), since script cannot read env() directly.
function reportViewport(why) {
  const probe = document.createElement('div');
  probe.style.cssText = 'position:fixed;visibility:hidden;pointer-events:none;' +
    'padding:env(safe-area-inset-top) env(safe-area-inset-right) ' +
    'env(safe-area-inset-bottom) env(safe-area-inset-left)';
  document.body.appendChild(probe);
  const cs = getComputedStyle(probe);
  const px = (v) => Math.round(parseFloat(v) || 0);
  const insets = [cs.paddingTop, cs.paddingRight, cs.paddingBottom, cs.paddingLeft].map(px);
  probe.remove();
  const bottom = (node) => (node ? Math.round(node.getBoundingClientRect().bottom) : null);
  const vv = window.visualViewport;
  report({
    event: 'viewport', why,
    standalone: !!navigator.standalone || matchMedia('(display-mode: standalone)').matches,
    screen: `${screen.width}x${screen.height}`,
    inner: `${innerWidth}x${innerHeight}`,
    visual: vv ? `${Math.round(vv.width)}x${Math.round(vv.height)}` : null,
    client: document.documentElement.clientHeight,
    insets,
    htmlBottom: bottom(document.documentElement),
    bodyBottom: bottom(document.body),
    stageBottom: bottom(el.stage),
    layout: checkLayout(collectLayout(document)),
  });
}
// A Home Screen launch in portrait fills the screen, so it is sized to the
// screen rather than to a viewport iOS sometimes reports 62px short (see
// style.css). screen.height is the long side on iOS whichever way it is held;
// landscape keeps 100dvh until a report shows what it does there.
//
// While the phone's keyboard is up the page is sized to the visual viewport
// instead, which is what the keyboard shrinks: iOS leaves the layout
// viewport, and so 100dvh, at full height and lays the keyboard over the
// bottom of the page. --keyboard-h lifts the fixed corners by the same
// amount, or the button that dismisses the keyboard would sit under it.
// The strip's height, matching .keyStrip in style.css.
const STRIP_H = 44;
function sizeToScreen() {
  const standalone = !!navigator.standalone || matchMedia('(display-mode: standalone)').matches;
  const root = document.documentElement.style;
  const vv = window.visualViewport;
  if (keyboard.isOpen() && vv) {
    const covered = Math.max(0, Math.round(innerHeight - vv.height - vv.offsetTop));
    // The key strip sits on the keyboard, so the page and the corners clear
    // both. --keyboard-cover is the keyboard alone, where the strip sits.
    const strip = keyboard.stripShown() ? STRIP_H : 0;
    root.setProperty('--app-h', `${Math.round(vv.height) - strip}px`);
    root.setProperty('--keyboard-h', `${covered + strip}px`);
    root.setProperty('--keyboard-cover', `${covered}px`);
    return;
  }
  root.removeProperty('--keyboard-h');
  root.removeProperty('--keyboard-cover');
  if (standalone && innerHeight > innerWidth) {
    root.setProperty('--app-h', `${Math.max(screen.width, screen.height)}px`);
  } else {
    root.removeProperty('--app-h');
  }
}
sizeToScreen();
window.addEventListener('resize', sizeToScreen);
keyboard.onChange(() => { sizeToScreen(); fitScreen(); });
setTimeout(() => reportViewport('load'), 1000);
window.addEventListener('orientationchange', () => setTimeout(() => reportViewport('turn'), 500));

report({ event: 'module-end', build: CLIENT_BUILD });
loadTargets();
