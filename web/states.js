// The full-screen states the viewer shows instead of a stream (Figma nodes
// 6:10, 6:16, 6:25, 6:36 and 6:47), and the action sheet that asks before
// something destructive.
//
// Each state is a plain object built by a pure function here, so its copy
// and its trace key are testable without a page:
//
//   { key, trace, icon, title, body, detail, actions: [{ id, label, primary }] }
//
// `trace` is what goes to /api/log as the overlay's title. It is the string
// the old showOverlay reported, kept word for word so existing
// kind:"client" records still group with new ones; `title` is what the
// screen says and is free to change. Actions are ids, and app.js binds them
// to handlers when it shows the state, so nothing here reaches into the
// session.

// retryText is the Reconnecting line's tail for a retry `ms` away.
// Rounded up, so the number never reads 0 while the wait is still running.
export function retryText(ms) {
  if (!(ms > 0)) return 'retrying now';
  return `next in ${Math.ceil(ms / 1000)} s`;
}

// shortHost names the server the way a person does: the machine, not the
// tailnet. An address is kept whole, since its first part names nothing.
export function shortHost(hostname) {
  const h = String(hostname || '');
  if (!h) return 'The server';
  if (/^[\d.]+$/.test(h) || h.includes(':')) return h;
  return h.split('.')[0];
}

export function connectingState({ label, kind, booting } = {}) {
  let body = 'Opening the stream.';
  if (booting && kind === 'emulator') body = 'Booting the emulator first.';
  else if (booting && kind === 'simulator') body = 'Booting the simulator first.';
  return {
    key: 'connecting',
    trace: 'Connecting',
    icon: 'progress',
    title: label ? `Connecting to ${label}` : 'Connecting',
    body,
    detail: '',
    actions: [],
  };
}

// reconnectingState covers both ways a session ends up retrying: a live
// connection that closed (trace "Disconnected") and a connect that threw
// (trace "Cannot connect", with the error as the detail).
export function reconnectingState({ remainingMs, error, attempt = 1 } = {}) {
  return {
    key: 'reconnecting',
    trace: error ? 'Cannot connect' : 'Disconnected',
    icon: 'progress',
    title: 'Reconnecting',
    body: `Attempt ${attempt}, ${retryText(remainingMs)}`,
    bodyAt: (ms) => `Attempt ${attempt}, ${retryText(ms)}`,
    detail: error || '',
    actions: [{ id: 'retryNow', label: 'Retry now' }],
  };
}

export function unreachableState({ hostname } = {}) {
  return {
    key: 'unreachable',
    trace: 'Cannot reach the server',
    icon: 'warning',
    title: 'Can’t reach the server',
    body: `${shortHost(hostname)} isn’t answering. If the Mac is asleep or off Tailscale, wake it and try again.`,
    detail: '',
    actions: [{ id: 'retry', label: 'Try again', primary: true }, { id: 'switchServer', label: 'Switch server' }],
  };
}

// cantStreamState is a target the server knows and cannot open. The reason
// is the server's own words, so it goes in the mono detail rather than
// being dressed up as a sentence; the retry already scheduled keeps
// running underneath.
export function cantStreamState({ label, reason } = {}) {
  return {
    key: 'cantStream',
    trace: `Cannot stream ${label}`,
    icon: 'error',
    title: `Can’t stream ${label}`,
    body: 'Loupe keeps trying in the background. Pick another device to watch in the meantime.',
    detail: reason || '',
    actions: [{ id: 'pick', label: 'Pick another device', primary: true }],
  };
}

// shutDownState is a target the server ended on purpose: shut down (it can be
// started again) or deleted (it cannot).
export function shutDownState({ label, reason } = {}) {
  // The server shares no protocol version with this page: a page cached from
  // before the server moved on. Only a reload fixes it.
  if (reason === 'unsupportedProtocol') {
    return {
      key: 'shutDown',
      trace: `${label}: page out of date`,
      icon: 'power',
      title: 'This page is out of date',
      body: 'The server speaks a newer protocol than this page. Reload to update it.',
      detail: '',
      actions: [{ id: 'reload', label: 'Reload', primary: true }],
    };
  }
  const deleted = reason === 'deleted';
  return {
    key: 'shutDown',
    trace: deleted ? `${label} was deleted` : `${label} is shut down`,
    icon: 'power',
    title: deleted ? `${label} was deleted` : `${label} is shut down`,
    body: deleted ? 'Pick another device to watch.' : 'Start it again, or pick another device.',
    detail: '',
    actions: deleted
      ? [{ id: 'pick', label: 'Pick another device', primary: true }]
      : [{ id: 'start', label: 'Start it', primary: true }, { id: 'pick', label: 'Pick another device' }],
  };
}

// afterClose is what a closed transport leads to. An end the server named
// beats everything: reconnecting would only be refused.
export function afterClose({ ended, manualClose } = {}) {
  return ended ? 'ended' : manualClose ? 'none' : 'reconnect';
}

// afterResume is what coming back to the foreground does. A target the server
// ended stays ended: reconnecting would boot a device the person shut down.
export function afterResume({ ended, open } = {}) {
  return ended ? 'stay' : open ? 'refresh' : 'connect';
}

// endedFromClose reads a WebTransport session close. Code 3 is the server
// saying the target ended (wtEnded in internal/httpapi/webtransport.go), with
// the reason as the close message; anything else is not an end.
export function endedFromClose(info) {
  if (!info || info.code !== 3) return null;
  return info.reason || 'shutDown';
}

