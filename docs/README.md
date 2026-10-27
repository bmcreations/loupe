# loupe docs

Start with the top-level `README.md` for what loupe is and how to run it.

## Using it

| Doc | What's in it |
| --- | --- |
| [running.md](running.md) | Every flag, the settings file, reading the stats overlay, tests and probes |
| [targets.md](targets.md) | AVDs (boot, pause, idle shutdown, reset, New device), the Android phone keyboard, the clipboard, device frames |
| [ios.md](ios.md) | iOS simulators through simcap, physical iPhones and iPads through CoreDevice |
| [agent-api.md](agent-api.md) | Every `/api/*` route, claims and the human hold, `loupe mcp`, recording |
| [api.schema.json](api.schema.json) | Request and response schema for the agent API |

## Extensions

| Doc | What's in it |
| --- | --- |
| [extensions.md](extensions.md) | The v0 contract: registering, the `/api/ext` proxy, the security model |

## How it works

| Doc | What's in it |
| --- | --- |
| [architecture.md](architecture.md) | Package layout, transports, deployment under launchd, auth, configuration, adaptive bitrate, telemetry, known gaps |
| [protocol.md](protocol.md) | The session wire protocol between the server and its viewers |
| [remote-targets.md](remote-targets.md) | Streaming targets attached to another machine; peer servers are built |
| [traps.md](traps.md) | Every trap already hit, symptom first. Search it for the symptom you see |

## Design notes

Written before the work, to argue its order. Parts have since been built, and
each note says which; `agent-api.md` describes what exists.

| Doc | What's in it |
| --- | --- |
| [design/agent-tooling.md](design/agent-tooling.md) | Four gaps in the agent API: MCP, iOS logs, crash reports, network capture |
| [design/remote-dev.md](design/remote-dev.md) | An agent gets a clean device and proves its change on it |
