# Flags

A flag is how a person watching a device tells an agent what is wrong on
the screen, and an interrupt is how they stop it. Agents read flags through
these routes or through `loupe mcp` ([mcp.md](mcp.md)). Claims and the human
hold, which flags interact with, are in [agent-api.md](agent-api.md).


A flag is a person telling an agent "something is off on this screen". In
the Mac app a ⌘-drag on the picture boxes an area (⌘-click flags the whole
screen), and the note card opens beside the box on release; the Device menu
and toolbar (⌥⌘F) arm the same drag. The web client's More menu starts one
on the focused pane. A note is optional, and "Watch while I reproduce it"
keeps the flag open until the person presses Stop. The card says whether
any agent is listening. Its "Send to" menu, on every flag and interrupt,
starts on the session holding the target and lists the listening sessions,
"Every agent", and the T3 Code threads below.

- `POST /api/flags?target=` `{note, region, watch, kind, to}`. `region` is `[left,
  top, right, bottom]` as fractions of the picture. The server keeps the
  screen as it was: a screenshot with the region outlined in red, the
  interactive UI tree, the region in device pixels, and `claimedBy`, the
  agent holding the target then. A failed capture is recorded on the flag
  (`screenshotError`, `uiError`) rather than failing it. `to: {agent,
  session}`, a session as `/listeners` names it, sends the flag to that
  session alone; without it every session gets it. `to: {t3Thread}` or
  `to: {t3Project}` sends it to T3 Code instead, and `to: {command}` or
  `to: {webhook}` to an entry in `destinations.json` (below).
- `POST /api/flags/end?id=` ends a watch: it adds the person's input since
  the flag (`events`, device pixels) and a second snapshot. An interrupt's
  end may carry `{note, region}`. A watch left
  open ends itself after 10 minutes. An interrupt's end may also carry
  `to`, the session picked while the person wrote. `eventsTruncated` is set when the
  session's 256-event ring dropped some, or the session restarted.
- `GET /api/flags` lists them without UI trees; `?after=<rev>&waitMs=`
  long-polls for the next change; `?id=` reads one in full. `GET
  /api/flags/shot?id=&which=start|end` is the PNG. The server keeps 32.
- `GET /api/flags/listeners?target=` is who would hear a flag raised now:
  each session long-polling `/api/flags` (`waitMs`), now or in the last 5s,
  with `holds` set on the one holding the target, `claimedBy`, and
  `holder`, that session as a `to`, listening or not. `active` lists the
  sessions that called the agent API in the last 15 minutes without waiting
  on flags, with `target` and `idleMs`: a flag can be sent to one, but it
  sees it only when it checks (an interrupt still stops its next input). It
  cannot say whether Claude Code registered the channel (trap 54), only
  that a `loupe mcp` is connected.

- `GET /api/flags/destinations` lists the T3 Code threads a flag can go to,
  for each project in the `t3Projects` setting: `{t3: {error, projects:
  [{path, name, error, threads: [{id, title, status, provider, model,
  updatedAt}]}]}}`, newest first, 10 per project, finished threads
  included. `t3` is absent when no projects are set. The listing is cached
  for 10s and a stale one is served while it refreshes, since each project
  costs the CLI about 0.7s. `commands` and `webhooks` list
  `destinations.json`'s entries as `{name, label}` only, and `error` says
  why that file was refused.

**Sending to T3 Code.** T3 threads can run any provider and may never have
touched loupe, so the server delivers the flag itself, as a message that
stands alone. When the flag is complete (at once, or when its watch or
interrupt ends) it writes the screenshots, UI tree and recorded input to
`~/Library/Caches/loupe/flags/<start>-<id>/`, keeping the newest 32, and
runs `t3 thread send <id> -` (a T3 thread gets it as its next turn; it does
not interrupt one mid-turn) or `t3 thread start --project <path>`, with the
note, the box, the target and those paths on stdin. The outcome lands on the
flag as `delivery: {state: sending|sent|failed, error, thread, dir}`. The
message leads with the note, links each file, and asks the agent to start
its reply with the screenshot as a Markdown image: T3 shows no image a CLI
message carries (the CLI has no attachment option), but renders one in an
agent's reply. The CLI
is `~/.t3-desktop/bin/t3`, which sets T3's home itself, so it works from
launchd; `t3Projects` paths must match T3's projects exactly, and a
`t3Project` that is not in the setting is refused. `loupe mcp` pushes no
plain flag sent to T3. An interrupt sent to T3 still stops whoever drove the
device, as an interrupt with no `to` would: the T3 thread did not move the
device and can't satisfy the gate, so the driver re-checks the screen.

