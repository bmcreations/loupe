package scrcpy

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// The layouts are ControlMessageReader's and DeviceMessageWriter's in the
// pinned 3.3.4 jar, so these bytes are written out by hand rather than
// derived from the encoder under test.
func TestSetClipboardLayout(t *testing.T) {
	got, err := AppendSetClipboard(nil, 7, "hé", true)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{9, 0, 0, 0, 0, 0, 0, 0, 7, 1, 0, 0, 0, 3, 'h', 0xc3, 0xa9}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % x, want % x", got, want)
	}
	if _, err := AppendSetClipboard(nil, 0, strings.Repeat("x", ClipboardTextMax+1), false); err == nil {
		t.Fatal("text over the limit was encoded")
	}
}

func TestGetClipboardLayout(t *testing.T) {
	if got := AppendGetClipboard(nil, 0); !bytes.Equal(got, []byte{8, 0}) {
		t.Fatalf("got % x", got)
	}
}

func TestReadDeviceMessages(t *testing.T) {
	var b bytes.Buffer
	b.WriteByte(0)
	b.Write(binary.BigEndian.AppendUint32(nil, 5))
	b.WriteString("hello")
	b.WriteByte(1)
	b.Write(binary.BigEndian.AppendUint64(nil, 42))
	b.WriteByte(2)
	b.Write([]byte{0, 3, 0, 1, 0xaa})
	b.WriteByte(0)
	b.Write(binary.BigEndian.AppendUint32(nil, 0))

	m, err := ReadDeviceMessage(&b)
	if err != nil || !m.IsClipboard() || m.Text != "hello" {
		t.Fatalf("clipboard: %+v %v", m, err)
	}
	m, err = ReadDeviceMessage(&b)
	if err != nil || m.Seq != 42 {
		t.Fatalf("ack: %+v %v", m, err)
	}
	m, err = ReadDeviceMessage(&b)
	if err != nil || m.ID != 3 || !bytes.Equal(m.Data, []byte{0xaa}) {
		t.Fatalf("uhid: %+v %v", m, err)
	}
	// An empty clipboard is still a clipboard message, not the end.
	m, err = ReadDeviceMessage(&b)
	if err != nil || !m.IsClipboard() || m.Text != "" {
		t.Fatalf("empty clipboard: %+v %v", m, err)
	}
}

func TestReadDeviceMessageRejectsWhatItCannotSkip(t *testing.T) {
	if _, err := ReadDeviceMessage(bytes.NewReader([]byte{9})); err == nil {
		t.Fatal("unknown type read as a message")
	}
	huge := append([]byte{0}, binary.BigEndian.AppendUint32(nil, deviceClipboardMax+1)...)
	if _, err := ReadDeviceMessage(bytes.NewReader(huge)); err == nil {
		t.Fatal("oversized clipboard read as a message")
	}
}
