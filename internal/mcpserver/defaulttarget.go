package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// noDefaultTarget names the tools a call's missing target is left alone for:
// tools that take none, take one only as a filter, or name an AVD that is not
// running yet, which no default could guess.
var noDefaultTarget = map[string]bool{
	"targets":         true,
	"flags":           true,
	"flag":            true,
	"submit_feedback": true,
	"boot":            true,
	"boot_status":     true,
}

// SetDefaultTarget makes every device tool called without a target drive id,
// as `loupe mcp -target` asks. Empty leaves the lone-device rule alone.
func (c *Client) SetDefaultTarget(id string) { c.defaultTarget = id }

// defaultTargetMiddleware fills a device tool call's missing target, so an
// agent with one device in front of it spends no turn on the targets tool.
// The -target flag wins; without it, the one running device is used. With
// several running and no flag, the call fails saying so, rather than guess.
func defaultTargetMiddleware(c *Client) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			r, ok := req.(*mcp.CallToolRequest)
			if method != "tools/call" || !ok || r.Params == nil || noDefaultTarget[r.Params.Name] {
				return next(ctx, method, req)
			}
			args := map[string]any{}
			if len(r.Params.Arguments) > 0 {
				if err := json.Unmarshal(r.Params.Arguments, &args); err != nil {
					return next(ctx, method, req)
				}
			}
			if args == nil { // "arguments": null
				args = map[string]any{}
			}
			if t, _ := args["target"].(string); t != "" {
				return next(ctx, method, req)
			}
			id, err := c.pickTarget(ctx)
			if err != nil {
				return errResult(err), nil
			}
			args["target"] = id
			b, err := json.Marshal(args)
			if err != nil {
				return next(ctx, method, req)
			}
			r.Params.Arguments = b
			return next(ctx, method, req)
		}
	}
}

// pickTarget is the target a call without one drives: the -target flag's, or
// the one device that is up and available.
func (c *Client) pickTarget(ctx context.Context) (string, error) {
	if c.defaultTarget != "" {
		return c.defaultTarget, nil
	}
	b, err := c.getJSON(ctx, "/api/targets", nil)
	if err != nil {
		return "", fmt.Errorf("no target given, and listing targets to pick one failed: %w", err)
	}
	var rows []struct {
		ID        string `json:"id"`
		State     string `json:"state"`
		Available bool   `json:"available"`
	}
	if err := json.Unmarshal(b, &rows); err != nil {
		return "", fmt.Errorf("no target given, and the targets list did not parse: %w", err)
	}
	var up []string
	for _, t := range rows {
		if t.State == "device" && t.Available {
			up = append(up, t.ID)
		}
	}
	switch len(up) {
	case 1:
		return up[0], nil
	case 0:
		return "", fmt.Errorf("no target given and no device is running; boot one (see targets) or pass target")
	default:
		return "", fmt.Errorf("no target given and %d devices are running (%s); pass target, or start loupe mcp with -target", len(up), strings.Join(up, ", "))
	}
}
