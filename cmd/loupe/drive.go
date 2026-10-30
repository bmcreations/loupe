package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/bmcreations/loupe/internal/mcpserver"
	"github.com/bmcreations/loupe/internal/target"
)

// driveFlags are the flags `loupe tap` and `loupe sequence` share: which
// server, as whom, on which target.
type driveFlags struct {
	addr, token, agent, session, target *string
}

func newDriveFlags(fs *flag.FlagSet) driveFlags {
	return driveFlags{
		addr:    fs.String("url", "https://localhost:18456", "the loupe server to drive"),
		token:   fs.String("token", "", "shared token (default: LOUPE_TOKEN, then the server's token file)"),
		agent:   fs.String("agent", "cli", "name sent as X-Loupe-Agent"),
		session: fs.String("session", os.Getenv("LOUPE_SESSION"), "session sent as X-Loupe-Session, to act as an agent session that holds the claim (default: LOUPE_SESSION)"),
		target:  fs.String("target", os.Getenv("LOUPE_TARGET"), "target id, e.g. emulator-5554 or sim:<udid> (default: LOUPE_TARGET)"),
	}
}

// post sends body to path and prints the answer: to stdout on success, to
// stderr with exit 1 on any other status.
func (d driveFlags) post(path string, body []byte) int {
	if *d.target == "" {
		fmt.Fprintln(os.Stderr, "missing -target (or LOUPE_TARGET)")
		return 2
	}
	tok, err := mcpToken(*d.token)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	c, err := mcpserver.NewClient(*d.addr, tok, *d.agent)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	c.UseSession(*d.session)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	out, err := c.Post(ctx, path, *d.target, body)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(string(out))
	return 0
}

// runTap is `loupe tap`: one tap at x y, or on the node named by -text, -id
// or -desc, which the server finds in the view hierarchy.
func runTap(args []string) int {
	fs := flag.NewFlagSet("loupe tap", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: loupe tap -target ID [flags] (X Y | -text S | -id S | -desc S)")
		fs.PrintDefaults()
	}
	d := newDriveFlags(fs)
	var (
		text     = fs.String("text", "", "tap the node with this text")
		id       = fs.String("id", "", "tap the node with this resource-id or accessibility identifier")
		desc     = fs.String("desc", "", "tap the node with this content description")
		contains = fs.Bool("contains", false, "match -text and -desc as case-insensitive substrings")
		index    = fs.Int("index", -1, "which of several matches, in tree order from 0")
		waitMs   = fs.Int("wait", 0, "keep looking for the node this many ms, up to 20000")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	body := map[string]any{"action": "tap"}
	switch {
	case *text != "" || *id != "" || *desc != "":
		if fs.NArg() != 0 {
			fs.Usage()
			return 2
		}
		m := target.Match{ID: *id, Text: *text, Desc: *desc, Contains: *contains, WaitMs: *waitMs}
		if *index >= 0 {
			m.Index = index
		}
		body["match"] = m
	case fs.NArg() == 2:
		x, errX := strconv.Atoi(fs.Arg(0))
		y, errY := strconv.Atoi(fs.Arg(1))
		if errX != nil || errY != nil {
			fs.Usage()
			return 2
		}
		body["x"], body["y"] = x, y
	default:
		fs.Usage()
		return 2
	}
	b, _ := json.Marshal(body)
	return d.post("/api/input", b)
}

// runSequence is `loupe sequence`: the steps of POST /api/sequence, a JSON
// array of /api/input bodies, read from a file or stdin.
func runSequence(args []string) int {
	fs := flag.NewFlagSet("loupe sequence", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: loupe sequence -target ID [flags] [FILE]   (steps as a JSON array; stdin when no FILE)")
		fs.PrintDefaults()
	}
	d := newDriveFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var in io.Reader = os.Stdin
	switch fs.NArg() {
	case 0:
	case 1:
		f, err := os.Open(fs.Arg(0))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer f.Close()
		in = f
	default:
		fs.Usage()
		return 2
	}
	var steps []json.RawMessage
	if err := json.NewDecoder(in).Decode(&steps); err != nil {
		fmt.Fprintf(os.Stderr, "steps: want a JSON array of /api/input bodies: %v\n", err)
		return 2
	}
	b, _ := json.Marshal(map[string]any{"steps": steps})
	return d.post("/api/sequence", b)
}
