package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bmcreations/loupe/internal/mcpserver"
)

// cliCommand is one `loupe <name>` command. Each is an MCP tool from
// internal/mcpserver run in process, so the CLI and `loupe mcp` share one
// table of tools, schemas and claim handling, and TestEveryToolNamesARoute
// covers both. args are the tool's fields taken by position, in order; a
// trailing "..." takes the rest, and a trailing "?" marks fields that may all
// be left out together (a tap by --text has no point). Any other field is a
// --name value flag. A field that is an array of objects takes JSON: inline,
// - for stdin, or @path.
type cliCommand struct {
	name, tool string
	args       []string
}

var cliCommands = []cliCommand{
	{"targets", "targets", nil},
	{"ui", "ui", []string{"target"}},
	{"screenshot", "screenshot", []string{"target"}},
	{"point", "point", []string{"target", "x", "y"}},
	{"tap", "tap", []string{"target", "x?", "y?"}},
	{"swipe", "swipe", []string{"target", "x", "y", "x2", "y2"}},
	{"type", "type_text", []string{"target", "text"}},
	{"key", "key", []string{"target", "keycode"}},
	{"button", "button", []string{"target", "button"}},
	{"launch", "launch", []string{"target", "package"}},
	{"open", "open_url", []string{"target", "url"}},
	{"sequence", "sequence", []string{"target", "steps"}},
	{"logcat", "logcat", []string{"target"}},
	{"crashes", "crashes", []string{"target"}},
	{"crash", "crash", []string{"target", "id"}},
	{"install", "install", []string{"target", "paths..."}},
	{"camera", "camera", []string{"target", "path"}},
	{"drop", "drop", []string{"target", "x", "y", "path"}},
	{"boot", "boot", []string{"target"}},
	{"boot-status", "boot_status", []string{"target"}},
	{"release", "release", []string{"target"}},
	{"flags", "flags", nil},
	{"flag", "flag", []string{"id"}},
}

func findCLICommand(name string) (cliCommand, bool) {
	for _, c := range cliCommands {
		if c.name == name {
			return c, true
		}
	}
	return cliCommand{}, false
}

// cliGlobals are the flags every command takes besides its tool's fields.
// None of them is a field name of any tool; TestCLIGlobalsAreNotFields
// checks it.
type cliGlobals struct {
	server, token, agent, session, out string
	help                               bool
}

func defaultCLIGlobals() cliGlobals {
	g := cliGlobals{
		server:  os.Getenv("LOUPE_URL"),
		agent:   os.Getenv("LOUPE_AGENT"),
		session: os.Getenv("LOUPE_SESSION"),
	}
	if g.server == "" {
		g.server = "https://localhost:18456"
	}
	if g.agent == "" {
		g.agent = "cli"
	}
	return g
}

// runCLI is `loupe <command> ...`. It prints the server's answer to stdout,
// a screenshot's PNG to -o or a pipe, and an error to stderr with exit 1.
// It never releases a claim it took: the claim is a lease the next command
// renews, and `loupe release` or ten idle minutes end it.
func runCLI(cmd cliCommand, argv []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	g := defaultCLIGlobals()
	tool, cs, closeFn, err := cliSession(ctx, cmd, &g, argv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loupe %s: %v\n", cmd.name, err)
		return 2
	}
	defer closeFn()

	in, err := parseCLIArgs(cmd, tool, argv, &g)
	if g.help {
		printCommandHelp(os.Stdout, cmd, tool)
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "loupe %s: %v\n%s", cmd.name, err, usageLine(cmd, tool))
		return 2
	}
	if cmd.tool == "screenshot" && g.out == "" && isTerminal(os.Stdout) {
		fmt.Fprintf(os.Stderr, "loupe screenshot: pass -o <file.png>, or pipe stdout\n")
		return 2
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: cmd.tool, Arguments: in})
	if err != nil {
		fmt.Fprintf(os.Stderr, "loupe %s: %v\n", cmd.name, err)
		return 1
	}
	return writeResult(res, g.out)
}

