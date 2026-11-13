package emugrpc

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Endpoint is where one running emulator's gRPC service listens. With
// -grpc-use-token that is 127.0.0.1 only.
type Endpoint struct {
	Port  int
	Token string
	// PID is the emulator process the endpoint was read for.
	PID int
}

// ErrNoGRPC is an emulator that has no gRPC service loupe can reach: it was
// started without -grpc (before loupe passed it, or by something other than
// loupe), or it left no running file to read the port from. Restarting it
// from loupe fixes both.
var ErrNoGRPC = errors.New("emulator has no gRPC service")

// RunningDir is where the emulator writes pid_<pid>.ini for each running
// instance: its console port, adb port, AVD name and, when it serves gRPC,
// grpc.port and grpc.token. This is the macOS location; emulator 37.3.6
// wrote it there.
func RunningDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Caches", "TemporaryItems", "avd", "running"), nil
}

// FindEndpoint reads the gRPC port and token of the emulator whose console
// port is consolePort from dir's running files. The port is read, never
// derived from the console port, so an emulator started with another -grpc
// is still found.
//
// A running file outlives an emulator that crashed, and a later emulator can
// take the same console port, so a file whose process is gone is skipped.
func FindEndpoint(dir string, consolePort int) (Endpoint, error) {
	files, err := filepath.Glob(filepath.Join(dir, "pid_*.ini"))
	if err != nil {
		return Endpoint{}, err
	}
	want := strconv.Itoa(consolePort)
	for _, f := range files {
		pid, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "pid_"), ".ini"))
		if err != nil || !processAlive(pid) {
			continue
		}
		kv, err := readINI(f)
		if err != nil || kv["port.serial"] != want {
			continue
		}
		portStr, ok := kv["grpc.port"]
		if !ok {
			return Endpoint{}, fmt.Errorf("%w: emulator-%d (pid %d) was started without -grpc; restart it from loupe", ErrNoGRPC, consolePort, pid)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil || port <= 0 || port > 65535 {
			return Endpoint{}, fmt.Errorf("%w: %s has grpc.port=%q", ErrNoGRPC, f, portStr)
		}
		return Endpoint{Port: port, Token: kv["grpc.token"], PID: pid}, nil
	}
	return Endpoint{}, fmt.Errorf("%w: no running emulator file for console port %d in %s", ErrNoGRPC, consolePort, dir)
}

// readINI reads key=value lines. Only the first '=' splits, so a value (the
// cmdline line) can hold more.
func readINI(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	kv := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return kv, sc.Err()
}

// processAlive reports whether pid is a running process. EPERM means it
// exists under another user.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
