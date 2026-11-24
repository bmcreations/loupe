package ios

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/bmcreations/loupe/internal/target"
)

// AddMedia goes through `simctl addmedia`, which puts photos and videos in
// the simulator's Photos library. It needs the simulator booted.
func (s *Simulator) AddMedia(ctx context.Context, path, name string, kind target.MediaKind) error {
	out, err := exec.CommandContext(ctx, "xcrun", "simctl", "addmedia", s.UDID(), path).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return fmt.Errorf("simctl addmedia: %w", err)
		}
		return fmt.Errorf("simctl addmedia: %w: %s", err, addMediaReason(msg))
	}
	return nil
}

var (
	_ target.MediaAdder = (*Simulator)(nil)
)

// addMediaReason picks simctl addmedia's own failure line. Its last line is
// CoreSimulator's "Multiple errors were returned; see stderr", which says
// nothing; the "Failed to import" line above it carries the Photos error.
func addMediaReason(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "Failed to import") {
			if i := strings.Index(l, "error "); i >= 0 {
				return l[i+len("error "):]
			}
			return l
		}
	}
	return target.LastLine(out)
}
