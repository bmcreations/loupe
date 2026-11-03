package hevc

import "testing"

func TestCodecStringMainProfile(t *testing.T) {
	// NAL header (type 33), then vps id 0 / one sub layer / nesting, then
	// Main profile (idc 1, compatible with 1 and 2), progressive and
	// frame-only constraints (0xB0), level 5.1 (153).
	sps := []byte{0x42, 0x01, 0x01, 0x01, 0x60, 0x00, 0x00, 0x00, 0xb0, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x99, 0xa0}
	got, err := CodecString(sps)
	if err != nil {
		t.Fatal(err)
	}
	if want := "hvc1.1.6.L153.B0"; got != want {
		t.Fatalf("CodecString = %q, want %q", got, want)
	}
}

func TestFindSPSSkipsVPS(t *testing.T) {
	buf := []byte{0, 0, 0, 1, 0x40, 0x01, 0x0c, 0, 0, 0, 1, 0x42, 0x01, 0x01, 0, 0, 0, 1, 0x44, 0x01, 0xc0}
	sps := FindSPS(buf)
	if Type(sps) != NalSPS || len(sps) != 3 {
		t.Fatalf("FindSPS = % x", sps)
	}
}

func TestCodecStringRejectsAPPS(t *testing.T) {
	if _, err := CodecString([]byte{0x44, 0x01, 0xc0}); err == nil {
		t.Fatal("a PPS read as an SPS")
	}
}
