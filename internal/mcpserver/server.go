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
	s.AddReceivingMiddleware(defaultTargetMiddleware(c))
	return s
}

// instructions reach the model on connect. They explain the channel
// messages pushFlags sends, which arrive as <channel source="loupe" ...> tags.
const instructions = `loupe streams Android and iOS devices to a person watching them, and lets you read and drive those devices with these tools.

Every tool call is a round trip through you, so a task finishes as fast as its number of calls allows. To drive in few calls:
- Read a screen with ui (interactive: true), not screenshot. It names every node you can act on by text, desc or id. Take a screenshot only to judge how something looks.
- On a screen you have read, put every step you can predict into one sequence call, through to the next screen, and end it with a wait whose match is what that screen should show. A wait fails unless its node appears, so it is also the check. A tap with a match waits for its node up to waitMs, and a scroll with find scrolls until its node is on screen.
- A sequence's answer ends with the screen it stopped on, so plan the next sequence from that instead of calling ui. If a step failed, the answer names it and why.
- Asked to check that something works, prove it once on the shortest path, answer, and stop. Ask before widening the check.

The person can flag something that looks off on a device's screen. In a Claude Code session started with channels, each flag arrives as a <channel source="loupe" target="..." flag_id="..." state="..."> message; claimed_by, when present, names the agent holding that target, and held_by_you="true" means that claim is yours. Act on a flag when to_you or held_by_you is set, or claimed_by is absent; leave it to its holder otherwise. to_you="true" means the person sent it to this session by name, so it is yours even if another agent holds the target. To act: read it with the flag tool (note, boxed region, UI tree, screenshots), then look into it. state="watching" means the person is reproducing it and a second message comes with state="ended" and their inputs. state="interrupt" (kind="interrupt") means the person stopped you mid-task: stop driving that target now and wait for the state="ended" message, or state="released", which means they sent it to another session and you may go on. Until you then read that flag with the flag tool, input on the target fails with code "interrupted"; read it, look at the screen as it is now, and decide whether what you were doing still applies before you act again. An input error with code "interrupted" means the same thing without channels. Without channels, check the flags tool, or wait on it with wait_seconds.

When loupe itself gets in your way (a tool fails or answers wrongly, a tool or option you need is missing, an error does not say what to do, a doc is wrong), report it once with submit_feedback after you have worked around it or given up, with what you sent, what came back and the change you would make to loupe. Leave secrets and screen contents out of it: it may leave this Mac.`

// Run serves MCP on t until the client hangs up or ctx ends, then releases
// every claim this process took. The release gets a context of its own,
// since ctx is usually the one that just ended.
//
// When ctx ends, Run releases and returns without waiting for the SDK's Run,
// which waits for every tool call in flight: a flags call can wait a minute,
// and whoever sent the signal may not wait that long before it kills the
// process. The caller exits right after, which ends those calls.
func Run(ctx context.Context, c *Client, t mcp.Transport) error {
	ct := &channelTransport{Transport: t}
	pctx, cancel := context.WithCancel(ctx)
	if !c.noPush {
		go pushFlags(pctx, c, ct)
	}
	sctx, scancel := context.WithCancel(context.Background())
	defer scancel()
	served := make(chan error, 1)
	go func() { served <- NewServer(c).Run(sctx, ct) }()
	var err error
	select {
	case err = <-served:
	case <-ctx.Done():
		err = ctx.Err()
	}
	cancel()
	rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer rcancel()
	c.ReleaseAll(rctx)
	return err
}
