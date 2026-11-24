package revyl

import "strings"

// clip and lastLine copy target's private helpers of the same names, so this
// package does not widen target's exported surface for two one-liners.

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// lastLine is the last non-empty line of s, trimmed.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
