package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"time"
)

// Wire bytes, from internal/session/protocol.go. Copied rather than imported
// so the probe reads the wire the way a non-Go client does, and the fixture
// tests catch a drift between the two.
const (
	msgInit        byte = 0x01
	msgFrame       byte = 0x02
	msgStats       byte = 0x03
	msgPong        byte = 0x04
	msgError       byte = 0x05
	msgEnded       byte = 0x0A
	msgPing        byte = 0x13
	msgClientStats byte = 0x14
	msgFrameAck    byte = 0x1A

	flagKey    byte = 1 << 0
	flagConfig byte = 1 << 1

	frameHeaderLen = 1 + 1 + 8 + 8
)

var errShort = errors.New("wsprobe: message shorter than its header")

// frame is one MsgFrame's header.
type frame struct {
	Key, Config bool
	PTS         uint64 // microseconds
	SentAt      int64  // server wall clock at fan-out, unix microseconds
	Len         int    // payload bytes after the header
}

func parseFrame(b []byte) (frame, error) {
	if len(b) < frameHeaderLen || b[0] != msgFrame {
		return frame{}, errShort
	}
	return frame{
		Key:    b[1]&flagKey != 0,
		Config: b[1]&flagConfig != 0,
		PTS:    binary.BigEndian.Uint64(b[2:]),
		SentAt: int64(binary.BigEndian.Uint64(b[10:])),
		Len:    len(b) - frameHeaderLen,
	}, nil
}

// parsePong returns the client timestamp a MsgPong echoes.
func parsePong(b []byte) (uint64, error) {
	if len(b) < 1+8 || b[0] != msgPong {
		return 0, errShort
	}
	return binary.BigEndian.Uint64(b[1:]), nil
}

func encodePing(clientMicros uint64) []byte {
	b := make([]byte, 9)
	b[0] = msgPing
	binary.BigEndian.PutUint64(b[1:], clientMicros)
	return b
}

func encodeAck(pts uint64) []byte {
	b := make([]byte, 9)
	b[0] = msgFrameAck
	binary.BigEndian.PutUint64(b[1:], pts)
	return b
}

// serverStats is the part of the per-second MsgStats the server sends this
// viewer that says how it judged it: per viewer, unlike the trace's shed
// counters, which are the session's.
type serverStats struct {
	Health       string   `json:"health"`
	HealthReason string   `json:"healthReason,omitempty"`
	Keep         float64  `json:"keep"`
	BaseMs       float64  `json:"baseMs"`
	RttMs        *float64 `json:"rttMs,omitempty"`
	Fps          *float64 `json:"fps,omitempty"`
	QueueMs      *float64 `json:"queueMs,omitempty"`
	Shed         uint64   `json:"shed"`
}

func parseStats(b []byte) (serverStats, error) {
	var s serverStats
	if len(b) < 1 || b[0] != msgStats {
		return s, errShort
	}
	return s, json.Unmarshal(b[1:], &s)
}

// rttMeter is the web client's round-trip bookkeeping (web/pane.js
// handlePong): rttMs is a running average with weight 1/8, rttNowMs the
// median of one second's samples.
type rttMeter struct {
	ewma   float64
	second []float64
}

func (m *rttMeter) add(sample float64) {
	if m.ewma == 0 {
		m.ewma = sample
	} else {
		m.ewma += (sample - m.ewma) / 8
	}
	m.second = append(m.second, sample)
}

// take returns the second's median and clears it; ok is false when no pong
// came back, and the report then leaves rttNowMs out, as web/health.js does.
func (m *rttMeter) take() (median float64, n int, ok bool) {
	n = len(m.second)
	if n == 0 {
		return 0, 0, false
	}
	s := append([]float64(nil), m.second...)
	sort.Float64s(s)
	h := n / 2
	if n%2 == 1 {
		median = s[h]
	} else {
		median = (s[h-1] + s[h]) / 2
	}
	m.second = m.second[:0]
	return median, n, true
}

