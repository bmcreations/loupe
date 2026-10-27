// Device settings (design spec "Tools sheet" section 3, client Task 9).
// GET/PUT /api/settings, over the field table internal/target/settings.go's
// Setting describes: darkMode, fontScale, reduceMotion, layoutBounds,
// talkBack, and posture on a foldable emulator. Built against that struct, not the plan's prose -- the real
// Setting has Min/Max instead of a nested range object, Supported/Reason
// instead of a single "why", and Applies is always "immediately" (there is
// no "nextSession" here the way /api/config has).
//
// GET /api/settings answers 501 for a target with no DeviceSettings
// (Synthetic today), which hides the section.
//
// Split the way inspect.js is: pure functions with no DOM, testable against
// plain objects, plus mount() for the DOM half (no harness, trap 21).

import { showToast } from './toast.js';

// labelFor(id) -> the row label. A plain lookup table rather than
// title-casing the id, because "TalkBack" and "APK" (in appcard.js) are not
// what title-casing "talkBack" or "apk" would produce.
const LABELS = {
  darkMode: 'Dark mode',
  fontScale: 'Font scale',
  reduceMotion: 'Reduce motion',
  layoutBounds: 'Layout bounds',
  talkBack: 'TalkBack',
  posture: 'Posture',
};

export function labelFor(id) {
  return LABELS[id] || id;
}

// optionLabel(id, value) -> the text on one segment of an enum setting.
const OPTION_LABELS = {
  posture: { closed: 'Closed', halfOpened: 'Half open', opened: 'Open' },
};

export function optionLabel(id, value) {
  return (OPTION_LABELS[id] && OPTION_LABELS[id][value]) || value;
}

// clampRange(value, min, max) -> value bounded to [min, max]. Guards a
// slider's own dragged-past-the-end input, and a value coming back from the
// device that is outside the range it itself advertises. A non-finite value
// (NaN from a blank input, or from JSON that lied about being a number)
// clamps to min rather than passing through, since min is always a real
// number for a SettingRange and NaN must not reach a PUT body.
export function clampRange(value, min, max) {
  if (typeof value !== 'number' || Number.isNaN(value)) return min;
  let v = value;
  if (typeof min === 'number' && v < min) v = min;
  if (typeof max === 'number' && v > max) v = max;
  return v;
}

// parseSettingsError(status, body) -> {status, message, failed, notRestored,
// settings}
//
// Normalizes the three failure shapes settings.go's putSettings sends --
// 400/502 carry only {"error"}; 409 carries {"error","failed","notRestored"?,
// "settings"?} -- into one shape the renderer can act on regardless of which
// one it got. failed/notRestored default to [] rather than undefined so a
// caller can read their .length without a null check, and settings defaults
// to null so "the device did not tell us anything new" (400/502, or a 409
// whose re-read itself failed) is distinguishable from "the device has no
// settings" (which cannot actually happen, but null is what "absent" means
// here rather than an empty array).
export function parseSettingsError(status, body) {
  const b = body && typeof body === 'object' ? body : {};
  return {
    status,
    message: typeof b.error === 'string' ? b.error : `settings: ${status}`,
    failed: Array.isArray(b.failed) ? b.failed : [],
    notRestored: Array.isArray(b.notRestored) ? b.notRestored : [],
    settings: Array.isArray(b.settings) ? b.settings : null,
  };
}

