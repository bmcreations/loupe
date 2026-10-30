# Remote targets

A server can list and stream targets that are attached to another loupe
server. The simulators on a Mac mini then show up on the MacBook or the
phone with the same 60fps video, live input, agent API and claims as a
local target. The client protocol (`docs/protocol.md`) does not change.

Peer servers are the only remote backend that exists. Any machine that can
run loupe can be one: a second Mac, a Linux box with emulators, a cloud VM.
iOS simulators can only be reached this way, since they need macOS and
simcap beside them. A remote adb serial and cloud device services are not
built.

## Peer servers

### Words

- **Server**: one `loupe` process.
- **Peer**: a server another server has been told about, by URL and token.
- **Hub**: a server with peers. Its `/api/targets` lists its own targets and
  every peer's, and it relays streams and API calls for the peer targets.
  Any server can be a hub; nothing is central and nothing is hosted.
- **Owner**: the server a target is attached to.

The phone and the Mac app link to one hub. A machine that stays awake, like
a mini, is the natural one. The MacBook can also hold the mini as a peer, so
its own agents see the mini's simulators.

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

- `name` is the prefix on every target the peer owns, so it must be unique,
  short and free of `/`. The hub picks one from the host when none is given.
- The list persists in `servers.json` beside `handsoff.json`, mode 0600,
  because it holds the peers' tokens. `/api/config` never returns a token,
  and the trace records only the name and URL.
- `POST` fails with the peer's own error when it cannot be reached or the
  token is refused, so a typo does not sit in the list looking healthy.
- The web client has an Add server form in the picker's menu and a server
  filter. The Mac app reads the same `/api/servers`.

### Reaching a peer: no Tailscale needed

The hub dials the peer's URL. A LAN name, a tailnet name, a VPN address and a
public address all work, as long as the hub reaches TCP and UDP on the
peer's port (UDP carries the stream).

Peer URLs are `https` only, because the token rides every request. A peer
that has a certificate from a trusted authority (`tailscale cert`, ACME on
a cloud VM) is added as is. The certificate must name the host in the URL,
an IP included.

A self-signed certificate, which is what a mini on the LAN without Tailscale
has, takes two steps:

1. `POST /api/servers` answers 409 `confirmFingerprint` with the
   certificate's SHA-256 in `fingerprint`. The token has not left the hub.
2. Compare it with the output of `openssl x509 -in <cert> -noout
   -fingerprint -sha256` run on the peer against the file its `-tls-cert`
   names. The Add server form prints it in that colon-separated form, and the
   API takes either. Send the request again with `fingerprint` set to pin it,
   the way SSH trusts a host key.

A fingerprint that does not match what the peer presents gets the 409 again,
carrying the one it does present.

A pinned peer must present exactly the pinned certificate, even if a trusted
authority signed another. If it changes, dials fail with `certChanged` until
the server is added again. A rotated token is fixed the same way: remove the
server and add it again, since there is no `PUT`.

`POST /api/servers` also checks that the owner's QUIC listener speaks a
protocol version the hub does. An owner that shares none is refused when
added, so a hub and an owner on different builds fail then rather than when
a phone opens a target. An owner that does not answer over QUIC at all (UDP
blocked, an older build) is added with a warning: listing and driving work
and only streaming may not.

The hub's token is the peer's one shared token, so a hub can do anything on
its peers that the token can. Scoped tokens do not exist yet.

Errors:

| Status | Reason |
| --- | --- |
| the owner's own 4xx or 5xx | passed through with its body |
| 502 | `serverDown`: a dial failure or a timeout |
| 409 | `confirmFingerprint`: certificate not confirmed, or not matching |
| 502 | `certChanged`: the certificate no longer matches its pin |

### Merged targets

The hub polls each peer every 2s and merges the rows.

- **IDs are namespaced**: `mini/sim:<udid>`. The first `/` splits the server
  from the owner's ID.
- **Rows gain `server`**, the peer's name, omitted for local rows. `kind`
  stays the owner's kind, since the client picks labels, frames and input
  rules by kind.
- **No nesting.** A mini and a MacBook that peer with each other each list
  the other's devices once.
- **A peer that stops answering** keeps its rows for 10s, then they read
  unavailable with reason `serverDown`, and a minute later they are dropped.
  It reconnects with no action from anyone.

### Working on a peer target

Every route takes the hub's ID for a peer target and calls the owner's
matching route. The response carries the hub's ID.

