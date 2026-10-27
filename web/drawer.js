// drawer.js -- the device picker, a sheet that rises from the pill
// (node 3:3 dark, 3:131 light).
//
// groupTargets/rowModel are pure and tested against
// testdata/client/drawer.json without touching the DOM; mount() is the DOM
// shell around them.
//
// /api/targets (internal/httpapi/manager.go's TargetInfo) returns `id`,
// `kind`, `label`, `active` and `available`, and optionally `state`,
// `reason`, `lastError`, `bootsOnOpen` and the description fields (`form`,
// `os`, `osVersion`, `apiLevel`, `transport`), which are omitted rather than
// guessed when a target has not described itself.
//
// Groups: Running, holding the current target while it can open and every
// other device that streams without booting (`isRunning`), the current one
// first, then one per platform -- Android, iOS, and Other for the synthetic pattern -- in
// the server's order. The server keeps a device adb stops listing as
// `state: "missing"` rather than dropping it, so a viewer who had it
// selected can see where it went; such a row stays in its platform's group,
// dimmed and disabled with the reason in its line, since opening it would
// only fail with that reason. A target with no `available` field is
// available, which is what a server from before this tracking sends.
//
// "Shut down" marks a registered AVD that has not booted. The signal is the
// server's `bootsOnOpen`, not an inference over `kind`/`osVersion`: an
// ADBTarget for an attached `emulator-5554` is also `kind: "emulator"`, and
// an emulator whose Describe lost the race against describeTimeout reads the
// same as one that never booted. `bootsOnOpen` is absent, not false, for
// anything that never boots. The line names no boot time: a warm AVD opens
// in about 2s and a cold one in 12-22s, and the listing does not say which.
import { showToast } from './toast.js';

// openIds, when given, is the open panes' target ids in pane order: each
// open row carries its pane's 1-based index as badge, open rows lead
// "Running" in that order, and a row that can stream and is not open carries
// beside: true, the offer to open it in another pane. Without openIds the
// rows carry neither key, which is the single-pane picker.
//
// server, when given, is the picker's server filter (serverFilterOf): 'all',
// 'local' for this server's own devices, or a peer's name.
export function groupTargets(list, currentId, query, fps, openIds, server) {
  const q = String(query || '').trim().toLowerCase();
  const matches = (t) => {
    if (!q) return true;
    const hay = [t.label, t.id, t.server, t.os, t.osVersion, TRANSPORT_WORDS[t.transport]]
      .filter((v) => v != null && v !== '')
      .join(' ')
      .toLowerCase();
    return hay.includes(q);
  };

  const order = ['Running', 'Android', 'iOS', 'Other'];
  const byTitle = new Map(order.map((title) => [title, []]));
  for (const t of list || []) {
    if (!t || !matches(t) || !onServer(t, server)) continue;
    const row = rowModel(t, currentId, fps);
    const paneAt = openIds ? openIds.indexOf(t.id) : -1;
    if (paneAt >= 0) row.badge = paneAt + 1;
    else if (openIds && !row.disabled && t.state !== 'creating') row.beside = true;
    // The current target goes under its platform when it cannot open: it is
    // not running, and "Running" above it would say it was.
    const open = row.current || paneAt >= 0;
    const running = open ? !row.disabled : isRunning(t);
    const title = running ? 'Running' : platformOf(t);
    if (running && open && !openIds) byTitle.get(title).unshift(row);
    else byTitle.get(title).push(row);
  }
  if (openIds) {
    // Open rows lead Running in pane order; the rest keep the list's order.
    const rank = (r) => (r.badge ? r.badge : openIds.length + 1);
    byTitle.get('Running').sort((x, y) => rank(x) - rank(y));
  }
  return order
    .filter((title) => byTitle.get(title).length)
    .map((title) => ({ title, rows: byTitle.get(title) }));
}

// Server filter values: 'all', 'local' (devices this server owns) or a peer's
// name. A peer's rows carry `server: "<name>"`; this server's own do not.
export const FILTER_ALL = 'all';
export const FILTER_LOCAL = 'local';
export const SERVER_FILTER_KEY = 'loupe.serverFilter';

// onServer is whether row t passes the filter. An absent filter passes all.
export function onServer(t, filter) {
  if (!filter || filter === FILTER_ALL) return true;
  if (filter === FILTER_LOCAL) return !t.server;
  return t.server === filter;
}

// peerNames is the peers the list mentions, in first-seen order.
export function peerNames(list) {
  const names = [];
  for (const t of list || []) {
    if (t && typeof t.server === 'string' && t.server && !names.includes(t.server)) names.push(t.server);
  }
  return names;
}

// serverFilterOf is the filter in force for a saved choice: a peer that has
// gone from the list, or any choice once no peer remains, falls back to All
// without forgetting what was saved (the peer may be back after a restart).
export function serverFilterOf(saved, list) {
  const names = peerNames(list);
  if (!names.length) return FILTER_ALL;
  if (saved === FILTER_LOCAL || names.includes(saved)) return saved;
  return FILTER_ALL;
}

