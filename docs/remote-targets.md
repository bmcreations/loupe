# Remote targets

Written 2026-10-26 as a design. Peer servers, the first backend, are built
(`internal/target/peer.go`); the other backends are not.

Every target loupe streams today is attached to the machine loupe runs on:
adb's devices and emulators, CoreSimulator through simcap, iPhones through
CoreDevice. A Mac mini is about to become a second device host, and the
simulators on it should be reachable from the MacBook as if they were local:
60fps, live input, the agent API and claims. Later hosts may be a Linux box
with emulators or a cloud VM.

This doc compares the backends, picks peer loupe servers as the first, and
lays out what that changes on the server's target side. The client protocol
(`docs/protocol.md`) does not change.

## Backends

Checked 2026-10-26. Sources are in the PR that adds this doc; items no
primary source confirmed are marked.

| Backend | Live video to a third party | Input | adb / simctl over the network | Cost | Fit for 60fps |
| --- | --- | --- | --- | --- | --- |
| Another machine running loupe | yes, loupe's own transport | yes | not needed | the machine | good |
| Remote adb server (Linux box, `adb -H`, SSH tunnel) | scrcpy over adb | adb | adb | the machine | good on a LAN |
| Android Device Streaming, `android device remote` | no API; scrcpy over its adb | adb | adb over SSL, real Pixels and partner devices | 30 free min/project/mo, then $0.15/min | plausible, unmeasured |
| Genymotion SaaS, `gmsaas instances adbconnect` | scrcpy over adb | adb | `localhost:<port>` serial | $0.05/min, 2 concurrent | plausible, unmeasured |
| Sauce Labs Real Device Access API | `liveViewUrl` (their viewer) | adb | adb through a localhost bridge; Enterprise plan and private devices only | enterprise | medium; their docs warn the tunnel adds latency |
| AWS Device Farm | their browser UI | browser or managed Appium | no; remote debugging discontinued | $0.17/device-min | poor |
| BrowserStack, LambdaTest | their web UI | web UI or Appium | none found | not checked | poor |
| Firebase Test Lab | no | no | no | batch only; shuts down 2027-09-30 | none |
| Xcode Cloud | none documented | none documented | none documented | CI compute hours | none |
| EC2 Mac, Scaleway | the whole Mac | the whole Mac | the whole Mac | 24h minimum | as a peer, unmeasured |

Two shapes fit, and nothing else does:

1. **A peer loupe.** Any machine that can run loupe, a second Mac, a Linux
   box or a cloud VM, already streams its own targets at loupe's bar. iOS
   only works this way: simulators need macOS and simcap beside them, and
   Apple ships nothing that streams a simulator off its Mac.
2. **A remote adb serial.** Every cloud Android service worth using reduces
   to an adb serial on this machine, after `android device remote create`,
   `gmsaas instances adbconnect` or Sauce's bridge. the cloud emulator study (not published)
   Option A covers what is missing locally: nothing runs the connect, and the
   picker lists the result as a physical device. scrcpy's stream rides TCP
   inside adb, so the video crosses the internet before loupe's transport
   starts.

Order: peer servers first, since the Mac mini is the host that exists. Then
remote adb serials, with `android device remote` the first provider, since
it is the only one with real devices at a per-minute price. A cloud VM is not
a third backend: it is a peer that runs Linux, which is
the cloud emulator study (not published) Option B.

## Peer servers

### Words

- **Server**: one `loupe` process, the binary launchd runs on :18456 today.
- **Peer**: a server another server has been told about, by URL and token.
- **Hub**: a server with peers. Its `/api/targets` lists its own targets and
  every peer's, and it relays streams and API calls for the peer targets.
  Any server can be a hub; nothing is central and nothing is hosted.
- **Owner**: the server a target is attached to.

The phone and the Mac app link to one hub. The mini is the natural one,
since it stays awake while the MacBook comes and goes. The MacBook can also
hold the mini as a peer, so its own agents see the mini's simulators.

### Adding a server

```
GET    /api/servers
  -> 200 [{"name":"mini","url":"https://mac-mini.local:18456","state":"up","targets":4,"rttMs":2}]
POST   /api/servers {"name":"mini","url":"https://mac-mini.local:18456","token":"…"}
  -> 201, after the hub has reached the peer and read its /api/targets
  -> 409 {"error":"…","code":"confirmFingerprint","fingerprint":"<sha256 hex>"}
     when no trusted authority signed the peer's certificate; no token sent
POST   /api/servers {…same…,"fingerprint":"<sha256 hex>"}
  -> 201, with the certificate pinned to that fingerprint
DELETE /api/servers?name=mini
  -> 204
```