**Sending to a program or a webhook.** `destinations.json`, beside the
settings file, names programs a flag can run and webhooks it can post to.
It is a file of its own, not a setting: any token holder can rewrite the
settings over `PUT /api/config`, and a program there would run as you. The
server reads it on each use and never writes it, and refuses it when group
or others may write it, or another user owns it.

```json
{
  "commands": [
    {"name": "claude", "label": "Claude Code (new run)",
     "program": "/Users/me/.local/bin/claude", "args": ["-p"],
     "dir": "/Users/me/dev/app", "timeoutSec": 1800},
    {"name": "codex", "label": "Codex (new run)",
     "program": "/opt/homebrew/bin/codex", "args": ["exec", "-"],
     "dir": "/Users/me/dev/app"},
    {"name": "claude-live", "label": "Claude Code (in Terminal)",
     "program": "/Users/me/.local/bin/claude", "args": ["{text}"],
     "dir": "/Users/me/dev/app", "terminal": "Terminal"}
  ],
  "webhooks": [
    {"name": "triage", "url": "https://hooks.example.com/flag",
     "headers": {"Authorization": "Bearer …"}}
  ]
}
```

A command's `program` and `dir` are absolute paths; it runs without a shell,
with the flag's message on stdin (the same text a T3 thread gets, less the
request for an image in the reply) and the environment of the server minus
every `LOUPE_` variable, plus its `env`. Arguments may hold `{id}`,
`{target}`, `{dir}`, `{message}` (the message as a file), `{start}`,
`{end}`, `{ui}`, `{events}` and `{text}` (the message itself, which starts
with `> ` or `The person`); never the note alone, which could pass for an
option. With `terminal` set to an app name, the run opens in a new window of
that app instead, so the person can watch it and answer it: Loupe writes it
as `run.command` in the flag's directory, opens that, and records `Opened in
<app>` as delivered. The program then has the window's input, so pass the
message with `{text}`. It runs in the flag's directory unless `dir` says otherwise, for up
to `timeoutSec` (default 600, at most 3600), and `delivery` stays `sending`
until it exits, then carries the end of its output as `output`. A webhook
is POSTed `{flag, message, start, end}` with the screenshots as base64,
since the receiver usually can't reach this server; `https` only, or plain
`http` to localhost, and redirects are not followed. A command or webhook
counts as a T3 thread does for interrupts and for `loupe mcp`.

Flags are the person's signal, like the clipboard: neither claims nor the
human hold gate them, and reading one claims nothing.

**An interrupt** (`kind: "interrupt"`) stops an agent mid-task. The Mac app
sends one on a ⇧⌘-drag, or from "Stop agent & flag" on the driving banner.
It is posted when the person starts, before they write anything, so the
agent hears at once, and it stays open like a watch. The posting viewer
(`X-Loupe-Viewer`) holds the target with no window until the end, capped at
10 minutes. The end takes the note and region, and the hold then runs out
over the usual window. From the post until the holder reads the ended flag
(`GET /api/flags?id=` with its headers, or the `flag` tool), its input
answers 409 `code: "interrupted"` with the flag's `id`; pointing (`move`) is
not refused. When nobody held the target, every agent is refused until one
reads it. An interrupt with `to` stops that session instead, whoever holds
the target. Since an interrupt is posted before the person picks, a `to` on
its end moves the stop onto that session, and `loupe mcp` tells every other
session it pushed the interrupt to that it may go on (`state=released`). Reading mid-interrupt lifts nothing, since the end screen is
what the agent must look at: whether its plan still applies is its call,
made from the device as the person left it. A newer interrupt on the
target replaces the older one. A read carrying `X-Loupe-Viewer` is a
person's app, such as the Mac app following a delivery, and lifts nothing.
