//go:build windows

package target

import (
	"os"
	"syscall"
)

// loupe does not run on Windows yet; these keep the package compiling.
// There are no process groups or sessions here, so the attrs are nil.
func ProcGroupAttr() *syscall.SysProcAttr { return nil }

func SessionAttr() *syscall.SysProcAttr { return nil }

// SignalPID terminates pid for any real signal and probes for existence on 0.
func SignalPID(pid int, sig syscall.Signal) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return syscall.ESRCH
	}
	defer p.Release()
	if sig == 0 {
		return nil
	}
	return p.Kill()
}

// SignalGroup has no process group to reach on Windows, so it signals pid alone.
func SignalGroup(pid int, sig syscall.Signal) error { return SignalPID(pid, sig) }
