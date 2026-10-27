// Command speccompare is a loupe extension: it stores design references for a
// target (a Figma frame export, usually) and compares the live screen against
// one. A comparison returns a score, a diff image and the regions that differ,
// each tied to the /api/ui node under it, and shows the reference and the
// regions to viewers through loupe's /api/overlays. See docs/spec-compare.md.
//
// It imports nothing from loupe's internal packages.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	var c Config
	flag.StringVar(&c.Addr, "addr", "127.0.0.1:18493", "listen address")
	flag.StringVar(&c.LoupeURL, "loupe", "https://localhost:18456", "loupe base URL")
	flag.StringVar(&c.Token, "token", "", "loupe token (default LOUPE_TOKEN, then ~/Library/Caches/loupe/token)")
	flag.StringVar(&c.DataDir, "data-dir", "", "where references and diffs live (default ~/Library/Caches/loupe/speccompare)")
	flag.Parse()

	home, _ := os.UserHomeDir()
	if c.DataDir == "" {
		c.DataDir = filepath.Join(home, "Library", "Caches", "loupe", "speccompare")
	}
	if c.Token == "" {
		c.Token = os.Getenv("LOUPE_TOKEN")
	}
	if c.Token == "" {
		if b, err := os.ReadFile(filepath.Join(home, "Library", "Caches", "loupe", "token")); err == nil {
			c.Token = strings.TrimSpace(string(b))
		}
	}
	s, err := NewServer(c)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("speccompare: listening on %s, loupe %s, data %s", c.Addr, c.LoupeURL, c.DataDir)
	log.Fatal(http.ListenAndServe(c.Addr, s.Handler()))
}
