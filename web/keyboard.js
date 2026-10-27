// Phone keyboard: type into a simulator or an Android device with this
// phone's own keyboard instead of the device's.
//
// One button in the overflow corner, two halves behind it. Raising the
// keyboard focuses a hidden textarea, which brings up this phone's keyboard;
// what it would have typed is sent over the session as text and key messages
// instead. And it tells the device to connect a hardware keyboard
// (/api/input {"action":"hardwareKeyboard"}), which is what stops the device
// raising its own keyboard over half the stream. Dismissing it -- the button
// again, or anything else that takes focus from the field -- disconnects the
// hardware keyboard, so the device's own keyboard is back for a person
// tapping a field without this one up. A tap on the stream does not take
// focus (app.js's canvas handlers preventDefault), so typing into a device
// field and tapping between fields keeps the keyboard up.
//
// Nothing reads the device's setting back -- a simulator keeps it until
// something turns it off, and an Android device drops it when its session
// ends -- so it is sent again when a session starts while the keyboard is up.
//
// It used to be a persisted preference with a separate "Keyboard" item to
// raise it. The old loupe.phoneKeyboard key is no longer read.
//
// Split the way inspect.js is: pure functions with no DOM, testable against
// plain objects, plus mount() for the DOM half (no harness, trap 21).

import { showToast } from './toast.js';

// Android keycodes, which a key message carries and a simulator maps onto
// HID usages (internal/target/siminput.go's androidToIOS).
export const KEYCODE_ENTER = 66;
export const KEYCODE_DEL = 67;

// The key strip: a row above the phone's keyboard with the keys it lacks.
// Off by default, a Viewer setting for whoever needs Esc or Tab. Each entry
// is a keycode, except Paste, which goes through clipboard.js, and Done,
// which dismisses the keyboard (an iPhone's has no key for that).
export const STRIP_KEYS = [
  { id: 'esc', label: 'Esc', keycode: 111 },
  { id: 'tab', label: 'Tab', keycode: 61 },
  { id: 'left', label: '←', keycode: 21, aria: 'Left' },
  { id: 'up', label: '↑', keycode: 19, aria: 'Up' },
  { id: 'down', label: '↓', keycode: 20, aria: 'Down' },
  { id: 'right', label: '→', keycode: 22, aria: 'Right' },
  { id: 'paste', label: 'Paste' },
  { id: 'done', label: 'Done' },
];

const STRIP_KEY = 'loupe.keyStrip';

export function readStripPref(storage) {
  try { return !!storage && storage.getItem(STRIP_KEY) === '1'; } catch (_) { return false; }
}

export function writeStripPref(storage, on) {
  try { if (storage) storage.setItem(STRIP_KEY, on ? '1' : '0'); } catch (_) {}
}

// The textarea keeps one character so a backspace on an "empty" field still
// fires beforeinput; Safari sends none for a delete with nothing to delete.
export const SENTINEL = ' ';

// takesKeyboard(targetId) -> whether the target can connect a hardware
// keyboard: a simulator, an AVD or an adb device. The synthetic pattern
// takes no input at all.
export function takesKeyboard(targetId) {
  return typeof targetId === 'string' && targetId !== '' && targetId !== 'synthetic';
}

// MAX_TEXT_BYTES is the server's session.MaxTextLen: the most UTF-8 one text
// message may carry.
export const MAX_TEXT_BYTES = 4096;

// chunkText(s, max) -> s split into pieces of at most max UTF-8 bytes, never
// inside a character, so a large paste goes as several messages the server
// accepts rather than one it rejects.
export function chunkText(s, max = MAX_TEXT_BYTES) {
  const out = [];
  let cur = '';
  let curBytes = 0;
  for (const ch of s) {
    const cp = ch.codePointAt(0);
    const n = cp < 0x80 ? 1 : cp < 0x800 ? 2 : cp < 0x10000 ? 3 : 4;
    if (curBytes + n > max && cur) {
      out.push(cur);
      cur = '';
      curBytes = 0;
    }
    cur += ch;
    curBytes += n;
  }
  if (cur) out.push(cur);
  return out;
}

