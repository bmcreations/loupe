package awake

import (
	"os"
	"os/exec"
	"strconv"
)

// System takes the assertion through /usr/bin/caffeinate -i, which holds
// PreventUserIdleSystemSleep for as long as it runs. -w ties it to this
// process, so a crash or a supervisor restart cannot leave the Mac held
// awake by an orphan. A child process rather than IOKit through cgo keeps
// the build cgo-free and cross-compilable; `pmset -g assertions` lists it
// as caffeinate with loupe as its parent.
func System() (release func(), err error) {
	cmd := exec.Command("/usr/bin/caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}, nil
}
