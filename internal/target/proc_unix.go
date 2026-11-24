//go:build !windows

package target

import "syscall"

// ProcGroupAttr puts a child in its own process group, so SignalGroup can
// reach everything it spawned.
func ProcGroupAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// SessionAttr starts a child in a new session, detached from loupe's terminal.
func SessionAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// SignalPID sends sig to pid. Signal 0 only checks that the pid exists.
func SignalPID(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }

// SignalGroup sends sig to the process group led by pid.
func SignalGroup(pid int, sig syscall.Signal) error { return syscall.Kill(-pid, sig) }
