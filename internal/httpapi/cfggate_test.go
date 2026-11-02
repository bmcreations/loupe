package httpapi

import (
	"testing"

	"github.com/bmcreations/loupe/internal/session"
)

func TestCfgGate(t *testing.T) {
	key := session.OutMsg{Key: true}
	cfg1 := session.OutMsg{Config: true, CfgGen: 1}
	cfg2 := session.OutMsg{Config: true, CfgGen: 2}
	var cur session.OutMsg
	have := false
	curFn := func() (session.OutMsg, bool) { return cur, have }

	var g cfgGate
	// Joined before the stream sent any config: nothing to send ahead.
	if _, ok := g.before(key, curFn); ok {
		t.Fatal("resent with no cached config")
	}
	// Config arrives but was dropped from this viewer's queue; the key is next.
	cur, have = cfg1, true
	if c, ok := g.before(key, curFn); !ok || c.CfgGen != 1 {
		t.Fatalf("key without config: got %v %v, want cfg gen 1", c, ok)
	}
	g.noteWritten(cfg1)
	if _, ok := g.before(key, curFn); ok {
		t.Fatal("resent a config the viewer already has")
	}
	// Inter frames and configs never trigger it.
	cur = cfg2
	if _, ok := g.before(session.OutMsg{}, curFn); ok {
		t.Fatal("inter frame triggered a resend")
	}
	if _, ok := g.before(cfg2, curFn); ok {
		t.Fatal("config triggered a resend")
	}
	// Stream reopened with new parameter sets: the key needs gen 2.
	if c, ok := g.before(key, curFn); !ok || c.CfgGen != 2 {
		t.Fatalf("config changed: got %v %v, want gen 2", c, ok)
	}
}
