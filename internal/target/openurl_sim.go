package target

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// simopenSource is the helper that opens a universal link in a named app
// on a simulator. See its header for why simctl openurl will not do.
//
//go:embed simopen/simopen.m
var simopenSource []byte

// simopenEntitlements are the simulated entitlements FrontBoard wants of
// a process that opens an app.
//
//go:embed simopen/entitlements.plist
var simopenEntitlements []byte

var (
	simopenMu   sync.Mutex
	simopenPath string
)

// simopenBuild builds the helper once per source, entitlements and SDK,
// into the cache dir the camera shim uses, and returns its path. The
// entitlements go in __TEXT,__entitlements, where the simulator reads
// them, and the binary is then signed ad hoc.
func simopenBuild(ctx context.Context) (string, error) {
	simopenMu.Lock()
	defer simopenMu.Unlock()
	if simopenPath != "" {
		if _, err := os.Stat(simopenPath); err == nil {
			return simopenPath, nil
		}
	}
	ver, err := exec.CommandContext(ctx, "xcrun", "--sdk", "iphonesimulator", "--show-sdk-version").Output()
	if err != nil {
		return "", fmt.Errorf("simopen: no iphonesimulator SDK: %v", err)
	}
	h := sha256.New()
	for _, b := range [][]byte{simopenSource, simopenEntitlements, ver} {
		h.Write(b)
	}
	dir, err := cameraDir()
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, "simopen-"+hex.EncodeToString(h.Sum(nil)[:6]))
	if _, err := os.Stat(out); err == nil {
		simopenPath = out
		return out, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	src := filepath.Join(dir, "simopen.m")
	ents := filepath.Join(dir, "simopen-entitlements.plist")
	if err := os.WriteFile(src, simopenSource, 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(ents, simopenEntitlements, 0o644); err != nil {
		return "", err
	}
	tmp := out + ".tmp"
	args := []string{"--sdk", "iphonesimulator", "clang",
		"-arch", "arm64", "-arch", "x86_64", "-mios-simulator-version-min=15.0",
		"-fobjc-arc", "-O2", "-framework", "Foundation", "-framework", "MobileCoreServices",
		"-Wl,-sectcreate,__TEXT,__entitlements," + ents,
		"-o", tmp, src}
	if b, err := exec.CommandContext(ctx, "xcrun", args...).CombinedOutput(); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("simopen: build: %v: %s", err, strings.TrimSpace(string(b)))
	}
	if b, err := exec.CommandContext(ctx, "codesign", "-f", "-s", "-", tmp).CombinedOutput(); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("simopen: codesign: %v: %s", err, strings.TrimSpace(string(b)))
	}
	if err := os.Rename(tmp, out); err != nil {
		return "", err
	}
	simopenPath = out
	return out, nil
}

// simApp is an installed app as `simctl listapps` reports it.
type simApp struct {
	BundleID   string
	Executable string // the main executable's path on the Mac
}

// parseSimListApps reads `simctl listapps` once plutil has made it JSON,
// keeping only apps the user installed: a system app claims no domain a
// link would go to, and reading every one's entitlements costs time.
func parseSimListApps(b []byte) ([]simApp, error) {
	var m map[string]struct {
		ApplicationType    string
		Bundle             string
		CFBundleExecutable string
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("simctl listapps: %w", err)
	}
	var apps []simApp
	for id, a := range m {
		if a.ApplicationType != "User" || a.Bundle == "" || a.CFBundleExecutable == "" {
			continue
		}
		bu, err := url.Parse(a.Bundle)
		if err != nil || bu.Scheme != "file" {
			continue
		}
		apps = append(apps, simApp{BundleID: id, Executable: filepath.Join(bu.Path, a.CFBundleExecutable)})
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].BundleID < apps[j].BundleID })
	return apps, nil
}

// simListApps is the simulator's user-installed apps. A var so a test can
// stand in for simctl.
var simListApps = func(ctx context.Context, udid string) ([]simApp, error) {
	list := exec.CommandContext(ctx, "xcrun", "simctl", "listapps", udid)
	raw, err := list.Output()
	if err != nil {
		return nil, fmt.Errorf("simctl listapps: %w", err)
	}
	conv := exec.CommandContext(ctx, "plutil", "-convert", "json", "-o", "-", "-")
	conv.Stdin = strings.NewReader(string(raw))
	b, err := conv.Output()
	if err != nil {
		return nil, fmt.Errorf("simctl listapps: plutil: %w", err)
	}
	return parseSimListApps(b)
}

// simAppEntitlements reads an installed app's entitlements. A var so a
// test can stand in for the executable.
var simAppEntitlements = func(exe string) (appLinkEntitlements, error) {
	b, err := machoEntitlements(exe)
	if err != nil || len(b) == 0 {
		return appLinkEntitlements{}, err
	}
	return parseAppLinkEntitlements(b)
}

// universalLinkApp is the installed app that takes u as a universal link,
// or "" when none does: no app names u's host in its associated domains,
// or the host's apple-app-site-association gives u's path to none of
// them. Apps are tried in bundle ID order, the first match winning. Any
// error is one that kept it from knowing, so the caller opens u as
// before.
func universalLinkApp(ctx context.Context, udid string, u *url.URL) (string, error) {
	host := u.Hostname()
	apps, err := simListApps(ctx, udid)
	if err != nil {
		return "", err
	}
	var errs []error
	for _, a := range apps {
		ents, err := simAppEntitlements(a.Executable)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.BundleID, err))
			continue
		}
		if ents.AppID == "" || !claimsAny(ents.Domains, host) {
			continue
		}
		file, err := aasaFor(ctx, host)
		if err != nil {
			return "", err
		}
		ok, err := aasaClaims(file, ents.AppID, u)
		if err != nil {
			return "", err
		}
		if ok {
			return a.BundleID, nil
		}
	}
	return "", errors.Join(errs...)
}

func claimsAny(domains []string, host string) bool {
	for _, d := range domains {
		if claimsHost(d, host) {
			return true
		}
	}
	return false
}

// openUniversalLink opens u in the app that claims it, as iOS would for a
// link tapped in Messages, and returns that app's bundle ID. ok is false
// when u is not a universal link any installed app takes, or when it
// could not be opened that way; the caller then opens u with simctl.
func (s *Simulator) openUniversalLink(ctx context.Context, u string) (bundle string, ok bool) {
	pu, err := url.Parse(u)
	if err != nil || (pu.Scheme != "https" && pu.Scheme != "http") || pu.Hostname() == "" {
		return "", false
	}
	bundle, err = universalLinkApp(ctx, s.UDID(), pu)
	if err != nil {
		log.Printf("openURL %s on %s: universal link lookup: %v", RedactURL(u), s.UDID(), redactErr(err, u))
	}
	if bundle == "" {
		return "", false
	}
	if err := simopenRun(ctx, s.UDID(), u, bundle); err != nil {
		log.Printf("openURL %s on %s: open in %s: %v; falling back to simctl openurl", RedactURL(u), s.UDID(), bundle, redactErr(err, u))
		return "", false
	}
	return bundle, true
}

// simopenRun runs the helper on the simulator. The helper prints nothing
// of the URL, and its output still goes through redactErr.
var simopenRun = func(ctx context.Context, udid, u, bundle string) error {
	helper, err := simopenBuild(ctx)
	if err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, "xcrun", "simctl", "spawn", udid, helper, u, bundle).CombinedOutput()
	if err != nil {
		return redactErr(fmt.Errorf("simopen: %w: %s", err, strings.TrimSpace(string(out))), u)
	}
	return nil
}
