# Wire protocol

The browser client and the server speak one semantic protocol over two
possible transports. Every fixture named below lives under
`testdata/protocol/` as lowercase hex with `#` comment lines, so a non-Go
client (a Kotlin test, `cmd/uiprobe`) can read the same file rather than
trust this document alone. `internal/session/golden_test.go` and
`internal/httpapi/golden_test.go` pin the Go encoders and decoders against
them.

Byte ranges in every table below count from the message's own type byte as
byte 0, when that message has one. A frame header has no type byte (see
below), so its byte 0 is its first field.

## Versioning

A client names the protocol range it speaks when it connects: `?proto=1` or
`?proto=1-3` on `/ws` and `/wt`, the `proto` field of `QUICOpen` on raw QUIC.
`session.NegotiateProtocol` picks the newest version both sides speak; the
server speaks `MinProtocolVersion` to `ProtocolVersion` (both 1 today). A
client that sends nothing is taken to speak version 1, so pages from before
negotiation keep working.

A client the server shares no version with is refused before the target is
acquired, so it never boots a device. The refusal is the ended reason
`unsupportedProtocol`, sent the way every ended reason is: `MsgEnded` on `/ws`,
close code 3 on `/wt`, `QUICCloseEnded` on raw QUIC. The web client shows
"This page is out of date" with a Reload button. Init carries the version in
use as `protocol`.

Bump `ProtocolVersion` for any change an older client would misread. Raise
`MinProtocolVersion` only once no installed client still needs the old
version: a native app can stay on a phone for months.

## Transports

Both implement `httpapi.Conn`, so nothing above them branches on which is
live.

- **WebSocket** (`/ws`) is TCP: reliable and ordered, and therefore unable to
  abandon a frame once written. Every message -- video frame, control
  message -- goes out length-implicit inside one WebSocket frame, with its
  own type byte at the front. A message must arrive as a binary WebSocket
  frame; a text frame is read and discarded (`wsConn.Recv`), and an empty
  binary frame is skipped the same way.
- **WebTransport** (`/wt`) is QUIC. Video runs one unidirectional stream per
  access unit: the stream's own FIN delimits the frame, so there is no
  message-type byte and no length prefix on that path. Everything else --
  touch, key, scroll, ping, hello and client stats from the client; init,
  stats, pong and event from the server -- runs on one bidirectional control
  stream, and every message on it is framed the same way in both directions:
  a u32 big-endian length prefix followed by that many bytes of the
  type-prefixed message the WebSocket transport writes as a whole frame
  (`wtConn.SendControl` on the way out, `wtConn.Recv` on the way in, both
  built on `wtFrameControl`/`wtReadControlFrame` in
  `internal/httpapi/webtransport.go`). The server refuses a length prefix
  above 1 MiB (`wtControlLimit`) without reading the body, since every real
  control message is tens of bytes and an unbounded read driven by an
  attacker-chosen length would be trivial to exploit. `web/transport.js:209-280`
  is the client's matching encode/decode. Fixture:
  `testdata/protocol/wt-control-hello.hex` -- a hello frames as `00000001
  15`.

Unrecognized client message types are logged and otherwise ignored; the
session and the connection both stay up (`dispatch`'s `default` case,
`internal/httpapi/server.go`).

## Frame headers

The same access unit, framed two different ways.

### WebSocket: 18-byte header (`session.FrameHeaderLen`)

Built by `session.EncodeFrame`. Fixtures: `testdata/protocol/frame-ws.hex`
(a keyframe) and `testdata/protocol/frame-ws-config.hex` (a config frame,
`flagConfig` set).

| Bytes | Field | Meaning |
| --- | --- | --- |
| 0 | type | `session.MsgFrame` (`0x02`) |
| 1 | flags | bit 0 `flagKey`, bit 1 `flagConfig` |
| 2-9 | pts | `ptsMicros`, u64 big-endian |
| 10-17 | sentAt | server wall clock at send time, microseconds, u64 big-endian |
| 18+ | data | the Annex B access unit |

`sentAt` is the server's own clock, not synchronized with the client's. The
client subtracts it from its own clock on arrival; the absolute number is
meaningless, but its variance is what shows whether the network is the
problem.

