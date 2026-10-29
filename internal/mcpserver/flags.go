package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bmcreations/loupe/internal/httpapi"
)

// flagRetry is how long pushFlags waits after the server fails to answer.
var flagRetry = 2 * time.Second

// pushFlags long-polls the server for flags and sends each new one, and each
// watch that ends, to the client as a channel message. Flags already on the
// server when it starts are not sent: they were raised for whoever was
// connected then. It returns when ctx ends.
func pushFlags(ctx context.Context, c *Client, ct *channelTransport) {
	var rev uint64
	started := false
	watching := map[uint64]bool{}
	for ctx.Err() == nil {
		q := url.Values{}
		if started {
			q.Set("after", strconv.FormatUint(rev, 10))
			q.Set("waitMs", "25000")
		}
		polled := time.Now()
		b, err := c.getJSON(ctx, "/api/flags", q)
		var resp httpapi.FlagsResponse
		if err == nil {
			err = json.Unmarshal(b, &resp)
		}
		if err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(flagRetry):
			}
			continue
		}
		if started && len(resp.Flags) == 0 && time.Since(polled) < time.Second {
			// A server that answers a long-poll at once with nothing, an
			// older one or a proxy, must not turn this into a busy loop.
			select {
			case <-ctx.Done():
			case <-time.After(flagRetry):
			}
		}
		if !started {
			for _, f := range resp.Flags {
				watching[f.ID] = f.Watching
			}
			rev, started = resp.Rev, true
			continue
		}
		for _, f := range resp.Flags {
			was, seen := watching[f.ID]
			watching[f.ID] = f.Watching
			if f.To.IsExternal() && f.Kind != httpapi.FlagInterrupt {
				// Sent to a T3 thread, program or webhook, which the server
				// delivers itself.
				continue
			}
			if f.To != nil && !f.To.IsExternal() && !c.is(*f.To) {
				// Sent to another session. An interrupt may have been
				// pushed here while the person was still picking: say it
				// no longer stops this session.
				if seen && was && !f.Watching && f.Kind == httpapi.FlagInterrupt {
					m := flagMeta(f, c.holds(f.Target))
					delete(m, "to_you")
					m["state"] = "released"
					ct.notify(ctx, flagReleased(f), m)
				}
				continue
			}
			switch {
			case !seen:
				ct.notify(ctx, flagMessage(f, false), flagMeta(f, c.holds(f.Target)))
			case was && !f.Watching:
				ct.notify(ctx, flagMessage(f, true), flagMeta(f, c.holds(f.Target)))
			}
		}
		if resp.Rev > rev {
			rev = resp.Rev
		}
	}
}

// flagMessage is the text the model reads for f: raised, or its watch ended.
func flagMessage(f httpapi.Flag, ended bool) string {
	var b strings.Builder
	if f.Kind == httpapi.FlagInterrupt {
		if ended {
			fmt.Fprintf(&b, "The person finished interrupting you on %s (flag %d).", f.Target, f.ID)
			if f.Note != "" {
				fmt.Fprintf(&b, " Their note: %q.", f.Note)
			}
			if r := f.Region; r != nil {
				fmt.Fprintf(&b, " They boxed the area at %d,%d, %dx%d device pixels.", r.X, r.Y, r.Width, r.Height)
			}
			if f.To.IsExternal() {
				fmt.Fprintf(&b, " They sent it to %s to deal with, not to you.", f.To.External())
			}
			fmt.Fprintf(&b, " Read it with the flag tool, id %d, before acting on %s again: input there is refused until you do. Compare its end screen with what you expected, and decide whether your plan still applies.", f.ID, f.Target)
		} else {
			fmt.Fprintf(&b, "The person interrupted you on %s (flag %d) and holds the device while they write what is wrong. Stop driving it; another message comes when they finish.", f.Target, f.ID)
		}
		return b.String()
	}
	if ended {
		fmt.Fprintf(&b, "The person stopped reproducing flag %d on %s: %d inputs recorded", f.ID, f.Target, len(f.Events))
		if f.EventsTruncated {
			b.WriteString(" (the earliest were lost)")
		}
		b.WriteString(".")
	} else {
		fmt.Fprintf(&b, "The person flagged something off on %s's screen (flag %d).", f.Target, f.ID)
		if f.Note != "" {
			fmt.Fprintf(&b, " Their note: %q.", f.Note)
		}
		if r := f.Region; r != nil {
			fmt.Fprintf(&b, " They boxed the area at %d,%d, %dx%d device pixels.", r.X, r.Y, r.Width, r.Height)
		}
		if f.Watching {
			b.WriteString(" They are reproducing it now; another message comes when they stop.")
		}
	}
	fmt.Fprintf(&b, " Read it with the flag tool, id %d.", f.ID)
	return b.String()
}

