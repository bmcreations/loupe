package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testBanner() banner {
	return banner{
		addr:       ":18456",
		scheme:     "https",
		handoff:    "https://mac.example.ts.net:18456/?token=s3cr3t",
		advertised: []string{"https://localhost:18456", "https://100.1.2.3:18456"},
		token:      "s3cr3t",
		tokenWhere: "/cache/loupe/token",
		tls:        true,
	}
}

// A terminal is someone at the keyboard who needs the link. Anything else is
// a file -- loupe-run.log under launchd -- and the token in it would outlive
// the run and be readable wherever the file is.
func TestBannerWithholdsTheTokenFromAFile(t *testing.T) {
	var sb strings.Builder
	testBanner().write(&sb, false)
	out := sb.String()

	if strings.Contains(out, "s3cr3t") {
		t.Errorf("banner for a file holds the token:\n%s", out)
	}
	if !strings.Contains(out, "/cache/loupe/token") {
		t.Errorf("banner does not say where the token is:\n%s", out)
	}
	if !strings.Contains(out, "https://100.1.2.3:18456") {
		t.Errorf("banner lost the bare links:\n%s", out)
	}
}

func TestBannerPrintsTheTokenToATerminal(t *testing.T) {
	var sb strings.Builder
	testBanner().write(&sb, true)
	out := sb.String()
	for _, want := range []string{
		"phone: https://mac.example.ts.net:18456/?token=s3cr3t",
		"https://100.1.2.3:18456?token=s3cr3t",
		"Authorization: Bearer s3cr3t",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("terminal banner is missing %q:\n%s", want, out)
		}
	}
}

// With -no-auth there is no secret, so the phone link and its QR code stay.
func TestBannerWithoutAuthKeepsThePhoneLink(t *testing.T) {
	b := testBanner()
	b.token, b.handoff = "", "https://mac.example.ts.net:18456/"
	var sb strings.Builder
	b.write(&sb, false)
	if !strings.Contains(sb.String(), "phone: https://mac.example.ts.net:18456/") {
		t.Errorf("no-auth banner lost the phone link:\n%s", sb.String())
	}
}

func TestTokenWhere(t *testing.T) {
	t.Setenv("LOUPE_TOKEN", "")
	if got := tokenWhere("x"); !strings.Contains(got, "-token") {
		t.Errorf("flag: %q", got)
	}
	if got := tokenWhere(""); !strings.HasSuffix(got, tokenFileName) {
		t.Errorf("file: %q", got)
	}
	t.Setenv("LOUPE_TOKEN", "y")
	if got := tokenWhere(""); !strings.Contains(got, "LOUPE_TOKEN") {
		t.Errorf("env: %q", got)
	}
}

// launchd creates loupe-run.log 0644 before the server runs; the server
// tightens whatever file its output was pointed at.
func TestTightenOutputMakesALogFileOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	tightenOutput(f)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if m := st.Mode().Perm(); m != 0o600 {
		t.Errorf("mode %o, want 600", m)
	}
}
