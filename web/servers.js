// servers.js -- the Add server sheet: the peers this server lists devices
// from (docs/remote-targets.md, "Adding a server"), a form to add one and a
// Remove button on each.
//
// The pure half (validateServerForm, serverError, postServer, fetchServers,
// deleteServer, serverRowModel) is tested in servers.test.mjs against an
// injected fetch; mount() is the DOM shell.
//
// A 401 from POST /api/servers is the PEER refusing the token, which the
// server passes through. It is not this page's own sign-in failing, so it
// reads "The server refused the token", and nothing here treats it as a
// reason to leave the page. Every other status shows the server's own
// message.

// validateServerForm(form) -> {ok, value} | {ok: false, field, error}.
// The URL is https only, since the token rides every request to the peer; the
// server checks the same and the page says so before the round trip. name is
// optional (the server derives one from the host) and becomes the prefix of
// every id, so it cannot hold a '/'.
export function validateServerForm({ url, token, name } = {}) {
  const rawURL = String(url || '').trim();
  const rawToken = String(token || '').trim();
  const rawName = String(name || '').trim();
  if (!rawURL) return { ok: false, field: 'url', error: 'Enter the server’s address.' };
  let u;
  try { u = new URL(/^[a-z][a-z0-9+.-]*:\/\//i.test(rawURL) ? rawURL : `https://${rawURL}`); } catch (_) {
    return { ok: false, field: 'url', error: 'That isn’t an address.' };
  }
  if (u.protocol !== 'https:') return { ok: false, field: 'url', error: 'The address must start with https://.' };
  if (!u.hostname) return { ok: false, field: 'url', error: 'That isn’t an address.' };
  if (!rawToken) return { ok: false, field: 'token', error: 'Enter that server’s token.' };
  if (rawName.includes('/')) return { ok: false, field: 'name', error: 'The name cannot contain a slash.' };
  const value = { url: u.origin, token: rawToken };
  if (rawName) value.name = rawName;
  return { ok: true, value };
}

// serverError is the message for a refused add or remove. body is the parsed
// {error, code} or null.
export function serverError(status, body, { removing = false } = {}) {
  if (status === 401 && !removing) return 'The server refused the token.';
  if (body && body.error) return String(body.error);
  return `${removing ? 'Could not remove that server' : 'Could not add that server'} (${status})`;
}

async function readBody(res) {
  try { return await res.json(); } catch (_) { return null; }
}

// postServer(fetchImpl, value) -> {ok: true, server, warning} or
// {ok: false, status, message}. status is 0 when the request never got an
// answer.
export async function postServer(fetchImpl, value) {
  let res;
  try {
    res = await fetchImpl('/api/servers', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(value),
    });
  } catch (_) {
    return { ok: false, status: 0, message: 'Could not reach this server.' };
  }
  const body = await readBody(res);
  if (!res.ok) return { ok: false, status: res.status, message: serverError(res.status, body) };
  return { ok: true, server: body, warning: body && typeof body.warning === 'string' ? body.warning : '' };
}

// fetchServers(fetchImpl) -> {list, failed}.
export async function fetchServers(fetchImpl) {
  try {
    const res = await fetchImpl('/api/servers');
    if (!res.ok) return { list: null, failed: true, status: res.status };
    const body = await res.json();
    if (!Array.isArray(body)) return { list: null, failed: true, status: res.status };
    return { list: body, failed: false, status: res.status };
  } catch (_) {
    return { list: null, failed: true, status: 0 };
  }
}

// deleteServer(fetchImpl, name) -> {ok, message}.
export async function deleteServer(fetchImpl, name) {
  let res;
  try {
    res = await fetchImpl(`/api/servers?name=${encodeURIComponent(name)}`, { method: 'DELETE' });
  } catch (_) {
    return { ok: false, message: 'Could not reach this server.' };
  }
  if (res.ok) return { ok: true, message: '' };
  return { ok: false, message: serverError(res.status, await readBody(res), { removing: true }) };
}

// serverRowModel(s) -> {name, host, state, up, detail}. detail is state, the
// target count and, while up, the round trip.
export function serverRowModel(s) {
  const up = s.state === 'up';
  const n = Number.isFinite(s.targets) ? s.targets : 0;
  const parts = [up ? 'Up' : 'Unreachable', `${n} ${n === 1 ? 'device' : 'devices'}`];
  if (up && Number.isFinite(s.rttMs) && s.rttMs > 0) parts.push(`${s.rttMs} ms`);
  let host = s.url || '';
  try { host = new URL(host).host; } catch (_) {}
  return { name: s.name, host, state: s.state, up, detail: parts.join(' · ') };
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[c]));
}

export function serverRowHTML(m) {
  return `
      <div class="serverRow" data-name="${escapeHTML(m.name)}">
        <span class="serverDot serverDot--${m.up ? 'up' : 'down'}" aria-hidden="true"></span>
        <span class="pickerRowMain">
          <span class="pickerRowLabel">${escapeHTML(m.name)}</span>
          <span class="pickerRowDetail${m.up ? '' : ' pickerRowDetail--warn'}">${escapeHTML(m.detail)} · ${escapeHTML(m.host)}</span>
        </span>
        <button type="button" class="serverRemove" data-remove="${escapeHTML(m.name)}"
                aria-label="Remove ${escapeHTML(m.name)}">Remove</button>
      </div>`;
}

