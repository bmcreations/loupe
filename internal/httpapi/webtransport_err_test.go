package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/quic-go/webtransport-go"
)

// A closed session's error has an empty message, which left ten viewer-stop
// records in the 28 September 07:05 trace reading `open frame stream: `.
// wtConn.Close is CloseWithError(0, ""), and this is the SessionError it
// leaves behind for every later open, write and read.
func TestWtErrNamesASessionCloseWithNoMessage(t *testing.T) {
	local := &webtransport.SessionError{ErrorCode: 0}
	remote := &webtransport.SessionError{ErrorCode: 7, Remote: true}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"local close", local, "session closed locally, code 0"},
		{"peer close", remote, "session closed by the peer, code 7"},
		{"wrapped", fmt.Errorf("open frame stream: %w", wtErr(local)),
			"open frame stream: session closed locally, code 0"},
		{"wrapped before wtErr", fmt.Errorf("stream: %w", remote),
			"stream:  (session closed by the peer, code 7)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := wtErr(c.err)
			if got := err.Error(); got != c.want {
				t.Errorf("Error() = %q, want %q", got, c.want)
			}
			var se *webtransport.SessionError
			if !errors.As(err, &se) {
				t.Error("the SessionError is no longer in the chain")
			}
			if fmt.Sprintf("%T", innermost(err)) != "*webtransport.SessionError" {
				t.Errorf("innermost = %T", innermost(err))
			}
		})
	}
}

// Anything with text of its own passes through untouched, so errors.Is and
// the viewer's io.EOF check still see what they saw before.
func TestWtErrLeavesOtherErrorsAlone(t *testing.T) {
	withMsg := &webtransport.SessionError{ErrorCode: 1, Message: "bye"}
	for _, err := range []error{nil, io.EOF, context.DeadlineExceeded, withMsg} {
		if got := wtErr(err); got != err {
			t.Errorf("wtErr(%v) = %v, want it unchanged", err, got)
		}
	}
}