- Any number of servers. `name` is the prefix on every target the peer
  owns, so it must be unique, short and free of `/`; the hub picks one from
  the host when none is given.
- The list persists in `servers.json` beside `handsoff.json`, mode 0600,
  since it holds the peers' tokens. It is not a `config.Field`: `/api/config`
  never returns a token, and the trace records only the name and URL.
- `POST` fails with the peer's own error when it cannot be reached or the
  token is refused, so a typo does not sit in the list looking healthy.
- The web client gets an Add server form in the picker's menu and a server
  filter. The Mac app reads the same `/api/servers`, but its UI is not part
  of this change.

### Reaching a peer: no Tailscale needed

The hub dials the peer's URL. A LAN name, a tailnet name, a VPN address and
a public address all work, provided the hub reaches TCP and UDP on the peer's
port (UDP for the raw-QUIC stream).

Trust is the part Tailscale was quietly providing: `loupe-supervisor.sh`
issues the certificate with `tailscale cert` when it has none. A mini on the
LAN without Tailscale has a self-signed one.

`POST /api/servers` first completes a TLS handshake with the peer and sends
nothing over it. A certificate that verifies against the system roots for
the host in the URL (`tailscale cert`, ACME on a cloud VM) is accepted
without a pin, and the add goes ahead. The host is checked for an IP URL
too: the certificate must name that IP. Any other certificate gets a 409
with code `confirmFingerprint` and the SHA-256 of its DER in `fingerprint`,
and the token has not left the hub. Compare it with
`openssl x509 -in <cert> -noout -fingerprint -sha256` run on the peer
against the file its `-tls-cert` names (the Add server form prints it in
that colon-separated form, and the API takes either). Sending the same
request again with that `fingerprint` pins it, the way SSH trusts a host
key. A fingerprint that does not match what the peer presents gets the 409
again, carrying the one it does present.

Every later dial, HTTP and QUIC, checks the pin first. A pinned peer must
present exactly that certificate, even one a trusted authority signed, and
one that changes fails the dial with `certChanged` until the server is
added again. A peer with no pin must keep verifying for its host.

Peer URLs are `https` only: the token rides every request.

The hub authenticates with `Authorization: Bearer` on HTTP and with the token
in the `QUICOpen` message on the stream, not in a `?token=` query, which
would land in the owner's logs. The token is the peer's
one shared token, so a hub can do anything on its peers that the token can.
That is the trust today's token already implies, and scoped tokens are a
follow-up. The pin is stored in `servers.json` beside the token. A rotated
token or a changed certificate is fixed by removing the server and adding it
again; there is no `PUT`.

`POST /api/servers` also probes the owner's raw-QUIC listener. An owner that
ends the connection with `unsupportedProtocol` (it shares no version with the
hub) is refused, so a hub and an owner on different builds fail when the
server is added rather than when a phone opens a target. An owner that does
not answer over QUIC at all (UDP blocked, an older build) is added with a
warning in the response: HTTP works, so listing and driving work and only
streaming may not.

Errors: an owner's own 4xx or 5xx passes through with its body. A dial
failure or a timeout is 502 with reason `serverDown`. An unconfirmed or
mismatched certificate is 409 `confirmFingerprint`, and a certificate that
no longer matches its pin is 502 `certChanged`.

### Merged targets

The hub polls each peer's `GET /api/targets?local=1` every 2s and merges the
rows:

- **IDs are namespaced**: `mini/sim:<udid>`. No local ID contains `/` today
  (AVD names cannot, serials are `host:port`, simulators are `sim:<udid>`),
  so the first `/` splits the server from the owner's ID.
- **`TargetInfo` gains `server`**, the peer's name, omitted for local rows.
  `kind` stays the owner's kind, since the client picks labels, frames and
  input rules by kind.
- **No nesting.** `?local=1` returns only the owner's own rows. A mini and a
  MacBook that peer with each other each list the other's devices once,
  rather than recursing.
- **A peer that stops answering** keeps its rows for 10s, then marks them
  unavailable with reason `serverDown`, then drops them after a minute. It
  reconnects with no action from anyone.

