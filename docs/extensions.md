# Writing a loupe extension

**Version 0.** This contract may still change, and isn't stable yet.

An extension is a separate program that works with devices through loupe's
HTTP API. It never imports loupe's code and never talks to a device directly.
An extension can do either or both of these:

- **Call loupe** like any other client, using `/api/screenshot`, `/api/ui`,
  `/api/input`, `/api/overlays` and the rest of [agent-api.md](agent-api.md).
- **Serve its own data to clients through loupe.** It registers with loupe,
  and loupe passes `/api/ext/{name}/...` through to it. A client such as the
  web client or Handset then reads the extension's data with loupe's token
  alone, and never needs to know the extension's port or its token.
- **Serve a page people open from the web client.** An extension that
  registers a `page` gets a row in the web client's device picker. See
  [Pages](#pages).
- **Load code into an app as loupe launches it.** An extension that
  registers with `launch` can be named in a launch, and tells loupe what to
  load. See [Launch-time injection](#launch-time-injection).

## Calling loupe

Send loupe's token as `Authorization: Bearer <token>` on every request.
The extensions here read the token from `-token`, then `LOUPE_TOKEN`, then
`~/Library/Caches/loupe/token`, and take loupe's address from `-loupe`
(default `https://localhost:18456`). Loupe's certificate is often
self-signed, so the extensions here skip certificate checks for that one
connection.

When an extension drives a device for an agent, it should forward the
agent's `X-Loupe-Agent` and `X-Loupe-Session` headers unchanged. Loupe
checks the device claim against them, so the claim stays with the agent
that made it. An extension does not hold claims of its own. The claim
rules are in [agent-api.md](agent-api.md).

## Registering

```
POST /api/extensions
{"name": "appmap", "url": "http://127.0.0.1:18494", "label": "App map"}
```

| Field | Rule |
| --- | --- |
| `name` | Lowercase letters, digits and dashes, starting with a letter or digit, at most 32 characters. It becomes the path segment in `/api/ext/{name}/`. |
| `url` | Where the extension listens. `http` or `https` on `127.0.0.1`, `::1` or `localhost`, with no user info, query or fragment. A path is allowed; requests are passed to paths under it. |
| `label` | Optional. A name to show people. Defaults to `name`. |
| `page` | Optional. A path under `url` that the web client opens as the extension's page, such as `/`. It must start with `/`, be at most 200 characters, and contain no `..`, `?`, `#`, backslash or control character, percent-encoded or not. |
| `launch` | Optional. `true` lets a launch name the extension in `with`. See [Launch-time injection](#launch-time-injection). |

The response is the registered entry:

```json
{"name": "appmap", "label": "App map", "path": "/api/ext/appmap/"}
```

With a `page`, the entry also carries it as a path on loupe:

```
POST /api/extensions
{"name": "slipway", "url": "http://127.0.0.1:18500", "label": "Slipway", "page": "/"}

{"name": "slipway", "label": "Slipway", "path": "/api/ext/slipway/", "page": "/api/ext/slipway/"}
```

A body that breaks a rule gets 400 with `{"error": "..."}`. Loupe keeps at
most 32 extensions, and the body may be at most 4 KiB. Fields loupe doesn't
know are ignored, so an extension that sends `page` to an older loupe still
registers, without a page.

**Only loopback URLs are accepted.** Anyone with the token can register, so
loupe will not proxy to any other host.

**A registration lasts 90 seconds.** Register again on a timer well inside
that window: appmap registers every 30 seconds. Registering the same name
again replaces the old entry, so a restarted extension can register again
straight away. An extension that stops without unregistering drops out of
the list once its registration expires.

To unregister on a clean shutdown:

```
DELETE /api/extensions?name=appmap
```

It answers 404 if that name isn't registered.

## Listing

```
GET /api/extensions
{"extensions": [{"name": "appmap", "label": "App map", "path": "/api/ext/appmap/"}]}
```

Entries are sorted by name. An entry has `page` only when the extension
registered one, and `"launch": true` only when it registered that. A client should show an extension's features only while it
is listed. Handset shows its Map tab only while `appmap` is listed.

Agents reach extensions the same way: the `list_extensions` and
`call_extension` MCP tools, or `loupe ext list` and `loupe ext call`. They
list this route and call `/api/ext/{name}/...` with the agent's loupe token,
so an agent needs neither the extension's port nor its own token. See
[mcp.md](mcp.md#tools).

## Pages

The web client lists every extension that has a `page` in its device
picker, in an Extensions group, by label. With none, the group isn't shown.
The picker reads `/api/extensions` when it opens, and again on each targets
poll, about every 5 seconds, while it stays open, so a docked picker on a
wide screen also picks up a registration or an expiry.

Tapping a row opens the page in a sheet over the picker, in an iframe at
the entry's `page` path. Done, Escape, or on a phone a tap outside the
sheet, closes it and removes the frame, so the page stops running.

The page opens in a frame rather than a new tab because a web app added to
an iPhone's Home Screen keeps its own cookies, apart from Safari's. A tab
opened from it would reach loupe without loupe's cookie and get 401. In the
frame, the page is on loupe's origin and its requests carry the viewer's
cookie.

That shapes how a page is written:

- **Use relative URLs.** The page is served from `/api/ext/{name}/`. A URL
  that starts with `/`, or a redirect to one, goes to loupe rather than the
  extension. Write `builds` or `./builds`, not `/builds`.
- **Don't refuse framing.** Loupe sets no `X-Frame-Options` and no CSP
  `frame-ancestors`, and passes the extension's response headers through.
  An extension that sends `X-Frame-Options: DENY` or
  `frame-ancestors 'none'` blocks its own page. `SAMEORIGIN` and `'self'`
  work.
- **Post with `fetch`, not a plain form.** Loupe sends
  `Referrer-Policy: no-referrer` on every response. Under the Fetch
  standard, a browser sends `Origin: null` on a form POST under that policy,
  and loupe refuses a cookie-authenticated request whose `Origin` isn't its
  own. A `fetch` POST from the page sends loupe's origin.
- **Cookies don't reach the extension.** Loupe removes the `Cookie` header
  on the way through, so a cookie the extension sets is stored on loupe's
  origin and never sent back to it. Keep state in the URL or the page.

## What the proxy does

A request to `/api/ext/{name}/rest` goes to `{url}/rest` with the same
method, body and other headers, and the extension's response comes back
unchanged. Loupe never reads or changes what passes through. The client
must know what the extension returns.

On the way through, loupe removes its own credentials: the `Authorization`
header, the `Cookie` header and the `token` query parameter. The extension
never sees loupe's token from a client.

The request's `Host` is the extension's own address from `url`, not
loupe's. Loupe also drops `Forwarded` and `X-Forwarded-*`, so the extension
can't learn loupe's address from a request. `Origin` passes through as the
browser sent it, so a POST from an extension's page arrives with loupe's
origin in `Origin` and the extension's address in `Host`. An extension that
compares the two would refuse its own page.

On the response, loupe adds `Referrer-Policy: no-referrer`, as on every
response it serves. A browser that signed in by pairing may also get a
`Set-Cookie` that renews its `loupe_session`. Every other header is the
extension's.

| Case | Response |
| --- | --- |
| `name` isn't registered | 404 `{"error": "no extension <name>"}` |
| `/api/ext/{name}` with no trailing slash | 301 to `/api/ext/{name}/` |
| The extension doesn't answer | 502 `{"error": "extension <name> is not answering: ..."}`, and the entry is removed until the extension registers again |

## Launch-time injection

An extension registered with `"launch": true` can load its own code into an
app that loupe launches, with no change to the app:

```
POST /api/input?target=<id>
{"action": "launch", "package": "com.example.app", "with": ["myext"]}
```

Before it launches anything, loupe asks each extension in `with`, in order,
what the launch should carry. It POSTs to `<url>/launch-spec` directly, not
through the proxy, with none of loupe's credentials. It follows no redirect
and waits up to 60 seconds for each answer, so an extension can build what it
hands back on its first call.

On an iOS simulator:

```
POST <url>/launch-spec
{"target": "sim:<udid>", "platform": "ios-simulator", "package": "com.example.app",
 "udid": "<udid>", "iosVersion": "27.0"}

{"env": {"DYLD_INSERT_LIBRARIES": "/Users/me/Library/Caches/loupe/myext/loader.dylib",
         "MYEXT_SERVER": "http://127.0.0.1:18500"}}
```

`iosVersion` is the simulator's runtime. Loupe passes each variable to
`simctl launch` as `SIMCTL_CHILD_<KEY>`, and always launches with
`--terminate-running-process`, because an app that is already running would
not load the library. The launch answers `"relaunched": true` when the app was
running. While a camera image is staged, the camera shim comes first in
`DYLD_INSERT_LIBRARIES`, then each extension's libraries in `with` order.

On Android:

```
POST <url>/launch-spec
{"target": "avd:Pixel_8", "platform": "android", "package": "com.example.app",
 "abi": "arm64-v8a", "sdk": 36}

{"agent": {"so": "/Users/me/Library/Caches/loupe/myext/libagent.so",
           "dex": "/Users/me/Library/Caches/loupe/myext/runtime.dex",
           "options": {"runtime": "@dex", "server": "local:myext"}},
 "reverse": "myext"}
```

`abi` is `ro.product.cpu.abi` and `sdk` is `ro.build.version.sdk`. Loupe
launches the app as it would without `with` and waits up to 10 seconds for its
process. Then, for each extension:

- **`reverse`** runs `adb reverse localabstract:<reverse> tcp:<port>`, where
  the port is the one in the extension's registered `url`. It runs on every
  launch, because an adb restart or a reboot drops it. A reverse belongs to
  the device, not the extension: a later launch with another extension that
  asks for the same name points the socket at that extension instead.
- **`agent`** needs a debuggable app. If `run-as` refuses the app, the launch
  answers 400 `<package> is not debuggable`, and the app is left running
  without the agent. Loupe pushes the files to
  `/data/local/tmp/loupe-ext/<name>.so` and `<name>.dex`, deletes the app's
  old copies, copies them into the app's `code_cache/loupe-ext/`, and makes the
  dex read-only. It then runs `am attach-agent` with the options as
  `key=value` pairs, sorted by key and joined by commas. `@dex` in a value
  stands for the device path of the copied dex.
- `am attach-agent` doesn't report an agent that fails to load, so loupe
  waits up to 3 seconds for the `.so` to show in the process's memory maps,
  and fails the launch if it doesn't. Logcat says why.
- An agent loupe already attached to the same process is not attached again.
  The launch reports `"attached": "already"` for it. Loupe checks the
  process's memory maps for the `.so` before it attaches, so this holds
  after loupe restarts too.

The launch answers with one entry per extension:

```json
{"ok": true, "with": [{"name": "myext", "pid": 4321, "attached": true}]}
```

### What loupe checks

Loupe checks every answer before it launches anything. An answer that fails
a check fails the launch with 502 `{"error": "extension <name>: ..."}`, and so
does one that isn't a 2xx, which repeats up to 512 bytes of its body. Loupe
never falls back to a launch without that extension.

| Field | Rule |
| --- | --- |
| The answer | At most 64 KiB. A simulator takes `env`; Android takes `agent`, `reverse` or both. An answer with nothing for the platform is an error. |
| `env` keys | `^[A-Z][A-Z0-9_]{0,63}$`, at most 16. `LOUPE_CAMERA_FILE` is loupe's own. Two extensions in one launch can't set the same key, except `DYLD_INSERT_LIBRARIES`. |
| `env` values | At most 1024 bytes, with no NUL or newline. |
| `DYLD_INSERT_LIBRARIES` | Each `:`-separated part is an absolute path to a regular file on this Mac. |
| `agent.so`, `agent.dex` | Both required, each an absolute path to a regular file on this Mac. |
| `agent.options` keys | `^[a-z][a-z0-9_]{0,31}$` |
| `agent.options` values | `^[A-Za-z0-9._:/@-]{1,200}$`: no comma, `=`, quote, space or `;`. |
| `reverse` | `^[a-z][a-z0-9-]{0,31}$`. Two extensions in one launch can't ask for the same name. |

The device paths and the reverse's port come from loupe, never from the
answer, and loupe builds the options string itself.

Only an iOS simulator and an Android device or emulator on this Mac launch
with extensions. A physical iPhone answers 400 `launch injection is
simulator-only`, and a peer's target answers 501.

## Security model

Requests that come through the proxy carry no credentials, so **an
extension cannot tell who sent them.** Loupe checked the token before
passing the request on. Anything else on the machine can also reach the
extension's port directly, with no token at all.

That leads to three rules:

- Listen on loopback only (`127.0.0.1:<port>`), never on all interfaces.
- Treat every request as coming from someone who holds loupe's token.
  Don't serve anything through the proxy that such a person shouldn't have.
- For anything that touches a device, call loupe with the caller's
  `X-Loupe-Agent` and `X-Loupe-Session` headers, as above, so loupe's claim
  check still applies.

**An extension's page runs as the person viewing it.** The page is served
on loupe's origin, so the browser attaches the viewer's loupe cookie to
every request the page makes there. Its script can call any `/api` route as
the viewer, reach into the web client around its frame, and read the
`loupe_token` cookie, which isn't HttpOnly because the web client puts the
token on the `/wt` URL itself. A paired browser's `loupe_session` cookie is
HttpOnly, so a page can't read it, but the cookie still rides on the page's
requests, and a session has the token's full authority.

Registering already takes the token, so this gives an extension's page the
reach its process already has. It also means an XSS bug in an extension's
page is an XSS bug in loupe. Escape anything the page renders that came
from a device, a build or a request.

**A launch extension decides what code an app loads.** Anyone with the
token can register an extension with `launch` and answer `/launch-spec`, so a
token holder can load any file on this Mac into a simulator app, or into a
debuggable Android app as a JVMTI agent. The token already allows as much
through `/api/install`, which installs any build. The checks above keep the
answer out of the device's shell, the device paths and the reverse's port;
they don't vet the code.

Keep data an extension writes outside the repo. `~/Library/Caches/loupe/<name>/`
is a good default.

## Building one

An extension can be a Go binary with its own `main` package, but nothing
requires it. Any program that speaks HTTP on loopback works.
