// Extension overlays: an image and labelled boxes an extension has put over
// the target's screen with POST /api/overlays (internal/httpapi/overlay.go),
// such as a Figma frame from spec-compare. The server sends the whole set as
// MsgOverlay each time it changes and after every init, so set() replaces
// what is drawn rather than patching it.
//
// Placement is fractions of the screen, as cursors are, so it lands on the
// canvas at whatever size it is drawn. The image is fetched over HTTP with
// the page's cookie, never sent on the session stream (trap 51).
//
// A person compares in one of three ways: fade (the reference at an
// opacity), swipe (the reference on the left of a divider they drag), and
// difference (a blend that leaves matching pixels black). The choice is
// per page, not per overlay: it is how they like to look, not what they
// are looking at.

export const MODES = ['fade', 'swipe', 'difference'];

// modeTitle(mode) -> the segment's label.
export function modeTitle(m) {
  return m === 'swipe' ? 'Swipe' : m === 'difference' ? 'Difference' : 'Fade';
}

// parseOverlays(body) -> the overlays in a MsgOverlay body, dropping any
// entry a client cannot place. Throws on JSON that is not an object.
export function parseOverlays(text) {
  const doc = JSON.parse(text);
  if (!doc || typeof doc !== 'object') throw new Error('overlay body is not an object');
  const list = Array.isArray(doc.overlays) ? doc.overlays : [];
  return list.filter((o) => o && typeof o.id === 'string' && validRect(o.rect));
}

function validRect(r) {
  return Array.isArray(r) && r.length === 4 && r.every((v) => Number.isFinite(v))
    && r[2] > r[0] && r[3] > r[1];
}

// rectStyle(rect) -> CSS left/top/width/height in percent of the screen.
export function rectStyle(r) {
  const pct = (v) => `${+(v * 100).toFixed(4)}%`;
  return { left: pct(r[0]), top: pct(r[1]), width: pct(r[2] - r[0]), height: pct(r[3] - r[1]) };
}

// imageStyle(mode, opacity, split) -> how the reference image is drawn.
// split is the swipe divider's position across the image, 0..1.
export function imageStyle(mode, opacity, split) {
  const o = Math.min(1, Math.max(0, Number.isFinite(opacity) ? opacity : 0.5));
  switch (mode) {
    case 'swipe': {
      const s = Math.min(1, Math.max(0, split));
      return { opacity: '1', mixBlendMode: 'normal', clipPath: `inset(0 ${+((1 - s) * 100).toFixed(4)}% 0 0)` };
    }
    case 'difference':
      return { opacity: '1', mixBlendMode: 'difference', clipPath: 'none' };
    default:
      return { opacity: String(o), mixBlendMode: 'normal', clipPath: 'none' };
  }
}

