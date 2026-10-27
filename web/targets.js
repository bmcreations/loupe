// targets.js does the /api/targets round trip and shape validation as a
// pure function of an injected fetch, plus the sequencing wrapper app.js
// uses to tell a superseded response apart from a current one.
//
// fetchTargets and latestOnly are deliberately two separate, small
// functions rather than one fetchTargetsList doing both jobs. A caller that
// needs "the result of my own request, whatever it is" (loadTargets) and a
// caller that needs "the most recent result, or nothing" (the pill's
// refresh, and the shared lastTargets/drawer render) are different
// questions, and folding them into one gate -- as the previous
// fetchTargetsList did -- silently answered both of them with the second
// one's answer. That produced a live bug: a click on the pill during the
// initial load starts a second /api/targets fetch, which bumps the shared
// sequence number before the FIRST fetch's caller (loadTargets) gets to act
// on its own response. loadTargets then read its own, perfectly good
// response as "superseded" and returned without calling connect() or
// showing a failure overlay -- the page sat on "Connecting" forever, event
// though the request that mattered had already succeeded. Chrome's cache
// lock queues the second GET behind the first, so any click during the
// initial fetch triggers this, regardless of which response the network
// actually returns first.
//
// The fix is not to remove the sequencing -- a second, later fetch legitimately
// should win the shared lastTargets cache and the open drawer's render, so
// an earlier response does not clobber newer data merely because its round
// trip took longer. The fix is to stop using that same signal to decide
// whether the CALLER's own request succeeded. latestOnly's `current` flag
// answers only "is my response still the most recent one anybody's asked
// for", and it is the caller's job to decide what that does and does not
// gate.

// fetchTargets performs one /api/targets round trip through the injected
// fetchImpl and validates the response shape. A network error, a non-OK
// response, and a body that is not an array are all reported the same way
// -- {list: null, failed: true} -- because every caller here has exactly
// one thing to do about any of them (show one failure state), and
// distinguishing them further would be a fact nothing acts on.
export async function fetchTargets(fetchImpl) {
  try {
    const res = await fetchImpl('/api/targets');
    if (!res.ok) return { list: null, failed: true };
    const body = await res.json();
    if (!Array.isArray(body)) return { list: null, failed: true };
    return { list: body, failed: false };
  } catch {
    return { list: null, failed: true };
  }
}

// latestOnly wraps an async function so every call still resolves with its
// own result -- nothing is ever dropped or replaced -- but also reports
// `current`: whether this call was still the most recently STARTED one by
// the time it resolved. A caller uses `current` to decide whether ITS
// result is the one that should win a piece of shared state (a cache, a
// rendered list); it says nothing about whether the call itself succeeded,
// and a caller that needs to act on its own outcome regardless of ordering
// -- see loadTargets in app.js -- must not gate that on `current`.
export function latestOnly(fn) {
  let seq = 0;
  return async (...args) => {
    const ticket = ++seq;
    const result = await fn(...args);
    return { current: ticket === seq, result };
  };
}

// ---------------------------------------------------------------------------
// Availability
//
// The server tracks adb live and keeps a device that drops off as "missing"
// rather than removing it, so these are the decisions a phone user meets when
// a device goes away. They live here rather than in app.js because app.js has
// no harness (trap 21).
// ---------------------------------------------------------------------------

// isAvailable treats an absent field as available: a server that predates
// availability tracking lists only targets it can open. drawer.js repeats
// this one comparison rather than importing it, since modules other than
// app.js must not import each other (trap 45).
export function isAvailable(t) {
  return !!t && t.available !== false;
}

// chooseTarget picks what to open. A target the viewer asked for is kept even
// when it cannot open, so the overlay can say why instead of quietly showing
// another screen. Without one, it prefers a target that can actually stream
// over one that cannot, one already running over one that would boot, and a
// real device over an emulator over a simulator over the pattern. Opening the
// page should show what is on, not start a second emulator beside it.
// A running AVD also matches the serial it is attached as, which is what a
// page saved before AVDs had ids of their own is still asking for.
// A strict pick returns null for a wanted target the list does not have,
// rather than falling back: with several panes open, the fallback is some
// other device opening beside the ones asked for.
export function chooseTarget(list, wanted, { strict = false } = {}) {
  if (!list || list.length === 0) return null;
  if (wanted) {
    const t = list.find(t => t.id === wanted) || list.find(t => t.serial === wanted);
    if (t) return t.id;
    if (strict) return null;
  }
  return best(list.filter(isAvailable)) || best(list);
}

// alternative is an available target to offer in place of one that cannot
// open, or null if there is none.
export function alternative(list, id) {
  return best((list || []).filter(t => t.id !== id && isAvailable(t)));
}

function best(list) {
  for (const kind of ['device', 'emulator', 'simulator']) {
    const t = list.find(t => t.kind === kind && isRunning(t));
    if (t) return t.id;
  }
  for (const kind of ['device', 'emulator']) {
    const t = list.find(t => t.kind === kind);
    if (t) return t.id;
  }
  return list.length ? list[0].id : null;
}

// isRunning is a device that can stream without booting first: available,
// not shut down, not being created, and not the test pattern.
export function isRunning(t) {
  return isAvailable(t) && !t.bootsOnOpen && t.state !== 'creating' && t.kind !== 'synthetic';
}

// failureReason is what the server knows about why a target did not open, or
// null. The target's current state comes first: lastError can be from an
// attempt made while it was still connected.
export function failureReason(list, id) {
  const t = (list || []).find(t => t.id === id);
  if (!t) return null;
  return t.reason || t.lastError || null;
}

// SHOWN is every field the drawer and pill render from. lastError is not one
// of them, so a new error on an unchanged list does not repaint anything.
// reason is: an unavailable row and a creating one show it, and a creating
// row's reason carries the time so far. claim is an object, compared by the
// parts the row shows; its idleMs changes on every poll and is not one.
const SHOWN = ['id', 'kind', 'label', 'available', 'state', 'bootsOnOpen', 'form', 'os', 'osVersion', 'apiLevel', 'transport', 'reason', 'handsOff', 'drivenBy', 'headless', 'server'];

// sameShown reports whether a fresh list would render exactly as the old one
// did, so the 5s poll does not repaint a drawer the viewer has open under
// their finger.
export function sameShown(a, b) {
  if (!a || !b || a.length !== b.length) return false;
  return a.every((t, i) => SHOWN.every(k => t[k] === b[i][k]) && claimShown(t.claim) === claimShown(b[i].claim));
}

function claimShown(c) {
  return c ? [c.agent, c.session, c.link].join('\n') : '';
}
