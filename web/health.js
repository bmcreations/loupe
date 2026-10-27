// health.js maps the server's per-second verdict (plan 1, Task 6's `health`
// and `healthReason` in MsgStats) plus the age of that report into what the
// pill's dot shows. The server decides *whether* the stream is healthy; this
// module only decides how a report, or the absence of one, reads as a dot.
//
// `now` is a parameter rather than read from Date.now() so the 2500ms stale
// boundary in dotFor.test.mjs can be pinned exactly, from both sides, without
// fake timers.
//
// `now` and `input.receivedAt` MUST both come from performance.now(), never
// Date.now(). Date.now() can step backwards (NTP adjustment, system clock
// change), which would produce a negative age for a report that just
// arrived; performance.now() is monotonic within a page's lifetime and is
// what the rest of the client already timestamps frames with.

const STALE_AFTER_MS = 2500;

// dotFor({health, healthReason, receivedAt}, now) -> {state, label}
//   state: 'good' | 'degraded' | 'bad' | 'stale' | 'idle'
//
// No MsgStats has ever arrived (input is null, has no receivedAt, or
// receivedAt is not a finite number) reads 'idle': there is nothing to judge
// yet, which is a different fact from the stream having gone quiet.
//
// A report whose age is STALE_AFTER_MS or more reads 'stale' regardless of
// the verdict it carried (trap 26: an old reading is no reading, not a stale
// confirmation of whatever it last said). Age is read as Math.abs(now -
// receivedAt): a receivedAt in the future -- a negative age, which a
// monotonic clock should never produce but a caller could still pass in --
// is exactly as untrustworthy as one that is too old, and reads 'stale'
// rather than falling through to the verdict switch.
//
// A report that has arrived but carries no `health` field at all reads
// 'idle', the same state as no report ever having arrived, with the label
// 'no signal'. This is not hypothetical: the base MsgStats this module was
// first wired against has no `health` field yet -- session-health adds it --
// so every live session reads this branch until that lands. Reading it as
// 'degraded' would mean every connected viewer showed amber for the life of
// the field's absence, which is a false alarm on a client that has said
// nothing wrong. Only a `health` value that is present and simply not one of
// the three known verdicts -- a newer server's vocabulary this client does
// not recognise yet -- is treated as an actual, if unreadable, answer and
// reads 'degraded' with that verdict as its own label.
export function dotFor(input, now) {
  if (!input || !Number.isFinite(input.receivedAt)) {
    return { state: 'idle', label: 'idle' };
  }
  if (Math.abs(now - input.receivedAt) >= STALE_AFTER_MS) {
    return { state: 'stale', label: 'no signal' };
  }
  if (input.health == null) {
    return { state: 'idle', label: 'no signal' };
  }
  switch (input.health) {
    case 'good':
      return { state: 'good', label: 'good' };
    case 'degraded':
      return { state: 'degraded', label: input.healthReason || 'degraded' };
    case 'bad':
      return { state: 'bad', label: input.healthReason || 'bad' };
    default:
      return { state: 'degraded', label: String(input.health) };
  }
}

// secondRtt(samples) -> the median of one second's ping round trips, or
// undefined when none came back. The server judges health on this rather than
// on the display's running average, which takes about four seconds to fall
// back after a burst and held the dot on degraded for all of them. The median
// rather than the mean, so one late pong does not make a clean second read
// slow.
export function secondRtt(samples) {
  if (!samples.length) return undefined;
  const s = [...samples].sort((a, b) => a - b);
  const m = s.length >> 1;
  return s.length % 2 ? s[m] : (s[m - 1] + s[m]) / 2;
}
