package httpapi

import (
	"context"
	"time"
	"unicode/utf8"

	"github.com/bmcreations/loupe/internal/clipshare"
	"github.com/bmcreations/loupe/internal/target"
)

// clipShareWriteTimeout bounds one write of the shared clipboard to a
// target. A simulator's goes through CoreDevice, which took up to 49ms.
const clipShareWriteTimeout = 3 * time.Second

func clipOrigin(targetID string) string { return "target:" + targetID }

// SetClipShare joins every local session to the shared clipboard: a device
// copy is published from the session, and each shared change is written to
// every other target with a live session. Call it before any session opens;
// one opened earlier does not feed it.
func (m *Manager) SetClipShare(s *clipshare.Service) {
	m.mu.Lock()
	m.clipShare = s
	m.mu.Unlock()
	s.Subscribe(func(v clipshare.Value) { m.fanOutClipboard(s, v) })
}

// fanOutClipboard writes v to each target with a live session except its
// origin. A target with no session is not written: Android reaches its
// clipboard through the scrcpy session and a simulator through its input
// process. Peer targets are skipped, because their owner writes them from
// its own shared clipboard. The targets' own echo suppression keeps each
// write from coming back as a device copy.
func (m *Manager) fanOutClipboard(share *clipshare.Service, v clipshare.Value) {
	if !share.Enabled() {
		return
	}
	type dest struct {
		id string
		cb target.Clipboarder
	}
	var dests []dest
	m.mu.Lock()
	for id, s := range m.sessions {
		select {
		case <-s.Done():
			continue
		default:
		}
		if clipOrigin(id) == v.Origin {
			continue
		}
		t := m.targets[id]
		if _, peer := t.(*target.PeerTarget); peer {
			continue
		}
		if cb, ok := t.(target.Clipboarder); ok {
			dests = append(dests, dest{id, cb})
		}
	}
	m.mu.Unlock()
	for _, d := range dests {
		if m.claimClipSend(d.id, v.Seq) {
			go m.writeSharedClipboard(d.id, d.cb, v.Text)
		}
	}
}

// applySharedClipboard writes the shared value to a target whose session
// just opened, so a target opened after a copy still gets it.
func (m *Manager) applySharedClipboard(targetID string, t target.Target, share *clipshare.Service) {
	cb, ok := t.(target.Clipboarder)
	if !ok {
		return
	}
	v, ok := share.Current()
	if !ok || v.Origin == clipOrigin(targetID) {
		return
	}
	if m.claimClipSend(targetID, v.Seq) {
		go m.writeSharedClipboard(targetID, cb, v.Text)
	}
}

// claimClipSend reports whether value seq still needs writing to targetID,
// and records it as sent if so. A fan-out racing a session's open would
// otherwise write the same value twice.
func (m *Manager) claimClipSend(targetID string, seq uint64) bool {
	m.clipSentMu.Lock()
	defer m.clipSentMu.Unlock()
	if m.clipSent == nil {
		m.clipSent = map[string]uint64{}
	}
	if seq == 0 || seq <= m.clipSent[targetID] {
		return false
	}
	m.clipSent[targetID] = seq
	return true
}

// writeSharedClipboard sets one target's clipboard without pasting. The
// trace gets the text's length, never the text.
func (m *Manager) writeSharedClipboard(targetID string, cb target.Clipboarder, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), clipShareWriteTimeout)
	defer cancel()
	rec := map[string]any{"kind": "clipboard", "target": targetID, "from": "shared"}
	if err := cb.WriteClipboard(ctx, text, false); err != nil {
		rec["error"] = err.Error()
	} else {
		rec["textLen"] = utf8.RuneCountInString(text)
	}
	m.trace.Record(rec)
}

// forgetClipSend starts a new session's record over: its device may have
// changed while no session watched it. Call it under m.mu before the session
// is published, so no fan-out can claim a value for the new session first.
func (m *Manager) forgetClipSend(targetID string) {
	m.clipSentMu.Lock()
	delete(m.clipSent, targetID)
	m.clipSentMu.Unlock()
}