// cliSession connects an in-process MCP client to the tools. The globals
// are read first, since the client needs -server and -token before it can
// list the tools whose schemas parse the rest.
func cliSession(ctx context.Context, cmd cliCommand, g *cliGlobals, argv []string) (*mcp.Tool, *mcp.ClientSession, func(), error) {
	preparseGlobals(argv, g)
	tok, err := mcpToken(g.token)
	if err != nil && !g.help {
		return nil, nil, nil, err
	}
	c, err := mcpserver.NewClient(g.server, tok, g.agent)
	if err != nil {
		return nil, nil, nil, err
	}
	c.SetSession(g.session)

	st, ct := mcp.NewInMemoryTransports()
	ss, err := mcpserver.NewServer(c).Connect(ctx, st, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "loupe-cli", Version: mcpserver.Version}, nil).Connect(ctx, ct, nil)
	if err != nil {
		ss.Close()
		return nil, nil, nil, err
	}
	closeFn := func() { cs.Close(); ss.Close() }
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		closeFn()
		return nil, nil, nil, err
	}
	for _, t := range tools.Tools {
		if t.Name == cmd.tool {
			return t, cs, closeFn, nil
		}
	}
	closeFn()
	return nil, nil, nil, fmt.Errorf("no tool %q", cmd.tool)
}

// preparseGlobals picks the global flags out of argv so the client can be
// built; parseCLIArgs reads them again and rejects anything else unknown.
func preparseGlobals(argv []string, g *cliGlobals) {
	for i := 0; i < len(argv); i++ {
		name, val, hasVal := splitFlag(argv[i])
		if name == "" {
			continue
		}
		if p := g.ptr(name); p != nil {
			if !hasVal && i+1 < len(argv) {
				i++
				val = argv[i]
			}
			*p = val
		} else if name == "h" || name == "help" {
			g.help = true
		}
	}
}

func (g *cliGlobals) ptr(name string) *string {
	switch name {
	case "server":
		return &g.server
	case "token":
		return &g.token
	case "agent":
		return &g.agent
	case "session":
		return &g.session
	case "o", "out":
		return &g.out
	}
	return nil
}

// splitFlag returns a flag's name, without its dashes, and an inline
// --name=value. A bare "-" or "--" and anything not starting with a dash is
// no flag; neither is a negative number, which a coordinate can be.
func splitFlag(s string) (name, val string, hasVal bool) {
	if !strings.HasPrefix(s, "-") || s == "-" || s == "--" {
		return "", "", false
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return "", "", false
	}
	s = strings.TrimLeft(s, "-")
	if i := strings.IndexByte(s, '='); i >= 0 {
		return s[:i], s[i+1:], true
	}
	return s, "", false
}