### WebTransport: 13-byte header

Built by `wtEncodeFrameHeader` in `internal/httpapi/webtransport.go`, called
from `wtConn.nextFrameHeader`, the same call `SendFrame` makes. Fixtures:
`testdata/protocol/frame-wt.hex` (a keyframe) and
`testdata/protocol/frame-wt-config.hex` (a config frame).

| Bytes | Field | Meaning |
| --- | --- | --- |
| 0-3 | frameID | u32 big-endian |
| 4-11 | ptsMicros | u64 big-endian |
| 12 | flags | bit 0 key, bit 1 config |

No type byte, no length: the uni stream itself is one frame, whole or
absent. A stream still in flight when it goes stale can be `RESET_STREAM`ed
by the sender, which is the reason this transport exists -- TCP cannot
recall a frame once written.

`frameID` is a per-connection counter (`wtConn.frameID`), starting at 1 and
incremented for every frame the session *attempts* to send on that
connection, including one that ends up reset before it is fully written.
Uni streams can arrive at the client out of QUIC-stream order, so `frameID`
is what lets it detect that: a gap in the sequence it has received means a
frame in between was reset, not a frame the client somehow missed intact.

### `ptsMicros`

`ptsMicros` is the device's own absolute uptime in microseconds, not a
counter that restarts per session -- it continues across an encoder relaunch
on the same device (trap 31, `CLAUDE.md`). A parameter-set (config) frame
reports pts `0`: it carries no picture, so no presentation time applies. A
jitter estimator or a buffer that anchors its clock on the first pts it sees
must treat a config frame's `0` as not a real sample, or a straddling pair
reads as a multi-hundred-thousand-second jump (`web/jitter.js`'s guard
against exactly this).

## Message ids

Every id below is `internal/session/protocol.go`'s own constant; nothing here
restates a number that file does not also define.

### Server -> client

