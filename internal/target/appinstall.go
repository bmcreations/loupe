package target

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// AppInstaller is implemented by the iOS targets: a physical iPhone, through
// devicectl, and a simulator, through simctl. It is not Installer because an
// iOS app is one bundle, not a base and its splits, and none of adb's flags
// apply.
type AppInstaller interface {
	// InstallApp installs the app in archive, a zip on the host: an .ipa
	// (Payload/Name.app/...) or a zipped Name.app. It replaces any installed
	// app with the same bundle ID. A device that refused the app returns an
	// *InstallError; any other error means it was never asked.
	InstallApp(ctx context.Context, archive string) (*AppInstallResult, error)
}

// AppInstallResult is an install that succeeded.
type AppInstallResult struct {
	// BundleID is CFBundleIdentifier from the app's Info.plist.
	BundleID string
	Output   string
}

// ErrNotIOSApp is an upload with no app bundle in it.
var ErrNotIOSApp = errors.New("not an iOS app")

// maxAppEntries bounds the files in one archive. A large app is a few
// thousand; this only stops a zip bomb of empty entries.
const maxAppEntries = 200_000

// UnpackIOSApp extracts the one .app bundle in archive into dir and returns
// its path. The bundle is either Payload/Name.app, as in an .ipa, or a
// Name.app at the top, as Finder zips one. Entries outside the bundle are
// skipped; an entry that would land outside dir, or a symlink that points
// out of the bundle, fails the whole unpack.
func UnpackIOSApp(archive, dir string) (string, error) {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return "", fmt.Errorf("%w: not a zip", ErrNotIOSApp)
	}
	defer zr.Close()
	if len(zr.File) > maxAppEntries {
		return "", fmt.Errorf("%w: over %d entries", ErrNotIOSApp, maxAppEntries)
	}

	bundle := ""
	for _, f := range zr.File {
		b, ok := appRoot(f.Name)
		if !ok {
			continue
		}
		if bundle != "" && b != bundle {
			return "", fmt.Errorf("%w: more than one app (%s and %s)", ErrNotIOSApp, bundle, b)
		}
		bundle = b
	}
	if bundle == "" {
		return "", fmt.Errorf("%w: no .app in the archive", ErrNotIOSApp)
	}

	root := filepath.Join(dir, filepath.Base(bundle))
	for _, f := range zr.File {
		name := filepath.ToSlash(f.Name)
		if name != bundle+"/" && !strings.HasPrefix(name, bundle+"/") {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(name, bundle), "/")
		dst := filepath.Join(root, filepath.FromSlash(rel))
		if !within(root, dst) {
			return "", fmt.Errorf("%w: %s lands outside the bundle", ErrNotIOSApp, f.Name)
		}
		if err := unpackEntry(f, root, dst); err != nil {
			return "", err
		}
	}
	if _, err := os.Stat(filepath.Join(root, "Info.plist")); err != nil {
		return "", fmt.Errorf("%w: %s has no Info.plist", ErrNotIOSApp, filepath.Base(bundle))
	}
	return root, nil
}

// appRoot is the bundle an entry belongs to: "Payload/Name.app" or
// "Name.app". macOS's zip adds __MACOSX/ copies of resource forks, which
// belong to nothing.
func appRoot(name string) (string, bool) {
	name = filepath.ToSlash(name)
	if strings.HasPrefix(name, "__MACOSX/") {
		return "", false
	}
	parts := strings.Split(name, "/")
	switch {
	case len(parts) >= 2 && parts[0] == "Payload" && strings.HasSuffix(parts[1], ".app"):
		return parts[0] + "/" + parts[1], true
	case strings.HasSuffix(parts[0], ".app") && len(parts) >= 2:
		return parts[0], true
	}
	return "", false
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// unpackEntry writes one entry, keeping its executable bit: a bundle's main
// binary is refused without it. A symlink is kept as one, since frameworks
// may carry them, as long as it resolves inside the bundle.
func unpackEntry(f *zip.File, root, dst string) error {
	mode := f.Mode()
	switch {
	case mode.IsDir():
		return os.MkdirAll(dst, 0o755)
	case mode&os.ModeSymlink != 0:
		rc, err := f.Open()
		if err != nil {
			return err
		}
		target, err := io.ReadAll(io.LimitReader(rc, 4096))
		rc.Close()
		if err != nil {
			return err
		}
		link := string(target)
		if filepath.IsAbs(link) || !within(root, filepath.Join(filepath.Dir(dst), link)) {
			return fmt.Errorf("%w: link %s points out of the bundle", ErrNotIOSApp, f.Name)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.Symlink(link, dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	perm := os.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// appBundleID reads CFBundleIdentifier out of an unpacked bundle. plutil
// reads both the XML and binary forms an Info.plist ships in. A var so
// tests off macOS do not need it.
var appBundleID = func(ctx context.Context, app string) (string, error) {
	out, err := exec.CommandContext(ctx, "plutil", "-extract", "CFBundleIdentifier", "raw", "-o", "-",
		filepath.Join(app, "Info.plist")).Output()
	if err != nil {
		return "", fmt.Errorf("%w: Info.plist has no CFBundleIdentifier", ErrNotIOSApp)
	}
	return strings.TrimSpace(string(out)), nil
}

// devicectlError is the error object `devicectl -j` writes when a command
// fails: the outermost description, with each underlying error's below it.
type devicectlError struct {
	Error *devicectlNSError `json:"error"`
}

type devicectlNSError struct {
	Code     int    `json:"code"`
	Domain   string `json:"domain"`
	UserInfo struct {
		Description struct {
			S string `json:"string"`
		} `json:"NSLocalizedDescription"`
		Reason struct {
			S string `json:"string"`
		} `json:"NSLocalizedFailureReason"`
		Suggestion struct {
			S string `json:"string"`
		} `json:"NSLocalizedRecoverySuggestion"`
		Underlying *struct {
			Error *devicectlNSError `json:"error"`
		} `json:"NSUnderlyingError"`
	} `json:"userInfo"`
}

// parseDevicectlInstall reads a failed `devicectl device install app -j`.
// Code is the innermost CoreDevice error, which is the one that says what
// was wrong with the app (3000 is "not a valid bundle"); Reason is every
// level's description, outermost first. ok is false when there is no error
// object, so the caller reports the exit instead.
func parseDevicectlInstall(b []byte) (*InstallError, bool) {
	var d devicectlError
	if json.Unmarshal(b, &d) != nil || d.Error == nil {
		return nil, false
	}
	var parts []string
	inner := d.Error
	for e := d.Error; e != nil; {
		for _, s := range []string{e.UserInfo.Description.S, e.UserInfo.Reason.S, e.UserInfo.Suggestion.S} {
			if s != "" {
				parts = append(parts, s)
			}
		}
		inner = e
		if e.UserInfo.Underlying == nil {
			break
		}
		e = e.UserInfo.Underlying.Error
	}
	return &InstallError{
		Code:   fmt.Sprintf("%s %d", inner.Domain, inner.Code),
		Reason: strings.Join(parts, " "),
		Output: string(b),
	}, true
}
