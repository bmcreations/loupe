// The open panes, in order, and which one has focus.
//
// Docked desktop shows up to four devices side by side (Figma 59:178,
// 59:761, 59:1374). This holds the list; app.js builds and tears down each
// pane through the `create` and `destroy` it passes in, and lays them out.
// No DOM here, so the rules are testable on their own:
//
//   - One pane per target. clientStats is scoped to the session and the
//     arbiter doesn't tell two viewers on one target apart, so a second pane
//     on an open target would garble both. Opening it focuses the open one.
//   - At most `max` panes (4).
//   - Exactly one pane has focus while any is open. Closing the focused pane
//     moves focus to the one that slides into its place, else the previous.

export function createPanes({ create, destroy, max = 4 }) {
  const list = [];
  const listeners = [];
  let focusedId = null;
  let seq = 0;

  function changed(kind) {
    for (const fn of listeners.slice()) fn({ kind });
  }
  function find(id) { return list.find((p) => p.id === id) || null; }
  function byTarget(targetId) { return list.find((p) => p.targetId === targetId) || null; }

  function focus(id) {
    const p = find(id);
    if (!p) return null;
    if (focusedId !== id) {
      focusedId = id;
      changed('focus');
    }
    return p;
  }

  return {
    // open returns the new pane, the pane already showing targetId, or null
    // when the list is full.
    open(targetId) {
      const open = targetId ? byTarget(targetId) : null;
      if (open) return focus(open.id);
      if (list.length >= max) return null;
      const p = create(targetId, `p${++seq}`);
      list.push(p);
      focusedId = p.id;
      changed('open');
      return p;
    },
    close(id) {
      const i = list.findIndex((p) => p.id === id);
      if (i < 0) return;
      const [p] = list.splice(i, 1);
      if (focusedId === id) {
        const next = list[i] || list[i - 1] || null;
        focusedId = next ? next.id : null;
      }
      destroy(p);
      changed('close');
    },
    // replace points a pane at another target. A target already open in
    // another pane is focused there instead, and that pane is returned.
    replace(id, targetId) {
      const p = find(id);
      if (!p) return null;
      const open = byTarget(targetId);
      if (open && open !== p) return focus(open.id);
      p.switchTarget(targetId);
      changed('replace');
      return p;
    },
    focus,
    // focusIndex is 1-based, as the ⌃1-⌃4 shortcuts and the header badges.
    focusIndex(n) {
      const p = list[n - 1];
      return p ? focus(p.id) : null;
    },
    focused() { return find(focusedId); },
    list() { return list.slice(); },
    each(fn) { for (const p of list.slice()) fn(p); },
    indexOf(id) { return list.findIndex((p) => p.id === id) + 1; },
    byTarget,
    full() { return list.length >= max; },
    onChange(fn) { listeners.push(fn); },
  };
}

// parseTargets reads the page's panes from its URL: `?target=` repeated, in
// order, the first one focused. A single `?target=` is today's URL.
export function parseTargets(search) {
  const out = [];
  for (const t of new URLSearchParams(search).getAll('target')) {
    if (t && !out.includes(t)) out.push(t);
  }
  return out;
}

// buildSearch writes targetIds back over the URL's `target` params, keeping
// every other param (the token, ?transport=ws) as it was.
export function buildSearch(search, targetIds) {
  const params = new URLSearchParams(search);
  params.delete('target');
  for (const t of targetIds) params.append('target', t);
  const s = params.toString();
  return s ? `?${s}` : '';
}

// paneShortcut(event, mac) -> 1-4, or 0 when the key is not a pane shortcut.
// Safari keeps ⌘1-⌘9 for its tabs and never passes them to the page, so a Mac
// uses ⌃; elsewhere ctrl+digit switches tabs, so Alt. Read by key position
// (code), since Alt changes the character a digit key types.
export function paneShortcut(e, mac) {
  const mod = mac ? e.ctrlKey && !e.altKey : e.altKey && !e.ctrlKey;
  if (!mod || e.metaKey || e.shiftKey) return 0;
  const m = /^Digit([1-4])$/.exec(e.code || '');
  return m ? Number(m[1]) : 0;
}

// paneKey(mac) -> the modifier a pane badge shows before its number.
export function paneKey(mac) {
  return mac ? '⌃' : 'Alt+';
}
