package session

import "testing"

// A frame reaches only the viewers on its own track.
func TestBroadcastFiltersOnTrack(t *testing.T) {
	s := newTestSession(t)
	s.pass = &passTrack{fourcc: "h265", codec: "hvc1"}
	s.remote = &remoteTrack{}
	_, main := s.AddViewerWith(ViewerOptions{Local: true})
	_, pass := s.AddViewerWith(ViewerOptions{Decodes: []string{"h265"}})
	_, remote := s.AddViewerWith(ViewerOptions{Acks: true})

	for _, tr := range []trackID{trackMain, trackPassthrough, trackRemote} {
		s.broadcast(OutMsg{Data: []byte{byte(tr) + 1}, Key: true, track: tr, seq: 1})
	}
	for name, c := range map[string]struct {
		ch   <-chan OutMsg
		want trackID
	}{"main": {main, trackMain}, "passthrough": {pass, trackPassthrough}, "remote": {remote, trackRemote}} {
		m := next(t, c.ch)
		if m.track != c.want {
			t.Fatalf("%s viewer got a %v frame", name, m.track)
		}
		for {
			select {
			case m := <-c.ch:
				if !m.Control && m.track != c.want {
					t.Fatalf("%s viewer also got a %v frame", name, m.track)
				}
				continue
			default:
			}
			break
		}
	}
}
