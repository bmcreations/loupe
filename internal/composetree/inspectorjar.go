package composetree

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
)

// MavenBase is where ui-android AARs are published. Each carries the
// Compose inspector for its own version as a root-level inspector.jar.
const MavenBase = "https://dl.google.com/android/maven2/androidx/compose/ui/ui-android"

// versionRE is what an app's META-INF/androidx.compose.ui_ui.version may
// say. The version goes into a URL and a cache path, so nothing else passes.
var versionRE = regexp.MustCompile(`^\d+\.\d+\.\d+(-(alpha|beta|rc)\d+)?$`)

// InspectorJar returns the path of the inspector.jar for a Compose version,
// downloading ui-android-<version>.aar from baseURL on first use and caching
// the jar under cacheDir/compose-inspector/<version>/.
func InspectorJar(ctx context.Context, cacheDir, baseURL, version string) (string, error) {
	if !versionRE.MatchString(version) {
		return "", fmt.Errorf("compose inspector: unexpected Compose version %q", version)
	}
	dir := filepath.Join(cacheDir, "compose-inspector", version)
	path := filepath.Join(dir, "inspector.jar")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}

	url := fmt.Sprintf("%s/%s/ui-android-%s.aar", baseURL, version, version)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("compose inspector: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("compose inspector: GET %s: %s", url, resp.Status)
	}
	aar, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return "", fmt.Errorf("compose inspector: %w", err)
	}
	z, err := zip.NewReader(bytes.NewReader(aar), int64(len(aar)))
	if err != nil {
		return "", fmt.Errorf("compose inspector: %s: %w", url, err)
	}
	f, err := z.Open("inspector.jar")
	if err != nil {
		return "", fmt.Errorf("compose inspector: %s has no inspector.jar", url)
	}
	defer f.Close()
	jar, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// Rename into place so a reader never sees half a jar.
	tmp, err := os.CreateTemp(dir, "inspector-*.jar")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(jar); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}
