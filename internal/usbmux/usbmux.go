// Package usbmux opens a TCP connection to a port on an iPhone over its USB
// cable, through macOS's own usbmuxd at /var/run/usbmuxd. It is what
// iproxy does, without iproxy: one Unix socket and a plist request, after
// which the socket is a byte stream to the phone's port.
//
// The phone end is the device's loopback, so a listener on the phone's
// 127.0.0.1 is reachable this way and from nowhere else. Only a device on
// a cable is listed with a USB connection; over Wi-Fi this has nothing to
// offer.
package usbmux

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
)

// SocketPath is usbmuxd's socket. A var so tests can point it elsewhere.
var SocketPath = "/var/run/usbmuxd"

// The header is four little-endian u32s: the whole message's length,
// protocol version 1 (plist), message type 8 (plist) and a tag the reply
// echoes.
const (
	headerLen   = 16
	versionPL   = 1
	messagePL   = 8
	maxReplyLen = 1 << 20
)

// Device is one phone usbmuxd lists.
type Device struct {
	ID int
	// Serial is the UDID. For an iPhone with a dashed UDID (00008140-...)
	// usbmuxd may report it with or without the dash; Match handles both.
	Serial         string
	ConnectionType string // "USB" or "Network"
}

// Match reports whether d is the device with this UDID.
func (d Device) Match(udid string) bool {
	norm := func(s string) string { return strings.ToUpper(strings.ReplaceAll(s, "-", "")) }
	return norm(d.Serial) == norm(udid)
}

// ResultError is usbmuxd refusing a request. Number 3 on a Connect is
// ECONNREFUSED: nothing is listening on that port on the phone.
type ResultError struct{ Number int }

func (e *ResultError) Error() string {
	if e.Number == 3 {
		return "usbmuxd: connection refused by the device (nothing listening)"
	}
	return "usbmuxd: result " + strconv.Itoa(e.Number)
}

// ErrNotOnUSB is a device usbmuxd does not list on a cable.
var ErrNotOnUSB = errors.New("usbmuxd does not list this device on USB")

func dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", SocketPath)
}

// List returns the devices usbmuxd knows, on a cable or the network.
func List(ctx context.Context) ([]Device, error) {
	c, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	stopCancel := closeOnDone(ctx, c)
	defer stopCancel()
	if err := send(c, map[string]any{"MessageType": "ListDevices"}); err != nil {
		return nil, err
	}
	v, err := recv(c)
	if err != nil {
		return nil, err
	}
	return parseDeviceList(v)
}

func parseDeviceList(v any) ([]Device, error) {
	top, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("usbmuxd: ListDevices reply is not a dict")
	}
	list, _ := top["DeviceList"].([]any)
	var out []Device
	for _, e := range list {
		m, _ := e.(map[string]any)
		props, _ := m["Properties"].(map[string]any)
		if props == nil {
			continue
		}
		// usbmuxd puts DeviceID beside Properties and inside it.
		id, ok := m["DeviceID"].(int)
		if !ok {
			id, _ = props["DeviceID"].(int)
		}
		serial, _ := props["SerialNumber"].(string)
		ct, _ := props["ConnectionType"].(string)
		out = append(out, Device{ID: id, Serial: serial, ConnectionType: ct})
	}
	return out, nil
}

// Connect opens port on the phone with this UDID, over USB only.
func Connect(ctx context.Context, udid string, port int) (net.Conn, error) {
	devs, err := List(ctx)
	if err != nil {
		return nil, err
	}
	id := -1
	for _, d := range devs {
		if d.ConnectionType == "USB" && d.Match(udid) {
			id = d.ID
			break
		}
	}
	if id < 0 {
		return nil, ErrNotOnUSB
	}
	return ConnectID(ctx, id, port)
}