### PeerTarget

A peer target registers in the hub's `Manager` like any other, as a
`target.PeerTarget`. Each capability it implements calls the owner's matching
route:

| Interface | Owner route |
| --- | --- |
| `Target.Open` | raw QUIC (see Streams) |
| `Inspector` | `GET /api/ui` |
| `Actuator`, `TextTyper`, `URLOpener` | `POST /api/input` |
| `Screenshotter` | `GET /api/screenshot` |
| `Framer` | `GET /api/frame` and `/api/frame/asset` |
| `Installer` | `POST /api/install` |
| `Launcher` | `POST /api/input` (`launch`) |
| `Logcatter`, `Crasher` | `GET /api/logcat`, `/api/crashes` |
| `Stopper`, `Starter` | `POST /api/devices/shutdown`, `/boot` |

Because the hub's handlers keep calling `s.mgr.Target(id)` and the interfaces
they already call, every route works on a peer target with no change to the
handler, and every response carries the hub's own ID. A capability the owner
may lack is still implemented on `PeerTarget`, and the owner's 501 passes
through, so the hub answers the way the owner would without a probe at add
time.

The first PR covers `Open`, `Inspector`, `Actuator`, `TextTyper`,
`URLOpener`, `Screenshotter` and `Framer`: watching, driving, and an agent
reading the screen. The rest follow one at a time, each with its own test.
`Deleter`, `Resetter` and creating devices stay off for peer targets: deleting
another machine's simulator from a phone linked to a different machine is a
mistake waiting to happen.

### Streams

`PeerTarget.Open` dials the owner's raw-QUIC listener (ALPN `loupe/1`,
the same port number as HTTPS) with the peer's pinned TLS config. The first
message on the control stream is `QUICOpen`: the token, the target id and the
protocol range. The owner answers with Init and the hub returns a
`target.Stream`.

Raw QUIC rather than `/wt` because the `/wt` path rotates its connection at
12 MiB for Safari's sake (`peerwindow.go`). A hub is not Safari, and a
rotation under a relay would mean a reconnect and a keyframe wait every few
seconds at 60fps.

- Frames: slot frames become a `target.Frame`. `Captured` is the hub's
  receive time, so the latency the phone shows starts at the hub, not at the
  owner's encoder.
- Input: `Touch`, `Key`, `Scroll` and `Text` encode the client messages from
  `docs/protocol.md`, so they reach the owner's arbiter as a viewer's input,
  and a person on the phone holds the target against agents on the owner too.
- Init and Stats from the owner are read for `Meta` and the owner's viewer id
  (`PeerStream.OwnerViewer`), then dropped. The hub sends its own, which
  describe the hub-to-phone link. So is the owner's `MsgControl`.
- Clipboard, cursor and events from the owner reach the hub's viewers, since
  they watch the same device. `MsgClipboard` feeds the stream's
  `ClipboardChanges` (four queued, the oldest dropped past that), which
  `Session.watchClipboard` sends on as for any target. `MsgCursor` and
  `MsgEvent` go out through `RelayedControl` to `Session.watchRelayed`, which
  publishes them as the hub's own. The hub never forwards an agent's `move`,
  so every relayed cursor is an agent on the owner.
- A relayed event loses its `viewer`, which names a viewer of the owner, and
  takes the hub's next `seq`. An event carrying the relay's own viewer id is
  dropped: it is a hub viewer's gesture or input, which the hub published
  already. An agent's `/api/input` on the hub for a peer target publishes
  nothing on the hub, because the owner publishes it and the relay brings it
  back; so the hub's event arrives a round trip after the call returns. The
  hub publishes its own only when the owner was never reached
  (`target.ErrPeerDown`).
- `RequestKeyframe` has no client message to send: the protocol has none
  (`0x10`-`0x15`). The owner sends keyframes on its own interval, one second
  on Android, so after a shed the hub waits up to that long. A client message
  for it would be a protocol bump and is the first follow-up if the wait shows
  in the trace.

The hub's session sits on top as usual, so fan-out to several viewers, the
`Tap` recordings hang off, and pacing all run unchanged. The cost is a second
pacer: the owner sheds for its link to the hub, and the hub for its link to
the phone. On a LAN the first should never fire.

So the owner sees one viewer per hub per target, however many phones watch
the hub. `Manager.Acquire` opens it on the first viewer and its idle grace
closes it after the last, as for any target, and a recording's hold keeps it
open the same way. While it is open the owner's idle pause and shutdown do
not fire and its `awake` holds, because the hub is a viewer.