// serverOptions is the filter's choices, or [] when no peer exists: with
// nothing to tell apart there is nothing to filter.
export function serverOptions(list) {
  const names = peerNames(list);
  if (!names.length) return [];
  return [{ value: FILTER_ALL, label: 'All' }, { value: FILTER_LOCAL, label: 'This server' }]
    .concat(names.map((n) => ({ value: n, label: n })));
}

export function readServerFilter(storage) {
  try { return storage.getItem(SERVER_FILTER_KEY) || FILTER_ALL; } catch (_) { return FILTER_ALL; }
}

export function writeServerFilter(storage, value) {
  try { storage.setItem(SERVER_FILTER_KEY, value); } catch (_) {}
}

// isRunning is targets.js's, repeated because a module here imports only
// toast.js (trap 45). A test holds the two to the same answers.
export function isRunning(t) {
  return !!t && t.available !== false && !t.bootsOnOpen && t.state !== 'creating' && t.kind !== 'synthetic';
}

function platformOf(t) {
  // A simulator being created has no `sim:` id yet; its pending row is
  // `sim-new:<name>`, and reads as iOS whatever else it carries.
  if (typeof t.id === 'string' && (t.id.startsWith('sim-new:') || t.id.startsWith('sim:'))) return 'iOS';
  if (t.os === 'android' || t.kind === 'device' || t.kind === 'emulator') return 'Android';
  if (t.os === 'ios' || t.kind === 'simulator' || t.kind === 'iosDevice') return 'iOS';
  return 'Other';
}

// Words for adb's device states, with what to do about each where there is
// something. adb's own word is kept for anything not listed, since it is
// still more than "Unavailable".
const STATE_WORDS = {
  missing: 'Not connected',
  unauthorized: 'Not authorized · accept on the phone',
  offline: 'Offline',
  adbUnreachable: 'adb not responding',
};

const TRANSPORT_WORDS = {
  usb: 'USB',
  wifi: 'Wi-Fi',
  emulator: 'Emulator',
  simulator: 'Simulator',
};

const KNOWN_ICONS = new Set(['phone', 'foldable', 'tablet', 'pattern']);

// rowModel(t, currentId, fps) -> {id, label, icon, tile, detail, tone,
// current, disabled}. tone is 'live' for the streaming row, 'warn' for one
// that cannot open, and '' otherwise; tile is the icon's platform colour.
// fps is the decoder's rate, shown on the streaming row once it is above 0.
export function rowModel(t, currentId, fps) {
  const icon = KNOWN_ICONS.has(t.form) ? t.form : (t.kind === 'synthetic' ? 'pattern' : 'phone');
  const current = t.id === currentId;
  const disabled = t.available === false;
  const platform = platformOf(t);
  const tile = platform === 'Android' ? 'android' : platform === 'iOS' ? 'ios' : 'other';

  const parts = [];
  if (TRANSPORT_WORDS[t.transport]) parts.push(TRANSPORT_WORDS[t.transport]);
  else if (t.kind === 'emulator') parts.push('Emulator');
  else if (t.kind === 'simulator' || (typeof t.id === 'string' && t.id.startsWith('sim-new:'))) parts.push('Simulator');
  // A headless emulator has no window on the Mac: this row is the only place
  // to see it.
  if (t.headless) parts.push('Headless');

  let tone = '';
  let progress = false;
  if (t.state === 'creating') {
    // The server's reason says which part it is in, with the time so far;
    // the bar is indeterminate, since the CLI reports no bytes.
    parts.push(t.reason || 'Creating');
    tone = 'busy';
    progress = true;
  } else if (disabled && t.reason === 'serverDown') {
    // The owner has stopped answering; the row is its last known state.
    parts.push(t.server ? `${t.server} is unreachable` : 'Server unreachable');
    tone = 'warn';
  } else if (disabled) {
    parts.push(STATE_WORDS[t.state] || t.reason || t.state || 'Unavailable');
    tone = 'warn';
  } else if (t.bootsOnOpen) {
    parts.push('Shut down · boots when opened');
  } else {
    if (t.osVersion) {
      const osName = t.os === 'ios' ? 'iOS' : t.os ? t.os.charAt(0).toUpperCase() + t.os.slice(1) : '';
      parts.push(osName ? `${osName} ${t.osVersion}` : String(t.osVersion));
      if (t.apiLevel) parts.push(`API ${t.apiLevel}`);
    } else if (t.kind === 'synthetic') {
      parts.push('Test pattern');
    }
    if (current) {
      if (fps > 0) parts.push(`${Math.round(fps)} fps`);
      tone = 'live';
    }
  }

  return { id: t.id, label: t.label, icon, tile, detail: parts.join(' · '), tone, current, disabled, progress, ...(t.server ? { server: String(t.server) } : {}), actions: rowActions(t).concat(agentActions(t)), ...agentLine(t) };
}

