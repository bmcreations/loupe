// newdevice.js -- the New device sheet (node 25:2 dark, 25:167 light),
// opened from the picker's Add row and from No devices.
//
// One form makes either an Android AVD or an iOS simulator from what is
// installed on the Mac: Platform, OS, Device and Name. The choices come from
// GET /api/devices/options and the create is POST /api/devices. The server
// answers 202 at once and puts the creation in /api/targets as a `creating`
// row, so this sheet closes on the 202 and the picker shows the progress.
//
// An Android AVD becomes the registered `avd:<name>` row in place. An iOS
// simulator's pending row (`sim-new:<name>`) is dropped when the real
// `sim:<udid>` appears, so after an iOS 202 watchCreated() polls the options
// until `job.createdId` names it, and onResolved hands that id to the picker.
//
// A server with only the old `android` CLI says `profilesFallback`, and the
// sheet is then the profile list over /api/avds/profiles it always was.
//
// Everything that is not DOM is a pure, exported function tested in
// newdevice.test.mjs against testdata/client/deviceoptions.json.

import { showToast } from './toast.js';

const ICON_PATHS = {
  phone: '<rect x="7.5" y="3" width="9" height="18" rx="2"/>',
  wide: '<rect x="3" y="6" width="18" height="12" rx="2"/>',
};

// profileRowModel(p) -> {name, label, icon, detail, disabled}.
export function profileRowModel(p) {
  const name = String(p && p.name || '');
  const label = p && p.label ? String(p.label) : name;
  const icon = /phone/.test(name) ? 'phone' : 'wide';
  const disabled = !!(p && p.exists);
  const detail = disabled ? `${name} · already on this Mac` : name;
  return { name, label, icon, detail, disabled };
}

// createError(status, body) -> the toast for a POST that was not accepted.
// body is the parsed ErrorResponse, or null when it was not JSON.
export function createError(status, body) {
  const code = body && body.code;
  if (code === 'creating') return 'Another device is still being created';
  if (code === 'avdExists') return 'That device is already on this Mac';
  if (status === 404) return 'This server can’t create devices: the android CLI is not installed';
  return (body && body.error) || `Couldn’t create the device (${status})`;
}

// ---------------------------------------------------------------------------
// Options -> view model (pure)
// ---------------------------------------------------------------------------

const GROUPS = [['phone', 'Phone'], ['foldable', 'Foldable'], ['tablet', 'Tablet']];

// platformsOf(opts) -> the platforms the server offers, [] for none.
export function platformsOf(opts) {
  return opts && Array.isArray(opts.platforms) ? opts.platforms.filter((p) => p && p.id) : [];
}

// platformById(opts, id) -> that platform, else the first one offered.
export function platformById(opts, id) {
  const all = platformsOf(opts);
  return all.find((p) => p.id === id) || all[0] || null;
}

// defaultOs(platform) -> the newest OS id. The server lists newest first, so
// that is the first; '' when nothing is installed.
export function defaultOs(platform) {
  const os = (platform && platform.os) || [];
  return os.length ? os[0].id : '';
}

// devicesFor(platform, osId) -> [{group, title, devices}] for the devices
// this OS pairs with, in Phone, Foldable, Tablet order, empty groups left
// out. Pairs are sparse (iOS 27.1 offers only a foldable), and an OS with no
// entry in `pairs` offers nothing; a server that sends no `pairs` at all
// offers every device.
export function devicesFor(platform, osId) {
  if (!platform) return [];
  const all = platform.devices || [];
  let allowed = all;
  if (platform.pairs) {
    const ids = new Set(platform.pairs[osId] || []);
    allowed = all.filter((d) => ids.has(d.id));
  }
  const known = new Set(GROUPS.map(([g]) => g));
  const groups = GROUPS.map(([group, title]) => ({ group, title, devices: allowed.filter((d) => d.group === group) }));
  groups.push({ group: 'other', title: 'Other', devices: allowed.filter((d) => !known.has(d.group)) });
  return groups.filter((g) => g.devices.length);
}

