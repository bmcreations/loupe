package target

import (
	"strings"
	"testing"
)

// A -phone waiting on the camera prompt said so only into the tail, which
// nothing read until the open failed 8s later.
func TestSimcapStderrEchoesWholeLinesUnderTheDeviceID(t *testing.T) {
	var log strings.Builder
	b := &tailBuffer{max: 4096, echo: &log, id: "00008140-001A"}
	b.Write([]byte("simcap: phone: waiting for camera "))
	if log.Len() != 0 {
		t.Fatalf("echoed a partial line: %q", log.String())
	}
	b.Write([]byte("access: allow it\nsimcap: phone: capturing iPhone\nother\n"))
	want := "simcap 00008140-001A: phone: waiting for camera access: allow it\n" +
		"simcap 00008140-001A: phone: capturing iPhone\n" +
		"simcap 00008140-001A: other\n"
	if log.String() != want {
		t.Fatalf("echo = %q, want %q", log.String(), want)
	}
	if got := b.suffix(); got != ": other" {
		t.Fatalf("suffix = %q", got)
	}
}