// agentLine is the row's second status line, about agents: {agent, agentTone,
// agentLink}. Hands off outranks a claim, and a claim outranks driving,
// because hands off refuses the claim's holder too. A claim's holder is the
// one driving it, so its line says both. agentLink is an https URL the holder
// sent, or ''.
export function agentLine(t) {
  const claim = t.claim && typeof t.claim === 'object' ? t.claim : null;
  const driver = typeof t.drivenBy === 'string' ? t.drivenBy : '';
  if (t.handsOff) {
    return { agent: driver ? `Hands off · ${driver} was refused` : 'Hands off · agents are refused', agentTone: 'handsoff', agentLink: '' };
  }
  if (claim) {
    const who = claim.agent || claim.session || 'an agent';
    const link = typeof claim.link === 'string' && claim.link.startsWith('https://') ? claim.link : '';
    return { agent: driver ? `${who} is driving` : `Claimed by ${who}`, agentTone: driver ? 'driving' : 'claimed', agentLink: link };
  }
  if (driver) return { agent: `${driver} is driving`, agentTone: 'driving', agentLink: '' };
  return { agent: '', agentTone: '', agentLink: '' };
}

// agentActions is what the person can do about agents from the row: mark it
// hands off or allow agents again, and end someone else's claim. Any row the
// server lists can be marked, a device included; a creation row cannot,
// since it is not a target yet.
export function agentActions(t) {
  const id = typeof t.id === 'string' ? t.id : '';
  if (!id || id.startsWith('sim-new:') || t.state === 'creating' || t.state === 'createFailed') return [];
  const out = [];
  if (t.claim) out.push('release');
  out.push(t.handsOff ? 'allow' : 'handsoff');
  return out;
}

// Which swipe actions a row offers. internal/httpapi/devicelife.go's
// deviceActions is the same rule; keep them together.
export function rowActions(t) {
  const id = typeof t.id === 'string' ? t.id : '';
  if (!id.startsWith('avd:') && !id.startsWith('sim:')) return [];
  if (t.state === 'creating' || t.available === false) return [];
  return t.bootsOnOpen ? ['delete'] : ['shutdown', 'delete'];
}

// swipeDecide turns a pointer's travel into the row's offset. The axis locks
// once either distance passes 10px, so a vertical scroll never drags a row.
export function swipeDecide({ dx, dy, locked, openWidth, wasOpen }) {
  let lock = locked || null;
  if (!lock && (Math.abs(dx) > 10 || Math.abs(dy) > 10)) lock = Math.abs(dx) > Math.abs(dy) ? 'x' : 'y';
  const base = wasOpen ? -openWidth : 0;
  const offset = lock === 'x' ? Math.max(-openWidth, Math.min(0, base + dx)) : base;
  return { locked: lock, offset, open: offset < -openWidth / 2, swallowClick: lock === 'x' };
}

// shouldDefer is whether a refresh must wait: a repaint under a finger, or
// under an open row, would replace the row being swiped.
export function shouldDefer({ pointerDown, openId, busy } = {}) {
  return !!pointerDown || !!openId || !!busy;
}

// swipeStart decides what a pointerdown in the list begins: 'ignore',
// 'closeOpen' (a touch outside the open row, which only closes it and must
// not also pick what it landed on) or 'drag'. While an action runs every
// touch is ignored, so a busy row is never closed or re-entered.
export function swipeStart({ openId, swipeId, onAction, busy } = {}) {
  if (busy) return 'ignore';
  if (openId != null && swipeId !== openId) return 'closeOpen';
  if (!swipeId || onAction) return 'ignore';
  return 'drag';
}

// readActionResult reads a shutdown or delete response into
// {ok, outcome, message}. A 204 (delete) has no body and is never parsed;
// shutdown's 200 body is a row nothing here needs, so ok is not parsed
// either. Only a refusal reads {error, code}. outcome is 'ok', the code, or
// the status.
export async function readActionResult(res) {
  if (res.ok) return { ok: true, outcome: 'ok', message: '' };
  const body = await res.json().catch(() => null);
  return {
    ok: false,
    outcome: (body && body.code) || String(res.status),
    message: actionRefusal(res.status, body),
  };
}

// actionRefusal is the toast for a shutdown or delete the server refused.
// body is the parsed {error, code} or null. 409 busy, recording and
// humanHeld and 422 notSupported all carry a message worth showing.
export function actionRefusal(status, body) {
  if (body && body.code === 'recording') return 'Stop the recording first';
  if (body && body.error) return String(body.error);
  return `Could not complete that (${status})`;
}

// ---------------------------------------------------------------------------
// DOM shell
// ---------------------------------------------------------------------------

// White glyphs on a platform-coloured tile, 24px box.
const ICON_PATHS = {
  phone: '<rect x="7.5" y="3" width="9" height="18" rx="2"/>',
  foldable: '<rect x="4.5" y="4" width="7" height="16" rx="1.5"/><rect x="12.5" y="4" width="7" height="16" rx="1.5"/>',
  tablet: '<rect x="4" y="4" width="16" height="16" rx="2"/>',
  pattern: '<path d="M5 12h14M12 5v14"/>',
};

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[c]));
}

