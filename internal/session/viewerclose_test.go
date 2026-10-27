package session

import (
	"encoding/hex"
	"sync"
	"testing"

	"github.com/bmcreations/loupe/internal/target"
)

// raceRounds is how many times each test races a republish against a viewer
// leaving. The unguarded window is a few instructions wide, so one round
// proves nothing; enough of them make an unfixed build fail reliably.
const raceRounds = 2000

// raceRemoval runs publish concurrently with RemoveViewer for a freshly added
// viewer, once per round. A send that can land after the close panics with
// "send on closed channel", which takes the whole process down with it: every
// other live session is dropped and the supervisor restarts the binary.
func raceRemoval(t *testing.T, s *Session, publish func(round int)) {
	t.Helper()
	for i := 0; i < raceRounds; i++ {
		id, _ := s.AddViewer()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			publish(i)
		}()
		go func() {
			defer wg.Done()
			s.RemoveViewer(id)
		}()
		wg.Wait()
	}
}

// A rotation republishes init to every viewer. A viewer disconnecting at the
// same moment must not be sent to after its channel is closed.
func TestARotationDoesNotSendToAViewerThatJustLeft(t *testing.T) {
	s := &Session{
		ID:      "test",
		key:     "test",
		stream:  &rotStream{},
		viewers: map[uint64]*viewer{},
		done:    make(chan struct{}),
		init:    InitPayload{CodedWidth: 480, CodedHeight: 1080},
	}

	raceRemoval(t, s, func(round int) {
		// Alternate, so every round is a real change of size and reaches the
		// send rather than returning early on an unchanged one.
		if round%2 == 0 {
			s.noteSize(target.Meta{Width: 1080, Height: 480})
		} else {
			s.noteSize(target.Meta{Width: 480, Height: 1080})
		}
	})
}

// The first parameter set rewrites init with the real codec string and sends
// it to every viewer that joined before it was known. Same race, other path.
func TestResolvingTheCodecDoesNotSendToAViewerThatJustLeft(t *testing.T) {
	// emulator-5554's portrait SPS, captured with cmd/rotprobe.
	sps, err := hex.DecodeString("6742c0298d681e0227e59080808083c2211a80")
	if err != nil {
		t.Fatal(err)
	}
	cfg := append([]byte{0, 0, 0, 1}, sps...)

	s := &Session{
		ID:      "test",
		key:     "test",
		stream:  &rotStream{},
		viewers: map[uint64]*viewer{},
		done:    make(chan struct{}),
		init:    InitPayload{Codec: "avc1.42E01E", CodedWidth: 480, CodedHeight: 1080},
	}

	raceRemoval(t, s, func(int) { s.resolveCodec(cfg) })

	if !s.codecResolved {
		t.Fatal("resolveCodec never resolved the codec, so the send path was never reached")
	}
}