// normalizeText(s) -> s with the characters iOS substitutes as you type put
// back to the ASCII a device can be sent. Smart punctuation curls quotes
// even with autocorrect off; a simulator types only what a US keyboard has
// keys for, and Android's virtual key-character map has no curly quotes
// either, so it would skip them.
export function normalizeText(s) {
  return s
    .replace(/[‘’]/g, "'")
    .replace(/[“”]/g, '"')
    .replace(/ /g, ' ');
}

// inputToActions(inputType, data) -> the typing actions one beforeinput
// event becomes. Anything not listed -- formatting, undo, a composition in
// progress -- is nothing, rather than a guess.
export function inputToActions(inputType, data) {
  switch (inputType) {
    case 'insertText':
    case 'insertReplacementText':
    case 'insertFromPaste':
    case 'insertFromDrop': {
      const text = normalizeText(data || '');
      return text ? [{ action: 'text', text }] : [];
    }
    case 'insertLineBreak':
    case 'insertParagraph':
      return [{ action: 'key', keycode: KEYCODE_ENTER }];
    // Held down, iOS moves on to deleting whole words. The field here holds
    // nothing to measure a word against, so each event is one backspace.
    case 'deleteContentBackward':
    case 'deleteWordBackward':
    case 'deleteSoftLineBackward':
      return [{ action: 'key', keycode: KEYCODE_DEL }];
    default:
      return [];
  }
}

// leftoverActions(value) -> the typing actions for a field that changed
// without a cancelled beforeinput: the text after the sentinel, a backspace
// if the sentinel itself went, and an Enter for each line break.
export function leftoverActions(value) {
  if (value === SENTINEL) return [];
  if (value === '') return [{ action: 'key', keycode: KEYCODE_DEL }];
  const typed = value.startsWith(SENTINEL) ? value.slice(SENTINEL.length) : value;
  const out = [];
  typed.split(/\r\n|\r|\n/).forEach((line, i) => {
    if (i > 0) out.push({ action: 'key', keycode: KEYCODE_ENTER });
    out.push(...inputToActions('insertText', line));
  });
  return out;
}

// createSender(post, onError) -> {push(body), pending()}
//
// Sends one typing action at a time, in order. post may be asynchronous;
// text typed while one is in flight joins the text queued behind it, and
// goes as one send instead of one per character. A key never merges, so a
// backspace stays between the characters it was typed between.
//
// A failed send is reported and the rest still go: a character the device
// cannot type should not swallow everything typed after it.
export function createSender(post, onError) {
  const queue = [];
  let busy = false;

  async function drain() {
    if (busy) return;
    busy = true;
    while (queue.length) {
      const body = queue.shift();
      try {
        await post(body);
      } catch (err) {
        onError(err, body);
      }
    }
    busy = false;
  }

  return {
    push(body) {
      const last = queue[queue.length - 1];
      if (body.action === 'text' && last && last.action === 'text') {
        last.text += body.text;
      } else {
        queue.push({ ...body });
      }
      drain();
    },
    pending() {
      return queue.length + (busy ? 1 : 0);
    },
  };
}

// mount(deps) -> {offered(), open(), setPref(on), pref(), onSession(targetId)}
//
// deps:
//   doc -- the document to build the textarea in; mount needs doc.body.
//   storage -- localStorage or null (handedness.js's safeStorage).
//   targetId() -- the current target.
//   hasControl() -- whether the session accepts input.
//   postInput(body) -- POST /api/input for the current target; rejects on a
//     non-2xx. Carries only the hardwareKeyboard switch.
//   send(body) -- a typing action from inputToActions, sent over the
//     session; throws or rejects when it cannot be.
//   toast -- the shared #toast element, shown through toast.js the way
//     corners.js and inspect.js do (app.js must not import toast.js, trap 45).
//   report(obj) -- client telemetry.
//   strip -- the #keyStrip element, or null for no strip.
//   stripAllowed() -- false where the strip never shows whatever the
//     setting says: the docked desktop layout, which has a real keyboard.
//     Absent means always allowed.
//   paste() -- clipboard.js's "Paste to device", run inside the strip's tap.
// A blur this soon after another focus is the field being handed straight
// back (the button's own tap, or iOS moving focus while it animates), not a
// dismissal, so the detach waits this long for a refocus.
export const DETACH_DELAY_MS = 400;

