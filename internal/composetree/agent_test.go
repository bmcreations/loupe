package composetree

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// TestAgentFilesArePinned keeps the committed agent build honest: the .so
// and dex are build output checked in so `make build` needs no NDK, and a
// rebuild that changes them has to repin here on purpose.
func TestAgentFilesArePinned(t *testing.T) {
	for _, abi := range []string{"arm64-v8a", "x86_64"} {
		so, dex, err := AgentFiles(abi)
		if err != nil {
			t.Fatalf("AgentFiles(%q): %v", abi, err)
		}
		check := func(name string, b []byte) {
			if len(b) == 0 {
				t.Fatalf("%s is empty", name)
			}
			h := sha256.Sum256(b)
			if got := hex.EncodeToString(h[:]); got != SHA256[name] {
				t.Fatalf("%s changed\n got %s\nwant %s", name, got, SHA256[name])
			}
		}
		check(SOName(abi), so)
		check(DexName, dex)
	}
	if _, _, err := AgentFiles("armeabi-v7a"); err == nil || !strings.Contains(err.Error(), "unsupported ABI") {
		t.Fatalf("armeabi-v7a: err = %v, want unsupported ABI", err)
	}
}
