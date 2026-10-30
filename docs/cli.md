# CLI

`loupe <command>` drives a running loupe server from a shell or a script:

```sh
loupe targets
loupe ui avd:Pixel_10 --interactive
loupe tap avd:Pixel_10 540 1200
loupe tap avd:Pixel_10 --text "Sign in"
loupe type avd:Pixel_10 "hello"
loupe screenshot avd:Pixel_10 -o shot.png
loupe release avd:Pixel_10
```

`loupe help` lists every command, and `loupe <command> -h` describes one
and its fields. The answer is the server's JSON on stdout. An error goes to
stderr with the server's code (`humanHeld`, `claimedBy`, `handsOff`) and
exit status 1; bad arguments exit 2.

## Commands are the MCP tools

Each command is a tool from `loupe mcp`, run in the same process over an
in-memory transport, so the two cannot drift: the same schemas, the same
claim before input, the same requests. `TestEveryToolHasACommand` fails when
a tool has no command or a command's positional names a field the tool does
not have.

| Command | Tool | Positional |
|---|---|---|
| `targets` | `targets` | |
| `ui` | `ui` | target |
| `screenshot` | `screenshot` | target |
| `point` | `point` | target x y |
| `tap` | `tap` | target [x y] |
| `swipe` | `swipe` | target x y x2 y2 |
| `type` | `type_text` | target text |
| `key` | `key` | target keycode |
| `button` | `button` | target button |
| `launch` | `launch` | target package |
| `open` | `open_url` | target url |
| `sequence` | `sequence` | target steps |
| `logcat`, `crashes` | `logcat`, `crashes` | target |
| `crash` | `crash` | target id |
| `install` | `install` | target path... |
| `camera` | `camera` | target path |
| `drop` | `drop` | target x y path |
| `boot`, `release` | `boot`, `release` | target |
| `flags` | `flags` | |
| `flag` | `flag` | id |

Any other field is a flag: `-ms 300` on `swipe`, `--interactive` on `ui`,
`-launch` on `install`. Values are converted by the type in the tool's
schema. A negative number is an argument, not a flag, and `--` ends flags,
for text that starts with a dash.

`tap` takes a point, or names a node with `--text`, `--id` or `--desc`
(plus `--contains`, `--index`, `--waitMs`) and leaves the point out. Giving
`x` without `y` is still an error.

`sequence` takes its steps as one JSON array of `/api/input` bodies, inline,
`-` for stdin, or `@path` for a file:

```sh
loupe sequence avd:Pixel_10 @signin.json
echo '[{"action":"tap","match":{"text":"Email"}},{"action":"text","text":"a@b.co"}]' |
  loupe sequence avd:Pixel_10 -
```

`screenshot` writes the PNG to `-o`, or to stdout when stdout is a pipe. On
a terminal without `-o` it refuses rather than print binary.

## Server, token and identity

| Flag | Environment | Default |
|---|---|---|
| `-server` | `LOUPE_URL` | `https://localhost:18456` |
| `-token` | `LOUPE_TOKEN` | the server's token file |
| `-agent` | `LOUPE_AGENT` | `cli` |
| `-session` | `LOUPE_SESSION` | none |

The certificate is not checked on a loopback host, as with `loupe mcp`.

## Claims last between commands

Input commands claim the target first, as the MCP tools do. A claim is a
lease that every call from its holder renews and that lapses after 10 idle
minutes (see Claims and the human hold in `agent-api.md`). `loupe mcp`
releases its claims when it exits; the CLI does not, because each command is
a new process and the next `tap` has to find the claim still held. End it
with `loupe release <target>` or let it lapse.

The server keys a claim by session when one is sent, and by agent name
otherwise. With neither `-session` nor `LOUPE_SESSION`, every CLI command
under the same agent name is one holder. Two scripts or two agents sharing
a machine should each set `LOUPE_SESSION` (or `-agent`) so one cannot drive
a device the other claimed.
