package httpapi

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
	"github.com/quic-go/quic-go"
)

// fakeOwnerViewer is the viewer id fakeOwner gives the relay in its Init.
const fakeOwnerViewer = 7

// fakeOwner answers the relay's control stream itself: it reads the open
// message and Hello, sends an Init naming fakeOwnerViewer, then waits for
// go and writes msgs, each a whole control message, type byte first. The
// relay's slots are never answered, so no frame arrives.
func fakeOwner(goSend <-chan struct{}, msgs ...[]byte) func(*Server, *quic.Conn) {
	return func(_ *Server, qc *quic.Conn) {
		ctl, err := qc.AcceptStream(qc.Context())
		if err != nil {
			return
		}
		var kind [1]byte
		if _, err := io.ReadFull(ctl, kind[:]); err != nil {
			return
		}
		for range 2 { // QUICOpen, then Hello
			var n [4]byte
			if _, err := io.ReadFull(ctl, n[:]); err != nil {
				return
			}
			if _, err := io.CopyN(io.Discard, ctl, int64(binary.BigEndian.Uint32(n[:]))); err != nil {
				return
			}
		}
		write := func(b []byte) {
			_, _ = ctl.Write(append(binary.BigEndian.AppendUint32(nil, uint32(len(b))), b...))
		}
		init, _ := json.Marshal(session.InitPayload{Codec: "avc1.42E01E", CodedWidth: 360, CodedHeight: 780, Name: "Rec 1", Viewer: fakeOwnerViewer, Control: true})
		write(append([]byte{session.MsgInit}, init...))
		select {
		case <-goSend:
		case <-qc.Context().Done():
			return
		}
		for _, m := range msgs {
			write(m)
		}
		<-qc.Context().Done()
	}
}

func ownerMsg(t *testing.T, typ byte, e session.Event) []byte {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte{typ}, b...)
}

func addFakeOwner(t *testing.T, serve func(*Server, *quic.Conn)) (*quicOwner, *hub) {
	t.Helper()
	o := startQUICOwner(t, serve)
	h := quicHub(t)
	if w := h.add(t, "mini", o.url, "tok"); w.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
	eventually(t, "mini/rec listed", func() bool { return find(h.targets(t, ""), "mini/rec") != nil })
	return o, h
}

func TestHubViewersGetTheOwnersClipboardCursorAndEvents(t *testing.T) {
	goSend := make(chan struct{})
	_, h := addFakeOwner(t, fakeOwner(goSend,
		append([]byte{session.MsgClipboard}, "copied on the owner"...),
		ownerMsg(t, session.MsgCursor, session.Event{T: 1, Source: "agent", Agent: "bot", Kind: "move", From: &session.Point{X: 0.25, Y: 0.75}}),
		// The relay's own viewer: a hub viewer's gesture, already published
		// on the hub.
		ownerMsg(t, session.MsgEvent, session.Event{Seq: 40, T: 2, Source: "viewer", Viewer: fakeOwnerViewer, Kind: "tap", From: &session.Point{X: 0.1, Y: 0.1}}),
		ownerMsg(t, session.MsgEvent, session.Event{Seq: 41, T: 3, Source: "viewer", Viewer: 9, Kind: "swipe", From: &session.Point{X: 0.2, Y: 0.2}, To: &session.Point{X: 0.3, Y: 0.3}, Ms: 300}),
	))

	sess, err := h.mgr.Acquire(context.Background(), "mini/rec")
	if err != nil {
		t.Fatal(err)
	}
	defer h.mgr.Release("mini/rec")
	_, out := sess.AddViewer()
	close(goSend)

	var clip string
	var cursor, event *session.Event
	deadline := time.After(10 * time.Second)
	for clip == "" || cursor == nil || event == nil {
		select {
		case m := <-out:
			if !m.Control || len(m.Data) == 0 {
				continue
			}
			switch m.Data[0] {
			case session.MsgClipboard:
				clip = string(m.Data[1:])
			case session.MsgCursor:
				cursor = &session.Event{}
				if err := json.Unmarshal(m.Data[1:], cursor); err != nil {
					t.Fatal(err)
				}
			case session.MsgEvent:
				if event != nil {
					t.Fatalf("a second event: %s", m.Data[1:])
				}
				event = &session.Event{}
				if err := json.Unmarshal(m.Data[1:], event); err != nil {
					t.Fatal(err)
				}
			}
		case <-deadline:
			t.Fatalf("clipboard=%q cursor=%v event=%v", clip, cursor, event)
		}
	}
	if clip != "copied on the owner" {
		t.Errorf("clipboard = %q", clip)
	}
	if cursor.Agent != "bot" || cursor.Kind != "move" || cursor.From == nil || cursor.From.X != 0.25 || cursor.Seq != 0 {
		t.Errorf("cursor = %+v", cursor)
	}
	// The owner sends the dropped tap first, on the same stream, so the first
	// event to arrive is the swipe only if the tap was dropped.
	if event.Kind != "swipe" || event.Viewer != 0 || event.Seq != 1 || event.Ms != 300 || event.T != 3 {
		t.Errorf("event = %+v", event)
	}
	if got := sess.EventsAfter(0); len(got) != 1 || got[0].Kind != "swipe" {
		t.Errorf("hub ring = %+v", got)
	}
}

