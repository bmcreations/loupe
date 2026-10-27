import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  takesKeyboard, chunkText, normalizeText, inputToActions, leftoverActions, createSender,
  SENTINEL,
  KEYCODE_ENTER, KEYCODE_DEL,
  STRIP_KEYS, readStripPref, writeStripPref,
} from './keyboard.js';

test('the key strip is off until it is turned on, and remembers', () => {
  const store = new Map();
  const storage = { getItem: (k) => (store.has(k) ? store.get(k) : null), setItem: (k, v) => store.set(k, v) };
  assert.equal(readStripPref(storage), false);
  assert.equal(readStripPref(null), false);
  writeStripPref(storage, true);
  assert.equal(readStripPref(storage), true);
  writeStripPref(storage, false);
  assert.equal(readStripPref(storage), false);
  const throwing = { getItem() { throw new Error('denied'); }, setItem() { throw new Error('denied'); } };
  assert.equal(readStripPref(throwing), false);
  writeStripPref(throwing, true);
});

test('every strip key but Paste and Done sends a keycode the simulator maps', () => {
  // internal/target/siminput.go's androidToIOS: ESCAPE, TAB and the DPAD,
  // pinned on the Go side by TestTheKeyStripKeysReachASimulator.
  const mapped = new Set([111, 61, 19, 20, 21, 22]);
  for (const k of STRIP_KEYS) {
    if (k.id === 'paste' || k.id === 'done') assert.equal(k.keycode, undefined, k.id);
    else assert.ok(mapped.has(k.keycode), k.id);
  }
});

test('every target but the synthetic pattern is offered the phone keyboard', () => {
  assert.equal(takesKeyboard('sim:98F85F5F-A448-4F5A-A511-45529A267273'), true);
  assert.equal(takesKeyboard('avd:Pixel_10'), true);
  assert.equal(takesKeyboard('emulator-5554'), true);
  assert.equal(takesKeyboard('synthetic'), false);
  assert.equal(takesKeyboard(''), false);
  assert.equal(takesKeyboard(undefined), false);
});

test('a paste is split under the byte limit and never inside a character', () => {
  assert.deepEqual(chunkText('abcde', 2), ['ab', 'cd', 'e']);
  // é is two bytes and 😀 four: neither may straddle a boundary.
  assert.deepEqual(chunkText('aé😀b', 3), ['aé', '😀', 'b']);
  assert.deepEqual(chunkText(''), []);
  const big = 'x'.repeat(5000);
  assert.deepEqual(chunkText(big).map((c) => c.length), [4096, 904]);
});

test('smart punctuation is put back to what a simulator can type', () => {
  assert.equal(normalizeText('it’s “fine”'), 'it\'s "fine"');
  assert.equal(normalizeText('a b'), 'a b');
});

test('typed text, paste and replacement become one text action each', () => {
  assert.deepEqual(inputToActions('insertText', 'a'), [{ action: 'text', text: 'a' }]);
  assert.deepEqual(inputToActions('insertFromPaste', 'hello'), [{ action: 'text', text: 'hello' }]);
  assert.deepEqual(inputToActions('insertReplacementText', 'the'), [{ action: 'text', text: 'the' }]);
  assert.deepEqual(inputToActions('insertText', ''), []);
  assert.deepEqual(inputToActions('insertText', null), []);
});

test('return and every kind of backspace become keys', () => {
  assert.deepEqual(inputToActions('insertLineBreak', null), [{ action: 'key', keycode: KEYCODE_ENTER }]);
  assert.deepEqual(inputToActions('insertParagraph', null), [{ action: 'key', keycode: KEYCODE_ENTER }]);
  for (const t of ['deleteContentBackward', 'deleteWordBackward', 'deleteSoftLineBackward']) {
    assert.deepEqual(inputToActions(t, null), [{ action: 'key', keycode: KEYCODE_DEL }], t);
  }
});

test('anything unlisted sends nothing', () => {
  assert.deepEqual(inputToActions('formatBold', null), []);
  assert.deepEqual(inputToActions('historyUndo', null), []);
});

test('what reached the field without a beforeinput is still sent', () => {
  assert.deepEqual(leftoverActions(SENTINEL), []);
  assert.deepEqual(leftoverActions(SENTINEL + 'ab'), [{ action: 'text', text: 'ab' }]);
  assert.deepEqual(leftoverActions(''), [{ action: 'key', keycode: KEYCODE_DEL }]);
  assert.deepEqual(leftoverActions(SENTINEL + 'it’s'), [{ action: 'text', text: "it's" }]);
  assert.deepEqual(leftoverActions(SENTINEL + 'a\nb'), [
    { action: 'text', text: 'a' },
    { action: 'key', keycode: KEYCODE_ENTER },
    { action: 'text', text: 'b' },
  ]);
  // The sentinel itself replaced, as a selection over it would: the whole
  // value is what was typed.
  assert.deepEqual(leftoverActions('x'), [{ action: 'text', text: 'x' }]);
});

test('text typed behind a request in flight merges, and a key keeps its place', async () => {
  const sent = [];
  let release;
  const post = (body) => {
    sent.push(structuredClone(body));
    return new Promise((r) => { release = r; });
  };
  const sender = createSender(post, () => {});
  sender.push({ action: 'text', text: 'a' });
  sender.push({ action: 'text', text: 'b' });
  sender.push({ action: 'text', text: 'c' });
  sender.push({ action: 'key', keycode: KEYCODE_DEL });
  sender.push({ action: 'text', text: 'd' });
  assert.deepEqual(sent, [{ action: 'text', text: 'a' }]);
  for (let i = 0; i < 4; i++) {
    release();
    await new Promise((r) => setImmediate(r));
  }
  assert.deepEqual(sent, [
    { action: 'text', text: 'a' },
    { action: 'text', text: 'bc' },
    { action: 'key', keycode: KEYCODE_DEL },
    { action: 'text', text: 'd' },
  ]);
  release();
  await new Promise((r) => setImmediate(r));
  assert.equal(sender.pending(), 0);
});

test('a failed request is reported and the rest still go', async () => {
  const sent = [];
  const errors = [];
  const post = async (body) => {
    sent.push(body.text);
    if (body.text === 'x') throw new Error('409');
  };
  const sender = createSender(post, (err, body) => errors.push([err.message, body.text]));
  sender.push({ action: 'text', text: 'x' });
  await new Promise((r) => setImmediate(r));
  sender.push({ action: 'text', text: 'y' });
  await new Promise((r) => setImmediate(r));
  assert.deepEqual(sent, ['x', 'y']);
  assert.deepEqual(errors, [['409', 'x']]);
});
