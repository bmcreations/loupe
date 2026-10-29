// The nav row (bottom-left corner by default) and the overflow corner
// (bottom-right by default) -- design spec "Layout" section. Handedness
// mirrors both corners through the `hand-right` class in style.css; this module only decides WHAT sits in each corner, never WHERE
// it sits on the page.
//
// navKeys is exported as a pure function with no DOM in it at all, so a
// later Kotlin client could drive the same three-slot layout from the same
// OS string. mount() is the DOM half: it owns #cornerNav and #cornerMore the
// way sheet.js and drawer.js own their own roots, and everything it needs
// from outside that subtree -- sendKey, the shared #toast, the client's
// report(), closing the drawer/sheet -- comes in through deps rather than
// being imported.

import { showToast } from './toast.js';

// The nav row's three grid slots are named for the L it replaced (see
// #cornerNav's grid-template-areas in style.css), not for what fills them:
// upright was the tall stroke's top and is now the corner end of the row,
// elbow is the middle, floor is the inner end. Both OS layouts put the same physical action (going
// home) at the elbow -- the design spec's table read as data:
//
//   | Position | Android | iOS          |
//   | upright  | Back    | App switcher |
//   | elbow    | Home    | Home         |
//   | floor    | Recents | Lock         |
//
// Android keys carry a keycode and go over the stream as MsgKey down/up
// (sendKey in app.js, instant, today's path). AKEYCODE_BACK=4, HOME=3,
// APP_SWITCH=187 -- internal/target/rotate.go's androidButtons table (it
// also carries "lock"=26, which the Android *nav row* does not use; lock is
// the iOS floor slot instead, per the table above).
//
// iOS keys carry no keycode: there is no MsgKey wire format for an OS scrcpy
// never targets, so they go through POST /api/input {"action":"button"}
// instead (design spec line 225), which the iOS target will implement. No
// iOS target exists yet, so that branch is exercised by this module's unit
// test only -- see the commit body, and do not add a dev switch to force it
// live.
const ANDROID_KEYS = [
  { slot: 'upright', button: 'back', label: 'Back', keycode: 4 },
  { slot: 'elbow', button: 'home', label: 'Home', keycode: 3 },
  { slot: 'floor', button: 'recents', label: 'Recents', keycode: 187 },
];

const IOS_KEYS = [
  { slot: 'upright', button: 'appSwitcher', label: 'App Switcher' },
  { slot: 'elbow', button: 'home', label: 'Home' },
  { slot: 'floor', button: 'lock', label: 'Lock' },
];

// navKeys(os) -> the three-slot layout for that OS, or [] for synthetic, an
// unrecognised OS, or no OS at all (a target with no `os` field, or none
// selected yet). An empty array is what tells the caller to hide the L
// outright rather than show an empty grid.
export function navKeys(os) {
  switch (os) {
    case 'android': return ANDROID_KEYS;
    case 'ios': return IOS_KEYS;
    default: return [];
  }
}

// errorMessage normalises whatever a rejected promise or a thrown value
// carries into one string, the same coercion app.js's own catch blocks do by
// hand (String((err && err.message) || err)) -- pulled out here so the toast
// and the report() payload always agree on the text, and so it is testable
// without a DOM.
export function errorMessage(err) {
  if (err && typeof err.message === 'string' && err.message) return err.message;
  return String(err);
}

// shouldDismissMenu decides whether a pointerdown seen in the capture phase
// should close the overflow menu. It listens on the document in the capture
// phase not because a captured pointer swallows anything -- per spec, a
// captured pointer's later events still bubble through the same ancestor
// chain a normal one would, and app.js's canvas handler never calls
// stopPropagation, so a bubble-phase listener here would see the same
// pointerdown just as reliably. The reason is ordering, not visibility: a
// bubble-phase listener on document fires *after* the target's own bubble
// listener (canvas's pointerdown, which starts a device gesture via
// setPointerCapture), so the dismiss decision would run one step behind
// whatever the tap's target already did with it. Capture phase runs before
// any bubble listener on the path, target included, which is what makes the
// dismiss check unconditional rather than a race against whatever else is
// listening.
//
// menuEl/toggleEl are Element-shaped (anything with a `contains` method), not
// necessarily real DOM nodes, so this is testable with plain fake objects.
export function shouldDismissMenu({ open, menuEl, toggleEl, target }) {
  if (!open) return false;
  if (menuEl && menuEl.contains(target)) return false;
  if (toggleEl && toggleEl.contains(target)) return false;
  return true;
}