export function rowHTML(row, { paneKey = '⌃' } = {}) {
  const disabledAttrs = row.disabled ? 'disabled aria-disabled="true"' : '';
  const tone = row.tone ? ` pickerRowDetail--${escapeHTML(row.tone)}` : '';
  const actions = row.actions || [];
  // VoiceOver has no other route to a gesture-only action.
  const hint = actions.length ? ' aria-description="Swipe left for actions"' : '';
  const button = `
    <button type="button" class="pickerRow" data-id="${escapeHTML(row.id)}" role="option"
      aria-selected="${String(!!row.current)}"${hint} ${disabledAttrs}>
      <span class="pickerTile pickerTile--${escapeHTML(row.tile || 'other')}" aria-hidden="true">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
             stroke-linecap="round" stroke-linejoin="round">${ICON_PATHS[row.icon] || ICON_PATHS.phone}</svg>
      </span>
      <span class="pickerRowMain">
        ${row.server
    ? `<span class="pickerRowTitle"><span class="pickerRowLabel">${escapeHTML(row.label)}</span><span class="pickerRowServer">${escapeHTML(row.server)}</span></span>`
    : `<span class="pickerRowLabel">${escapeHTML(row.label)}</span>`}
        ${row.detail ? `<span class="pickerRowDetail${tone}">${escapeHTML(row.detail)}</span>` : ''}
        ${row.agent ? `<span class="pickerRowAgent pickerRowAgent--${escapeHTML(row.agentTone)}">${escapeHTML(row.agent)}</span>` : ''}
        ${row.progress ? '<span class="pickerProgress" aria-hidden="true"><span></span></span>' : ''}
      </span>
      ${row.badge ? `<span class="pickerBadge" aria-label="Pane ${row.badge}">${escapeHTML(paneKey)}${row.badge}</span>` : ''}
      ${row.current && !row.disabled ? '<span class="pickerCheck" aria-hidden="true">&#x2713;</span>' : ''}
    </button>`;
  // Open beside sits outside the row button, like the session link: a
  // button inside a button is not reachable on its own.
  const beside = row.beside
    ? `\n    <button type="button" class="pickerBeside" data-beside="${escapeHTML(row.id)}" aria-label="Open ${escapeHTML(row.label)} beside">Open beside</button>` : '';
  // The link back to a claim's session sits outside the row button, since a
  // link inside a button is not reachable on its own.
  const link = row.agentLink && /^https:\/\//.test(row.agentLink)
    ? `<a class="pickerAgentLink" href="${escapeHTML(row.agentLink)}" target="_blank" rel="noopener noreferrer">Open session</a>` : '';
  if (!actions.length) return beside ? `<div class="pickerRowWrap">${button}${beside}</div>${link}` : button + link;
  const id = escapeHTML(row.id);
  const buttons = actions.map((act) => `
      <button type="button" class="pickerAction pickerAction--${act}" data-act="${act}" data-id="${id}" tabindex="-1">${ACTION_LABELS[act]}</button>`).join('');
  return `
    <div class="pickerSwipe" data-swipe="${id}">${button}
      <div class="pickerActions" aria-hidden="true">${buttons}
      </div>${beside}
    </div>${link}`;
}

const ACTION_LABELS = { shutdown: 'Shut Down', delete: 'Delete', handsoff: 'Hands Off', allow: 'Allow Agents', release: 'Release' };

const NEW_DEVICE_ROW = `
        <button type="button" class="pickerRow" data-action="new">
          <span class="pickerTile pickerTile--add" aria-hidden="true">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
                 stroke-linecap="round"><path d="M12 6v12M6 12h12"/></svg>
          </span>
          <span class="pickerRowMain">
            <span class="pickerRowLabel pickerRowLabel--tint">New device…</span>
            <span class="pickerRowDetail">Emulator on this Mac</span>
          </span>
        </button>`;

const ADD_SERVER_ROW = `
        <button type="button" class="pickerRow" data-action="server">
          <span class="pickerTile pickerTile--add" aria-hidden="true">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
                 stroke-linecap="round"><path d="M12 6v12M6 12h12"/></svg>
          </span>
          <span class="pickerRowMain">
            <span class="pickerRowLabel pickerRowLabel--tint">Add server…</span>
            <span class="pickerRowDetail">Another Mac running loupe</span>
          </span>
        </button>`;

// addGroupHTML is the Add group: New device… once the server can create one,
// Add server… whenever the picker can open that sheet.
export function addGroupHTML({ newDevice = false, server = false } = {}) {
  if (!newDevice && !server) return '';
  return `
    <div class="pickerGroup">
      <div class="pickerGroupTitle">Add</div>
      <div class="pickerGroupRows">${newDevice ? NEW_DEVICE_ROW : ''}${server ? ADD_SERVER_ROW : ''}
      </div>
    </div>`;
}

// serversHTML is the server filter: a row of buttons, or '' with no peer.
export function serversHTML(options, active) {
  if (!options.length) return '';
  return options.map((o) => `<button type="button" class="pickerChip" role="radio" data-server="${escapeHTML(o.value)}"
      aria-checked="${String(o.value === active)}">${escapeHTML(o.label)}</button>`).join('');
}

