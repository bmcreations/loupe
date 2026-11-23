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
registered one. A client should show an extension's features only while it
is listed. Handset shows its Map tab only while `appmap` is listed.

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

Keep data an extension writes outside the repo. `~/Library/Caches/loupe/<name>/`
is a good default.

## Building one

An extension can be a Go binary with its own `main` package, but nothing
requires it. Any program that speaks HTTP on loopback works.