// defaultDevice(groups) -> the first phone, else the first device of the
// first group there is, else ''.
export function defaultDevice(groups) {
  const phones = groups.find((g) => g.group === 'phone');
  const g = phones || groups[0];
  return g ? g.devices[0].id : '';
}

// reconcile(platform, osId, deviceId) -> {os, device} with each kept if it is
// still valid and reset to a default if not. Changing OS can make a device
// unavailable, so the selection is settled here rather than left stale.
export function reconcile(platform, osId, deviceId) {
  const os = (platform && platform.os) || [];
  const osOk = os.some((o) => o.id === osId) ? osId : defaultOs(platform);
  const groups = devicesFor(platform, osOk);
  const stillThere = groups.some((g) => g.devices.some((d) => d.id === deviceId));
  return { os: osOk, device: stillThere ? deviceId : defaultDevice(groups) };
}

// ---------------------------------------------------------------------------
// Names (pure)
// ---------------------------------------------------------------------------

function compileRule(rule) {
  if (!rule) return null;
  try { return new RegExp(rule); } catch (_) { return null; }
}

// badRules(opts) -> the ids of platforms whose nameRule the browser cannot
// compile. Their names are then not validated here (the server still does),
// which is worth a trace record rather than a silent skip.
export function badRules(opts) {
  return platformsOf(opts).filter((p) => p.nameRule && !compileRule(p.nameRule)).map((p) => p.id);
}

// sanitizeName(name, rule) -> name with each character the rule rejects
// turned into '_' and the whole cut to the rule's length. The rule itself
// is the test, so nothing here knows Android's class from iOS's.
export function sanitizeName(name, rule) {
  const re = compileRule(rule);
  if (!re) return name;
  // Built up one character at a time and tested as the whole string so far,
  // so a rule that treats the first character differently (iOS's must start
  // with a letter or digit) and one that caps the length both hold. A
  // character the rule rejects becomes '_' if that is accepted, else it is
  // dropped.
  let out = '';
  for (const c of Array.from(name)) {
    if (re.test(out + c)) out += c;
    else if (re.test(out + '_')) out += '_';
  }
  return out || name;
}

// taken(name, existing) -> whether a device already has this name.
// existing is existingNames, {android: [...], ios: [...]}. The server refuses
// a name held on either platform (409 avdExists), without regard to case, so
// both lists are compared lowercased.
function taken(name, existing) {
  const e = existing || {};
  const lower = name.toLowerCase();
  return [...(e.android || []), ...(e.ios || [])].some((n) => String(n).toLowerCase() === lower);
}

// fit(base, suffix, re) -> base + suffix, with base cut from the end until
// the whole passes the rule, so a suffix cannot push a name past its length.
function fit(base, suffix, re) {
  const chars = Array.from(base);
  for (let k = chars.length; k > 0; k--) {
    const cand = chars.slice(0, k).join('') + suffix;
    if (!re || re.test(cand)) return cand;
  }
  return base + suffix;
}

// dedupeName(name, existing, platformId, rule) -> name, or name with _2, _3...
// (Android) or " 2", " 3"... (iOS) until it collides with nothing. With the
// platform's nameRule, the base is shortened so the suffixed name fits it.
export function dedupeName(name, existing, platformId, rule) {
  if (!taken(name, existing)) return name;
  const re = compileRule(rule);
  const sep = platformId === 'android' ? '_' : ' ';
  for (let n = 2; ; n++) {
    const next = fit(name, `${sep}${n}`, re);
    if (!taken(next, existing)) return next;
  }
}

// osVersion(platformId, os) -> the version part of an OS entry: Android's
// `android-36.1` segment of a system image id, iOS's `iOS-27-0` runtime id
// as 27.0. iOS falls back to the label's trailing number, then ''.
export function osVersion(platformId, os) {
  if (!os) return '';
  const id = String(os.id || '');
  if (platformId === 'android') {
    // The id's own segment: `36.1`, `34`, or `CANARY` for a preview image.
    // Never the label, whose trailing number is not an API level.
    const m = /android-([A-Za-z0-9][A-Za-z0-9.]*)/.exec(id);
    return m ? m[1] : '';
  } else {
    const m = /iOS-(\d+(?:-\d+)*)/.exec(id);
    if (m) return m[1].replace(/-/g, '.');
  }
  const l = /(\d+(?:\.\d+)*)\s*$/.exec(String(os.label || ''));
  return l ? l[1] : '';
}

