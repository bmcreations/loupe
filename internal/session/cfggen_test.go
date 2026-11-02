package session

import (
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

// A viewer that joins before the stream has sent any config gets none at join;
// the config that follows is stamped with a generation, and the cached copy is
// what a writer re-sends ahead of a keyframe it never had config for.
// When the stream is replaced the old config must not be replayed.
func TestConfigGenerationAndClearOnStreamReplace(t *testing.T) {
	ks1 := &keyStream{frames: make(chan target.Frame, 4)}
	ks2 := &keyStream{frames: make(chan target.Frame, 4)}
	s := &Session{
		ID:      "test",
		key:     "test",
		stream:  ks1,
		viewers: map[uint64]*viewer{},
		done:    make(chan struct{}),
	}
	_, out := s.AddViewer()
	<-out // init
	if _, ok := s.CurrentConfig(); ok {
		t.Fatal("config cached before the stream sent any")
	}
	go s.pump()

	ks1.frames <- target.Frame{Config: true, Data: []byte{0, 0, 0, 1, 0x67}}
	waitFor(t, "config cached", func() bool { _, ok := s.CurrentConfig(); return ok })
	cfg, _ := s.CurrentConfig()
	if cfg.CfgGen == 0 || !cfg.Config {
		t.Fatalf("cached config not stamped: %+v", cfg)
	}
	got := <-out
	for !got.Config {
		got = <-out // control messages queued at join
	}
	if got.CfgGen != cfg.CfgGen {
		t.Fatalf("broadcast gen %d != cached gen %d", got.CfgGen, cfg.CfgGen)
	}

	// Stream reopens: the stale config is dropped before the new one arrives,
	// so a joiner in the gap is not handed SPS/PPS from the previous encoder.
	s.streamMu.Lock()
	s.stream = ks2
	s.streamMu.Unlock()
	close(ks1.frames)
	waitFor(t, "stale config cleared", func() bool { _, ok := s.CurrentConfig(); return !ok })

	ks2.frames <- target.Frame{Config: true, Data: []byte{0, 0, 0, 1, 0x67, 1}}
	waitFor(t, "new config cached", func() bool { _, ok := s.CurrentConfig(); return ok })
	cfg2, _ := s.CurrentConfig()
	if cfg2.CfgGen <= cfg.CfgGen {
		t.Fatalf("generation did not advance: %d then %d", cfg.CfgGen, cfg2.CfgGen)
	}
	s.streamMu.Lock()
	s.closing = true
	s.streamMu.Unlock()
	close(ks2.frames)
}
