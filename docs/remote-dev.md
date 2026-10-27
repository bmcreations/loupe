# Remote dev: an agent gets a clean device and proves its change on it

Status: design, not started.

Shopify's Remote Dev (shopify.engineering/remote-dev) gives every agent
session a ready Mac with the app built and a simulator or emulator on it, and
streams those devices to a browser where people can watch and take over. The
agent reproduces the bug, fixes it, and records the app before and after.

loupe already covers the streaming and most of the driving: `/api/ui`,
`/api/input`, `/api/screenshot`, `/api/record` with contact sheets, claims,
hands off, and a person's hold over agents. What is missing is the part
between an agent starting and an agent posting its proof:

- An agent has no way to ask for a device that is clean and its own. Today
  it claims whichever target it finds and inherits whatever the last user
  left on it.
- An agent cannot open a deep link, which is how most bugs are reproduced.
- Nothing can be shared outside the one token. A recording or a live
  session cannot go in a PR comment without handing out the key to every
  device.
- Every agent learns the API from scratch. The rules for driving loupe live
  in a personal `CLAUDE.md`, not anywhere an agent in another repo would
  find them.

This doc covers those four, in the order they should land. It does not
cover a fleet of hosts, separate build machines or a chat front end; see
"Not in scope".

## 1. Deep links

`POST /api/input {"action":"openURL","url":"myapp://orders/42"}`, built
in #111.

| Target | Command | Nothing handles it |
| --- | --- | --- |
| Android | `am start -W -a android.intent.action.VIEW -d <url>` | exits 1, "unable to resolve Intent": 422 `noHandler` |
| Simulator | `xcrun simctl openurl <udid> <url>` | exits 115: 422 `noHandler` |
| Physical iOS | none: 501 | |

A physical iPhone has no "whatever handles this URL" route: devicectl
opens a URL only as the payload of launching a named app. An optional
`package` to pin the handler would make that route possible, and would
pass a component to `am start`; neither is built.

It goes through arbitration the way `launch` does: a person's hold answers
409 `humanHeld`, a claim or hands off refuses first, and the URL is checked
before arbitration so a bad one is a 400 rather than a 409 that no retry
would fix. The check requires a scheme and no host (`mailto:` has none),
and refuses whitespace, control bytes and anything over 2048 bytes. The URL
reaches the device shell only through `shellQuote`, like `input text`.
Traced as `kind:"agent"`, `action:"openURL"` with `scheme`, `host` and
`urlLen` only, since a path or query can carry a token.

## 2. Device leases

An agent asks for a device of a kind and gets one nobody else is using, in a
known state, and gives it back when it is done:

```
POST   /api/leases {"platform":"android","os":"android-36","device":"pixel_9"}
  -> 201 {"id":"lease-…","target":"avd:pool-android-36-pixel_9-2","claim":{…}}
DELETE /api/leases/{id}
  -> 204, after the device is reset
```

### Only pool devices are leased

A lease resets the device on release, which throws away everything on it.
That is fine for a device made for agents and a disaster for the person's
own `Pixel_10` or `acmechat_blockstore`. So leases only ever touch devices
whose name starts `pool-`, created by the lease path itself. The person's
devices stay what they are today: claimable, never reset.

### A lease is a claim that resets on release

The lease takes the claim on the target in the caller's name
(`X-Loupe-Agent`, `X-Loupe-Session`), so every rule that keeps two agents
apart already applies. Hands off still refuses it. The claim's 10-minute
lapse (`claimLease`) is the lease's lapse too: an agent that dies mid-task
frees the device within 10 minutes of its last call, and the reset runs
then rather than on `DELETE`.

### Reset is mostly free on Android

Every boot from `loupe-warm` already discards what the last session changed,
because the emulator runs with `-no-snapshot-save`. So on Android a release
is a shutdown, and the next lease boots from the snapshot: 1.7-1.9s to boot
and 3.3-3.8s to first frame on `loupe_bench`, on a quiet Mac. A pool AVD is
headless, so if a lease is held without being used, the idle pause and the
30-minute idle shutdown still apply to it.

The first boot of a new pool AVD has no snapshot: 19.7s, plus 1.2s to save
`loupe-warm`. So the pool creates and cold-boots one device ahead of demand
for each kind it has been asked for, and a lease that finds no idle device
of its kind waits for a cold boot rather than failing. Each pool AVD costs
1.7-2GB of snapshot on top of its userdata, so the pool has a cap
(`poolMax`, per kind and in total) and deletes the least recently leased
device when it needs room.

### Reset on a simulator is not measured

Simulators keep their state across a shutdown. Two routes, neither run yet:

- `simctl erase` on release. It returns the simulator to a fresh install,
  which also removes the app the agent was testing, and the next boot is a
  first boot.
- A golden simulator per kind, booted once and left at the Home Screen, and
  `simctl clone` from it per lease, deleting the clone on release.

Which one is faster to a first frame decides it, and that is the first thing
to measure before any simulator code.

### Physical devices are never leased