// parseCLIArgs builds the tool's arguments from positionals and --field
// flags, converting each by the type the tool's schema gives it.
func parseCLIArgs(cmd cliCommand, tool *mcp.Tool, argv []string, g *cliGlobals) (map[string]any, error) {
	props := schemaProps(tool)
	in := map[string]any{}
	var pos []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			pos = append(pos, argv[i+1:]...)
			break
		}
		name, val, hasVal := splitFlag(a)
		if name == "" {
			pos = append(pos, a)
			continue
		}
		if name == "h" || name == "help" {
			g.help = true
			continue
		}
		if g.ptr(name) != nil {
			if !hasVal {
				i++
			}
			continue
		}
		typ, ok := props[name]
		if !ok {
			return nil, fmt.Errorf("unknown flag -%s", name)
		}
		if typ == "boolean" && !hasVal {
			val, hasVal = "true", true
		}
		if !hasVal {
			if i+1 >= len(argv) {
				return nil, fmt.Errorf("-%s needs a value", name)
			}
			i++
			val = argv[i]
		}
		if err := setField(in, name, typ, val); err != nil {
			return nil, err
		}
	}
	// Optional fields are left out only together: one given means all are.
	required := 0
	for _, field := range cmd.args {
		if !strings.HasSuffix(field, "?") {
			required++
		}
	}
	skipOptional := len(pos) <= required
	objects := objectArrays(tool)
	for i, field := range cmd.args {
		field, optional := strings.CutSuffix(field, "?")
		if optional && skipOptional {
			continue
		}
		if objects[field] {
			if i >= len(pos) {
				return nil, fmt.Errorf("missing <%s>", field)
			}
			v, err := readJSONArg(field, pos[i])
			if err != nil {
				return nil, err
			}
			in[field] = v
			continue
		}
		if rest, ok := strings.CutSuffix(field, "..."); ok {
			for _, v := range pos[min(i, len(pos)):] {
				if err := setField(in, rest, props[rest], v); err != nil {
					return nil, err
				}
			}
			pos = nil
			break
		}
		if i >= len(pos) {
			if _, set := in[field]; set {
				continue
			}
			return nil, fmt.Errorf("missing <%s>", field)
		}
		if err := setField(in, field, props[field], pos[i]); err != nil {
			return nil, err
		}
	}
	if extra := len(pos) - len(cmd.args); extra > 0 && !variadic(cmd) {
		return nil, fmt.Errorf("unexpected argument %q", pos[len(cmd.args)])
	}
	return in, nil
}

// objectArrays names a tool's fields that are arrays of objects, which a
// shell word can only carry as JSON.
func objectArrays(tool *mcp.Tool) map[string]bool {
	out := map[string]bool{}
	for name, p := range schemaProperties(tool) {
		if items, ok := p["items"].(map[string]any); ok && schemaType(p) == "array" && schemaType(items) == "object" {
			out[name] = true
		}
	}
	return out
}

// readJSONArg decodes a JSON argument given inline, as - for stdin, or as
// @path for a file.
func readJSONArg(name, val string) (any, error) {
	b := []byte(val)
	switch {
	case val == "-":
		var err error
		if b, err = io.ReadAll(os.Stdin); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	case strings.HasPrefix(val, "@"):
		var err error
		if b, err = os.ReadFile(val[1:]); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("%s: want JSON, inline, - for stdin or @path: %v", name, err)
	}
	return v, nil
}

func variadic(cmd cliCommand) bool {
	return len(cmd.args) > 0 && strings.HasSuffix(cmd.args[len(cmd.args)-1], "...")
}

func setField(in map[string]any, name, typ, val string) error {
	switch typ {
	case "integer":
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return fmt.Errorf("%s: %q is not a whole number", name, val)
		}
		in[name] = n
	case "number":
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return fmt.Errorf("%s: %q is not a number", name, val)
		}
		in[name] = f
	case "boolean":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("%s: %q is not true or false", name, val)
		}
		in[name] = b
	case "array":
		list, _ := in[name].([]any)
		in[name] = append(list, val)
	default:
		in[name] = val
	}
	return nil
}

// schemaProps maps each of a tool's fields to its JSON Schema type. A
// nullable field's type is a list, ["null", "array"], and reads as the one
// that is not null.
func schemaProps(tool *mcp.Tool) map[string]string {
	out := map[string]string{}
	for name, p := range schemaProperties(tool) {
		out[name] = schemaType(p)
	}
	return out
}

func schemaProperties(tool *mcp.Tool) map[string]map[string]any {
	b, _ := json.Marshal(tool.InputSchema)
	var s struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	_ = json.Unmarshal(b, &s)
	return s.Properties
}

func schemaType(p map[string]any) string {
	switch t := p["type"].(type) {
	case string:
		return t
	case []any:
		for _, v := range t {
			if s, ok := v.(string); ok && s != "null" {
				return s
			}
		}
	}
	return "string"
}

