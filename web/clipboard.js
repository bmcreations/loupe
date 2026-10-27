// Clipboard sharing between this phone and the device.
//
// Device to phone: the server sends MsgClipboard (0x07) each time something
// is copied on the device, and a banner offers it. Safari writes the
// clipboard only inside a tap, so the banner is a button rather than a
// silent copy. The overflow menu's "Copy from device" reads it on demand.
//
// Phone to device: "Paste to device" reads this phone's clipboard, which
// iOS answers with its own Paste callout, and POSTs it to /api/clipboard
// with paste set, so it lands in whatever field has focus on the device.
//
// No text reaches the trace from here, only lengths.
//
// Split the way keyboard.js is: pure functions, plus mount() for the DOM
// half (no harness, trap 21).

import { showToast } from './toast.js';

// OFFER_MS is how long the banner stays up for a copy nobody taps.
export const OFFER_MS = 8000;

// preview(text, max) -> text on one line, cut to max characters with an
// ellipsis, for the banner. Whitespace runs, newlines among them, become one
// space so a copied paragraph does not stretch the banner.
export function preview(text, max = 40) {
  const flat = String(text).replace(/\s+/g, ' ').trim();
  const chars = Array.from(flat);
  return chars.length > max ? chars.slice(0, max - 1).join('') + '…' : flat;
}

// offered(targetId, hasControl) -> whether the target has a clipboard this
// page can reach: anything but the synthetic pattern, over a session that
// takes input. An Android device's clipboard is reached through the session.
export function offered(targetId, hasControl) {
  return typeof targetId === 'string' && targetId !== '' && targetId !== 'synthetic' && !!hasControl;
}

// cancelled(err) -> whether a clipboard read failed because the user said no:
// Safari rejects readText with NotAllowedError when its Paste callout is
// dismissed. That is a choice, not a fault, so it gets no error toast.
export function cancelled(err) {
  return !!err && (err.name === 'NotAllowedError' || err.name === 'AbortError');
}

// mount(deps) builds the banner and returns the menu's actions.
//
// deps:
//   doc -- the document.
//   host -- the element the banner goes in; doc.body by default.
//   clipboard -- navigator.clipboard, or undefined outside a secure context.
//   ClipboardItem -- window.ClipboardItem, or undefined.
//   targetId(), hasControl() -- as keyboard.js takes them.
//   fetch(url, init) -- window.fetch.
//   toast -- the shared #toast element.
//   report(obj) -- client telemetry.
export function mount(deps) {
  const banner = deps.doc.createElement('button');
  banner.type = 'button';
  banner.className = 'clipOffer';
  banner.hidden = true;
  (deps.host || deps.doc.body).appendChild(banner);

  let pending = null;
  let timer = 0;
  const win = deps.doc.defaultView;

  function hide() {
    banner.hidden = true;
    pending = null;
  }

  function fail(action, err) {
    const msg = String((err && err.message) || err);
    deps.report({ event: 'clipboard-error', action, err: msg });
    showToast(deps.toast, `Clipboard: ${msg}`, 'more');
  }

  function url() {
    return `/api/clipboard?target=${encodeURIComponent(deps.targetId() || '')}`;
  }

  async function readDevice() {
    const res = await deps.fetch(url());
    if (!res.ok) throw new Error(`${res.status} ${(await res.text()).trim().slice(0, 120)}`);
    return (await res.json()).text || '';
  }

  // writePhone must be called inside the tap. With ClipboardItem the write
  // starts synchronously and the text may still be on its way, which is the
  // form Safari accepts after an await; writeText after an await is refused.
  function writePhone(textOrPromise) {
    if (!deps.clipboard) return Promise.reject(new Error('this page cannot reach the clipboard'));
    if (typeof textOrPromise === 'string') return deps.clipboard.writeText(textOrPromise);
    if (deps.ClipboardItem && deps.clipboard.write) {
      const blob = textOrPromise.then((t) => new Blob([t], { type: 'text/plain' }));
      return deps.clipboard.write([new deps.ClipboardItem({ 'text/plain': blob })]);
    }
    return textOrPromise.then((t) => deps.clipboard.writeText(t));
  }

  banner.addEventListener('click', () => {
    const text = pending;
    hide();
    if (text == null) return;
    writePhone(text).then(() => {
      deps.report({ event: 'clipboard-copy', via: 'offer', textLen: text.length });
      showToast(deps.toast, 'Copied to this phone');
    }, (err) => fail('copy', err));
  });

  return {
    offered: () => offered(deps.targetId(), deps.hasControl()),

    // handleMessage(bytes) takes MsgClipboard's body: the device's clipboard.
    handleMessage(bytes) {
      const text = new TextDecoder().decode(bytes);
      if (text === '') return;
      pending = text;
      banner.textContent = `Copied on device: “${preview(text)}” — tap to copy`;
      banner.hidden = false;
      deps.report({ event: 'clipboard-offer', textLen: text.length });
      if (win) {
        win.clearTimeout(timer);
        timer = win.setTimeout(hide, OFFER_MS);
      }
    },

    // copy is "Copy from device": must run inside the menu tap.
    copy() {
      const text = readDevice();
      writePhone(text).then(() => text).then((t) => {
        deps.report({ event: 'clipboard-copy', via: 'menu', textLen: t.length });
        showToast(deps.toast, t ? 'Copied to this phone' : 'The device clipboard is empty');
      }, (err) => fail('copy', err));
    },

    // paste is "Paste to device": must run inside the menu tap, which is
    // what lets iOS show its Paste callout for readText.
    paste() {
      if (!deps.clipboard || !deps.clipboard.readText) {
        fail('paste', new Error('this page cannot reach the clipboard'));
        return;
      }
      deps.clipboard.readText().then(async (text) => {
        if (!text) {
          showToast(deps.toast, 'This phone’s clipboard is empty');
          return;
        }
        const res = await deps.fetch(url(), {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ text, paste: true }),
        });
        if (!res.ok) throw new Error(`${res.status} ${(await res.text()).trim().slice(0, 120)}`);
        deps.report({ event: 'clipboard-paste', textLen: text.length });
      }).catch((err) => {
        // NotAllowedError is also what a read outside the tap gets, so the
        // report keeps the message; the toast only stops calling it a fault.
        if (cancelled(err)) {
          deps.report({ event: 'clipboard-paste', cancelled: true, err: String(err.message || err) });
          showToast(deps.toast, 'Paste cancelled');
          return;
        }
        fail('paste', err);
      });
    },
  };
}
