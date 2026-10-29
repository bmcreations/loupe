// Package dest reads the places a flag can be sent beyond loupe mcp sessions
// and T3 threads: a program on this Mac, such as `claude -p` or `codex exec`,
// or a webhook. They live in a file of their own that the server reads and
// never writes. The settings file will not do: any holder of the shared token
// can rewrite it over PUT /api/config, and a program or URL there would let
// them run code on this Mac or post its screenshots anywhere.
package dest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// File is destinations.json.
type File struct {
	Commands []Command `json:"commands,omitempty"`
	Webhooks []Webhook `json:"webhooks,omitempty"`
}

// Command runs a program with the flag's message on stdin.
type Command struct {
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
	// Program is an absolute path: the server runs under launchd, whose PATH
	// finds little.
	Program string   `json:"program"`
	Args    []string `json:"args,omitempty"`
	// Dir is the working directory, absolute; empty is the flag's own.
	Dir string `json:"dir,omitempty"`
	// Env adds to the server's environment, which reaches the program
	// without any LOUPE_ variable, the token among them.
	Env        map[string]string `json:"env,omitempty"`
	TimeoutSec int               `json:"timeoutSec,omitempty"`
	// Terminal opens the run in a window of that app, "Terminal" say, so
	// the person can watch it and answer it, rather than running it out of
	// sight. The program then has the window's input, not the message: pass
	// it with {text} or {message}. Loupe does not see it finish.
	Terminal string `json:"terminal,omitempty"`
}

// Webhook is POSTed the flag as JSON.
type Webhook struct {
	Name    string            `json:"name"`
	Label   string            `json:"label,omitempty"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// DefaultTimeout bounds a command without timeoutSec; a one-shot agent run
// can take minutes.
const DefaultTimeout = 10 * time.Minute

// maxTimeout caps timeoutSec.
const maxTimeout = time.Hour

// WebhookTimeout bounds one POST.
const WebhookTimeout = 30 * time.Second

// tailBytes is how much of a command's output, or a webhook's reply, is kept
// to show why it failed.
const tailBytes = 2048

var name = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// opener is what opens a terminal run; tests point it at a fake.
var opener = "/usr/bin/open"

var appName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 .-]{0,39}$`)

// placeholder matches {word} in an argument.
var placeholder = regexp.MustCompile(`\{([a-z]+)\}`)

// Vars are what an argument's placeholders become. The note is never one on
// its own: an argument made from it could start with a dash and pass for an
// option. {text} is the whole message, which starts with "> " or "The
// person", so it cannot; {message} is the same text as a file.
var Vars = []string{"id", "target", "dir", "message", "text", "start", "end", "ui", "events"}