// canCreate puts New device… first, for a server that has the android CLI.
// Without it Try again leads; with it there is none, since the targets poll
// leaves this state on its own once a device appears.
export function noDevicesState({ canCreate = false } = {}) {
  const help = { id: 'connectHelp', label: 'How to connect a phone' };
  return {
    key: 'noDevices',
    trace: 'No targets',
    icon: 'add',
    title: 'No devices yet',
    body: canCreate
      ? 'Plug in an Android phone with USB debugging on, start an emulator, or create one here.'
      : 'Plug in an Android phone with USB debugging on, or start an emulator.',
    detail: '',
    actions: canCreate
      ? [{ id: 'newDevice', label: 'New device…', primary: true }, help]
      : [{ id: 'retry', label: 'Try again', primary: true }, help],
  };
}

export function unsupportedState() {
  return {
    key: 'unsupported',
    trace: 'WebCodecs not available',
    icon: 'error',
    title: 'This browser can’t play the stream',
    body: 'Loupe decodes video with WebCodecs, which needs iOS 17 or newer. Safari in Lockdown Mode turns it off.',
    detail: '',
    actions: [],
  };
}

// Static markup only: none of these carry text from a device or a server.
const ICON_SVG = {
  progress: '<span class="ovSpinner"></span>',
  error: '<svg viewBox="0 0 24 24"><path d="M7 7l10 10M17 7L7 17"/></svg>',
  warning: '<svg viewBox="0 0 24 24"><path d="M12 6v8"/><circle cx="12" cy="18" r=".6"/></svg>',
  add: '<svg viewBox="0 0 24 24"><path d="M12 6v12M6 12h12"/></svg>',
  power: '<svg viewBox="0 0 24 24"><path d="M12 4v8"/><path d="M7.5 7.5a7 7 0 1 0 9 0"/></svg>',
};

// mountOverlay owns one overlay: the page's #overlay or a pane's. show() replaces whatever state is up,
// including its countdown, so a Reconnecting tick can never overwrite the
// Can't stream state that explainFailure puts up a moment later.
// `until` is a performance.now() deadline for a state with bodyAt.
export function mountOverlay(root, { report, now = () => performance.now() } = {}) {
  const icon = root.querySelector('.ovIcon');
  const title = root.querySelector('.ovTitle');
  const body = root.querySelector('.ovBody');
  const detail = root.querySelector('.ovDetail');
  const actions = root.querySelector('.ovActions');
  let ticker = 0;
  let shown = null;

  function stopTicker() {
    if (ticker) clearInterval(ticker);
    ticker = 0;
  }

  function show(s, handlers = {}, until) {
    stopTicker();
    // Every dead end in this client ends here, so logging here covers all
    // of them at once and keeps the reason on the server rather than on a
    // screen someone has to photograph.
    if (report) report({ event: 'overlay', title: s.trace, state: s.key });
    shown = s.key;
    root.dataset.icon = s.icon;
    icon.innerHTML = ICON_SVG[s.icon] || '';
    title.textContent = s.title;
    body.textContent = s.body;
    detail.textContent = s.detail || '';
    detail.hidden = !s.detail;
    actions.replaceChildren(...s.actions.map((a) => {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = a.primary ? 'ovButton ovButton--primary' : 'ovButton';
      b.textContent = a.label;
      b.addEventListener('click', () => {
        if (report) report({ event: 'overlay-action', state: s.key, action: a.id });
        const fn = handlers[a.id];
        if (fn) fn();
      });
      return b;
    }));
    if (s.bodyAt && until !== undefined) {
      ticker = setInterval(() => { body.textContent = s.bodyAt(until - now()); }, 250);
    }
    root.hidden = false;
  }

  function hide() {
    stopTicker();
    shown = null;
    root.hidden = true;
  }

  return { show, hide, isShown: () => !root.hidden, state: () => (root.hidden ? null : shown) };
}

// ask shows an iOS-style action sheet and resolves to the chosen action's
// id, or null for Cancel, the scrim or Escape. It builds and removes its
// own element, so two asks never share state.
export function ask({ title, message, actions }, doc = document) {
  return new Promise((resolve) => {
    const wrap = doc.createElement('div');
    wrap.className = 'actionSheet';
    wrap.setAttribute('role', 'dialog');
    wrap.setAttribute('aria-modal', 'true');
    const panel = doc.createElement('div');
    panel.className = 'actionSheet__panel';
    const group = doc.createElement('div');
    group.className = 'actionSheet__group';
    if (title || message) {
      const head = doc.createElement('div');
      head.className = 'actionSheet__head';
      if (title) {
        const t = doc.createElement('p');
        t.className = 'actionSheet__title';
        t.textContent = title;
        head.append(t);
      }
      if (message) {
        const m = doc.createElement('p');
        m.className = 'actionSheet__message';
        m.textContent = message;
        head.append(m);
      }
      group.append(head);
    }
    let done = false;
    function finish(id) {
      if (done) return;
      done = true;
      doc.removeEventListener('keydown', onKey, true);
      wrap.remove();
      resolve(id);
    }
    function onKey(e) {
      if (e.key === 'Escape') { e.stopPropagation(); finish(null); }
    }
    for (const a of actions) {
      const b = doc.createElement('button');
      b.type = 'button';
      b.className = a.destructive ? 'actionSheet__button actionSheet__button--destructive' : 'actionSheet__button';
      b.textContent = a.label;
      b.addEventListener('click', () => finish(a.id));
      group.append(b);
    }
    const cancel = doc.createElement('button');
    cancel.type = 'button';
    cancel.className = 'actionSheet__button actionSheet__cancel';
    cancel.textContent = 'Cancel';
    cancel.addEventListener('click', () => finish(null));
    panel.append(group, cancel);
    wrap.append(panel);
    wrap.addEventListener('click', (e) => { if (e.target === wrap) finish(null); });
    doc.addEventListener('keydown', onKey, true);
    doc.body.append(wrap);
  });
}
