// Adaptive jitter buffer.
//
// Frames decode at a steady rate but arrive clumped: measured inter-arrival
// jitter runs above one frame interval, so several land between two animation
// frames and then none land for a while. Drawing on arrival therefore presents
// well under the true frame rate, which reads as judder even though nothing was
// lost.
//
// So frames are presented against a clock rather than on arrival. The clock
// runs `bufferMs` behind the stream, which absorbs the clumping. That buffer is
// pure added latency, so it tracks measured jitter and is capped hard: on a
// clean link it collapses toward the floor and behaves like draw-on-arrival, on
// a jittery one it grows just enough to smooth delivery and never past the cap.
//
// This lives apart from the DOM so it can be tested. The first version of it
// shipped a deadlock that no browser-coupled test could have caught.

export class JitterBuffer {
  /**
   * @param {object}   opts
   * @param {number}   opts.maxBufferMs  Latency ceiling. Past this, smoother
   *                                     video is not worth the delay on
   *                                     something you are trying to tap.
   * @param {number}   opts.minBufferMs  Floor the buffer collapses toward.
   * @param {number}   opts.queueMax     Depth before frames are discarded. Must
   *                                     clear the burst a fresh connection
   *                                     delivers, or normal startup is treated
   *                                     as an overrun.
   * @param {number}   opts.slew         Fraction of the gap closed per tick.
   * @param {function} opts.dispose      Called with each discarded item.
   */
  constructor({
    maxBufferMs = 50,
    minBufferMs = 0,
    queueMax = 24,
    initialBufferMs = 16,
    slew = 0.05,
    pull = 0.25,
    interactiveBufferMs = 16,
    floorSlotMs = 500,
    floorSlots = 4,
    dispose = () => {},
  } = {}) {
    this.maxBufferMs = maxBufferMs;
    this.minBufferMs = minBufferMs;
    this.queueMax = queueMax;
    this.slew = slew;
    this.pull = pull;
    this.interactiveBufferMs = interactiveBufferMs;
    this.floorSlotMs = floorSlotMs;
    this.floorSlots = floorSlots;
    this.dispose = dispose;

    // While a finger is down the cushion is capped at interactiveBufferMs, so
    // what the finger did reaches the glass sooner. Not dropped to zero: frames
    // arrive in clumps, and with no cushion only the newest of each clump is
    // drawn, which reads as a third of the frame rate during the drag.
    this.interactive = false;

    // The lowest transit seen recently, as now - ptsMs, kept per slot so it
    // can rise again when the path gets slower. The clock is held a cushion
    // above this rather than above whatever the first frame happened to take.
    this.slots = [];
    this.slotId = null;
    this.enqueuedAt = new WeakMap();
    this.holds = [];

    this.queue = [];
    // baseline maps a frame's PTS onto the local clock. null means the clock is
    // unanchored, which is only ever a transient state.
    this.baseline = null;
    this.bufferMs = initialBufferMs;

    this.overruns = 0;
    this.skipped = 0;
  }

  get held() { return this.queue.length; }

  /**
   * Anchor the clock against the oldest queued frame.
   *
   * Normally that frame becomes due one buffer interval from now, which is what
   * builds the cushion. When catching up it becomes due immediately: an overrun
   * means the clock is already behind the stream, and re-anchoring into the
   * future there stalls presentation for another buffer interval while frames
   * keep arriving, so the buffer falls further behind and overruns forever.
   * Adding latency when you are already late is backwards.
   */
  sync(now, { immediate = false } = {}) {
    if (!this.queue.length) {
      this.baseline = null;
      return;
    }
    this.baseline = now - this.queue[0].ptsMs + (immediate ? 0 : this.bufferMs);
  }

  /**
   * Add a decoded frame. `item` needs a numeric `ptsMs`; anything else on it is
   * carried through untouched and handed back by due().
   */
  enqueue(item, now) {
    this.queue.push(item);
    this.enqueuedAt.set(item, now);
    this.noteTransit(now - item.ptsMs, now);
    if (this.baseline === null) this.sync(now);

    // Runaway guard. This must re-anchor the clock, never clear it: clearing it
    // deadlocks, because due() does nothing without a baseline, so the queue
    // never drains, so this guard fires on every frame and clears it again. The
    // guard becomes the runaway it exists to prevent.
    while (this.queue.length > this.queueMax) {
      this.dispose(this.queue.shift());
      this.overruns++;
      this.sync(now, { immediate: true });
    }
  }

  noteTransit(offset, now) {
    const slot = Math.floor(now / this.floorSlotMs);
    if (slot !== this.slotId) {
      this.slotId = slot;
      this.slots.push(Infinity);
      if (this.slots.length > this.floorSlots) this.slots.shift();
    }
    const last = this.slots.length - 1;
    this.slots[last] = Math.min(this.slots[last], offset);
  }

  /** Lowest recent now - ptsMs, or Infinity before any frame. */
  get transitFloor() { return Math.min(...this.slots); }

