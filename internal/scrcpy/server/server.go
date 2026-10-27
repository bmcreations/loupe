// Package server embeds the scrcpy-server jar that loupe pushes to the device.
//
// Embedding it rather than detecting a host install removes the requirement
// that the machine running loupe also have scrcpy installed. Only the jar was
// ever used: the desktop binary was consulted purely to read a version string,
// which an embedded jar knows at build time.
//
// It also makes the jar a build input rather than something that can move
// underneath the process, which is what lets server_test.go assert loupe's
// protocol constants against the artifact they describe.
package server

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// Version is the embedded jar's own version string.
//
// It is not cosmetic. scrcpy-server compares the version passed on its command
// line against its own BuildConfig.VERSION_NAME and exits before opening its
// socket when they differ, which surfaces as a connect timeout with nothing
// explaining it. TestVersionMatchesTheJar keeps this honest.
const Version = "3.3.4"

// SHA256 pins the embedded jar. Any change to the artifact fails
// TestJarIsThePinnedArtifact, which is deliberate: loupe hardcodes constants
// read out of this jar's dex, and a new jar means every one of them needs
// re-checking. scrcpy 4.1 moved PACKET_FLAG_CONFIG from bit 63 to bit 62 and
// renamed send_codec_meta, neither of which fails loudly at runtime.
const SHA256 = "8588238c9a5a00aa542906b6ec7e6d5541d9ffb9b5d0f6e1bc0e365e2303079e"

//go:embed scrcpy-server-3.3.4.jar
var jar []byte

// Jar returns the embedded server bytes.
func Jar() []byte { return jar }

// DefaultDir is where Materialize stages the jar when no directory is given.
func DefaultDir() string {
	cache, err := os.UserCacheDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "loupe")
	}
	return filepath.Join(cache, "loupe")
}

// Materialize writes the embedded jar into dir and returns the path.
//
// adb push takes a host path, so the bytes have to reach the filesystem before
// a device launch can use them. Writing is skipped when the file is already
// present with the right contents, so repeated launches cost a hash rather
// than a write. An empty dir means DefaultDir.
func Materialize(dir string) (string, error) {
	if dir == "" {
		dir = DefaultDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("stage scrcpy-server: %w", err)
	}
	path := filepath.Join(dir, "scrcpy-server-"+Version+".jar")

	if existing, err := os.ReadFile(path); err == nil && sum(existing) == sum(jar) {
		return path, nil
	}
	// Write to a sibling and rename, so a concurrent launch never observes a
	// half-written jar and pushes it to a device.
	tmp, err := os.CreateTemp(dir, ".scrcpy-server-*")
	if err != nil {
		return "", fmt.Errorf("stage scrcpy-server: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(jar); err != nil {
		tmp.Close()
		return "", fmt.Errorf("stage scrcpy-server: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("stage scrcpy-server: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("stage scrcpy-server: %w", err)
	}
	return path, nil
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