// prefillName(opts, platformId, osId, deviceId) -> the name offered for this
// choice: `Pixel_9_API_36.1` on Android, `iPhone 17 iOS 27.0` on iOS, cut to
// the platform's name rule and then moved off any name already taken.
export function prefillName(opts, platformId, osId, deviceId) {
  const platform = platformById(opts, platformId);
  if (!platform) return '';
  const os = (platform.os || []).find((o) => o.id === osId);
  const dev = (platform.devices || []).find((d) => d.id === deviceId);
  if (!dev) return '';
  const ver = osVersion(platformId, os);
  let base;
  if (platformId === 'android') {
    base = dev.label.trim().replace(/\s+/g, '_') + (ver ? `_API_${ver}` : '');
  } else {
    base = ver ? `${dev.label} iOS ${ver}` : dev.label;
  }
  const clean = sanitizeName(base, platform.nameRule);
  return dedupeName(clean, existingFor(opts), platformId, platform.nameRule);
}

function existingFor(opts) {
  return (opts && opts.existingNames) || {};
}

// validateName(opts, platformId, name) -> an inline message, or '' when the
// name is fine to POST.
export function validateName(opts, platformId, name) {
  const platform = platformById(opts, platformId);
  const n = String(name || '');
  if (!n.trim()) return 'Enter a name';
  const re = compileRule(platform && platform.nameRule);
  if (re && !re.test(n)) {
    return platformId === 'android'
      ? 'Use letters, numbers, “.”, “_” and “-” only, up to 64'
      : 'That name has characters this platform doesn’t accept';
  }
  if (taken(n, existingFor(opts))) return `A device named “${n}” already exists`;
  return '';
}

// ---------------------------------------------------------------------------
// Job, submit and outcome (pure)
// ---------------------------------------------------------------------------

// jobView(job) -> {kind: 'none'|'creating'|'failed'|'done', ...}.
export function jobView(job) {
  if (!job || !job.state) return { kind: 'none' };
  if (job.state === 'creating') return { kind: 'creating', id: job.id, name: job.name, platform: job.platform };
  if (job.state === 'createFailed') return { kind: 'failed', id: job.id, name: job.name, error: job.error || 'The device couldn’t be created' };
  if (job.state === 'done') return { kind: 'done', id: job.id, name: job.name, createdId: job.createdId };
  return { kind: 'none' };
}

// visibleFailure(job, started, dismissed) -> the failed job to show on the
// form, or null. Only a create this page started is shown, and once: the
// server keeps a failed job until the next create, so showing every one would
// open the sheet on someone else's, or an old one, each time.
export function visibleFailure(job, started, dismissed) {
  const v = jobView(job);
  if (v.kind !== 'failed' || !v.id) return null;
  if (!started.has(v.id) || dismissed.has(v.id)) return null;
  return v;
}

// awaitListed({poll, isListed, stillHere, attempts, sleep, intervalMs})
// -> 'listed' | 'unlisted' | 'moved'. Refreshes the target list until it
// holds the new id, because one refresh can be discarded when the regular poll
// overlaps it (latestOnly) and does nothing while the page is hidden. 'moved'
// is the person having gone to another target meanwhile; 'unlisted' is the
// attempts running out, after which the caller may still hand the id on.
export async function awaitListed({ poll, isListed, stillHere, attempts = 5, intervalMs = 1000,
  sleep = (ms) => new Promise((r) => setTimeout(r, ms)) }) {
  for (let i = 0; i < attempts; i++) {
    if (!stillHere()) return 'moved';
    await poll();
    if (!stillHere()) return 'moved';
    if (isListed()) return 'listed';
    if (i < attempts - 1) await sleep(intervalMs);
  }
  return 'unlisted';
}