// ConnectID opens port on the device usbmuxd numbers id.
func ConnectID(ctx context.Context, id, port int) (net.Conn, error) {
	c, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	stopCancel := closeOnDone(ctx, c)
	err = func() error {
		err := send(c, map[string]any{
			"MessageType": "Connect",
			"DeviceID":    id,
			// The port goes in network byte order inside the integer.
			"PortNumber": int(swap16(uint16(port))),
		})
		if err != nil {
			return err
		}
		v, err := recv(c)
		if err != nil {
			return err
		}
		return resultOf(v)
	}()
	stopCancel()
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func swap16(v uint16) uint16 { return v<<8 | v>>8 }

func resultOf(v any) error {
	m, ok := v.(map[string]any)
	if !ok || m["MessageType"] != "Result" {
		return fmt.Errorf("usbmuxd: unexpected reply %v", v)
	}
	n, _ := m["Number"].(int)
	if n != 0 {
		return &ResultError{Number: n}
	}
	return nil
}

// closeOnDone closes c when ctx ends, until the returned func is called.
func closeOnDone(ctx context.Context, c net.Conn) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

func send(w io.Writer, msg map[string]any) error {
	msg["ClientVersionString"] = "loupe"
	msg["ProgName"] = "loupe"
	body := encodePlist(msg)
	var hdr [headerLen]byte
	binary.LittleEndian.PutUint32(hdr[0:], uint32(headerLen+len(body)))
	binary.LittleEndian.PutUint32(hdr[4:], versionPL)
	binary.LittleEndian.PutUint32(hdr[8:], messagePL)
	binary.LittleEndian.PutUint32(hdr[12:], 1)
	_, err := w.Write(append(hdr[:], body...))
	return err
}

func recv(r io.Reader) (any, error) {
	var hdr [headerLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("usbmuxd: read header: %w", err)
	}
	n := binary.LittleEndian.Uint32(hdr[0:])
	if n < headerLen || n > maxReplyLen {
		return nil, fmt.Errorf("usbmuxd: reply length %d", n)
	}
	body := make([]byte, n-headerLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("usbmuxd: read body: %w", err)
	}
	return decodePlist(body)
}

// encodePlist writes a flat dict of strings and ints, which is every
// request this package sends.
func encodePlist(m map[string]any) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0"><dict>`)
	// Sorted, so the bytes are stable for tests.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		b.WriteString("<key>")
		xml.EscapeText(&b, []byte(k))
		b.WriteString("</key>")
		switch v := m[k].(type) {
		case int:
			b.WriteString("<integer>" + strconv.Itoa(v) + "</integer>")
		case string:
			b.WriteString("<string>")
			xml.EscapeText(&b, []byte(v))
			b.WriteString("</string>")
		}
	}
	b.WriteString("</dict></plist>\n")
	return b.Bytes()
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// decodePlist reads an XML plist's dict, array, string, integer, data
// ([]byte), true and false; date and real are skipped as nil. That covers
// usbmuxd's and lockdownd's replies.
func decodePlist(body []byte) (any, error) {
	d := xml.NewDecoder(bytes.NewReader(body))
	for {
		tok, err := d.Token()
		if err != nil {
			return nil, fmt.Errorf("usbmuxd: plist: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local != "plist" {
			return decodeValue(d, se)
		}
	}
}

func decodeValue(d *xml.Decoder, se xml.StartElement) (any, error) {
	switch se.Name.Local {
	case "dict":
		m := map[string]any{}
		var key string
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				if t.Name.Local == "key" {
					var s string
					if err := d.DecodeElement(&s, &t); err != nil {
						return nil, err
					}
					key = s
					continue
				}
				v, err := decodeValue(d, t)
				if err != nil {
					return nil, err
				}
				m[key] = v
			case xml.EndElement:
				return m, nil
			}
		}
	case "array":
		var a []any
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				v, err := decodeValue(d, t)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			case xml.EndElement:
				return a, nil
			}
		}
	case "true", "false":
		if err := d.Skip(); err != nil {
			return nil, err
		}
		return se.Name.Local == "true", nil
	}
	var s string
	if err := d.DecodeElement(&s, &se); err != nil {
		return nil, err
	}
	switch se.Name.Local {
	case "string":
		return s, nil
	case "integer":
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return nil, err
		}
		return n, nil
	case "data":
		// Base64 broken over lines and indented with tabs.
		b, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
		if err != nil {
			return nil, err
		}
		return b, nil
	}
	return nil, nil
}
