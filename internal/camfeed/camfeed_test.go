package camfeed

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeHelper points helperPath at a script that prints answer and copies
// its stdin to the returned file.
func fakeHelper(t *testing.T, answer string) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "stdin")
	script := filepath.Join(dir, "loupe-camfeed")
	body := "#!/bin/sh\necho " + answer + "\nexec cat > " + out + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	old := helperPath
	helperPath = func() (string, error) { return script, nil }
	t.Cleanup(func() { helperPath = old })
	return out
}

// fakeHelperExit points helperPath at a script that prints answer, then exits
// with code, as loupe-camfeed does when another process holds the camera.
func fakeHelperExit(t *testing.T, answer string, code int) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "loupe-camfeed")
	body := fmt.Sprintf("#!/bin/sh\necho %s\nexit %d\n", answer, code)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	old := helperPath
	helperPath = func() (string, error) { return script, nil }
	t.Cleanup(func() { helperPath = old })
}

func framed(frames ...[]byte) []byte {
	var b bytes.Buffer
	for _, f := range frames {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(f)))
		b.Write(n[:])
		b.Write(f)
	}
	return b.Bytes()
}

// waitFor polls path until it holds want.
func waitFor(t *testing.T, path string, want []byte) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got []byte
	for time.Now().Before(deadline) {
		got, _ = os.ReadFile(path)
		if bytes.Equal(got, want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("helper got %q; want %q", got, want)
}

func TestFeedWritesFramesThenStop(t *testing.T) {
	out := fakeHelper(t, "ready")
	f := &Feed{}
	if err := f.Send([]byte("one")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, out, framed([]byte("one")))
	f.Stop()
	// A stop is an empty frame.
	waitFor(t, out, framed([]byte("one"), nil))
}

func TestFeedMissingCameraBacksOff(t *testing.T) {
	fakeHelper(t, "missing")
	f := &Feed{}
	if err := f.Send([]byte("one")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := f.Send([]byte("two"))
		if errors.Is(err, ErrNoCamera) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Send = %v; want ErrNoCamera after a missing answer", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestFeedBusyBacksOff(t *testing.T) {
	fakeHelperExit(t, "ready", exitBusy)
	f := &Feed{}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := f.Send([]byte("one"))
		if errors.Is(err, ErrBusy) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Send = %v; want ErrBusy after a busy exit", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestStopWithNothingRunningStartsNoHelper(t *testing.T) {
	out := fakeHelper(t, "ready")
	f := &Feed{}
	f.Stop()
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(out); err == nil {
		t.Error("Stop started a helper")
	}
}

func TestNoHelper(t *testing.T) {
	old := helperPath
	helperPath = func() (string, error) { return "", ErrNoHelper }
	t.Cleanup(func() { helperPath = old })
	if err := (&Feed{}).Send([]byte("x")); !errors.Is(err, ErrNoHelper) {
		t.Errorf("Send = %v; want ErrNoHelper", err)
	}
}
