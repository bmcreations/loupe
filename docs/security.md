# Security

Reaching loupe's port is enough to drive every device it serves: tap, type,
install an APK, read the clipboard. Everything here follows from that. The
mechanisms are described in full under Auth in
[architecture.md](architecture.md); this page says what they guarantee and
what they don't, then documents pairing.

## What protects a server

- **Every request needs a credential.** That is the shared token, or a
  paired session. The token is read from `-token`, then `LOUPE_TOKEN`, then
  `~/Library/Caches/loupe/token`, and generated into that file on first run.
  `-no-auth` turns this off and says so at startup. Use it only on a
  loopback address.
- **Plain HTTP is loopback only.** Without `-tls-cert` and `-tls-key` the
  server refuses any `-addr` that is not `127.0.0.1`, `::1` or `localhost`,
  because the token, the cookie, the stream and clipboard text would cross
  the network readable. `-insecure-http` overrides that and prints a warning.
- **The cookie is `SameSite=Strict`**, and a request that authenticates by
  cookie alone must also pass an Origin check. That stops a page on another
  port of the same host from driving a device through the viewer's cookie.
- **The token stays out of logs and history.** A `?token=` link redirects to
  strip it, every response sends `Referrer-Policy: no-referrer`, and traces
  redact `token=`, `code=` and `ticket=`. Under launchd the startup banner
  leaves the token and its QR code out of `loupe-run.log`. The run log and the
  trace are created owner-only.
- **Device shell input is checked.** Anything that reaches a device shell
  goes through a pattern check and `shellQuote`.
- **Agents and people do not fight over a device.** A person's input holds
  a target against agents, and a person can mark a device hands off. See
  Claims and the human hold in [agent-api.md](agent-api.md).

## What it does not do

- **There are no scopes.** The token and every paired session can do
  everything, on every target. A session can be revoked on its own, but
  while it is live it carries the master token's full authority.
- **A link with no token does not grant access.** The GitHub preview action
  (`action/README.md`) posts `https://<server>/?target=<id>` without the
  token, so it opens only in a browser that already holds loupe's cookie.
  Scoped share links don't exist yet.
- **Extensions see no credentials.** loupe checks the token and then
  proxies `/api/ext/{name}/...` to the extension without it, so the
  extension must listen on loopback and treat every request as coming from
  a token holder. See Security model in [extensions.md](extensions.md).
- **A paired browser cannot open `/wt` yet.** The web client still builds
  the WebTransport URL from the master token cookie. Pairing currently
  serves native clients, bearer tokens and peer servers.

Run it over TLS on a network you control. A tailnet works well: the
supervisor issues its certificate with `tailscale cert`. See
[getting-started.md](getting-started.md).

## Pairing and sessions


A paired device holds a session instead of the master token, so one device
can be signed out without re-keying every other. Sessions are added beside
the master token, so the master token, the `loupe_token` cookie and `?token=`
still work exactly as before.

- `POST /api/pairings {"target": [...], "label", "canPair"}` mints a
  one-time code, `XXXX-XXXX-XXXX` from a 32-letter alphabet with no `0 O 1 I`,
  good for 5 minutes. 201 `{"id", "code", "expiresAt"}`. Only the master
  token or a session paired with `canPair` may call it (403 otherwise), and
  the caller decides whether the redeemed session gets `canPair`. The code
  records who minted it as `parent`: a session id, or `master`.
- `GET /api/pairings` lists pending codes without the code itself;
  `DELETE /api/pairings/{id}` withdraws one. Same 403 rule.
- `POST /api/pair {"code", "kind": "browser"|"bearer"|"peer", "label"}` is
  the one write route that takes no credential. `browser` answers 204 and
  sets `loupe_session`; `bearer` and `peer` answer 201
  `{"id", "token", "expiresAt"}` with an `lsn_` token shown once. Dashes,
  spaces and case in the code are ignored. An unknown, expired, spent or
  orphaned code is the same 401. Failures are limited to 5 a minute per
  remote address, 429 past that. There is no cap across addresses, so junk
  from elsewhere cannot keep pairing shut for everyone. A `browser`
  exchange runs the Origin check, so a page elsewhere cannot pair a
  visitor's browser into its own session.
- `GET /api/auth` answers any caller with `{"method":
  "master"|"session"|"none", "session", "pairing", "canPair"}`. A server
  older than this answers 404, which is how a client tells them apart.
  `pairing` is false when `sessions.json` could not be read at startup;
  every pairing route then answers 404 and only the master token works.
- `GET /api/sessions` lists sessions without their hashes: every session for
  the master token, and for a session only itself and the sessions it
  paired. The caller's own row has `"current": true`. `DELETE /api/sessions/{id}` revokes one;
  `DELETE /api/sessions?others=1` revokes every session with the master
  token, and with a session only the ones it paired. A session may revoke
  only itself and the sessions it paired, down the line, and gets 403 for
  any other, so a stolen phone session cannot sign out every other device.
  Both answer `{"revoked": [ids]}`.

**Revoking cascades.** A revoke also revokes every session paired through
a code the revoked one minted, and so on down, and drops its pending codes.
A code whose minter is no longer a live `canPair` session will not redeem.
Without that, a stolen `canPair` session could pair a second device and
survive its own revocation. A revoke does not yet close a stream already
open (spec Task 4).

**Sessions slide.** One expires 30 days after its last use. Use is noted in
memory and written to disk at most every 5 minutes per session. Revoked
rows stay 7 days so `loupe sessions` can show them, then go.

**`sessions.json` sits beside `config.json`** (by default
`~/Library/Application Support/loupe/`; a spare server with `-config` keeps
its own), 0600, written whole through a temp file and a
rename. It holds SHA-256 hashes of tokens and codes, never the secrets, so
a lookup is a map index on the hash and needs no constant-time compare. A
file that does not parse is left alone and pairing is off until it is
fixed, rather than overwritten and signing every device out.

**From the shell**, with the master token from the token file as `loupe mcp`
reads it:

```sh
loupe pair -label "phone"                 # prints a link, a QR code, the code and its expiry
loupe pair -can-pair -label "desktop"     # the paired device may pair others
loupe pair -bearer -label "mcp on ci"     # mints and redeems; prints only the lsn_ token on stdout
loupe sessions                            # table of sessions
loupe sessions revoke <id>                # one, and its descendants
loupe sessions revoke -all
```

Each takes `-url` (default `https://localhost:18456`) and `-token`.

