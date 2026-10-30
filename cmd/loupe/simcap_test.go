package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveSimcap(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	if got := resolveSimcap(""); got != ".build/simcap" {
		t.Errorf("nothing built: %q", got)
	}
	onPath := filepath.Join(bin, "simcap")
	if err := os.WriteFile(onPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := resolveSimcap(""); got != onPath {
		t.Errorf("on PATH: %q, want %q", got, onPath)
	}
	if err := os.MkdirAll(".build", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".build/simcap", nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := resolveSimcap(""); got != ".build/simcap" {
		t.Errorf("checkout build should win: %q", got)
	}
	if got := resolveSimcap("/x/simcap"); got != "/x/simcap" {
		t.Errorf("flag: %q", got)
	}
}
