# Agent tooling gaps

Written 2026-10-22 after comparing loupe's agent API with Quern (quern.dev), a
local MCP server for driving iOS and Android devices from an agent. Since
then the MCP server (`loupe mcp`) and simulator crash reports
(`/api/crashes`) have been built; `docs/mcp.md` and `docs/agent-api.md` describe them. It
records four gaps, what closing each would take on this Mac,
and an order to close them in, so the order can be argued with before any code
is written.

The agent this is for is a developer's own coding session working on their
app: install the PR's build, open the screen, see what broke. That is the lane
loupe's agent API is for, beside a person testing
from a phone. It is not Quern's lane. Quern lists 107 tools in 17 categories
and depends on Homebrew, Python 3.11, Node 22, idb-companion, pymobiledevice3,
mitmproxy and scrcpy. Matching it tool for tool is not the goal; each section
asks what the developer's session cannot do today and the smallest thing that
would let it.

Each section has the same parts: the gap, what loupe has now, the mechanism by
platform, what is not known, and the first thing to build.

## 1. An MCP server

**The gap.** An agent drives loupe today by reading CLAUDE.md's Agent API
section and composing `curl` calls, with the bearer token, `X-Loupe-Agent`,
`X-Loupe-Session` and a claim made by hand. An MCP client would get the same
routes as typed tools it can discover, without being taught the headers.

**What loupe has.** Every capability an MCP server would expose is already a
route: `/api/targets`, `/api/ui`, `/api/input`, `/api/screenshot`,
`/api/install`, `/api/logcat`, `/api/media`, `/api/record`, `/api/claims` and
`/api/devices/*`. The route table in `internal/httpapi/server.go` is one list
shared by `Routes` and `Handler`, and `TestEveryJSONRouteHasASchemaEntry`
fails when a JSON route has no entry in `docs/api.schema.json`.

**Mechanism.** A thin client over HTTP, holding no device code:

- Tool definitions generated from `docs/api.schema.json`. The test above
  already keeps the schema in step with the routes, so tools generated from it
  cannot drift from what the server accepts.
- The token from `LOUPE_TOKEN` or `~/Library/Caches/loupe/token`, as the
  server reads it.
- `X-Loupe-Agent` and `X-Loupe-Session` on every call, and a claim taken on
  the first input to a target and released on exit. A 409 `claimedBy`,
  `handsOff` or `humanHeld` goes back to the agent as the tool's error, with
  `retryAfterMs` when there is one. The server never forces a claim.
- `/api/screenshot` returned as an image result, so the agent sees the screen
  without a second fetch.

The server stays HTTP-only. It ships nothing from `internal/target` or
`simcap`, so publishing it publishes no more of loupe than the routes
already do.

What MCP adds is discovery and typed arguments, not capability. An agent with
`curl` can already do everything listed here.

**Not known.**

- Whether it is a subcommand of the loupe binary (`loupe mcp`, one install,
  Go's MCP SDK) or a separate npm package (`npx`, which is how most Claude Code
  users add a server). The first keeps one version to match against the
  server; the second fits how the server will usually be installed.
- Whether `/api/logcat?follow=1` and a recording in progress map onto MCP at
  all, or stay bounded reads (`lines`, `since`) and start/stop pairs.

**First thing to build.** Generate tools for `/api/targets`, `/api/ui`,
`/api/input` and `/api/screenshot` from the schema, with the claim handling,
and drive one PR's flow from Claude Code with nothing else configured.

## 2. iOS logs

**Status.** Built in #115, both halves. The phone half needed no new
dependency; the paragraph on it below is corrected.

**The gap.** `/api/logcat` covers Android: the last `lines`, everything
`since` a time, `follow`, and one of `pid`, `package` or `foreground`. A
simulator or a phone answers 501, so an agent debugging an iOS build has no
log through loupe.

**What loupe has.** `Logcatter` and `LogcatQuery` in
`internal/target/logcat.go`, optional the way `Inspector` is, and the handler
that streams and flushes a follow.

**Mechanism, simulator.** `xcrun simctl spawn <udid> log show` and
`log stream`, which run the simulator's own `log`. Run on this Mac against a
booted iOS 27 simulator:

- `log show --last 30s --style ndjson --predicate 'process == "SpringBoard"'`
  printed one JSON object per line with `subsystem`, `category`,
  `formatString`, `threadID` and `activityIdentifier` among the fields.
- `log stream --style ndjson --level info` printed the same shape live.
- Both print `getpwuid_r did not find a match for uid 501` on stderr first,
  which a parser has to skip.

`--last` maps onto `since` as a duration, and `--start` takes a time. `log`
has no line count, so `lines` would be trimmed on the server, as logcat's
`since` is now. `package` maps onto a predicate on the process's bundle;
`foreground` needs the front app's process, which the simulator path can
already name.

The output is not logcat's format. Either the route returns `log`'s own lines
for an iOS target, or a new route returns NDJSON for both. Changing
`/api/logcat`'s format for Android would break any caller reading it now.

**Mechanism, physical phone.** No first-party command streams a phone's log.
`devicectl device` has `capture`, `copy`, `info`, `process`, `sysdiagnose` and
others, and none of them is a log stream; `devicectl device sysdiagnose`
gathers an archive after the fact. That much holds, but live logs need no new
dependency: lockdownd's `com.apple.syslog_relay` is reachable through the
usbmuxd socket loupe already dials for the XCUITest runner, over a cable or
Wi-Fi, with the pair record usbmuxd hands out. It is what `idevicesyslog`
reads. On bmc-iphone (iOS 27.2, Wi-Fi) it sent a 64KB backlog in under a
second. It is text only, with no time index.

