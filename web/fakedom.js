// A minimal fake DOM for testing the mount() halves of app.js's panel
// modules (corners.js, appcard.js, devicesettings.js, inspect.js) in plain
// Node, with no browser and no dependency. Not a test file itself --
// run-tests.mjs only picks up *.test.mjs, so this is safe to import from
// several of them without being run as one.
//
// Built for the review's finding that mutating the mount() halves left 18 of
// 28 mutations undetected: the pure-function tests these modules already had
// (nodeAt, applyResponse, formatApp, ...) cover the logic but never exercise
// the DOM wiring that calls them -- a 501 that fails to hide a section, a
// PUT body with the wrong shape, a focus() that never happens, all pass a
// pure-function suite cleanly. This is deliberately not a full CSS engine:
// it implements exactly the selector forms these four modules use
// (#id, tag, .class, [attr], [attr=value], and simple ANDed combinations of
// those), not the general case.

class ClassList {
  constructor(el) { this.el = el; }
  _set() {
    return new Set((this.el.className || '').split(/\s+/).filter(Boolean));
  }
  add(...names) {
    const s = this._set();
    for (const n of names) s.add(n);
    this.el.className = Array.from(s).join(' ');
  }
  remove(...names) {
    const s = this._set();
    for (const n of names) s.delete(n);
    this.el.className = Array.from(s).join(' ');
  }
  contains(name) { return this._set().has(name); }
  toggle(name, force) {
    const has = this.contains(name);
    const want = force === undefined ? !has : force;
    if (want) this.add(name); else this.remove(name);
    return want;
  }
}

// dataset.foo <-> attribute data-foo, both directions, the same as a real
// element's DOMStringMap -- camelCase keys, kebab-case attribute names.
// Found while writing sheet.test.mjs's showSection/hideSection coverage
// (review round 3): sheet.js sets `section.dataset.section = id` directly
// (never setAttribute), and the plain `{}` this used to be left
// `querySelector('[data-section="x"]')` unable to find it, since attrs and
// dataset were two unconnected stores.
function camelToKebab(k) { return k.replace(/[A-Z]/g, (c) => '-' + c.toLowerCase()); }
function kebabToCamel(k) { return k.replace(/-([a-z])/g, (_, c) => c.toUpperCase()); }
function makeDataset(el) {
  return new Proxy({}, {
    get(_t, prop) {
      if (typeof prop !== 'string') return undefined;
      return el.attrs['data-' + camelToKebab(prop)];
    },
    set(_t, prop, value) {
      if (typeof prop === 'string') el.attrs['data-' + camelToKebab(prop)] = String(value);
      return true;
    },
    deleteProperty(_t, prop) {
      if (typeof prop === 'string') delete el.attrs['data-' + camelToKebab(prop)];
      return true;
    },
    has(_t, prop) {
      return typeof prop === 'string' && ('data-' + camelToKebab(prop)) in el.attrs;
    },
    ownKeys(_t) {
      return Object.keys(el.attrs).filter((k) => k.startsWith('data-')).map((k) => kebabToCamel(k.slice(5)));
    },
    getOwnPropertyDescriptor(_t, prop) {
      if (typeof prop !== 'string') return undefined;
      const key = 'data-' + camelToKebab(prop);
      if (!(key in el.attrs)) return undefined;
      return { enumerable: true, configurable: true, value: el.attrs[key] };
    },
  });
}

export class FakeElement {
  constructor(tag) {
    this.tagName = String(tag || '').toUpperCase();
    this.tag = this.tagName;
    this.children = [];
    this.parentNode = null;
    this.attrs = {};
    this.dataset = makeDataset(this);
    // Custom properties land as plain keys, so a test can read
    // el.style['--depth'] back.
    this.style = {
      setProperty(k, v) { this[k] = String(v); },
      removeProperty(k) { delete this[k]; },
    };
    this.listeners = {};
    this.hidden = false;
    this.disabled = false;
    this.checked = false;
    this.value = '';
    this._text = '';
    this._rect = { left: 0, top: 0, width: 0, height: 0 };
    this.classList = new ClassList(this);
    this.className = '';
  }

  set innerHTML(_v) { this.children = []; }
  // Minimal serialization -- enough for tests to assert "something/nothing
  // rendered" and to check text content. Not a real HTML parser/serializer.
  get innerHTML() {
    const render = (n) => {
      if (n.children.length) return n.children.map(render).join('');
      const tag = n.tagName.toLowerCase();
      return `<${tag}>${n._text || ''}</${tag}>`;
    };
    return this.children.map(render).join('');
  }

  set textContent(v) { this._text = String(v); this.children = []; }
  get textContent() {
    if (this.children.length) return this.children.map((c) => c.textContent).join('');
    return this._text;
  }

  append(...kids) { for (const k of kids) this.appendChild(k); }