// mount(root, {fetch, confirm, onChanged, onClose, report}) -> {open, close,
// isOpen}. confirm(name) -> Promise<boolean> asks before a Remove.
// onChanged() runs after an add or remove, so the picker can fetch its list
// again. This is the same sheet shell as helpsheets.js's, repeated because a
// module other than app.js imports nothing (trap 45).
export function mount(root, { fetch: fetchImpl = globalThis.fetch && globalThis.fetch.bind(globalThis), confirm, onChanged, onClose, report } = {}) {
  root.setAttribute('role', 'dialog');
  root.setAttribute('aria-modal', 'true');
  root.setAttribute('aria-labelledby', 'addServerTitle');
  root.setAttribute('tabindex', '-1');
  root.innerHTML = `
    <div class="pickerTop">
      <div class="sheetHandle" aria-hidden="true"><div class="sheetGrip"></div></div>
      <div class="pickerHeader">
        <span></span>
        <h2 id="addServerTitle" class="pickerTitle">Servers</h2>
        <button type="button" class="pickerDone">Done</button>
      </div>
    </div>
    <div class="pickerList">
      <div class="pickerGroup" id="serverListGroup" hidden>
        <div class="pickerGroupTitle">Added</div>
        <div class="pickerGroupRows" id="serverList"></div>
      </div>
      <p class="switchError" id="serverListError" hidden></p>
      <form class="switchForm" id="serverForm" autocomplete="off" novalidate>
        <label class="pickerGroupTitle" for="serverUrl">Add server</label>
        <input id="serverUrl" class="switchInput" name="url" type="text" inputmode="url"
               autocapitalize="off" autocorrect="off" spellcheck="false"
               placeholder="https://mac-mini.local:18456" aria-label="Server address">
        <input id="serverToken" class="switchInput" name="token" type="password"
               autocapitalize="off" autocorrect="off" spellcheck="false"
               placeholder="Token" aria-label="Token">
        <input id="serverName" class="switchInput" name="name" type="text"
               autocapitalize="off" autocorrect="off" spellcheck="false"
               placeholder="Name (optional)" aria-label="Name">
        <p class="switchError" id="serverError" role="alert" hidden></p>
        <p class="serverWarning" id="serverWarning" role="status" hidden></p>
        <button type="submit" class="ovButton ovButton--primary">Add</button>
      </form>
      <p class="pickerFootnote">Needs https. The token is the other server’s own, from its link or its token file. Its devices appear in the picker under its name.</p>
    </div>`;
  const $ = (sel) => root.querySelector(sel);
  const form = $('#serverForm');
  const fields = { url: $('#serverUrl'), token: $('#serverToken'), name: $('#serverName') };
  const errorEl = $('#serverError');
  const warningEl = $('#serverWarning');
  const submit = form.querySelector('button[type="submit"]');
  const group = $('#serverListGroup');
  const listEl = $('#serverList');
  const listError = $('#serverListError');

  let isOpen = false;
  function onKeydown(ev) {
    if (ev.key === 'Escape') { ev.stopPropagation(); ev.preventDefault(); close(); }
  }
  function open() {
    if (isOpen) return;
    isOpen = true;
    root.hidden = false;
    window.addEventListener('keydown', onKeydown, true);
    errorEl.hidden = true;
    warningEl.hidden = true;
    root.focus();
    refreshList();
  }
  function close() {
    if (!isOpen) return;
    isOpen = false;
    root.hidden = true;
    window.removeEventListener('keydown', onKeydown, true);
    fields.token.value = '';
    if (onClose) onClose();
  }
  $('.pickerDone').addEventListener('click', () => close());

  async function refreshList() {
    const r = await fetchServers(fetchImpl);
    listError.hidden = !r.failed;
    if (r.failed) {
      // 404 is a server with peers switched off, which has nothing to list.
      listError.textContent = r.status === 404 ? 'This server does not take other servers.' : 'Could not load the servers.';
      group.hidden = true;
      return;
    }
    group.hidden = r.list.length === 0;
    listEl.innerHTML = r.list.map((s) => serverRowHTML(serverRowModel(s))).join('');
  }

  listEl.addEventListener('click', async (ev) => {
    const b = ev.target.closest('[data-remove]');
    if (!b || b.disabled) return;
    const name = b.getAttribute('data-remove');
    if (confirm && !(await confirm(name))) return;
    b.disabled = true;
    const r = await deleteServer(fetchImpl, name);
    if (report) report({ event: 'server-remove', outcome: r.ok ? 'ok' : 'failed' });
    if (!r.ok) {
      b.disabled = false;
      listError.textContent = r.message;
      listError.hidden = false;
      return;
    }
    if (onChanged) onChanged();
    refreshList();
  });

  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    if (submit.disabled) return;
    errorEl.hidden = true;
    warningEl.hidden = true;
    const v = validateServerForm({ url: fields.url.value, token: fields.token.value, name: fields.name.value });
    if (!v.ok) {
      errorEl.textContent = v.error;
      errorEl.hidden = false;
      fields[v.field].focus();
      return;
    }
    submit.disabled = true;
    submit.textContent = 'Adding…';
    const r = await postServer(fetchImpl, v.value);
    submit.disabled = false;
    submit.textContent = 'Add';
    if (report) report({ event: 'server-add', outcome: r.ok ? (r.warning ? 'warning' : 'ok') : String(r.status) });
    if (!r.ok) {
      errorEl.textContent = r.message;
      errorEl.hidden = false;
      return;
    }
    // The token is gone from the page as soon as the server has it.
    fields.token.value = '';
    fields.url.value = '';
    fields.name.value = '';
    if (r.warning) {
      warningEl.textContent = r.warning;
      warningEl.hidden = false;
    }
    if (onChanged) onChanged();
    refreshList();
  });

  return { open, close, isOpen: () => isOpen };
}
