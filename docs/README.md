# loupe docs

The top-level `README.md` says what loupe does. Start with
[getting-started.md](getting-started.md) to run it.

## Using it

| Doc | What's in it |
| --- | --- |
| [getting-started.md](getting-started.md) | From a clone to a device on your phone, a launchd service and a connected agent |
| [running.md](running.md) | Every flag, the settings file, reading the stats overlay, tests and probes |
| [targets.md](targets.md) | AVDs (boot, pause, idle shutdown, reset, New device), the Android phone keyboard, the clipboard, device frames |
| [ios.md](ios.md) | iOS simulators through simcap, and physical iPhones and iPads in preview through CoreDevice |
| [security.md](security.md) | What the token protects and what it doesn't, pairing a device, sessions and revoking them |

## Agents

| Doc | What's in it |
| --- | --- |
| [agent-api.md](agent-api.md) | Every device route under `/api/*`, tap by text and `/api/sequence`, claims and the human hold |
| [cli.md](cli.md) | `loupe <command>`: the MCP tools as shell commands, and how claims last between them |
| [mcp.md](mcp.md) | `loupe mcp`, its tools, and pushing flags into Claude Code over channels |
| [flags.md](flags.md) | A person flagging a screen or interrupting an agent, and where a flag can be sent |
| [recording.md](recording.md) | Recording a target's encoder output to MP4 |
| [api.schema.json](api.schema.json) | Request and response schema for the agent API |

## Extensions

| Doc | What's in it |
| --- | --- |
| [extensions.md](extensions.md) | The v0 contract: registering, the `/api/ext` proxy, the security model |
| [../action/README.md](../action/README.md) | The GitHub Action that installs a PR's APK and links the PR to the device |

## How it works

| Doc | What's in it |
| --- | --- |
| [architecture.md](architecture.md) | Package layout, transports, deployment under launchd, auth, configuration, adaptive bitrate, telemetry, known gaps |
| [protocol.md](protocol.md) | The session wire protocol between the server and its viewers |
| [remote-targets.md](remote-targets.md) | Streaming targets attached to another machine; peer servers are built |

## Working on loupe

| Doc | What's in it |
| --- | --- |
| [releasing.md](releasing.md) | Tagging a release, the Homebrew formula, and why simcap builds from source |
| [internals/traps.md](internals/traps.md) | Problems hit while building loupe and what the code does about each, symptom first. Code comments cite them by number |

## Design notes

Written before the work, to argue its order. Parts have since been built,
and each note says which; the reference docs above describe what exists.

| Doc | What's in it |
| --- | --- |
| [design/agent-tooling.md](design/agent-tooling.md) | Four gaps in the agent API: MCP, iOS logs, crash reports, network capture |
| [design/remote-dev.md](design/remote-dev.md) | An agent gets a clean device and proves its change on it |
