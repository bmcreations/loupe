package ios

import (
	"testing"
)

func TestAddMediaReasonSkipsSeeStderr(t *testing.T) {
	out := "Failed to import '/tmp/x.png', error [PHPhotosErrorDomain] 3301: The operation couldn’t be completed.\n\n" +
		"An error was encountered processing the command (domain=com.apple.CoreSimulator.LaunchdSimError, code=133):\n" +
		"Multiple errors were returned; see stderr"
	if got, want := addMediaReason(out), "[PHPhotosErrorDomain] 3301: The operation couldn’t be completed."; got != want {
		t.Fatalf("addMediaReason = %q, want %q", got, want)
	}
	if got := addMediaReason("one\ntwo"); got != "two" {
		t.Fatalf("addMediaReason fallback = %q", got)
	}
}
