// The Connection page's rows, as data. app.js gathers a snapshot once a
// second and paints what this returns; nothing here touches the DOM, so the
// labels, units and thresholds have a node test.
//
// A value is graded only once it crosses a threshold. A page where every
// healthy number is green reads as a page of colour, and the one amber value
// that matters is lost in it. The thresholds are the HUD's, which
// internal/session/health.go copies for the server's own verdict.
//
// connectionRows(s) -> { link, video, problems }, each an array of
// { id, label, value, grade }, grade '' | 'warn' | 'bad'. problems holds only
// rows whose count is above zero, so an empty array means nothing is wrong.
//
// s, every field optional:
//   status       'live' | 'connecting' | 'error' | ...  (statusText names it)
//   statusText   what the overlay calls that status
//   transport    'webtransport' | 'websocket' | '' while closed
//   rtt          ms, the phone's own round trip
//   jitter       ms, arrival jitter
//   kbps         received this second
//   width, height, codec
//   targetId
//   decFps, fps  decoded and drawn this second
//   bufferMs     jitter buffer depth
//   dropped      frames this client could not use, since the session opened
//   decodeErrors since the session opened
//   shed         frames the server shed in the last second
//   queue        decoder queue depth, listed from 2

const DASH = '--';

const capitalised = (t) => t.charAt(0).toUpperCase() + t.slice(1);

function graded(value, warnAt, badAt) {
  if (!Number.isFinite(value)) return '';
  return value >= badAt ? 'bad' : value >= warnAt ? 'warn' : '';
}

// 55fps is where a 60fps stream starts reading as uneven; zero is no stream,
// which the status row already says.
function gradeFps(fps) {
  if (!(fps > 0)) return '';
  return fps < 40 ? 'bad' : fps < 55 ? 'warn' : '';
}

function ms(v, digits = 0) {
  // Loopback reads well under a millisecond, which toFixed(0) calls 0.
  if (digits === 0 && v > 0 && v < 1) return '<1 ms';
  return Number.isFinite(v) && v > 0 ? `${v.toFixed(digits)} ms` : DASH;
}

function fps(v) {
  return Number.isFinite(v) && v > 0 ? `${v} fps` : DASH;
}

function rate(kbps) {
  if (!(kbps > 0)) return DASH;
  return kbps >= 1000 ? `${(kbps / 1000).toFixed(1)} Mbps` : `${kbps} kbps`;
}

export function connectionRows(s = {}) {
  const status = s.status || 'idle';
  const link = [
    {
      id: 'status', label: 'Status',
      value: capitalised(s.statusText || status),
      grade: status === 'live' || status === 'idle' ? '' : status === 'error' ? 'bad' : 'warn',
    },
    {
      id: 'transport', label: 'Transport',
      value: s.transport === 'webtransport' ? 'WebTransport'
        : s.transport === 'websocket' ? 'WebSocket' : DASH,
      // WebSocket is the fallback, and falling back is silent otherwise.
      grade: s.transport === 'websocket' ? 'warn' : '',
    },
    { id: 'rtt', label: 'Round trip', value: ms(s.rtt), grade: s.rtt > 0 ? graded(s.rtt, 60, 120) : '' },
    { id: 'jitter', label: 'Jitter', value: ms(s.jitter, 1), grade: graded(s.jitter, 4, 10) },
    { id: 'rate', label: 'Rate', value: rate(s.kbps), grade: '' },
  ];

  const video = [
    { id: 'device', label: 'Device', value: s.targetId || DASH, grade: '' },
    {
      id: 'size', label: 'Size',
      value: s.width && s.height ? `${s.width} × ${s.height}` : DASH,
      grade: '',
    },
    { id: 'codec', label: 'Codec', value: s.codec || DASH, grade: '' },
    { id: 'decoded', label: 'Decoded', value: fps(s.decFps), grade: gradeFps(s.decFps) },
    { id: 'drawn', label: 'Drawn', value: fps(s.fps), grade: gradeFps(s.fps) },
    { id: 'buffer', label: 'Buffer', value: Number.isFinite(s.bufferMs) ? `${s.bufferMs.toFixed(0)} ms` : DASH, grade: graded(s.bufferMs, 35, 48) },
  ];

  const problems = [];
  if (s.shed > 0) {
    problems.push({ id: 'shed', label: 'Shed by server', value: `${s.shed} / s`, grade: graded(s.shed, 5, 20) || 'warn' });
  }
  if (s.dropped > 0) {
    problems.push({ id: 'dropped', label: 'Dropped', value: String(s.dropped), grade: 'warn' });
  }
  if (s.decodeErrors > 0) {
    problems.push({ id: 'decodeErrors', label: 'Decode errors', value: String(s.decodeErrors), grade: 'bad' });
  }
  // One frame waiting is the decoder between calls. Steady state reads 0
  // (CLAUDE.md, "The one result that matters"), so two or more is a backlog.
  if (s.queue > 1) {
    problems.push({ id: 'queue', label: 'Decoder queue', value: String(s.queue), grade: 'warn' });
  }

  return { link, video, problems };
}