// Load reads the file at path. A missing file is no destinations. One the
// server's user does not own, or that others may write, is refused: whoever
// can edit it can run programs as this user.
func Load(path string) (File, error) {
	var f File
	fh, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	defer fh.Close()
	fi, err := fh.Stat()
	if err != nil {
		return f, err
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return f, fmt.Errorf("%s: group or others may write it; chmod 600 it", path)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return f, fmt.Errorf("%s: owned by uid %d, not this user", path, st.Uid)
	}
	dec := json.NewDecoder(io.LimitReader(fh, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return File{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := f.Validate(); err != nil {
		return File{}, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// Validate checks every entry and that names are unique within each kind.
func (f File) Validate() error {
	seen := map[string]bool{}
	for _, c := range f.Commands {
		if !name.MatchString(c.Name) {
			return fmt.Errorf("command name %q: want lowercase letters, digits and dashes", c.Name)
		}
		if seen["c:"+c.Name] {
			return fmt.Errorf("command %q: listed twice", c.Name)
		}
		seen["c:"+c.Name] = true
		if !filepath.IsAbs(c.Program) {
			return fmt.Errorf("command %q: program %q is not an absolute path", c.Name, c.Program)
		}
		if c.Dir != "" && !filepath.IsAbs(c.Dir) {
			return fmt.Errorf("command %q: dir %q is not an absolute path", c.Name, c.Dir)
		}
		if c.TimeoutSec < 0 || time.Duration(c.TimeoutSec)*time.Second > maxTimeout {
			return fmt.Errorf("command %q: timeoutSec %d: want 0 to %d", c.Name, c.TimeoutSec, int(maxTimeout/time.Second))
		}
		if c.Terminal != "" && !appName.MatchString(c.Terminal) {
			return fmt.Errorf("command %q: terminal %q is not an app name", c.Name, c.Terminal)
		}
		for k := range c.Env {
			if strings.HasPrefix(k, "LOUPE_") {
				return fmt.Errorf("command %q: env %s: LOUPE_ variables are not passed on", c.Name, k)
			}
		}
		for _, a := range c.Args {
			for _, m := range placeholder.FindAllStringSubmatch(a, -1) {
				if !known(m[1]) {
					return fmt.Errorf("command %q: unknown placeholder {%s}; known: %s", c.Name, m[1], strings.Join(Vars, ", "))
				}
			}
		}
	}
	for _, w := range f.Webhooks {
		if !name.MatchString(w.Name) {
			return fmt.Errorf("webhook name %q: want lowercase letters, digits and dashes", w.Name)
		}
		if seen["w:"+w.Name] {
			return fmt.Errorf("webhook %q: listed twice", w.Name)
		}
		seen["w:"+w.Name] = true
		if err := checkURL(w.URL); err != nil {
			return fmt.Errorf("webhook %q: %w", w.Name, err)
		}
	}
	return nil
}

func known(v string) bool {
	for _, k := range Vars {
		if k == v {
			return true
		}
	}
	return false
}

// checkURL allows https anywhere and plain http only to this Mac: a webhook's
// headers usually carry a secret, and the body carries screenshots.
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("url %q: not an absolute URL", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		h := u.Hostname()
		if h == "localhost" {
			return nil
		}
		if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("url %q: plain http only to localhost; use https", raw)
	}
	return fmt.Errorf("url %q: want https", raw)
}

// Command finds a command by name.
func (f File) Command(n string) (Command, bool) {
	for _, c := range f.Commands {
		if c.Name == n {
			return c, true
		}
	}
	return Command{}, false
}

// Webhook finds a webhook by name.
func (f File) Webhook(n string) (Webhook, bool) {
	for _, w := range f.Webhooks {
		if w.Name == n {
			return w, true
		}
	}
	return Webhook{}, false
}

// Title is what a menu shows for an entry: its label, else its name.
func Title(label, n string) string {
	if label != "" {
		return label
	}
	return n
}

// Run runs c with stdin as its input and vars filling its arguments' place
// holders, in dir unless c names its own. It returns the tail of what the
// program wrote, to stdout and stderr together.
func (c Command) Run(ctx context.Context, vars map[string]string, dir, stdin string) (string, error) {
	timeout := DefaultTimeout
	if c.TimeoutSec > 0 {
		timeout = time.Duration(c.TimeoutSec) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := make([]string, len(c.Args))
	for i, a := range c.Args {
		args[i] = placeholder.ReplaceAllStringFunc(a, func(m string) string {
			return vars[m[1:len(m)-1]]
		})
	}
	if c.Terminal != "" {
		return c.openInTerminal(ctx, args, dir)
	}
	cmd := exec.CommandContext(ctx, c.Program, args...)
	cmd.Dir = dir
	if c.Dir != "" {
		cmd.Dir = c.Dir
	}
	cmd.Env = env(c.Env)
	cmd.Stdin = strings.NewReader(stdin)
	var out tail
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("still running after %s; stopped", timeout)
	}
	return out.String(), err
}

// openInTerminal writes the run as a .command script in dir and opens it in
// c.Terminal, which runs it in a new window. It returns once the window is
// asked for; the run is the person's from there.
func (c Command) openInTerminal(ctx context.Context, args []string, dir string) (string, error) {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	wd := dir
	if c.Dir != "" {
		wd = c.Dir
	}
	fmt.Fprintf(&b, "cd %s || exit 1\n", shellQuote(wd))
	for k, v := range c.Env {
		fmt.Fprintf(&b, "export %s=%s\n", shellQuote(k), shellQuote(v))
	}
	b.WriteString(shellQuote(c.Program))
	for _, a := range args {
		b.WriteString(" " + shellQuote(a))
	}
	b.WriteString("\n")
	script := filepath.Join(dir, "run.command")
	if err := os.WriteFile(script, []byte(b.String()), 0o700); err != nil {
		return "", err
	}
	out, err := exec.CommandContext(ctx, opener, "-a", c.Terminal, script).CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("open in %s: %w", c.Terminal, err)
	}
	return "Opened in " + c.Terminal, nil
}

// shellQuote quotes s for sh: single quotes, each one inside closed, escaped
// and reopened.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// env is the server's environment without LOUPE_ variables, plus extra.
func env(extra map[string]string) []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "LOUPE_") {
			continue
		}
		if _, ok := extra[k]; ok {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

// tail keeps the last tailBytes written to it.
type tail struct{ b []byte }

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > tailBytes {
		t.b = append(t.b[:0], t.b[len(t.b)-tailBytes:]...)
	}
	return len(p), nil
}

func (t *tail) String() string { return strings.TrimSpace(string(t.b)) }

// Post sends body to w as JSON. Redirects are not followed: one could carry
// the headers somewhere the file never named.
func (w Webhook) Post(ctx context.Context, body []byte) (int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, WebhookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "loupe")
	for k, v := range w.Headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	var out tail
	io.Copy(&out, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, out.String(), fmt.Errorf("%s", resp.Status)
	}
	return resp.StatusCode, out.String(), nil
}
