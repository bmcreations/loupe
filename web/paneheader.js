// The strip above each pane while several are open side by side (Figma
// 59:177): which ⌃ number focuses it, its health dot, the target's name,
// what it is streaming, and a close button. One pane alone has none; the pill
// says the same.
//
// model(pane, {index, focused, now}) is pure, so what the strip says is
// testable without a page. mount(host, {onClose}) builds the strip once and
// paint(model) writes only what changed: the 1s tick repaints every pane.

import { dotFor } from './health.js';

const STATUS_TEXT = { live: 'streaming', error: 'disconnected' };

export function model(pane, { index, focused = false, now, key = '⌃' }) {
  const st = pane.stats() || {};
  const dot = dotFor({ health: st.health, healthReason: st.healthReason, receivedAt: st.statsReceivedAt }, now);
  const info = pane.info;
  const detail = info && info.codedWidth && Number.isFinite(st.fps)
    ? `${info.codedWidth}×${info.codedHeight} · ${Math.round(st.fps)} fps`
    : STATUS_TEXT[pane.status] || 'connecting';
  return {
    badge: `${key}${index}`,
    label: pane.targetLabel || pane.targetId || '',
    dot: dot.state === 'idle' ? '' : dot.state,
    dotLabel: dot.label,
    detail,
    focused: !!focused,
  };
}

export function mount(host, { onClose }) {
  const badge = document.createElement('span');
  badge.className = 'paneBadge';
  const dot = document.createElement('span');
  dot.className = 'paneDot dot';
  const name = document.createElement('span');
  name.className = 'paneName';
  const detail = document.createElement('span');
  detail.className = 'paneDetail';
  const close = document.createElement('button');
  close.className = 'paneClose';
  close.type = 'button';
  close.textContent = '×';
  // The header's own pointerdown focuses the pane; the close button's click
  // closes it, focused or not.
  close.addEventListener('click', () => onClose());
  host.replaceChildren(badge, dot, name, detail, close);

  let last = {};
  return {
    paint(m) {
      if (m.badge !== last.badge) badge.textContent = m.badge;
      if (m.dot !== last.dot) dot.className = 'paneDot dot' + (m.dot ? ' ' + m.dot : '');
      if (m.dotLabel !== last.dotLabel) dot.title = m.dotLabel;
      if (m.label !== last.label) {
        name.textContent = m.label;
        name.title = m.label;
        close.setAttribute('aria-label', `Close ${m.label}`);
      }
      if (m.detail !== last.detail) detail.textContent = m.detail;
      if (m.focused !== last.focused) host.classList.toggle('focused', m.focused);
      last = m;
    },
  };
}