// submitModel(opts, sel) -> {ok, error, body}. ok is whether Create is
// enabled for this selection: an OS and a device chosen, a name that
// validates, and no create already running.
export function submitModel(opts, sel) {
  const platform = platformById(opts, sel.platform);
  if (!platform || !(platform.os || []).length) return { ok: false, error: '', body: null };
  const job = jobView(opts && opts.job);
  if (job.kind === 'creating') return { ok: false, error: '', body: null };
  if (!sel.os || !sel.device) return { ok: false, error: '', body: null };
  const error = validateName(opts, platform.id, sel.name);
  if (error) return { ok: false, error, body: null };
  return { ok: true, error: '', body: { platform: platform.id, os: sel.os, device: sel.device, name: sel.name } };
}

// emptyOsText(platformId) -> the line shown when nothing is installed.
export function emptyOsText(platformId) {
  if (platformId === 'ios') return 'No iOS runtimes installed. Add one in Xcode › Settings › Components.';
  return 'No Android system images installed. Install one in Android Studio’s SDK Manager, then reopen this sheet.';
}

// withLegacy(opts) -> {kind: 'fallback'} for a server with only the android
// CLI (`profilesFallback` and no platform), else {kind: 'options', opts}.
// `profilesFallback` beside an iOS platform means Android still creates
// through the profile list, so Android is added as a platform that opens it
// (`legacy: true`) and iOS stays a form.
export function withLegacy(opts) {
  if (!opts.profilesFallback) return { kind: 'options', opts };
  if (!platformsOf(opts).length) return { kind: 'fallback' };
  const platforms = platformsOf(opts);
  if (platforms.some((p) => p.id === 'android')) return { kind: 'options', opts };
  const android = { id: 'android', label: 'Android', legacy: true, os: [], devices: [], pairs: {} };
  return { kind: 'options', opts: { ...opts, platforms: [android, ...platforms] } };
}

// loadOptions(fetchImpl) -> {kind: 'options', opts} | {kind: 'fallback'} |
// {kind: 'none', status, body} | {kind: 'error'}. 'none' is a server that
// can create nothing (404) or refused; 'fallback' is the profile list.
export async function loadOptions(fetchImpl) {
  let res;
  try { res = await fetchImpl('/api/devices/options'); } catch (_) { return { kind: 'error' }; }
  if (!res.ok) return { kind: 'none', status: res.status, body: await res.json().catch(() => null) };
  const opts = await res.json().catch(() => null);
  if (!opts) return { kind: 'error' };
  return withLegacy(opts);
}

// canCreateFrom(status, body) -> whether the server can create anything:
// options that offer a platform, or the profile fallback.
export function canCreateFrom(status, body) {
  if (status < 200 || status >= 300 || !body) return false;
  return !!body.profilesFallback || platformsOf(body).length > 0;
}

// createOutcome(status, body) -> the trace outcome of a POST.
export function createOutcome(status, body) {
  if (status === 202) return 'ok';
  const code = body && body.code;
  if (code === 'avdExists') return 'exists';
  if (code === 'creating') return 'creating';
  return 'error';
}

// postCreate(fetchImpl, req) -> {outcome, id, message}. message is the toast
// for a refusal. A network failure is outcome 'error'.
export async function postCreate(fetchImpl, req) {
  let res;
  try {
    res = await fetchImpl('/api/devices', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(req),
    });
  } catch (_) {
    return { outcome: 'error', id: null, message: 'Couldn’t reach the server' };
  }
  const body = await res.json().catch(() => null);
  const outcome = createOutcome(res.status, body);
  if (outcome === 'ok') return { outcome, id: body && body.id, message: '' };
  return { outcome, id: null, message: createError(res.status, body) };
}

