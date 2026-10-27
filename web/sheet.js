// The Tools sheet: a bottom sheet over the full-size device, with a grab
// handle and two open detents, half and full. Released, it settles to the
// nearest detent to wherever the finger let go, including closed -- a small
// downward drag from half settles back to half rather than closing -- and a
// downward fling closes it outright regardless of position. The device does
// not re-fit underneath it (spec).
//
// The body is an iOS grouped list: five fixed groups, none of which
// collapses, and rows that push a page of their own (Event log, Connection,
// each Stream picker) with a Back button to return. Pushing a page keeps the
// sheet short at the half detent, which collapsing sections used to do.
//
// settle() is the pure part and is fixture-tested against
// testdata/client/sheet.json, the same pattern layout.js and orientation.js
// use. mount() is DOM wiring, tested against fakedom.js.

// Detents are fractions of the viewport height the sheet covers.
export const DETENTS = { half: 0.5, full: 0.92 };

// A downward fling is measured in px/ms and does not depend on distance
// travelled -- a fast short flick closes the sheet as surely as a slow long
// drag past the threshold.
const FLING_VELOCITY = 0.5;

// Velocity is estimated over a short trailing window rather than from the
// last two pointermove samples. Two samples a millisecond apart -- from
// event coalescing, or a platform that repeats a timestamp -- otherwise read
// as an enormous, spurious fling: 50px in 1ms is 50px/ms, twenty times the
// threshold, and nothing about that number says how long the finger was
// actually moving.
const VELOCITY_WINDOW_MS = 80;   // samples older than this (from release) do not count
const VELOCITY_STALE_MS = 100;   // no movement this recently before release reads as zero
const VELOCITY_MIN_SPAN_MS = 16; // a window narrower than this cannot be trusted either way

// estimateVelocity(samples, now) -> px/ms, positive moving down the screen.
//
// samples is {t, y} in ascending t order (oldest first), t and now in the
// same clock as pointer events' timeStamp. Two guards, in order:
//   1. If the newest sample is already more than VELOCITY_STALE_MS behind
//      `now`, the finger stopped moving before it lifted -- a hold-then-
//      release -- and the answer is zero regardless of how it got there.
//   2. Otherwise the window spans from the newest sample back to the oldest
//      one still within VELOCITY_WINDOW_MS *of the newest sample* -- not of
//      `now`. Release itself is not instantaneous, so a real gap can open up
//      between the last recorded movement and the up-event's own timestamp;
//      measuring the window from `now` would let that gap silently eat into
//      the window, on top of what the staleness guard above already checked
//      for. If that span is narrower than VELOCITY_MIN_SPAN_MS there is not
//      enough time between the two points to say anything -- this is what
//      stops a 1ms jitter from reading as a fling -- and the answer is zero
//      rather than a spike.
export function estimateVelocity(samples, now) {
  if (!samples || samples.length === 0) return 0;
  const newest = samples[samples.length - 1];
  if (now - newest.t > VELOCITY_STALE_MS) return 0;

  let oldest = newest;
  for (let i = samples.length - 1; i >= 0; i--) {
    const s = samples[i];
    if (newest.t - s.t > VELOCITY_WINDOW_MS) break;
    oldest = s;
  }

  const dt = newest.t - oldest.t;
  if (dt < VELOCITY_MIN_SPAN_MS) return 0;
  return (newest.y - oldest.y) / dt;
}

// settle(startDetent, dragDy, velocityY, vh) -> 'closed' | 'half' | 'full'
//
// dragDy is the net pointer movement in px since the drag began, positive
// moving down the screen. velocityY is px/ms at release, same sign
// convention. vh is the viewport height the drag happened in.
//
// Order matters:
//   1. A downward fling closes the sheet outright, wherever it let go --
//      the user's intent is unambiguous and overrides position.
//   2. An upward fling from 'half' promotes straight to 'full'.
//   3. Dragging down out of 'full' and releasing below the 'half' detent's
//      own position closes the sheet rather than snapping back to 'half'.
//      This only applies when the drag started at 'full': starting at
//      'half' itself, any downward movement at all puts the position below
//      half's own fraction, which would make an epsilon nudge close the
//      whole sheet. From 'half', closing is decided by nearest-detent below
//      (step 4), whose natural boundary between 'closed' and 'half' sits at
//      the midpoint between them.
//   4. Otherwise, the nearest detent (including 'closed', at 0) to wherever
//      the finger let go.
export function settle(startDetent, dragDy, velocityY, vh) {
  if (!vh) return startDetent;

  const startFrac = startDetent === 'closed' ? 0 : (DETENTS[startDetent] ?? DETENTS.half);
  let currentFrac = startFrac - dragDy / vh;
  if (currentFrac < 0) currentFrac = 0;
  if (currentFrac > 1) currentFrac = 1;

  if (velocityY > FLING_VELOCITY) return 'closed';
  if (velocityY < -FLING_VELOCITY && startDetent === 'half') return 'full';

  if (startDetent === 'full' && dragDy > 0 && currentFrac < DETENTS.half) return 'closed';

  let best = 'closed';
  let bestDist = Math.abs(currentFrac - 0);
  for (const [name, frac] of Object.entries(DETENTS)) {
    const dist = Math.abs(currentFrac - frac);
    if (dist < bestDist) {
      best = name;
      bestDist = dist;
    }
  }
  return best;
}

