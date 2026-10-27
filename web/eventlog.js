// The Tools sheet's event log: decode MsgEvent (0x06), track it with the
// catch-up fetch to /api/events, and format each entry for display.
//
// decodeEvent/formatRow have no DOM in them (trap 21) and are pinned against
// testdata/client/eventlog.json plus the golden hex under testdata/protocol/
// (event.hex, event-full.hex, event-redacted.hex, event-button.hex) -- the
// same bytes internal/session's own golden test asserts against, so decode
// cannot silently disagree with the server. EventLog is DOM-free too and has
// no harness beyond the fixture file. mount() (bottom of the file) is the
// only DOM-touching part, and it takes every piece of state it needs about
// the surrounding page -- fetch, the current target, the connection
// generation, whether the section is visible -- as injected `deps`, so it
// can be driven from a minimal fake DOM in eventlog.test.mjs. app.js's own
// job shrinks to calling mount() once and wiring its three entry points
// (noteInit, handleEvent, render-on-sheet-open) into the transport
// callbacks it already has.
//
// Init now carries this client's own id as `viewer` (touches.js uses it to
// skip its own echoed taps), but the log does not read it: source is always
// `agent` or `viewer N`, and nothing here renders `you`.

// decodeEvent parses a MsgEvent body (the bytes AFTER the 0x06 type byte --
// handleControl already stripped it the same way it does for MsgInit and
// MsgStats) into the event object session.Event marshals. The wire body is
// already exactly that JSON object, so this is the decode, not a reshaping
// of it: field names and shapes (from/to/scroll as [x,y], rotation as a
// literal 0 not omitted, ...) are passed through untouched.
export function decodeEvent(bytes) {
  return JSON.parse(new TextDecoder().decode(bytes));
}

// --- formatting -------------------------------------------------------

const REDACTED_MARKER = '•••'; // "•••", passwordMarker on the server

function fixed(n) {
  // Server already rounds from/to/scroll to 3 decimals (round3 in
  // internal/session/events.go); this only avoids float noise like
  // 0.30000000000000004 reappearing if a caller hands formatRow an
  // unrounded point in a test.
  return Math.round(n * 1000) / 1000;
}

function point(p) {
  return `${fixed(p[0])}, ${fixed(p[1])}`;
}

// AKEYCODE_NAMES names the Android keycodes an event log is likely to see on
// a `kind:"key"` event -- one with no named button (see docs/protocol.md's
// Key section and CLAUDE.md's note that a viewer's own key events are never
// redacted, unlike typed text). An unlisted code falls back to the raw
// number rather than guessing, since inventing a name for an untested code
// is worse than admitting this table is not exhaustive.
const AKEYCODE_NAMES = {
  3: 'Home',
  4: 'Back',
  24: 'Volume up',
  25: 'Volume down',
  26: 'Power',
  61: 'Tab',
  62: 'Space',
  66: 'Enter',
  67: 'Delete',
  82: 'Menu',
  111: 'Escape',
  122: 'Move home',
  123: 'Move end',
  187: 'Recents',
};

function keyName(keycode) {
  return AKEYCODE_NAMES[keycode] || `keycode ${keycode}`;
}

function titleCase(s) {
  return s.length ? s[0].toUpperCase() + s.slice(1) : s;
}

function direction(from, to) {
  const dx = to[0] - from[0];
  const dy = to[1] - from[1];
  if (Math.abs(dx) >= Math.abs(dy)) return dx >= 0 ? 'right' : 'left';
  return dy >= 0 ? 'down' : 'up';
}

