package emugrpc

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
)

// The want bytes are protoc's: each text message below piped through
// protoc --encode=android.emulation.control.<Message> emulator_controller.proto
// with the proto from emulator 37.3.6's emulator/lib.
func TestMarshalMatchesProtoc(t *testing.T) {
	audio := []byte{0x01, 0x02, 0xff, 0x80}
	cases := []struct {
		name string
		got  []byte
		want string
	}{
		{
			// format { samplingRate: 16000 channels: Mono format: AUD_FMT_S16
			// mode: MODE_REAL_TIME } timestamp: 0 audio: "\x01\x02\xff\x80"
			name: "first packet",
			got: AudioPacket{
				Format: &AudioFormat{SamplingRate: 16000, Channels: Mono, Format: S16, Mode: ModeRealTime},
				Audio:  audio,
			}.Marshal(),
			want: "0a0708807d180120011a040102ff80",
		},
		{
			// timestamp: 20000 audio: "\x01\x02\xff\x80"
			name: "later packet",
			got:  AudioPacket{Timestamp: 20000, Audio: audio}.Marshal(),
			want: "10a09c011a040102ff80",
		},
		{
			// samplingRate: 48000 channels: Stereo format: AUD_FMT_U8
			// mode: MODE_UNSPECIFIED
			name: "format with zero enums",
			got:  AudioFormat{SamplingRate: 48000, Channels: Stereo, Format: U8, Mode: ModeUnspecified}.Marshal(),
			want: "0880f7021001",
		},
		{
			// timestamp: 1700000000000000
			name: "long timestamp, no audio",
			got:  AudioPacket{Timestamp: 1700000000000000}.Marshal(),
			want: "108080f9c0c1c48203",
		},
		{
			// format { }
			name: "empty format",
			got:  AudioPacket{Format: &AudioFormat{}}.Marshal(),
			want: "0a00",
		},
	}
	for _, c := range cases {
		if got := hex.EncodeToString(c.got); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestFrame(t *testing.T) {
	msg := AudioPacket{Timestamp: 20000, Audio: []byte{0x01, 0x02, 0xff, 0x80}}.Marshal()
	got := appendFrame(nil, msg)
	want, _ := hex.DecodeString("000000000a" + "10a09c011a040102ff80")
	if !bytes.Equal(got, want) {
		t.Fatalf("frame %x, want %x", got, want)
	}

	// Empty, google.protobuf.Empty's encoding, then a 300-byte message:
	// the length is big-endian.
	big := bytes.Repeat([]byte{7}, 300)
	body := appendFrame(appendFrame(nil, nil), big)
	if !bytes.Equal(body[5:10], []byte{0, 0, 0, 1, 0x2c}) {
		t.Fatalf("300-byte header %x, want 000000012c", body[5:10])
	}
	msgs, err := splitFrames(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || len(msgs[0]) != 0 || !bytes.Equal(msgs[1], big) {
		t.Fatalf("split %d messages, lengths %v", len(msgs), lens(msgs))
	}
}

func TestSplitFramesRejects(t *testing.T) {
	for name, body := range map[string][]byte{
		"short header":     {0, 0, 0},
		"short message":    {0, 0, 0, 0, 4, 1, 2},
		"compressed":       {1, 0, 0, 0, 0},
		"trailing garbage": append(appendFrame(nil, []byte{1}), 0),
	} {
		_, err := splitFrames(body)
		if err == nil {
			t.Errorf("%s: no error", name)
		}
		if name != "compressed" && !errors.Is(err, errShortFrame) {
			t.Errorf("%s: %v, want errShortFrame", name, err)
		}
	}
}

func lens(msgs [][]byte) []int {
	var n []int
	for _, m := range msgs {
		n = append(n, len(m))
	}
	return n
}