function groupHTML(group, opts) {
  return `
    <div class="pickerGroup">
      <div class="pickerGroupTitle">${escapeHTML(group.title)}</div>
      <div class="pickerGroupRows">${group.rows.map((r) => rowHTML(r, opts)).join('')}</div>
    </div>`;
}

// handlePick is the picker's whole reaction to a row tap, factored out of
// mount() so a test can call it with plain function stubs instead of a real
// DOM: it closes the picker, then reports the pick. That order matters --
// onPick drives a reconnect (trap 41's generation-guarded path), and it must
// not run while the picker still thinks it is open. A tap on the row already
// streaming only closes: picking it again would reconnect for nothing.
// pickIntent(event, offered) -> 'beside' | 'pick'. A ⌘- or ctrl-click on a
// row offered beside opens it in another pane; any other click picks.
export function pickIntent(e, offered) {
  return offered && e && (e.metaKey || e.ctrlKey) ? 'beside' : 'pick';
}

export function handlePick(id, { close, onPick, currentId } = {}) {
  if (close) close();
  if (onPick && id !== currentId) onPick(id);
}

// refocusTarget(previousId, availableIds) -> string | null
//
// Pure decision behind refresh()'s "put focus back where it was" step: given
// the id of the row that was focused before a refresh replaced the list,
// and the ids still present afterward, returns that id if it survived or
// null if it did not. The caller decides what "no survivor" falls back to,
// because that choice depends on the pointer type.
export function refocusTarget(previousId, availableIds) {
  if (previousId == null) return null;
  return (availableIds || []).includes(previousId) ? previousId : null;
}

// FOCUSABLE selects everything a focus trap needs to cycle Tab across. It
// deliberately excludes disabled/hidden elements via the browser's own
// tabIndex computation rather than trying to reproduce that logic here.
const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

// A drag down on the handle or header further than this closes the sheet.
const DRAG_CLOSE_PX = 80;