  /**
   * Hold the clock one cushion above the transit floor.
   *
   * sync() anchors on whichever frame was at the head of the queue, and the
   * first frame of a session is a keyframe: the largest the encoder makes and
   * the slowest to arrive. Anchored there, every later frame waited out the
   * difference between that keyframe's transit and its own, for the whole
   * session, and nothing moved the clock back, because the drift guard in
   * due() only acts past four times the ceiling. On three drags recorded from
   * loupe-probe with `wtprobe -dump` the first keyframe arrived 37-98ms slower
   * than the fastest frame after it, and replaying them (jitter.replay.mjs)
   * held frames p50 54-110ms while bufferMs read 4-50. Following the floor, the
   * same recordings hold p50 6-23ms and draw the same frames or nearly. So the clock follows the floor instead:
   * quickly when it is holding frames too long, slowly when the cushion has to
   * grow, since that is added delay and a step in it is a visible stutter.
   */
  track() {
    const floor = this.transitFloor;
    if (this.baseline === null || !Number.isFinite(floor)) return;
    const cushion = this.interactive
      ? Math.min(this.bufferMs, this.interactiveBufferMs)
      : this.bufferMs;
    const gap = floor + cushion - this.baseline;
    // A gap this size is a replaced timeline, not a link: re-anchor on it.
    if (Math.abs(gap) > maxPlausibleTransitMs) {
      this.baseline += gap;
      return;
    }
    this.baseline += gap * (gap < 0 ? this.pull : this.slew);
  }

  /** p50 and max of how long presented frames waited here, then reset. */
  drainHolds() {
    const h = this.holds.sort((a, b) => a - b);
    const out = h.length ? { p50: h[h.length >> 1], max: h[h.length - 1] } : null;
    this.holds = [];
    return out;
  }

  /** Track the buffer toward measured jitter, slewing rather than jumping. */
  tick(jitterMs) {
    // 1.5x absorbs ordinary clumping without paying for the worst outlier.
    const desired = Math.min(
      Math.max(jitterMs * 1.5, this.minBufferMs),
      this.maxBufferMs,
    );
    // A step change in the presentation clock is itself a visible stutter,
    // which is the thing this buffer exists to remove.
    this.bufferMs += (desired - this.bufferMs) * this.slew;
  }

  /**
   * Return the newest frame that is due for presentation, or null. Frames older
   * than the one returned are discarded: they are already history and would
   * never have reached the glass.
   */
  due(now) {
    // An unanchored clock with frames waiting can only stall. Whatever cleared
    // it, recover here rather than trusting the enqueue path.
    if (this.baseline === null && this.queue.length) this.sync(now);
    if (this.baseline === null || !this.queue.length) return null;
    this.track();

    let idx = -1;
    for (let i = 0; i < this.queue.length; i++) {
      if (this.baseline + this.queue[i].ptsMs <= now) idx = i;
      else break;
    }

    if (idx < 0) {
      // Nothing due. If the head has waited far longer than the buffer should
      // ever hold it, the clock drifted late; pull it forward rather than
      // stalling on a queue that is actually full.
      const wait = this.baseline + this.queue[0].ptsMs - now;
      if (wait > this.maxBufferMs * 4) this.baseline -= wait - this.bufferMs;
      return null;
    }

    for (let i = 0; i < idx; i++) {
      this.dispose(this.queue[i]);
      this.skipped++;
    }
    const item = this.queue[idx];
    this.queue.splice(0, idx + 1);
    const at = this.enqueuedAt.get(item);
    if (at !== undefined && this.holds.length < 1000) this.holds.push(now - at);
    return item;
  }

  flush() {
    for (const item of this.queue) this.dispose(item);
    this.queue.length = 0;
    this.baseline = null;
    this.slots = [];
    this.slotId = null;
  }
}

// Arrival jitter, RFC 3550.
//
// J is defined on D(i,j) = (Rj - Ri) - (Sj - Si): how much the transit time
// changed between two frames, which is the arrival gap minus the gap the
// sender intended. Both terms matter. Dropping the second one and comparing
// consecutive arrival gaps is only equivalent when the source emits at a fixed
// rate, and scrcpy does not: an idle screen has nothing to show, so it emits
// nothing for twenty seconds and then bursts (trap 20).
//
// That is the whole bug this class exists to fix. A 25s gap fed
// |25000 - 83| into the average and read as 1.5 seconds of jitter, which
// pinned the buffer at its ceiling and bought a third of a second of tap
// latency for a screen that was merely idle. With the sending term restored an
// idle gap contributes nothing, because both deltas grew together.
// A pts gap this far out of step with arrival is a discontinuity in the
// timeline, not variation in the link. The transport abandons a frame at a
// deadline that caps at one second, so nothing it delivers can legitimately
// disagree by five, and the buffer is capped in the low hundreds of
// milliseconds regardless -- a larger sample cannot inform it, it can only
// poison the average for the ~60 samples it takes to decay.
export const maxPlausibleTransitMs = 5000;

export class ArrivalJitter {
  constructor({ weight = 16 } = {}) {
    this.weight = weight;
    this.reset();
  }

  reset() {
    this.value = 0;
    this.lastArrivalMs = null;
    this.lastPtsMs = null;
  }

  /**
   * @param {number} arrivalMs  local clock, monotonic (performance.now())
   * @param {number} ptsMs      the frame's presentation timestamp
   */
  observe(arrivalMs, ptsMs) {
    const prevArrival = this.lastArrivalMs;
    const prevPts = this.lastPtsMs;
    this.lastArrivalMs = arrivalMs;
    this.lastPtsMs = ptsMs;

    if (prevArrival === null) return;

    // A backwards timestamp means the timeline was replaced: pts is the source
    // device's uptime, so switching target hands us a different clock entirely.
    // Differencing across that seam measures the swap, not the link.
    if (ptsMs < prevPts) return;

    const d = (arrivalMs - prevArrival) - (ptsMs - prevPts);

    // Forwards discontinuities are the same problem and are not rare: scrcpy
    // reports pts 0 for a parameter-set frame and absolute device uptime for
    // every media frame, so the pair straddling one reads as the whole uptime
    // -- 3.5e8ms on an emulator, 2.7e9ms on an Acme One. Re-anchor on it rather
    // than average it in.
    if (Math.abs(d) > maxPlausibleTransitMs) return;

    this.value += (Math.abs(d) - this.value) / this.weight;
  }
}