// applyResponse(current, incoming, pendingIds) -> the settings list to
// render.
//
// The rule the task states directly: the UI never shows a value the device
// doesn't have. `incoming` is whatever the server just told us (a GET, a
// successful PUT's body, or a 409's optional re-read); when it is a real
// array, it replaces `current` outright, because it is a full fresh read and
// stale entries in `current` are exactly the values-the-device-doesn't-have
// this rule forbids. When it is null (a 400/502, or a 409 whose re-read
// failed), nothing was confirmed, so `current` -- the last state actually
// read from the device -- is kept rather than guessing at the attempted
// change.
//
// `pendingIds` (a Set of bare setting ids, optional) is the review round 4
// fix: a refresh() GET can resolve while a write for one of its ids is still
// in flight -- the GET was in flight first and simply took longer, or the
// sheet was reopened mid-write. That GET's answer necessarily predates the
// write's own outcome, so for any id in `pendingIds` the row is left exactly
// as `current` already has it (value, supported, everything) rather than
// taking the incoming, soon-to-be-stale one. renderRow disables the row
// independently, from the same pending set, so nothing about this holds it
// looking any different from before the GET landed. The write's own
// eventual render() -- which does not go through this filter -- is what
// then shows the real outcome.
export function applyResponse(current, incoming, pendingIds) {
  if (!Array.isArray(incoming)) return Array.isArray(current) ? current : [];
  if (!pendingIds || pendingIds.size === 0) return incoming;
  const currentById = new Map((Array.isArray(current) ? current : []).map((s) => [s.id, s]));
  return incoming.map((s) => (pendingIds.has(s.id) && currentById.has(s.id) ? currentById.get(s.id) : s));
}