// An agent tap or swipe legitimately carries no from/to: agentScreenSize
// (internal/httpapi/agent.go) answers 0,0 when it cannot read the target's
// screen size, and fractionalPoint publishes no coordinate rather than
// guess one or divide by zero -- Event.From/To are `*Point` with
// `omitempty`, so the field is simply absent from the wire JSON, not null
// or zero. Rendering that as "position unknown" instead of indexing into an
// absent array is what keeps one such event from throwing mid-render and
// blanking every row after it (see render()'s per-row try/catch in mount,
// the other half of the same guard for anything this table does not
// anticipate).
function whatFor(ev) {
  switch (ev.kind) {
    case 'tap':
      return ev.from ? `Tap ${point(ev.from)}` : 'Tap (position unknown)';
    case 'longPress': {
      const where = ev.from ? point(ev.from) : '(position unknown)';
      return ev.ms ? `Long press ${where} · ${ev.ms}ms` : `Long press ${where}`;
    }
    case 'swipe': {
      const dir = (ev.from && ev.to) ? direction(ev.from, ev.to) : null;
      return dir ? `Swipe ${dir} · ${ev.ms}ms` : `Swipe (position unknown) · ${ev.ms}ms`;
    }
    case 'pinch': {
      const how = ev.scale >= 1 ? 'out' : 'in';
      return `Pinch ${how} ×${fixed(ev.scale)} · ${ev.ms}ms`;
    }
    case 'multiTouch':
      return `Multi-touch · ${ev.pointers} pointers`;
    case 'scroll':
      return ev.scroll ? `Scroll ${fixed(ev.scroll[0])}, ${fixed(ev.scroll[1])}` : 'Scroll (delta unknown)';
    case 'key':
      return `Key ${keyName(ev.keycode)}`;
    case 'button':
      return `Button ${titleCase(ev.button)}`;
    case 'text':
      if (ev.redacted) return `typed ${ev.textLen} characters (hidden)`;
      return `Typed "${ev.text}"`;
    case 'rotate':
      return `Rotate ${ev.rotation * 90}°`;
    default:
      return `Event: ${ev.kind}`;
  }
}

function pad2(n) {
  return String(n).padStart(2, '0');
}

// formatTime renders t (unix ms) as HH:MM:SS in the timezone tzOffsetMin
// describes, using the same sign convention as Date.prototype
// .getTimezoneOffset(): minutes to ADD to local time to reach UTC (so a
// caller typically passes `new Date().getTimezoneOffset()` for "my
// timezone", or 0 for UTC in a fixture that must not depend on where the
// test runs).
function formatTime(t, tzOffsetMin) {
  const local = new Date(t - tzOffsetMin * 60000);
  return `${pad2(local.getUTCHours())}:${pad2(local.getUTCMinutes())}:${pad2(local.getUTCSeconds())}`;
}

// formatRow turns a decoded event into what the log row shows. source is
// always `agent` or `viewer N` -- see the module comment on why `you` does
// not exist here.
export function formatRow(ev, tzOffsetMin = 0) {
  return {
    time: formatTime(ev.t, tzOffsetMin),
    what: whatFor(ev),
    source: ev.source === 'agent' ? 'agent' : `viewer ${ev.viewer}`,
    failed: !!ev.err,
  };
}

// --- EventLog -----------------------------------------------------------

