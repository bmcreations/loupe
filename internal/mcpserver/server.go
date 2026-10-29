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
	s := mcp.NewServer(&mcp.Implementation{Name: "loupe", Version: Version}, &mcp.ServerOptions{
		Instructions: instructions,
		// Tools stays nil so the SDK still infers it from addTools.
		Capabilities: &mcp.ServerCapabilities{
			Logging:      &mcp.LoggingCapabilities{},
			Experimental: map[string]any{channelCapability: map[string]any{}},
		},
		// Claude Code registers no channel on a stdio server that negotiated
		// 2026-07-28, so loupe stops at the handshake-era versions.
		SupportedProtocolVersions: []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"},
	})
	addTools(s, c)
	addFlagTools(s, c)
	return s
}

// instructions reach the model on connect. They explain the channel
// messages pushFlags sends, which arrive as <channel source="loupe" ...> tags.
const instructions = `loupe streams Android and iOS devices to a person watching them, and lets you read and drive those devices with these tools.

The person can flag something that looks off on a device's screen. In a Claude Code session started with channels, each flag arrives as a <channel source="loupe" target="..." flag_id="..." state="..."> message; claimed_by, when present, names the agent holding that target, and held_by_you="true" means that claim is yours. Act on a flag when to_you or held_by_you is set, or claimed_by is absent; leave it to its holder otherwise. to_you="true" means the person sent it to this session by name, so it is yours even if another agent holds the target. To act: read it with the flag tool (note, boxed region, UI tree, screenshots), then look into it. state="watching" means the person is reproducing it and a second message comes with state="ended" and their inputs. state="interrupt" (kind="interrupt") means the person stopped you mid-task: stop driving that target now and wait for the state="ended" message, or state="released", which means they sent it to another session and you may go on. Until you then read that flag with the flag tool, input on the target fails with code "interrupted"; read it, look at the screen as it is now, and decide whether what you were doing still applies before you act again. An input error with code "interrupted" means the same thing without channels. Without channels, check the flags tool, or wait on it with wait_seconds.`

// Run serves MCP on t until the client hangs up or ctx ends, then releases
// every claim this process took. The release gets a context of its own,
// since ctx is usually the one that just ended.
func Run(ctx context.Context, c *Client, t mcp.Transport) error {
	ct := &channelTransport{Transport: t}
	pctx, cancel := context.WithCancel(ctx)
	if !c.noPush {
		go pushFlags(pctx, c, ct)
	}
	err := NewServer(c).Run(ctx, ct)
	cancel()
	rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer rcancel()
	c.ReleaseAll(rctx)
	return err
}
