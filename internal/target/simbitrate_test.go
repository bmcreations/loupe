package target

import (
	"bytes"
	"errors"
	"os/exec"
	"testing"
)

type nopWriteCloser struct{ *bytes.Buffer }

func (nopWriteCloser) Close() error { return nil }

// The command is 0x80 then a big-endian u32, which is what simcap's stdin
// parser reads. Its bytes can include 17, which is why simcap parses rather
// than scanning for TYPE_RESET_VIDEO.
func TestSetBitRateWritesSimcapsCommand(t *testing.T) {
	var buf bytes.Buffer
	s := &simStream{cmd: &exec.Cmd{Path: "simcap"}, stdin: nopWriteCloser{&buf}, live: true}
	if err := s.SetBitRate(0x11_22_33_44); err != nil {
		t.Fatal(err)
	}
	if want := []byte{0x80, 0x11, 0x22, 0x33, 0x44}; !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("wrote % x, want % x", buf.Bytes(), want)
	}
}

func TestSetBitRateRefusesAnOlderSimcap(t *testing.T) {
	var buf bytes.Buffer
	s := &simStream{cmd: &exec.Cmd{Path: "simcap"}, stdin: nopWriteCloser{&buf}}
	if err := s.SetBitRate(1_000_000); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("wrote % x to a simcap that cannot parse it", buf.Bytes())
	}
}
