# MCP

`loupe mcp` exposes the agent API to any MCP client as a stdio server, and
pushes flags into Claude Code sessions. The routes behind each tool are in
[agent-api.md](agent-api.md); flags themselves are in [flags.md](flags.md).

## Setup

Copy the binary first, then register the copy. The reason is under The
server, below.

```sh
cp loupe ~/.local/bin/loupe
claude mcp add loupe -- ~/.local/bin/loupe mcp
```

Any other MCP client takes the same command: `~/.local/bin/loupe mcp`, with
`-url` if the server is not on `https://localhost:18456`.

## The server

**`loupe mcp` puts the Agent API behind MCP**, so an agent that speaks MCP
needs no curl. It is a client of a running server, not a second server: a
stdio MCP server (the official Go SDK) that turns each tool call into one
request. Tools: `targets`, `ui`, `screenshot` (an image, not JSON), `point`,
`tap`, `swipe`, `type_text`, `key`, `button`, `launch`, `open_url`, `logcat`,
`crashes`, `crash`, `install`, `camera`, `boot`, `release`, `flags` and `flag`. `TestEveryToolNamesARoute` checks each
against `httpapi.Routes()`.

- **Flags:** `-url` (default `https://localhost:18456`), `-token` and
  `-agent` (default `claude`). The token is read from `-token`, then
  `LOUPE_TOKEN`, then the server's token file; `loupe mcp` never writes one.
  TLS is verified except for a loopback host, whose cert names the ts.net
  host instead.
- **It claims before it drives.** Every request carries `X-Loupe-Agent` and
  `X-Loupe-Session`, a UUID per process. The first driving call on a target
  claims it; a 409 `claimedBy` forgets the claim so the next call tries
  again. Reads claim nothing. On exit, SIGINT or SIGTERM included, each
  claim is released with a plain `DELETE`, never `?force=1`.
- **It never sends `X-Loupe-Viewer`**, which would make its input a
  person's and walk through the human hold. A refusal comes back as a tool
  error with the status, `code`, `retryAfterMs` and the holder.
- **Stdout is the MCP wire**, so `main` dispatches `mcp` before the server's
  flags and banner, and everything else goes to stderr.

Run through a go-sdk client over stdio against the launchd server on
Pixel_10 (emulator-5556), 22 October: `ui` in 3.2s, `screenshot` as a 169KB
PNG in 219ms, then `button` home claimed the target, `launch` Settings in
0.47s, `release` emptied `/api/claims`, a `tap` claimed again and closing the
client released it. With another agent's claim on the target, `button` and
`launch` came back as tool errors naming `claimedBy` and the holder, and
that claim was still there after the client exited.

Point an MCP config at a copy of the binary, not at `./loupe`: a client
holds the executable open for its whole session, and `make build` then
fails with ETXTBSY, which is why the supervisor runs from `.run/`.


## Flags over channels

**`loupe mcp` pushes flags into a Claude Code session** through channels
(a research preview, `code.claude.com/docs/en/channels-reference.md`). It
long-polls `/api/flags` and sends each new flag, and each watch that ends,
as a `notifications/claude/channel` message; the model sees a `<channel
source="loupe" target=... flag_id=... state=open|watching|interrupt|ended
kind=interrupt claimed_by=... held_by_you=true to_you=true>` tag and is
told, in the server's instructions, to act when `to_you` or `held_by_you` is
set, or nobody holds the target. `claimed_by` alone can't say that: every
Claude Code session's agent name is `claude`. A flag with `to` is pushed
only to that session, marked `to_you`; every other flag goes to every
connected `loupe mcp`. Flags already on the server when it starts are not sent. Without
channels the same flags are there through the `flags` tool (`wait_seconds`
to wait) and `flag`, which returns the record and both screenshots.

Channels need the server in a project `.mcp.json` and the session started
with the development flag:

```sh
claude --dangerously-load-development-channels server:loupe
```

The go-sdk has no call for a notification it does not define, so
`channelTransport` keeps the connection the SDK serves on and writes the
JSON-RPC notification to it. See trap 54 for the protocol version.
