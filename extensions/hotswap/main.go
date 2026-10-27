// Command hotswap is a loupe extension: one call applies a code patch to a
// running debug app, waits for the app to redraw, and returns a screenshot and
// the UI tree. It serves the hotswap protocol the debug apps already poll
// (/publish, /next, /seq, /patches/, /report) and reaches loupe only over HTTP.
//
// It imports nothing from loupe's internal packages.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	var c Config
	flag.StringVar(&c.Addr, "addr", ":18492", "listen address; the app polls this")
	flag.StringVar(&c.LoupeURL, "loupe", "https://localhost:18456", "loupe base URL")
	flag.StringVar(&c.Token, "token", "", "loupe token (default LOUPE_TOKEN, then ~/Library/Caches/loupe/token)")
	flag.StringVar(&c.BuildCmd, "build-cmd", "", "shell command that builds a patch (required); see docs/hotswap.md")
	flag.StringVar(&c.BuildDir, "build-dir", ".", "working directory of the build command (the app repo)")
	flag.StringVar(&c.DataDir, "data-dir", "", "where patches and screenshots live (default ~/Library/Caches/loupe/hotswap)")
	flag.DurationVar(&c.BuildTimeout, "build-timeout", 180*time.Second, "build timeout")
	flag.DurationVar(&c.ReportTimeout, "report-timeout", 20*time.Second, "how long to wait for the app's report")
	flag.DurationVar(&c.PollWindow, "poll-window", 5*time.Second, "an app that has not polled /next within this is not polling")
	flag.DurationVar(&c.SettleTimeout, "settle-timeout", 2500*time.Millisecond, "after the report, screenshot until two match or this passes; 0 takes one shot")
	flag.Parse()

	if c.BuildCmd == "" {
		fmt.Fprintln(os.Stderr, "hotswap: -build-cmd is required")
		os.Exit(2)
	}
	if c.DataDir == "" {
		home, _ := os.UserHomeDir()
		c.DataDir = filepath.Join(home, "Library", "Caches", "loupe", "hotswap")
	}
	if c.Token == "" {
		c.Token = os.Getenv("LOUPE_TOKEN")
	}
	if c.Token == "" {
		home, _ := os.UserHomeDir()
		if b, err := os.ReadFile(filepath.Join(home, "Library", "Caches", "loupe", "token")); err == nil {
			c.Token = strings.TrimSpace(string(b))
		}
	}
	s, err := NewServer(c)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("hotswap: listening on %s, seq %d, loupe %s, data %s", c.Addr, s.currentSeq(), c.LoupeURL, c.DataDir)
	log.Fatal(http.ListenAndServe(c.Addr, s.Handler()))
}
