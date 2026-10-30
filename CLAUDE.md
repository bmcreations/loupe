# Loupe

Stream an Android device or emulator to an iPhone's browser at 60fps with input
latency low enough to feel live. Built to make remote device testing usable
from a phone.

Owner: Brandon McAnsh (bmc08gt). Module path `github.com/bmcreations/loupe`.

## The one result that matters

**The iPhone's hardware decoder is not the bottleneck and never was.** A trace
of 48 steady-state samples showed decoded fps median 60.0 with `decodeQueue`
never exceeding 0. The decoder sat idle waiting for frames.

Everything since has been transport work. Do not re-litigate the decode path.


## Where things are

Read the doc before touching its area. None of these load automatically.

- `docs/architecture.md`: shape, the package layout table, transports
  (WebSocket, WebTransport, raw QUIC, rotation at Safari's 16 MiB), running
  it, sleep, auth, configuration, adaptive bitrate, client telemetry, known
  gaps, environment notes.
- `docs/targets.md`: AVDs (boot, `loupe-warm`, pause, idle shutdown, reset,
  New device), Android keyboard, clipboard, device frames.
- `docs/ios.md`: iOS simulators (simcap, HID, CoreDevice) and physical iPhones.
- `docs/running.md`: every flag, the stats overlay, tests.
- `docs/agent-api.md`: every device route under `/api/*`, claims and the
  human hold.
- `docs/cli.md`: `loupe <command>`, the MCP tools as shell commands.
- `docs/mcp.md`: `loupe mcp`, its tools, flags pushed over channels.
- `docs/flags.md`: flags, watches, interrupts, T3 Code and other destinations.
- `docs/recording.md`: `/api/record` and the MP4 writer.
- `docs/security.md`: what auth guarantees and doesn't, pairing and sessions.
- `docs/getting-started.md`: the walkthrough from a clone to a phone,
  the service and an agent.
- `docs/internals/traps.md`: the traps list, symptom first. Grep it for the symptom you see,
  and read the entry before changing code a trap names.
- `docs/protocol.md`: session wire protocol.
- `docs/extensions.md`: the v0 contract for extensions: registering, the
  `/api/ext` proxy, and the security model.
- `docs/README.md`: the index of every doc, design notes included.
- Skill `loupe-probes`: wtprobe, simprobe, overlaptest and the other probes,
  and the layout self-check.

## Build, deploy, test

```sh
make build           # this IS the deploy: launchd's supervisor restarts onto it
make agent-status    # state, pid, last exit
make agent-log       # tail loupe-run.log
make test            # go test ./... and node web/run-tests.mjs
make simcap          # clones and builds the simulator/iPhone helper, not part of build
```

The server runs under launchd as `dev.bmcreations.loupe` from a copy in
`.run/`, so `make build` never hits ETXTBSY. Point MCP configs at a copy of the
binary too, not `./loupe`. Test against a spare server on another port, not the
launchd one.

## Hard rules

- Every request needs the shared token (`Authorization: Bearer`, cookie, or
  `?token=`). `/ws` and `/wt` must stay exempt from the token-stripping
  redirect: WebTransport sends no cookies (trap 11).
- `/api/input` coordinates are device pixels, as `/api/ui` reports them.
  Drive from the accessibility tree, never from the video.
- Anything reaching a device shell goes through a pattern check and
  `shellQuote`.
- Only `app.js` imports client modules; shared code comes in through `mount`'s
  `deps` (trap 45). A new module must be listed in `clientModules` (trap 43).
  `web/` has no build step.
- Do not move to scrcpy 4.x without changing the parser: 4.1 shifts every
  video packet flag down a bit and misparses in silence. See Environment notes in `docs/architecture.md`.
- Verify with the probes and the trace (`loupe-trace.jsonl`), not numbers read
  off a phone or a screenshot.