**Not known.** Whether predicates on `subsystem` are worth exposing, or a
`package` filter is enough. How long a `log stream` takes to show its first
line under load, which matters for `follow`.

**First thing to build.** The simulator half: `Logcatter` on the simulator
target over `simctl spawn log`, `lines`, `since` and `follow` only, with the
format question settled first. The phone half followed in the same change,
since syslog_relay needed nothing new.

## 3. Crash reports

**The gap.** When the app under test crashes, the agent sees the screen go to
the launcher and has to find out why from logcat, if the crash is still in the
buffer. There is no list of crashes for a target, and nothing on iOS.

**Mechanism, Android.** `dumpsys dropbox` is readable without root. On
emulator-5556 (`sdk_gphone16k_arm64`, a Play Store image) it held three
entries, tagged `system_app_anr`, `system_app_native_crash` and
`system_server_watchdog`. `data_app_crash` and `data_app_anr` are the tags
Android uses for an installed app's crash and ANR. Neither was present then,
since no app had crashed. `am crash` later left a `data_app_crash` on
emulator-5556 (API 37) and a `system_app_crash` on an API 29 emulator, and
`dumpsys dropbox --print --file <tag>` read both the same way.

Tombstones under `/data/tombstones` need root, and `adb root` on that emulator
answered "adbd cannot run as root in production builds". Most AVDs on this Mac
are Play Store images, so dropbox is the route, and a native crash's full
tombstone is out of reach there.

**Mechanism, simulator.** A simulator's crashes land on the Mac, in
`~/Library/Logs/DiagnosticReports/*.ips`. The first line of each is a JSON
header (`app_name`, `bundleID`, `timestamp`, `"platform":7`), and the body
carries `"coalitionName" : "com.apple.CoreSimulator.SimDevice.<udid>"`, which
names the simulator. 229 of the 232 `.ips` files on this Mac carried that
coalition, so filtering by it is how one simulator's crashes are told apart
from another's and from the Mac's own.

**Mechanism, physical phone.** `devicectl device copy from --domain-type
systemCrashLogs` copies the phone's crash logs. Its help lists
`systemCrashLogs` among four domains, beside `temporary` and the two app
containers. It has not been run against either phone here.

**Shape.** `GET /api/crashes?target=&since=` listing each crash with its
process, package or bundle, time and kind, and `GET /api/crashes/{id}` for the
report itself. On Android the dropbox entry, on iOS the `.ips` as it is.
Symbolication is out of scope: a debug build from a PR carries its symbols.

**Not known.** Whether dropbox keeps enough entries to be useful after a busy
session; its limits are set per device.

**Measured since.** A simulator's report is late: one was written 22s after a
Acmechat crash, and one was listed 37s after Calendar was sent SIGABRT on
`loupe-probe`, so an agent that asks straight after a crash finds nothing yet. A dropbox entry is
not: `am crash` on the selv sample on emulator-5556 was listed by
`/api/crashes` 0.13s later.

**First thing to build.** The simulator list, since it is a directory read
with a filter and needs nothing from the device. Then Android's dropbox.

## 4. Network capture and mocking

**The gap.** An agent cannot see what the app sent or got back, or make a
request fail to check the error path. Quern does both through mitmproxy. Its
page does not say how it installs the CA on a device or what a mock rule looks
like.

**Mechanism.** A proxy on the Mac, the device's traffic sent through it, and
the proxy's CA trusted on the device. Each part has a platform limit:

- **Android apps ignore user-installed CAs** from Android 7, unless the app's
  network security config trusts user CAs, which a debug build can do. Trusting
  the proxy in a release build needs the CA in the system store, which needs
  root, which Play Store images refuse (above). So capture works for a debug
  build that opts in, or on a `google_apis` image where `adb root` works.
- **A simulator** takes `simctl keychain <udid> add-root-cert`. It uses the
  Mac's network, so routing it through a proxy is a host-level setting or the
  app's own proxy configuration. Neither has been tried here.
- **A physical iPhone** needs the CA installed as a profile and full trust
  turned on by hand in Settings, and a proxy set on its Wi-Fi network. Nothing
  here can do that without a person at the phone.

**Decisions this section leaves open.**

- mitmproxy, a Python dependency with a mature flow format, or a proxy in Go
  inside the loupe binary, which keeps the install to one binary and means
  writing TLS interception.
- What a mock rule is: a URL pattern and a canned response, a status override,
  a delay. This is undesigned.
- Whether capture is on per target, per session, or per agent claim, and who
  else sees the flows. Flows carry tokens and personal data, so the trace rules
  for `openURL` (no paths or queries recorded) are the floor.

**First thing to build.** Nothing yet. This is the one gap that needs new
infrastructure on the device, and it has a hard platform limit on Android
release builds. Settling the open decisions comes first.

## Proposed order

1. **MCP server.** It wraps routes that exist, adds no device code, and makes
   every later gap reachable from an MCP client the day it lands.
2. **iOS logs.** It sits beside `logcat.go` with a command already shown to
   work here, and the phone half reads syslog_relay through usbmuxd.
3. **Crash reports.** Simulator first, a directory read; then Android's
   dropbox; then the phone through `devicectl`.
4. **Network.** Last, because it needs new infrastructure on the device and
   the decisions above before anything can be built.
