// Handedness: which side of the page the nav row and the overflow corner sit
// on. Persisted so a phone held the same way every session does not have to
// be told again.
//
// storage is whatever the caller passes -- real code passes localStorage,
// tests pass a fake with getItem/setItem. Private browsing can make either
// call throw (Safari does, in some modes), so both directions swallow the
// error and fall back to the safe default rather than crashing the page
// over a preference.

export const KEY = 'loupe.hand';

// safeStorage(win = window) -> Storage or null.
//
// readHand/writeHand's try/catch guards storage.getItem/setItem throwing,
// but in Safari with all storage blocked (some private-mode configurations),
// the throw happens earlier: merely reading the `window.localStorage`
// property throws, because the API is disabled outright rather than left in
// place with methods that fail individually. That throw happens at the call
// site before readHand/writeHand are ever entered, so it needs its own
// guard. Callers do `readHand(safeStorage())`; readHand and writeHand
// already treat a null storage as "nothing stored" via the same catch that
// handles getItem/setItem failing.
//
// win is a parameter, defaulting to the global `window`, so a test can pass
// an object whose `localStorage` getter throws without needing a DOM.
export function safeStorage(win) {
  const target = win === undefined ? (typeof window === 'undefined' ? null : window) : win;
  if (!target) return null;
  try {
    return target.localStorage || null;
  } catch {
    return null;
  }
}

// readHand: 'left' (default) or 'right'. Anything else stored -- missing,
// junk, or an exception reading it -- reads as 'left'.
export function readHand(storage) {
  try {
    return storage.getItem(KEY) === 'right' ? 'right' : 'left';
  } catch {
    return 'left';
  }
}

// writeHand: best-effort persist. A storage error (private mode, quota) is
// not worth surfacing for a preference the page still works without.
export function writeHand(storage, hand) {
  try {
    storage.setItem(KEY, hand === 'right' ? 'right' : 'left');
  } catch {
    // ignored: the preference just does not stick this session
  }
}

// applyHand: the one class the CSS keys off to swap the two corners.
export function applyHand(root, hand) {
  root.classList.toggle('hand-right', hand === 'right');
}
