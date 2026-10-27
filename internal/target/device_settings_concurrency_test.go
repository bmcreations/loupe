package target

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestApplySettingsSerializesConcurrentCallsOnOneTarget drives two concurrent
// ApplySettings calls against one *ADBTarget, through a real subprocess (a
// fake "adb" script) rather than a shellFunc double, so it exercises
// settingsMu itself rather than assuming it is wired up correctly.
//
// darkMode's read-then-write is two shell invocations. Each invocation here
// sleeps 50ms and appends its own start/end timestamps to a private file
// (one file per invocation, since concurrent appends to one file are not
// portably atomic). If settingsMu genuinely serializes ApplySettings, all
// four invocations across both calls form one strict chain: sorted by start
// time, every invocation starts no earlier than the previous one ended. If
// the mutex is removed, the two calls' reads (both "cmd uimode night", timed
// identically) race and their intervals overlap.
func TestApplySettingsSerializesConcurrentCallsOnOneTarget(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no /bin/sh available")
	}

	logDir := t.TempDir()
	scriptPath := filepath.Join(t.TempDir(), "fake-adb")
	script := fmt.Sprintf(`#!/bin/sh
start=$(date +%%s%%N)
sleep 0.05
end=$(date +%%s%%N)
f=$(mktemp %q/entry.XXXXXX)
echo "$start $end $*" > "$f"
case "$*" in
  *night) echo "Night mode: no" ;;
esac
exit 0
`, logDir)
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake adb script: %v", err)
	}

	tgt := NewADBTarget(ADB{Path: scriptPath}, DeviceInfo{Serial: "fake-serial", State: "device"}, "", "")

	var wg sync.WaitGroup
	results := make([]error, 2)
	values := []bool{true, false}
	for i, v := range values {
		wg.Add(1)
		go func(i int, v bool) {
			defer wg.Done()
			_, _, err := tgt.ApplySettings(context.Background(), map[string]any{"darkMode": v})
			results[i] = err
		}(i, v)
	}
	wg.Wait()

	for i, err := range results {
		if err != nil {
			t.Fatalf("call %d: ApplySettings: %v", i, err)
		}
	}

	entries, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatalf("read log dir: %v", err)
	}
	type interval struct{ start, end int64 }
	var got []interval
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(logDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		fields := strings.Fields(string(b))
		if len(fields) < 2 {
			t.Fatalf("malformed log entry %q", string(b))
		}
		start, err1 := strconv.ParseInt(fields[0], 10, 64)
		end, err2 := strconv.ParseInt(fields[1], 10, 64)
		if err1 != nil || err2 != nil {
			t.Fatalf("malformed timestamps in %q", string(b))
		}
		got = append(got, interval{start, end})
	}
	if len(got) != 4 {
		t.Fatalf("want 4 shell invocations (2 per call), got %d", len(got))
	}
	sort.Slice(got, func(i, j int) bool { return got[i].start < got[j].start })

	for i := 1; i < len(got); i++ {
		if got[i].start < got[i-1].end {
			t.Fatalf("invocation %d started (%d) before invocation %d ended (%d) -- calls interleaved, settingsMu is not serializing them",
				i, got[i].start, i-1, got[i-1].end)
		}
	}
}
