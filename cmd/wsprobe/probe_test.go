package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// goldenBytes reads testdata/protocol/<name>, as cmd/uiprobe and
// internal/session do: one hex string per non-comment line, concatenated. The
// probe parses the same bytes a non-Go client would.
func goldenBytes(t *testing.T, name string) []byte {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "testdata", "protocol", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []byte
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.ReplaceAll(strings.TrimSpace(sc.Text()), " ", "")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		b, err := hex.DecodeString(line)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out = append(out, b...)
	}
	return out
}

func TestParseFrameReadsTheWSFixture(t *testing.T) {
	f, err := parseFrame(goldenBytes(t, "frame-ws.hex"))
	if err != nil {
		t.Fatal(err)
	}
	want := frame{Key: true, PTS: 352074200000, SentAt: 1727177400123456, Len: 8}
	if f != want {
		t.Fatalf("parsed %+v, want %+v", f, want)
	}
}

func TestParseFrameReadsTheConfigFixture(t *testing.T) {
	f, err := parseFrame(goldenBytes(t, "frame-ws-config.hex"))
	if err != nil {
		t.Fatal(err)
	}
	want := frame{Config: true, PTS: 0, SentAt: 1727177400000000, Len: 8}
	if f != want {
		t.Fatalf("parsed %+v, want %+v", f, want)
	}
}

func TestParseFrameRefusesAShortOrForeignMessage(t *testing.T) {
	b := goldenBytes(t, "frame-ws.hex")
	if _, err := parseFrame(b[:frameHeaderLen-1]); err == nil {
		t.Error("parsed a frame shorter than its header")
	}
	if _, err := parseFrame(goldenBytes(t, "pong.hex")); err == nil {
		t.Error("parsed a pong as a frame")
	}
}

func TestPingAndPongMatchTheFixtures(t *testing.T) {
	if got, want := encodePing(1234567890), goldenBytes(t, "ping.hex"); !bytes.Equal(got, want) {
		t.Errorf("encodePing = %x, want the fixture %x", got, want)
	}
	echoed, err := parsePong(goldenBytes(t, "pong.hex"))
	if err != nil {
		t.Fatal(err)
	}
	if echoed != 1234567890 {
		t.Errorf("pong echoed %d, want 1234567890", echoed)
	}
}

func TestAckIsTypeThenPTS(t *testing.T) {
	got := encodeAck(352074200000)
	want := append([]byte{0x1A}, goldenBytes(t, "frame-ws.hex")[2:10]...)
	if !bytes.Equal(got, want) {
		t.Errorf("encodeAck = %x, want %x", got, want)
	}
}

func TestParseStatsReadsTheServersVerdict(t *testing.T) {
	b := append([]byte{msgStats}, `{"health":"degraded","healthReason":"rtt 400ms","keep":0.5,"baseMs":90.2,"rttMs":400,"shed":12}`...)
	s, err := parseStats(b)
	if err != nil {
		t.Fatal(err)
	}
	if s.Health != "degraded" || s.Keep != 0.5 || s.BaseMs != 90.2 || s.Shed != 12 || s.RttMs == nil || *s.RttMs != 400 {
		t.Errorf("parsed %+v", s)
	}
}

// The web client's numbers: a 1/8 running average, and the median of the
// second, absent when no pong came back.
func TestRttMeterMatchesTheWebClient(t *testing.T) {
	var m rttMeter
	if _, _, ok := m.take(); ok {
		t.Fatal("a second without pongs reported an rtt")
	}
	m.add(100)
	m.add(180)
	if m.ewma != 110 {
		t.Errorf("ewma %v after 100 then 180, want 110", m.ewma)
	}
	m.add(120)
	med, n, ok := m.take()
	if !ok || n != 3 || med != 120 {
		t.Errorf("take = %v %d %v, want the median 120 of 3", med, n, ok)
	}
	m.add(100)
	m.add(200)
	if med, _, _ := m.take(); med != 150 {
		t.Errorf("median of two = %v, want their mean 150", med)
	}
}

