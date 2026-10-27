// Stream group in the Tools sheet: Max size, Bitrate and Frame rate, read
// from GET /api/config and written with one PUT per pick. Each row pushes a
// picker page with the current value checked.
//
// What /api/config reports is what this shows, not what was last picked: the
// server's `value` is what it is using, and a `pinned` setting is held by a
// command-line flag, so a write would persist and change nothing. Such a row
// reads "set by a flag" and cannot be opened rather than claiming a change
// that did not happen.

import { showToast } from './toast.js';

export const SETTINGS = [
  { name: 'maxSize', label: 'Max size', options: [0, 720, 1080, 1440, 1920] },
  { name: 'bitRate', label: 'Bitrate', options: [500000, 1000000, 2000000, 4000000, 8000000, 16000000] },
  { name: 'maxFps', label: 'Frame rate', options: [30, 60] },
];

export const FOOTER = 'Changes apply to the next session.';

export function formatValue(name, v) {
  if (typeof v !== 'number' || !Number.isFinite(v)) return '';
  if (name === 'maxSize') return v === 0 ? 'Native' : `${v}`;
  if (name === 'bitRate') return v >= 1000000 ? `${+(v / 1000000).toFixed(1)} Mbps` : `${Math.round(v / 1000)} kbps`;
  if (name === 'maxFps') return `${v} fps`;
  return `${v}`;
}

// optionsFor(setting, current) -> the picker's values, ascending, with the
// current value added when it is not one of the presets: a value set in the
// config file by hand still shows as checked rather than as nothing.
export function optionsFor(setting, current) {
  const out = [...setting.options];
  if (typeof current === 'number' && !out.includes(current)) out.push(current);
  return out.sort((a, b) => a - b);
}

// fieldsByName(resp) -> {name: field} from a GET or PUT response.
export function fieldsByName(resp) {
  const out = {};
  for (const f of (resp && resp.fields) || []) out[f.name] = f;
  return out;
}

// putOutcome(status, resp, name) -> 'ok' | 'pinned' | 'error', for the
// config-put report and for what the row says afterwards.
export function putOutcome(status, resp, name) {
  if (status < 200 || status >= 300) return 'error';
  if (resp && Array.isArray(resp.pinned) && resp.pinned.includes(name)) return 'pinned';
  return 'ok';
}

// mount(sheet, deps) -> {refresh()}
//   sheet  -- sheet.js's handle: sections, addLink, addPage, popPage, setFooter.
//   deps.fetch  -- window.fetch, injectable for tests.
//   deps.report -- client telemetry.
//   deps.toast  -- optional #toast element, for a failed write.
export function mount(sheet, deps) {
  const doFetch = deps.fetch || ((...a) => fetch(...a));
  const report = deps.report || (() => {});
  let fields = {};
  const rows = {};

  sheet.setFooter('stream', FOOTER);

  for (const setting of SETTINGS) {
    const pageId = `stream-${setting.name}`;
    const page = sheet.addPage(pageId, setting.label);
    const detail = sheet.addLink('stream', setting.label, pageId);
    const row = detail.parentElement;
    row.hidden = true;
    rows[setting.name] = { setting, page, detail, row };
  }

  function paintPicker(r) {
    const f = fields[r.setting.name];
    r.page.replaceChildren();
    if (!f) return;
    for (const v of optionsFor(r.setting, f.value)) {
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'sheetCheckRow';
      btn.setAttribute('role', 'menuitemradio');
      btn.setAttribute('aria-checked', v === f.value ? 'true' : 'false');
      btn.textContent = formatValue(r.setting.name, v);
      btn.addEventListener('click', () => pick(r, v));
      r.page.appendChild(btn);
    }
  }

  function paint() {
    for (const r of Object.values(rows)) {
      const f = fields[r.setting.name];
      r.row.hidden = !f;
      if (!f) continue;
      r.row.disabled = !!f.pinned;
      r.detail.textContent = f.pinned ? 'set by a flag' : formatValue(r.setting.name, f.value);
      paintPicker(r);
    }
  }

  async function pick(r, v) {
    const name = r.setting.name;
    sheet.popPage();
    if (fields[name] && fields[name].value === v) return;
    let status = 0;
    let resp = null;
    try {
      const res = await doFetch('/api/config', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ [name]: v }),
      });
      status = res.status;
      resp = await res.json().catch(() => null);
    } catch (_) {}
    const outcome = putOutcome(status, resp, name);
    report({ event: 'config-put', setting: name, value: v, outcome });
    if (outcome === 'error') {
      if (deps.toast) showToast(deps.toast, `Could not change ${r.setting.label.toLowerCase()}`);
      return;
    }
    fields = { ...fields, ...fieldsByName(resp) };
    paint();
  }

  async function refresh() {
    try {
      const res = await doFetch('/api/config');
      if (!res.ok) return;
      fields = fieldsByName(await res.json());
      paint();
    } catch (_) {}
  }

  return { refresh };
}
