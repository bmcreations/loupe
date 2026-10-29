package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Claude Code's channels push a message into a running session without the
// model calling a tool (code.claude.com/docs/en/channels-reference.md). The
// server declares the capability and sends a notification; the session has
// to be started with --dangerously-load-development-channels server:<name>.
// The go-sdk has no call for a notification it does not know, so
// channelTransport keeps the connection the SDK serves on and writes to it.
const (
	channelCapability = "claude/channel"
	channelMethod     = "notifications/claude/channel"
)

// channelTransport wraps the transport Run serves on and remembers its
// connection, so notify can write to the same stream the SDK does.
// Connection.Write is safe for concurrent use.
type channelTransport struct {
	mcp.Transport

	mu   sync.Mutex
	conn mcp.Connection
}

func (t *channelTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	c, err := t.Transport.Connect(ctx)
	if err == nil {
		t.mu.Lock()
		t.conn = c
		t.mu.Unlock()
	}
	return c, err
}

var errNotConnected = errors.New("mcp: not connected")

// notify sends one channel message. content is the body of the <channel>
// tag the model sees; each meta entry becomes an attribute of it, and keys
// may hold only letters, digits and underscores.
func (t *channelTransport) notify(ctx context.Context, content string, meta map[string]string) error {
	t.mu.Lock()
	c := t.conn
	t.mu.Unlock()
	if c == nil {
		return errNotConnected
	}
	params, err := json.Marshal(struct {
		Content string            `json:"content"`
		Meta    map[string]string `json:"meta,omitempty"`
	}{content, meta})
	if err != nil {
		return err
	}
	// A request with no ID is a notification.
	return c.Write(ctx, &jsonrpc.Request{Method: channelMethod, Params: params})
}