// mount(root, {scrim, trigger, onPick, fps}) renders the picker, wires the
// search input, and calls onPick(id) on a row tap. fps() is read on each
// render for the streaming row, and once a second while open, when only that
// row's detail text is rewritten: a full render would drop the list's focus.
// root and scrim start `hidden`; this owns flipping that attribute for both,
// since nothing else opens or closes the picker.
// onNewDevice, when given, adds the Add group's New device… row, shown once
// setCanCreate(true) says the server has the android CLI.
// onAction(id, action) -> Promise runs a row's swipe action ('shutdown' or
// 'delete'); a rejection's message is toasted on `toast`.
// openIds and onOpenBeside, when given, are the side-by-side picker: openIds()
// is read on each render (null in single-pane mode), and an Open beside tap
// or a ⌘/ctrl-click on an offered row calls onOpenBeside(id) instead of onPick.
export function mount(root, { scrim, trigger, onPick, fps, onNewDevice, onAction, toast, openIds, onOpenBeside, paneKey, onAddServer, storage } = {}) {
  root.setAttribute('role', 'dialog');
  root.setAttribute('aria-modal', 'true');
  let pinned = false;
  root.setAttribute('aria-labelledby', 'pickerTitle');
  // -1: focusable programmatically (open() may focus the dialog itself
  // rather than the search field -- see the pointer:fine gate below) but not
  // reachable by Tab, which is exactly what a dialog root needs.
  root.setAttribute('tabindex', '-1');
  root.innerHTML = `
    <div class="pickerTop">
      <div class="sheetHandle" aria-hidden="true"><div class="sheetGrip"></div></div>
      <div class="pickerHeader">
        <span></span>
        <h2 id="pickerTitle" class="pickerTitle">Devices</h2>
        <button type="button" class="pickerDone">Done</button>
      </div>
    </div>
    <div class="pickerSearchWrap">
      <input id="drawerSearch" class="pickerSearch" type="search"
             placeholder="Search" aria-label="Search devices" autocomplete="off">
    </div>
    <div id="drawerServers" class="pickerChips" role="radiogroup" aria-label="Server" hidden></div>
    <div id="drawerList" class="pickerList" role="listbox" aria-label="Devices"></div>
  `;
  const top = root.querySelector('.pickerTop');
  const done = root.querySelector('.pickerDone');
  const search = root.querySelector('#drawerSearch');
  const list = root.querySelector('#drawerList');
  const chips = root.querySelector('#drawerServers');
  let savedFilter = storage ? readServerFilter(storage) : FILTER_ALL;

  let targets = [];
  let currentId = null;
  let isOpen = false;
  let lastFocused = null;
  let canCreate = false;
  let fpsTicker = null;

  // tickFps rewrites the streaming row's detail with the decoder's rate.
  function tickFps() {
    const t = targets.find((x) => x.id === currentId);
    if (!t || !fps) return;
    const row = Array.from(list.querySelectorAll('.pickerRow[data-id]')).find((b) => b.getAttribute('data-id') === currentId);
    const detail = row && row.querySelector('.pickerRowDetail');
    if (!detail) return;
    const text = rowModel(t, currentId, fps()).detail;
    if (detail.textContent !== text) detail.textContent = text;
  }

  // A swipe is a finger on a row, or a row left open. A repaint during either
  // would replace the row being worked, so refresh() parks the newest list in
  // pendingTargets and flush() applies it once both have ended.
  let pointerDown = false;
  let openId = null;
  let swallow = false;
  let gesture = null;
  let pendingTargets = null;

  function swipeEl(id) {
    return Array.from(list.querySelectorAll('.pickerSwipe')).find((el) => el.getAttribute('data-swipe') === id) || null;
  }

  function setRowOpen(el, on) {
    const row = el.querySelector('.pickerRow');
    const acts = el.querySelector('.pickerActions');
    row.style.transition = '';
    row.style.transform = on ? `translateX(${-acts.offsetWidth}px)` : '';
    acts.setAttribute('aria-hidden', on ? 'false' : 'true');
    for (const b of acts.querySelectorAll('.pickerAction')) b.tabIndex = on ? 0 : -1;
  }

  function closeRow() {
    if (openId == null) return;
    const el = swipeEl(openId);
    openId = null;
    if (el) setRowOpen(el, false);
  }

  // Ids whose shutdown or delete is in flight. While any is, the list is held,
  // touches are ignored and render() re-marks the row busy.
  const busyIds = new Set();

  function markBusy() {
    for (const id of busyIds) swipeEl(id)?.classList.add('pickerSwipe--busy');
  }

  function flush() {
    if (shouldDefer({ pointerDown, openId, busy: busyIds.size }) || !pendingTargets) return;
    const next = pendingTargets;
    pendingTargets = null;
    refresh(next);
  }

  // A click that follows a drag, or one that only closed an open row, must
  // not pick the row under it. A drag's click arrives right after pointerup,
  // so it gets a short timer. An outside-close touch has no bound on how long
  // it is held, so its swallow is armed without a timer and is released by
  // the click, or, when no click comes (a cancelled touch), shortly after the
  // matching pointerup or pointercancel.
  let swallowPid = null;
  function swallowNextClick() {
    swallow = true;
    setTimeout(() => { if (swallowPid == null) swallow = false; }, 120);
  }
  function swallowUntilRelease(pid) {
    swallow = true;
    swallowPid = pid;
  }
  function releaseSwallow(ev) {
    if (swallowPid == null || ev.pointerId !== swallowPid) return;
    swallowPid = null;
    setTimeout(() => { if (swallowPid == null) swallow = false; }, 120);
  }
  list.addEventListener('pointerup', releaseSwallow);
  list.addEventListener('pointercancel', releaseSwallow);

  list.addEventListener('click', (ev) => {
    // While an action runs no tap reaches a row or an action.
    if (busyIds.size) {
      ev.stopPropagation();
      ev.preventDefault();
      return;
    }
    if (!swallow) return;
    swallow = false;
    swallowPid = null;
    ev.stopPropagation();
    ev.preventDefault();
  }, true);

  list.addEventListener('pointerdown', (ev) => {
    if (ev.pointerType === 'mouse' && ev.button !== 0) return;
    const sw = ev.target.closest('.pickerSwipe');
    const swipeId = sw ? sw.getAttribute('data-swipe') : null;
    const start = swipeStart({
      openId, swipeId, onAction: !!ev.target.closest('.pickerAction'), busy: busyIds.size,
    });
    if (start === 'closeOpen') {
      closeRow();
      swallowUntilRelease(ev.pointerId);
      flush();
      return;
    }
    if (start !== 'drag') return;
    pointerDown = true;
    gesture = {
      sw, pid: ev.pointerId, x0: ev.clientX, y0: ev.clientY, locked: null,
      openWidth: sw.querySelector('.pickerActions').offsetWidth,
      wasOpen: sw.getAttribute('data-swipe') === openId,
      res: null,
    };
  });

  list.addEventListener('pointermove', (ev) => {
    const g = gesture;
    if (!g || ev.pointerId !== g.pid) return;
    const res = swipeDecide({
      dx: ev.clientX - g.x0, dy: ev.clientY - g.y0, locked: g.locked,
      openWidth: g.openWidth, wasOpen: g.wasOpen,
    });
    const first = res.locked === 'x' && g.locked !== 'x';
    g.locked = res.locked;
    g.res = res;
    if (res.locked !== 'x') return;
    if (first) g.sw.setPointerCapture?.(ev.pointerId);
    const row = g.sw.querySelector('.pickerRow');
    row.style.transition = 'none';
    row.style.transform = `translateX(${res.offset}px)`;
  });

  function endGesture() {
    const g = gesture;
    if (!g) return;
    gesture = null;
    pointerDown = false;
    const id = g.sw.getAttribute('data-swipe');
    if (g.locked === 'x') {
      swallowNextClick();
      if (g.res && g.res.open) {
        openId = id;
        setRowOpen(g.sw, true);
      } else {
        if (openId === id) openId = null;
        setRowOpen(g.sw, false);
      }
    } else if (g.wasOpen) {
      // A tap on the open row only closes it.
      closeRow();
      swallowNextClick();
    }
    flush();
  }
  list.addEventListener('pointerup', (ev) => { if (gesture && ev.pointerId === gesture.pid) endGesture(); });
  list.addEventListener('pointercancel', (ev) => { if (gesture && ev.pointerId === gesture.pid) endGesture(); });

  list.addEventListener('click', async (ev) => {
    const b = ev.target.closest('.pickerAction');
    if (!b || !onAction) return;
    const sw = b.closest('.pickerSwipe');
    const id = b.getAttribute('data-id');
    if (busyIds.has(id)) return;
    busyIds.add(id);
    sw.classList.add('pickerSwipe--busy');
    try {
      await onAction(id, b.getAttribute('data-act'));
    } catch (err) {
      showToast(toast, (err && err.message) || 'Could not complete that');
    } finally {
      busyIds.delete(id);
      swipeEl(id)?.classList.remove('pickerSwipe--busy');
      if (sw.isConnected) closeRow(); else openId = null;
      flush();
    }
  });

  function render() {
    // A render rebuilds every row closed, so the swipe state ends here, and
    // it takes the list a held refresh was waiting to show.
    openId = null;
    pointerDown = false;
    gesture = null;
    if (pendingTargets) { targets = pendingTargets; pendingTargets = null; }
    const open = openIds && onOpenBeside ? openIds() : null;
    const filter = serverFilterOf(savedFilter, targets);
    const options = serverOptions(targets);
    chips.hidden = options.length === 0;
    chips.innerHTML = serversHTML(options, filter);
    const groups = groupTargets(targets, currentId, search.value, fps ? fps() : 0, open || undefined, filter);
    const adding = !search.value.trim();
    list.innerHTML = (groups.length
      ? groups.map((g) => groupHTML(g, { paneKey })).join('')
      : '<div class="pickerEmpty">No devices match.</div>')
      + (adding ? addGroupHTML({ newDevice: !!(canCreate && onNewDevice), server: !!onAddServer }) : '');
    const add = list.querySelector('[data-action="new"]');
    if (add) add.addEventListener('click', () => onNewDevice());
    const addSrv = list.querySelector('[data-action="server"]');
    if (addSrv) addSrv.addEventListener('click', () => onAddServer());
    for (const btn of list.querySelectorAll('.pickerRow[data-id]')) {
      btn.addEventListener('click', (e) => {
        if (btn.disabled) return;
        const id = btn.getAttribute('data-id');
        const offered = !!(open && onOpenBeside && !open.includes(id));
        if (pickIntent(e, offered) === 'beside') { close(); onOpenBeside(id); return; }
        handlePick(id, { close, onPick, currentId });
      });
    }
    for (const btn of list.querySelectorAll('.pickerBeside')) {
      btn.addEventListener('click', () => { close(); onOpenBeside(btn.getAttribute('data-beside')); });
    }
    markBusy();
  }

  search.addEventListener('input', render);
  chips.addEventListener('click', (ev) => {
    const b = ev.target.closest('[data-server]');
    if (!b) return;
    savedFilter = b.getAttribute('data-server');
    if (storage) writeServerFilter(storage, savedFilter);
    render();
  });
  done.addEventListener('click', () => close());

  // Focus trap: Tab and Shift+Tab cycle within the dialog's own focusable
  // elements instead of escaping to whatever sits behind the scrim.
  // Recomputed on every Tab, since the search filtering changes which rows
  // exist.
  function trapTab(ev) {
    const focusable = Array.from(root.querySelectorAll(FOCUSABLE));
    if (!focusable.length) {
      ev.preventDefault();
      return;
    }
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    const active = root.getRootNode ? root.getRootNode().activeElement : document.activeElement;
    if (ev.shiftKey) {
      if (active === first || !focusable.includes(active)) {
        ev.preventDefault();
        last.focus();
      }
    } else if (active === last) {
      ev.preventDefault();
      first.focus();
    }
  }

  function onKeydown(ev) {
    if (ev.key === 'Escape') {
      ev.stopPropagation();
      close();
      return;
    }
    if (ev.key === 'Tab') trapTab(ev);
  }

  function onScrimClick() { close(); }

  // Drag to close, from the handle or the header only, so a scroll through
  // the list stays a scroll. The sheet follows the finger down and springs
  // back if it is let go short of DRAG_CLOSE_PX.
  let dragStartY = null;
  top.addEventListener('pointerdown', (ev) => {
    if (ev.target === done) return;
    // A pinned picker is a side panel; there is no edge to drag it off.
    if (pinned) return;
    dragStartY = ev.clientY;
    top.setPointerCapture?.(ev.pointerId);
  });
  top.addEventListener('pointermove', (ev) => {
    if (dragStartY == null) return;
    const dy = Math.max(0, ev.clientY - dragStartY);
    root.style.transform = dy ? `translateY(${dy}px)` : '';
  });
  const endDrag = (ev) => {
    if (dragStartY == null) return;
    const dy = ev.clientY - dragStartY;
    dragStartY = null;
    root.style.transform = '';
    if (dy > DRAG_CLOSE_PX) close();
  };
  top.addEventListener('pointerup', endDrag);
  top.addEventListener('pointercancel', endDrag);

  function open(newTargets, newCurrentId) {
    targets = newTargets || [];
    currentId = newCurrentId;
    if (!(pinned && isOpen)) search.value = '';
    render();

    isOpen = true;
    root.hidden = false;
    if (trigger) trigger.setAttribute('aria-expanded', 'true');
    if (fps && fpsTicker == null) fpsTicker = setInterval(tickFps, 1000);
    // Pinned, the picker is a sidebar beside the device rather than a
    // dialog over it: no scrim, no focus trap, and focus stays put.
    if (pinned) return;
    if (scrim) {
      scrim.hidden = false;
      scrim.addEventListener('click', onScrimClick);
    }
    document.addEventListener('keydown', onKeydown, true);

    lastFocused = document.activeElement;
    // Focusing the search field pops the on-screen keyboard on a touch
    // device, which is not what opening a picker should do. A fine pointer
    // (mouse or trackpad) focuses search; anything else focuses the dialog.
    const hasFinePointer = typeof matchMedia === 'function' && matchMedia('(pointer: fine)').matches;
    (hasFinePointer ? search : root).focus();
  }

  function close() {
    if (!isOpen || pinned) return;
    isOpen = false;
    openId = null;
    pointerDown = false;
    gesture = null;
    pendingTargets = null;
    root.hidden = true;
    root.style.transform = '';
    if (scrim) {
      scrim.hidden = true;
      scrim.removeEventListener('click', onScrimClick);
    }
    if (trigger) trigger.setAttribute('aria-expanded', 'false');
    document.removeEventListener('keydown', onKeydown, true);
    clearInterval(fpsTicker);
    fpsTicker = null;

    const toFocus = trigger || lastFocused;
    if (toFocus && typeof toFocus.focus === 'function') toFocus.focus();
    lastFocused = null;
  }

  // refresh updates the target list in place, without open()'s other side
  // effects (clearing the search field, re-adding listeners), and is a no-op
  // while closed rather than opening the picker as a side effect.
  //
  // render() replaces the list's innerHTML wholesale, which destroys the row
  // focus was on and drops focus to the body, outside the trap. So focus goes
  // back to the same row if it survived (refocusTarget), and otherwise to
  // search or the dialog root by pointer type, as open() chooses.
  function refresh(newTargets) {
    if (!isOpen) return;
    if (shouldDefer({ pointerDown, openId, busy: busyIds.size })) {
      pendingTargets = newTargets || [];
      return;
    }
    const active = root.getRootNode ? root.getRootNode().activeElement : document.activeElement;
    const focusWasInList = !!(active && list.contains(active));
    const previousId = focusWasInList ? active.getAttribute('data-id') : null;
    targets = newTargets || [];
    render();
    if (!focusWasInList) return;

    const rows = Array.from(list.querySelectorAll('.pickerRow[data-id]'));
    const survivorId = refocusTarget(previousId, rows.map((b) => b.getAttribute('data-id')));
    const survivorRow = survivorId != null
      ? rows.find((b) => b.getAttribute('data-id') === survivorId)
      : null;
    if (survivorRow && !survivorRow.disabled) {
      survivorRow.focus();
      return;
    }
    const hasFinePointer = typeof matchMedia === 'function' && matchMedia('(pointer: fine)').matches;
    (hasFinePointer ? search : root).focus();
  }

  // setCurrent(id) moves the checkmark. open() is the only other place that
  // sets it, and a pinned picker is opened once and never closes, so without
  // this it kept marking the target the page loaded with after every switch.
  // Mid-swipe it parks the list in pendingTargets, so flush() repaints with
  // the new id once the row is let go.
  function setCurrent(id) {
    if (currentId === id) return;
    currentId = id;
    repaint();
  }

  // repaint() redraws the rows from the list it has, for a change to
  // openIds() that no new list or current id comes with.
  function repaint() {
    if (!isOpen) return;
    if (shouldDefer({ pointerDown, openId, busy: busyIds.size })) {
      pendingTargets = pendingTargets || targets;
      return;
    }
    render();
  }

  function setCanCreate(on) {
    if (canCreate === !!on) return;
    canCreate = !!on;
    if (isOpen) render();
  }

  // pin(on) makes the picker a permanent panel (app.js, at desktop widths):
  // close() does nothing while it holds, so a pick, Escape or the pill leaves
  // it up. Pinning an open picker drops the dialog parts it opened with;
  // unpinning leaves it open for the caller to close.
  function pin(on) {
    pinned = !!on;
    root.setAttribute('aria-modal', pinned ? 'false' : 'true');
    if (pinned && isOpen) {
      if (scrim) {
        scrim.hidden = true;
        scrim.removeEventListener('click', onScrimClick);
      }
      document.removeEventListener('keydown', onKeydown, true);
      lastFocused = null;
    }
  }

  return { open, close, pin, refresh, setCurrent, repaint, setCanCreate, isOpen: () => isOpen };
}
