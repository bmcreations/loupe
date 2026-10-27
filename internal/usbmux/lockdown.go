package usbmux

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

// lockdownPort is lockdownd on the phone, the service that starts every
// other one.
const lockdownPort = 62078

// maxLockdownLen bounds one lockdown reply. The largest seen is a few KB.
const maxLockdownLen = 1 << 20

// ErrNotListed is a device usbmuxd does not list at all, on a cable or on
// the network.
var ErrNotListed = errors.New("usbmuxd does not list this device")

// ErrNotPaired is a device with no pair record on this Mac, so lockdownd
// will not start a session for it.
var ErrNotPaired = errors.New("this Mac holds no pair record for the device")

// pairRecord is the part of usbmuxd's pair record a lockdown session needs.
// It is held in memory only: the key never goes to disk, a trace or an
// error.
type pairRecord struct {
	hostID, systemBUID string
	cert               tls.Certificate
}

// readPairRecord asks usbmuxd for the record pairing made for this UDID.
func readPairRecord(ctx context.Context, udid string) (*pairRecord, error) {
	c, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	stop := closeOnDone(ctx, c)
	defer stop()
	if err := send(c, map[string]any{"MessageType": "ReadPairRecord", "PairRecordID": udid}); err != nil {
		return nil, err
	}
	v, err := recv(c)
	if err != nil {
		return nil, err
	}
	m, _ := v.(map[string]any)
	data, _ := m["PairRecordData"].([]byte)
	if data == nil {
		return nil, ErrNotPaired
	}
	return parsePairRecord(data)
}

// parsePairRecord reads an XML pair record. The certificate and key are
// PEM inside <data>. An error names the field, never its contents.
func parsePairRecord(data []byte) (*pairRecord, error) {
	v, err := decodePlist(data)
	if err != nil {
		return nil, fmt.Errorf("pair record: %w", err)
	}
	m, _ := v.(map[string]any)
	pr := &pairRecord{}
	pr.hostID, _ = m["HostID"].(string)
	pr.systemBUID, _ = m["SystemBUID"].(string)
	cert, _ := m["HostCertificate"].([]byte)
	key, _ := m["HostPrivateKey"].([]byte)
	switch {
	case pr.hostID == "":
		return nil, errors.New("pair record: no HostID")
	case pr.systemBUID == "":
		return nil, errors.New("pair record: no SystemBUID")
	case cert == nil || key == nil:
		return nil, errors.New("pair record: no host certificate or key")
	}
	if pr.cert, err = tls.X509KeyPair(cert, key); err != nil {
		return nil, errors.New("pair record: host certificate and key do not load")
	}
	return pr, nil
}

// find picks the device usbmuxd lists for this UDID, a cable before the
// network: usbmuxd lists a phone on both once for each path.
func find(ctx context.Context, udid string) (Device, error) {
	devs, err := List(ctx)
	if err != nil {
		return Device{}, err
	}
	var onNet *Device
	for i, d := range devs {
		if !d.Match(udid) {
			continue
		}
		if d.ConnectionType == "USB" {
			return d, nil
		}
		if onNet == nil {
			onNet = &devs[i]
		}
	}
	if onNet == nil {
		return Device{}, ErrNotListed
	}
	return *onNet, nil
}

// lockdownSend writes one lockdown message: a big-endian u32 length, then
// an XML plist.
func lockdownSend(w io.Writer, m map[string]any) error {
	b := encodePlist(m)
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], uint32(len(b)))
	_, err := w.Write(append(h[:], b...))
	return err
}

func lockdownRecv(r io.Reader) (map[string]any, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, fmt.Errorf("lockdown: read length: %w", err)
	}
	n := binary.BigEndian.Uint32(h[:])
	if n > maxLockdownLen {
		return nil, fmt.Errorf("lockdown: reply length %d", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, fmt.Errorf("lockdown: read reply: %w", err)
	}
	v, err := decodePlist(b)
	if err != nil {
		return nil, fmt.Errorf("lockdown: %w", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("lockdown: reply is not a dict")
	}
	if e, _ := m["Error"].(string); e != "" {
		return m, fmt.Errorf("lockdown: %s", e)
	}
	return m, nil
}

// StartService asks lockdownd on the phone with this UDID to start a
// service, and returns a connection to it. It goes over a cable when there
// is one and the network otherwise, which usbmuxd carries the same way.
func StartService(ctx context.Context, udid, service string) (net.Conn, error) {
	d, err := find(ctx, udid)
	if err != nil {
		return nil, err
	}
	pr, err := readPairRecord(ctx, d.Serial)
	if err != nil {
		return nil, err
	}
	lc, err := ConnectID(ctx, d.ID, lockdownPort)
	if err != nil {
		return nil, fmt.Errorf("lockdown: %w", err)
	}
	defer lc.Close()
	stop := closeOnDone(ctx, lc)
	defer stop()

	port, serviceSSL, err := startService(ctx, lc, pr, service)
	if err != nil {
		return nil, err
	}
	sc, err := ConnectID(ctx, d.ID, port)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", service, err)
	}
	if !serviceSSL {
		return sc, nil
	}
	tc := tls.Client(sc, tlsConfig(pr))
	if err := tc.HandshakeContext(ctx); err != nil {
		sc.Close()
		return nil, fmt.Errorf("%s: tls: %w", service, err)
	}
	return tc, nil
}

// startService runs a lockdown session on lc far enough to start service:
// StartSession, TLS on the same connection, then StartService.
func startService(ctx context.Context, lc net.Conn, pr *pairRecord, service string) (port int, serviceSSL bool, err error) {
	err = lockdownSend(lc, map[string]any{
		"Label": "loupe", "Request": "StartSession",
		"HostID": pr.hostID, "SystemBUID": pr.systemBUID,
	})
	if err != nil {
		return 0, false, err
	}
	r, err := lockdownRecv(lc)
	if err != nil {
		return 0, false, err
	}
	var c io.ReadWriter = lc
	if r["EnableSessionSSL"] == true {
		tc := tls.Client(lc, tlsConfig(pr))
		if err := tc.HandshakeContext(ctx); err != nil {
			return 0, false, fmt.Errorf("lockdown: tls: %w", err)
		}
		c = tc
	}
	if err := lockdownSend(c, map[string]any{"Label": "loupe", "Request": "StartService", "Service": service}); err != nil {
		return 0, false, err
	}
	if r, err = lockdownRecv(c); err != nil {
		return 0, false, fmt.Errorf("%s: %w", service, err)
	}
	port, _ = r["Port"].(int)
	if port <= 0 || port > 0xffff {
		return 0, false, fmt.Errorf("%s: lockdown gave no port", service)
	}
	return port, r["EnableServiceSSL"] == true, nil
}

// tlsConfig is the phone's TLS: the host presents the pair record's
// certificate, and the phone's is self-signed by its own pairing root,
// which nothing on the Mac holds as a CA, so it is not verified. Pairing
// is what authenticates the two ends.
func tlsConfig(pr *pairRecord) *tls.Config {
	return &tls.Config{InsecureSkipVerify: true, Certificates: []tls.Certificate{pr.cert}}
}

// Syslog opens the phone's com.apple.syslog_relay: every process's log,
// as text lines each followed by a NUL, starting with a backlog.
func Syslog(ctx context.Context, udid string) (net.Conn, error) {
	return StartService(ctx, udid, "com.apple.syslog_relay")
}
