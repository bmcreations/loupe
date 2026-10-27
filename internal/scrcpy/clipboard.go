package scrcpy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Device message types, from scrcpy-server 3.3.4's DeviceMessageWriter: what
// the server writes back up the control socket.
const (
	deviceMsgClipboard    byte = 0
	deviceMsgAckClipboard byte = 1
	deviceMsgUhidOutput   byte = 2
)

// ClipboardTextMax is the most text AppendSetClipboard sends, in bytes. It
// is the scrcpy client's own limit: a control message is at most 256KiB, and
// a set-clipboard message spends 14 bytes on its tag, sequence, paste flag
// and length.
const ClipboardTextMax = 1<<18 - 14

// deviceClipboardMax is the most text the server writes in one clipboard
// message: DeviceMessageWriter truncates at 0x3fffb bytes. Anything larger
// is a desynchronised stream, not a clipboard.
const deviceClipboardMax = 0x3fffb

// AppendSetClipboard appends a message that sets the device's clipboard:
// the tag, a u64 sequence, a paste byte, and the text behind a u32 length.
// Read out of ControlMessageReader.parseSetClipboard. With paste set, the
// server also presses paste in whatever has focus, which types the text
// whole, characters inject-text cannot type among it.
//
// A sequence of 0 asks for no acknowledgement.
func AppendSetClipboard(buf []byte, seq uint64, text string, paste bool) ([]byte, error) {
	if len(text) > ClipboardTextMax {
		return buf, fmt.Errorf("set clipboard: text is %d bytes, the limit is %d", len(text), ClipboardTextMax)
	}
	buf = append(buf, typeSetClipboard)
	buf = binary.BigEndian.AppendUint64(buf, seq)
	p := byte(0)
	if paste {
		p = 1
	}
	buf = append(buf, p)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(text)))
	return append(buf, text...), nil
}

// AppendGetClipboard appends a message asking the server for the device's
// clipboard, which it answers with a clipboard device message. copyKey 0
// reads it as it is; 1 and 2 press copy or cut first.
func AppendGetClipboard(buf []byte, copyKey byte) []byte {
	return append(buf, typeGetClipboard, copyKey)
}

// DeviceMessage is one message the server sent up the control socket. Only
// the fields of its type are set.
type DeviceMessage struct {
	Type byte
	// Text is a clipboard message's contents.
	Text string
	// Seq is an acknowledgement's sequence.
	Seq uint64
	// ID and Data are a UHID device's output report.
	ID   uint16
	Data []byte
}

// IsClipboard reports whether m carries the device's clipboard.
func (m DeviceMessage) IsClipboard() bool { return m.Type == deviceMsgClipboard }

var errUnknownDeviceMessage = errors.New("scrcpy: unknown device message type")

// ReadDeviceMessage reads one message the server wrote with
// DeviceMessageWriter: a type byte, then a clipboard's u32 length and
// UTF-8, an acknowledgement's u64 sequence, or a UHID output's u16 id and
// u16-length data.
//
// An unknown type is an error rather than something to skip, because the
// format has no length to skip it by.
func ReadDeviceMessage(r io.Reader) (DeviceMessage, error) {
	var t [1]byte
	if _, err := io.ReadFull(r, t[:]); err != nil {
		return DeviceMessage{}, err
	}
	m := DeviceMessage{Type: t[0]}
	switch m.Type {
	case deviceMsgClipboard:
		var n [4]byte
		if _, err := io.ReadFull(r, n[:]); err != nil {
			return m, err
		}
		size := binary.BigEndian.Uint32(n[:])
		if size > deviceClipboardMax {
			return m, fmt.Errorf("scrcpy: clipboard message of %d bytes, stream desynchronized", size)
		}
		b := make([]byte, size)
		if _, err := io.ReadFull(r, b); err != nil {
			return m, err
		}
		m.Text = string(b)
	case deviceMsgAckClipboard:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return m, err
		}
		m.Seq = binary.BigEndian.Uint64(b[:])
	case deviceMsgUhidOutput:
		var h [4]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return m, err
		}
		m.ID = binary.BigEndian.Uint16(h[:2])
		m.Data = make([]byte, binary.BigEndian.Uint16(h[2:]))
		if _, err := io.ReadFull(r, m.Data); err != nil {
			return m, err
		}
	default:
		return m, fmt.Errorf("%w %d", errUnknownDeviceMessage, m.Type)
	}
	return m, nil
}
