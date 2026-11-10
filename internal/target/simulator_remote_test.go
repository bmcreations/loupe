package target

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

type nopCloser struct{ bytes.Buffer }

func (*nopCloser) Close() error { return nil }

func TestRemoteOpsWriteContractBytes(t *testing.T) {
	in := &nopCloser{}
	s := &simStream{stdin: in, remote: make(chan Frame, 1)}
	for _, err := range []error{
		s.RemoteGate(true), s.RemoteGate(false), s.RemoteKeyframe(),
		s.RemoteCap(4_000_000), s.RemoteMaxSize(720),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	want := []byte{0x82, 1, 0x82, 0, 0x83, 0x84, 0, 0x3d, 0x09, 0x00, 0x85, 0x02, 0xd0}
	if !bytes.Equal(in.Bytes(), want) {
		t.Fatalf("stdin = % x, want % x", in.Bytes(), want)
	}
}

func TestRemoteOpsRefusedWithoutRemoteTrack(t *testing.T) {
	in := &nopCloser{}
	s := &simStream{stdin: in}
	if s.Remote() != nil {
		t.Fatal("Remote() should be nil")
	}
	if s.RemoteGate(true) == nil || s.RemoteKeyframe() == nil || s.RemoteCap(1) == nil || s.RemoteMaxSize(1) == nil {
		t.Fatal("expected errors")
	}
	if in.Len() != 0 {
		t.Fatalf("wrote % x", in.Bytes())
	}
}

func packet(pts uint64, flags uint64, data []byte) []byte {
	b := make([]byte, 12, 12+len(data))
	binary.BigEndian.PutUint64(b, pts|flags)
	binary.BigEndian.PutUint32(b[8:], uint32(len(data)))
	return append(b, data...)
}

func TestReadRemoteParsesPackets(t *testing.T) {
	s := &simStream{stdin: &nopCloser{}, remote: make(chan Frame, 8)}
	var buf bytes.Buffer
	buf.Write(packet(0, 1<<63, []byte{0, 0, 0, 1, 0x67}))
	buf.Write(packet(1000, 1<<62, []byte{0, 0, 0, 1, 0x65}))
	buf.Write(packet(2000, 0, []byte{0, 0, 0, 1, 0x41}))
	s.readRemote(&buf)
	c, k, d := <-s.remote, <-s.remote, <-s.remote
	if !c.Config || !k.Key || d.Key || d.Config || d.PTS != 2*time.Millisecond {
		t.Fatalf("frames %+v %+v %+v", c, k, d)
	}
}

func TestReadRemoteResumesAtKeyframeWhenFull(t *testing.T) {
	in := &nopCloser{}
	s := &simStream{stdin: in, remote: make(chan Frame, 1)}
	var buf bytes.Buffer
	buf.Write(packet(0, 1<<62, []byte{1}))
	buf.Write(packet(1, 0, []byte{2})) // channel full: dropped, waits for key
	buf.Write(packet(2, 0, []byte{3})) // skipped while waiting
	s.readRemote(&buf)
	if got := len(s.remote); got != 1 {
		t.Fatalf("queued %d", got)
	}
}

func TestRemoteTrackWanted(t *testing.T) {
	cases := []struct {
		env  string
		caps bool
		want bool
	}{{"", false, false}, {"", true, true}, {"off", true, false}, {"0", true, false}, {"on", false, true}, {"1", false, true}}
	for _, c := range cases {
		t.Setenv("LOUPE_REMOTE_TRACK", c.env)
		if got := remoteTrackWanted(c.caps); got != c.want {
			t.Errorf("env %q caps %v: got %v", c.env, c.caps, got)
		}
	}
}
