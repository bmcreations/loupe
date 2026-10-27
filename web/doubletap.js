// Double-tap on the blank parts of the stage: enters immersive mode from the
// bezel or the letterbox, and leaves it from the letterbox.
//
// It only ever acts on a tap whose target is #stage itself or #screenWrap
// itself. The canvas, the touch and inspect layers, the corner buttons, the
// sheets and the handle are all other elements, so a double-tap on the device
// or on a control falls through untouched: this module listens and never
// preventDefaults, and the canvas keeps every pointer event it had before.
// The frame art layers are pointer-events:none, so a tap on the bezel lands on
// #screenWrap.
//
// Touch is the primary input and iOS gives a PWA with user-scalable=no no
// dblclick worth relying on, so a double-tap is two timed pointerups, not a
// dblclick.
export const GAP_MS = 300; // longest gap from the first tap's release to the second's
export const SLOP_PX = 24; // furthest the second tap may land from the first
export const MOVE_PX = 10; // a press that moves further than this was a drag
export const TAP_MS = 300; // a press held longer than this was a long press

// createDetector() -> {tap(t), reset()}. Pure: it is told about completed
// taps ({t, x, y, type, zone}) and answers whether this one finished a double.
// After a double the pair is spent, so a third tap starts a new one.
export function createDetector({ gapMs = GAP_MS, slopPx = SLOP_PX } = {}) {
  let prev = null;
  return {
    tap(cur) {
      const p = prev;
      if (
        p &&
        p.type === cur.type &&
        p.zone === cur.zone &&
        cur.t - p.t <= gapMs &&
        Math.hypot(cur.x - p.x, cur.y - p.y) <= slopPx
      ) {
        prev = null;
        return true;
      }
      prev = cur;
      return false;
    },
    reset() { prev = null; },
  };
}

// mount(stage, deps) wires the gesture on #stage (pointer events from the
// canvas and everything else inside it bubble there, and are only observed).
//
//   deps:
//     wrap          #screenWrap; a double-tap on it enters immersive.
//     isBezel(t)    in place of wrap: whether t is a pane's bezel, any pane.
//     isImmersive() whether immersive mode is on.
//     enter(via) / exit(via)   called with 'double-tap'.
//     now()         optional clock, for tests; defaults to performance.now().
export function mount(stage, deps) {
  const now = deps.now || (() => performance.now());
  const det = createDetector();
  const down = new Map(); // pointerId -> {t, x, y, moved}
  let tainted = false; // a second finger came down: nothing until all are up

  function zoneOf(target) {
    if (target === stage) return 'stage';
    const bezel = deps.isBezel ? deps.isBezel(target) : target === deps.wrap;
    if (bezel && !deps.isImmersive()) return 'bezel';
    return null;
  }

  stage.addEventListener('pointerdown', (ev) => {
    if (down.size > 0) {
      tainted = true;
      det.reset();
    }
    down.set(ev.pointerId, { t: now(), x: ev.clientX, y: ev.clientY, moved: false });
  });

  stage.addEventListener('pointermove', (ev) => {
    const d = down.get(ev.pointerId);
    if (d && Math.hypot(ev.clientX - d.x, ev.clientY - d.y) > MOVE_PX) {
      d.moved = true;
      det.reset();
    }
  });

  stage.addEventListener('pointercancel', (ev) => {
    down.delete(ev.pointerId);
    if (down.size === 0) tainted = false;
    det.reset();
  });

  stage.addEventListener('pointerup', (ev) => {
    const d = down.get(ev.pointerId);
    down.delete(ev.pointerId);
    const wasTainted = tainted;
    if (down.size === 0) tainted = false;
    if (!d) return;
    const t = now();
    const zone = zoneOf(ev.target);
    if (wasTainted || d.moved || t - d.t > TAP_MS || !zone) {
      det.reset();
      return;
    }
    const x = ev.clientX;
    const y = ev.clientY;
    if (!det.tap({ t, x, y, type: ev.pointerType, zone })) return;
    if (deps.isImmersive()) {
      if (zone === 'stage') deps.exit('double-tap');
    } else {
      deps.enter('double-tap');
    }
  });
}
