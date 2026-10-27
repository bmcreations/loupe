# App map extension

Status: design, nothing built. Companion: `docs/spec-compare.md`.

When an agent walks an app today, what it saw lives only in its own context:
screenshots it took, taps it made, screens it reached. Once the run ends, none
of it can be browsed or reused. This extension records a walk as a map: each
screen once, with a screenshot and its UI tree, and the actions that lead
between screens. Handset shows the map, and `docs/spec-compare.md` can check
every screen on it against its Figma frame.

## Why an extension, and where the line is

The map describes one app, so like hotswap it is a separate binary that reaches
loupe only over HTTP. It also keeps a line `docs/product-direction.md` draws:
loupe is "not an autonomous AI QA product". The extension doesn't decide where
to go. The agent drives, through the `/api/ui` and `/api/input` it already
uses, and the extension records what happened. A map is a record of a walk,
not a test verdict.

## Who drives

**Claude, by default.** Given "map the send flow", Claude reads `/api/ui`, taps
by node, and reports each step to the extension. The request decides how far
the walk goes, which is the part a scripted runner can't do.

**Maestro, as an import.** A Maestro flow is a fixed path that someone already
wrote. The extension can take a finished run's screenshots and commands and
add them as a path. Running Maestro live alongside the extension doesn't work
on Android: Maestro's driver holds the device's one UiAutomation connection,
and `/api/ui` fails while it does (trap 46). A live Maestro walk would get
screenshots but no trees, so no node identity and no fingerprint.

## Telling screens apart

A map is only useful if returning to a screen lands on the same node, and that
depends on how a screen is fingerprinted:

| Fingerprint | Same screen, new content | Different screens |
| --- | --- | --- |
| Screenshot hash | Splits it: every new message makes a new node | Separates them |
| Text in the tree | Splits it the same way | Separates them |
| Tree shape: class and resource id of interactive nodes, no text | Merges it | Can merge two screens built from the same components |
| Composable names from `/api/ui?compose=1` | Merges it (`ChatFlowScreen` stays `ChatFlowScreen`) | Separates them, by name |

Start with tree shape, and when `compose=1` works (a debuggable Compose app),
add the outermost screen-level composable name to the key. An empty list and a
full one may still split, because the empty state draws different nodes. Let
the agent merge or rename nodes by hand when that happens, rather than tuning
the hash further.

iOS has no composable names; its accessibility tree gives tree shape only.

## Recording a step

The agent reports each action, so the extension can take the "after" capture
once the screen settles rather than at whatever moment it happens to look:

- `POST /maps?target=` starts a map, returns `{"id"}`, and records the current
  screen as the root.
- `POST /maps/{id}/steps` takes `{"action": "tap", "node": {"id", "text"},
  "note": "open send"}`. The extension sends the input through
  `/api/input`, screenshots until two in a row match (hotswap's settle rule),
  reads `/api/ui`, fingerprints it, and returns `{"screen", "new": true}`.
  A swipe with `ms` and no movement is a long press; the capture waits out
  the hold. `"screen": "Message menu"` names where the step lands when the
  tree can't tell it apart from the screen under it: an iOS context menu
  whose buttons carry no ids, or a sheet the tree leaves out.
  A name the map already has lands on that screen, so naming the way back
  out of a sheet returns to the screen under it instead of copying it.
- `GET /maps/{id}` returns screens (fingerprint, name, screenshot URL, tree) and
  edges (from, to, action).
- `POST /maps/{id}/screens/{s}` renames or merges a screen.
- `DELETE /maps/{id}/edges?from=&to=` drops a step filed under the wrong
  screen; the screens stay.

Screenshots are full-resolution PNGs, a few hundred KB to over 1 MB each, kept
under `~/Library/Caches/loupe/app-map/<id>/`. A map of 40 screens is tens of
MB, which is fine on disk but too much to send to a viewer at once, so the map
response carries thumbnail URLs.

## Showing it

Handset gets a map view beside Inspect: screens as thumbnails, edges as arrows
labelled with their action. Clicking a screen shows its screenshot, its tree
and the path from the root. The web client can show the same thing as a grid
with the path listed, which is the readable version on a phone.

Handset, the native app, draws it as a flow diagram. The first screen sits at the top and
each row is one move further from it. A screen belongs to the first screen that
reached it, so the branches fan out underneath it, with a parent centred over
its children. A move down one row is a solid arrow labelled with its action
over the screen it opens. A return to the screen that opened this one is a chip
on the screen, since a line for every Back would cross the branches. Any other
move up or across is a dashed curve on the right, a move down past a row a
solid curve on the left, and a move that stays put a small loop.

Core doesn't know the map's shape. An extension registers with
`POST /api/extensions` and clients read it through `/api/ext/{name}/...`
(`docs/agent-api.md`), so Handset and the web client talk only to loupe.
Handset shows the Map tab only when `appmap` is registered.

## Later

- **Take me there.** Replay the path from the root to a screen by its recorded
  node selectors. Selectors break when an app changes, so a replay should stop
  at the first node it can't find and say which.
- **Spec check per screen.** Attach a Figma node id to a screen once, then run
  `docs/spec-compare.md`'s comparison across the whole map.
- **Diff two maps**, for example main against a PR build, to list the screens a
  change touched.

## Open questions

- Is the map per app version or branch, so a PR's map can be compared with
  main's?
- Should Maestro import be in the first version, or wait until someone has a
  flow to import?