There is no reset for a real phone, and they are few. They stay claimable as
today.

### What it does not solve

Builds still share one Mac's CPU. Claude Code worktrees give each agent its
own checkout, but three agents running `gradle assembleDebug` at once will be
slow. A lease does not change that; more hosts would, and they are out of
scope here.

## 3. Share links

One token opens everything: every target, `/api/input`, shutdown and delete.
That is why nothing from loupe can go in a PR comment today. A share is a
second, narrow credential:

```
POST /api/shares {"target":"avd:pool-…","scope":"view","ttlSec":86400}
POST /api/shares {"recording":"<id>","ttlSec":604800}
  -> 201 {"id":"…","url":"https://<host>:18456/?share=…","expires":"…"}
DELETE /api/shares/{id}
```

- **`view`** opens one target's stream and nothing else: `/ws` and `/wt` for
  that target, `/api/frame` for its bezel, and the static client. No
  `/api/input`, no picker, no other target.
- **`drive`** adds viewer input on that target, arbitrated as any viewer's.
  It still cannot shut down, delete or install.
- **A recording share** serves one recording's `.mp4`, its metadata and its
  contact sheet, so a PR comment can link to the before and after.

The share token rides the same paths the main token does, including
`?share=` on `/wt`, which sends no cookies (the reason `/ws` and `/wt` are
exempt from the token-stripping redirect, trap 11). Shares are saved beside
`handsoff.json` so a `make build` does not break every link. Each request
made with one is traced with the share's id, which also starts to close the
known gap that the trace cannot tell two holders of the token apart.

A share URL is still only reachable where the Mac is: on the tailnet today.
A reviewer outside it gets a link that does not load. Recordings are the
exception worth solving first, since they are files: the skill can upload
the MP4 or the contact sheet somewhere the PR can show it, and use the share
link only when the reader is on the tailnet. GitHub's REST API has no way to
attach an image to a comment, so "somewhere" is an open question.

## 4. The loupe skill

A skill, shipped in this repo and installed into `~/.claude/skills`, that
replaces the Devices section of a personal `CLAUDE.md` and makes the proof
loop the default:

1. **Get a device.** Lease one of the kind the change targets, or claim a
   named one when the user names it.
2. **Install.** `/api/install?launch=1` with the build, `allowTest=1` for a
   build from Android Studio.
3. **Reproduce and record before.** `/api/record`, then `openURL` or
   `launch`, `/api/ui?interactive=1` to find the node, taps by its `center`.
   Never locate a button from the video.
4. **Fix, rebuild, install, record after**, with the same steps.
5. **Prove it.** A contact sheet of each recording from
   `/api/record/{id}/frames`, read by the agent itself to check the after
   shows the fix, then shared or uploaded for the PR.
6. **Give it back.** Release the lease, or the claim.

It also carries the rules agents keep tripping on: send `X-Loupe-Agent` and
`X-Loupe-Session` on every call, treat 409 `claimedBy` and `handsOff` as
"pick another device or ask", wait out `humanHeld` with its `retryAfterMs`,
and treat `touchRejected` as a failed tap rather than retrying it blind.

A thin CLI (`cmd/loupectl`) sits under the skill so it does not spend
context on `curl` flags: it reads the token from
`~/Library/Caches/loupe/token`, sets both headers, and has one subcommand per
step above. Shopify's agents use their CLI the same way, through SSH.

## Measuring it

Every claim, lease, install, input and recording is already a `kind:"agent"`
record with the agent's name. A small report over the trace, per agent and
per day, answers whether the loop is being followed: leases taken and
released, recordings made per lease, refusals by code, and leases that lapsed
rather than being released. That is enough to see which step agents skip
before changing the skill.

## Order

1. Deep links. Small, self-contained, and the skill needs it.
2. Measure simulator reset, then leases: Android first, since its reset
   already exists.
3. The skill and `loupectl`, against leases.
4. Share links, with recordings first.
5. The trace report, once there are a few weeks of leases to read.

Each new route goes in the route table in `internal/httpapi/server.go` and
needs a response entry in `internal/apischema`, or
`TestEveryJSONRouteHasASchemaEntry` fails.

## Not in scope

- **More hosts.** A pool of Macs, each running loupe, with something above
  them that hands out a host per agent. It is where Shopify's cost went and
  it is not needed while one Mac carries the agents. `docs/cloud-emulators.md`
  covers moving Android off the Mac, which is the same question as their
  Linux emulator VMs.
- **Builds on the device host.** Per-agent build machines with warm caches.
- **A chat front end.** Claude Code's own sessions and routines start the
  work; loupe does not need a Slack bot.

## Open questions

- Simulator reset: `erase` or `clone`, decided by measurement.
- Where the skill uploads recordings for a reviewer off the tailnet.
- Whether a lease should hand back an already-booted device when one is idle,
  which costs RAM while it waits, or always boot from the snapshot, which
  costs 2-4 seconds per lease.
- Whether `drive` shares are worth having before there is a person outside
  the tailnet to share with.