| id | name | body |
| --- | --- | --- |
| `0x01` | `MsgInit` | JSON, see [Init](#init-json) |
| `0x02` | `MsgFrame` | binary, see [Frame headers](#frame-headers) |
| `0x03` | `MsgStats` | JSON, see [Stats](#stats-json) |
| `0x04` | `MsgPong` | binary, see [Pong](#pong) |
| `0x05` | `MsgError` | reserved; no encoder constructs it today |
| `0x06` | `MsgEvent` | JSON, see [Event](#event-json) |
| `0x0B` | `MsgCursor` | JSON, an [Event](#event-json) with kind `move` and no `seq`; live only, never in the log or `/api/events` |
| `0x0C` | `MsgOverlay` | JSON `{"overlays":[...]}`, every extension overlay on the target as fractions of the screen; the whole set each time, an empty list clears, and sent to each joiner after init. Images are a server path to GET, never on this stream. See `/api/overlays` in [agent-api.md](agent-api.md) |

### Client -> server

| id | name | body |
| --- | --- | --- |
| `0x10` | `MsgTouch` | binary, see [Touch](#touch) |
| `0x11` | `MsgKey` | binary, see [Key](#key) |
| `0x12` | `MsgScroll` | binary, see [Scroll](#scroll) |
| `0x13` | `MsgPing` | binary, see [Ping](#ping) |
| `0x14` | `MsgClientStats` | JSON, opaque map, see [Client stats](#client-stats) |
| `0x15` | `MsgHello` | empty body |

`MsgHello` exists because QUIC streams are lazy: opening one is a local
operation, and the peer learns about it only when the first bytes arrive. A
server waiting to accept the control stream would wait forever behind a
client that opened it and had nothing to say yet, so the client sends
`MsgHello` immediately on connect and the body carries nothing. See
`testdata/protocol/wt-control-hello.hex` for how it looks framed on `/wt`.

## Client -> server messages

### Touch

Fixture: `testdata/protocol/touch.hex`. Decoded by `session.DecodeTouch`
(the leading type byte is stripped first).

| Bytes | Field | Meaning |
| --- | --- | --- |
| 1 | action | `0` down, `1` up, `2` move, `3` cancel (`web/pane.js`'s `ACTION_DOWN`/`ACTION_UP`/`ACTION_MOVE`/`ACTION_CANCEL`, matching Android's `MotionEvent` action constants) |
| 2-5 | pointerId | u32 big-endian |
| 6-9 | x | i32 big-endian, stream coordinates |
| 10-13 | y | i32 big-endian, stream coordinates |
| 14-15 | pressure | u16 big-endian, `0..0xFFFF` maps to `0.0..1.0` |

Coordinates are in the space `Meta.Width`/`Height` describes -- what
`MaxSize` rescales the stream to -- not device pixels.

### Key

Fixture: `testdata/protocol/key.hex`. Decoded by `session.DecodeKey`.

| Bytes | Field | Meaning |
| --- | --- | --- |
| 1 | action | `0` down, `1` up (`web/app.js`'s `KEY_DOWN`/`KEY_UP`) |
| 2-5 | keycode | i32 big-endian, an Android keycode |

A key-up whose keycode names a device button (`target.ButtonForKeycode`)
publishes as an [Event](#event-json) with `kind:"button"`, not `kind:"key"`.
Any other key-up publishes `kind:"key"` with the raw keycode on the wire,
unredacted -- there is no password-field check on this path the way there is
for typed text (see [Redacted text event](#redacted-text-event)); that is a
known gap, not something this fixture set changes.

### Scroll

Fixture: `testdata/protocol/scroll.hex`. Decoded by `session.DecodeScroll`.

| Bytes | Field | Meaning |
| --- | --- | --- |
| 1-4 | x | i32 big-endian, stream coordinates |
| 5-8 | y | i32 big-endian, stream coordinates |
| 9-12 | hscroll | f32 big-endian (IEEE 754) |
| 13-16 | vscroll | f32 big-endian (IEEE 754) |

### Ping

Fixture: `testdata/protocol/ping.hex`. Decoded by `session.DecodePing`.

| Bytes | Field | Meaning |
| --- | --- | --- |
| 1-8 | clientMicros | u64 big-endian, the client's own clock |

### Client stats

`MsgClientStats`'s body is JSON the server treats as an opaque
`map[string]any` (`session.RecordClientStats`) and stores verbatim as
`Session.ClientStats()`. The server reads exactly two keys back out of it,
by `healthInputFromClientReport` (`internal/httpapi/health_track.go`):
`rttMs` (float64, the client's own measured round trip) and `decFps`
(float64, decoded frames per second, truncated to an int as `Delivered`).
Any other keys the client sends ride along unread.

## Server -> client messages

### Init (JSON)

The first message every viewer receives. Type: `session.InitPayload`.
Fixture: `testdata/protocol/init.json`.

```json
{"codec":"avc1.42E01E","codedWidth":1080,"codedHeight":2424,"name":"emulator-5554","kind":"emulator","control":true,"sessionId":"emulator-5554","maxBufferMs":400,"viewer":1,"protocol":1}
```

| Field | Meaning |
| --- | --- |
| `codec` | RFC 6381 string for `VideoDecoder.configure`, derived from the stream's own SPS |
| `codedWidth`, `codedHeight` | dimensions of the encoded video |
| `name` | label for the UI |
| `kind` | `"device"`, `"emulator"` or `"synthetic"` |
| `control` | whether this session accepts input; the client hides its input affordances when false |
| `sessionId` | the target id (`session.go:307`), the same string `GET /api/targets` lists it under -- **not** a per-connection or per-session-instance value. It stays the same across a session restart against the same target, which is exactly why it cannot be used to detect one; see `EventPage.session` below for the field that changes. |
| `maxBufferMs` | ceiling on the client's jitter buffer, a server setting because the operator is the one who knows the link |
| `viewer` | this viewer's id, the one its own gestures carry as `Event.viewer`; every viewer is sent every event, its own included |
| `protocol` | the session protocol version in use on this connection; see Versioning |

Sent again, with new dimensions, on a rotation -- see `CLAUDE.md` trap 42. The
client treats a changed `codedWidth`/`codedHeight` as grounds to resize the
canvas and reconfigure the decoder.

### Pong

Fixture: `testdata/protocol/pong.hex`. Built by `session.EncodePong`.

| Bytes | Field | Meaning |
| --- | --- | --- |
| 0 | type | `MsgPong` (`0x04`) |
| 1-8 | clientMicros | u64 big-endian, echoed back verbatim |
| 9-16 | serverMicros | u64 big-endian, the server's own clock at reply time |

### Stats (JSON)

Sent once a second per viewer. Built by `statsMsgBody`
(`internal/httpapi/viewer.go`). Fixtures: `testdata/protocol/stats.json` (a
live health report) and `testdata/protocol/stats-stale.json` (no report has
arrived, or the last one is too old).

```json
{"baseMs":233.4,"fps":52,"health":"degraded","healthReason":"delivered 158 of 175 frames","keep":0.85,"rttMs":265.1,"shed":3,"shedRate":0,"transport":"webtransport"}
```

| Field | Always present | Meaning |
| --- | --- | --- |
| `health` | yes | `"good"`, `"degraded"`, `"bad"` or `"stale"` -- `session.Health`'s verdict |
| `healthReason` | yes | names the counter that drove the verdict, e.g. `"delivered 158 of 175 frames"`, `"shed N/s"`, `"rtt Xms over a Yms base"`, `"no report"`; empty string when `health` is `"good"` |
| `shed` | yes | `Session.ShedCount()` -- the whole session's cumulative shed count, summed across every viewer of it, not just this one |
| `shedRate` | yes | that same session-wide counter, diffed since the last tick: frames shed this second across all viewers, all reasons combined (pacer, stale, await-key, abandon) |
| `baseMs` | yes | the pacer's learned path baseline, milliseconds (`Pacer.Base()`) |
| `keep` | yes | the pacer's current send fraction, `[0.1, 1]` (`Pacer.Keep()`) -- the fraction of sheddable frames being sent |
| `transport` | yes | `"websocket"` or `"webtransport"` (`Conn.Name()`) |
| `fps` | only when `health != "stale"` | the most recently *consumed* client report's decoded-frames-per-second, carried forward unchanged on any tick with nothing fresh to read -- so it can be reporting a number up to just under 2.5s old (`healthTracker.lastDelivered`) |
| `rttMs` | only when `health != "stale"` | the same report's measured round trip, carried forward the same way (`healthTracker.lastRttMs`) |

`fps` and `rttMs` are dropped entirely, not zeroed, on a stale verdict:
reporting them would repeat a stale or absent number as though it were a
current measurement. `stats-stale.json` is the fixture that proves the
encoder actually omits the keys rather than sending `0`.

#### `health`'s verdict order

`session.Health` (`internal/session/health.go`) checks, in this fixed order,
and returns on the first one that fires:

1. **Stale.** `ReportAge >= 2.5s` (`staleAfter`) returns `"stale", "no
   report"` immediately, before anything else below is even read. The
   boundary is inclusive: a report exactly 2.5s old reads stale, matching
   `web/health.js`'s `>=`. A client report timestamped before this viewer
   attached is never counted as fresh in the first place (see
   `healthTracker.sampleAt`'s `fresh` check), so a report age can only ever
   describe something recorded after attach.

If the report is fresh, three live checks are each judged and the worst one
wins; delivery ratio is checked first, then shed rate, then rtt, and that
order also breaks a tie between two checks landing on the same severity,
since only one reason string can be returned:

2. **Delivery ratio**, only once at least `minProducedForRatio` (5) frames
   have been produced in the window -- an idle panel legitimately produces
   as little as one frame every few seconds (trap 20), and below 5 a single
   missed frame swings the ratio wildly. `< 0.6` is bad, `< 0.9` is
   degraded.
3. **Shed rate**, `>= 20`/s bad, `>= 5`/s degraded.
4. **Rtt**, judged as excess over `baseMs`, not as an absolute number (trap
   5): `>= 120ms` over base is bad, `>= 60ms` degraded. Skipped entirely
   while `baseMs` is 0 -- that is not a zero-latency path, it is the pacer
   not having sampled anything yet, and every fresh viewer's first ticks
   would otherwise read "rtt Xms over a 0ms base" regardless of how healthy
   the link actually is.

#### The delivery ratio's own window

`healthReason`'s `"delivered N of M frames"` is **not** one second's count on
either side. `healthTracker` (`internal/httpapi/health_track.go`) keeps a
sliding window of up to `healthWindowTicks` (5) *fresh client reports*, and
both `N` and `M` are that window's sums:

- Every stats tick reads `Session.MediaCount()` -- media frames only, a
  config frame is excluded before this counter is even incremented, so a
  parameter-set resend never counts as a produced frame -- and diffs it
  against the last tick to get this tick's production rate.
- A tick pushes a new sample into the window only when a client report
  landed on it that is both no older than this viewer's own attach time and
  strictly newer than the last report already consumed (`fresh` in
  `sampleAt`); most ticks have nothing fresh and push nothing.
- When one does land, it is paired with the *previous* tick's production
  rate, not the tick it landed on -- `web/pane.js`'s client reports decFps
  for the second before its own once-a-second timer fired, so the report
  arriving on tick *i* actually describes what was produced during tick
  *i-1*.
- `N` (delivered) and `M` (produced) are the sums of `delivered` and
  `producedPerSec` across whatever samples are currently in the window --
  up to 5 of them, fewer for a viewer that has not been attached long
  enough to fill it.
- The ratio itself is `(N + phaseAllowance) / M` -- the allowance is added to
  the delivered count, not to `M` (though the reason string still names the
  raw, un-lifted `N`): the client's report timer runs at an arbitrary phase
  against the server's ticker, and `phaseAllowance` forgives the resulting
  edge error. It is `max(2, |newest tick's landed rate - oldest paired
  produced rate|)` -- the newer term is the production rate of the tick the
  window's newest report landed on, the older is the production rate the
  window's oldest report was paired with (see `phaseAllowance` in
  `health_track.go` for the derivation), floored at `minPhaseAllowance` (2)
  so an idle-panel window spanning a 0-frame report and a 2-frame report does
  not read degraded on a perfectly healthy link.

So a reading like `"delivered 158 of 175 frames"` describes up to five
seconds of reports summed together, not one second, and `M` will not in
general match any single tick's own production count.

### Event (JSON)

One completed gesture, from a viewer's own input or from `POST /api/input`.
Type: `session.Event`, built by `session.EncodeEvent` (type byte `0x06`
followed by the JSON body -- `EncodeEvent`'s own leading byte, not a separate
wrapper).

#### Plain event (tap)

Fixture: `testdata/protocol/event.hex`.

```json
{"seq":1,"t":1727177400000,"source":"viewer","viewer":2,"kind":"tap","from":[0.512,0.734]}
```

| Field | Meaning |
| --- | --- |
| `seq` | monotonically increasing per session, restarts at 1 on every `Open` |
| `t` | unix milliseconds |
| `source` | `"viewer"` or `"agent"` |
| `viewer` | which viewer produced it; omitted for an agent-sourced event |
| `kind` | `tap`, `longPress`, `swipe`, `pinch`, `multiTouch`, `scroll`, `key`, `text`, `button` or `rotate` |
| `from`, `to` | `[x, y]`, fraction of the screen `[0,1]` on each axis, rounded to 3 decimals -- a fraction rather than pixels because a viewer's touch is in stream coordinates and an agent's is in device pixels, and a fraction is the one space both map into |
| `ms` | gesture duration, where applicable |
| `pointers` | pointer count, for `multiTouch` |
| `scale` | zoom, for `pinch`: above 1 spreads, below 1 closes |
| `keycode` | raw Android keycode, for a `key` event with no named button (see [Key](#key) -- this is never redacted) |
| `button` | named button, for a `button` event -- see below |
| `text` | typed text, or the redaction marker -- see below |
| `rotation` | quarter turns, for a `rotate` event; a pointer field because `0` (back to portrait) is a real value distinct from "no rotation" |
| `scroll` | `[h, v]` delta, the same two-float shape as `from`/`to` but unbounded and signed -- not a `[0,1]` fraction |
| `err` | the underlying action's error, when it failed. Set on an agent action (`publishAgentEvent`) and equally on a viewer's own key event that the device rejected (`Session.ViewerKey`); the event still publishes either way, so a failed input is visible on the event log rather than only in the HTTP response or (for a viewer) nowhere at all. |

Fields at their zero value are omitted (`omitempty`), which is why the tap
fixture above carries only `seq`, `t`, `source`, `viewer`, `kind` and
`from`. `testdata/protocol/event-full.hex` pins every optional field's exact
wire name and shape at once, in a combination no single real gesture
produces -- see that fixture's own comment. `TestEventJSONTagsAreExplicit`
(`internal/session/golden_test.go`) separately lists every tag `Event` can
marshal, so a field added later with `omitempty` and a zero value that is
never exercised by a fixture still fails a test rather than going unpinned.

#### Redacted text event

Fixture: `testdata/protocol/event-redacted.hex`.

```json
{"seq":2,"t":1727177400500,"source":"agent","kind":"text","text":"•••","redacted":true,"textLen":8}
```

| Field | Meaning |
| --- | --- |
| `redacted` | `true` when the characters typed were withheld from the wire |
| `textLen` | rune count of what was actually typed, present alongside `redacted` so a viewer can see how much was entered without seeing what it was |

`text` carries the fixed marker `"•••"`, never the characters, whenever
`redacted` is true. The decision is made in `handleInput`
(`internal/httpapi/agent.go`), which calls `redactionForFocusedField` before
typing: redaction fires when some focused node in the current view hierarchy
dump reports `password="true"` -- a dump can carry more than one focused
node, and any of them saying so is enough -- or, fail-closed, when that could
not be positively ruled out at all: no `Inspector`, a dump error, a dump
timeout, or no focused node in the dump. This check exists only for
`POST /api/input {"action":"text"}`; a viewer's own key events are never
redacted (see [Key](#key)).

#### Button event

Fixture: `testdata/protocol/event-button.hex`.

```json
{"seq":3,"t":1727177401000,"source":"viewer","viewer":2,"kind":"button","button":"home"}
```

`kind:"button"` names a hardware or software button rather than carrying a
raw keycode, from either of two sources:

- a viewer's own key-up whose keycode names a button
  (`Session.ViewerKey` -> `target.ButtonForKeycode`);
- an agent's `POST /api/input {"action":"button", ...}`, which publishes the
  same shape with `source:"agent"`.

Buttons are named on the wire, not keycoded, so a non-Android target can map
`"home"` to its own input event instead of an Android-specific number. The
Android table (`internal/target/rotate.go`) names `home`, `back`, `recents`
and `lock`; `appSwitcher` is deliberately absent from it, since that is
iOS's name for what Android calls `recents`, and an Android target asked for
it gets a 400 naming the buttons it actually has rather than a silent guess.

## Replay: `GET /api/events`

A viewer that sees a gap in `seq` -- control sends are non-blocking, so a
busy viewer misses some -- or that joins late, fetches what it missed with
`GET /api/events?after=N`. `?target=` is required, the same as every other
agent endpoint (`agentTarget`); its absence is a 400 naming
`GET /api/targets`, not a silent default.

A target that exists but has no session open yet -- nothing has streamed to
it or been driven through `POST /api/input` -- is not an error either: the
handler deliberately builds an empty page,
`{"session":"","oldest":0,"latest":0,"events":[]}`, with `events` an empty
array rather than JSON `null`.

The response is not the event JSON repeated: it wraps in `session.EventPage`.
Fixture: `testdata/protocol/eventpage.json`, built from the three events
above.

```json
{"session":"a1b2c3d4e5f6a7b8","oldest":1,"latest":3,"events":[...]}
```

| Field | Meaning |
| --- | --- |
| `session` | a per-`Session`-*instance* identity (`Session.key`, 8 random bytes hex-encoded), distinct from `sessionId` in [Init](#init-json) -- that one is the target id and is unchanged by a restart, this one is freshly generated by every `Open`, which is exactly what makes it the field to compare when detecting one |
| `oldest`, `latest` | the seq range currently held in the 256-entry replay ring; both `0` when the ring is empty |
| `events` | events with `seq > after`, oldest first |

A poller uses `session` and `latest` to tell a session restart apart from
ordinary catch-up: `seq` restarts at 1 on every `Open`, so a stale `after`
against a new session can read as "caught up" when it is actually far
behind. A changed `session`, or `after > latest`, means exactly that, and the
poller should refetch from `after=0`. `oldest > after+1` means the ring has
wrapped past what was asked for; those events are gone for good.