// watchCreated({fetchImpl, id, name, before, onResolved, intervalMs, maxMs,
// sleep}) -> cancel(). After an iOS create is accepted, polls the options
// until the job names the simulator it made (`createdId`), then calls
// onResolved with it. `before` is the job the options held when Create was
// pressed ({id, createdId} or null): a done job with the same name stays in
// the options until the next create, so one with that same createdId is the
// old create's and is ignored, unless the watcher has seen this create run
// (`creating`) since. It stops on a failed job, on a 401, 403 or 404, and
// after maxMs. The pending row is dropped as the real simulator appears, so
// the picker would otherwise be left holding an id that no longer exists.
export function watchCreated({ fetchImpl, id, name, before = null, onResolved, intervalMs = 2000, maxMs = 15 * 60 * 1000,
  sleep = (ms) => new Promise((r) => setTimeout(r, ms)) }) {
  let live = true;
  (async () => {
    const deadline = Date.now() + maxMs;
    let sawCreating = false;
    while (live && Date.now() < deadline) {
      await sleep(intervalMs);
      if (!live) return;
      const r = await loadOptions(fetchImpl);
      if (!live) return;
      if (r.kind === 'none' && [401, 403, 404].includes(r.status)) return;
      if (r.kind !== 'options') continue;
      const job = jobView(r.opts.job);
      if (job.kind === 'none') continue;
      if (job.id !== id && job.name !== name) continue;
      if (job.kind === 'creating') { sawCreating = true; continue; }
      if (job.kind === 'failed') return;
      if (job.kind === 'done' && job.createdId) {
        const stale = !sawCreating && before && before.id === job.id && before.createdId === job.createdId;
        if (stale) continue;
        live = false;
        onResolved(job.createdId);
        return;
      }
    }
  })();
  return () => { live = false; };
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[c]));
}

function rowHTML(row) {
  const dis = row.disabled ? 'disabled aria-disabled="true"' : '';
  return `
    <button type="button" class="pickerRow" data-profile="${escapeHTML(row.name)}" ${dis}>
      <span class="pickerTile pickerTile--android" aria-hidden="true">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
             stroke-linecap="round" stroke-linejoin="round">${ICON_PATHS[row.icon]}</svg>
      </span>
      <span class="pickerRowMain">
        <span class="pickerRowLabel">${escapeHTML(row.label)}</span>
        <span class="pickerRowDetail">${escapeHTML(row.detail)}</span>
      </span>
    </button>`;
}

function checkHTML() {
  return '<span class="pickerCheck" aria-hidden="true">&#x2713;</span>';
}

// One selectable row: an OS or a device, with a check on the chosen one.
function choiceRowHTML(attr, id, label, detail, selected, disabled) {
  return `
    <button type="button" class="pickerRow" ${attr}="${escapeHTML(id)}" role="radio" aria-checked="${selected ? 'true' : 'false'}"${disabled ? ' disabled' : ''}>
      <span class="pickerRowMain">
        <span class="pickerRowLabel">${escapeHTML(label)}</span>
        ${detail ? `<span class="pickerRowDetail">${escapeHTML(detail)}</span>` : ''}
      </span>
      ${selected ? checkHTML() : ''}
    </button>`;
}

