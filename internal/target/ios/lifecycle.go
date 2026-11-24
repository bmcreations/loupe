package ios

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

var (
	defaultSimctl = func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "xcrun", append([]string{"simctl"}, args...)...).CombinedOutput()
	}
	simctl = defaultSimctl
)

// Stop shuts the simulator down. One already shut down is done.
func (s *Simulator) Stop(ctx context.Context) error {
	udid := s.UDID()
	if out, err := simctl(ctx, "shutdown", udid); err != nil &&
		!strings.Contains(string(out), "current state: Shutdown") {
		return fmt.Errorf("simctl shutdown: %v: %s", err, strings.TrimSpace(string(out)))
	}
	// Through input(), not s.in: a first /api/input sets s.in inside
	// inputOnce, and reading it bare races that write. An unstarted input
	// process closes as a no-op.
	s.input().close()
	s.setBooted(false)
	info := s.Info()
	info.State = "Shutdown"
	s.Update(info)
	return nil
}

// Delete shuts the simulator down and deletes it.
func (s *Simulator) Delete(ctx context.Context) error {
	if err := s.Stop(ctx); err != nil {
		return err
	}
	if out, err := simctl(ctx, "delete", s.UDID()); err != nil {
		return fmt.Errorf("simctl delete: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