// writeResult prints a tool's answer. Text goes to stdout as the server sent
// it, an image to out or stdout, and a tool error to stderr with exit 1.
func writeResult(res *mcp.CallToolResult, out string) int {
	w := io.Writer(os.Stdout)
	if res.IsError {
		w = os.Stderr
	}
	for _, c := range res.Content {
		switch c := c.(type) {
		case *mcp.TextContent:
			fmt.Fprintln(w, strings.TrimRight(c.Text, "\n"))
		case *mcp.ImageContent:
			if out != "" {
				if err := os.WriteFile(out, c.Data, 0o644); err != nil {
					fmt.Fprintf(os.Stderr, "loupe: %v\n", err)
					return 1
				}
				fmt.Fprintln(os.Stderr, out)
				continue
			}
			if _, err := os.Stdout.Write(c.Data); err != nil {
				return 1
			}
		}
	}
	if res.IsError {
		return 1
	}
	return 0
}

func usageLine(cmd cliCommand, tool *mcp.Tool) string {
	var b strings.Builder
	b.WriteString("usage: loupe " + cmd.name)
	for _, a := range cmd.args {
		if name, ok := strings.CutSuffix(a, "?"); ok {
			b.WriteString(" [<" + name + ">]")
			continue
		}
		b.WriteString(" <" + a + ">")
	}
	positional := map[string]bool{}
	for _, a := range cmd.args {
		positional[strings.TrimSuffix(strings.TrimSuffix(a, "..."), "?")] = true
	}
	var opts []string
	for name := range schemaProps(tool) {
		if !positional[name] {
			opts = append(opts, name)
		}
	}
	if len(opts) > 0 {
		b.WriteString(" [flags]")
	}
	b.WriteString("\n")
	return b.String()
}

func printCommandHelp(w io.Writer, cmd cliCommand, tool *mcp.Tool) {
	fmt.Fprint(w, usageLine(cmd, tool))
	if tool == nil {
		return
	}
	fmt.Fprintf(w, "\n%s\n", tool.Description)
	props := schemaProperties(tool)
	names := make([]string, 0, len(props))
	for n := range props {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) > 0 {
		fmt.Fprintln(w, "\nfields (by position as above, or -name value):")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, n := range names {
			desc, _ := props[n]["description"].(string)
			fmt.Fprintf(tw, "  -%s\t%s\t%s\n", n, schemaType(props[n]), desc)
		}
		tw.Flush()
	}
	fmt.Fprint(w, cliGlobalsHelp)
}

const cliGlobalsHelp = `
global flags:
  -server url    the loupe server (LOUPE_URL, default https://localhost:18456)
  -token token   shared token (LOUPE_TOKEN, then the server's token file)
  -agent name    X-Loupe-Agent, shown on the viewer's banner (LOUPE_AGENT, default cli)
  -session id    X-Loupe-Session, to keep two agents' claims apart (LOUPE_SESSION)
  -o file        screenshot: write the PNG here
`

// printCLIHelp is `loupe help`: the commands, then the server's own flags
// are at `loupe -h`.
func printCLIHelp(w io.Writer) {
	fmt.Fprint(w, `loupe streams Android devices, emulators and iOS simulators to a browser,
and lets agents and scripts drive them.

usage:
  loupe [flags]              run the server (loupe -h lists its flags)
  loupe <command> [args]     drive a running server
  loupe mcp                  the same commands as an MCP server on stdio
  loupe pair | sessions      pair a device, list and revoke sessions

commands:
`)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range cliCommands {
		var args []string
		for _, a := range c.args {
			args = append(args, "<"+a+">")
		}
		fmt.Fprintf(tw, "  %s\t%s\n", c.name, strings.Join(args, " "))
	}
	tw.Flush()
	fmt.Fprint(w, "\n`loupe <command> -h` describes a command and its flags.\n")
	fmt.Fprint(w, cliGlobalsHelp)
}