// mount(root, {fetchImpl, onCreated, onResolved, onClose, toast, report})
// -> {open, close, isOpen}.
// toast is the #toast element, as every module that shows one takes it.
// onCreated(id, {platform}) runs after a 202; onResolved(id) when an iOS
// create has produced its real simulator; onClose() after any close, so the
// caller can put focus back on the picker underneath.
export function mount(root, { fetchImpl = fetch, onCreated, onResolved, onClose, toast, report } = {}) {
  root.setAttribute('role', 'dialog');
  root.setAttribute('aria-modal', 'true');
  root.setAttribute('aria-labelledby', 'newDeviceTitle');
  root.setAttribute('tabindex', '-1');
  root.innerHTML = `
    <div class="pickerTop">
      <div class="sheetHandle" aria-hidden="true"><div class="sheetGrip"></div></div>
      <div class="pickerHeader">
        <button type="button" class="pickerDone pickerCancel">Cancel</button>
        <h2 id="newDeviceTitle" class="pickerTitle">New device</h2>
        <span></span>
      </div>
    </div>
    <div class="pickerList" id="newDeviceBody"></div>`;
  const body = root.querySelector('#newDeviceBody');
  root.querySelector('.pickerCancel').addEventListener('click', () => close());

  let open_ = false;
  let busy = false;
  let opts = null;
  let sel = null; // {platform, os, device, name, edited}
  const started = new Set();   // job ids this page's creates returned
  const dismissed = new Set(); // failed jobs already shown and put away
  let cancelWatch = null;
  let refreshTimer = null;

  // On window, capture phase: that runs before the picker's own Escape on
  // document, so Escape closes this sheet and leaves the picker open.
  function onKeydown(ev) {
    if (ev.key === 'Escape') { ev.stopPropagation(); ev.preventDefault(); close(); }
  }

  function say(html) { body.innerHTML = html; }
  function empty(text) { return `<div class="pickerEmpty">${escapeHTML(text)}</div>`; }

  async function load() {
    say(empty('Loading…'));
    const r = await loadOptions(fetchImpl);
    if (!open_) return;
    if (r.kind === 'fallback') {
      if (report) report({ event: 'newdevice-open', platforms: '' });
      return loadProfiles(body);
    }
    if (r.kind === 'none') {
      if (report) report({ event: 'newdevice-open', platforms: '', error: String(r.status) });
      return say(empty(createError(r.status, r.body)));
    }
    if (r.kind === 'error') {
      if (report) report({ event: 'newdevice-open', platforms: '', error: 'network' });
      return say(empty('Couldn’t reach the server.'));
    }
    opts = r.opts;
    const platform = platformById(opts, null);
    sel = { platform: platform ? platform.id : '', os: '', device: '', name: '', edited: false };
    choose({ platform: sel.platform });
    if (report) report({ event: 'newdevice-open', platforms: platformsOf(opts).map((p) => p.id).join(',') });
    const bad = badRules(opts);
    if (bad.length && report) report({ event: 'newdevice-error', reason: 'nameRule', platforms: bad.join(',') });
    watchJob();
  }

  // While a create is running the sheet's Create is disabled, and it would
  // stay so if that create finished with the sheet open. So refresh the
  // options every 2s while the job is creating (at most 15 minutes), and
  // draw again when its state changes. Stopped by close().
  function watchJob() {
    clearTimeout(refreshTimer);
    refreshTimer = null;
    if (!open_ || !opts || jobView(opts.job).kind !== 'creating') return;
    let left = 450;
    const tick = async () => {
      refreshTimer = null;
      if (!open_ || --left < 0) return;
      const r = await loadOptions(fetchImpl);
      if (!open_) return;
      if (r.kind === 'options') {
        const was = jobView(opts.job).kind;
        opts = r.opts;
        if (jobView(opts.job).kind !== was) { choose({}); return; }
      }
      refreshTimer = setTimeout(tick, 2000);
    };
    refreshTimer = setTimeout(tick, 2000);
  }

  // choose settles a change of platform, OS or device: the other two follow
  // (a device the new OS lacks is replaced), and the name is re-offered
  // unless the person has typed their own.
  function choose(change) {
    const platformChanged = change.platform && change.platform !== sel.platform;
    if (change.platform) sel.platform = change.platform;
    const platform = platformById(opts, sel.platform);
    const os = platformChanged ? '' : (change.os || sel.os);
    const dev = platformChanged ? '' : (change.device || sel.device);
    const r = reconcile(platform, os, dev);
    sel.os = r.os;
    sel.device = r.device;
    if (platformChanged) sel.edited = false;
    if (!sel.edited) sel.name = prefillName(opts, sel.platform, sel.os, sel.device);
    render();
  }

  function render() {
    const platform = platformById(opts, sel.platform);
    const job = jobView(opts.job);
    const parts = [];

    const failure = visibleFailure(opts.job, started, dismissed);
    if (failure) {
      parts.push(`
        <div class="pickerGroup">
          <div class="pickerGroupTitle">Couldn’t create${failure.name ? ` ${escapeHTML(failure.name)}` : ''}</div>
          <div class="pickerGroupRows"><div class="pickerEmpty" id="newDeviceFailure">${escapeHTML(failure.error)}</div></div>
        </div>
        <button type="button" class="ovButton ovButton--primary" id="newDeviceBack">Back to the form</button>`);
      say(parts.join(''));
      body.querySelector('#newDeviceBack').addEventListener('click', () => { dismissed.add(failure.id); render(); });
      return;
    }

    const creating = job.kind === 'creating';
    if (creating) {
      parts.push(`
        <div class="pickerGroup">
          <div class="pickerGroupRows"><div class="pickerEmpty" id="newDeviceProgress">Creating ${escapeHTML(job.name || 'device')}…</div></div>
        </div>`);
    }

    const platforms = platformsOf(opts);
    parts.push('<div class="pickerGroup"><div class="pickerGroupTitle">Platform</div>');
    if (platforms.length > 1) {
      parts.push(`<div class="settingSegments settingSegments--wide" role="radiogroup" aria-label="Platform" id="newDevicePlatform">${platforms.map((p) =>
        `<button type="button" class="settingSegment" role="radio" data-platform="${escapeHTML(p.id)}" aria-checked="${p.id === sel.platform ? 'true' : 'false'}"${creating ? ' disabled' : ''}>${escapeHTML(p.label || p.id)}</button>`).join('')}</div>`);
    } else {
      parts.push(`<div class="pickerGroupRows"><div class="pickerRow"><span class="pickerRowLabel">${escapeHTML(platform ? (platform.label || platform.id) : '')}</span></div></div>`);
    }
    parts.push('</div>');

    if (platform && platform.legacy) {
      parts.push('<div id="newDeviceLegacy"></div>');
      say(parts.join(''));
      wire(creating);
      loadProfiles(body.querySelector('#newDeviceLegacy'));
      return;
    }
    if (!platform || !(platform.os || []).length) {
      parts.push(`<div class="pickerGroup"><div class="pickerGroupRows">${empty(emptyOsText(platform && platform.id))}</div></div>`);
    } else {
      parts.push(`<div class="pickerGroup"><div class="pickerGroupTitle">OS</div><div class="pickerGroupRows" role="radiogroup" aria-label="OS">${
        platform.os.map((o) => choiceRowHTML('data-os', o.id, o.label || o.id, o.detail, o.id === sel.os, creating)).join('')}</div></div>`);
      for (const g of devicesFor(platform, sel.os)) {
        parts.push(`<div class="pickerGroup"><div class="pickerGroupTitle">${escapeHTML(g.title)}</div><div class="pickerGroupRows" role="radiogroup" aria-label="${escapeHTML(g.title)}">${
          g.devices.map((d) => choiceRowHTML('data-device', d.id, d.label || d.id, d.size, d.id === sel.device, creating)).join('')}</div></div>`);
      }
      parts.push(`
        <form class="switchForm" id="newDeviceForm" autocomplete="off">
          <label class="pickerGroupTitle" for="newDeviceName">Name</label>
          <input id="newDeviceName" class="switchInput" type="text"
                 autocapitalize="off" autocorrect="off" spellcheck="false" value="${escapeHTML(sel.name)}"${creating ? ' disabled' : ''}>
          <p class="switchError" id="newDeviceNameError" hidden></p>
          <button type="submit" class="ovButton ovButton--primary" id="newDeviceCreate">Create</button>
        </form>`);
    }
    say(parts.join(''));
    wire(creating);
  }

  function wire(creating) {
    for (const btn of body.querySelectorAll('[data-platform]')) {
      btn.addEventListener('click', () => { if (!creating) choose({ platform: btn.getAttribute('data-platform') }); });
    }
    for (const btn of body.querySelectorAll('[data-os]')) {
      btn.addEventListener('click', () => { if (!creating) choose({ os: btn.getAttribute('data-os') }); });
    }
    for (const btn of body.querySelectorAll('[data-device]')) {
      btn.addEventListener('click', () => { if (!creating) choose({ device: btn.getAttribute('data-device') }); });
    }
    const input = body.querySelector('#newDeviceName');
    const form = body.querySelector('#newDeviceForm');
    if (!input || !form) return;
    input.addEventListener('input', () => { sel.name = input.value; sel.edited = true; syncSubmit(); });
    form.addEventListener('submit', (ev) => { ev.preventDefault(); create(); });
    syncSubmit();
  }

  // syncSubmit updates the Create button and the inline name error without
  // re-rendering, which would drop focus from the field being typed in.
  function syncSubmit() {
    const m = submitModel(opts, sel);
    const btn = body.querySelector('#newDeviceCreate');
    const err = body.querySelector('#newDeviceNameError');
    if (btn) btn.disabled = !m.ok;
    if (err) { err.textContent = m.error; err.hidden = !m.error; }
  }

  async function create() {
    const m = submitModel(opts, sel);
    if (!m.ok || busy) return;
    busy = true;
    const btn = body.querySelector('#newDeviceCreate');
    if (btn) btn.disabled = true;
    try {
      const r = await postCreate(fetchImpl, m.body);
      if (report) report({ event: 'newdevice-create', platform: m.body.platform, os: m.body.os, device: m.body.device, nameLength: m.body.name.length, outcome: r.outcome });
      if (r.outcome === 'ok') {
        const { platform, name } = m.body;
        const prev = jobView(opts && opts.job);
        const before = prev.kind === 'none' ? null : { id: prev.id, createdId: prev.createdId };
        if (r.id) { started.add(r.id); dismissed.delete(r.id); }
        close();
        if (platform === 'ios' && onResolved) {
          if (cancelWatch) cancelWatch();
          cancelWatch = watchCreated({ fetchImpl, id: r.id, name, before, onResolved });
        }
        if (onCreated) onCreated(r.id, { platform });
        return;
      }
      if (toast) showToast(toast, r.message);
      // A refusal for a job already running: reload so the form shows it.
      if (r.outcome === 'creating' && open_) load();
    } finally {
      busy = false;
      syncSubmit();
    }
  }

  // The profile list, for a server with only the android CLI. It is the
  // sheet as it was before the form: tap a profile, and that is the create.
  async function loadProfiles(host) {
    host.innerHTML = (`
      <div class="pickerGroup">
        <div class="pickerGroupTitle">Emulator on this Mac</div>
        <div class="pickerGroupRows" id="newDeviceRows">${empty('Loading…')}</div>
      </div>
      <p class="pickerFootnote">A profile whose system image is not on this Mac downloads it first: 2.3 GB for a phone, about four minutes. Each profile makes one device, named after it.</p>`);
    const rows = host.querySelector('#newDeviceRows');
    let profiles = null;
    try {
      const res = await fetchImpl('/api/avds/profiles');
      if (res.ok) profiles = (await res.json()).profiles;
      else rows.innerHTML = empty(createError(res.status, await res.json().catch(() => null)));
    } catch (_) {
      rows.innerHTML = empty('Couldn’t reach the server.');
    }
    if (!profiles || !open_) return;
    rows.innerHTML = profiles.length
      ? profiles.map(profileRowModel).map(rowHTML).join('')
      : empty('The android CLI lists no profiles.');
    for (const btn of rows.querySelectorAll('[data-profile]')) {
      btn.addEventListener('click', () => { if (!btn.disabled) createProfile(btn.getAttribute('data-profile')); });
    }
  }

  async function createProfile(profile) {
    if (busy) return;
    busy = true;
    try {
      const res = await fetchImpl('/api/avds', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ profile }),
      });
      const resBody = await res.json().catch(() => null);
      if (res.status === 202) {
        if (report) report({ event: 'avd-create', profile, outcome: 'accepted' });
        if (resBody && resBody.id) { started.add(resBody.id); dismissed.delete(resBody.id); }
        close();
        if (onCreated) onCreated(resBody && resBody.id, { platform: 'android' });
        return;
      }
      const msg = createError(res.status, resBody);
      if (report) report({ event: 'avd-create', profile, outcome: (resBody && resBody.code) || String(res.status) });
      if (toast) showToast(toast, msg);
    } catch (_) {
      if (toast) showToast(toast, 'Couldn’t reach the server');
    } finally {
      busy = false;
    }
  }

  function open() {
    if (open_) return;
    open_ = true;
    root.hidden = false;
    window.addEventListener('keydown', onKeydown, true);
    root.focus();
    load();
  }

  function close() {
    if (!open_) return;
    open_ = false;
    clearTimeout(refreshTimer);
    refreshTimer = null;
    root.hidden = true;
    window.removeEventListener('keydown', onKeydown, true);
    if (onClose) onClose();
  }

  return { open, close, isOpen: () => open_ };
}
