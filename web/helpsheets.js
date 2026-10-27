// helpsheets.js -- the two sheets the full-screen states open: How to
// connect a phone, from No devices, and Switch server, from Can't reach.
//
// Switch server has to work while this server is not answering, so it
// fetches nothing. It takes a host from a field or from the servers this
// origin has switched between, and navigates there. Nothing from this URL
// goes with it: the other server has its own token, and a first visit there
// needs that server's link.
//
// serverOrigin, readServers and rememberServer are pure and tested in
// helpsheets.test.mjs; the mounts are the DOM shell.

export const SERVERS_KEY = 'loupe.servers';
const MAX_SERVERS = 5;

// serverOrigin(input, here) -> the https origin `input` names, or '' when it
// names none. A bare host keeps this page's port, since every loupe listens
// on the same one unless told otherwise. `here` is a Location-like object.
export function serverOrigin(input, here) {
  let s = String(input || '').trim();
  if (!s) return '';
  if (!/^[a-z]+:\/\//i.test(s)) s = `https://${s}`;
  let u;
  try { u = new URL(s); } catch (_) { return ''; }
  if (u.protocol !== 'https:' && u.protocol !== 'http:') return '';
  // Checked here, not left to URL: Chrome's parser takes "not a host!" and
  // percent-encodes it into a hostname, where Node's throws.
  if (!/^([a-z0-9-]+\.)*[a-z0-9-]+$/i.test(u.hostname) && !/^\[[0-9a-f:.]+\]$/i.test(u.hostname)) return '';
  const typedPort = /^[a-z]+:\/\/[^/]*:\d+/i.test(s);
  if (!typedPort && here && here.port) u.port = here.port;
  return u.origin;
}

export function readServers(storage) {
  try {
    const list = JSON.parse(storage.getItem(SERVERS_KEY) || '[]');
    return Array.isArray(list) ? list.filter((x) => typeof x === 'string').slice(0, MAX_SERVERS) : [];
  } catch (_) {
    return [];
  }
}

// rememberServer puts `origin` first, once, and keeps the newest five.
export function rememberServer(storage, origin) {
  if (!origin) return readServers(storage);
  const list = [origin, ...readServers(storage).filter((x) => x !== origin)].slice(0, MAX_SERVERS);
  try { storage.setItem(SERVERS_KEY, JSON.stringify(list)); } catch (_) {}
  return list;
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[c]));
}

// sheetShell wires what both sheets share: dialog roles, Done, Escape
// ahead of anything underneath, and onClose.
function sheetShell(root, { titleId, title, body, onClose }) {
  root.setAttribute('role', 'dialog');
  root.setAttribute('aria-modal', 'true');
  root.setAttribute('aria-labelledby', titleId);
  root.setAttribute('tabindex', '-1');
  root.innerHTML = `
    <div class="pickerTop">
      <div class="sheetHandle" aria-hidden="true"><div class="sheetGrip"></div></div>
      <div class="pickerHeader">
        <span></span>
        <h2 id="${titleId}" class="pickerTitle">${title}</h2>
        <button type="button" class="pickerDone">Done</button>
      </div>
    </div>
    <div class="pickerList">${body}</div>`;
  let open_ = false;
  function onKeydown(ev) {
    if (ev.key === 'Escape') { ev.stopPropagation(); ev.preventDefault(); close(); }
  }
  function open() {
    if (open_) return;
    open_ = true;
    root.hidden = false;
    window.addEventListener('keydown', onKeydown, true);
    root.focus();
  }
  function close() {
    if (!open_) return;
    open_ = false;
    root.hidden = true;
    window.removeEventListener('keydown', onKeydown, true);
    if (onClose) onClose();
  }
  root.querySelector('.pickerDone').addEventListener('click', () => close());
  return { open, close, isOpen: () => open_ };
}

// Static copy: nothing here comes from a device or a server.
const CONNECT_STEPS = [
  ['An Android phone',
    'Turn on Developer options and USB debugging, plug the phone into this Mac, and allow debugging when it asks. <code>adb devices</code> on the Mac should list it as <code>device</code>.'],
  ['An Android emulator',
    'Every AVD on this Mac is listed, booted or not. Make one in Android Studio’s Device Manager, or with New device… where it is offered.'],
  ['An iPhone or iPad',
    'A phone paired with this Mac in Xcode is listed when <code>devicectl</code> can reach it. Streaming it needs a cable, or Wi-Fi on iOS 27.'],
  ['An iOS simulator',
    'Every simulator is listed once simcap is built: run <code>make simcap</code> in the loupe checkout.'],
];

