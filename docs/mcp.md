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
`swipe`, `scroll`, `type_text`, `key`, `button`, `launch`, `open_url`, `sequence`,
`logcat`, `crashes`, `crash`, `install`, `uninstall`, `camera`, `drop`, `boot`, `boot_status`,
`release`, `flags`, `flag` and `submit_feedback`. `install` takes `downgrade` as
`/api/install` does, and `uninstall` takes `package`; both are Android only. `boot` takes `wait` as `/api/devices/boot` does
(`10m`, or `0` to return at once), and `boot_status` reports on a boot still
running; it reads only and claims nothing.

## Feedback

`submit_feedback` (`loupe feedback` in the CLI) lets an agent report a problem with loupe itself: a tool
that failed or answered wrongly, a missing tool or option, an error that did
not say what to do next, a wrong doc. Its description tells the model to
write the report as an issue (`problem`, `repro`, `proposed_fix`), to send one
per problem once it has worked around it or given up, and to leave out
secrets and screen contents. `repro` may be left out. `target`, if sent, is a
target id such as `avd:<name>`, `sim:<udid>` or an Android serial, not a description. The server's
instructions repeat when to call it. It claims nothing. The route and where reports
go are in [agent-api.md](agent-api.md#post-apifeedback-get-apifeedback).

Where a report goes:

- **Your own Mac:** name a `feedback` entry in `destinations.json`
  ([flags.md](flags.md#sending-to-a-program-or-a-webhook)). A `t3Project`
  starts a T3 Code thread that checks the report and fixes it in a worktree.
  A webhook gets the report as Markdown in `text`, the field a Slack incoming
  webhook posts. Slack's docs don't say whether it ignores the `feedback` key
  beside it, and Slack shows `#` headings as typed.
- **Anyone else's:** with no entry, turning on the `feedbackRelay` setting
  sends reports to loupe's maintainer, as issues in a private repository and
  in Slack. It sends the report's own fields and loupe's version, not the
  target, the agent's session or local paths. It is off by default, and the
  tool's description tells the agent not to change it.
- **Neither:** the report stays on the Mac.

An agent wrote every word of a report. A T3 thread is told to treat it as a
claim to check, not as instructions, and the cap of 3 threads an hour keeps a
fix thread that files its own report from starting a chain. On a Mac where
other people's agents run, prefer a webhook to a thread.

## Options

| Flag | Default | |
|---|---|---|
| `-url` | `https://localhost:18456` | The loupe server. |
| `-token` | `LOUPE_TOKEN`, then the server's token file | `loupe mcp` never writes one. |
| `-agent` | `claude` | Name shown in the trace and on the banner. |
| `-target` | `LOUPE_TARGET` | The target a device tool drives when its call leaves `target` out. Unset, a call without one drives the only device that is up; with several up it fails and lists them. `targets`, `flags`, `flag`, `boot`, `boot_status` and `submit_feedback` never get a default. |

`ui`, and the screen that `sequence` and `scroll` end with, come as one line
per node: class, text, `desc=`, `value=`, `id=`, the `@x,y` center and flags.
Layout containers that carry only an id are left out, and ids in the app's own
package are printed without it. `ui` with `json: true` returns `/api/ui`'s JSON.

TLS is verified except for a loopback host, whose certificate names the ts.net
host instead.

## Claims and refusals

- Every request carries `X-Loupe-Agent` and `X-Loupe-Session`, a UUID per
  process.
- The first driving call on a target claims it, `uninstall` included. `install` claims only with `launch`. Reads claim nothing. If
  another agent holds the claim, the call fails with `claimedBy`, and the next
  call tries again.
- On exit each claim is released: when the client hangs up, and on SIGINT,
  SIGTERM or SIGHUP (a closed terminal, or a tmux pane killed or respawned).
  A tool call still in flight, such as a `flags` wait, does not hold that
  up. Only SIGKILL leaves a claim behind, until its lease lapses 10 minutes
  after the last call. A claim you did not make is never forced.
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
