package session

import (
	"context"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

// inputStream is a swapStream that reports whether its input opened, the way
// a physical iPhone's stream does.
type inputStream struct {
	*swapStream
	in bool
}

func (s inputStream) AcceptsInput() bool { return s.in }

type inputTarget struct {
	*swapTarget
	stream target.Stream
}

func (t inputTarget) Open(ctx context.Context, opts target.Options) (target.Stream, error) {
	return t.stream, nil
}

// MsgInit's control flag is what a native client gates every click on, so a
// phone whose input opened must say so, and one whose input didn't must not.
func TestInitControlFollowsTheStreamsInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   bool
	}{{"input opened", true}, {"no input", false}} {
		t.Run(tc.name, func(t *testing.T) {
			st := inputStream{swapStream: newSwapStream(), in: tc.in}
			tgt := inputTarget{swapTarget: &swapTarget{kind: target.KindIOSDevice}, stream: st}
			s, err := Open(t.Context(), "phone", tgt, target.Options{})
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer s.Close()
			if s.init.Control != tc.in {
				t.Errorf("init control = %v, want %v", s.init.Control, tc.in)
			}
		})
	}
}
