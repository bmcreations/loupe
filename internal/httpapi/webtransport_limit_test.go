package httpapi

import (
	"fmt"
	"testing"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
)

// Both stream limits have to read as a frame to abandon, not a dead
// session: the QUIC one is what ended a live session (trap 49).
func TestIsStreamLimitMatchesBothLimits(t *testing.T) {
	for _, err := range []error{
		&webtransport.StreamLimitReachedError{},
		&quic.StreamLimitReachedError{},
		fmt.Errorf("open: %w", &quic.StreamLimitReachedError{}),
	} {
		if !isStreamLimit(err) {
			t.Errorf("isStreamLimit(%T) = false", err)
		}
	}
	if isStreamLimit(fmt.Errorf("too many open streams")) {
		t.Error("matched on the message rather than the type")
	}
}
