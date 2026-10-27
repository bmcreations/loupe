package composetree

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

type rw struct {
	in  *bytes.Reader
	out bytes.Buffer
}

func (r *rw) Read(p []byte) (int, error)  { return r.in.Read(p) }
func (r *rw) Write(p []byte) (int, error) { return r.out.Write(p) }

func frame(status byte, body []byte) []byte {
	b := []byte{status, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(b[1:], uint32(len(body)))
	return append(b, body...)
}

func TestCallFramesTheRequestAndReadsTheReply(t *testing.T) {
	c := &rw{in: bytes.NewReader(frame(0, []byte("ok")))}
	got, err := Call(c, 'L', []byte("/x.jar"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ok" {
		t.Fatalf("body = %q", got)
	}
	if want := "L\x00\x00\x00\x06/x.jar"; c.out.String() != want {
		t.Fatalf("request = %q, want %q", c.out.String(), want)
	}
}

func TestCallReturnsTheAgentsError(t *testing.T) {
	c := &rw{in: bytes.NewReader(frame(1, []byte("no inspector loaded")))}
	_, err := Call(c, 'C', nil)
	if err == nil || !strings.Contains(err.Error(), "no inspector loaded") {
		t.Fatalf("err = %v", err)
	}
}

func TestWindowsSplitsTheReply(t *testing.T) {
	var b []byte
	b = binary.BigEndian.AppendUint32(b, 2)
	b = binary.BigEndian.AppendUint32(b, 3)
	b = append(b, "abc"...)
	b = binary.BigEndian.AppendUint32(b, 0)
	got, err := Windows(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || string(got[0]) != "abc" || len(got[1]) != 0 {
		t.Fatalf("windows = %q", got)
	}
	if _, err := Windows(b[:6]); err == nil {
		t.Fatal("want an error for a truncated reply")
	}
}
