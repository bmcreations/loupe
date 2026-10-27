package target

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Android's crash reports are DropBoxManager entries, the ones the system
// writes for a Java crash, a native crash and an ANR in an app, a system
// app or system_server. They are read with `dumpsys dropbox --print --file
// <tag>`, which works on API 29 and API 37 alike; --proto is missing on 29.
// Each tag is one call, because dumpsys matches every positional argument
// against the same entry, so two tags name nothing. The tags are constants
// and never a caller's input.
var dropboxCrashTags = []string{
	"data_app_crash", "data_app_native_crash", "data_app_anr",
	"system_app_crash", "system_app_native_crash", "system_app_anr",
	"system_server_crash", "system_server_native_crash", "system_server_anr",
}

// dropboxIDRE is an Android crash id: the entry's tag and the epoch millis
// its file is named by, so a fetch knows which tag to read.
var dropboxIDRE = regexp.MustCompile(`^(data_app|system_app|system_server)_(crash|native_crash|anr)-\d{10,13}$`)

// dropboxSeparator opens every entry in dumpsys's output.
const dropboxSeparator = "========================================"

// dropboxPathRE is the line after an entry's header, naming its file:
// /data/system/dropbox/system_app_anr@1792700263858.txt.gz.
var dropboxPathRE = regexp.MustCompile(`^/data/system/dropbox/([a-z_]+)@(\d+)\.`)

// dropboxPackageRE is a Package line, "com.android.settings v29 (10)":
// the package, the versionCode and the versionName.
var dropboxPackageRE = regexp.MustCompile(`^(\S+) v(\d+) \((.*)\)$`)

// dropboxEntry is one entry of a tag's output, header line to the next
// separator.
type dropboxEntry struct {
	tag    string
	millis int64
	text   []byte
}

func (e dropboxEntry) id() string { return e.tag + "-" + strconv.FormatInt(e.millis, 10) }

// parseDropbox splits one tag's dumpsys output into its entries. The
// preamble before the first separator and "(No entries found.)" are not
// entries; neither is a block whose second line names no dropbox file.
func parseDropbox(out []byte, tag string) []dropboxEntry {
	var entries []dropboxEntry
	blocks := bytes.Split(out, []byte(dropboxSeparator+"\n"))
	for _, b := range blocks[1:] {
		header, rest, ok := bytes.Cut(b, []byte("\n"))
		if !ok {
			continue
		}
		path, _, _ := bytes.Cut(rest, []byte("\n"))
		m := dropboxPathRE.FindSubmatch(bytes.TrimSpace(path))
		if m == nil || string(m[1]) != tag || !bytes.Contains(header, []byte(" "+tag+" (")) {
			continue
		}
		millis, err := strconv.ParseInt(string(m[2]), 10, 64)
		if err != nil {
			continue
		}
		entries = append(entries, dropboxEntry{tag: tag, millis: millis, text: bytes.TrimRight(b, "\n")})
	}
	return entries
}

// dropboxReport reads a row out of an entry. The body opens with
// "Key: value" lines up to the first blank line (API 37 adds
// SystemUptimeMs and others ahead of Process), then the crash itself.
func dropboxReport(e dropboxEntry) CrashReport {
	r := CrashReport{ID: e.id(), Time: time.UnixMilli(e.millis).UTC()}
	switch {
	case strings.HasSuffix(e.tag, "_native_crash"):
		r.Kind = "nativeCrash"
	case strings.HasSuffix(e.tag, "_anr"):
		r.Kind = "anr"
	default:
		r.Kind = "crash"
	}
	lines := strings.Split(string(e.text), "\n")
	body := len(lines)
	for i := 2; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			body = i + 1
			break
		}
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "Process":
			if r.Process == "" {
				r.Process = v
			}
		case "PID":
			if r.PID == 0 {
				r.PID, _ = strconv.Atoi(v)
			}
		case "Package":
			// A shared uid lists every package in it; the first is the
			// one the process was started for.
			if m := dropboxPackageRE.FindStringSubmatch(v); m != nil && r.BundleID == "" {
				r.BundleID, r.Build, r.Version = m[1], m[2], m[3]
			}
		}
	}
	r.Summary = dropboxSummary(r.Kind, lines[min(body, len(lines)):])
	return r
}

