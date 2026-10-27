package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bmcreations/loupe/internal/mcpserver"
	scrcpyserver "github.com/bmcreations/loupe/internal/scrcpy/server"
)

// runMCP is `loupe mcp`: an MCP server on stdin and stdout that drives a
// running loupe server through its HTTP API. Stdout is the protocol, so
// nothing else may print there; everything else goes to stderr.
func runMCP(args []string) int {
	fs := flag.NewFlagSet("loupe mcp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		addr  = fs.String("url", "https://localhost:18456", "the loupe server to drive")
		token = fs.String("token", "", "shared token (default: LOUPE_TOKEN, then the server's token file)")
		agent = fs.String("agent", "claude", "name sent as X-Loupe-Agent, shown on the viewer's banner and in claims")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	logger := log.New(os.Stderr, "loupe mcp: ", 0)

	tok, err := mcpToken(*token)
	if err != nil {
		logger.Print(err)
		return 1
	}
	c, err := mcpserver.NewClient(*addr, tok, *agent)
	if err != nil {
		logger.Print(err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := mcpserver.Run(ctx, c, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		logger.Print(err)
		return 1
	}
	return 0
}

// mcpToken reads the token the way the server does, except that it never
// makes one: a token this process invented would match no server.
func mcpToken(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if env := os.Getenv("LOUPE_TOKEN"); env != "" {
		return env, nil
	}
	path := filepath.Join(scrcpyserver.DefaultDir(), tokenFileName)
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("no token: pass -token, set LOUPE_TOKEN, or run the server once to write %s", path)
	}
	return strings.TrimSpace(string(b)), nil
}