export function mount(deps) {
  // attachedTo is the target a hardware keyboard was last sent for, so a
  // dismissal detaches only what this page attached.
  let attachedTo = '';
  let detachTimer = 0;
  const listeners = [];
  let stripOn = readStripPref(deps.storage);
  const changed = () => {
    paintStrip();
    for (const fn of listeners) fn(isOpen());
  };

  const field = deps.doc.createElement('textarea');
  field.className = 'phoneKeyboard';
  field.setAttribute('aria-label', 'Type on the device');
  // Every one of these changes what the keyboard sends. Autocorrect and
  // capitalisation would rewrite text the simulator has already been sent.
  field.setAttribute('autocapitalize', 'off');
  field.setAttribute('autocorrect', 'off');
  field.setAttribute('autocomplete', 'off');
  field.setAttribute('spellcheck', 'false');
  deps.doc.body.appendChild(field);

  let lastError = 0;
  const sender = createSender(deps.send, (err, body) => {
    deps.report({ event: 'keyboard-error', action: body.action, err: String((err && err.message) || err) });
    // One toast a second at most: a run of characters the device cannot
    // type would otherwise stack one per keystroke.
    const now = Date.now();
    if (now - lastError > 1000) {
      lastError = now;
      showToast(deps.toast, String((err && err.message) || err), 'more');
    }
  });

  function reset() {
    field.value = SENTINEL;
    field.setSelectionRange(SENTINEL.length, SENTINEL.length);
  }

  // What each open of the keyboard did, for telemetry: nothing in the trace
  // otherwise tells a keyboard that never came up from one whose input went
  // nowhere. Only the first few input events of an open are reported, and
  // never their text.
  let openedAt = 0;
  let inputsReported = 0;
  let composing = false;
  function noteInput(path, ev) {
    if (inputsReported >= 3) return;
    inputsReported++;
    deps.report({
      event: 'keyboard-input', path, inputType: ev.inputType || '',
      cancelable: !!ev.cancelable, composing: !!ev.isComposing || composing,
    });
  }

  field.addEventListener('beforeinput', (ev) => {
    noteInput('beforeinput', ev);
    // A composition (a CJK or a dictation keyboard) edits its own text
    // until it commits; compositionend below sends what it committed.
    if (ev.isComposing || composing || ev.inputType === 'insertCompositionText') return;
    // One that cannot be cancelled changes the field anyway, and the input
    // listener sends what it changed; sending it here too would type it twice.
    if (!ev.cancelable) return;
    ev.preventDefault();
    for (const body of inputToActions(ev.inputType, ev.data)) sender.push(body);
  });
  field.addEventListener('compositionstart', () => { composing = true; });
  field.addEventListener('compositionend', (ev) => {
    composing = false;
    for (const body of inputToActions('insertText', ev.data)) sender.push(body);
    reset();
  });
  // Anything that got past beforeinput is sent from what it did to the
  // field, then put back, so the field never grows. This used to only put
  // it back, which dropped it.
  field.addEventListener('input', (ev) => {
    if (ev.isComposing || composing) return;
    if (field.value === SENTINEL) return;
    noteInput('input', ev);
    for (const body of leftoverActions(field.value)) sender.push(body);
    reset();
  });
  const win = deps.doc.defaultView;
  // Focus that did not come through open() -- iOS handing it back after a
  // blur -- still shows the strip and refits the page.
  field.addEventListener('focus', () => {
    if (win) win.clearTimeout(detachTimer);
    changed();
  });
  field.addEventListener('blur', () => {
    if (openedAt) {
      deps.report({ event: 'keyboard-blur', afterMs: Date.now() - openedAt, inputs: inputsReported });
      openedAt = 0;
    }
    changed();
    if (!win) return detach();
    win.clearTimeout(detachTimer);
    detachTimer = win.setTimeout(() => {
      if (!isOpen()) detach();
    }, DETACH_DELAY_MS);
  });

  function isOpen() {
    return deps.doc.activeElement === field;
  }

  const strip = deps.strip || null;
  function stripShown() {
    return !!strip && stripOn && isOpen() && (!deps.stripAllowed || deps.stripAllowed());
  }
  function paintStrip() { if (strip) strip.hidden = !stripShown(); }
  if (strip) {
    for (const k of STRIP_KEYS) {
      const b = deps.doc.createElement('button');
      b.type = 'button';
      b.className = 'stripKey';
      b.dataset.key = k.id;
      b.textContent = k.label;
      if (k.aria) b.setAttribute('aria-label', k.aria);
      // Keeps focus on the field, or the tap would dismiss the keyboard
      // before the key is sent.
      b.addEventListener('pointerdown', (ev) => ev.preventDefault());
      b.addEventListener('click', () => {
        if (k.id === 'done') { field.blur(); return; }
        if (k.id === 'paste') { if (deps.paste) deps.paste(); return; }
        sender.push({ action: 'key', keycode: k.keycode });
      });
      strip.appendChild(b);
    }
    paintStrip();
  }

  function detach() {
    if (!attachedTo) return;
    const target = attachedTo;
    attachedTo = '';
    // A target switched away from, or a session that ended, has nothing to
    // send to: an Android device drops its keyboard with the session and
    // would answer 409 here. A simulator keeps it until the next dismissal
    // with a session open.
    if (target === deps.targetId() && deps.hasControl()) sendSwitch(target, false);
  }

  function sendSwitch(targetId, state) {
    if (!takesKeyboard(targetId)) return;
    deps.postInput({ action: 'hardwareKeyboard', on: state }).catch((err) => {
      deps.report({ event: 'keyboard-error', action: 'hardwareKeyboard', err: String((err && err.message) || err) });
      showToast(deps.toast, `Keyboard: ${(err && err.message) || err}`, 'more');
    });
  }

  return {
    offered: () => takesKeyboard(deps.targetId()) && deps.hasControl(),
    takes: () => takesKeyboard(deps.targetId()),
    isOpen,
    onChange(fn) { listeners.push(fn); },
    // Must run inside the tap that asked for it: iOS raises a keyboard for
    // a focus() only from a user gesture, so the focus comes before the
    // fetch that attaches the hardware keyboard.
    open() {
      if (win) win.clearTimeout(detachTimer);
      reset();
      field.focus();
      openedAt = Date.now();
      inputsReported = 0;
      const vv = () => (win && win.visualViewport ? Math.round(win.visualViewport.height) : 0);
      const before = vv();
      deps.report({ event: 'keyboard-open', focused: isOpen(), vvHeight: before });
      const target = deps.targetId();
      if (attachedTo !== target) {
        attachedTo = target;
        sendSwitch(target, true);
      }
      changed();
      // A keyboard that came up shrinks the visual viewport; one that did
      // not leaves it where it was. Read once it has had time to animate.
      if (win) {
        win.setTimeout(() => {
          deps.report({
            event: 'keyboard-shown', focused: isOpen(),
            vvHeight: vv(), vvBefore: before,
          });
        }, 800);
      }
    },
    close() {
      field.blur();
    },
    stripShown,
    stripPref: () => stripOn,
    setStripPref(on) {
      stripOn = !!on;
      writeStripPref(deps.storage, stripOn);
      deps.report({ event: 'keystrip-pref', on: stripOn });
      changed();
    },
    // Called when a session starts. The keyboard is resent only while it is
    // up: a phone not typing leaves the device's keyboard alone.
    onSession(targetId) {
      if (!isOpen()) { attachedTo = ''; return; }
      attachedTo = targetId;
      sendSwitch(targetId, true);
    },
  };
}
