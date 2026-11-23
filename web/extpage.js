// extpage.js -- an extension's page (docs/extensions.md, "Pages") in a sheet
// over the picker, holding a frame at the page's path on loupe.
//
// A frame, not a new tab or a navigation away. A Home Screen web app on iOS
// has its own cookies, apart from Safari's, so a tab opened from it reaches
// loupe without the cookie and gets a 401. The frame is on loupe's origin and
// its requests carry the cookie like the rest of this page's.
//
// The frame has no sandbox attribute. Without allow-same-origin its origin is
// "null", which auth.go's sameOrigin refuses, so the page could not call its
// own extension; with allow-same-origin and allow-scripts a same-origin frame
// can remove its own sandbox. docs/security.md says what that reach means.
//
// Built with createElement rather than innerHTML so extpage.test.mjs can
// drive it on fakedom.js. Like servers.js, it imports nothing (trap 45).

// framePath(page) -> the path to load, or null for anything that is not under
// /api/ext/. The server only lists such paths; this keeps anything else, a
// javascript: URL above all, out of the frame's src.
export function framePath(page) {
  return typeof page === 'string' && page.startsWith('/api/ext/') ? page : null;
}

function el(tag, className) {
  const e = document.createElement(tag);
  if (className) e.className = className;
  return e;
}

// mount(root, {onClose, report, keyTarget}) -> {open, close, isOpen}.
// open({name, label, page}) shows the sheet and loads page; it returns false,
// and shows nothing, for a page framePath refuses. close() removes the frame,
// so the page's scripts and timers stop with the sheet. keyTarget is where
// Escape is heard, window by default: in the capture phase there it runs
// before the picker's own listener on document, so Escape closes this sheet
// and not the picker under it.
//
// Each open makes a new frame rather than setting src on the old one. A src
// change on a frame that has loaded adds an entry to the tab's history, so
// Back, or an edge swipe in a Home Screen app, would walk the hidden frame
// back into a page the person closed. A new frame's first load adds none.
export function mount(root, { onClose, report, keyTarget = globalThis.window } = {}) {
  root.setAttribute('role', 'dialog');
  root.setAttribute('aria-modal', 'true');
  root.setAttribute('aria-labelledby', 'extPageTitle');
  root.setAttribute('tabindex', '-1');

  const handle = el('div', 'sheetHandle');
  handle.setAttribute('aria-hidden', 'true');
  handle.appendChild(el('div', 'sheetGrip'));
  const title = el('h2', 'pickerTitle');
  title.setAttribute('id', 'extPageTitle');
  const done = el('button', 'pickerDone');
  done.setAttribute('type', 'button');
  done.textContent = 'Done';
  const header = el('div', 'pickerHeader');
  header.append(el('span'), title, done);
  const top = el('div', 'pickerTop');
  top.append(handle, header);
  root.replaceChildren(top);

  let isOpen = false;
  let lastFocused = null;
  let frame = null;

  function onKeydown(ev) {
    if (ev.key === 'Escape') { ev.stopPropagation(); ev.preventDefault(); close(); }
  }

  // Keys typed in the page go to the frame's own window, not this one. The
  // frame is same-origin, so Escape is heard there too, on each page it loads.
  function onFrameLoad(ev) {
    try {
      const win = ev && ev.target && ev.target.contentWindow;
      if (win) win.addEventListener('keydown', onKeydown, true);
    } catch (_) { /* a page that navigated off loupe's origin */ }
  }

  function dropFrame() {
    if (!frame) return;
    frame.removeEventListener('load', onFrameLoad);
    frame.remove();
    frame = null;
  }

  function open(p) {
    const src = framePath(p && p.page);
    if (!src) return false;
    const label = (p.label || p.name || 'Extension');
    title.textContent = label;
    dropFrame();
    frame = el('iframe', 'extPageFrame');
    frame.setAttribute('title', label);
    frame.addEventListener('load', onFrameLoad);
    frame.src = src;
    root.appendChild(frame);
    if (report) report({ event: 'extpage-open', name: p.name });
    if (isOpen) return true;
    isOpen = true;
    root.hidden = false;
    if (keyTarget) keyTarget.addEventListener('keydown', onKeydown, true);
    lastFocused = document.activeElement;
    root.focus();
    return true;
  }

  function close() {
    if (!isOpen) return;
    isOpen = false;
    root.hidden = true;
    dropFrame();
    if (keyTarget) keyTarget.removeEventListener('keydown', onKeydown, true);
    if (lastFocused && typeof lastFocused.focus === 'function') lastFocused.focus();
    lastFocused = null;
    if (onClose) onClose();
  }

  done.addEventListener('click', () => close());

  return { open, close, isOpen: () => isOpen };
}
