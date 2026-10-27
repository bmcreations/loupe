package httpapi

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bmcreations/loupe/internal/session"
)

// goldenBytes reads a fixture from testdata/protocol/<name>. See the
// identical helper in internal/session/golden_test.go for the format.
func goldenBytes(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "protocol", name)

	if !strings.HasSuffix(name, ".hex") {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return b
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var out []byte
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		b, err := hex.DecodeString(line)
		if err != nil {
			t.Fatalf("%s: bad hex line %q: %v", name, line, err)
		}
		out = append(out, b...)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return out
}

// TestFrameGoldenWT pins the 13-byte WebTransport frame header against
// testdata/protocol/frame-wt.hex -- the same access unit as
// frame-ws.hex's MsgFrame, but with no type byte and no length prefix,
// since the stream's own FIN delimits the frame.
func TestFrameGoldenWT(t *testing.T) {
	want := goldenBytes(t, "frame-wt.hex")
	got := wtEncodeFrameHeader(1, 352074200000, true, false)
	if !bytes.Equal(got, want) {
		t.Errorf("frame-wt.hex mismatch:\n got  %x\n want %x", got, want)
	}
}

// TestFrameGoldenWTConfig pins the WT header for a config frame through
// wtConn.nextFrameHeader -- the same call SendFrame makes -- rather than
// through wtEncodeFrameHeader directly, so the fixture also proves SendFrame
// itself reaches the encoder with the config flag set and not just that the
// encoder produces the right bytes in isolation.
func TestFrameGoldenWTConfig(t *testing.T) {
	want := goldenBytes(t, "frame-wt-config.hex")
	c := &wtConn{}
	_, got := c.nextFrameHeader(session.OutMsg{PTSMicros: 0, Key: false, Config: true})
	if !bytes.Equal(got, want) {
		t.Errorf("frame-wt-config.hex mismatch:\n got  %x\n want %x", got, want)
	}
}

// TestWTControlHelloGolden pins the framing every WebTransport control
// message uses in both directions -- init, stats, pong, event on the way
// out; touch, key, scroll, ping, hello on the way in -- a u32 big-endian
// length prefix ahead of the message, against the simplest message there
// is: an empty-bodied hello.
func TestWTControlHelloGolden(t *testing.T) {
	want := goldenBytes(t, "wt-control-hello.hex")
	got := wtFrameControl([]byte{session.MsgHello})
	if !bytes.Equal(got, want) {
		t.Errorf("wt-control-hello.hex mismatch:\n got  %x\n want %x", got, want)
	}
}

// TestWTReadControlFrameRoundTrips checks wtReadControlFrame against what
// wtFrameControl writes, for a message longer than one byte, and confirms
// the length-prefix framing survives a round trip rather than only ever
// being tested one direction at a time.
func TestWTReadControlFrameRoundTrips(t *testing.T) {
	msg := append([]byte{session.MsgTouch}, []byte{0x00, 0x00, 0x01, 0x86, 0xa0}...)
	framed := wtFrameControl(msg)
	got, _, err := wtReadControlFrame(bytes.NewReader(framed), nil)
	if err != nil {
		t.Fatalf("wtReadControlFrame: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Errorf("round trip mismatch:\n got  %x\n want %x", got, msg)
	}
}

// TestWTReadControlFrameRejectsOversizedLength confirms the server refuses a
// length prefix over wtControlLimit (1 MiB) -- every real control message is
// tens of bytes, so a length near this cap is either a bug or an attacker
// probing for an unbounded allocation -- and accepts one of exactly
// wtControlLimit.
//
// The oversized case supplies a full body (wtControlLimit+1 bytes)
// deliberately: with only a truncated body, a deleted length check would
// still fail with an unrelated EOF from the short read and the test would
// pass for the wrong reason. With a full body present, a deleted check lets
// the read succeed, so this only fails closed when the length itself is
// rejected -- and it checks the error names the limit, not just that some
// error occurred.
func TestWTReadControlFrameRejectsOversizedLength(t *testing.T) {
	// The 1 MiB figure is spelled out as a literal, not read back off
	// wtControlLimit: if the two boundary cases were built from the
	// constant under test, an off-by-one (or any other) change to the
	// constant would move both the input and the expectation together and
	// this test would keep passing no matter what the cap actually was.
	const oneMiB = 1 << 20
	if wtControlLimit != oneMiB {
		t.Fatalf("wtControlLimit = %d, this test assumes 1 MiB (%d) -- update it", wtControlLimit, oneMiB)
	}

	t.Run("over the limit is rejected", func(t *testing.T) {
		var prefix [4]byte
		binary.BigEndian.PutUint32(prefix[:], oneMiB+1)
		body := make([]byte, oneMiB+1)
		r := bytes.NewReader(append(prefix[:], body...))

		_, _, err := wtReadControlFrame(r, nil)
		if err == nil {
			t.Fatal("wtReadControlFrame accepted a length over wtControlLimit, want an error")
		}
		if !strings.Contains(err.Error(), "exceeds limit") {
			t.Errorf("error = %q, want it to name the limit", err)
		}
	})

	t.Run("exactly the limit is accepted", func(t *testing.T) {
		var prefix [4]byte
		binary.BigEndian.PutUint32(prefix[:], oneMiB)
		body := make([]byte, oneMiB)
		r := bytes.NewReader(append(prefix[:], body...))

		got, _, err := wtReadControlFrame(r, nil)
		if err != nil {
			t.Fatalf("wtReadControlFrame rejected a length of exactly wtControlLimit: %v", err)
		}
		if len(got) != oneMiB {
			t.Errorf("len(got) = %d, want %d", len(got), oneMiB)
		}
	})
}

// TestStatsGolden pins MsgStats' body for a viewer with a current health
// report: health, healthReason, fps, rttMs, baseMs, keep and transport are
// all present. Delivered/Produced here describe a five-report sliding
// window (healthTracker, internal/httpapi/health_track.go), not one
// second's frames, which is why healthReason's numbers ("158 of 175") are
// not close to fps (52) the way a single tick's counts would be -- see
// internal/session/health.go and health_track.go for what each field
// carries.
func TestStatsGolden(t *testing.T) {
	want := goldenBytes(t, "stats.json")
	tick := healthTick{RttMs: 265.1, Delivered: 52}
	body := statsMsgBody(tick, "degraded", "delivered 158 of 175 frames", 3, 233.4, 0.85, "webtransport")
	got, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')
	if !bytes.Equal(got, want) {
		t.Errorf("stats.json mismatch:\n got  %s want %s", got, want)
	}
}

// TestStatsStaleGolden pins the "stale" shape: no report has ever arrived
// for this viewer, or the last one is older than staleAfter, so fps and
// rttMs are dropped entirely rather than reporting a stale or synthetic
// number as though it were current (statsMsgBody).
func TestStatsStaleGolden(t *testing.T) {
	want := goldenBytes(t, "stats-stale.json")
	body := statsMsgBody(healthTick{}, "stale", "no report", 0, 233.4, 1, "websocket")
	got, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')
	if !bytes.Equal(got, want) {
		t.Errorf("stats-stale.json mismatch:\n got  %s want %s", got, want)
	}

	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := decoded["fps"]; ok {
		t.Error("fps present in a stale MsgStats body, want dropped")
	}
	if _, ok := decoded["rttMs"]; ok {
		t.Error("rttMs present in a stale MsgStats body, want dropped")
	}
}