// Minimal inline icons, 24x24 viewBox to match .cornerBtn/.moreMenuItem svg's
// sizing rule in style.css (stroke: currentColor, no fill). Shapes are
// recognisable rather than exact -- there is no icon set vendored here and
// none of this is shipping design work.
const ICONS = {
  back: '<path d="M14 6l-6 6 6 6"/>',
  home: '<path d="M4 11l8-6 8 6"/><path d="M6 10v9h12v-9"/>',
  recents: '<rect x="5" y="4" width="10" height="10" rx="2"/><rect x="9" y="10" width="10" height="10" rx="2"/>',
  appSwitcher: '<rect x="4" y="4" width="7" height="7" rx="1.5"/><rect x="13" y="4" width="7" height="7" rx="1.5"/><rect x="4" y="13" width="7" height="7" rx="1.5"/><rect x="13" y="13" width="7" height="7" rx="1.5"/>',
  lock: '<rect x="5" y="10" width="14" height="10" rx="2"/><path d="M8 10V7a4 4 0 018 0v3"/>',
  screenshot: '<rect x="4" y="7" width="16" height="12" rx="2"/><circle cx="12" cy="13" r="3"/><path d="M9 7l1.2-2h3.6L15 7"/>',
  rotate: '<path d="M3 12a9 9 0 1 1 2.6 6.4"/><path d="M3 21v-6h6"/>',
  inspect: '<circle cx="11" cy="11" r="6"/><path d="M20 20l-4.3-4.3"/>',
  fullscreen: '<path d="M4 9V4h5M20 9V4h-5M4 15v5h5M20 15v5h-5"/>',
  flag: '<path d="M6 21V4"/><path d="M6 5h11l-2.5 4L17 13H6"/>',
  record: '<circle cx="12" cy="12" r="8"/><circle cx="12" cy="12" r="4" fill="currentColor"/>',
  peek: '<rect x="7" y="2" width="10" height="20" rx="2"/><path d="M11 18h2"/>',
  pip: '<rect x="3" y="5" width="18" height="14" rx="2"/><rect x="12" y="11" width="7" height="6" rx="1"/>',
  clipboard: '<rect x="6" y="5" width="12" height="16" rx="2"/><path d="M9 5V3h6v2"/><path d="M9 11h6M9 15h4"/>',
  touches: '<circle cx="12" cy="12" r="3"/><circle cx="12" cy="12" r="7" opacity=".5"/>',
  link: '<path d="M10 14a4 4 0 005.7 0l3-3a4 4 0 00-5.7-5.7l-1 1"/><path d="M14 10a4 4 0 00-5.7 0l-3 3a4 4 0 005.7 5.7l1-1"/>',
  zoom: '<circle cx="11" cy="11" r="6"/><path d="M20 20l-4.3-4.3M11 8v6M8 11h6"/>',
  check: '<path d="M5 12.5l4.5 4.5L19 7.5"/>',
};

// pageLink is this page's URL narrowed to one target, the form panes.js's
// parseTargets reads back. It carries no token: whoever opens it signs in.
export function pageLink(origin, targetId) {
  if (!targetId) return null;
  return `${origin}/?target=${encodeURIComponent(targetId)}`;
}

function svgIcon(name) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('aria-hidden', 'true');
  svg.innerHTML = ICONS[name] || '';
  return svg;
}

