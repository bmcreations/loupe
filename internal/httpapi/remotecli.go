package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	cliInstallTimeout = 5 * time.Minute
	// cliMaxBytes bounds the download; the CLI is about 6 MB.
	cliMaxBytes = 64 << 20
	// cliCaskURL is Homebrew's public record of the CLI's download URL and
	// checksum, so loupe pins neither.
	cliCaskURL = "https://formulae.brew.sh/api/cask/android-cli.json"
)

// CLIInstaller installs Google's android CLI on this host: through Homebrew
// when it is there, so updates stay with Homebrew, and otherwise as a
// checksum-verified download into PrivatePath, which needs no admin rights.
type CLIInstaller struct {
	// Brew is the path to brew, or "" when Homebrew is not installed.
	Brew string
	// PrivatePath is where the download lands; target.AndroidCLI looks there.
	PrivatePath string
	// Arch is runtime.GOARCH; tests set it.
	Arch string

	runBrew func(ctx context.Context, brew string) ([]byte, error)
	fetch   func(ctx context.Context, url string) ([]byte, error)
}

// NewCLIInstaller looks for brew in the two places Homebrew installs to,
// since launchd's PATH reaches neither.
func NewCLIInstaller(privatePath string) *CLIInstaller {
	in := &CLIInstaller{PrivatePath: privatePath, Arch: runtime.GOARCH, runBrew: runBrewInstall, fetch: httpFetch}
	for _, c := range []string{"/opt/homebrew/bin/brew", "/usr/local/bin/brew"} {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			in.Brew = c
			break
		}
	}
	return in
}

func runBrewInstall(ctx context.Context, brew string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, brew, "install", "--cask", "android-cli")
	cmd.Env = append(os.Environ(),
		"PATH="+filepath.Dir(brew)+":/usr/bin:/bin:/usr/sbin:/sbin",
		"HOMEBREW_NO_AUTO_UPDATE=1", "HOMEBREW_NO_ENV_HINTS=1", "NONINTERACTIVE=1")
	return cmd.CombinedOutput()
}

func httpFetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, cliMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > cliMaxBytes {
		return nil, fmt.Errorf("%s is larger than %d MB", url, cliMaxBytes>>20)
	}
	return b, nil
}

// caskDownload picks the macOS download for arch out of Homebrew's cask
// record: the top level is Apple silicon, and Intel is under variations.
func caskDownload(record []byte, arch string) (url, sum string, err error) {
	var cask struct {
		URL        string `json:"url"`
		SHA256     string `json:"sha256"`
		Variations map[string]struct {
			URL    string `json:"url"`
			SHA256 string `json:"sha256"`
		} `json:"variations"`
	}
	if err := json.Unmarshal(record, &cask); err != nil {
		return "", "", fmt.Errorf("read the cask record: %w", err)
	}
	url, sum = cask.URL, cask.SHA256
	if arch == "amd64" {
		url, sum = "", ""
		keys := make([]string, 0, len(cask.Variations))
		for k := range cask.Variations {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if v := cask.Variations[k]; !strings.Contains(k, "linux") && strings.Contains(v.URL, "darwin") && v.SHA256 != "" {
				url, sum = v.URL, v.SHA256
				break
			}
		}
	}
	if !strings.HasPrefix(url, "https://dl.google.com/") || len(sum) != 64 {
		return "", "", errors.New("the cask record has no Google download with a checksum for this Mac")
	}
	return url, sum, nil
}

// Install returns how the CLI was installed: "homebrew" or "download".
func (in *CLIInstaller) Install(ctx context.Context) (string, error) {
	var brewErr error
	if in.Brew != "" {
		out, err := in.runBrew(ctx, in.Brew)
		if err == nil {
			return "homebrew", nil
		}
		brewErr = fmt.Errorf("brew install --cask android-cli: %s", brewFailure(out, err))
	}
	if err := in.download(ctx); err != nil {
		if brewErr != nil {
			return "", fmt.Errorf("%w; then %w", brewErr, err)
		}
		return "", err
	}
	return "download", nil
}

func (in *CLIInstaller) download(ctx context.Context) error {
	if in.PrivatePath == "" {
		return errors.New("no place to put the android CLI")
	}
	record, err := in.fetch(ctx, cliCaskURL)
	if err != nil {
		return fmt.Errorf("look up the android CLI download: %w", err)
	}
	url, want, err := caskDownload(record, in.Arch)
	if err != nil {
		return err
	}
	bin, err := in.fetch(ctx, url)
	if err != nil {
		return fmt.Errorf("download the android CLI: %w", err)
	}
	if got := sha256.Sum256(bin); !strings.EqualFold(hex.EncodeToString(got[:]), want) {
		return errors.New("the downloaded android CLI does not match its published checksum")
	}
	if err := os.MkdirAll(filepath.Dir(in.PrivatePath), 0o755); err != nil {
		return err
	}
	tmp := in.PrivatePath + ".part"
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, in.PrivatePath)
}

func brewFailure(out []byte, err error) string {
	lines := strings.Split(strings.TrimSpace(string(bytes.TrimSpace(out))), "\n")
	if s := strings.TrimSpace(lines[len(lines)-1]); s != "" {
		return s
	}
	return err.Error()
}

// SetCLIInstaller lets POST /api/remote/cli install the CLI. Without one the
// route answers 501.
func (r *RemoteDevices) SetCLIInstaller(in *CLIInstaller) { r.installer = in }

// installCLI starts an install and returns at once; /api/remote/account
// reports cliInstalling and, after a failure, cliInstallError.
func (r *RemoteDevices) installCLI() {
	r.mu.Lock()
	if r.installing {
		r.mu.Unlock()
		return
	}
	r.installing, r.installErr = true, ""
	r.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), cliInstallTimeout)
		defer cancel()
		how, err := r.installer.Install(ctx)
		rec := map[string]any{"action": "install-cli", "how": how}
		r.mu.Lock()
		r.installing = false
		if err != nil {
			r.installErr = err.Error()
			rec["error"] = r.installErr
		}
		r.mu.Unlock()
		r.record(rec)
	}()
}

func (s *Server) handleRemoteCLI(w http.ResponseWriter, r *http.Request) {
	rd := s.remote
	if rd == nil {
		http.NotFound(w, r)
		return
	}
	if rd.installer == nil {
		http.Error(w, "this server cannot install the android CLI", http.StatusNotImplemented)
		return
	}
	if !rd.Installed() {
		rd.installCLI()
	}
	ctx, cancel := context.WithTimeout(r.Context(), remoteCallTimeout)
	defer cancel()
	writeJSON(w, http.StatusAccepted, rd.account(ctx))
}