  appendChild(child) {
    child.parentNode = this;
    this.children.push(child);
    return child;
  }
  // append takes strings too, as a text child, like the real one.
  append(...nodes) {
    for (const n of nodes) {
      if (typeof n === 'string') { const t = new FakeElement('#text'); t._text = n; this.appendChild(t); } else this.appendChild(n);
    }
  }
  replaceChildren(...nodes) {
    for (const c of this.children) c.parentNode = null;
    this.children = [];
    this.append(...nodes);
  }
  removeChild(child) {
    this.children = this.children.filter((c) => c !== child);
    if (child.parentNode === this) child.parentNode = null;
    return child;
  }
  remove() { if (this.parentNode) this.parentNode.removeChild(this); }

  setAttribute(k, v) {
    this.attrs[k] = String(v);
    if (k === 'id') this.id = String(v);
    if (k === 'class') this.className = String(v);
  }
  getAttribute(k) {
    if (k === 'id') return this.id || null;
    if (k === 'class') return this.className || null;
    return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null;
  }
  removeAttribute(k) { delete this.attrs[k]; }
  hasAttribute(k) { return this.getAttribute(k) != null; }

  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  removeEventListener(type, fn) {
    if (!this.listeners[type]) return;
    this.listeners[type] = this.listeners[type].filter((f) => f !== fn);
  }
  dispatch(type, ev = {}) {
    const event = { type, target: this, preventDefault() {}, stopPropagation() {}, ...ev };
    for (const fn of (this.listeners[type] || []).slice()) fn(event);
    return event;
  }

  contains(other) {
    let n = other;
    while (n) { if (n === this) return true; n = n.parentNode; }
    return false;
  }

  focus() { setActiveElement(this); }
  blur() { if (activeElement === this) setActiveElement(fakeDocument.body); }

  getBoundingClientRect() { return this._rect; }

  all() {
    const out = [this];
    for (const c of this.children) out.push(...c.all());
    return out;
  }

  // --- minimal selector matching --------------------------------------
  _matchesSimple(sel) {
    if (sel === '*') return true;
    if (sel.startsWith('#')) return this.id === sel.slice(1);
    if (sel.startsWith('.')) return this.classList.contains(sel.slice(1));
    if (sel.startsWith('[')) {
      const inner = sel.slice(1, -1);
      const eq = inner.indexOf('=');
      if (eq === -1) return this.hasAttribute(inner);
      const key = inner.slice(0, eq);
      let val = inner.slice(eq + 1);
      val = val.replace(/^["']|["']$/g, '');
      return this.getAttribute(key) === val;
    }
    return this.tagName === sel.toUpperCase();
  }
  // A compound selector like '.cornerBtn[data-slot]' or 'input[type="range"]'
  // is a sequence of simple selectors, all of which must match this element.
  matches(compound) {
    const parts = compound.match(/(#[\w-]+|\.[\w-]+|\[[^\]]+\]|[a-zA-Z][\w-]*)/g) || [];
    return parts.every((p) => this._matchesSimple(p));
  }

  querySelectorAll(sel) {
    const out = [];
    for (const c of this.children) {
      out.push(...c.all().filter((e) => e !== this && e.matches(sel)));
    }
    return out;
  }
  querySelector(sel) {
    return this.querySelectorAll(sel)[0] || null;
  }
}

let activeElement = null;
function setActiveElement(el) { activeElement = el; }

const documentListeners = {};

export const fakeDocument = {
  createElement(tag) { return new FakeElement(tag); },
  createElementNS(_ns, tag) { return new FakeElement(tag); },
  addEventListener(type, fn) { (documentListeners[type] ||= []).push(fn); },
  removeEventListener(type, fn) {
    if (!documentListeners[type]) return;
    documentListeners[type] = documentListeners[type].filter((f) => f !== fn);
  },
  dispatch(type, ev = {}) {
    const event = { type, preventDefault() {}, stopPropagation() {}, ...ev };
    for (const fn of (documentListeners[type] || []).slice()) fn(event);
    return event;
  },
  get activeElement() { return activeElement || fakeDocument.body; },
};
fakeDocument.body = new FakeElement('body');
fakeDocument.documentElement = new FakeElement('html');
setActiveElement(fakeDocument.body);

// installFakeDom() -> installs `document` as a global (module mount()s under
// test read the bare `document` global, the same as app.js does in a real
// page) and returns a `reset()` to clear per-test state (focus, document
// listeners) between tests in the same process, since document listeners
// registered by one mount() call would otherwise leak into the next test's
// key events.
export function installFakeDom() {
  globalThis.document = fakeDocument;
  return function reset() {
    for (const k of Object.keys(documentListeners)) delete documentListeners[k];
    setActiveElement(fakeDocument.body);
  };
}

export function tick(n = 1) {
  let p = Promise.resolve();
  for (let i = 0; i < n; i++) p = p.then(() => new Promise((r) => setTimeout(r, 0)));
  return p;
}

export function deferred() {
  let resolve;
  const promise = new Promise((r) => { resolve = r; });
  return { promise, resolve };
}

export function jsonResponse(status, body) {
  return {
    status,
    ok: status >= 200 && status < 300,
    json: async () => body,
    text: async () => JSON.stringify(body),
  };
}