// mount(layer, deps) draws overlays in layer, which covers the pane's
// screenWrap. deps: { canvas, storage? }.
export function mount(layer, deps) {
  const doc = layer.ownerDocument || globalThis.document;
  const store = deps.storage;
  let overlays = [];
  let mode = read('overlayMode', 'fade');
  if (!MODES.includes(mode)) mode = 'fade';
  let opacity = Number(read('overlayOpacity', 'NaN'));
  let split = 0.5;
  let hidden = false;

  const screen = doc.createElement('div');
  screen.className = 'extScreen';
  layer.appendChild(screen);
  const bar = doc.createElement('div');
  bar.className = 'extBar';
  layer.appendChild(bar);

  function read(k, d) {
    try { return store?.getItem(k) ?? d; } catch (_) { return d; }
  }
  function write(k, v) {
    try { store?.setItem(k, String(v)); } catch (_) {}
  }

  // The screen box follows the canvas, which a bezel can inset in the wrap.
  function place() {
    const c = deps.canvas.getBoundingClientRect();
    const l = layer.getBoundingClientRect();
    Object.assign(screen.style, {
      left: `${c.left - l.left}px`, top: `${c.top - l.top}px`,
      width: `${c.width}px`, height: `${c.height}px`,
    });
  }

  function draw() {
    screen.replaceChildren();
    layer.hidden = overlays.length === 0;
    if (layer.hidden) return;
    place();
    for (const o of overlays) {
      if (o.image && !hidden) {
        const img = doc.createElement('img');
        img.className = 'extImage';
        img.alt = '';
        img.src = o.image;
        Object.assign(img.style, rectStyle(o.rect),
          imageStyle(mode, Number.isFinite(opacity) ? opacity : o.opacity, split));
        screen.appendChild(img);
        if (mode === 'swipe') {
          const line = doc.createElement('div');
          line.className = 'extDivider';
          line.style.left = `${+((o.rect[0] + (o.rect[2] - o.rect[0]) * split) * 100).toFixed(4)}%`;
          line.addEventListener('pointerdown', (e) => drag(e, o));
          screen.appendChild(line);
        }
      }
      if (hidden) continue;
      for (const b of o.boxes || []) {
        if (!validRect(b.rect)) continue;
        const box = doc.createElement('div');
        box.className = `extBox${b.kind ? ` kind-${b.kind.replace(/[^a-z0-9-]/gi, '')}` : ''}`;
        Object.assign(box.style, rectStyle(b.rect));
        if (b.label) {
          const tag = doc.createElement('span');
          tag.className = 'extBoxLabel';
          tag.textContent = b.label;
          box.appendChild(tag);
        }
        screen.appendChild(box);
      }
    }
    drawBar();
  }

  function drawBar() {
    bar.replaceChildren();
    const o = overlays.find((x) => x.image) || overlays[0];
    const title = doc.createElement('span');
    title.className = 'extTitle';
    title.textContent = o.label || o.source;
    bar.appendChild(title);
    if (overlays.some((x) => x.image)) {
      const seg = doc.createElement('div');
      seg.className = 'extSeg';
      seg.setAttribute('role', 'radiogroup');
      seg.setAttribute('aria-label', 'Compare by');
      for (const m of MODES) {
        const b = doc.createElement('button');
        b.type = 'button';
        b.className = 'extSegBtn';
        b.setAttribute('role', 'radio');
        b.setAttribute('aria-checked', String(m === mode));
        b.textContent = modeTitle(m);
        b.addEventListener('click', () => {
          if (m === mode) return;
          mode = m;
          write('overlayMode', mode);
          draw();
        });
        seg.appendChild(b);
      }
      bar.appendChild(seg);
      if (mode === 'fade') {
        const range = doc.createElement('input');
        range.type = 'range';
        range.min = '0';
        range.max = '1';
        range.step = '0.05';
        range.value = String(Number.isFinite(opacity) ? opacity : o.opacity);
        range.setAttribute('aria-label', 'Reference opacity');
        range.addEventListener('input', () => {
          opacity = Number(range.value);
          write('overlayOpacity', opacity);
          for (const img of screen.querySelectorAll('.extImage')) img.style.opacity = range.value;
        });
        bar.appendChild(range);
      }
    }
    const hide = doc.createElement('button');
    hide.type = 'button';
    hide.textContent = hidden ? 'Show' : 'Hide';
    hide.addEventListener('click', () => { hidden = !hidden; draw(); });
    bar.appendChild(hide);
  }

  function drag(e, o) {
    e.preventDefault();
    e.stopPropagation();
    const target = e.currentTarget;
    target.setPointerCapture?.(e.pointerId);
    const move = (ev) => {
      const r = screen.getBoundingClientRect();
      const x = (ev.clientX - r.left) / r.width;
      split = Math.min(1, Math.max(0, (x - o.rect[0]) / (o.rect[2] - o.rect[0])));
      const s = imageStyle('swipe', 1, split);
      for (const img of screen.querySelectorAll('.extImage')) img.style.clipPath = s.clipPath;
      target.style.left = `${+((o.rect[0] + (o.rect[2] - o.rect[0]) * split) * 100).toFixed(4)}%`;
    };
    const up = () => {
      target.removeEventListener('pointermove', move);
      target.removeEventListener('pointerup', up);
      target.removeEventListener('pointercancel', up);
    };
    target.addEventListener('pointermove', move);
    target.addEventListener('pointerup', up);
    target.addEventListener('pointercancel', up);
  }

  const ro = globalThis.ResizeObserver ? new ResizeObserver(() => { if (!layer.hidden) place(); }) : null;
  ro?.observe(deps.canvas);
  layer.hidden = true;

  return {
    // handleMessage(body) takes a MsgOverlay body as bytes or text.
    handleMessage(body) {
      const text = typeof body === 'string' ? body : new TextDecoder().decode(body);
      overlays = parseOverlays(text);
      draw();
    },
    // clear() drops what is drawn: the pane is leaving this target, and the
    // next target's init brings its own set.
    clear() {
      overlays = [];
      draw();
    },
    get count() { return overlays.length; },
  };
}
