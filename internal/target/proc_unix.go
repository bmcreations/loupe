//go:build !windows

package target

import "syscall"

// procGroupAttr puts a child in its own process group, so signalGroup can
// reach everything it spawned.
func procGroupAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// sessionAttr starts a child in a new session, detached from loupe's terminal.
func sessionAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// signalPid sends sig to pid. Signal 0 only checks that the pid exists.
func signalPid(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }

// signalGroup sends sig to the process group led by pid.
func signalGroup(pid int, sig syscall.Signal) error { return syscall.Kill(-pid, sig) }