// EventLog tracks the events a session has published, merges in whatever
// `/api/events?after=` replays, and reports rows newest first.
//
// seq is scoped to a single Session *instance* (docs/protocol.md's Replay
// section: "seq restarts at 1 on every Open"), so two different epochs of
// the log can legitimately reuse the same seq numbers. Every entry is
// tagged with the epoch it arrived in, and epoch advances on exactly two
// things:
//   - noteInit() being called with a *different* target than the one this
//     log was last tracking. The log is per target, so a target switch
//     wipes everything -- epoch, lastSeq, session and every stored entry --
//     rather than stacking a new target's rows under the old one's.
//   - merge() noticing EventPage.session has changed from what the log last
//     saw. init.json's sessionId is the *target* id and never changes
//     across a restart, so EventPage.session is the field that does; a
//     restart on the *same* target keeps the old epoch's rows (sorted below
//     the new ones) rather than discarding them, since they are still real
//     events for the target being watched.
//
// noteInit() on a *reconnect to the same target* does not know yet whether
// the session survived (a resume) or the server restarted underneath it (a
// phone locked past idleGrace -- trap 21's path). It cannot ask: the answer
// only exists in the catch-up page merge() has not fetched yet. So it opens
// a *provisional* epoch -- one higher than the one it was tracking -- and
// live events arriving before the catch-up resolves are recorded there,
// never mixed into the old epoch on a guess. `_pending` remembers what to do
// once the answer is known: {epoch, priorEpoch, priorSession}.
//
// merge() is the only place that answer arrives (EventPage.session), and it
// resolves `_pending` one of two ways:
//   - page.session === priorSession: it was a resume. The provisional
//     epoch's rows are folded back into priorEpoch, deduplicated by seq, and
//     lastSeq/epoch/session revert to continuing that same epoch -- so a
//     resume reads as one unbroken session, not two.
//   - page.session !== priorSession: the server restarted. The provisional
//     epoch *becomes* the new session's permanent epoch; priorEpoch's rows
//     stay exactly where they were, sorted below it by rows()'s (epoch, seq)
//     order, never relabelled and never merged with the new ones.
// If the catch-up fails outright, merge() never runs and _pending never
// resolves -- the provisional rows simply stay provisional, still sorted
// above the old epoch, which is the same "never mixed" outcome as an
// explicit restart, just without the label. Nothing here loops retrying a
// failed catch-up; the next live event or the next noteInit tries again.
//
// An earlier version of noteInit bumped the epoch on every call
// unconditionally with no provisional/resolve step, which was the
// reconnect-duplicates-the-ring bug: every resume relabelled the same events
// under a fresh epoch, dedupe keys on (epoch, seq) so nothing caught it, and
// a single resume turned 3 rows into 6.
//
// rows() sorts by (epoch, seq) rather than by insertion order, so a
// dedupe/gap-fill/fold from merge() lands in the right place regardless of
// when it happened to arrive.
export class EventLog {
  constructor(limit = 200) {
    this.limit = limit;
    this.epoch = 0;
    this.lastSeq = 0;
    this.session = null;
    this.target = undefined; // undefined: noteInit has never been told a target
    this.entries = [];
    this._seen = new Set();
    this._gapSeen = new Set();
    this._pending = null; // {epoch, priorEpoch, priorSession}, see the class comment
  }

  _clear() {
    this.epoch = 0;
    this.lastSeq = 0;
    this.session = null;
    this.entries = [];
    this._seen = new Set();
    this._gapSeen = new Set();
    this._pending = null;
  }

  // noteInit resets the log for a fresh connection's catch-up fetch. Call
  // it on every MsgInit, before fetching /api/events?after=.
  //
  // targetId is optional. Passed and different from the target this log
  // was last tracking, it is a target switch: the whole log is per-target
  // state, so this wipes it (see the class comment). Passed and unchanged,
  // or omitted, it is an ordinary reconnect: a provisional epoch opens (see
  // the class comment) and lastSeq resets, so the catch-up fetch that
  // follows asks the ring for everything from the right place; whether the
  // *session* actually changed is left to merge(), which is the only place
  // that can tell (EventPage.session is not known until the fetch answers).
  noteInit(targetId) {
    if (targetId !== undefined && targetId !== this.target) {
      this.target = targetId;
      this._clear();
      return;
    }
    if (targetId !== undefined) this.target = targetId;

    if (this._pending) {
      // A second (or later) MsgInit arrived before the first one's catch-up
      // resolved -- the server sends MsgInit twice at the start of every
      // session (a provisional codec, then the resolved one once SPS
      // arrives) and a rotation resends it too. The still-open provisional
      // epoch already has live events/gaps recorded in it; opening a THIRD
      // epoch on top the way the old code did stranded that epoch under a
      // number nothing ever folds or promotes again -- merge() only ever
      // sees the latest _pending, so the earlier provisional's rows sat
      // orphaned while lastSeq reset to 0 made the very next live event
      // look like a gap from 1. Retire the open provisional into a new one
      // instead: move its entries (and their epoch-scoped dedupe keys)
      // forward, and keep tracking the ORIGINAL priorEpoch/priorSession --
      // not the provisional being retired -- so merge() still folds against
      // (or promotes past) the real prior session once catch-up answers.
      const retiring = this.epoch;
      const newEpoch = retiring + 1;
      let maxSeq = 0;
      for (const e of this.entries) {
        if (e.epoch === retiring) {
          e.epoch = newEpoch;
          if (!e.gap) maxSeq = Math.max(maxSeq, e.seq);
        }
      }
      const prefix = `${retiring}:`;
      const rekey = (set) => new Set(
        [...set].map((k) => (k.startsWith(prefix) ? `${newEpoch}:${k.slice(prefix.length)}` : k))
      );
      this._seen = rekey(this._seen);
      this._gapSeen = rekey(this._gapSeen);
      this.epoch = newEpoch;
      this.lastSeq = maxSeq;
      this._pending = { epoch: newEpoch, priorEpoch: this._pending.priorEpoch, priorSession: this._pending.priorSession };
      return;
    }

    const priorEpoch = this.epoch;
    const priorSession = this.session;
    this.epoch++;
    this.lastSeq = 0;
    this._pending = { epoch: this.epoch, priorEpoch, priorSession };
  }