func TestClientStatsReportsOnlyWhatWasMeasured(t *testing.T) {
	m := rttMeter{ewma: 112.34}
	decode := func(b []byte) map[string]any {
		if b[0] != msgClientStats {
			t.Fatalf("type 0x%02x, want MsgClientStats", b[0])
		}
		var v map[string]any
		if err := json.Unmarshal(b[1:], &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	web := decode(clientStats(37, &m, 140, true, true, "wsprobe-web"))
	if web["decFps"] != 37.0 || web["rxFps"] != 37.0 || web["rttMs"] != 112.3 || web["rttNowMs"] != 140.0 {
		t.Errorf("web report %v", web)
	}
	for _, k := range []string{"bufferMs", "decodeQueue", "dropped", "jitterMs"} {
		if _, ok := web[k]; ok {
			t.Errorf("report carries %s, which the probe does not measure", k)
		}
	}
	if _, ok := decode(clientStats(37, &m, 0, false, true, "wsprobe-web"))["rttNowMs"]; ok {
		t.Error("rttNowMs sent for a second with no pongs")
	}
	if _, ok := decode(clientStats(37, &m, 140, true, false, "wsprobe-native"))["rttNowMs"]; ok {
		t.Error("the native shape sent rttNowMs")
	}
}

func TestTrackerCountsASecond(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	tr := tracker{frameDur: time.Second / 60}
	sentAt := func(at time.Time, lag time.Duration) int64 { return at.Add(-lag).UnixMicro() }

	tr.frameArrived(frame{Config: true, Len: 30}, t0)
	pts := uint64(0)
	for i := 0; i < 10; i++ {
		at := t0.Add(time.Duration(i) * 20 * time.Millisecond)
		tr.frameArrived(frame{Key: i == 0, PTS: pts, SentAt: sentAt(at, time.Duration(10+i)*time.Millisecond), Len: 1000}, at)
		pts += 16666
	}
	// Two frames the server never sent this viewer, then one 300 ms late.
	pts += 2 * 16666
	late := t0.Add(500 * time.Millisecond)
	tr.frameArrived(frame{PTS: pts, SentAt: sentAt(late, 300*time.Millisecond), Len: 1000}, late)

	s := tr.close(t0.Add(time.Second), 1)
	if s.Frames != 11 || s.Keys != 1 || s.Configs != 1 {
		t.Errorf("frames %d keys %d configs %d, want 11 1 1", s.Frames, s.Keys, s.Configs)
	}
	if s.Bytes != 11*(frameHeaderLen+1000)+frameHeaderLen+30 {
		t.Errorf("bytes %d", s.Bytes)
	}
	if s.PTSMissing != 2 {
		t.Errorf("ptsMissing %d, want the 2 skipped", s.PTSMissing)
	}
	if *s.DelayP50 != 15 || *s.DelayMax != 300 {
		t.Errorf("delay p50 %v max %v, want 15 and 300", *s.DelayP50, *s.DelayMax)
	}
	// The open gap from the last frame (500 ms) to the second's end is the
	// longest, not the 320 ms closed one before it.
	if s.GapMaxMs != 500 {
		t.Errorf("gapMax %v, want 500 (the open gap to the second's end)", s.GapMaxMs)
	}

	// A second with nothing in it still reports, and carries the stall.
	s2 := tr.close(t0.Add(2*time.Second), 2)
	if s2.Frames != 0 || s2.DelayP50 != nil || s2.GapMaxMs != 1500 {
		t.Errorf("empty second: frames %d delay %v gap %v, want 0, nil, 1500", s2.Frames, s2.DelayP50, s2.GapMaxMs)
	}

	// pts going backwards is a relaunch, not a negative skip.
	tr.frameArrived(frame{PTS: 0, SentAt: sentAt(t0.Add(2100*time.Millisecond), 10*time.Millisecond)}, t0.Add(2100*time.Millisecond))
	if s3 := tr.close(t0.Add(3*time.Second), 3); s3.PTSBack != 1 || s3.PTSMissing != 0 {
		t.Errorf("after a pts reset: back %d missing %d, want 1 and 0", s3.PTSBack, s3.PTSMissing)
	}
}

func TestSummaryAcrossSeconds(t *testing.T) {
	keep := func(k float64, h string) *serverStats { return &serverStats{Health: h, Keep: k} }
	d := func(v float64) *float64 { return &v }
	secs := []second{
		{Frames: 60, Keys: 1, delays: []float64{10, 20}, RttNowMs: d(100), Server: keep(1, "good")},
		{Frames: 0, gap: 1500 * time.Millisecond, Server: keep(0.25, "bad")},
		{Frames: 30, delays: []float64{900}, RttNowMs: d(1800), Server: keep(0.5, "degraded")},
	}
	s := summarize(secs, 1200*time.Millisecond)
	if s.Frames != 90 || s.Keys != 1 || s.StallSecs != 1 {
		t.Errorf("frames %d keys %d stalls %d", s.Frames, s.Keys, s.StallSecs)
	}
	if s.KeepMin != 0.25 || s.KeepP50 != 0.5 {
		t.Errorf("keep min %v p50 %v, want 0.25 and 0.5", s.KeepMin, s.KeepP50)
	}
	if s.Health["good"] != 1 || s.Health["bad"] != 1 || s.Health["degraded"] != 1 {
		t.Errorf("health %v", s.Health)
	}
	if s.DelayMax != 900 || s.GapMaxMs != 1500 || s.RttNowMax != 1800 {
		t.Errorf("delay max %v gap %v rtt max %v", s.DelayMax, s.GapMaxMs, s.RttNowMax)
	}
}

func TestViewerURLAddsWhatTheWebClientSends(t *testing.T) {
	got, err := viewerURL("https://127.0.0.1:18466/ws?target=synthetic", true)
	if err != nil {
		t.Fatal(err)
	}
	if got != "wss://127.0.0.1:18466/ws?acks=1&proto=1&target=synthetic" {
		t.Errorf("got %s", got)
	}
	if _, err := viewerURL("wss://127.0.0.1:18466/ws", true); err == nil {
		t.Error("accepted a URL with no target")
	}
}
