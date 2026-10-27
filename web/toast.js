// The shared #toast element (index.html) has one timer, not one per caller.
// corners.js, devicesettings.js (via app.js) and inspect.js each used to run
// their own setTimeout against the same element; the last one to fire won
// arbitrarily, and a toast shown while another was already counting down
// could be hidden early by the first timer rather than the one that just
// displayed it. showToast owns the single timer here so a second call always
// replaces the first cleanly, regardless of which module made it (client
// Task 9 review, MEDIUM-9).

let timer = 0;

// showToast(el, message, corner) -> shows `message` in `el` for 2.5s.
// `corner` is optional and sets `dataset.corner`, which style.css uses to
// anchor the toast near the corner that triggered it (corners.js's own
// nav/more distinction); a caller with no corner concept (devicesettings,
// inspect) leaves it out rather than guessing one.
export function showToast(el, message, corner) {
  if (!el) return;
  el.textContent = message;
  if (corner) el.dataset.corner = corner;
  el.hidden = false;
  clearTimeout(timer);
  timer = setTimeout(() => { el.hidden = true; }, 2500);
}
