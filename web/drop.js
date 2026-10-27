// Dropping a file on the viewer.
//
// A photo or a video goes to POST /api/media, which puts it in the device's
// photo library: Pictures or Movies plus a media scan on Android, `simctl
// addmedia` on a simulator. An .apk goes to POST /api/install. Nothing can be
// dropped into an app on the device itself: neither adb nor the simulator's
// HID carries a drag, so the library is where a dropped file can land.
//
// The server decides what a file is from its leading bytes, so route() only
// picks the endpoint, and a file it cannot place is refused here before any
// upload. No file name or contents reach the trace, only kind and size.
//
// Split the way clipboard.js is: pure functions, plus mount() for the DOM
// half (no harness, trap 21).

import { showToast } from './toast.js';

const MEDIA_EXT = /\.(png|jpe?g|gif|webp|heic|heif|mp4|m4v|mov)$/i;

// route(file, targetId) -> 'media', 'install' or null. A file's type is often
// empty for .heic and .apk on macOS, so the extension decides as well.
export function route(file, targetId) {
  if (!file || !offered(targetId)) return null;
  const name = String(file.name || '');
  const type = String(file.type || '');
  if (/\.apk$/i.test(name) || type === 'application/vnd.android.package-archive') {
    return android(targetId) ? 'install' : null;
  }
  if (/^(image|video)\//.test(type) || MEDIA_EXT.test(name)) return 'media';
  return null;
}

// offered(targetId) -> whether a drop can reach this target: a simulator or an
// Android device or AVD. A physical iPhone and the synthetic pattern have no
// photo library this server can write.
export function offered(targetId) {
  return typeof targetId === 'string' && targetId !== '' &&
    targetId !== 'synthetic' && !targetId.startsWith('ios:') && !targetId.startsWith('sim-new:');
}

function android(targetId) {
  return !targetId.startsWith('sim:');
}

// carriesFiles(dataTransfer) -> whether a drag holds files, so text or a link
// dragged across the page does not light the stage up.
export function carriesFiles(dt) {
  if (!dt) return false;
  return Array.from(dt.types || []).includes('Files');
}

// endpoint(kind, targetId) -> the URL a file of that kind is POSTed to.
// allowTest because Android Studio marks the APKs it builds testOnly, which
// install refuses without -t.
export function endpoint(kind, targetId) {
  const t = encodeURIComponent(targetId);
  return kind === 'install' ? `/api/install?target=${t}&allowTest=1` : `/api/media?target=${t}`;
}

// message(kind, ok, body) -> the toast for an upload's answer.
export function message(kind, ok, body) {
  if (ok) {
    if (kind === 'install') return body && body.package ? `Installed ${body.package}` : 'Installed';
    return 'Added to Photos';
  }
  const why = body && (body.error || body.code);
  return (kind === 'install' ? "Couldn't install" : "Couldn't add to Photos") + (why ? `: ${why}` : '');
}

// mount(deps) -> {} wires drag and drop on deps.zone. deps: doc, zone (the
// element that shows the drop outline), targetId(), fetch, toast, report.
export function mount(deps) {
  const { doc, zone, targetId, toast, report } = deps;
  let depth = 0;

  const light = (on) => zone.classList.toggle('dropping', on);

  doc.addEventListener('dragenter', (e) => {
    if (!carriesFiles(e.dataTransfer) || !offered(targetId())) return;
    e.preventDefault();
    depth++;
    light(true);
  });
  doc.addEventListener('dragover', (e) => {
    if (!carriesFiles(e.dataTransfer) || !offered(targetId())) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'copy';
  });
  doc.addEventListener('dragleave', () => {
    depth = Math.max(0, depth - 1);
    if (depth === 0) light(false);
  });
  doc.addEventListener('drop', (e) => {
    if (!carriesFiles(e.dataTransfer)) return;
    e.preventDefault();
    depth = 0;
    light(false);
    const id = targetId();
    for (const file of Array.from(e.dataTransfer.files || [])) send(file, id);
  });

  async function send(file, id) {
    const kind = route(file, id);
    if (!kind) {
      report({ event: 'drop', outcome: 'refused', bytes: file.size });
      showToast(toast, offered(id) ? `Can't add ${file.name || 'that file'}` : "This device can't take files");
      return;
    }
    showToast(toast, kind === 'install' ? 'Installing…' : 'Adding to Photos…');
    const started = Date.now();
    let res, body = null;
    try {
      res = await deps.fetch(endpoint(kind, id), { method: 'POST', body: file });
      try { body = await res.json(); } catch (e) { body = null; }
    } catch (e) {
      report({ event: 'drop', kind, outcome: 'network', bytes: file.size });
      showToast(toast, message(kind, false, { error: 'network' }));
      return;
    }
    report({ event: 'drop', kind, outcome: res.ok ? 'ok' : (body && body.code) || String(res.status), bytes: file.size, tookMs: Date.now() - started });
    showToast(toast, message(kind, res.ok, body));
  }

  return {};
}