When the owner drops the stream, the frames channel closes with an error, the
hub's session ends, and the phone reconnects the way it does after any
target's session fails. The hub does not reconnect underneath a live session.

Adaptive bitrate on the hub is off for peer targets in the first version: a
relaunch would mean a reconnect to the owner and a keyframe wait, and the
owner's own controller already adapts to the link it can see.

### Claims

`claims.go` keys claims by target ID in one server's memory, so a hub and the
owner would each hold their own claim on the same simulator, and two agents
on two hubs could both drive it. For peer targets the hub forwards
`/api/claims` to the owner with the agent's `X-Loupe-Agent` and
`X-Loupe-Session`, and the owner arbitrates. Hands off is the owner's too.
The hub's `claimedBy`, `handsOff` and `drivenBy` fields come from the owner's
row.

Input is arbitrated on both servers, each for what it can see:

- A person on a phone linked to the hub reaches the owner as a viewer through
  the relay, so the owner's human hold refuses agents driving the owner
  directly.
- An agent's `/api/input` on the hub goes through the hub's arbiter first,
  which refuses it while a hub viewer holds the target. `PeerTarget` then
  forwards it with `X-Loupe-Agent` and `X-Loupe-Session`, so the owner refuses
  it with `humanHeld` while a person on the owner holds the target, and with
  `claimedBy` while another agent's claim does. The hub passes the owner's
  refusal through unchanged.
- A hub viewer's own `/api/input` (a phone's Home or Back button) goes to the
  owner with `X-Loupe-Viewer: <the owner's viewer id for the hub's relay on
  that target>`, read from the live relay stream, not with an agent's
  headers. The owner then treats it as the person it is: not refused
  `humanHeld`, and never refused by a claim. With no relay open there is no
  such id and the call goes as an agent. The owner still cannot tell two
  phones on the hub apart; they share one relay viewer, and the hub's arbiter
  sorts them out.
- `/api/claims` and `/api/devices/handsoff` for a peer id are forwarded whole;
  the hub's own claim book never holds a peer id and its arbiter does not
  consult it for one. Reads (`/api/ui`, `/api/screenshot`, `/api/frame`) carry
  the agent headers too, so a holder keeps its claim alive on the owner by
  using it through the hub. The owner's status and body come back verbatim,
  with the owner's target id in the body rewritten to the hub's. `GET
  /api/claims` lists this server's own claims only.
- `/api/clipboard` for a peer id goes to the owner's `/api/clipboard`, and the
  owner's refusal comes back as it sent it: 409 when the owner has no session
  for the target. The hub's shared clipboard never writes a peer target.
- The shared clipboard crosses servers through a bridge per peer, not
  through peer targets: the hub long-polls the peer's
  `/api/clipboard/shared` and posts its own changes there
  (`docs/agent-api.md`). Each server writes its own targets.

### Hub code that assumes a target is local

Each needs to skip or forward peer rows:

- `markHeadless` and `traceAVD` read local qemu processes.
- `AVDNames`, `SimulatorNames`, `avdresolve` and `isADB` match local
  emulators and adb serials.
- `devicelife.go`'s rule for which rows offer shutdown, boot and reset.
- `emupause.go`'s idle pause and shutdown: the owner runs its own.
- `awake`: the hub holds off its own sleep while relaying, and the owner holds
  off its own because the hub is its viewer.

## Measuring it

On this Mac, never against :18456: one spare server on another port as the
owner, a second as the hub with the first as its peer. `wtprobe -for 20s`
against the hub for a peer simulator, and against the owner directly, while
`simprobe` scrolls. The hub path is acceptable when its fps matches the
direct path within 2fps and its pts p90 within 5ms. Then the same pair with
the owner on the mini, once it exists.

## Follow-ups

- **Direct to the owner from the Mac app.** Each merged row can carry the
  owner's URL, and a client that can reach the owner opens video and input
  there, skipping the hub. Waits for the relay measurement, and touches
  Handset.
- **`/ws` to the owner** for a peer reachable over TCP only.
- **A keyframe request message** in the protocol.
- **Remote adb serials**, backend 2 above.
- **Scoped tokens.** A peer's token opens everything on the peer, the same
  gap `docs/design/remote-dev.md` names for share links.