// clientStats is the MsgClientStats body the probe sends once a second. It
// names only what the probe measured: it does not decode, so decFps is frames
// received, and it has no jitter buffer, so bufferMs and its kin are absent
// rather than reported as zero. web adds rttNowMs, as web/pane.js does; the
// native client's rows in the trace carry rttMs alone, and the server then
// judges on the running average.
func clientStats(received int, rtt *rttMeter, nowMs float64, nowOK, web bool, name string) []byte {
	body := map[string]any{
		"fps":       received,
		"decFps":    received,
		"rxFps":     received,
		"rttMs":     math.Round(rtt.ewma*10) / 10,
		"transport": "websocket",
		"client":    name,
	}
	if web && nowOK {
		body["rttNowMs"] = math.Round(nowMs*10) / 10
	}
	j, _ := json.Marshal(body)
	return append([]byte{msgClientStats}, j...)
}

// window gathers one second of arrivals. Not safe for concurrent use; the
// probe guards it with a mutex.
type window struct {
	frames, keys, configs int
	bytes                 int
	delays                []float64 // ms, fan-out to arrival
	gapMax                time.Duration
	ptsMissing, ptsBack   int
	lastStats             *serverStats
}

// tracker holds what spans seconds: the last frame's arrival and pts, so gaps
// and skipped frames are measured across a second's edge.
type tracker struct {
	frameDur  time.Duration // nominal, for counting frames a pts jump skipped
	lastAt    time.Time
	lastPTS   uint64
	havePTS   bool
	cur       window
	gapMaxAll time.Duration
}

// frameArrived records one MsgFrame read at now (wall clock). Config frames
// are counted apart and do not move the gap or pts bookkeeping: the server
// sends them with pts 0 (trap 31), and the client never acks or presents one.
func (t *tracker) frameArrived(f frame, now time.Time) {
	t.cur.bytes += frameHeaderLen + f.Len
	if f.Config {
		t.cur.configs++
		return
	}
	t.cur.frames++
	if f.Key {
		t.cur.keys++
	}
	t.cur.delays = append(t.cur.delays, float64(now.UnixMicro()-f.SentAt)/1000)
	if !t.lastAt.IsZero() {
		gap := now.Sub(t.lastAt)
		t.cur.gapMax = max(t.cur.gapMax, gap)
		t.gapMaxAll = max(t.gapMaxAll, gap)
	}
	t.lastAt = now
	if t.havePTS && t.frameDur > 0 {
		switch {
		case f.PTS < t.lastPTS:
			// The synthetic target restarts its frame index on relaunch.
			t.cur.ptsBack++
		case f.PTS > t.lastPTS:
			step := float64(f.PTS-t.lastPTS) / float64(t.frameDur.Microseconds())
			if skipped := int(math.Round(step)) - 1; skipped > 0 {
				t.cur.ptsMissing += skipped
			}
		}
	}
	t.lastPTS, t.havePTS = f.PTS, true
}

// second is one per-second output line.
type second struct {
	T          string        `json:"t"`
	Sec        int           `json:"sec"`
	Frames     int           `json:"frames"`
	Keys       int           `json:"keys"`
	Configs    int           `json:"configs"`
	Bytes      int           `json:"bytes"`
	DelayP50   *float64      `json:"delayP50Ms"`
	DelayP90   *float64      `json:"delayP90Ms"`
	DelayMax   *float64      `json:"delayMaxMs"`
	GapMaxMs   float64       `json:"gapMaxMs"`
	PTSMissing int           `json:"ptsMissing"`
	PTSBack    int           `json:"ptsBack,omitempty"`
	RttMs      float64       `json:"rttMs"`
	RttNowMs   *float64      `json:"rttNowMs"`
	Pongs      int           `json:"pongs"`
	SentDecFps int           `json:"sentDecFps"`
	Server     *serverStats  `json:"server"`
	delays     []float64     // kept for the summary
	gap        time.Duration // unrounded, for the summary
}

