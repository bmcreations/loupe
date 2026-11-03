package target

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A fake simcap that does what the real one does on a phone: the HEVC
// keyframe goes to fd 3 before any H.264 header reaches stdout, and it is
// larger than a pipe's buffer. Open has to drain fd 3 while it waits, or the
// two block each other until the start budget runs out.
const fakeHEVCSimcap = `#!/bin/sh
hdr() { head -c 64 /dev/zero; printf "$1"; printf '\000\000\004\300\000\000\012\140'; }
{ hdr 'h265'; printf '\000\000\000\000\000\000\000\000\000\002\000\000'; head -c 131072 /dev/zero; } >&3
hdr 'h264'
cat >/dev/null
`

func TestOpenDrainsPassthroughBeforeTheH264Header(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "simcap")
	if err := os.WriteFile(bin, []byte(fakeHEVCSimcap), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := openSimcapWith(ctx, bin, "phone", "phone", Options{}, true)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	select {
	case f := <-s.Passthrough():
		if len(f.Data) != 131072 {
			t.Fatalf("passthrough frame of %d bytes", len(f.Data))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no passthrough frame")
	}
}
