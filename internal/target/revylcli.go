package target

import (
	"os"
	"os/exec"
	"path/filepath"
)

// PrivateRevylCLI is where loupe puts a revyl CLI it downloads itself, next
// to its android CLI. It needs no admin rights to write.
func PrivateRevylCLI() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "loupe", "bin", "revyl")
}

// RevylWorkDir is the directory revyl runs in, so it neither reads a
// project's .revyl/config.yaml from wherever loupe was started nor caches
// sessions there.
func RevylWorkDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "loupe", "revyl")
}

// FindRevylCLI finds the revyl CLI: the explicit path, then where Revyl's
// install script puts it (~/.revyl/bin, scripts/install.sh), the two
// usual bin directories, PATH, and loupe's own download. launchd's PATH
// reaches none of the others, so PATH comes after them. It returns "" when
// there is none.
func FindRevylCLI(explicit string) string {
	cands := []string{explicit}
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands, filepath.Join(home, ".revyl", "bin", "revyl"))
	}
	cands = append(cands, "/opt/homebrew/bin/revyl", "/usr/local/bin/revyl")
	if p, err := exec.LookPath("revyl"); err == nil {
		cands = append(cands, p)
	}
	cands = append(cands, PrivateRevylCLI())
	for _, c := range cands {
		if c == "" {
			continue
		}
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return ""
}