// close ends the second at now and starts the next. gapMax includes the gap
// still open since the last frame, so a second in a stall reports its length
// instead of 0.
func (t *tracker) close(now time.Time, sec int) second {
	w := t.cur
	t.cur = window{}
	if !t.lastAt.IsZero() {
		w.gapMax = max(w.gapMax, now.Sub(t.lastAt))
	}
	out := second{
		T: now.UTC().Format(time.RFC3339Nano), Sec: sec,
		Frames: w.frames, Keys: w.keys, Configs: w.configs, Bytes: w.bytes,
		GapMaxMs:   ms(w.gapMax),
		PTSMissing: w.ptsMissing, PTSBack: w.ptsBack,
		Server: w.lastStats, delays: w.delays, gap: w.gapMax,
	}
	if len(w.delays) > 0 {
		p50, p90, mx := quantiles(w.delays)
		out.DelayP50, out.DelayP90, out.DelayMax = &p50, &p90, &mx
	}
	return out
}

func ms(d time.Duration) float64 { return math.Round(float64(d.Microseconds())/100) / 10 }

// quantiles returns p50, p90 and max by nearest rank, rounded to 0.1 ms.
func quantiles(v []float64) (p50, p90, mx float64) {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	rank := func(q float64) float64 {
		i := int(math.Ceil(q*float64(len(s)))) - 1
		return math.Round(s[max(i, 0)]*10) / 10
	}
	return rank(0.5), rank(0.9), math.Round(s[len(s)-1]*10) / 10
}

// summary is the last line: the whole run.
type summary struct {
	Summary     bool           `json:"summary"`
	Seconds     int            `json:"seconds"`
	Frames      int            `json:"frames"`
	Keys        int            `json:"keys"`
	Configs     int            `json:"configs"`
	Bytes       int            `json:"bytes"`
	PTSMissing  int            `json:"ptsMissing"`
	PTSBack     int            `json:"ptsBack"`
	DelayP50    float64        `json:"delayP50Ms"`
	DelayP90    float64        `json:"delayP90Ms"`
	DelayMax    float64        `json:"delayMaxMs"`
	GapMaxMs    float64        `json:"gapMaxMs"`
	RttNowP50   float64        `json:"rttNowP50Ms"`
	RttNowMax   float64        `json:"rttNowMaxMs"`
	Health      map[string]int `json:"health"`
	KeepMin     float64        `json:"keepMin"`
	KeepP50     float64        `json:"keepP50"`
	StallSecs   int            `json:"stallSeconds"`
	EndedReason string         `json:"ended,omitempty"`
}

func summarize(secs []second, gapMaxAll time.Duration) summary {
	s := summary{Summary: true, Seconds: len(secs), Health: map[string]int{}, GapMaxMs: ms(gapMaxAll)}
	var delays, rtts, keeps []float64
	for _, x := range secs {
		s.Frames += x.Frames
		s.Keys += x.Keys
		s.Configs += x.Configs
		s.Bytes += x.Bytes
		s.PTSMissing += x.PTSMissing
		s.PTSBack += x.PTSBack
		delays = append(delays, x.delays...)
		if x.RttNowMs != nil {
			rtts = append(rtts, *x.RttNowMs)
		}
		if x.Server != nil {
			s.Health[x.Server.Health]++
			keeps = append(keeps, x.Server.Keep)
		}
		if x.Frames == 0 {
			s.StallSecs++
		}
		s.GapMaxMs = max(s.GapMaxMs, ms(x.gap))
	}
	if len(delays) > 0 {
		s.DelayP50, s.DelayP90, s.DelayMax = quantiles(delays)
	}
	if len(rtts) > 0 {
		s.RttNowP50, _, s.RttNowMax = quantiles(rtts)
	}
	if len(keeps) > 0 {
		s.KeepP50, _, _ = quantiles(keeps)
		s.KeepMin = keeps[0]
		for _, k := range keeps {
			s.KeepMin = min(s.KeepMin, k)
		}
	}
	return s
}
