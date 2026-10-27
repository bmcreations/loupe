// Immersive mode: the stream alone, with the top bar, the nav row, the overflow
// corner and the bezel all gone, fitted to the whole stage and centred on it.
// A simulator of the viewer's own phone then fills that phone's screen.
//
// Everything that could leave immersive mode is hidden by it, so the way out
// is a handle of its own at the top edge, and it takes a hold rather than a
// tap. The handle sits over the video, and a tap that meant "take me out" but
// landed a pixel off would reach the device instead; a hold on the handle is
// never a touch on the device, because the handle takes the pointer. A short
// tap on it says how to leave, so the handle explains itself the first time.
//
// A double-tap on the blank stage around the device leaves it as well
// (doubletap.js). That one is safe where a tap on the handle is not: the
// letterbox is never part of the device, so a double-tap that lands there
// cannot reach it, and a double-tap that lands on the video does nothing
// new. The handle stays all the same, because the stream can fill the stage
// and leave little or no letterbox to tap.
//
// Two fingers are not an option for the way out: the canvas already turns
// two fingers into a pinch on the device.
//
// iPhone Safari has no element fullscreen API, only video fullscreen, so
// this cannot hide Safari's own bars. It fills what the page is given.
//
// toast.js is imported here directly, the way corners.js, appcard.js,
// devicesettings.js and inspect.js do (trap 45): it holds no state.
import { showToast } from './toast.js';

export const KEY = 'loupe.immersive';
export const HOLD_MS = 500;

// readImmersive: off unless '1' is stored. A storage error reads as off, the
// same as handedness.js's default, since the page works either way.
export function readImmersive(storage) {
  try {
    return !!storage && storage.getItem(KEY) === '1';
  } catch {
    return false;
  }
}

export function writeImmersive(storage, on) {
  try {
    if (storage) storage.setItem(KEY, on ? '1' : '0');
  } catch {
    // ignored: the preference just does not stick this session
  }
}

// centreFit(stage, box) -> {scale, x, y, width, height} or null.
//
// A contain fit centred on both axes. fitStage in layout.js top-aligns and
// reserves room for the corner clusters, and in immersive mode there are no
// clusters and nothing above the device to sit under, so the letterbox is
// split evenly instead. Floor, as fitStage does, so a fractional stage never
// rounds the box past the space it has.
export function centreFit(stage, box) {
  const ok = (n) => typeof n === 'number' && isFinite(n) && n > 0;
  if (!stage || !box || !ok(stage.width) || !ok(stage.height) || !ok(box.width) || !ok(box.height)) {
    return null;
  }
  const scale = Math.min(stage.width / box.width, stage.height / box.height);
  const width = Math.floor(box.width * scale);
  const height = Math.floor(box.height * scale);
  return {
    scale,
    x: Math.round((stage.width - width) / 2),
    y: Math.round((stage.height - height) / 2),
    width,
    height,
  };
}

// mount(root, handle, deps) wires immersive mode.
//
//   root    the element the `immersive` class goes on (<html>); style.css
//           keys every change off that one class.
//   handle  the top-edge exit handle, shown only while immersive.
//   deps:
//     storage        localStorage or null (handedness.js's safeStorage).
//     onChange(on)   called after every change, so app.js can refit.
//     report(rec)    app.js's report(), traced as kind:"client".
//     toast          the shared #toast element.
//     timers         optional {setTimeout, clearTimeout}, for tests.
//
// Returns {isOn(), set(on), toggle(), enter(via), exit(via)}. The stored preference is applied at
// mount without a report, since nothing changed.
export function mount(root, handle, deps) {
  const timers = deps.timers || globalThis;
  let on = readImmersive(deps.storage);
  let hold = null;
  let held = false;

  function apply() {
    root.classList.toggle('immersive', on);
    handle.hidden = !on;
  }

  function set(next, via) {
    next = !!next;
    if (next === on) return;
    on = next;
    writeImmersive(deps.storage, on);
    apply();
    if (deps.report) deps.report({ event: 'immersive', on, via: via || 'menu' });
    if (deps.onChange) deps.onChange(on);
  }

  function cancelHold() {
    if (hold !== null) timers.clearTimeout(hold);
    hold = null;
  }

  handle.addEventListener('pointerdown', (ev) => {
    ev.preventDefault();
    cancelHold();
    held = false;
    hold = timers.setTimeout(() => {
      hold = null;
      held = true;
      set(false, 'hold');
    }, HOLD_MS);
  });
  handle.addEventListener('pointerup', () => {
    const wasHeld = held;
    cancelHold();
    held = false;
    if (!wasHeld && on) showToast(deps.toast, 'Hold to leave full screen');
  });
  handle.addEventListener('pointercancel', cancelHold);
  handle.addEventListener('pointerleave', cancelHold);
  // A long press on iOS otherwise opens the callout menu over the page.
  handle.addEventListener('contextmenu', (ev) => ev.preventDefault());

  apply();

  return {
    isOn: () => on,
    set: (next) => set(next, 'menu'),
    toggle: () => set(!on, 'menu'),
    enter: (via) => set(true, via),
    exit: (via) => set(false, via),
  };
}
