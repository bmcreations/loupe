package ios

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/bmcreations/loupe/internal/target"
)

// OpenURL opens u with `simctl openurl`. It gets u as its own exec
// argument, with no shell between. simctl gives every https URL to
// Safari, so an http or https URL an installed app claims as a universal
// link goes to that app through the simopen helper first, and the result
// names it. See openUniversalLink.
func (s *Simulator) OpenURL(ctx context.Context, u string) (*target.LaunchResult, error) {
	if err := target.CheckURL(u); err != nil {
		return nil, err
	}
	if bundle, ok := s.openUniversalLink(ctx, u); ok {
		return &target.LaunchResult{Activity: bundle}, nil
	}
	out, err := exec.CommandContext(ctx, "xcrun", "simctl", "openurl", s.UDID(), u).CombinedOutput()
	if err != nil {
		if simOpenURLUnhandled(err) {
			return nil, fmt.Errorf("%w: %s", target.ErrNoURLHandler, target.RedactURL(u))
		}
		return nil, target.RedactErr(fmt.Errorf("simctl openurl: %w: %s", err, strings.TrimSpace(string(out))), u)
	}
	return &target.LaunchResult{}, nil
}

// CheckURL implements URLOpener.
func (s *Simulator) CheckURL(u string) error { return target.CheckURL(u) }

// simOpenURLUnhandled reports whether simctl openurl failed because no app
// claims the URL: it exits 115, LSApplicationWorkspaceErrorDomain's code
// for that, on Xcode 27.
func simOpenURLUnhandled(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 115
}