  _pushEvent(epoch, ev) {
    const key = `${epoch}:${ev.seq}`;
    if (this._seen.has(key)) return false;
    this._seen.add(key);
    this.entries.push({ epoch, seq: ev.seq, ev });
    return true;
  }

  // A gap is reported once: pushGap is a no-op for a (epoch, after, before)
  // triple already recorded, so re-processing the same range through add()
  // and then merge() cannot double it up.
  _pushGap(epoch, after, before, lost) {
    const key = `${epoch}:${after}:${before}`;
    if (this._gapSeen.has(key)) return;
    this._gapSeen.add(key);
    // Sorts between the events on either side of it: after < after+0.5 < before.
    this.entries.push({ epoch, seq: after + 0.5, gap: true, after, before, lost: !!lost });
  }

  // _trim drops entries in (epoch, seq) order -- the same order rows()
  // displays them in, just from the opposite end -- not insertion order.
  // After merge()'s resume fold, entries pushed from the catch-up page land
  // at the END of the array regardless of how low their seq is (they were
  // appended after the fold, not inserted where a live add() would have put
  // them), so a plain FIFO shift() drops whatever happened to be inserted
  // first -- the middle of the surviving range, not the oldest of it.
  //
  // A gap marker sorts at after+0.5 (see _pushGap) and is trimmed by that
  // same rule, with no preference either way over a real event. An earlier
  // version of this evicted a gap ahead of any real event on the theory
  // that a marker is cheap to lose -- it is not, once the log is already
  // at the limit: every add() past that point pushes one entry over, so a
  // gap the very same add() just opened (to record the live jump that
  // triggered it) would be evicted in the same call that created it,
  // before the catch-up it triggers has any chance to fill it. Trimming
  // strictly by (epoch, seq) means a gap survives exactly as long as a
  // real event at the same position would: until every entry shown is
  // newer than it.
  _trim() {
    while (this.entries.length > this.limit) {
      let idx = 0;
      for (let i = 1; i < this.entries.length; i++) {
        const a = this.entries[i];
        const b = this.entries[idx];
        if (a.epoch < b.epoch || (a.epoch === b.epoch && a.seq < b.seq)) idx = i;
      }
      const [removed] = this.entries.splice(idx, 1);
      if (removed.gap) {
        this._gapSeen.delete(`${removed.epoch}:${removed.after}:${removed.before}`);
      }
    }
  }

  // openGapAfter returns the `after` of the lowest still-open (not lost, not
  // yet filled) gap in the current epoch, or null when there is none. A
  // catch-up fetch triggered by add()'s gap return must ask from here, not
  // from lastSeq: add() already advances lastSeq past the seq that revealed
  // the gap (the point of noticing it at all is that something skipped
  // ahead), so a fetch keyed on lastSeq asks for events *after* the very
  // range it needs and the gap marker can never be filled.
  openGapAfter() {
    let min = null;
    for (const e of this.entries) {
      if (e.gap && !e.lost && e.epoch === this.epoch) {
        if (min === null || e.after < min) min = e.after;
      }
    }
    return min;
  }

