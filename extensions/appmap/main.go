// Command appmap is a loupe extension that records an agent's walk through an
// app as a map: each screen once, with a screenshot and its UI tree, and the
// actions that lead between screens. The agent decides where to go and reports
// each step; this extension sends the input, waits for the screen to settle,
// and works out whether it has seen that screen before. See docs/app-map.md.
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
	"time"
)

func main() {
	var c Config
	flag.StringVar(&c.Addr, "addr", "127.0.0.1:18494", "listen address")
	flag.StringVar(&c.LoupeURL, "loupe", "https://localhost:18456", "loupe base URL")
	flag.StringVar(&c.Token, "token", "", "loupe token (default LOUPE_TOKEN, then ~/Library/Caches/loupe/token)")
	flag.StringVar(&c.DataDir, "data-dir", "", "where maps live (default ~/Library/Caches/loupe/app-map)")
	flag.DurationVar(&c.SettleTimeout, "settle", 3*time.Second, "longest wait for the screen to stop changing after a step")
	flag.Parse()

	home, _ := os.UserHomeDir()
	if c.DataDir == "" {
		c.DataDir = filepath.Join(home, "Library", "Caches", "loupe", "app-map")
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
	log.Printf("appmap: listening on %s, loupe %s, data %s", c.Addr, c.LoupeURL, c.DataDir)
	go newLoupeClient(c.LoupeURL, c.Token).keepRegistered(c.Addr, log.Printf)
	log.Fatal(http.ListenAndServe(c.Addr, s.Handler()))
}
