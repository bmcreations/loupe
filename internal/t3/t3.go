// Package t3 drives T3 Code's CLI: list a project's threads, send one a
// message, start a new one. A flag the person raises in Loupe can go to a T3
// thread this way, whatever provider that thread runs.
//
// It shells out rather than speaking T3's own protocol, which is not
// published. Every call goes through exec with the message on stdin, never
// through a shell.
package t3

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Thread is one T3 thread as `t3 thread list --json` reports it.
type Thread struct {
	ID        string    `json:"threadId"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	Provider  string    `json:"providerInstanceId"`
	Model     string    `json:"model"`
	Settled   bool      `json:"settled"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// threadID is the shape of the ids T3 hands out: a UUID, bare or under
// up to two word prefixes, as in mcp:<uuid> for a thread an MCP tool made
// and import:claudeAgent:<uuid> for an imported session.
var threadID = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9]*:){0,2}[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidThread reports whether id looks like a T3 thread id.
func ValidThread(id string) bool { return threadID.MatchString(id) }

// CLI runs one t3 binary.
type CLI struct {
	Path string
}

// Find locates the t3 CLI. The desktop app's own wrapper comes first: it sets
// T3CODE_HOME itself, which launchd does not, and a bare t3 without it reads
// a different data directory and knows none of the app's projects.
func Find() (CLI, error) {
	if p := os.Getenv("T3CODE_CLI_PATH"); p != "" {
		return CLI{Path: p}, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".t3-desktop", "bin", "t3")
		if fi, err := os.Stat(p); err == nil && fi.Mode()&0o111 != 0 {
			return CLI{Path: p}, nil
		}
	}
	p, err := exec.LookPath("t3")
	if err != nil {
		return CLI{}, errors.New("t3 CLI not found: install T3 Code, or set T3CODE_CLI_PATH")
	}
	return CLI{Path: p}, nil
}

func (c CLI) run(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.Path, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if msg == "" {
			return nil, fmt.Errorf("t3 %s: %w", args[0]+" "+args[1], err)
		}
		return nil, fmt.Errorf("t3 %s: %s", args[0]+" "+args[1], msg)
	}
	return out.Bytes(), nil
}

// Threads lists project's threads, newest first. project is a path inside
// the project, as T3 accepts it.
func (c CLI) Threads(ctx context.Context, project string) ([]Thread, error) {
	out, err := c.run(ctx, "", "thread", "list", "--json", "--project", project)
	if err != nil {
		return nil, err
	}
	var ts []Thread
	if err := json.Unmarshal(out, &ts); err != nil {
		return nil, fmt.Errorf("t3 thread list: %w", err)
	}
	return ts, nil
}

// Send gives thread msg. A busy thread takes it as its next turn; T3 does not
// interrupt one.
func (c CLI) Send(ctx context.Context, thread, msg string) error {
	if !ValidThread(thread) {
		return fmt.Errorf("not a T3 thread id: %q", thread)
	}
	_, err := c.run(ctx, msg, "thread", "send", thread, "-")
	return err
}

// Start opens a thread in project with msg as its first message and returns
// the new thread's id.
func (c CLI) Start(ctx context.Context, project, title, msg string) (string, error) {
	out, err := c.run(ctx, msg, "thread", "start", "--json", "--title", title, project, "-")
	if err != nil {
		return "", err
	}
	return startedID(out)
}

// startedID reads the new thread's id from thread start's output: a JSON
// object carrying threadId, or failing that the bare id.
func startedID(out []byte) (string, error) {
	var v struct {
		ThreadID string `json:"threadId"`
		ID       string `json:"id"`
	}
	if json.Unmarshal(out, &v) == nil {
		if v.ThreadID != "" {
			return v.ThreadID, nil
		}
		if v.ID != "" {
			return v.ID, nil
		}
	}
	if s := strings.TrimSpace(string(out)); ValidThread(s) {
		return s, nil
	}
	return "", fmt.Errorf("t3 thread start: no thread id in %q", truncate(string(out), 200))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