  // _removeFilledGaps drops a (not-lost) gap marker once every seq strictly
  // between its after/before has actually arrived -- merge() can fill a gap
  // add() recorded earlier, and this is what lets the marker disappear
  // rather than sitting there next to the events that closed it. A `lost`
  // gap (ring rollover) is never removed: those seqs are gone for good, so
  // the loop below can never find them and the marker is permanent, which
  // is the point.
  _removeFilledGaps(epoch) {
    this.entries = this.entries.filter((e) => {
      if (!e.gap || e.epoch !== epoch || e.lost) return true;
      for (let s = e.after + 1; s < e.before; s++) {
        if (!this._seen.has(`${epoch}:${s}`)) return true;
      }
      this._gapSeen.delete(`${epoch}:${e.after}:${e.before}`);
      return false;
    });
  }

  // add is one live MsgEvent. Returns {gap: {after, before}} when ev.seq
  // skipped ahead of what was expected next, else null. A duplicate or
  // stale seq (already recorded in this epoch) is silently ignored.
  add(ev) {
    const key = `${this.epoch}:${ev.seq}`;
    if (this._seen.has(key)) return null;
    let gap = null;
    if (ev.seq > this.lastSeq + 1) {
      gap = { after: this.lastSeq, before: ev.seq };
      this._pushGap(this.epoch, gap.after, gap.before, false);
    }
    this._pushEvent(this.epoch, ev);
    this.lastSeq = Math.max(this.lastSeq, ev.seq);
    this._removeFilledGaps(this.epoch);
    this._trim();
    return gap;
  }

  // merge takes an EventPage ({session, oldest, latest, events}, events
  // oldest first) from GET /api/events?after=. It fills gaps, drops
  // duplicates and keeps seq order.
  //
  // If a noteInit() is still unresolved (this._pending), this is the fetch
  // it triggered, and page.session is the first place the log can tell a
  // resume from a restart -- see the class comment for the fold-back vs.
  // promote decision this makes. Otherwise this is an ordinary periodic
  // catch-up with no provisional epoch involved, and a session change here
  // (EventPage.session vs. what the log last saw) starts a new epoch the
  // same way it always has. session equal to what the log already had (the
  // ordinary case) does nothing special, and existing entries are kept as
  // is -- dedupe is what makes replaying the same page a no-op.
  //
  // A ring rollover -- page.oldest ahead of what this log has already seen
  // -- means those events are gone for good (docs/protocol.md's Replay
  // section). Rather than looping fetches that can never succeed, this
  // surfaces exactly one `lost` gap marker for the unrecoverable range and
  // resumes from page.oldest.
  merge(page) {
    const pending = (this._pending && this._pending.epoch === this.epoch) ? this._pending : null;
    if (pending) this._pending = null;

    if (pending && page.session === pending.priorSession) {
      // A resume: fold the provisional epoch's rows back into the one it
      // was opened from, deduplicating anything the catch-up also returned.
      const seen = new Set();
      const gapSeen = new Set();
      const remapped = [];
      for (const e of this.entries) {
        const epoch = e.epoch === pending.epoch ? pending.priorEpoch : e.epoch;
        if (e.gap) {
          const gkey = `${epoch}:${e.after}:${e.before}`;
          if (gapSeen.has(gkey)) continue;
          gapSeen.add(gkey);
          remapped.push({ ...e, epoch });
        } else {
          const key = `${epoch}:${e.seq}`;
          if (seen.has(key)) continue;
          seen.add(key);
          remapped.push({ ...e, epoch });
        }
      }
      this.entries = remapped;
      this._seen = seen;
      this._gapSeen = gapSeen;
      this.epoch = pending.priorEpoch;
      this.session = pending.priorSession;
      let maxSeq = 0;
      for (const e of this.entries) {
        if (e.epoch === this.epoch && !e.gap) maxSeq = Math.max(maxSeq, e.seq);
      }
      this.lastSeq = maxSeq;
    } else if (pending) {
      // A restart: the provisional epoch is now the permanent one for this
      // session. priorEpoch's rows are untouched and stay sorted below it.
      this.session = page.session;
    } else {
      if (this.session !== null && page.session !== this.session) {
        this.epoch++;
        this.lastSeq = 0;
      }
      this.session = page.session;
    }

    // A gap add() recorded earlier can fall entirely below what the ring
    // still holds by the time a catch-up page lands -- the events that
    // would have filled it were rolled off in between. Left "open",
    // openGapAfter() would keep returning its `after` forever and every
    // later gap-triggered fetch would ask for the same unreachable range
    // (after=1, repeatedly, post-rollover). Mark it lost once this page
    // proves the ring no longer goes back that far: the range strictly
    // between after/before is what the gap covers, so `before <= oldest`
    // means none of it can still be in the ring.
    if (page.oldest > 0) {
      for (const e of this.entries) {
        if (e.gap && !e.lost && e.epoch === this.epoch && e.before <= page.oldest) {
          e.lost = true;
        }
      }
    }

    let runner = this.lastSeq;
    if (page.oldest > 0 && page.oldest > runner + 1) {
      this._pushGap(this.epoch, runner, page.oldest, true);
      runner = page.oldest - 1;
    }
    for (const ev of page.events || []) {
      if (ev.seq > runner + 1) {
        this._pushGap(this.epoch, runner, ev.seq, false);
      }
      this._pushEvent(this.epoch, ev);
      runner = Math.max(runner, ev.seq);
    }
    this.lastSeq = Math.max(this.lastSeq, runner);
    this._removeFilledGaps(this.epoch);
    this._trim();
  }

