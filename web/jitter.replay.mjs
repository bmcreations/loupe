// Replays frame arrivals recorded by `wtprobe -dump` through the jitter buffer
// and reports how long frames waited between arriving and reaching the glass.
//
//   node web/jitter.replay.mjs arrivals.txt [jitter.js] [interactive]
//
// A browser answers this only while it is in front and painting, and it cannot
// replay the same arrivals twice. This can, so two versions of jitter.js are
// compared on one recording. Presents on a 60Hz vsync grid, as rAF does, and
// counts decode as instant, which it is here (see CLAUDE.md).
import { readFileSync } from 'node:fs';
import { pathToFileURL } from 'node:url';
import { resolve } from 'node:path';

const [dumpPath, modPath = './jitter.js', mode] = process.argv.slice(2);
if (!dumpPath) {
  console.error('usage: node web/jitter.replay.mjs arrivals.txt [jitter.js] [interactive]');
  process.exit(2);
}
// ./jitter.js is this directory's; any other path is taken from the cwd.
const mod = await import(modPath === './jitter.js'
  ? new URL(modPath, import.meta.url).href
  : pathToFileURL(resolve(modPath)).href);

// Config packets (flag bit 1) carry parameter sets, not a picture, and the
// client never queues them for presentation.
const frames = readFileSync(dumpPath, 'utf8').trim().split('\n').map((l) => {
  const [arr, pts, flags] = l.split(' ').map(Number);
  return { arr, ptsMs: pts / 1000, flags };
}).filter((f) => !(f.flags & 2));

const arrival = new mod.ArrivalJitter();
const buf = new mod.JitterBuffer({ maxBufferMs: 50 });
if (mode === 'interactive') buf.interactive = true;

const VSYNC = 1000 / 60;
const waits = [];
let next = 0;
const end = frames[frames.length - 1].arr + 500;
for (let t = 0; t < end; t += VSYNC) {
  while (next < frames.length && frames[next].arr <= t) {
    const f = frames[next++];
    arrival.observe(f.arr, f.ptsMs);
    buf.enqueue({ ptsMs: f.ptsMs, arr: f.arr }, f.arr);
  }
  buf.tick(arrival.value);
  const item = buf.due(t);
  if (item) waits.push(t - item.arr);
}

const q = (p) => waits[Math.min(waits.length - 1, Math.floor(p * waits.length))];
waits.sort((a, b) => a - b);
console.log(`${modPath}${mode ? ' ' + mode : ''}: drawn ${waits.length} of ${frames.length} | ` +
  `wait p50 ${q(0.5).toFixed(1)}ms p90 ${q(0.9).toFixed(1)} p99 ${q(0.99).toFixed(1)} max ${waits[waits.length - 1].toFixed(1)} | ` +
  `bufferMs ${buf.bufferMs.toFixed(1)}`);