func TestRelayKeepsTheNewestClipboardsAndClosesWithTheStream(t *testing.T) {
	goSend := make(chan struct{})
	var msgs [][]byte
	for _, c := range []string{"c1", "c2", "c3", "c4", "c5", "c6"} {
		msgs = append(msgs, append([]byte{session.MsgClipboard}, c...))
	}
	// Read after the clipboards, so its arrival says they were all read.
	msgs = append(msgs, ownerMsg(t, session.MsgEvent, session.Event{Seq: 1, Source: "agent", Kind: "key"}))
	o, h := addFakeOwner(t, fakeOwner(goSend, msgs...))

	st := openPeer(t, h, "mini/rec")
	cw, ok := st.(target.ClipboardWatcher)
	if !ok {
		t.Fatal("the relay is not a ClipboardWatcher")
	}
	cr, ok := st.(target.ControlRelayer)
	if !ok {
		t.Fatal("the relay is not a ControlRelayer")
	}
	close(goSend)
	select {
	case b := <-cr.RelayedControl():
		if b[0] != session.MsgEvent {
			t.Fatalf("relayed % x", b)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no event relayed")
	}
	var got []string
	for len(got) < 4 {
		select {
		case c := <-cw.ClipboardChanges():
			got = append(got, c)
		default:
			t.Fatalf("queued clipboards = %v", got)
		}
	}
	if want := []string{"c3", "c4", "c5", "c6"}; !equalStrings(got, want) {
		t.Fatalf("queued clipboards = %v, want %v", got, want)
	}

	o.dropConns()
	for name, closed := range map[string]func() bool{
		"clipboard": func() bool { _, ok := <-cw.ClipboardChanges(); return !ok },
		"relayed":   func() bool { _, ok := <-cr.RelayedControl(); return !ok },
	} {
		done := make(chan bool, 1)
		go func() { done <- closed() }()
		select {
		case ok := <-done:
			if !ok {
				t.Errorf("%s channel delivered after the queue was drained", name)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s channel still open after the owner went", name)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// hubEvents waits for n events on the hub's session, then long enough for a
// duplicate to arrive, and returns what is there.
func hubEvents(t *testing.T, sess *session.Session, n int) []session.Event {
	t.Helper()
	eventually(t, "hub events", func() bool { return len(sess.EventsAfter(0)) >= n })
	time.Sleep(300 * time.Millisecond)
	return sess.EventsAfter(0)
}

func TestPeerAgentTapIsOneEventOnTheHub(t *testing.T) {
	p := newPair(t, true)
	hsess, ok := p.h.mgr.Session("mini/rec")
	if !ok {
		t.Fatal("no hub session")
	}
	if w := p.hub(http.MethodPost, "/api/input?target=mini/rec", tapBody, HeaderAgent, "claude"); w.Code != http.StatusOK {
		t.Fatalf("tap = %d %s", w.Code, w.Body)
	}
	got := hubEvents(t, hsess, 1)
	if len(got) != 1 {
		t.Fatalf("hub events = %+v", got)
	}
	if e := got[0]; e.Kind != "tap" || e.Source != "agent" || e.Agent != "claude" || e.Viewer != 0 {
		t.Fatalf("hub event = %+v", e)
	}
}

func TestPeerHubViewerTapIsOneEventOnTheHubAsThatViewer(t *testing.T) {
	p := newPair(t, true)
	hsess, ok := p.h.mgr.Session("mini/rec")
	if !ok {
		t.Fatal("no hub session")
	}
	hv, hout := hsess.AddViewer()
	go func() {
		for range hout {
		}
	}()
	if w := p.hub(http.MethodPost, "/api/input?target=mini/rec", tapBody, HeaderViewer, strconv.FormatUint(hv, 10)); w.Code != http.StatusOK {
		t.Fatalf("tap = %d %s", w.Code, w.Body)
	}
	got := hubEvents(t, hsess, 1)
	if len(got) != 1 || got[0].Viewer != hv || got[0].Source != "viewer" {
		t.Fatalf("hub events = %+v", got)
	}
}

func TestOwnerPublishesAPeerAgentsEventUnlessItWasNeverReached(t *testing.T) {
	pt := target.NewPeerTarget("mini", "rec", target.KindDevice, "Rec", nil, nil)
	agent := inputCaller{agent: "claude"}
	for _, c := range []struct {
		name string
		t    target.Target
		c    inputCaller
		err  error
		want bool
	}{
		{"agent on a peer", pt, agent, nil, true},
		{"owner refused or failed", pt, agent, &target.PeerError{Status: http.StatusConflict}, true},
		{"owner never reached", pt, agent, target.ErrPeerDown, false},
		{"owner never reached, wrapped", pt, agent, errors.Join(errors.New("tap"), target.ErrPeerDown), false},
		{"hub viewer on a peer", pt, inputCaller{viewer: 3}, nil, false},
		{"local target", &recTarget{}, agent, nil, false},
	} {
		if got := ownerPublishes(c.t, c.c, c.err); got != c.want {
			t.Errorf("%s: ownerPublishes = %v, want %v", c.name, got, c.want)
		}
	}
}