// mount(nav, more, deps) wires the nav row into `nav` (#cornerNav, already
// holding three <button data-slot="..."> per index.html) and the overflow
// button+menu into `more` (#cornerMore, holding #moreBtn and #moreMenu).
//
// deps:
//   sendAndroidKey(keycode) -- presses and releases an Android key over the
//     live stream. Must throw/reject when it cannot act (e.g. no open
//     session) so a failure has something to report; the raw MsgKey path in
//     app.js is otherwise a silent no-op, so app.js wraps sendKey() to give
//     it that signal rather than this module reaching into transport state.
//   pressIOSButton(name) -- POST /api/input {"action":"button",...}, for the
//     OS with no MsgKey path. Unexercised until an iOS target exists.
//   screenshot() -- the existing deviceShot/canvasShot/deliver flow
//     (app.js's takeScreenshot). It already reports its own failure as
//     kind:"client" (screenshot-error), so this module shows the toast
//     without reporting a second time for that one action.
//   rotate() -- POST /api/input {"action":"rotate"} with no "to". See
//     app.js's rotateDevice() for why "to" is deliberately omitted rather
//     than computed here.
//   inspect -- optional {label, run()}. Left out until Task 9 exists; the
//     overflow simply has two items instead of three until then.
//   fullScreen() -- optional; enters immersive mode (immersive.js). The
//     item is left out without it.
//   pip -- optional {label(), run()}, from pip.js, present only behind
//     ?pip=1 on a browser that can float a video. run() enters or leaves
//     Picture in Picture and must run inside the tap; label() names which,
//     re-read each time the menu opens.
//   peek -- optional {offered(), open()}, from peek.js. The item shows
//     only on a Mac (a fine pointer that hovers) and opens this target in a
//     window of its own; open() must run inside the tap.
//   keyboard -- optional {offered(), isOpen(), open(), close(),
//     onChange(fn)}, from keyboard.js. #kbdBtn, beside the overflow button,
//     shows while offered() -- re-read on every setOS and setControl, so it
//     follows a target switch and a session dropping -- and raises or
//     dismisses the phone keyboard. It sits in this corner rather than the
//     nav row so the row keeps its reserved size; two buttons here are
//     narrower than the --corner-w the row needs.
//   clipboard -- optional {offered(), paste(), copy()}, from clipboard.js.
//     "Paste to device" and "Copy from device" show while offered(), re-read
//     each time the menu opens; both actions run inside the tap, which is
//     what lets Safari reach the clipboard.
//   flag -- optional {run()}, from flag.js. "Flag for agent" enters flag mode
//     on the focused pane.
//   record -- optional {label(), toggle(), onChange(fn)}, from record.js.
//     "Record" after Screenshot, relabelled "Stop Recording" through
//     onChange while this page's recording runs.
//   link -- optional {url(), write(text)}. "Copy Link": url() is the focused
//     target's page URL without the token (null with no target), write()
//     puts it on the clipboard, and "Link copied" is flashed.
//   zoom -- optional {enter()}, from zoom.js. A "Zoom" item that starts zoom
//     mode.
//   touches -- optional {pref(), setPref(on)}, from touches.js. A "Show
//     touches" switch on every target, its label re-read each open.
//   toast -- the shared #toast element. Shown through toast.js's showToast,
//     which owns the one timer on that element -- see toast.js's own
//     comment for why a per-module timer here raced app.js's and
//     inspect.js's (client Task 9 review, MEDIUM-9).
//   report(rec) -- app.js's report(), so a failure is traced as kind:"client"
//     the same way screenshot-error and friends already are.
//   closeOtherPanels() -- optional; closes the drawer/sheet if either is
//     open. Called before the overflow opens, so it never opens on top of a
//     scrim it does not own (see the module-level note on scrim coexistence
//     below).
//
// Returns {setOS(os), setControl(hasControl), closeMenu(), isMenuOpen(),
// refreshKeyboard()}.
// app.js calls setOS() on every target switch (loadTargets, onPickTarget)
// since the L depends on the selected target's OS, setControl() from
// onPaneInit with MsgInit's info.control (the L stays hidden without it,
// same as the pre-redesign nav bar -- see setControl's own comment for why
// this module owns the AND of the two rather than app.js), and closeMenu()
// from wherever the drawer or the sheet open.
//
// Scrim coexistence: the overflow menu never touches #scrim itself -- it is
// a small dropdown, not a panel the drawer/sheet pattern applies to. Two
// rules keep it from fighting the two that do:
//   1. Opening the overflow calls closeOtherPanels() first, so a drawer or
//      sheet left open behind it is closed (and its own close() hides the
//      scrim) rather than sitting open under a menu with no way back to it.
//   2. app.js calls closeMenu() from the drawer's open call site and the
//      sheet's onOpen, so the reverse never happens either.
export function mount(nav, more, deps) {
  const toggleBtn = more.querySelector('#moreBtn') || more.querySelector('button');
  const menu = more.querySelector('#moreMenu');
  const navButtons = Array.from(nav.querySelectorAll('.cornerBtn[data-slot]'));

  // ARIA: a menu button controlling a popup menu, per the WAI-ARIA menu
  // button pattern -- aria-controls ties the button to the menu it opens
  // (menu.id is trusted to exist; index.html gives #moreMenu one), and
  // aria-expanded (already toggled by openMenu below) is what a screen
  // reader uses to announce open/closed state.
  if (menu.id) toggleBtn.setAttribute('aria-controls', menu.id);
  menu.setAttribute('role', 'menu');

  let menuOpen = false;

  // --- Keyboard button --------------------------------------------------------

  const kbdBtn = more.querySelector('#kbdBtn');
  function refreshKeyboardButton() {
    if (!kbdBtn) return;
    if (!deps.keyboard) { kbdBtn.hidden = true; return; }
    const offered = deps.keyboard.offered();
    kbdBtn.hidden = !offered;
    // A target that does not take typing takes the keyboard down with it. A
    // lost connection only hides the button: every reconnect drops control
    // until the next init, and closing there would dismiss the keyboard on
    // whoever is typing. onSession resends the hardware keyboard instead.
    const takes = deps.keyboard.takes ? deps.keyboard.takes() : offered;
    if (!takes && deps.keyboard.isOpen()) deps.keyboard.close();
    kbdBtn.setAttribute('aria-pressed', deps.keyboard.isOpen() ? 'true' : 'false');
  }
  if (kbdBtn && deps.keyboard) {
    // The field would lose focus to this tap before the click could ask
    // whether the keyboard is up, and the tap that dismisses it would
    // raise it again instead.
    kbdBtn.addEventListener('pointerdown', (ev) => ev.preventDefault());
    kbdBtn.addEventListener('click', (ev) => {
      ev.stopPropagation();
      openMenu(false);
      // Runs inside the click, which iOS needs before it raises a keyboard.
      try {
        if (deps.keyboard.isOpen()) deps.keyboard.close();
        else deps.keyboard.open();
      } catch (err) { fail(err, 'more', 'keyboard-error'); }
      refreshKeyboardButton();
    });
    if (deps.keyboard.onChange) deps.keyboard.onChange(refreshKeyboardButton);
  }

  function fail(err, corner, event) {
    const message = errorMessage(err);
    // Positions the shared #toast against whichever corner raised the error
    // via data-corner, which style.css mirrors under html.hand-right the
    // same way #cornerNav/#cornerMore already are. The timer itself is
    // toast.js's, shared with app.js and inspect.js's own calls, so two
    // toasts racing each other resolve to whichever fired last rather than
    // to whichever module's private setTimeout happened to fire first.
    if (deps.toast) showToast(deps.toast, message, corner);
    if (event && deps.report) deps.report({ event, err: message });
  }

  // --- Nav L ----------------------------------------------------------------

  function clearNav() {
    for (const btn of navButtons) {
      btn.hidden = true;
      btn.innerHTML = '';
      btn.onclick = null;
      btn.removeAttribute('aria-label');
      btn.removeAttribute('title');
    }
  }

  function press(key) {
    const action = typeof key.keycode === 'number'
      ? () => deps.sendAndroidKey(key.keycode)
      : () => deps.pressIOSButton(key.button);
    return Promise.resolve().then(action).catch((err) => fail(err, 'nav', 'nav-key-error'));
  }

  // The L is hidden unless BOTH hold: the stream reports control (MsgInit's
  // info.control -- false for synthetic and anything else session.go builds
  // with Control: t.Kind() != target.KindSynthetic) AND navKeys(os) is
  // non-empty (no recognised OS, or no target at all). Two independent
  // sources feed this -- onPaneInit calls setControl() on every (re)connect,
  // setOS() runs on every target switch, well before the first of those --
  // so the visibility decision lives here rather than as two call sites each
  // writing nav.hidden and racing the other's idea of it.
  let currentKeys = [];
  let currentControl = false;

  function updateNavVisibility() {
    nav.hidden = !currentControl || currentKeys.length === 0;
    refreshKeyboardButton();
  }

  // setOS rebuilds the L for the given OS, called on mount and on every
  // target switch. An empty navKeys() result (synthetic, unknown OS, no
  // target) leaves every slot hidden, which is what makes the L disappear
  // rather than show three buttons that do nothing.
  function setOS(os) {
    clearNav();
    currentKeys = navKeys(os);
    for (const key of currentKeys) {
      const btn = navButtons.find((b) => b.dataset.slot === key.slot);
      if (!btn) continue;
      btn.hidden = false;
      btn.appendChild(svgIcon(key.button));
      btn.setAttribute('aria-label', key.label);
      btn.title = key.label;
      btn.onclick = () => press(key);
    }
    updateNavVisibility();
  }

  // setControl reflects MsgInit's info.control. app.js's onPaneInit (from
  // pane.js's handleInit) calls this instead of writing nav.hidden itself,
  // so this module stays the one place the two conditions above are combined.
  function setControl(hasControl) {
    currentControl = hasControl;
    updateNavVisibility();
  }

  // --- Overflow menu ----------------------------------------------------------

  function openMenu(open) {
    if (open && !menuOpen && deps.closeOtherPanels) deps.closeOtherPanels();
    menuOpen = open;
    menu.classList.remove('pressing');
    if (open) { refreshClipboardItems(); refreshTouchesItem(); refreshPipItem(); }
    menu.hidden = !open;
    toggleBtn.setAttribute('aria-expanded', open ? 'true' : 'false');
    // Land focus inside the menu on open -- the WAI-ARIA menu button
    // pattern's own expectation for a keyboard activation, and the only way
    // ArrowDown/Up below have anything to act on immediately after open.
    // The last row is the first item, the one nearest the button.
    if (open) {
      const items = menuItems();
      if (items.length) items[items.length - 1].focus();
    }
  }

  function closeMenu() { if (menuOpen) openMenu(false); }
  function isMenuOpen() { return menuOpen; }

  // The menu is laid out the way iOS lays out a UIMenu: rows grouped into
  // sections with a wider gap between them, the label leading and the icon
  // trailing, and an on/off item shown as a checkmark rather than a switch.
  // Groups and items are added in priority order, and layoutMenu() puts the
  // first of each nearest the button. The button is at the bottom of the
  // screen, so that is the reverse of the order they were added in, which is
  // what UIMenu's automatic element order does for a menu that opens upwards:
  // the most used item sits under the thumb.
  const groups = [];
  function addGroup() {
    const el = document.createElement('div');
    el.className = 'moreMenuGroup';
    el.setAttribute('role', 'group');
    const group = { el, items: [] };
    groups.push(group);
    return group;
  }

  // hideOnPress fades the whole menu on pointerdown, before the click. An
  // item whose click raises a system callout needs it: Safari shows the
  // callout for clipboard.readText before painting, so the menu, already
  // hidden in the DOM by closeMenu, stayed on screen under "Paste". Opacity
  // rather than hidden, so the item still takes the click. readText cannot
  // be deferred instead: from a 50ms timer Safari refused it outright.
  function addMenuItem(group, iconName, label, onClick, opts = {}) {
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'moreMenuItem';
    btn.setAttribute('role', 'menuitem');
    const check = svgIcon('check');
    check.setAttribute('class', 'moreMenuCheck');
    btn.appendChild(check);
    const span = document.createElement('span');
    span.textContent = label;
    btn.appendChild(span);
    btn.appendChild(svgIcon(iconName));
    btn.addEventListener('click', () => {
      closeMenu();
      // closeMenu() hides the item this click landed on, so without this the
      // browser drops focus to <body> -- the same lost-place problem
      // Escape's own handler already fixes, just reached by activating an
      // item instead of dismissing the menu (review LOW-8).
      toggleBtn.focus();
      onClick();
    });
    if (opts.hideOnPress) {
      btn.addEventListener('pointerdown', () => menu.classList.add('pressing'));
      btn.addEventListener('pointercancel', () => menu.classList.remove('pressing'));
      // A lift off the item is no click. Touch captures the pointer to the
      // button, so the event's target says nothing; hit-test the lift point.
      btn.addEventListener('pointerup', (e) => {
        if (!btn.contains(document.elementFromPoint(e.clientX, e.clientY))) menu.classList.remove('pressing');
      });
    }
    group.items.push(btn);
    return btn;
  }

  function layoutMenu() {
    menu.innerHTML = '';
    for (const group of groups.slice().reverse()) {
      if (!group.items.length) continue;
      for (const item of group.items.slice().reverse()) group.el.appendChild(item);
      menu.appendChild(group.el);
    }
  }

  const actions = addGroup();
  addMenuItem(actions, 'screenshot', 'Screenshot', () => {
    // takeScreenshot() already reports its own failure (screenshot-error), so
    // this only needs to surface it as a toast, not report it a second time.
    Promise.resolve().then(deps.screenshot).catch((err) => fail(err, 'more', null));
  });
  if (deps.flag) {
    addMenuItem(actions, 'flag', 'Flag for agent', () => {
      try { deps.flag.run(); } catch (err) { fail(err, 'more', 'flag-error'); }
    });
  }
  if (deps.record) {
    const recordItem = addMenuItem(actions, 'record', deps.record.label(), () => {
      Promise.resolve().then(deps.record.toggle).catch((err) => fail(err, 'more', 'record-error'));
    });
    deps.record.onChange(() => { recordItem.querySelector('span').textContent = deps.record.label(); });
  }
  addMenuItem(actions, 'rotate', 'Rotate', () => {
    Promise.resolve().then(deps.rotate).catch((err) => fail(err, 'more', 'rotate-error'));
  });
  if (deps.fullScreen) {
    addMenuItem(actions, 'fullscreen', 'Full screen', () => {
      try { deps.fullScreen(); } catch (err) { fail(err, 'more', 'fullscreen-error'); }
    });
  }
  if (deps.peek && deps.peek.offered()) {
    addMenuItem(actions, 'peek', 'Open in a window', () => {
      try { deps.peek.open(); } catch (err) { fail(err, 'more', null); }
    });
  }
  let pipItem = null;
  if (deps.pip) {
    pipItem = addMenuItem(actions, 'pip', deps.pip.label(), () => {
      try {
        Promise.resolve(deps.pip.run()).catch((err) => fail(err, 'more', null));
      } catch (err) { fail(err, 'more', 'pip-error'); }
    });
  }
  function refreshPipItem() {
    if (pipItem) pipItem.querySelector('span').textContent = deps.pip.label();
  }

  if (deps.link) {
    addMenuItem(actions, 'link', 'Copy Link', () => {
      const url = deps.link.url();
      if (!url) { fail('No device to link to', 'more', null); return; }
      // Called inside the tap, so Safari lets the write through.
      Promise.resolve().then(() => deps.link.write(url))
        .then(() => { if (deps.toast) showToast(deps.toast, 'Link copied', 'more'); })
        .catch((err) => fail(err, 'more', 'copy-link-error'));
    });
  }

  const views = addGroup();
  if (deps.zoom) {
    addMenuItem(views, 'zoom', 'Zoom', () => {
      try { deps.zoom.enter(); } catch (err) { fail(err, 'more', 'zoom-error'); }
    });
  }
  if (deps.inspect) {
    addMenuItem(views, 'inspect', deps.inspect.label || 'Inspect', () => {
      try { deps.inspect.run(); } catch (err) { fail(err, 'more', 'inspect-error'); }
    });
  }
  // Shown on every target: the markers are drawn by this page, so they need
  // nothing from the device.
  let touchesSwitch = null;
  if (deps.touches) {
    touchesSwitch = addMenuItem(views, 'touches', 'Show touches', () => {
      deps.touches.setPref(!deps.touches.pref());
    });
    touchesSwitch.setAttribute('role', 'menuitemcheckbox');
  }

  // A checkmark on one item indents every label, as UIMenu does, so the
  // labels stay in one column whether or not an item is checked.
  function refreshTouchesItem() {
    if (!touchesSwitch) return;
    const on = deps.touches.pref();
    touchesSwitch.setAttribute('aria-checked', on ? 'true' : 'false');
    menu.classList.toggle('checked', on);
  }

  const clipGroup = addGroup();
  let clipItems = [];
  if (deps.clipboard) {
    clipItems = [
      addMenuItem(clipGroup, 'clipboard', 'Paste to device', () => {
        try { deps.clipboard.paste(); } catch (err) { fail(err, 'more', 'clipboard-error'); }
      }, { hideOnPress: true }),
      addMenuItem(clipGroup, 'clipboard', 'Copy from device', () => {
        try { deps.clipboard.copy(); } catch (err) { fail(err, 'more', 'clipboard-error'); }
      }),
    ];
  }

  layoutMenu();

  // The group goes with its items, or its gap would stay behind.
  function refreshClipboardItems() {
    if (!deps.clipboard) return;
    const offered = deps.clipboard.offered();
    for (const item of clipItems) item.hidden = !offered;
    clipGroup.el.hidden = !offered;
  }

  toggleBtn.addEventListener('click', (ev) => {
    ev.stopPropagation();
    openMenu(!menuOpen);
  });

  // See shouldDismissMenu's own comment for why the capture phase.
  document.addEventListener('pointerdown', (ev) => {
    if (shouldDismissMenu({ open: menuOpen, menuEl: menu, toggleEl: toggleBtn, target: ev.target })) {
      closeMenu();
    }
  }, true);

  // ArrowUp/Down/Home/End move focus among the menu's own items, the
  // keyboard model role="menu" implies (WAI-ARIA menu button pattern) --
  // giving the role without it is worse than not claiming it, since a
  // screen-reader user is told this is a menu and then finds Tab is the
  // only way to move through it. Wrapping (End->Home, Home->End) matches
  // that same pattern rather than stopping dead at either end.
  function menuItems() {
    return Array.from(menu.querySelectorAll('.moreMenuItem')).filter((b) => !b.hidden);
  }

  function focusMenuItem(delta) {
    const items = menuItems();
    if (!items.length) return;
    const current = items.indexOf(document.activeElement);
    const next = current === -1
      ? (delta > 0 ? 0 : items.length - 1)
      : (current + delta + items.length) % items.length;
    items[next].focus();
  }

  document.addEventListener('keydown', (ev) => {
    if (!menuOpen) return;
    if (ev.key === 'Escape') {
      // Focus can be on a menu item -- arrow-key or Tab navigation inside an
      // open menu, both handled here and by the browser respectively -- when
      // Escape closes it; returning focus to the toggle button is what keeps
      // a keyboard user from losing their place, same as a11y practice for
      // any disclosure widget.
      const active = document.activeElement;
      const hadMenuFocus = active && menu.contains(active);
      closeMenu();
      if (hadMenuFocus) toggleBtn.focus();
      return;
    }
    if (!menu.contains(document.activeElement) && document.activeElement !== toggleBtn) return;
    if (ev.key === 'ArrowDown') { ev.preventDefault(); focusMenuItem(1); }
    else if (ev.key === 'ArrowUp') { ev.preventDefault(); focusMenuItem(-1); }
    else if (ev.key === 'Home') { ev.preventDefault(); const items = menuItems(); if (items.length) items[0].focus(); }
    else if (ev.key === 'End') { ev.preventDefault(); const items = menuItems(); if (items.length) items[items.length - 1].focus(); }
  });

  setOS(undefined);

  return { setOS, setControl, closeMenu, isMenuOpen, refreshKeyboard: refreshKeyboardButton };
}