  // rows() returns decoded events and gap markers, newest first. A gap
  // marker is {gap: true, after, before, lost}; everything else is the
  // event object as decodeEvent produced it.
  rows() {
    return this.entries
      .slice()
      .sort((a, b) => (b.epoch - a.epoch) || (b.seq - a.seq))
      .map((e) => (e.gap ? { gap: true, after: e.after, before: e.before, lost: e.lost } : e.ev));
  }
}

// --- mount ----------------------------------------------------------------

// mount wires an EventLog into the DOM. It owns nothing about the page
// beyond listEl and whatever deps hands it, which is what makes it
// runnable from eventlog.test.mjs's fake elements instead of only from a
// real browser (app.js itself has no harness -- trap 21 -- so this is the
// one piece of the event log's wiring that gets one at all).
//
// deps:
//   getTarget()     -> the current target id, or a falsy value for "none".
//   getGeneration() -> the connection's current generation -- connGen in
//                      pane.js, already bumped by every connect() call,
//                      including a resume to the same target (trap 41's own
//                      fix). Comparing this before *and* after the fetch
//                      awaits is what drops a catch-up page whose
//                      connection has since been superseded, rather than
//                      merging it into whatever log is current by the time
//                      it lands -- see fetchCatchUp's comment.
//   isOpen()        -> whether the section is currently visible. render()
//                      is a no-op for the row list while this reads false;
//                      the caller re-renders on its own open transition
//                      (app.js does this the same way it already does for
//                      the HUD's mirrorSheetMetrics -- see sheetHandle's
//                      onOpen).
//   fetch           -> injectable; defaults to the global fetch.
//   tzOffsetMin()   -> injectable; defaults to the real local offset.
//   badgeEl         -> optional; textContent tracks the row count (gap
//                      markers excluded) on every mutation, independent of
//                      isOpen -- a closed section's badge should still be
//                      current.
//   limit           -> forwarded to `new EventLog(limit)`.
//
// Returns { noteInit(), handleEvent(body), render(), log }. log is exposed
// for tests and for anything that wants eventLog.rows() directly.
export function mount(listEl, deps = {}) {
  const {
    getTarget = () => null,
    getGeneration = () => 0,
    isOpen = () => true,
    fetch: fetchImpl = (typeof fetch === 'function' ? fetch : undefined),
    tzOffsetMin = () => new Date().getTimezoneOffset(),
    badgeEl = null,
    limit = 200,
  } = deps;

  const log = new EventLog(limit);
  const inFlightKeys = new Set(); // "target:generation" pairs with a fetch running

  function updateBadge() {
    if (!badgeEl) return;
    badgeEl.textContent = String(log.rows().filter((r) => !r.gap).length);
  }

  // buildRow renders one entry. Thrown out of formatRow/whatFor for a shape
  // this client does not know about, that throw is caught per row by the
  // caller (render()) rather than here, so one bad row cannot blank the
  // rows around it -- see the module comment on why an agent tap/swipe
  // with no coordinate must not be one of those throws in the first place.
  function buildRow(entry, tz) {
    const row = document.createElement('div');
    if (entry.gap) {
      row.className = 'logRow logGap';
      row.textContent = entry.lost
        ? `Events ${entry.after + 1}–${entry.before - 1} are gone (ring rolled past them)`
        : `Gap: events ${entry.after + 1}–${entry.before - 1} missing`;
      return row;
    }
    const r = formatRow(entry, tz);
    row.className = 'logRow' + (r.failed ? ' logFailed' : '');
    const time = document.createElement('span');
    time.className = 'logTime';
    time.textContent = r.time;
    const what = document.createElement('span');
    what.className = 'logWhat';
    what.textContent = r.what;
    const source = document.createElement('span');
    source.className = 'logSource';
    source.textContent = r.source;
    row.appendChild(time);
    row.appendChild(what);
    row.appendChild(source);
    return row;
  }

  function buildErrorRow() {
    const row = document.createElement('div');
    row.className = 'logRow logGap';
    row.textContent = 'Unreadable event';
    return row;
  }

  function render() {
    updateBadge();
    if (!listEl || !isOpen()) return;
    listEl.innerHTML = '';
    const rows = log.rows();
    if (!rows.length) {
      const empty = document.createElement('div');
      empty.className = 'logEmpty';
      empty.textContent = 'No events yet';
      listEl.appendChild(empty);
      return;
    }
    const tz = tzOffsetMin();
    for (const entry of rows) {
      let node;
      try {
        node = buildRow(entry, tz);
      } catch (_) {
        node = buildErrorRow();
      }
      listEl.appendChild(node);
    }
  }

  // fetchCatchUp is single-flight *per (target, generation)*, not globally:
  // an in-flight fetch is keyed on the connection it was started for, so a
  // fetch already running for a superseded connection does not block the
  // one a switch or a same-target reconnect needs to start right behind it.
  // Keying on a bare boolean was the bug -- noteInit() mid-fetch (a switch,
  // or a same-target reconnect) found fetchInFlight already true, returned
  // immediately, and the old response was then dropped by the currency
  // check below, so nothing ever fetched for the new connection at all.
  //
  // Within one (target, generation), a gap discovered while its fetch is
  // already running does not start a second request -- handleEvent below
  // relies on this to coalesce N gaps into at most one fetch.
  //
  // The target and generation are captured before the await and checked
  // again after -- both the network round trip and the JSON parse are
  // places a switch or a reconnect can land in between -- and a mismatch on
  // either check drops the response outright rather than merging it.
  // Merging a superseded connection's catch-up page is exactly what let one
  // target's late response collide, by seq, with the next connection's live
  // events and silently swallow them (trap 41's shape, one layer up).
  async function fetchCatchUp() {
    if (!fetchImpl) return;
    const target = getTarget();
    if (!target) return;
    const gen = getGeneration();
    const key = `${target}:${gen}`;
    if (inFlightKeys.has(key)) return;
    const stillCurrent = () => getTarget() === target && getGeneration() === gen;
    inFlightKeys.add(key);
    try {
      // openGapAfter(), not log.lastSeq: add() already advanced lastSeq past
      // whatever seq revealed the gap, so asking from lastSeq would ask for
      // events after the very range this fetch exists to fill (see its own
      // comment). null (no open gap -- an ordinary noteInit-triggered
      // catch-up) falls back to lastSeq, which is 0 right after noteInit.
      const after = log.openGapAfter() ?? log.lastSeq;
      const res = await fetchImpl(`/api/events?target=${encodeURIComponent(target)}&after=${after}`);
      if (!stillCurrent() || !res || !res.ok) return;
      const page = await res.json();
      if (!stillCurrent()) return;
      log.merge(page);
      render();
    } catch (_) {
      // Best-effort: a failed catch-up just means the log stays short until
      // the next MsgInit or the next live event, neither of which depends
      // on this succeeding.
    } finally {
      inFlightKeys.delete(key);
    }
  }

  function noteInit() {
    log.noteInit(getTarget() || undefined);
    fetchCatchUp();
  }

  function handleEvent(body) {
    const gap = log.add(decodeEvent(body));
    render();
    if (gap) fetchCatchUp();
  }

  return { noteInit, handleEvent, render, log };
}

