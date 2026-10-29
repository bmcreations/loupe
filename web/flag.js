// Flag for agent: a person watching a device tells an AI agent "something is
// off on this screen" (POST /api/flags, internal/httpapi/flags.go; the Mac
// app's FlagViews.swift is the model for the interaction).
//
// Flag mode is per pane. While it is on, #flagLayer (the pane's flag layer,
// same slot as the inspect layer) sits over the picture and takes pointer
// events instead of the device, so a mouse or finger drag draws a red box
// rather than reaching Android. The box is kept as fractions of the picture
// [left, top, right, bottom], the form the server wants, so it is independent
// of zoom, letterboxing and the bezel: fractions are taken against the
// canvas's own on-screen rect (deps.pictureRect), which already includes any
// zoom transform, and the box is drawn as percentages of that same rect.
//
// A panel at the foot of the pane holds the note, the watch checkbox and the
// buttons. With watch, a successful send leaves flag mode (touches go to the
// device again) and a pill at the top of the pane stays until Stop posts
// /api/flags/end.
//
// Split the way record.js is: pure functions, then mount() for the DOM half.
//
// toast.js is imported here rather than passed in (trap 45).

import { showToast } from './toast.js';

// MAX_NOTE is the server's limit, in characters (maxFlagNote).
export const MAX_NOTE = 2000;
// SLOP is how far a press may move and still be a click, which clears the box.
export const SLOP = 4;

const clamp01 = (v) => Math.min(1, Math.max(0, v));

// fractionAt(rect, x, y) -> [fx, fy], the client point as fractions of the
// picture rect {left, top, width, height}, held to 0..1 so a drag that leaves
// the picture sticks to its edge.
export function fractionAt(rect, x, y) {
  if (!rect || !(rect.width > 0) || !(rect.height > 0)) return [0, 0];
  return [clamp01((x - rect.left) / rect.width), clamp01((y - rect.top) / rect.height)];
}

// regionOf(a, b) -> [left, top, right, bottom] from two fraction points, or
// null when the box has no area (the server wants left < right, top < bottom).
export function regionOf(a, b) {
  const left = Math.min(a[0], b[0]);
  const right = Math.max(a[0], b[0]);
  const top = Math.min(a[1], b[1]);
  const bottom = Math.max(a[1], b[1]);
  if (!(left < right) || !(top < bottom)) return null;
  return [left, top, right, bottom];
}

// requestBody({note, region, watch}) -> the POST /api/flags body. The note is
// trimmed and cut to the server's limit, and an empty note, no region and no
// watch are left out, as the server treats absent as "not given".
export function requestBody({ note = '', region = null, watch = false } = {}) {
  const body = {};
  const n = String(note).trim();
  if (n) body.note = Array.from(n).slice(0, MAX_NOTE).join('');
  if (region) body.region = region.slice();
  if (watch) body.watch = true;
  return body;
}

// flagShortcut(e) -> true for Alt+Meta+F (Mac) and Alt+Ctrl+F elsewhere.
// Read by key position, since Alt changes the character F types.
export function flagShortcut(e) {
  if (!e || !e.altKey || e.shiftKey) return false;
  if (!(e.metaKey || e.ctrlKey)) return false;
  return e.code === 'KeyF';
}

// errorText(res) -> the server's own `error` string, or a status line.
async function errorText(res) {
  try {
    const j = await res.json();
    if (j && typeof j.error === 'string' && j.error) return j.error;
  } catch { /* not JSON */ }
  return `${res.status || 'failed'}`;
}

