package h264

import "testing"

func TestStartsNewPicture(t *testing.T) {
	tests := []struct {
		name string
		nal  []byte
		want bool
	}{
		// first_mb_in_slice = 0 encodes as a single 1 bit, so the top bit is set.
		{"IDR first slice", []byte{0x65, 0x88, 0x84, 0x00}, true},
		{"non-IDR first slice", []byte{0x41, 0x9A, 0x00, 0x00}, true},
		// first_mb_in_slice > 0 encodes with leading zero bits.
		{"IDR continuation slice", []byte{0x65, 0x0C, 0x84, 0x00}, false},
		{"non-IDR continuation slice", []byte{0x41, 0x22, 0x00, 0x00}, false},
		// Non-VCL NALs never start a picture.
		{"SPS", []byte{0x67, 0xFF, 0x00}, false},
		{"PPS", []byte{0x68, 0xFF, 0x00}, false},
		{"SEI", []byte{0x06, 0xFF, 0x00}, false},
		{"too short", []byte{0x65}, false},
		{"empty", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := StartsNewPicture(tc.nal); got != tc.want {
				t.Errorf("StartsNewPicture(%v) = %v, want %v", tc.nal, got, tc.want)
			}
		})
	}
}

func TestCodecString(t *testing.T) {
	// Baseline profile (0x42), constraints 0xC0, level 4.2 (0x2A).
	sps := []byte{0x67, 0x42, 0xC0, 0x2A, 0xAC, 0x2B}
	got, err := CodecString(sps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "avc1.42C02A"; got != want {
		t.Errorf("CodecString = %q, want %q", got, want)
	}

	if _, err := CodecString([]byte{0x65, 0x42, 0xC0, 0x2A}); err == nil {
		t.Error("expected an error for a non-SPS NAL")
	}
	if _, err := CodecString([]byte{0x67}); err == nil {
		t.Error("expected an error for a truncated SPS")
	}
}

func TestFindSPS(t *testing.T) {
	stream := []byte{
		0, 0, 0, 1, 0x67, 0x42, 0xC0, 0x1F, // SPS
		0, 0, 0, 1, 0x68, 0xCE, 0x3C, 0x80, // PPS
		0, 0, 1, 0x65, 0x88, 0x84, // IDR
	}
	sps := FindSPS(stream)
	if sps == nil {
		t.Fatal("FindSPS returned nil")
	}
	if Type(sps) != NALSPS {
		t.Errorf("found NAL type %d, want SPS", Type(sps))
	}
	codec, err := CodecString(sps)
	if err != nil {
		t.Fatalf("CodecString: %v", err)
	}
	if want := "avc1.42C01F"; codec != want {
		t.Errorf("codec = %q, want %q", codec, want)
	}

	if FindSPS([]byte{0, 0, 0, 1, 0x65, 0x88}) != nil {
		t.Error("expected nil when the stream carries no SPS")
	}
}

func TestSplitAnnexB(t *testing.T) {
	// Mixed three- and four-byte start codes, which both occur in real streams.
	stream := []byte{
		0, 0, 0, 1, 0x67, 0x42, 0xC0,
		0, 0, 1, 0x68, 0xCE,
		0, 0, 0, 1, 0x65, 0x88, 0x84,
	}
	var types []uint8
	SplitAnnexB(stream, func(nal []byte) { types = append(types, Type(nal)) })

	want := []uint8{NALSPS, NALPPS, NALIDR}
	if len(types) != len(want) {
		t.Fatalf("got %d NALs (%v), want %d", len(types), types, len(want))
	}
	for i := range want {
		if types[i] != want[i] {
			t.Errorf("NAL %d: type %d, want %d", i, types[i], want[i])
		}
	}
}
