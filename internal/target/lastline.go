package target

import (
	"strings"
)

// LastLine is the last non-empty line of s, trimmed.
func LastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
