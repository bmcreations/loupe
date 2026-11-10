package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// fakeCLIBin stands in for the downloaded CLI. It is a script that prints
// nothing, so a test that ends up running it reads "not signed in".
var fakeCLIBin = []byte("#!/bin/sh\nexit 0\n")

// caskFor is Homebrew's record with its checksums swapped for fakeCLIBin's,
// so a download of fakeCLIBin verifies.
func caskFor(t *testing.T, bin []byte) []byte {
	t.Helper()
	sum := sha256.Sum256(bin)
	var cask map[string]any
	if err := json.Unmarshal(remoteFixture(t, "fx-cask.json"), &cask); err != nil {
		t.Fatal(err)
	}
	cask["sha256"] = hex.EncodeToString(sum[:])
	out, _ := json.Marshal(cask)
	return out
}

func testInstaller(t *testing.T, record, bin []byte) (*CLIInstaller, *[]string) {
	t.Helper()
	var asked []string
	in := &CLIInstaller{PrivatePath: filepath.Join(t.TempDir(), "bin", "android"), Arch: "arm64"}
	in.fetch = func(_ context.Context, url string) ([]byte, error) {
		asked = append(asked, url)
		if url == cliCaskURL {
			return record, nil
		}
		return bin, nil
	}
	in.runBrew = func(context.Context, string) ([]byte, error) {
		t.Fatal("brew ran without a brew path")
		return nil, nil
	}
	return in, &asked
}

func TestCaskDownloadPicksTheMacBuildForTheArch(t *testing.T) {
	record := remoteFixture(t, "fx-cask.json")
	url, sum, err := caskDownload(record, "arm64")
	if err != nil || !strings.HasSuffix(url, "/darwin_arm64/android") || len(sum) != 64 {
		t.Fatalf("arm64 = %q %q %v", url, sum, err)
	}
	intel, intelSum, err := caskDownload(record, "amd64")
	if err != nil || !strings.Contains(intel, "darwin") || strings.Contains(intel, "arm64") || intelSum == sum {
		t.Fatalf("amd64 = %q %q %v", intel, intelSum, err)
	}
	if _, _, err := caskDownload([]byte(`{"url":"https://example.com/android","sha256":"`+sum+`"}`), "arm64"); err == nil {
		t.Error("a download outside dl.google.com was accepted")
	}
}

func TestInstallDownloadsAndVerifiesWithoutHomebrew(t *testing.T) {
	in, asked := testInstaller(t, caskFor(t, fakeCLIBin), fakeCLIBin)
	how, err := in.Install(context.Background())
	if err != nil || how != "download" {
		t.Fatalf("Install = %q, %v", how, err)
	}
	fi, err := os.Stat(in.PrivatePath)
	if err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("installed file: %v %v", fi, err)
	}
	if len(*asked) != 2 || !strings.HasPrefix((*asked)[1], "https://dl.google.com/") {
		t.Errorf("fetched %v", *asked)
	}
}

func TestInstallRefusesADownloadThatFailsItsChecksum(t *testing.T) {
	in, _ := testInstaller(t, caskFor(t, fakeCLIBin), []byte("something else"))
	if _, err := in.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(in.PrivatePath); !os.IsNotExist(err) {
		t.Errorf("a file was left behind: %v", err)
	}
}

func TestInstallPrefersHomebrewAndFallsBackWhenItFails(t *testing.T) {
	in, asked := testInstaller(t, caskFor(t, fakeCLIBin), fakeCLIBin)
	in.Brew = "/opt/homebrew/bin/brew"
	in.runBrew = func(context.Context, string) ([]byte, error) { return []byte("installed"), nil }
	if how, err := in.Install(context.Background()); err != nil || how != "homebrew" || len(*asked) != 0 {
		t.Fatalf("with brew: %q %v fetched %v", how, err, *asked)
	}
	in.runBrew = func(context.Context, string) ([]byte, error) {
		return []byte("Error: no network\n"), errors.New("exit status 1")
	}
	if how, err := in.Install(context.Background()); err != nil || how != "download" {
		t.Fatalf("after brew failed: %q %v", how, err)
	}
}

func TestRemoteCLIRouteInstallsAndTheNextLookupFindsIt(t *testing.T) {
	in, _ := testInstaller(t, caskFor(t, fakeCLIBin), fakeCLIBin)
	find := func() string {
		if _, err := os.Stat(in.PrivatePath); err != nil {
			return ""
		}
		return in.PrivatePath
	}
	rd := NewRemoteDevices(find, func(string, string) {}, nil)
	s := &Server{mgr: NewManager(target.Options{})}
	s.SetRemoteDevices(rd)

	if rec := do(s, http.MethodPost, "/api/remote/cli", ""); rec.Code != http.StatusNotImplemented {
		t.Fatalf("without an installer = %d", rec.Code)
	}
	rd.SetCLIInstaller(in)
	var acct RemoteAccountResponse
	rec := do(s, http.MethodGet, "/api/remote/account", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &acct); err != nil || acct.CLIInstalled || !acct.CanInstallCLI {
		t.Fatalf("before: %s", rec.Body)
	}
	if rec := do(s, http.MethodPost, "/api/remote/cli", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("install = %d %s", rec.Code, rec.Body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec := do(s, http.MethodGet, "/api/remote/account", "")
		acct = RemoteAccountResponse{}
		if err := json.Unmarshal(rec.Body.Bytes(), &acct); err != nil {
			t.Fatal(err)
		}
		if acct.CLIInstalled && !acct.CLIInstalling {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never installed: %s", rec.Body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if acct.CLIInstallError != "" || acct.SignedIn {
		t.Errorf("after: %+v", acct)
	}
}

func TestRemoteCLIRouteReportsAFailedInstall(t *testing.T) {
	in, _ := testInstaller(t, caskFor(t, fakeCLIBin), []byte("tampered"))
	rd := NewRemoteDevices(func() string { return "" }, func(string, string) {}, nil)
	rd.SetCLIInstaller(in)
	s := &Server{mgr: NewManager(target.Options{})}
	s.SetRemoteDevices(rd)
	do(s, http.MethodPost, "/api/remote/cli", "")
	deadline := time.Now().Add(5 * time.Second)
	for {
		var acct RemoteAccountResponse
		rec := do(s, http.MethodGet, "/api/remote/account", "")
		_ = json.Unmarshal(rec.Body.Bytes(), &acct)
		if !acct.CLIInstalling && acct.CLIInstallError != "" {
			if acct.CLIInstalled || !strings.Contains(acct.CLIInstallError, "checksum") {
				t.Errorf("after: %+v", acct)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no failure reported: %s", rec.Body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestInstallFromTheRealDownload fetches Google's binary for real. It runs
// only with LOUPE_NET_TEST=1.
func TestInstallFromTheRealDownload(t *testing.T) {
	if os.Getenv("LOUPE_NET_TEST") == "" {
		t.Skip("set LOUPE_NET_TEST=1 to download the android CLI")
	}
	in := NewCLIInstaller(filepath.Join(t.TempDir(), "bin", "android"))
	in.Brew = ""
	how, err := in.Install(context.Background())
	if err != nil || how != "download" {
		t.Fatalf("Install = %q, %v", how, err)
	}
	fi, err := os.Stat(in.PrivatePath)
	if err != nil || fi.Size() < 1<<20 {
		t.Fatalf("downloaded file: %v %v", fi, err)
	}
	t.Logf("downloaded %d bytes to %s", fi.Size(), in.PrivatePath)
}
