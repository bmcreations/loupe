package emugrpc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// deadPID is far above macOS's pid limit, so no process has it.
const deadPID = 99999998

func writeRunning(t *testing.T, dir string, pid int, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("pid_%d.ini", pid)), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// running is the shape emulator 37.3.6 writes, cmdline included: it holds
// "-grpc" and '=' of its own.
func running(serial, grpcPort string) string {
	s := "emulator.build=16696583\navd.id=pixel_fold_hinge\nport.serial=" + serial + "\nport.adb=5559\n" +
		"cmdline=\"qemu-system-aarch64\" \"-avd\" \"pixel_fold_hinge\" \"-grpc\" \"8558\" \"-prop\" \"a=b\"\n"
	if grpcPort != "" {
		s += "grpc.token=tok/with+chars==\ngrpc.port=" + grpcPort + "\n"
	}
	return s
}

func TestFindEndpoint(t *testing.T) {
	dir := t.TempDir()
	live := os.Getpid()
	// A crashed emulator left a file for the same console port.
	writeRunning(t, dir, deadPID, running("5558", "9999"))
	writeRunning(t, dir, live, running("5558", "8558"))
	writeRunning(t, dir, os.Getppid(), running("5554", "8554"))

	ep, err := FindEndpoint(dir, 5558)
	if err != nil {
		t.Fatal(err)
	}
	if ep != (Endpoint{Port: 8558, Token: "tok/with+chars==", PID: live}) {
		t.Fatalf("got %+v", ep)
	}
	if ep, err := FindEndpoint(dir, 5554); err != nil || ep.Port != 8554 {
		t.Fatalf("5554: %+v, %v", ep, err)
	}
}

func TestFindEndpointNoGRPC(t *testing.T) {
	dir := t.TempDir()
	writeRunning(t, dir, os.Getpid(), running("5558", ""))
	writeRunning(t, dir, deadPID, running("5556", "8556"))
	writeRunning(t, dir, os.Getppid(), running("5560", "notaport"))
	for _, port := range []int{
		5558, // booted without -grpc
		5556, // only a dead emulator's file
		5560, // unreadable port
		5562, // no file at all
	} {
		if _, err := FindEndpoint(dir, port); !errors.Is(err, ErrNoGRPC) {
			t.Errorf("%d: %v, want ErrNoGRPC", port, err)
		}
	}
	if _, err := FindEndpoint(filepath.Join(dir, "missing"), 5558); !errors.Is(err, ErrNoGRPC) {
		t.Errorf("missing dir: %v, want ErrNoGRPC", err)
	}
}
