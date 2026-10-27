// Record: start and stop a server-side recording of the current target, the
// pill's red dot and elapsed time, and the banner that shares the file.
//
// Safari allows navigator.share only inside a tap, and the file arrives
// after an await, so stopping fetches the file and the banner's own tap
// shares it.
//
// The recording is the server's (/api/record); this page only remembers the
// id its own 201 returned, in sessionStorage so a reload keeps it. An
// agent's or another viewer's recording on the same target is never this
// page's, so a reload re-adopts, and a target switch stops, only that id.
//
// Split the way clipboard.js is: pure functions, plus mount() for the DOM
// half (no harness, trap 21).
//
// toast.js is imported here rather than passed in: app.js importing it
// would stamp its specifier there and not in the other modules that import
// it, and the page would hold two toast timers (trap 45).

import { showToast } from './toast.js';

// POLL_MS is how often a running recording is asked whether it ended on its
// own: the limit, a rotation, the session ending, or the disk.
export const POLL_MS = 2000;

// elapsed(ms) -> "m:ss", seconds floored.
export function elapsed(ms) {
  const s = Math.max(0, Math.floor(ms / 1000));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`;
}

const END_MESSAGES = {
  stopped: 'Recording ready',
  limit: 'Recording reached its limit',
  resized: 'Stopped when the screen rotated',
  fellBehind: 'Stopped: the disk fell behind',
  sessionEnded: 'Stopped when the stream ended',
  writeError: 'Stopped: could not write the file',
};

// endMessage(reason, frames) -> the banner's text for the server's
// Meta.reason. frames is Meta.frames: 0 means the file holds nothing to share.
export function endMessage(reason, frames) {
  if (frames === 0) return 'Nothing was recorded';
  return Object.hasOwn(END_MESSAGES, reason) ? END_MESSAGES[reason] : 'Recording stopped';
}

// progress(got, total) -> the banner while the file downloads. total is
// Content-Length, 0 when the response did not carry one.
export function progress(got, total) {
  if (!total) return 'Preparing…';
  const mb = (n) => Math.round(n / 1e6);
  return `Preparing… ${mb(got)} of ${mb(total)} MB`;
}

const STORE_KEY = 'loupe.recordId';
const storeKey = (target) => `${STORE_KEY}.${target}`;

// adoptable(list, target, storedId) -> the in-progress recording on target
// whose id is the one this tab stored, or null. Without a stored id nothing
// is this tab's.
export function adoptable(list, target, storedId) {
  if (!storedId || !Array.isArray(list)) return null;
  for (const m of list) {
    if (m && m.inProgress && m.target === target && m.id === storedId) return m;
  }
  return null;
}

// mount(deps) -> { toggle(), recording(), onTargetSwitch(id), sync(), label(), onChange(fn), repaint() }
//
// deps:
//   doc -- the document.
//   host -- the element the banner goes in; doc.body by default.
//   fetch(url, init) -- window.fetch.
//   targetId() -- the target in front.
//   pillRec -- the pill's #pillRec span: red dot and elapsed time. A tap on
//     it stops the recording.
//   isFront() -- optional; whether this pane is the one the pill names.
//     Every pane records on its own, and only the focused one paints the
//     pill or answers its tap. repaint() is called when focus moves.
//   report(obj) -- client telemetry.
//   toast -- the shared #toast element.
//   now() -- optional clock in ms, performance.now by default.
//   wall() -- optional wall clock in ms, Date.now by default.
//
// onChange(fn) registers fn, called each time recording() flips, so the
// More menu relabels its item.
export function mount(deps) {
  const doc = deps.doc;
  const win = doc.defaultView;
  const now = deps.now || (() => win.performance.now());
  const wall = deps.wall || (() => Date.now());
  const pillRec = deps.pillRec;
  const recTime = pillRec && pillRec.querySelector('.recTime');
  const isFront = deps.isFront || (() => true);

  // Banner: the message, then Share and Save. Tapping the message dismisses.
  const banner = doc.createElement('div');
  banner.className = 'recBanner';
  banner.hidden = true;
  const msg = doc.createElement('span');
  msg.className = 'recMsg';
  const shareBtn = doc.createElement('button');
  shareBtn.type = 'button';
  shareBtn.textContent = 'Share';
  const saveLink = doc.createElement('a');
  saveLink.textContent = 'Save';
  saveLink.setAttribute('role', 'button');
  banner.append(msg, shareBtn, saveLink);
  (deps.host || doc.body).appendChild(banner);

  let rec = null;      // {id, target, started} while this page's recording runs
  let busy = false;    // a start or a stop is in flight
  let file = null;     // the finished File the banner offers
  let fileURL = '';
  let tickTimer = 0;
  let pollTimer = 0;
  let gen = 0;         // bumps on each new download, so a stale one stops painting
  const listeners = [];

  // The id of the recording this tab started, kept across a reload.
  // Keyed by target. sync passes deps.targetId() read at that moment; run and
  // settle pass the recording's own target, since a switch changes the front one.
  // The old unkeyed key is adopted by the first read, then removed.
  function storedId(target) {
    try {
      const store = win.sessionStorage;
      const key = storeKey(target);
      const own = store.getItem(key);
      if (own) return own;
      const legacy = store.getItem(STORE_KEY);
      if (!legacy) return '';
      store.removeItem(STORE_KEY);
      store.setItem(key, legacy);
      return legacy;
    } catch (e) { return ''; }
  }
  function storeId(target, id) {
    try { win.sessionStorage.setItem(storeKey(target), id); } catch (e) {}
  }
  function clearId(target) {
    try { win.sessionStorage.removeItem(storeKey(target)); } catch (e) {}
  }

  function changed() {
    for (const fn of listeners) {
      try { fn(); } catch (e) {}
    }
  }

  function fail(action, err) {
    const m = String((err && err.message) || err);
    deps.report({ event: 'record-error', action, err: m });
    showToast(deps.toast, `Record: ${m}`, 'more');
  }

  async function errorOf(res) {
    let body = '';
    try { body = (await res.text()).trim(); } catch (e) {}
    let parsed = null;
    try { parsed = JSON.parse(body); } catch (e) {}
    const err = new Error(`${res.status} ${(parsed && parsed.error) || body.slice(0, 120)}`);
    err.code = parsed && parsed.code;
    err.id = parsed && parsed.id;
    err.status = res.status;
    return err;
  }

  function hideBanner() {
    banner.hidden = true;
    file = null;
    if (fileURL) {
      win.URL.revokeObjectURL(fileURL);
      fileURL = '';
    }
    saveLink.removeAttribute('href');
    saveLink.removeAttribute('download');
  }

  function showBanner(text, ready) {
    msg.textContent = text;
    shareBtn.hidden = !ready || !(win.navigator && win.navigator.share);
    saveLink.hidden = !ready;
    banner.hidden = false;
  }

  function paintTime() {
    if (rec && recTime && isFront()) recTime.textContent = elapsed(now() - rec.started);
  }

  function paintPill() {
    if (pillRec) pillRec.hidden = !(rec && isFront());
    paintTime();
  }

  // settle clears the running state; the file, if any, is fetched after.
  function settle() {
    if (rec) clearId(rec.target);
    rec = null;
    win.clearInterval(tickTimer);
    win.clearInterval(pollTimer);
    tickTimer = pollTimer = 0;
    paintPill();
    changed();
  }

  async function start() {
    const target = deps.targetId();
    if (!target) throw new Error('no device selected');
    const res = await deps.fetch(`/api/record?target=${encodeURIComponent(target)}`, { method: 'POST' });
    if (res.status === 409) {
      const err = await errorOf(res);
      if (err.code === 'recording') {
        deps.report({ event: 'record-error', action: 'start', code: 'recording' });
        // The body names the one in progress. Adopt it when it is this
        // device's, which is what a reload leaves behind.
        if (err.id && await adopt(err.id, target)) return;
        showToast(deps.toast, 'Already recording this device', 'more');
        return;
      }
      throw err;
    }
    if (res.status !== 201) throw await errorOf(res);
    const { id } = await res.json();
    hideBanner();
    run({ id, target }, 0);
    deps.report({ event: 'record-start', target });
    // The viewer switched away while the POST was in flight, and
    // onTargetSwitch had nothing to stop yet.
    if (deps.targetId() !== target) await stop('switch');
  }

  // run puts the pill and the timers on a recording of this page's; ago is
  // how long it has already been going.
  function run({ id, target }, ago) {
    rec = { id, target, started: now() - ago };
    storeId(target, id);
    paintPill();
    tickTimer = win.setInterval(paintTime, 1000);
    pollTimer = win.setInterval(poll, POLL_MS);
    changed();
  }

  // adoptMeta takes a recording in progress on target as this page's.
  // The server's start time is wall clock, so the age is read against the
  // page's own wall clock and clamped, which a skewed clock cannot send
  // negative.
  function adoptMeta(meta, target) {
    if (rec || busy || !meta || !meta.inProgress || meta.target !== target) return false;
    if (deps.targetId() !== target) return false;
    const ago = Math.max(0, wall() - Date.parse(meta.started)) || 0;
    run({ id: meta.id, target }, ago);
    deps.report({ event: 'record-adopt', target, agoMs: Math.round(ago) });
    return true;
  }

  async function adopt(id, target) {
    try {
      const res = await deps.fetch(`/api/record/${encodeURIComponent(id)}`);
      if (!res.ok) return false;
      return adoptMeta(await res.json(), target);
    } catch (e) {
      return false;
    }
  }

  // sync finds the recording this tab started before a reload and takes it
  // up again. Another viewer's or an agent's on the same target is left alone.
  async function sync() {
    const target = deps.targetId();
    const id = storedId(target);
    if (!target || !id || rec || busy) return;
    try {
      const res = await deps.fetch('/api/record');
      if (!res.ok) return;
      const best = adoptable(await res.json(), target, id);
      if (best) adoptMeta(best, target);
    } catch (e) {}
  }

  // stop asks the server to end this page's recording; its 200 comes once
  // the file is finished, so the download can start straight after.
  async function stop(via) {
    const { id } = rec;
    settle();
    showBanner(progress(0, 0), false);
    let meta;
    try {
      const res = await deps.fetch(`/api/record/${encodeURIComponent(id)}/stop`, { method: 'POST' });
      if (!res.ok) throw await errorOf(res);
      meta = await res.json();
    } catch (err) {
      hideBanner();
      fail('stop', err);
      return;
    }
    await deliver(id, { ...meta, reason: meta.reason || 'stopped' }, via);
  }

  // poll notices a recording that ended without this page asking.
  async function poll() {
    if (!rec || busy) return;
    const { id } = rec;
    let meta;
    try {
      const res = await deps.fetch(`/api/record/${encodeURIComponent(id)}`);
      if (res.status === 404) {
        if (rec && rec.id === id) {
          settle();
          fail('poll', new Error('the recording is gone'));
        }
        return;
      }
      if (!res.ok) return; // a passing failure; ask again next tick
      meta = await res.json();
    } catch (e) {
      return;
    }
    if (meta.inProgress || !rec || rec.id !== id || busy) return;
    busy = true;
    try {
      settle();
      showBanner(progress(0, 0), false);
      await deliver(id, meta, 'ended');
    } finally {
      busy = false;
    }
  }

  // deliver downloads the MP4 with a running count, then offers it. A
  // recording with no frames is not fetched: there is nothing to share.
  async function deliver(id, meta, via) {
    const mine = ++gen;
    const { reason, frames } = meta;
    deps.report({ event: 'record-stop', reason, via, frames });
    if (frames === 0) {
      showBanner(endMessage(reason, frames), false);
      return;
    }
    try {
      const res = await deps.fetch(`/api/record/${encodeURIComponent(id)}.mp4`);
      if (!res.ok) throw await errorOf(res);
      const total = Number(res.headers.get('Content-Length')) || 0;
      let blob;
      if (res.body && res.body.getReader) {
        const reader = res.body.getReader();
        const parts = [];
        let got = 0;
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          parts.push(value);
          got += value.byteLength;
          if (mine === gen) msg.textContent = progress(got, total);
        }
        blob = new Blob(parts, { type: 'video/mp4' });
      } else {
        blob = await res.blob();
      }
      if (mine !== gen) return;
      file = new File([blob], `loupe-${id}.mp4`, { type: 'video/mp4' });
      fileURL = win.URL.createObjectURL(file);
      saveLink.href = fileURL;
      saveLink.download = file.name;
      showBanner(endMessage(reason), true);
    } catch (err) {
      if (mine !== gen) return;
      hideBanner();
      fail('fetch', err);
    }
  }

  async function toggle() {
    if (busy) return;
    busy = true;
    try {
      if (rec) await stop('menu');
      else await start();
    } catch (err) {
      fail(rec ? 'stop' : 'start', err);
    } finally {
      busy = false;
    }
  }

  if (pillRec) {
    // It sits inside the pill's button, so the drawer would open too
    // without the stop, as #pillDot does.
    pillRec.addEventListener('click', (ev) => {
      ev.stopPropagation();
      ev.preventDefault();
      if (!rec || busy || !isFront()) return;
      busy = true;
      stop('pill').finally(() => { busy = false; });
    });
  }

  // Share must start inside this tap: the file is already here.
  shareBtn.addEventListener('click', () => {
    if (!file) return;
    const f = file;
    let p;
    try {
      p = win.navigator.share({ files: [f] });
    } catch (err) {
      p = Promise.reject(err);
    }
    Promise.resolve(p).then(() => {
      deps.report({ event: 'record-share', outcome: 'shared', bytes: f.size });
      hideBanner();
    }, (err) => {
      if (err && err.name === 'AbortError') {
        deps.report({ event: 'record-share', outcome: 'cancelled' });
        return;
      }
      const m = String((err && err.message) || err);
      deps.report({ event: 'record-share', outcome: 'error', err: m });
      showToast(deps.toast, `Share: ${m}`, 'more');
    });
  });

  // iOS may ask before it saves and read the blob after the answer, so the
  // object URL lives until the banner goes (hideBanner), not on a timer.
  saveLink.addEventListener('click', () => {
    if (!file) return;
    deps.report({ event: 'record-save', bytes: file.size });
  });

  msg.addEventListener('click', () => {
    if (file) hideBanner();
  });

  return {
    toggle,
    recording: () => !!rec,
    label: () => (rec ? 'Stop Recording' : 'Record'),
    onChange(fn) { listeners.push(fn); },
    repaint: paintPill,
    sync,

    // onTargetSwitch(newId) stops this page's recording when the viewer
    // moves to another target; the server would keep it running otherwise.
    onTargetSwitch(newId) {
      if (file) hideBanner();
      if (!rec || rec.target === newId || busy) return;
      busy = true;
      stop('switch').catch((err) => fail('stop', err)).finally(() => { busy = false; });
    },
  };
}
