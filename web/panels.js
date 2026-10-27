// Docked panels: whether the picker on the left and Tools on the right are
// collapsed. Only the docked layout (html.docked, 900px and wider) has
// panels to collapse; on a phone they are sheets that open and close, so
// the classes this sets do nothing there.
//
// Persisted, because someone who wants the device alone on a desktop wants
// it again after every reload. storage is whatever the caller passes, as in
// handedness.js: real code passes safeStorage(), tests a fake. A storage
// that throws or holds junk reads as both panels open.

export const KEY = 'loupe.panels';

export function readPanels(storage) {
  const open = { left: false, right: false };
  try {
    const raw = storage && storage.getItem(KEY);
    if (!raw) return open;
    const v = JSON.parse(raw);
    return { left: v.left === true, right: v.right === true };
  } catch {
    return open;
  }
}

export function writePanels(storage, state) {
  try {
    if (storage) storage.setItem(KEY, JSON.stringify({ left: !!state.left, right: !!state.right }));
  } catch { /* a blocked storage keeps the choice for this page only */ }
}

// toggle(state, side) -> a new state with that side flipped.
export function toggle(state, side) {
  return { ...state, [side]: !state[side] };
}

// applyPanels sets left-collapsed and right-collapsed on root, and each
// button's aria-expanded, which reads "open" since the button shows or hides
// its panel.
export function applyPanels(root, state, buttons = {}) {
  root.classList.toggle('left-collapsed', !!state.left);
  root.classList.toggle('right-collapsed', !!state.right);
  if (buttons.left) buttons.left.setAttribute('aria-expanded', String(!state.left));
  if (buttons.right) buttons.right.setAttribute('aria-expanded', String(!state.right));
}
