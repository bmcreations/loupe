# MCP

`loupe mcp` exposes the agent API to any MCP client as a stdio server, and
pushes flags into Claude Code sessions. It is a client of a running loupe
server, not a second server: each tool call becomes one request. The routes
behind each tool, with parameters and errors, are in
[agent-api.md](agent-api.md); flags are in [flags.md](flags.md). The same
tools run as shell commands in [cli.md](cli.md).

## Setup

Copy the binary, then register the copy:

```sh
cp loupe ~/.local/bin/loupe
claude mcp add loupe -- ~/.local/bin/loupe mcp
```

A client holds the executable open for its whole session, so pointing it at
`./loupe` makes the next `make build` fail with ETXTBSY.

Any other MCP client takes the same command, `~/.local/bin/loupe mcp`, with
`-url` if the server is not on `https://localhost:18456`.

## Tools

`targets`, `ui`, `screenshot` (returns an image, not JSON), `point`, `tap`,
`swipe`, `type_text`, `key`, `button`, `launch`, `open_url`, `sequence`,
`logcat`, `crashes`, `crash`, `install`, `camera`, `drop`, `boot`, `boot_status`,
`release`, `flags` and `flag`. `boot` takes `wait` as `/api/devices/boot` does
(`10m`, or `0` to return at once), and `boot_status` reports on a boot still
running; it reads only and claims nothing.

## Options

| Flag | Default | |
|---|---|---|
| `-url` | `https://localhost:18456` | The loupe server. |
| `-token` | `LOUPE_TOKEN`, then the server's token file | `loupe mcp` never writes one. |
| `-agent` | `claude` | Name shown in the trace and on the banner. |

TLS is verified except for a loopback host, whose certificate names the ts.net
host instead.

## Claims and refusals

- Every request carries `X-Loupe-Agent` and `X-Loupe-Session`, a UUID per
  process.
- The first driving call on a target claims it. Reads claim nothing. If
  another agent holds the claim, the call fails with `claimedBy`, and the next
  call tries again.
- On exit, SIGINT and SIGTERM included, each claim is released. A claim you
  did not make is never forced.
- It never sends `X-Loupe-Viewer`, so its input is never a person's and does
  not bypass the human hold.
- A refusal comes back as a tool error with the status, `code`,
  `retryAfterMs` and the holder.

## Flags over channels

`loupe mcp` pushes flags into a Claude Code session through channels (a
research preview, `code.claude.com/docs/en/channels-reference.md`). It sends
each new flag, and each watch that ends, as a `notifications/claude/channel`
message. The model sees a tag like:

```
<channel source="loupe" target=... flag_id=... state=open|watching|interrupt|ended
 kind=interrupt claimed_by=... held_by_you=true to_you=true>
```

and is told to act when `to_you` or `held_by_you` is set, or nobody holds the
target. `claimed_by` alone can't say that, because every Claude Code
session's agent name is `claude`.

- A flag with `to` is pushed only to that session, marked `to_you`. Every
  other flag goes to every connected `loupe mcp`.
- Flags already on the server when `loupe mcp` starts are not sent.
- Without channels the same flags are available through the `flags` tool
  (`wait_seconds` to wait) and `flag`, which returns the record and both
  screenshots.

Channels need the server in a project `.mcp.json` and the session started with
the development flag:

```sh
claude --dangerously-load-development-channels server:loupe
```
