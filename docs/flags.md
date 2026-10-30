# Flags

A flag is how a person watching a device tells an agent what is wrong on
the screen, and an interrupt is how they stop it. Agents read flags through
these routes or through `loupe mcp` ([mcp.md](mcp.md)). Claims and the human
hold, which flags interact with, are in [agent-api.md](agent-api.md).

## Raising a flag

In the Mac app, ⌘-drag on the picture boxes an area and ⌘-click flags the
whole screen; the note card opens beside the box on release. The Device menu
and toolbar (⌥⌘F) arm the same drag. The web client's More menu starts one on
the focused pane.

- The note is optional.
- "Watch while I reproduce it" keeps the flag open until the person presses
  Stop.
- The card says whether any agent is listening. Its "Send to" menu, on every
  flag and interrupt, starts on the session holding the target and lists the
  listening sessions, "Every agent", and the T3 Code threads below.

Flags are the person's signal, like the clipboard: neither claims nor the
human hold gate them, and reading one claims nothing.

## Routes

`POST /api/flags?target=` takes `{note, region, watch, kind, to}`.

- `region` is `[left, top, right, bottom]` as fractions of the picture.
- The server keeps the screen as it was: a screenshot with the region outlined
  in red, the interactive UI tree, the region in device pixels, and
  `claimedBy`, the agent holding the target then. A failed capture is recorded
  on the flag (`screenshotError`, `uiError`) rather than failing it.
- `to` picks the destination:

  | `to` | Goes to |
  | --- | --- |
  | omitted | every session |
  | `{agent, session}` | that session alone, as `/listeners` names it |
  | `{t3Thread}` or `{t3Project}` | T3 Code |
  | `{command}` or `{webhook}` | an entry in `destinations.json` |

`POST /api/flags/end?id=` ends a watch. It adds the person's input since the
flag (`events`, device pixels) and a second snapshot.

- A watch left open ends itself after 10 minutes.
- An interrupt's end may carry `{note, region}` and `to`, the session picked
  while the person wrote.
- `eventsTruncated` is set when the session's 256-event ring dropped some, or
  the session restarted.

Reading:

- `GET /api/flags` lists flags without UI trees. `?after=<rev>&waitMs=`
  long-polls for the next change; `?id=` reads one in full. The server keeps 32.
- `GET /api/flags/shot?id=&which=start|end` is the PNG.

`GET /api/flags/listeners?target=` is who would hear a flag raised now:

- Each session long-polling `/api/flags` (`waitMs`), now or in the last 5s,
  with `holds` set on the one holding the target.
- `claimedBy`, and `holder`: that session as a `to`, listening or not.
- `active`: sessions that called the agent API in the last 15 minutes without
  waiting on flags, with `target` and `idleMs`. A flag can be sent to one, but
  it sees it only when it checks (an interrupt still stops its next input).

The listing says only that a `loupe mcp` is connected, not that Claude Code
registered its channel.

`GET /api/flags/destinations` lists where a flag can go:

- `t3`: the threads of each project in the `t3Projects` setting, as
  `{error, projects: [{path, name, error, threads: [{id, title, status,
  provider, model, updatedAt}]}]}`. Newest first, 10 per project, finished
  threads included. Absent when no projects are set. The listing can be a few
  seconds stale.
- `commands` and `webhooks`: `destinations.json`'s entries as `{name, label}`
  only.
- `error`: why `destinations.json` was refused.

## Sending to T3 Code

The server delivers the flag itself, as a message that stands alone, so the
thread can run any provider and may never have touched loupe.

- When the flag is complete (at once, or when its watch or interrupt ends), it
  writes the screenshots, UI tree and recorded input to
  `~/Library/Caches/loupe/flags/<start>-<id>/`, keeping the newest 32.
- It sends the note, the box, the target and those paths to the thread as its
  next turn, or starts a new thread in a project. It does not interrupt a
  thread mid-turn.
- The message leads with the note and asks the agent to start its reply with the
  screenshot as a Markdown image.
- The outcome lands on the flag as `delivery: {state: sending|sent|failed,
  error, thread, dir}`.
- `t3Projects` paths must match T3's projects exactly. A `t3Project` that is
  not in the setting is refused.
- `loupe mcp` pushes no plain flag sent to T3.
- An interrupt sent to T3 still stops whoever drove the device, as an interrupt
  with no `to` would, and the driver re-checks the screen.

## Sending to a program or a webhook

`destinations.json`, beside the settings file, names programs a flag can run
and webhooks it can post to. It is a file of its own, not a setting. The
server reads it on each use and never writes it, and refuses it when group or
others may write it, or another user owns it.

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

**Commands**

- `program` and `dir` are absolute paths. It runs without a shell, in the
  flag's directory unless `dir` says otherwise.
- The flag's message arrives on stdin. The environment is the server's minus
  every `LOUPE_` variable, plus the command's `env`.
- Arguments may hold `{id}`, `{target}`, `{dir}`, `{message}` (the message as a
  file), `{start}`, `{end}`, `{ui}`, `{events}` and `{text}` (the message
  itself).
- With `terminal` set to an app name, the run opens in a new window of that app
  so the person can watch and answer it, and `Opened in <app>` is recorded as
  delivered. The program then has the window's input, so pass the message with
  `{text}`.
- `timeoutSec` defaults to 600, at most 3600. `delivery` stays `sending` until
  the program exits, then carries the end of its output as `output`.

**Webhooks**

- POSTed `{flag, message, start, end}` with the screenshots as base64, since the
  receiver usually can't reach this server.
- `https` only, or plain `http` to localhost. Redirects are not followed.

A command or webhook counts as a T3 thread does for interrupts and for
`loupe mcp`.

## Interrupts

An interrupt (`kind: "interrupt"`) stops an agent mid-task. The Mac app sends
one on a ⇧⌘-drag, or from "Stop agent & flag" on the driving banner.

- It is posted when the person starts, before they write anything, and stays
  open like a watch.
- The posting viewer (`X-Loupe-Viewer`) holds the target with no window until
  the end, capped at 10 minutes. The end takes the note and region, and the hold
  then runs out over the usual window.
- From the post until the holder reads the ended flag (`GET /api/flags?id=` with
  its headers, or the `flag` tool), its input answers 409 `code: "interrupted"`
  with the flag's `id`. Pointing (`move`) is not refused.
- When nobody held the target, every agent is refused until one reads it.
- An interrupt with `to` stops that session instead, whoever holds the target. A
  `to` on its end moves the stop onto that session, and `loupe mcp` tells every
  other session it pushed the interrupt to that it may go on
  (`state=released`).
- Reading mid-interrupt lifts nothing, so the agent decides from the device as
  the person left it whether its plan still applies.
- A newer interrupt on the target replaces the older one.
- A read carrying `X-Loupe-Viewer` is a person's app, such as the Mac app
  following a delivery, and lifts nothing.
