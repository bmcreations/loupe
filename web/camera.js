// Camera: feed this device's own camera into an AVD that runs on Handset
// Camera. The phone (or Mac) viewing loupe captures a camera with
// getUserMedia, and each frame is POSTed to /api/camera, where the server
// hands it to the Handset Camera feed so the emulated app sees it live.
//
// Only an Android emulator can take the live feed (offered() gates it), and
// only when the page is a secure context: Safari and Chrome refuse
// getUserMedia over plain http, so a phone reaching a loupe by http://<ip>
// cannot capture. The control says so rather than failing silently.
//
// The server is the source of truth for whether a frame fed the live camera.
// An AVD on Handset Camera answers a frame POST with live: true; an AVD on
// any other camera stages the frame as a still and answers live: false (both
// otherwise report applies: boot). So capture is serialized -- one frame in
// flight at a time -- and the first answer that is not live stops the loop
// with guidance, rather than re-staging a PNG many times a second.
//
// Split like record.js: pure functions, then mount() for the DOM half. The
// camera source (getUserMedia + a canvas grab) is one injected seam
// (deps.openCamera) so the loop is testable without a browser.
//
// toast.js is imported, not passed, for the reason record.js gives (trap 45).

import { showToast } from './toast.js';

// The feeder repeats the latest frame at 30 fps, so ~12 fps of fresh frames
// is plenty and keeps a phone's radio and battery down.
export const INTERVAL_MS = 80;
// Uploads allowed in flight once the feed is live; two hide one round trip
// without letting a slow link queue stale frames.
export const MAX_IN_FLIGHT = 2;
// Each side is capped to match the feeder's 1280x720 target; a larger grab is
// only re-scaled downstream.
export const MAX_W = 1280;
export const MAX_H = 720;
export const JPEG_QUALITY = 0.7;
// A run of failed POSTs stops the loop rather than spinning on a dead link.
export const MAX_NET_FAILS = 5;

// offered(t) -> whether the camera control applies to target t. Only an
// Android emulator takes the Handset Camera live feed; a physical Android
// device, a simulator or synthetic does not.
export function offered(t) {
  return !!t && t.os === 'android' && t.kind === 'emulator';
}

// fitDimensions(vw, vh, maxW, maxH) -> {w, h} that fits within the cap with
// the source aspect kept, never upscaled. A source not yet sized is {0, 0}.
export function fitDimensions(vw, vh, maxW, maxH) {
  if (!vw || !vh) return { w: 0, h: 0 };
  const scale = Math.min(1, maxW / vw, maxH / vh);
  return { w: Math.max(1, Math.round(vw * scale)), h: Math.max(1, Math.round(vh * scale)) };
}

// facingName(facing) -> the word for the camera: getUserMedia's 'user' is the
// front camera, 'environment' the back.
export function facingName(facing) {
  return facing === 'user' ? 'front' : 'back';
}

// statusText(state) -> the note under the control. state is {kind, facing?, detail?}.
export function statusText(state) {
  const s = state || {};
  switch (s.kind) {
    case 'idle':
      return 'Feed this device’s camera into the emulator.';
    case 'requesting':
      return 'Asking for camera access…';
    case 'live':
      return `Live from your ${facingName(s.facing)} camera.`;
    case 'notLive':
      return 'This emulator isn’t using Handset Camera. Set its Webcam to Handset Camera, reboot it, then try again.';
    case 'insecure':
      return 'Camera capture needs a secure (https) connection; open loupe over https to use it.';
    case 'denied':
      return 'Camera access was denied. Allow the camera for this site, then try again.';
    case 'nocamera':
      return 'No camera was found on this device.';
    case 'unavailable':
      return s.detail || 'Handset Camera isn’t available right now.';
    case 'error':
      return s.detail ? `Couldn’t feed the camera: ${s.detail}` : 'Couldn’t feed the camera.';
    default:
      return '';
  }
}

// openErrorKind(err) -> the state kind for a getUserMedia rejection.
export function openErrorKind(err) {
  const name = err && err.name;
  if (name === 'NotAllowedError' || name === 'SecurityError') return 'denied';
  if (name === 'NotFoundError' || name === 'OverconstrainedError' || name === 'DevicesNotFoundError') return 'nocamera';
  return 'error';
}