// flagReleased is the text for an interrupt this session was told to stop
// for, once the person sent it to another session.
func flagReleased(f httpapi.Flag) string {
	return fmt.Sprintf("The person sent interrupt %d on %s to another session. It does not stop you: you may go on.", f.ID, f.Target)
}

// flagMeta becomes the <channel> tag's attributes. Keys are letters, digits
// and underscores only; Claude Code drops any other. claimed_by is an agent
// name, and every Claude Code session is "claude", so held_by_you says
// whether the claim is this process's own.
func flagMeta(f httpapi.Flag, mine bool) map[string]string {
	m := map[string]string{
		"target":  f.Target,
		"flag_id": strconv.FormatUint(f.ID, 10),
		"state":   "open",
	}
	switch {
	case f.Watching && f.Kind == httpapi.FlagInterrupt:
		m["state"] = "interrupt"
	case f.Watching:
		m["state"] = "watching"
	case f.End != nil:
		m["state"] = "ended"
	}
	if f.Kind != "" {
		m["kind"] = f.Kind
	}
	if f.ClaimedBy != "" {
		m["claimed_by"] = f.ClaimedBy
	}
	if mine {
		m["held_by_you"] = "true"
	}
	if f.To != nil && !f.To.IsExternal() {
		// Only the session it was sent to is pushed one, but for an
		// interrupt's release, which pushFlags marks after.
		m["to_you"] = "true"
	}
	return m
}

type flagsIn struct {
	Target      string `json:"target,omitempty" jsonschema:"only this target's flags"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"wait up to this many seconds (at most 60) for a new flag or a change to one, instead of answering at once"`
	After       uint64 `json:"after,omitempty" jsonschema:"only flags changed after this rev, from an earlier answer"`
}

type flagIn struct {
	ID uint64 `json:"id" jsonschema:"the flag's id"`
}

func addFlagTools(s *mcp.Server, c *Client) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "flags",
		Description: "List what people flagged as off on a device's screen, newest last, with their notes and boxed regions. Pass wait_seconds to wait for the next one. Read one in full with the flag tool.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in flagsIn) (*mcp.CallToolResult, any, error) {
		q := url.Values{}
		if in.Target != "" {
			q.Set("target", in.Target)
		}
		if in.After > 0 {
			q.Set("after", strconv.FormatUint(in.After, 10))
		}
		if in.WaitSeconds > 0 {
			q.Set("waitMs", strconv.Itoa(min(in.WaitSeconds, 60)*1000))
		}
		return jsonResult(c.getJSON(ctx, "/api/flags", q))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "flag",
		Description: "Read one flag in full: the person's note, the boxed region in device pixels, the interactive UI tree and a screenshot as the screen was when they flagged it (region outlined in red), and, if they reproduced it, their inputs since and the screen when they stopped. Reading claims nothing.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in flagIn) (*mcp.CallToolResult, any, error) {
		id := strconv.FormatUint(in.ID, 10)
		b, err := c.getJSON(ctx, "/api/flags", url.Values{"id": {id}})
		if err != nil {
			return errResult(err), nil, nil
		}
		var f httpapi.Flag
		if err := json.Unmarshal(b, &f); err != nil {
			return errResult(err), nil, nil
		}
		res := textResult(string(b))
		for _, shot := range []struct {
			which string
			have  bool
		}{{"start", f.Start.Screenshot != ""}, {"end", f.End != nil && f.End.Screenshot != ""}} {
			if !shot.have {
				continue
			}
			png, _, err := c.do(ctx, http.MethodGet, "/api/flags/shot", url.Values{"id": {id}, "which": {shot.which}}, "", nil)
			if err != nil {
				continue
			}
			res.Content = append(res.Content,
				&mcp.TextContent{Text: "Screenshot at " + shot.which + ":"},
				&mcp.ImageContent{Data: png, MIMEType: "image/png"})
		}
		return res, nil, nil
	})
}
