package target

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var _ Crasher = (*IOSDevice)(nil)

// phoneCrashMaxBytes skips a file too large to be a crash report before it
// is copied. The phone's crash directory also holds spindumps of 2-12MB;
// an app's crash report on Brandon's iPhone was 19KB.
const phoneCrashMaxBytes = 4 << 20

// phoneCrashCopies is how many files are copied off the phone at once.
// Each copy is its own devicectl run, about 1.7s whatever the size.
const phoneCrashCopies = 4

// phoneReportNameRE is a report's file name, the process and the time it
// was written: "LoupeRunner-Runner-2026-10-22-230313.ips".
var phoneReportNameRE = regexp.MustCompile(`^(.+)-\d{4}-\d\d-\d\d-\d{6}(\.\d+)?\.ips$`)

// phoneNoiseRE matches the reports in the phone's crash directory that are
// not crashes, by the process part of the name. On Brandon's iPhone on 22
// October that was 100 of the 101 .ips files: analytics, Siri feedback,
// Jetsam events and resource reports (`.cpu_resource`). A name that slips
// through costs one copy, which is cached, and parseIPS still drops it by
// bug_type; a crash wrongly matched here would be lost, so the list holds
// only names seen there.
var phoneNoiseRE = regexp.MustCompile(`^(Analytics|SFA-|JetsamEvent$|SiriSearchFeedback$|WiFiLQMMetrics$|ecosystemanalyticsd$|proactive_event_tracker|xp_amp_)|_resource$|\.json$`)

// phoneFile is one entry of `devicectl device info files`.
type phoneFile struct {
	RelativePath string `json:"relativePath"`
	Metadata     struct {
		Size        int64     `json:"size"`
		LastModDate time.Time `json:"lastModDate"`
	} `json:"metadata"`
	Resources struct {
		IsDirectory bool `json:"isDirectory"`
	} `json:"resources"`
}

// parsePhoneFiles reads the json-output of `devicectl device info files`.
func parsePhoneFiles(raw []byte) ([]phoneFile, error) {
	var doc struct {
		Result struct {
			Files []phoneFile `json:"files"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("devicectl info files: %w", err)
	}
	return doc.Result.Files, nil
}

// phoneCrashCandidates keeps the files that may be crash reports: .ips
// files in the directory or its Retired one, small enough, not named as
// one of the reports that are never crashes, and not written before since.
// A report is written after the crash, so a file older than since cannot
// hold a crash after it.
func phoneCrashCandidates(files []phoneFile, since time.Time) []phoneFile {
	var out []phoneFile
	for _, f := range files {
		dir, name := path.Split(f.RelativePath)
		if f.Resources.IsDirectory || (dir != "" && dir != "Retired/") {
			continue
		}
		m := phoneReportNameRE.FindStringSubmatch(name)
		if m == nil || phoneNoiseRE.MatchString(m[1]) || f.Metadata.Size > phoneCrashMaxBytes {
			continue
		}
		if !since.IsZero() && f.Metadata.LastModDate.Before(since) {
			continue
		}
		out = append(out, f)
	}
	return out
}

// phoneCrashCache is where one phone's reports are kept once copied, under
// their own names. macOS moves a report into Retired keeping its name, so
// a retired report is not copied again.
func phoneCrashCache(udid string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "loupe", "ios-crashes", udid), nil
}

// crashMu serialises refreshes of one phone's cache, so two listings do
// not copy the same files side by side.
var crashMu sync.Map // cache dir -> *sync.Mutex

// refreshCrashes lists the phone's crash directory, copies each candidate
// not already cached at the same size, and removes cached files the phone
// no longer has. It returns the cache directory, which then holds exactly
// the candidates.
func (d *IOSDevice) refreshCrashes(ctx context.Context, since time.Time) (string, error) {
	info := d.Info()
	dir, err := phoneCrashCache(cmp.Or(info.UDID, info.Identifier))
	if err != nil {
		return "", err
	}
	mu, _ := crashMu.LoadOrStore(dir, new(sync.Mutex))
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	files, err := d.crashFiles(ctx)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	all := phoneCrashCandidates(files, time.Time{})
	keep := make(map[string]bool, len(all))
	var want []phoneFile
	for _, f := range all {
		name := path.Base(f.RelativePath)
		keep[name] = true
		if !since.IsZero() && f.Metadata.LastModDate.Before(since) {
			continue
		}
		if fi, err := os.Stat(filepath.Join(dir, name)); err == nil && fi.Size() == f.Metadata.Size {
			continue
		}
		want = append(want, f)
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if !keep[e.Name()] {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}

	errs := make(chan error, len(want))
	sem := make(chan struct{}, phoneCrashCopies)
	var wg sync.WaitGroup
	for _, f := range want {
		wg.Add(1)
		go func(f phoneFile) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			errs <- d.copyCrash(ctx, f.RelativePath, filepath.Join(dir, path.Base(f.RelativePath)))
		}(f)
	}
	wg.Wait()
	close(errs)
	// One file that will not copy should not hide the others; a listing
	// that copied nothing it needed says why.
	var first error
	copied := 0
	for err := range errs {
		if err == nil {
			copied++
		} else if first == nil {
			first = err
		}
	}
	if first != nil && copied == 0 {
		return "", first
	}
	return dir, nil
}

// crashFiles lists the phone's crash directory, Retired included.
func (d *IOSDevice) crashFiles(ctx context.Context) ([]phoneFile, error) {
	tmp, err := os.MkdirTemp("", "loupe-ioscrashes")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	out := filepath.Join(tmp, "files.json")
	b, err := exec.CommandContext(ctx, "xcrun", "devicectl", "device", "info", "files",
		"--device", d.device(), "--domain-type", "systemCrashLogs", "-j", out).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("devicectl info files: %v: %s", err, strings.TrimSpace(string(b)))
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		return nil, err
	}
	return parsePhoneFiles(raw)
}

// copyCrash copies one file off the phone to dst, through a temporary name
// so a failed copy never leaves a partial report in the cache.
func (d *IOSDevice) copyCrash(ctx context.Context, rel, dst string) error {
	tmp := dst + ".part"
	defer os.Remove(tmp)
	b, err := exec.CommandContext(ctx, "xcrun", "devicectl", "device", "copy", "from",
		"--device", d.device(), "--domain-type", "systemCrashLogs",
		"--source", rel, "--destination", tmp).CombinedOutput()
	if err != nil {
		return fmt.Errorf("devicectl copy %s: %v: %s", rel, err, strings.TrimSpace(string(b)))
	}
	return os.Rename(tmp, dst)
}

// Crashes lists the phone's crash reports. The phone holds only its own,
// so no coalition is matched. Each report is copied off once and cached;
// a listing still costs one devicectl run, 0.8-1.9s.
func (d *IOSDevice) Crashes(ctx context.Context, q CrashQuery) ([]CrashReport, error) {
	dir, err := d.refreshCrashes(ctx, q.Since)
	if err != nil {
		return nil, err
	}
	return listCrashes(ctx, dir, "", q)
}

// Crash returns one of the phone's crash reports as iOS wrote it.
func (d *IOSDevice) Crash(ctx context.Context, id string) ([]byte, error) {
	if !ValidCrashID(id) {
		return nil, fmt.Errorf("%w: %q is not a crash id", ErrCrashNotFound, id)
	}
	dir, err := d.refreshCrashes(ctx, time.Time{})
	if err != nil {
		return nil, err
	}
	return readCrash(dir, "", id)
}
