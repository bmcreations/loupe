package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// The revyl CLI loupe downloads when none is installed. Revyl publishes no
// Homebrew cask, so the version and checksums are pinned here rather than
// read from a cask record as the android CLI's are (remotecli.go). The
// asset names come from revyl-cli's scripts/build-all.sh:63 and
// scripts/install.sh:255 at v0.2.27; the sums are that release's
// checksums.txt.
const (
	revylCLIVersion = "v0.2.27"
	revylCLIBase    = "https://github.com/RevylAI/revyl-cli/releases/download/" + revylCLIVersion + "/"
	// revylCLIMaxBytes is above the 46.8 MB arm64 and 49.5 MB amd64
	// binaries with room for a few releases, and below anything a
	// misdirected download should be allowed to fill memory with.
	revylCLIMaxBytes = 128 << 20
)

var revylCLISums = map[string]string{
	"arm64": "96e20b13e1ec8efc231c1ffea08d7c19d62cc49b9820775744169aa25c399369",
	"amd64": "92f8e2b9cb2feea12cbf3d1563821c35a68ce13b0830fe7191b66ec4c342b0f7",
}

// RevylInstaller downloads the pinned revyl CLI into loupe's own bin
// directory. It needs no admin rights and no Homebrew.
type RevylInstaller struct {
	// PrivatePath is where the binary goes (revyl.PrivateCLI).
	PrivatePath string
	// OS and Arch pick the asset; only darwin is published for here.
	OS, Arch string

	fetch func(ctx context.Context, url string, max int64) ([]byte, error)
}

// NewRevylInstaller installs to privatePath for this machine.
func NewRevylInstaller(privatePath string) *RevylInstaller {
	return &RevylInstaller{PrivatePath: privatePath, OS: runtime.GOOS, Arch: runtime.GOARCH, fetch: fetchLimited}
}

// Install downloads the binary, checks it against the pinned sum, and
// puts it in place.
func (in *RevylInstaller) Install(ctx context.Context) error {
	if in.PrivatePath == "" {
		return errors.New("no place to put the revyl CLI")
	}
	want, ok := revylCLISums[in.Arch]
	if in.OS != "darwin" || !ok {
		return fmt.Errorf("loupe downloads the revyl CLI for macOS on arm64 or amd64, not %s/%s", in.OS, in.Arch)
	}
	bin, err := in.fetch(ctx, revylCLIBase+"revyl-darwin-"+in.Arch, revylCLIMaxBytes)
	if err != nil {
		return fmt.Errorf("download the revyl CLI: %w", err)
	}
	if got := sha256.Sum256(bin); !strings.EqualFold(hex.EncodeToString(got[:]), want) {
		return errors.New("the downloaded revyl CLI does not match its published checksum")
	}
	return writeExecutable(in.PrivatePath, bin)
}

// fetchLimited is httpFetch with its own size cap: the revyl binary is too
// close to cliMaxBytes to share it.
func fetchLimited(ctx context.Context, url string, max int64) ([]byte, error) {
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
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is larger than %d MB", url, max>>20)
	}
	return b, nil
}

// writeExecutable writes bin to path through a .part file, so a reader
// never runs half a binary.
func writeExecutable(path string, bin []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".part"
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