| Hub route | Works on a peer target |
| --- | --- |
| stream (`/wt`, `/ws`) | yes, see Streams |
| `GET /api/ui` | yes |
| `POST /api/input` (taps, swipes, text, keys, buttons, URLs) | yes |
| `GET /api/screenshot` | yes |
| `GET /api/frame`, `/api/frame/asset` (device frames) | yes |
| `/api/clipboard` | yes, forwarded to the owner |
| `/api/claims`, `/api/devices/handsoff` | yes, the owner arbitrates (see Claims) |
| `/api/install`, `launch`, `/api/logcat`, `/api/crashes`, boot and shutdown | not yet |
| delete, reset, create device | off on purpose: deleting another machine's simulator from a phone linked to a different machine is a mistake waiting to happen |

A capability the owner lacks passes the owner's 501 through.

### Streams

The hub connects to the owner over raw QUIC, on the same port number as
HTTPS, and sends the token, the target id and its protocol range on the
first message. The hub's session sits on top as usual, so several viewers,
recordings and pacing work unchanged.

- **Latency** shown on the phone starts at the hub's receive time, not at
  the owner's encoder.
- **Input** from a phone reaches the owner as a viewer's input, so a person
  on the phone holds the target against agents on the owner too.
- **Clipboard, cursor and events** from the owner reach the hub's viewers,
  since they watch the same device. A relayed event loses its `viewer`,
  which names a viewer of the owner, and takes the hub's next `seq`.
- **An agent's `/api/input` on the hub** for a peer target publishes its
  event when the owner's copy comes back through the relay, a round trip
  after the call returns. The hub publishes its own only when the owner was
  never reached.
- **One viewer per hub per target.** The owner sees a single viewer however
  many phones watch the hub. It opens with the first viewer and closes after
  the last, with the usual idle grace. While it is open the owner's idle
  pause and shutdown do not fire and the owner stays awake.
- **Two pacers.** The owner sheds for its link to the hub, and the hub for
  its link to the phone. On a LAN the first should never fire. After a shed,
  the hub waits for the owner's next keyframe, up to a second on Android.
- **Adaptive bitrate** is off for peer targets. The owner's own controller
  adapts to the link it can see.
- **When the owner drops the stream**, the hub's session ends and the phone
  reconnects as it does after any target's session fails. The hub does not
  reconnect underneath a live session.

### Claims

Claims are held by the owner. The hub forwards `/api/claims` for a peer
target with the agent's `X-Loupe-Agent` and `X-Loupe-Session`, so two agents
on two hubs cannot both drive one simulator. Hands off is the owner's too.
The hub's `claimedBy`, `handsOff` and `drivenBy` fields come from the owner's
row. `GET /api/claims` lists this server's own claims only.

Input is arbitrated on both servers, each for what it can see:

- A person on a phone linked to the hub reaches the owner as a viewer, so
  the owner's human hold refuses agents driving the owner directly.
- An agent's `/api/input` on the hub is refused while a hub viewer holds the
  target. Otherwise it is forwarded with the agent's headers, and the owner
  refuses it with `humanHeld` while a person on the owner holds the target,
  or with `claimedBy` while another agent's claim does. The hub passes the
  refusal through unchanged.
- A hub viewer's own `/api/input` (a phone's Home or Back button) reaches
  the owner as that person: never refused `humanHeld`, never refused by a
  claim. With no stream open it goes as an agent. The owner cannot tell two
  phones on the hub apart; the hub's arbiter sorts them out.
- Reads (`/api/ui`, `/api/screenshot`, `/api/frame`) carry the agent headers,
  so a holder keeps its claim alive on the owner by using it through the hub.
- The owner's status and body come back verbatim, with the owner's target id
  rewritten to the hub's. `/api/clipboard` for a peer target is the owner's
  too, so it is 409 when the owner has no session for the target.
- The shared clipboard crosses servers through a bridge per peer: the hub
  long-polls the peer's `/api/clipboard/shared` and posts its own changes
  there (`docs/agent-api.md`). Each server writes its own targets.

## Limits

- A peer's token opens everything on the peer. Scoped tokens are not built.
- Streams need UDP between hub and owner. Over TCP only, listing and driving
  work and streaming may not.
- Adaptive bitrate does not run on the hub for peer targets.
- A changed token or certificate needs the server removed and added again.
- The Mac app and the viewer both go through the hub; neither opens video
  directly to the owner.