// mountLogPage builds the Event log page's body: on Android a Gestures /
// Logcat switch over the pane's gesture list and a logcat view (Mac
// InspectorView.swift:393-440). Logcat is read on demand, 300 lines from
// GET /api/logcat, and again on Reload.
//
//   deps.targetId() -> the focused pane's target id.
//   deps.fetch(url) -> a Response-like {ok, text()}; defaults to fetch.
//
// Returns {setList(el), setAndroid(bool), reload()}. setList swaps in the
// focused pane's gesture list; setAndroid is false for iOS and Synthetic,
// where the switch is hidden and the view returns to Gestures.
export function mountLogPage(page, deps) {
  const doFetch = deps.fetch || ((url) => fetch(url));
  let mode = 'gestures';
  let seq = 0;

  const sw = document.createElement('div');
  sw.className = 'logcatSwitch settingSegments settingSegments--wide';
  sw.setAttribute('role', 'radiogroup');
  sw.setAttribute('aria-label', 'Event log view');
  sw.hidden = true;
  const buttons = [];
  for (const [value, label] of [['gestures', 'Gestures'], ['logcat', 'Logcat']]) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'settingSegment';
    b.dataset.value = value;
    b.setAttribute('role', 'radio');
    b.textContent = label;
    b.addEventListener('click', () => setMode(value));
    sw.appendChild(b);
    buttons.push(b);
  }

  const listHolder = document.createElement('div');
  listHolder.className = 'logGestures';

  const body = document.createElement('div');
  body.className = 'logcatBody';
  body.hidden = true;
  const bar = document.createElement('div');
  bar.className = 'logcatBar';
  const reloadBtn = document.createElement('button');
  reloadBtn.type = 'button';
  reloadBtn.className = 'logcatReload';
  reloadBtn.textContent = 'Reload';
  reloadBtn.addEventListener('click', () => reload());
  bar.appendChild(reloadBtn);
  const text = document.createElement('pre');
  text.className = 'logcatText';
  body.appendChild(bar);
  body.appendChild(text);

  page.replaceChildren(sw, listHolder, body);

  function paint() {
    for (const b of buttons) b.setAttribute('aria-checked', String(b.dataset.value === mode));
    listHolder.hidden = mode !== 'gestures';
    body.hidden = mode !== 'logcat';
  }

  async function reload() {
    const mine = ++seq;
    text.textContent = 'Loading…';
    let out;
    try {
      const res = await doFetch(`/api/logcat?target=${encodeURIComponent(deps.targetId() || '')}&lines=300`);
      if (!res.ok) throw new Error('logcat');
      out = await res.text();
    } catch (_) {
      out = 'Could not read logcat.';
    }
    if (mine === seq) text.textContent = out;
  }

  function setMode(next) {
    mode = next;
    paint();
    if (mode === 'logcat') reload();
  }

  paint();
  return {
    setList(listEl) { listHolder.replaceChildren(listEl); },
    setAndroid(on) {
      sw.hidden = !on;
      if (!on && mode !== 'gestures') { seq++; mode = 'gestures'; }
      paint();
    },
    reload,
  };
}
