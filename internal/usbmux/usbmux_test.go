package usbmux

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// fakeMuxd answers ListDevices with one phone on USB and Connect with
// result, recording the Connect request's fields, then echoes bytes.
func fakeMuxd(t *testing.T, result int) chan map[string]any {
	t.Helper()
	dir, err := os.MkdirTemp("", "mux")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	old := SocketPath
	SocketPath = path
	t.Cleanup(func() { SocketPath = old })

	got := make(chan map[string]any, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				v, err := recv(c)
				if err != nil {
					return
				}
				m := v.(map[string]any)
				got <- m
				switch m["MessageType"] {
				case "ListDevices":
					writeRaw(c, `<plist version="1.0"><dict><key>DeviceList</key><array>
<dict><key>DeviceID</key><integer>9</integer><key>Properties</key><dict>
<key>ConnectionType</key><string>Network</string><key>SerialNumber</key><string>00008140-0004583C22D8801C</string></dict></dict>
<dict><key>DeviceID</key><integer>4</integer><key>Properties</key><dict>
<key>ConnectionType</key><string>USB</string><key>SerialNumber</key><string>00008140001A45020C81401C</string></dict></dict>
</array></dict></plist>`)
				case "Connect":
					writeRaw(c, `<plist version="1.0"><dict><key>MessageType</key><string>Result</string><key>Number</key><integer>`+itoa(result)+`</integer></dict></plist>`)
					if result == 0 {
						_, _ = io.Copy(c, c)
					}
				}
			}(c)
		}
	}()
	return got
}

func itoa(n int) string { return string(rune('0' + n)) }

func writeRaw(w io.Writer, body string) {
	var hdr [headerLen]byte
	n := uint32(headerLen + len(body))
	hdr[0], hdr[1], hdr[2], hdr[3] = byte(n), byte(n>>8), byte(n>>16), byte(n>>24)
	_, _ = w.Write(append(hdr[:], body...))
}

func TestConnectFindsTheUSBPhoneAndSendsThePortInNetworkOrder(t *testing.T) {
	got := fakeMuxd(t, 0)
	c, err := Connect(context.Background(), "00008140-001A45020C81401C", 7110)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	<-got // ListDevices
	req := <-got
	if req["DeviceID"] != 4 {
		t.Errorf("DeviceID = %v; want 4, the phone on USB", req["DeviceID"])
	}
	// 7110 is 0x1BC6; on the wire it is 0xC61B.
	if req["PortNumber"] != 0xC61B {
		t.Errorf("PortNumber = %#x; want 0xc61b", req["PortNumber"])
	}
	if _, err := c.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping\n" {
		t.Fatalf("after Connect the socket should be the byte stream: %q %v", buf, err)
	}
}

func TestConnectRefusedIsAResultError(t *testing.T) {
	fakeMuxd(t, 3)
	_, err := Connect(context.Background(), "00008140001A45020C81401C", 47999)
	var re *ResultError
	if !errors.As(err, &re) || re.Number != 3 {
		t.Fatalf("err = %v; want ResultError 3", err)
	}
}

func TestConnectIgnoresAPhoneOnlyOnTheNetwork(t *testing.T) {
	fakeMuxd(t, 0)
	_, err := Connect(context.Background(), "00008140-0004583C22D8801C", 7110)
	if !errors.Is(err, ErrNotOnUSB) {
		t.Fatalf("err = %v; want ErrNotOnUSB", err)
	}
}
