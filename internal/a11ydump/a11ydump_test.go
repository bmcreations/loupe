package a11ydump

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// TestDexIsPinned keeps the committed build honest: the dex is build output
// checked in so `make build` needs no Android SDK, and a rebuild that changes
// it has to repin on purpose.
func TestDexIsPinned(t *testing.T) {
	if len(Dex()) == 0 {
		t.Fatal("dex is empty")
	}
	h := sha256.Sum256(Dex())
	if got := hex.EncodeToString(h[:]); got != SHA256 {
		t.Fatalf("loupe-a11y.dex changed\n got %s\nwant %s", got, SHA256)
	}
}