// defaultOpenCamera(facing, {doc, win}) -> a source that grabs JPEG frames
// from a live camera. Source = { facing, grab(): Promise<Blob|null>, stop() }.
// grab() returns null until the video reports a size.
async function defaultOpenCamera(facing, { doc, win }) {
  const stream = await win.navigator.mediaDevices.getUserMedia({
    video: { facingMode: { ideal: facing } },
    audio: false,
  });
  const video = doc.createElement('video');
  video.muted = true;
  video.playsInline = true;
  video.setAttribute('playsinline', '');
  video.srcObject = stream;
  try { await video.play(); } catch (e) {}
  const canvas = doc.createElement('canvas');
  return {
    facing,
    async grab() {
      const vw = video.videoWidth;
      const vh = video.videoHeight;
      if (!vw || !vh) return null;
      const { w, h } = fitDimensions(vw, vh, MAX_W, MAX_H);
      canvas.width = w;
      canvas.height = h;
      canvas.getContext('2d').drawImage(video, 0, 0, w, h);
      return await new Promise((res) => canvas.toBlob(res, 'image/jpeg', JPEG_QUALITY));
    },
    stop() {
      for (const t of stream.getTracks()) t.stop();
      video.srcObject = null;
    },
  };
}

// errorOf(res) -> an Error carrying the server's {error} message, like record.js.
async function errorOf(res) {
  let body = '';
  try { body = (await res.text()).trim(); } catch (e) {}
  let parsed = null;
  try { parsed = JSON.parse(body); } catch (e) {}
  const err = new Error((parsed && parsed.error) || body.slice(0, 160) || `${res.status}`);
  err.status = res.status;
  return err;
}

