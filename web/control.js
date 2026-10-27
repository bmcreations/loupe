// Input arbitration, as the viewer sees it. The server holds a target
// against agents' /api/input while a person at a viewer is using it, and
// sends every viewer MsgControl (0x08) whenever that or the agent's state
// changes: a type byte, then JSON {held, holder, agent, agentName}.
//
// This module shows it as a banner: "<agent> is driving" while an agent's
// input is landing, "<agent> is waiting for you" while one is being refused
// because this phone is holding the target. While an agent drives it offers
// Take over, which sends MsgTakeOver (0x17): the server holds the target for
// this viewer and sends nothing to the device. Touching the screen also takes
// control, but acts on the device as it does. Only the button takes taps;
// the banner overlaps the top of the device and must not eat taps meant for
// it. It is shown regardless of "Show touches", which is a separate choice
// about drawing gestures.
//
// It also builds the X-Loupe-Viewer header this viewer's own /api/input
// calls carry, so the server counts an iOS button or a rotation as the
// person's rather than refusing it as an agent's.

export const MSG_CONTROL = 0x08;
export const MSG_TAKE_OVER = 0x17;

// decodeControl parses a MsgControl body (without its type byte). A body
// that does not parse reads as free and idle rather than throwing: a stale
// banner is worse than a missing one.
export function decodeControl(body) {
  try {
    const st = JSON.parse(new TextDecoder().decode(body));
    return {
      held: !!st.held,
      holder: Number(st.holder) || 0,
      agent: st.agent === 'driving' || st.agent === 'waiting' ? st.agent : 'idle',
      agentName: typeof st.agentName === 'string' ? st.agentName : '',
    };
  } catch (_) {
    return { held: false, holder: 0, agent: 'idle', agentName: '' };
  }
}

// bannerText is what the banner says for a state, or '' for none. self is
// init's viewer id, 0 when the server sent none. "waiting for you" is said
// only to the viewer holding the target; another viewer is told the agent is
// waiting, without the "you".
export function bannerText(st, self) {
  const who = st.agentName || 'An agent';
  if (st.agent === 'driving') return `${who} is driving`;
  if (st.agent === 'waiting') {
    return !self || !st.holder || st.holder === self
      ? `${who} is waiting for you`
      : `${who} is waiting`;
  }
  return '';
}

// heldByOther is whether a different viewer holds the device: the Tools
// sheet offers Take Over then. self is init's viewer id, 0 when none.
export function heldByOther(st, self) {
  return !!(st.held && st.holder && self && st.holder !== self);
}

// viewerHeaders is what this viewer adds to its own /api/input requests.
export function viewerHeaders(self) {
  return self ? { 'X-Loupe-Viewer': String(self) } : {};
}

// mount(deps) builds the banner.
//
// deps:
//   doc -- the document.
//   host -- the element the banner goes in, and whose data-agent style.css
//     reads; doc.body by default.
//   report(obj) -- client telemetry.
//   takeOver() -- sends MsgTakeOver on the session; throws when it cannot.
//   onHeldByOther(on) -- optional; called when whether another viewer holds
//     the device changes (the Tools sheet's Control section follows it).
export function mount(deps) {
  const banner = deps.doc.createElement('div');
  banner.className = 'controlBanner';
  banner.setAttribute('role', 'status');
  banner.hidden = true;
  const label = deps.doc.createElement('span');
  label.className = 'controlText';
  banner.appendChild(label);
  const button = deps.doc.createElement('button');
  button.className = 'controlTakeOver';
  button.type = 'button';
  button.textContent = 'Take over';
  button.hidden = true;
  banner.appendChild(button);
  const host = deps.host || deps.doc.body;
  host.appendChild(banner);

  button.addEventListener('click', () => {
    const rec = { event: 'control-takeover', agent: last.agentName };
    try {
      deps.takeOver();
    } catch (err) {
      rec.error = String((err && err.message) || err);
    }
    if (deps.report) deps.report(rec);
  });

  let self = 0;
  // MsgInit's info.control: whether this session accepts input at all.
  let allowed = false;
  let last = { held: false, holder: 0, agent: 'idle', agentName: '' };
  let shown = '';

  let otherHolds = false;
  function setOther(on) {
    if (on === otherHolds) return;
    otherHolds = on;
    if (deps.onHeldByOther) deps.onHeldByOther(on);
  }

  function show(st) {
    last = st;
    setOther(heldByOther(st, self));
    const text = bannerText(st, self);
    if (text === shown) return;
    shown = text;
    label.textContent = text;
    button.hidden = st.agent !== 'driving';
    banner.dataset.agent = st.agent;
    banner.hidden = !text;
    // style.css glows the device off this while an agent is driving.
    if (text) host.dataset.agent = st.agent;
    else delete host.dataset.agent;
    if (deps.report) deps.report({ event: 'control-banner', state: st.agent, agent: st.agentName, held: st.held });
  }

  return {
    onInit(info) {
      self = (info && info.viewer) || 0;
      allowed = !!(info && info.control);
      show(last);
    },
    handleMessage(body) {
      show(decodeControl(body));
    },
    // A new target or a dropped connection: the state belonged to a session
    // this page is no longer in, and the next one sends its own after init.
    clear() {
      self = 0;
      allowed = false;
      show({ held: false, holder: 0, agent: 'idle', agentName: '' });
    },
    headers: () => viewerHeaders(self),
    // Whether the current session takes input: the nav row, the keyboard
    // and the clipboard gate on it.
    hasControl: () => allowed,
  };
}