// mount(container, deps) builds the Device settings list into `container`
// (the sheet's 'device' section content element) and returns {refresh()}.
//
// deps:
//   targetId() -- the currently selected target id.
//   fetchImpl -- defaults to window.fetch.
//   onUnavailable() -- called every time GET /api/settings answers 501.
//   onAvailable() -- called once a read for the target in hand succeeds; a
//     switch away from a 501 target must not leave the section hidden
//     forever (review HIGH-1).
//   report(rec) -- app.js's report().
//   toast -- the shared #toast element (index.html), the same shape
//     corners.js and inspect.js take -- not a wrapper function. Review
//     round 3 found app.js passing this module a closure while this
//     module called showToast(deps.toast, ...) expecting the element
//     itself, which set properties on the function and showed nothing.
//     Every module now takes the same element and calls toast.js's
//     showToast directly; a settings write has no corner to anchor near
//     (unlike corners.js's own nav/more distinction), so this passes no
//     corner argument. Optional; a failed write still reverts and reports
//     without one.
export function mount(container, deps = {}) {
  let unavailable = false;
  // currentByTarget, not one shared `current` (review round 5, finding 3):
  // `container` only ever shows one target's rows, so a single `current`
  // variable used to just track "whatever render() last painted" -- which
  // after an A -> B switch was B's array. Switching back to B -> A while
  // A's own write was still pending then fed that (B's) array into
  // applyResponse's pendingIds fallback: it found B's own "darkMode" row --
  // the ids collide across targets by design (review round 3, finding 3) --
  // and painted B's value onto A's screen. Keying by target makes "A's own
  // last known value" mean what it says regardless of what was rendered in
  // between.
  const currentByTarget = new Map();
  function currentFor(targetId) {
    return currentByTarget.get(targetId) || [];
  }
  let lastTargetId; // undefined sentinel: "no read attempted yet"
  // gen is bumped by every refresh() -- i.e. every time the target in hand
  // changes, since that is the only thing that calls refresh() beyond the
  // initial mount. A write() captures the epoch it started in and discards
  // its own response outright if a switch has moved the epoch on by the
  // time it resolves (review HIGH-3): the control that write belongs to no
  // longer exists in `container`, which now holds the new target's row for
  // that same id.
  let gen = 0;
  // pending tracks ids with a write in flight, so a fast repeat action (a
  // second switch flip before the first PUT lands) does not race two PUTs
  // for the same id against each other. In practice a real pointer can't
  // trigger this -- the control disables itself the instant the write
  // starts -- so this is the defensive half of that, for anything that
  // fires 'change' programmatically.
  //
  // Keyed by `${targetId}:${id}`, not by id alone (review round 3, finding
  // 3): "darkMode" is a valid id on both target A and target B, and a plain
  // `Set` of ids meant a write started against A that resolved after a
  // switch to B could delete B's own in-flight "darkMode" entry -- or, if B
  // never wrote anything, leave A's stale entry sitting there with nothing
  // left to ever delete it, since A's own rows are gone.
  //
  // Keyed by target, not by gen (review round 4): `render()` and
  // `renderRow()` computed this same key from `pendingKey(id)`, which
  // returned the bare id, so the disabled check never matched what write()
  // stored under `gen + ':' + id` -- the render-side half of the guard was
  // dead from the start. Worse, `refresh()` bumped `gen` on every call,
  // including a refresh of the *same* target (reopening the sheet, or a
  // plain reconnect), which silently invalidated every in-flight write's
  // key and both let a second PUT go out for a control that looked
  // re-enabled and dropped the first PUT's own response as "stale" once it
  // returned. A target id does not change just because a refresh happens to
  // run again, so keying on it instead -- computed the same way in write()
  // and in renderRow() -- makes the two checks the same check by
  // construction, and a same-target refresh no longer disturbs it. `gen`
  // remains, but only for refresh()'s own out-of-order GET protection
  // below; it plays no part in pending or in a write's accept/reject
  // decision any more.
  const pending = new Set();
  function pendingKey(id) {
    const t = deps.targetId ? deps.targetId() : null;
    return (t || '') + ':' + id;
  }

  function fetchImpl() {
    return deps.fetchImpl || (typeof fetch !== 'undefined' ? fetch : null);
  }

  function render(settings, forTarget) {
    const t = forTarget !== undefined ? forTarget : (deps.targetId ? deps.targetId() : null);
    currentByTarget.set(t, settings);
    container.innerHTML = '';
    for (const setting of settings) {
      container.appendChild(renderRow(setting));
    }
  }

  function renderRow(setting) {
    const row = document.createElement('div');
    row.className = 'settingRow';
    if (!setting.supported) row.classList.add('settingRow--unsupported');

    const label = document.createElement('label');
    label.className = 'settingLabel';
    label.textContent = labelFor(setting.id);
    if (!setting.supported && setting.reason) label.title = setting.reason;
    row.appendChild(label);

    if (setting.type === 'range') {
      row.appendChild(renderRangeControl(setting));
    } else if (setting.type === 'enum') {
      row.appendChild(renderEnumControl(setting));
    } else {
      row.appendChild(renderBoolControl(setting));
    }
    return row;
  }

  function renderBoolControl(setting) {
    const input = document.createElement('input');
    input.type = 'checkbox';
    input.className = 'settingSwitch';
    input.checked = !!setting.value;
    input.disabled = !setting.supported || pending.has(pendingKey(setting.id));
    input.setAttribute('role', 'switch');
    input.setAttribute('aria-checked', String(!!setting.value));
    // A checkbox's own click already commits the change (unlike a slider
    // being dragged), so 'change' -- which fires once per toggle -- is one
    // PUT per user action, same as the rule for the slider below.
    input.addEventListener('change', () => {
      const value = input.checked;
      input.setAttribute('aria-checked', String(value));
      // Disable synchronously, on the same tick as the action, rather than
      // waiting for a re-render that only lands once the PUT resolves --
      // otherwise a second toggle before the first PUT lands races it
      // (review HIGH-4).
      input.disabled = true;
      write(setting.id, value, () => {
        input.checked = !value;
        input.setAttribute('aria-checked', String(!value));
        input.disabled = !setting.supported;
      });
    });
    return input;
  }

  // A segmented control: one button per option, the current one pressed.
  // A tap on another is one PUT, the same one-PUT-per-action rule as the
  // switch, and every segment is disabled until it resolves.
  function renderEnumControl(setting) {
    const group = document.createElement('div');
    group.className = 'settingSegments';
    group.setAttribute('role', 'radiogroup');
    group.setAttribute('aria-label', labelFor(setting.id));
    const options = Array.isArray(setting.options) ? setting.options : [];
    const buttons = [];
    const busy = !setting.supported || pending.has(pendingKey(setting.id));
    const mark = (value) => {
      for (const b of buttons) b.setAttribute('aria-checked', String(b.dataset.value === value));
    };
    for (const value of options) {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'settingSegment';
      b.dataset.value = value;
      b.setAttribute('role', 'radio');
      b.textContent = optionLabel(setting.id, value);
      b.disabled = busy;
      b.addEventListener('click', () => {
        if (value === setting.value) return;
        const previous = setting.value;
        mark(value);
        for (const o of buttons) o.disabled = true;
        write(setting.id, value, () => {
          mark(previous);
          for (const o of buttons) o.disabled = !setting.supported;
        });
      });
      buttons.push(b);
      group.appendChild(b);
    }
    mark(setting.value);
    return group;
  }

  function renderRangeControl(setting) {
    const wrap = document.createElement('div');
    wrap.className = 'settingRangeWrap';
    const input = document.createElement('input');
    input.type = 'range';
    input.className = 'settingRange';
    input.min = String(setting.min);
    input.max = String(setting.max);
    input.step = '0.05';
    input.value = String(clampRange(Number(setting.value), setting.min, setting.max));
    input.disabled = !setting.supported || pending.has(pendingKey(setting.id));
    const out = document.createElement('output');
    out.className = 'settingRangeValue';
    out.textContent = input.value;
    // Live-update the number while dragging, for feedback -- but this is
    // display only. 'input' fires on every drag tick; the PUT itself goes
    // out on 'change', which for a range input fires once, on release. That
    // is the one-PUT-per-action rule from a slider's own input model, not an
    // added debounce.
    input.addEventListener('input', () => {
      out.textContent = input.value;
    });
    input.addEventListener('change', () => {
      const value = clampRange(Number(input.value), setting.min, setting.max);
      const previous = setting.value;
      input.disabled = true; // see renderBoolControl's own change handler
      write(setting.id, value, () => {
        input.value = String(clampRange(Number(previous), setting.min, setting.max));
        out.textContent = input.value;
        input.disabled = !setting.supported;
      });
    });
    wrap.appendChild(input);
    wrap.appendChild(out);
    return wrap;
  }

  // pendingIdsFor(targetId) -> the bare ids (no target prefix) with a write
  // in flight against that target, for applyResponse's stale-GET filter.
  function pendingIdsFor(targetId) {
    const prefix = (targetId || '') + ':';
    const ids = new Set();
    for (const key of pending) {
      if (key.startsWith(prefix)) ids.add(key.slice(prefix.length));
    }
    return ids;
  }

  async function write(id, value, revert) {
    const f = fetchImpl();
    if (!f || !deps.targetId) {
      revert();
      return;
    }
    // The target this write belongs to, fixed at the moment it starts --
    // not `gen`. A switch away and back (A -> B -> A) bumps `gen` twice but
    // leaves `deps.targetId()` reading A again by the time this resolves,
    // and A's rows are exactly what should receive this response (review
    // round 4, bug 1). `gen` alone could not tell "back on the original
    // target" apart from "still on some other target", because it only
    // counts switches, not where they land.
    const myTarget = deps.targetId();
    const key = pendingKey(id); // targetId:id, computed against myTarget
    if (pending.has(key)) return; // one flight per id per target
    pending.add(key);
    // settings-put: one record per PUT, naming the ids and how it ended.
    // Never the value -- telemetry carries no text or coordinates, and the
    // settings-error record below already carries what an error needs.
    const put = (outcome, status) => {
      if (deps.report) deps.report({ event: 'settings-put', ids: [id], outcome, status });
    };
    try {
      const res = await f(`/api/settings?target=${encodeURIComponent(myTarget || '')}`, {
        method: 'PUT',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ changes: { [id]: value } }),
      });
      // Accept whenever we are back on this write's own target, regardless
      // of how many switches happened in between (review round 4, bug 1).
      const stillOnTarget = () => !deps.targetId || deps.targetId() === myTarget;
      if (!stillOnTarget()) { pending.delete(key); put('superseded', res.status); return; }
      if (res.status === 501) {
        put('unavailable', res.status);
        unavailable = true;
        pending.delete(key);
        revert();
        if (deps.onUnavailable) deps.onUnavailable();
        return;
      }
      let body = null;
      try {
        body = await res.json();
      } catch {
        // A non-JSON body (shouldn't happen; writeJSON always sends JSON)
        // is handled below by parseSettingsError's own fallback message.
      }
      if (!stillOnTarget()) { pending.delete(key); put('superseded', res.status); return; }
      if (res.ok) {
        put('ok', res.status);
        // Clear BEFORE the re-render, not after: renderRow reads `pending`
        // to decide the control's disabled state, and this write is done
        // (review HIGH-4 -- the old code cleared it in `finally`, after
        // render() had already read a stale "still pending"). Any other id
        // still mid-write against this same target is protected from this
        // response's own possibly-stale view of it by pendingIdsFor.
        pending.delete(key);
        render(applyResponse(currentFor(myTarget), body && body.settings, pendingIdsFor(myTarget)), myTarget);
        if (deps.onAvailable) deps.onAvailable();
        return;
      }
      const parsed = parseSettingsError(res.status, body);
      put('error', res.status);
      pending.delete(key);
      // `current` is never optimistically mutated by a control's own change
      // handler -- it holds whatever the last successful read or write
      // reported -- so applyResponse(current, parsed.settings) already shows
      // the un-attempted value on a 400/409/502: `render()` rebuilds every
      // row from scratch, so calling the closure-captured `revert()` here
      // too would touch a detached input from the row this just replaced.
      render(applyResponse(currentFor(myTarget), parsed.settings, pendingIdsFor(myTarget)), myTarget);
      if (deps.toast) showToast(deps.toast, parsed.message);
      if (deps.report) {
        deps.report({
          event: 'settings-error',
          id,
          status: parsed.status,
          err: parsed.message,
          failed: parsed.failed,
          notRestored: parsed.notRestored,
        });
      }
    } catch (err) {
      pending.delete(key);
      // Not revert(): a refresh() GET can have resolved while this PUT was
      // in flight (review round 5, finding 1). That GET's render() rebuilt
      // every row from scratch -- applyResponse held this id back at
      // `current`'s value because pendingIdsFor still listed it, but the
      // *element* revert() closes over is the one from the row that
      // render() replaced. Calling it touches a detached input and the
      // live control -- the new one render() just created -- never leaves
      // its disabled state. render() from the same current/applyResponse
      // path the other error statuses already take rebuilds the live row
      // instead, the same way a 400/409/502 does just above.
      if (!deps.targetId || deps.targetId() === myTarget) {
        render(applyResponse(currentFor(myTarget), null, pendingIdsFor(myTarget)), myTarget);
      }
      const message = String((err && err.message) || err);
      put('error');
      if (deps.toast) showToast(deps.toast, message);
      if (deps.report) deps.report({ event: 'settings-error', id, err: message });
    }
  }

  async function refresh() {
    const targetId = deps.targetId ? deps.targetId() : null;
    if (targetId !== lastTargetId) {
      // A fresh target: the previous target's 501 (or lack of one) says
      // nothing about this one (review HIGH-1).
      lastTargetId = targetId;
      unavailable = false;
    }
    // `gen` only orders this function's own GETs against each other now
    // (review round 4): a write's accept/reject decision is keyed to
    // `deps.targetId()` directly, not to this counter, because a same-target
    // refresh (reopening the sheet, a reconnect) used to bump `gen` and
    // silently invalidate every write in flight against that same target.
    gen++;
    const myGen = gen;
    if (unavailable) return;
    const f = fetchImpl();
    if (!f || !deps.targetId) return;
    try {
      const res = await f(`/api/settings?target=${encodeURIComponent(targetId || '')}`);
      if (myGen !== gen) return; // a later switch already won
      if (res.status === 501) {
        unavailable = true;
        if (deps.onUnavailable) deps.onUnavailable();
        return;
      }
      if (!res.ok) {
        let body = null;
        try { body = await res.json(); } catch { /* fall through to status-only message */ }
        throw new Error(parseSettingsError(res.status, body).message);
      }
      const body = await res.json();
      if (myGen !== gen) return;
      // A write for one of these ids may have started after this GET went
      // out and still be in flight: this GET's answer necessarily predates
      // that write's own outcome, so applyResponse leaves those rows exactly
      // as `current` has them rather than showing a value the write is
      // about to supersede (review round 4, bug 2 -- reopening the sheet or
      // reconnecting mid-write used to re-enable the control and let a
      // second PUT race the first).
      render(applyResponse(currentFor(targetId), body && body.settings, pendingIdsFor(targetId)), targetId);
      if (deps.onAvailable) deps.onAvailable();
    } catch (err) {
      const message = String((err && err.message) || err);
      if (deps.report) deps.report({ event: 'settings-error', err: message });
    }
  }

  return { refresh };
}