// mount(deps) -> { start(), stop(), active(), onTargetSwitch(id), refresh(), label() }
//
// deps:
//   doc -- the document.
//   host -- the element the control goes in (the sheet's camera section).
//   fetch(url, init) -- window.fetch.
//   targetId() -- the target in front.
//   report(obj) -- client telemetry.
//   toast -- the shared #toast element.
//   win -- optional; the window, for mediaDevices and isSecureContext.
//   isSecure() -- optional; whether the page may use getUserMedia.
//   openCamera(facing, {doc, win}) -- optional; the camera source, injected in tests.
//   sleep(ms) -- optional; the pacing wait, injected in tests.
export function mount(deps) {
  const doc = deps.doc;
  const win = deps.win || doc.defaultView;
  const isSecure = deps.isSecure || (() => !!(win && win.isSecureContext));
  const openCamera = deps.openCamera || defaultOpenCamera;
  const now = deps.now || (() => Date.now());
  const sleep = deps.sleep || ((ms) => new Promise((r) => (win ? win.setTimeout(r, ms) : setTimeout(r, ms))));

  let facing = 'environment'; // back camera by default
  let active = false;
  let paused = false;
  let source = null;
  let capturedTarget = '';
  let netFails = 0;
  let gen = 0; // bumps on each run; an older loop sees the change and exits

  // Facing selector, the toggle, then the status note.
  const facingRow = doc.createElement('div');
  facingRow.className = 'camFacing';
  const backBtn = doc.createElement('button');
  const frontBtn = doc.createElement('button');
  for (const [btn, f, text] of [[backBtn, 'environment', 'Back'], [frontBtn, 'user', 'Front']]) {
    btn.type = 'button';
    btn.className = 'sheetAction camFacingBtn';
    btn.textContent = text;
    btn.addEventListener('click', () => setFacing(f));
  }
  facingRow.append(backBtn, frontBtn);

  const toggleBtn = doc.createElement('button');
  toggleBtn.type = 'button';
  toggleBtn.className = 'sheetAction';
  toggleBtn.addEventListener('click', () => (active ? stop('button') : start()));

  const note = doc.createElement('p');
  note.className = 'sheetNote';

  (deps.host || doc.body).append(facingRow, toggleBtn, note);

  function setStatus(kind, detail) {
    note.textContent = statusText({ kind, facing, detail });
  }

  function paint() {
    toggleBtn.textContent = active ? 'Stop camera' : 'Use my camera';
    const secure = isSecure();
    toggleBtn.disabled = !secure;
    backBtn.setAttribute('aria-pressed', String(facing === 'environment'));
    frontBtn.setAttribute('aria-pressed', String(facing === 'user'));
    backBtn.disabled = frontBtn.disabled = active;
  }

  function setFacing(f) {
    if (f === facing) return;
    facing = f;
    paint();
    if (active) restart();
  }

  function start() {
    if (active) return;
    if (!isSecure()) { setStatus('insecure'); paint(); return; }
    const target = deps.targetId();
    if (!target) return;
    active = true;
    paused = false;
    netFails = 0;
    capturedTarget = target;
    setStatus('requesting');
    paint();
    deps.report({ event: 'camera-start', target, facing });
    run(target);
  }

  function stop(reason) {
    if (!active && !source) return;
    active = false;
    gen++; // any running loop exits
    if (source) { try { source.stop(); } catch (e) {} source = null; }
    paint();
    if (note.textContent === statusText({ kind: 'live', facing }) || !note.textContent) {
      setStatus('idle');
    }
    if (reason) deps.report({ event: 'camera-stop', reason });
  }

  // restart drops the open source and begins a fresh run (a facing change).
  function restart() {
    if (!active) return;
    gen++;
    if (source) { try { source.stop(); } catch (e) {} source = null; }
    setStatus('requesting');
    run(capturedTarget);
  }

  async function run(target) {
    const mine = ++gen;
    let src;
    try {
      src = await openCamera(facing, { doc, win });
    } catch (err) {
      if (mine !== gen) return;
      const kind = openErrorKind(err);
      setStatus(kind, (err && err.message) || String(err));
      deps.report({ event: 'camera-error', stage: 'open', kind, err: String((err && err.message) || err) });
      active = false;
      paint();
      return;
    }
    if (mine !== gen || !active) { try { src.stop(); } catch (e) {} return; }
    source = src;
    // Until a frame comes back live, send one at a time so an AVD that isn't
    // on Handset Camera stages a single still before the loop stops. After
    // that, keep up to MAX_IN_FLIGHT uploads going and pace from each
    // frame's start: waiting out every round trip held a phone on a
    // tailnet to about 4 fps.
    let confirmed = false;
    let inFlight = 0;
    while (active && mine === gen) {
      if (paused) { await sleep(250); continue; }
      if (inFlight >= MAX_IN_FLIGHT) { await sleep(10); continue; }
      const started = now();
      let blob = null;
      try { blob = await src.grab(); } catch (e) { blob = null; }
      if (!active || mine !== gen) break;
      if (blob && !confirmed) {
        const outcome = await postFrame(target, blob);
        if (!active || mine !== gen) break;
        if (outcome === 'stop') { stopFromLoop(mine); return; }
        if (outcome === 'ok') confirmed = true;
      } else if (blob) {
        inFlight += 1;
        postFrame(target, blob).then((outcome) => {
          inFlight -= 1;
          if (outcome === 'stop' && active && mine === gen) stopFromLoop(mine);
        });
      }
      await sleep(Math.max(0, INTERVAL_MS - (now() - started)));
    }
    try { src.stop(); } catch (e) {}
    if (source === src) source = null;
  }

  // stopFromLoop ends capture after the loop itself decided to (not live, or a
  // dead link); the status note it set stays.
  function stopFromLoop(mine) {
    if (mine !== gen) return;
    active = false;
    if (source) { try { source.stop(); } catch (e) {} source = null; }
    paint();
  }

  // postFrame sends one JPEG and reads what the server did with it.
  // Returns 'ok' to keep going, 'retry' on a passing network error, or 'stop'.
  async function postFrame(target, blob) {
    let res;
    try {
      res = await deps.fetch(`/api/camera?target=${encodeURIComponent(target)}`, {
        method: 'POST',
        headers: { 'Content-Type': 'image/jpeg' },
        body: blob,
      });
    } catch (e) {
      netFails += 1;
      if (netFails >= MAX_NET_FAILS) {
        setStatus('error', (e && e.message) || String(e));
        deps.report({ event: 'camera-error', stage: 'post', err: String((e && e.message) || e) });
        return 'stop';
      }
      return 'retry';
    }
    netFails = 0;
    if (res.status === 503) {
      const err = await errorOf(res);
      setStatus('unavailable', err.message);
      deps.report({ event: 'camera-error', stage: 'post', status: 503, err: err.message });
      return 'stop';
    }
    if (!res.ok) {
      const err = await errorOf(res);
      setStatus('error', err.message);
      deps.report({ event: 'camera-error', stage: 'post', status: res.status, err: err.message });
      return 'stop';
    }
    let body = {};
    try { body = await res.json(); } catch (e) {}
    if (body.live) {
      setStatus('live');
      return 'ok';
    }
    // The frame was staged as a still: this AVD is not on Handset Camera.
    setStatus('notLive');
    deps.report({ event: 'camera-notlive', target });
    return 'stop';
  }

  // The tab going to the background pauses capture (Safari suspends the camera
  // anyway); coming back resumes it.
  if (doc.addEventListener) {
    doc.addEventListener('visibilitychange', () => {
      paused = !!(doc.hidden);
    });
  }

  setStatus('idle');
  paint();

  return {
    start,
    stop,
    active: () => active,
    label: () => (active ? 'Stop camera' : 'Use my camera'),
    refresh() { if (!isSecure() && !active) setStatus('insecure'); paint(); },

    // onTargetSwitch(newId) stops capture when the viewer moves to another
    // target: the feed is one Mac-wide camera, so it must not keep sending the
    // old device's frames.
    onTargetSwitch(newId) {
      if (active && newId !== capturedTarget) {
        stop('switch');
        setStatus('idle');
      }
    },
  };
}
