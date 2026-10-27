package mcpserver

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Version is reported to the MCP client in the initialize handshake.
const Version = "0.1.0"

// NewServer returns an MCP server whose tools call c.
func NewServer(c *Client) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "loupe", Version: Version}, nil)
	addTools(s, c)
	return s
}

// Run serves MCP on t until the client hangs up or ctx ends, then releases
// every claim this process took. The release gets a context of its own,
// since ctx is usually the one that just ended.
func Run(ctx context.Context, c *Client, t mcp.Transport) error {
	err := NewServer(c).Run(ctx, t)
	rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c.ReleaseAll(rctx)
	return err
}
