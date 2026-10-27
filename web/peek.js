// Peek: one target's stream in a window of its own on the Mac, drawn in its
// bezel the way the emulator's own window draws it. An AVD loupe boots
// headless has no window, and the emulator cannot grow one without a
// reboot, so this is how someone at the Mac looks at it: the same viewer in
// a pop-up with the chrome taken away, still taking the mouse as touches.
//
// The pop-up is the page itself with ?peek=1. index.html's inline script
// adds html.peek before first paint, style.css hides the top bar under it,
// and app.js forces the frame on without touching the loupe.frame
// preference. The nav row and the overflow stay: a headless emulator has no
// Back or Home of its own to reach for. This module opens the window and sizes it to the
// bezel's shape, since a window the size of the art (1198x2531 for a
// Pixel 10) would not fit on a laptop screen.
//
// A leaf, like its siblings (trap 43, 45): it imports nothing.

const PARAM = 'peek';

// The share of the screen's available height a peek takes at most, so the
// window opens with room around it rather than edge to edge.
const SCREEN_SHARE = 0.85;

export function isPeek(search) {
  return new URLSearchParams(search || '').has(PARAM);
}

export function peekURL(origin, targetId) {
  const q = new URLSearchParams();
  if (targetId) q.set('target', targetId);
  q.set(PARAM, '1');
  return `${origin}/?${q}`;
}

// One window per target: opening a peek of a target that already has one
// brings that window back rather than stacking a second viewer on it.
export function windowName(targetId) {
  return 'loupe-peek-' + String(targetId || 'default').replace(/[^A-Za-z0-9_-]/g, '_');
}

// fitWindow returns the outer size that shows `box` (the bezel, or the
// stream when there is none) as large as SCREEN_SHARE of `avail` allows,
// with `chrome` -- the window's title bar and borders, outer minus inner --
// added back. `chrome` also carries whatever the page itself keeps beside
// the bezel (the stage's padding, the nav row's band), so the bezel is what
// fills the window. Null when either size is unusable.
export function fitWindow(box, avail, chrome) {
  if (!box || !(box.width > 0) || !(box.height > 0)) return null;
  if (!avail || !(avail.width > 0) || !(avail.height > 0)) return null;
  const cw = Math.max(0, (chrome && chrome.width) || 0);
  const ch = Math.max(0, (chrome && chrome.height) || 0);
  const roomW = avail.width * SCREEN_SHARE - cw;
  const roomH = avail.height * SCREEN_SHARE - ch;
  if (roomW <= 0 || roomH <= 0) return null;
  const scale = Math.min(roomW / box.width, roomH / box.height);
  return {
    width: Math.round(box.width * scale) + cw,
    height: Math.round(box.height * scale) + ch,
  };
}

// mount(win, deps) -> {active, offered(), open(), sizeTo(box)}
//
// deps:
//   targetId() -- the target the opener is showing now.
//   report(rec) -- app.js's report().
//
// offered() is false inside a peek and on a touch screen: a phone has no
// second window to put one in. open() runs inside the tap, since a pop-up
// opened after an await is blocked, and throws when the browser blocks it
// anyway. sizeTo() is the peek's own half: app.js calls it from fitScreen
// with the box it fitted, and it resizes once per shape, so a rotation
// reshapes the window and a person's own resize is left alone otherwise.
// Each sizeTo(box, around) passes the box it fitted and the space around it.
export function mount(win, deps) {
  const active = isPeek(win.location && win.location.search);
  let sizedFor = '';

  function offered() {
    if (active) return false;
    try {
      return !!(win.matchMedia && win.matchMedia('(hover: hover) and (pointer: fine)').matches);
    } catch (_) {
      return false;
    }
  }

  function chrome() {
    return {
      width: (win.outerWidth || 0) - (win.innerWidth || 0),
      height: (win.outerHeight || 0) - (win.innerHeight || 0),
    };
  }

  function avail() {
    const s = win.screen || {};
    return { width: s.availWidth || 0, height: s.availHeight || 0 };
  }

  function open() {
    const target = deps.targetId();
    // A phone-shaped guess until the peek knows its bezel and resizes
    // itself. The opener's chrome stands in for the pop-up's, which has no
    // tab strip but is near enough for a first size.
    const size = fitWindow({ width: 9, height: 19.5 }, avail(), chrome()) || { width: 420, height: 860 };
    const w = win.open(peekURL(win.location.origin, target), windowName(target),
      `popup,width=${size.width},height=${size.height}`);
    if (!w) {
      deps.report({ event: 'peek-open', target, outcome: 'blocked' });
      throw new Error('The browser blocked the window');
    }
    try { w.focus(); } catch (_) {}
    deps.report({ event: 'peek-open', target, outcome: 'ok' });
  }

  // `around` is what the page keeps beside the bezel, {width, height} in
  // CSS pixels: app.js knows it, this module does not.
  function sizeTo(box, around) {
    if (!active || !box) return;
    // Keyed by shape, not size: the bezel fetched after the first frame
    // reshapes the window once, a rotation once more, and nothing else.
    const key = (box.width / box.height).toFixed(3);
    if (key === sizedFor) return;
    const c = chrome();
    const extra = around || {};
    const size = fitWindow(box, avail(), {
      width: c.width + Math.max(0, extra.width || 0),
      height: c.height + Math.max(0, extra.height || 0),
    });
    if (!size) return;
    sizedFor = key;
    try {
      win.resizeTo(size.width, size.height);
    } catch (_) {
      // A window the page did not open cannot be resized. Peek still
      // works there, letterboxed.
    }
    deps.report({ event: 'peek-size', w: size.width, h: size.height });
  }

  return { active, offered, open, sizeTo };
}