// detentHeightPx(vh, detentName) -> the px height a detent resolves to at a
// given viewport height. Pure, so a resize handler can be pinned without a
// DOM: heightFor() in mount() is this plus the one live measurement.
export function detentHeightPx(vh, detentName) {
  const frac = DETENTS[detentName] ?? 0;
  return Math.round(frac * (vh || 0));
}

const SECTION_TITLES = {
  hardware: 'Hardware',
  control: 'Control',
  app: 'App',
  device: 'Device',
  stream: 'Stream',
  viewer: 'Viewer',
  more: 'More',
};

// The Android keys the Hardware section presses (Mac InspectorView.swift:44-90):
// KEYCODE_VOLUME_DOWN, KEYCODE_VOLUME_UP, KEYCODE_POWER.
export const HARDWARE_KEYS = [
  { label: 'Vol \u2212', keycode: 25 },
  { label: 'Vol +', keycode: 24 },
  { label: 'Power', keycode: 26 },
];

export const SECTION_ORDER = ['hardware', 'control', 'app', 'device', 'stream', 'viewer', 'more'];

// mount(root, {onOpen, onClose, onPage, scrim, trigger}) builds the sheet's
// DOM into root (expected to be the empty #sheet element) and wires:
//   - the grab handle's drag, settled with settle() above
//   - the scrim click and Escape to close
//   - the page stack: addLink() rows push a page, its Back button pops it
//
// onOpen is called with the detent opened to and onClose with the detent
// closed from, which is what the sheet-open/sheet-close telemetry carries.
// onPage(id, 'push'|'pop') fires when a page is pushed or popped by the
// person, not when close() clears the stack.
//
// Returns a handle. sections maps a group id to its content element, so a
// caller fills a group by appending into sections.viewer etc. rather than
// this module knowing what any group contains. addPage(id, title) returns a
// page's content element the same way.
export function mount(root, opts = {}) {
  const { onOpen, onClose, onPage, scrim, trigger } = opts;

  root.innerHTML = '';
  root.classList.add('sheet');

  const handle = document.createElement('div');
  handle.className = 'sheetHandle';
  const grip = document.createElement('div');
  grip.className = 'sheetGrip';
  handle.appendChild(grip);
  root.appendChild(handle);

  const body = document.createElement('div');
  body.className = 'sheetBody';
  root.appendChild(body);

  // The root page holds the groups; every other page is a sibling of it, and
  // exactly one of them is shown at a time.
  const rootPage = document.createElement('div');
  rootPage.className = 'sheetPage';
  rootPage.dataset.page = '';
  body.appendChild(rootPage);

  // Shown only when docked, where Tools is a panel with no grabber to say
  // what it is.
  const rootTitle = document.createElement('h2');
  rootTitle.className = 'sheetRootTitle';
  rootTitle.textContent = 'Tools';
  rootPage.appendChild(rootTitle);

  const sections = {};
  const sectionEls = {};

  for (const id of SECTION_ORDER) {
    const section = document.createElement('section');
    section.dataset.section = id;
    section.className = 'sheetSection';

    const header = document.createElement('h3');
    header.className = 'sheetSectionHeader';
    header.textContent = SECTION_TITLES[id];
    section.appendChild(header);

    const content = document.createElement('div');
    content.className = 'sheetSectionContent';
    section.appendChild(content);

    rootPage.appendChild(section);
    sections[id] = content;
    sectionEls[id] = section;
  }

  const pages = { '': { el: rootPage, title: 'Tools' } };
  const stack = [];

  function showPage(id) {
    for (const [pid, p] of Object.entries(pages)) p.el.hidden = pid !== id;
    body.scrollTop = 0;
  }

  function addPage(id, title) {
    const page = document.createElement('div');
    page.className = 'sheetPage';
    page.dataset.page = id;
    page.hidden = true;

    const header = document.createElement('div');
    header.className = 'sheetPageHeader';
    const back = document.createElement('button');
    back.type = 'button';
    back.className = 'sheetBack';
    back.addEventListener('click', () => popPage());
    const heading = document.createElement('h2');
    heading.className = 'sheetPageTitle';
    heading.textContent = title;
    header.appendChild(back);
    header.appendChild(heading);
    page.appendChild(header);

    const content = document.createElement('div');
    content.className = 'sheetPageContent';
    page.appendChild(content);

    body.appendChild(page);
    pages[id] = { el: page, title, back };
    return content;
  }

  function current() {
    return stack.length ? stack[stack.length - 1] : '';
  }

  function pushPage(id) {
    if (!id || !pages[id]) return;
    const parent = current();
    stack.push(id);
    // Back names the page it returns to, as iOS's does: a picker pushed from
    // a page reads that page's title, one pushed from the groups "Tools".
    pages[id].back.textContent = pages[parent].title;
    showPage(id);
    if (onPage) onPage(id, 'push');
  }

  function popPage() {
    if (!stack.length) return;
    const id = stack.pop();
    showPage(current());
    if (onPage) onPage(id, 'pop');
  }

  // addLink(container, label, pageId) -> the row's detail element, which the
  // caller fills with a value ("1080") or a count, as Settings does beside
  // its chevron. container is a group id or any element, a page's content
  // included.
  function addLink(container, label, pageId) {
    const parent = typeof container === 'string' ? sections[container] : container;
    const row = document.createElement('button');
    row.type = 'button';
    row.className = 'sheetLinkRow';
    row.dataset.page = pageId;
    const span = document.createElement('span');
    span.className = 'sheetLinkLabel';
    span.textContent = label;
    const detail = document.createElement('span');
    detail.className = 'sheetLinkDetail';
    row.appendChild(span);
    row.appendChild(detail);
    row.addEventListener('click', () => pushPage(pageId));
    parent.appendChild(row);
    return detail;
  }

  // setFooter(id, text): the note under a group, the way iOS explains a
  // setting below its rows rather than beside each one.
  function setFooter(id, text) {
    const section = sectionEls[id];
    if (!section) return;
    let p = section.querySelector('.sheetSectionFooter');
    if (!p) {
      p = document.createElement('p');
      p.className = 'sheetSectionFooter';
      section.appendChild(p);
    }
    p.textContent = text;
  }

  let detent = 'closed';

  // The drag reads the viewport the same way the detents are sized, so a
  // fraction computed one place means the same number of px the other place
  // -- trap 18's class, one layer up: `vh` in CSS and `window.innerHeight`
  // in the pointer handler can disagree once Safari's toolbars show or hide,
  // which is exactly the gap that trap fixed for the video stage. Heights
  // are set in px from the same measurement the drag uses, so there is only
  // ever one number in play.
  function viewportHeight() {
    return window.visualViewport ? window.visualViewport.height : (window.innerHeight || 1);
  }

  // Pinned (app.js, at desktop widths) the sheet is a permanent panel whose
  // height the CSS overrides: nothing drags it, nothing closes it, and it
  // shows no scrim and takes no focus. pin(false) leaves it open for the
  // caller to close.
  let pinned = false;
  function pin(on) { pinned = !!on; }

  function heightFor(d) {
    return `${detentHeightPx(viewportHeight(), d)}px`;
  }

  // A rotation or Safari's toolbar showing/hiding changes the viewport height
  // without any pointer event at all, and the height was set in fixed px at
  // open time -- so without this, 'full' opened in 390x844 portrait is still
  // 776px after rotating to 844x390, with its handle off the top of the
  // screen. Reapplied only while open and not mid-drag: a drag is already
  // driving root.style.height itself from a fresh viewportHeight() read on
  // every move (see onPointerMove), and stomping on that here would fight it.
  function reflow() {
    if (detent === 'closed' || dragging) return;
    root.style.height = heightFor(detent);
  }

  function setOpenState(d, { silent = false } = {}) {
    const prev = detent;
    detent = d;
    if (d === 'closed') {
      root.hidden = true;
      root.style.height = '';
      root.style.transform = '';
      if (scrim) scrim.hidden = true;
      // The next open starts at the groups, not on whatever page was up.
      stack.length = 0;
      showPage('');
      if (prev !== 'closed') {
        if (onClose) onClose(prev);
        if (trigger) { try { trigger.focus(); } catch (_) {} }
      }
    } else {
      const wasClosed = root.hidden;
      root.hidden = false;
      root.style.transition = 'height .2s ease';
      root.style.height = heightFor(d);
      root.style.transform = '';
      if (scrim && !pinned) scrim.hidden = false;
      // A drag that starts and ends on the same detent is not an open --
      // the sheet was already open -- so onOpen fires only on an actual
      // transition, not every time settle() confirms where it already was.
      if (!silent && (wasClosed || prev !== d)) {
        if (onOpen) onOpen(d);
      }
      if (wasClosed && !pinned) focusSheet();
    }
  }

  function focusSheet() {
    // A candidate inside a hidden section or page (hideSection sets the
    // `hidden` attribute on the section, e.g. for 'app' when the target has
    // no data for it) cannot take focus -- the browser leaves focus wherever
    // the click that opened the sheet put it, and .focus() fails without
    // throwing. Skip anything hidden rather than taking the first DOM match.
    const candidates = body.querySelectorAll('button, [href], input, select, textarea, [tabindex]');
    for (const target of candidates) {
      if (target.closest('[hidden]')) continue;
      try { target.focus(); } catch (_) {}
      if (document.activeElement === target) return;
    }
  }

  function open(d) {
    setOpenState(d === 'full' ? 'full' : 'half');
  }

  function close() {
    if (detent === 'closed' || pinned) return;
    setOpenState('closed');
  }

  // Dragging. Only the handle starts a drag -- not the body -- so a scroll
  // inside the list is a scroll and not a sheet drag. Velocity comes from
  // estimateVelocity's trailing window (see above), not the last two
  // samples, so a millisecond of jitter cannot register as a fling.
  let dragging = false;
  let dragStartY = 0;
  let dragStartDetent = 'half';
  let samples = [];

  function onPointerDown(ev) {
    if (detent === 'closed' || pinned) return;
    dragging = true;
    dragStartY = ev.clientY;
    dragStartDetent = detent;
    samples = [{ t: ev.timeStamp, y: ev.clientY }];
    root.style.transition = 'none';
    try { ev.currentTarget.setPointerCapture(ev.pointerId); } catch (_) {}
  }

  function onPointerMove(ev) {
    if (!dragging) return;
    const dy = ev.clientY - dragStartY;
    const vh = viewportHeight();
    const startFrac = DETENTS[dragStartDetent] ?? DETENTS.half;
    let frac = startFrac - dy / vh;
    if (frac < 0) frac = 0;
    if (frac > DETENTS.full) frac = DETENTS.full;
    root.style.height = `${frac * vh}px`;
    samples.push({ t: ev.timeStamp, y: ev.clientY });
  }

  function onPointerUp(ev) {
    if (!dragging) return;
    dragging = false;
    root.style.transition = 'height .2s ease';
    const dy = ev.clientY - dragStartY;
    const vh = viewportHeight();
    const velocityY = estimateVelocity(samples, ev.timeStamp);
    const result = settle(dragStartDetent, dy, velocityY, vh);
    setOpenState(result);
  }

  // pointercancel means the platform took the gesture away -- a scroll
  // recognized as belonging to something else, a system gesture, a window
  // losing focus mid-drag -- not that the user chose an endpoint. Settling
  // it like a release would act on a position the user never released at,
  // so it returns to the detent the drag started from instead.
  function onPointerCancel() {
    if (!dragging) return;
    dragging = false;
    root.style.transition = 'height .2s ease';
    setOpenState(dragStartDetent, { silent: true });
  }

  handle.addEventListener('pointerdown', onPointerDown);
  handle.addEventListener('pointermove', onPointerMove);
  handle.addEventListener('pointerup', onPointerUp);
  handle.addEventListener('pointercancel', onPointerCancel);

  if (scrim) scrim.addEventListener('click', close);
  // Escape steps back a page before it closes the sheet, as Back would.
  document.addEventListener('keydown', (ev) => {
    if (ev.key !== 'Escape' || detent === 'closed') return;
    // A pinned sheet is not modal, so only Escape from inside it counts.
    if (pinned && !root.contains(document.activeElement)) return;
    if (stack.length) popPage(); else close();
  });

  // visualViewport is preferred because it is what viewportHeight() itself
  // reads -- the same instrument the drag uses -- and it fires on iOS for a
  // toolbar show/hide that window's own resize event does not. Fall back to
  // window resize only where visualViewport does not exist at all.
  if (typeof window !== 'undefined') {
    if (window.visualViewport && window.visualViewport.addEventListener) {
      window.visualViewport.addEventListener('resize', reflow);
    } else if (window.addEventListener) {
      window.addEventListener('resize', reflow);
    }
  }

  function hideSection(id) {
    const section = sectionEls[id];
    if (section) section.hidden = true;
  }

  // showSection reverses hideSection. app.js starts App hidden (it would
  // otherwise flash empty before the first fetch lands) and calls this once
  // a per-target fetch actually succeeds -- and again after a target switch
  // away from one that answered 501, since 501 is a property of the OLD
  // target, not of the panel itself (client Task 9 review, HIGH-1).
  function showSection(id) {
    const section = sectionEls[id];
    if (section) section.hidden = false;
  }

  return {
    open, close, pin, hideSection, showSection, sections,
    addPage, addLink, pushPage, popPage, setFooter,
    get detent() { return detent; },
    get page() { return current(); },
  };
}

