# Writing a loupe extension

**Version 0.** This contract may still change, and isn't stable yet.

An extension is a separate program that works with devices through loupe's
HTTP API. It never imports loupe's code and never talks to a device directly.
`extensions/hotswap`, `extensions/speccompare` and `extensions/appmap` are the
three in this repo, and each one is a working example.

An extension can do either or both of these:

- **Call loupe** like any other client, using `/api/screenshot`, `/api/ui`,
  `/api/input`, `/api/overlays` and the rest of [agent-api.md](agent-api.md).
- **Serve its own data to clients through loupe.** It registers with loupe,
  and loupe passes `/api/ext/{name}/...` through to it. A client such as the
  web client or Handset then reads the extension's data with loupe's token
  alone, and never needs to know the extension's port or its token.

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

The response is the registered entry:

```json
{"name": "appmap", "label": "App map", "path": "/api/ext/appmap/"}
```

A body that breaks a rule gets 400 with `{"error": "..."}`. Loupe keeps at
most 32 extensions, and the body may be at most 4 KiB.

**Only loopback URLs are accepted.** Anyone with the token can register.
If any host were allowed, the token would let them reach any other machine
through loupe.

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

Entries are sorted by name. A client should show an extension's features
only while it is listed. Handset shows its Map tab only while `appmap` is
listed.

## What the proxy does

A request to `/api/ext/{name}/rest` goes to `{url}/rest` with the same
method, body and other headers, and the extension's response comes back
unchanged. Loupe never reads or changes what passes through. The client
must know what the extension returns.

On the way through, loupe removes its own credentials: the `Authorization`
header, the `Cookie` header and the `token` query parameter. The extension
never sees loupe's token from a client.

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

Keep data an extension writes outside the repo. The extensions here use
`~/Library/Caches/loupe/<name>/`, which can be changed with `-data-dir`.

## Building one

The extensions here are Go binaries with their own `main` package under
`extensions/<name>/`, built with `make <name>` into `.build/<name>`. That
layout is a convention, not part of the contract. Any program that speaks
HTTP on loopback works.

[app-map.md](app-map.md) and [spec-compare.md](spec-compare.md) describe two
extensions end to end. The registry is tested in
`internal/httpapi/extension_test.go`.