// mountConnectHelp(root, {onClose, onNewDevice}) -> {open, close, isOpen}.
// onNewDevice, when given, adds a button that closes this and opens New device.
export function mountConnectHelp(root, { onClose, onNewDevice } = {}) {
  const steps = CONNECT_STEPS.map(([h, p]) => `
    <div class="helpStep">
      <div class="helpStepTitle">${h}</div>
      <p class="helpStepBody">${p}</p>
    </div>`).join('');
  const sheet = sheetShell(root, {
    titleId: 'connectHelpTitle',
    title: 'Connect a device',
    body: `<div class="pickerGroup"><div class="helpSteps">${steps}</div></div>
      <div class="helpActions">
        <button type="button" class="ovButton ovButton--primary" data-act="newDevice" hidden>New device…</button>
      </div>`,
    onClose,
  });
  const nd = root.querySelector('[data-act="newDevice"]');
  nd.addEventListener('click', () => { sheet.close(); if (onNewDevice) onNewDevice(); });
  return {
    ...sheet,
    open({ canCreate = false } = {}) { nd.hidden = !(canCreate && onNewDevice); sheet.open(); },
  };
}

// mountSwitchServer(root, {storage, here, navigate, onClose, report})
// -> {open, close, isOpen}. navigate(url) defaults to location.assign.
export function mountSwitchServer(root, { storage, here = location, navigate, onClose, report } = {}) {
  const sheet = sheetShell(root, {
    titleId: 'switchServerTitle',
    title: 'Switch server',
    body: `
      <form class="switchForm" autocomplete="off">
        <label class="pickerGroupTitle" for="switchHost">Server</label>
        <input id="switchHost" class="switchInput" type="text" inputmode="url"
               autocapitalize="off" autocorrect="off" spellcheck="false"
               placeholder="mac-name.tailnet.ts.net">
        <p class="switchError" hidden>That isn’t a host name or an address.</p>
        <button type="submit" class="ovButton ovButton--primary">Go</button>
      </form>
      <div class="pickerGroup" id="switchRecentGroup" hidden>
        <div class="pickerGroupTitle">Recent</div>
        <div class="pickerGroupRows" id="switchRecent"></div>
      </div>
      <p class="pickerFootnote">A server you have not opened on this phone before needs its own link, with its token, the first time.</p>`,
    onClose,
  });
  const input = root.querySelector('#switchHost');
  const err = root.querySelector('.switchError');
  const recentGroup = root.querySelector('#switchRecentGroup');
  const recent = root.querySelector('#switchRecent');
  const go = navigate || ((url) => location.assign(url));

  function switchTo(origin, via) {
    if (report) report({ event: 'switch-server', via, sameOrigin: origin === here.origin });
    rememberServer(storage, here.origin);
    rememberServer(storage, origin);
    go(`${origin}/`);
  }

  root.querySelector('.switchForm').addEventListener('submit', (ev) => {
    ev.preventDefault();
    const origin = serverOrigin(input.value, here);
    err.hidden = !!origin;
    if (origin) switchTo(origin, 'typed');
  });

  function renderRecent() {
    const list = readServers(storage).filter((o) => o !== here.origin);
    recentGroup.hidden = list.length === 0;
    recent.innerHTML = list.map((o) => `
      <button type="button" class="pickerRow" data-origin="${escapeHTML(o)}">
        <span class="pickerRowMain">
          <span class="pickerRowLabel">${escapeHTML(new URL(o).hostname.split('.')[0])}</span>
          <span class="pickerRowDetail">${escapeHTML(o.replace(/^https:\/\//, ''))}</span>
        </span>
      </button>`).join('');
    for (const b of recent.querySelectorAll('[data-origin]')) {
      b.addEventListener('click', () => switchTo(b.getAttribute('data-origin'), 'recent'));
    }
  }

  return {
    ...sheet,
    open() {
      input.value = here.host;
      err.hidden = true;
      renderRecent();
      sheet.open();
    },
  };
}
