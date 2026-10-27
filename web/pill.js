// pill.js paints the top-left pill: the current target's label plus a dot
// whose colour and title come from health.js's verdict (dotFor's output).
// It is the one place that touches el.pillName/el.pillDot, so app.js's
// one-second tick and handleStats can both call it without either owning
// the DOM write.

// paintPill(el, {label, dot}) -- el needs pillName and pillDot.
// dot is health.js's dotFor() result: {state, label}. A falsy dot paints the
// idle look (no class, "idle" title) so a caller need not special-case the
// pre-connect moment.
//
// pillName's own CSS (text-overflow: ellipsis, inside .pill's max-width:
// 62vw) truncates a long label like "Acme One (ACME00000000001)" so it can
// never crowd the Tools button, but that leaves the clipped half invisible
// entirely -- title puts the full label one tap-and-hold (or hover, on a
// device with a pointer) away.
//
// headless shows the Headless tag after the label: an emulator with no window
// on the Mac, so this stream is the only place it can be seen.
export function paintPill(el, { label, dot, headless } = {}) {
  const text = label == null ? '' : String(label);
  if (el.pillName) el.pillName.textContent = text;
  if (el.pill) el.pill.title = headless ? text + ' (headless)' : text;
  if (el.pillTag) el.pillTag.hidden = !headless;
  if (el.pillDot) {
    const state = dot && dot.state !== 'idle' ? dot.state : '';
    el.pillDot.className = 'dot' + (state ? ' ' + state : '');
    el.pillDot.title = (dot && dot.label) || 'idle';
  }
}