// dropboxSummary is a crash's one line: the exception for a Java crash,
// the signal and any abort message for a native one, and the Subject an
// ANR gives after its CPU usage, "executing service ..., waited 20367ms".
func dropboxSummary(kind string, body []string) string {
	switch kind {
	case "anr":
		for _, l := range body {
			if v, ok := strings.CutPrefix(l, "Subject: "); ok {
				return clip(strings.TrimSpace(v), 300)
			}
		}
	case "crash":
		for _, l := range body {
			if l = strings.TrimSpace(l); l != "" {
				return clip(l, 300)
			}
		}
	case "nativeCrash":
		var signal, abort string
		for _, l := range body {
			l = strings.TrimSpace(l)
			if signal == "" && strings.HasPrefix(l, "signal ") {
				signal = l
			}
			if abort == "" && strings.HasPrefix(l, "Abort message: ") {
				abort = strings.TrimPrefix(l, "Abort message: ")
			}
			if signal != "" && abort != "" {
				break
			}
		}
		if abort != "" {
			return clip(signal+", "+abort, 300)
		}
		return signal
	}
	return ""
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// dropboxEntries reads every crash tag, one dumpsys each and side by side.
// An ANR prints its whole body, about 200KB for one in systemui, so this
// is the bodies of every report the dropbox still holds, not a list of
// files. A tag that fails fails the listing, since a partial list would
// read as fewer crashes.
func (t *ADBTarget) dropboxEntries(ctx context.Context, tags []string) ([]dropboxEntry, error) {
	if !t.info.Ready() {
		return nil, fmt.Errorf("device %s is in state %q", t.info.Serial, t.info.State)
	}
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		entries []dropboxEntry
		first   error
	)
	for _, tag := range tags {
		wg.Add(1)
		go func(tag string) {
			defer wg.Done()
			out, err := t.adb.ExecOut(ctx, t.info.Serial, "dumpsys", "dropbox", "--print", "--file", tag)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if first == nil {
					first = fmt.Errorf("dumpsys dropbox: %w", err)
				}
				return
			}
			entries = append(entries, parseDropbox(out, tag)...)
		}(tag)
	}
	wg.Wait()
	if first != nil {
		return nil, first
	}
	return entries, nil
}

// Crashes lists the device's crash reports from the dropbox, newest first.
// The dropbox lives on /data, so an emulator that boots from loupe-warm
// without emulatorKeepState starts each time with the reports it had when
// the snapshot was saved.
func (t *ADBTarget) Crashes(ctx context.Context, q CrashQuery) ([]CrashReport, error) {
	entries, err := t.dropboxEntries(ctx, dropboxCrashTags)
	if err != nil {
		return nil, err
	}
	return dropboxReports(entries, q), nil
}

func dropboxReports(entries []dropboxEntry, q CrashQuery) []CrashReport {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].millis != entries[j].millis {
			return entries[i].millis > entries[j].millis
		}
		return entries[i].tag < entries[j].tag
	})
	var list []CrashReport
	for _, e := range entries {
		if !q.Since.IsZero() && time.UnixMilli(e.millis).Before(q.Since) {
			break
		}
		if q.Limit > 0 && len(list) == q.Limit {
			break
		}
		list = append(list, dropboxReport(e))
	}
	return list
}

// Crash returns one dropbox entry as dumpsys prints it: the header, the
// file's path and the body, decompressed.
func (t *ADBTarget) Crash(ctx context.Context, id string) ([]byte, error) {
	if !dropboxIDRE.MatchString(id) {
		return nil, ErrCrashNotFound
	}
	tag := id[:strings.LastIndexByte(id, '-')]
	entries, err := t.dropboxEntries(ctx, []string{tag})
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.id() == id {
			return append(e.text, '\n'), nil
		}
	}
	return nil, ErrCrashNotFound
}