// mount(layer, deps) -> { run(), toggle(), off(), on(), watching(), relayout(),
//   onTargetSwitch() }
//
// deps:
//   doc -- the document.
//   host -- the pane's root; the panel and the pill go in it.
//   targetId() -- the pane's target.
//   pictureRect() -- the canvas's on-screen rect.
//   fetch(url, init) -- window.fetch.
//   headers() -- the pane's viewer headers, so the server counts the request
//     as the person's.
//   beforeEnter() -- optional; the page leaves zoom first.
//   focusNote() -- optional; whether to focus the note on entering. A phone's
//     keyboard would cover the picture the person is about to box, so the page
//     says no on a coarse pointer.
//   onToggle(on) -- optional.
//   toast -- the shared #toast element.
//   report(obj) -- client telemetry.
export function mount(layer, deps) {
  const doc = deps.doc;
  const report = deps.report || (() => {});
  const mk = (tag, cls, text) => {
    const e = doc.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  };

  // Layer: the picture-sized clip, the box with its dimming shadow, the hint.
  const pic = mk('div', 'flagPic');
  const box = mk('div', 'flagBox');
  box.hidden = true;
  pic.appendChild(box);
  const hint = mk('div', 'flagHint', "Drag to box what's off");
  layer.append(pic, hint);
  layer.hidden = true;

  // Panel.
  const panel = mk('div', 'flagPanel');
  panel.setAttribute('role', 'dialog');
  panel.setAttribute('aria-label', 'Flag for agent');
  panel.hidden = true;
  const title = mk('div', 'flagTitle', 'Flag for agent');
  const note = mk('textarea', 'flagNote');
  note.rows = 2;
  note.maxLength = MAX_NOTE;
  note.setAttribute('placeholder', 'What looks off? (optional)');
  note.setAttribute('aria-label', 'Note for the agent');
  const watchRow = mk('label', 'flagWatchRow');
  const watchBox = mk('input');
  watchBox.type = 'checkbox';
  watchRow.append(watchBox, mk('span', null, 'Watch while I reproduce it'));
  const errLine = mk('p', 'flagError');
  errLine.setAttribute('role', 'alert');
  errLine.hidden = true;
  const clearBtn = mk('button', 'flagBtn', 'Clear Box');
  clearBtn.type = 'button';
  clearBtn.hidden = true;
  const cancelBtn = mk('button', 'flagBtn', 'Cancel');
  cancelBtn.type = 'button';
  const sendBtn = mk('button', 'flagBtn flagSend', 'Send');
  sendBtn.type = 'button';
  const spacer = mk('span', 'flagSpacer');
  const actions = mk('div', 'flagActions');
  actions.append(clearBtn, spacer, cancelBtn, sendBtn);
  panel.append(title, note, watchRow, errLine, actions);

  // Pill while a watch runs.
  const pill = mk('div', 'flagPill');
  pill.setAttribute('role', 'status');
  pill.hidden = true;
  const stopBtn = mk('button', 'flagStop', 'Stop');
  stopBtn.type = 'button';
  pill.append(mk('span', 'flagDot'), mk('span', 'flagPillText', 'Agent is watching your input'), stopBtn);

  (deps.host || doc.body).append(panel, pill);

  let isOn = false;
  let region = null;      // [l, t, r, b] fractions, or null
  let preview = null;     // the box while a drag is in progress
  let drag = null;        // {id, x, y, start}
  let sending = false;
  let watchId = null;     // the open flag's id while a watch runs
  let gen = 0;            // bumps on exit, so a send that lands late is dropped

  function shown() { return preview || region; }

  // paint draws the box and the hint, and the panel's buttons. The box is
  // placed as percentages of the layer, so a zoom transform on the wrap, which
  // scales both, changes nothing here.
  function paint() {
    const lr = layer.getBoundingClientRect();
    const pr = deps.pictureRect();
    if (lr && pr && lr.width > 0 && lr.height > 0) {
      pic.style.left = `${((pr.left - lr.left) / lr.width) * 100}%`;
      pic.style.top = `${((pr.top - lr.top) / lr.height) * 100}%`;
      pic.style.width = `${(pr.width / lr.width) * 100}%`;
      pic.style.height = `${(pr.height / lr.height) * 100}%`;
    }
    const r = shown();
    box.hidden = !r;
    if (r) {
      box.style.left = `${r[0] * 100}%`;
      box.style.top = `${r[1] * 100}%`;
      box.style.width = `${(r[2] - r[0]) * 100}%`;
      box.style.height = `${(r[3] - r[1]) * 100}%`;
    }
    hint.hidden = !!r;
    clearBtn.hidden = !region;
    sendBtn.textContent = watchBox.checked ? 'Send and Watch' : 'Send';
    sendBtn.disabled = sending;
  }

  function setError(text) {
    errLine.textContent = text || '';
    errLine.hidden = !text;
  }

  function enter() {
    if (isOn || !deps.targetId()) return;
    if (deps.beforeEnter) deps.beforeEnter();
    isOn = true;
    region = null;
    preview = null;
    drag = null;
    sending = false;
    note.value = '';
    watchBox.checked = false;
    setError('');
    layer.hidden = false;
    panel.hidden = false;
    paint();
    if (!deps.focusNote || deps.focusNote()) note.focus();
    if (deps.onToggle) deps.onToggle(true);
    report({ event: 'flag-open' });
  }

  function exit() {
    if (!isOn) return;
    isOn = false;
    gen++;
    drag = null;
    preview = null;
    region = null;
    sending = false;
    layer.hidden = true;
    panel.hidden = true;
    if (deps.onToggle) deps.onToggle(false);
  }

  async function send() {
    if (!isOn || sending) return;
    const watch = watchBox.checked;
    const body = requestBody({ note: note.value, region, watch });
    const mine = gen;
    sending = true;
    setError('');
    paint();
    try {
      const res = await deps.fetch(`/api/flags?target=${encodeURIComponent(deps.targetId() || '')}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', ...(deps.headers ? deps.headers() : {}) },
        body: JSON.stringify(body),
      });
      if (mine !== gen) return;
      if (!res.ok) throw new Error(await errorText(res));
      const flag = await res.json();
      if (mine !== gen) return;
      report({ event: 'flag-sent', watch, region: !!region, note: !!body.note });
      const watching = watch && flag && flag.watching !== false && flag.id != null;
      exit();
      if (watching) {
        watchId = flag.id;
        pill.hidden = false;
      } else {
        showToast(deps.toast, 'Flag sent');
      }
    } catch (err) {
      if (mine !== gen) return;
      sending = false;
      setError(`Could not send: ${err && err.message ? err.message : err}`);
      paint();
    }
  }

  async function stop() {
    if (watchId == null) return;
    const id = watchId;
    stopBtn.disabled = true;
    try {
      const res = await deps.fetch(`/api/flags/end?id=${encodeURIComponent(id)}`, {
        method: 'POST',
        headers: deps.headers ? deps.headers() : {},
      });
      if (!res.ok) throw new Error(await errorText(res));
      watchId = null;
      pill.hidden = true;
      showToast(deps.toast, 'Flag sent');
      report({ event: 'flag-end' });
    } catch (err) {
      showToast(deps.toast, `Could not stop: ${err && err.message ? err.message : err}`);
    } finally {
      stopBtn.disabled = false;
    }
  }

  // Pointer drags. The layer is above the canvas, so none of this reaches the
  // device. A press that moves less than SLOP is a click and clears the box.
  layer.addEventListener('pointerdown', (ev) => {
    if (!isOn || drag) return;
    if (ev.button != null && ev.button > 0) return;
    if (ev.preventDefault) ev.preventDefault();
    try { if (layer.setPointerCapture) layer.setPointerCapture(ev.pointerId); } catch { /* not capturable */ }
    drag = {
      id: ev.pointerId,
      x: ev.clientX,
      y: ev.clientY,
      start: fractionAt(deps.pictureRect(), ev.clientX, ev.clientY),
      moved: false,
    };
  });
  layer.addEventListener('pointermove', (ev) => {
    if (!drag || ev.pointerId !== drag.id) return;
    if (!drag.moved && Math.hypot(ev.clientX - drag.x, ev.clientY - drag.y) < SLOP) return;
    drag.moved = true;
    preview = regionOf(drag.start, fractionAt(deps.pictureRect(), ev.clientX, ev.clientY));
    paint();
  });
  layer.addEventListener('pointerup', (ev) => {
    if (!drag || ev.pointerId !== drag.id) return;
    const d = drag;
    drag = null;
    preview = null;
    region = d.moved ? regionOf(d.start, fractionAt(deps.pictureRect(), ev.clientX, ev.clientY)) : null;
    paint();
  });
  layer.addEventListener('pointercancel', (ev) => {
    if (!drag || ev.pointerId !== drag.id) return;
    drag = null;
    preview = null;
    paint();
  });

  clearBtn.addEventListener('click', () => { region = null; paint(); });
  cancelBtn.addEventListener('click', exit);
  sendBtn.addEventListener('click', send);
  stopBtn.addEventListener('click', stop);
  watchBox.addEventListener('change', paint);
  note.addEventListener('keydown', (ev) => {
    if (ev.key === 'Enter' && !ev.shiftKey && !ev.isComposing) {
      if (ev.preventDefault) ev.preventDefault();
      send();
    }
  });
  doc.addEventListener('keydown', (ev) => {
    if (isOn && ev.key === 'Escape') exit();
  });

  return {
    on: () => isOn,
    watching: () => watchId != null,
    run: enter,
    toggle: () => { if (isOn) exit(); else enter(); },
    off: exit,
    relayout: () => { if (isOn) paint(); },
    // A target switch ends the pane's flag mode and, best effort, its watch:
    // the watch was of the old target's input.
    onTargetSwitch() {
      exit();
      if (watchId != null) stop();
    },
  };
}
