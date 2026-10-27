# Spec comparison extension

Status: design, nothing built. Companion: `docs/app-map.md`.

An agent that changes a screen has no way to check the result against the
design except by looking at a screenshot and a Figma frame side by side and
judging. This extension makes that a call: post a reference image for a
target, get back a score, a diff image, and the regions that differ, each tied
to the `/api/ui` node under it. Handset and the web client show the same
reference as an overlay on the live video, so a person can check it too.

## Why an extension, not core

loupe is a general device viewer. Comparing against a spec is about one app's
design, so it follows the hotswap model (`docs/hotswap.md`): its own binary,
nothing imported from `internal/`, and only HTTP to loupe (`/api/screenshot`,
`/api/ui`). A user who never compares against Figma never runs it.

Core needs one addition, shared with `docs/app-map.md`: a way for an extension
to put something in front of the viewer. See "What core adds" below.

## Getting the reference

Two ways in, and the first is enough to start:

1. **The agent brings it.** Claude already reaches Figma through its own Figma
   MCP (`get_screenshot` on a frame's node id) and posts the PNG to the
   extension. The extension never holds Figma credentials.
2. **The extension fetches it.** With a Figma personal access token, the
   extension calls Figma's images API itself, so a person can compare from
   Handset with no agent in the loop. This puts a token in another config file.

A reference records the Figma file key and node id it came from, so a diff can
name the frame it was checked against.

## Lining the two images up

The diff itself is easy. Getting the two images into the same space is where
this gets hard:

- **Scale.** A Figma iPhone frame is in points (393×852 for a recent iPhone); a
  device screenshot is in pixels (1080×2400 on the Pixel 7a). Scale the
  reference to the screenshot's width. The aspect ratios rarely match, so the
  heights won't either.
- **System bars.** The status bar and navigation bar on the device show the real
  time, battery and signal; Figma shows a mock. Mask both bars, using the
  insets the target reports, and anchor the reference below the status bar.
- **Live content.** Times, avatars, balances and list contents change on every
  run. A reference can carry masks for those regions, set once per frame.

Expect a first version to need masks on most screens. If the masks make it
unusable, the layout comparison below is the fallback, not more tuning.

## Two ways to compare

**Pixels.** Compare the masked images in blocks (for example 16×16 px), with a
per-block threshold so antialiasing and compression noise don't count. Merge
neighbouring failing blocks into regions. Each region gets the deepest
`/api/ui` node whose bounds contain it, so the result reads "the node
`send_contact_row` differs" rather than "pixels 120–480 differ".

**Layout.** Figma's node tree gives every layer's box. Match layers to `/api/ui`
nodes by text, content description or name, then compare positions and sizes:
"the Send button is 6 px lower and 12 px narrower than spec". This ignores
colour and imagery but tolerates live content, and it gives an agent something
it can act on directly. It needs the Figma tree, so it needs the second way of
getting the reference, or an agent that posts the tree alongside the image.

Build pixels first. It works on any app, and the layout match can reuse its
region-to-node mapping.

## API sketch

The extension serves these; the port follows hotswap's convention (a default
like 18493).

- `POST /references?target=` takes a PNG plus `{"figma": {"file", "node"},
  "masks": [[x1,y1,x2,y2]...]}` and returns `{"id"}`.
- `POST /compare?target=&reference=` takes a fresh `/api/screenshot` and
  `/api/ui`, and returns `{"score", "diff": "<url>", "regions":
  [{"bounds", "delta", "node": {"id", "text", "class"}}]}`. The score is the
  failing share of unmasked area.
- `GET /references?target=` lists them, so a client can offer them.

An agent calls `/compare` after each change, the same way hotswap returns a
screenshot after each patch.

## What core adds

Both extensions need to show something on or beside a target in Handset and the
web client. Rather than each client knowing about each extension, loupe gains a
small generic surface:

- `POST /api/overlays?target=` from an extension: an image, its placement in
  device pixels, a default opacity, and optional boxes with labels. loupe keeps
  it in memory and tells viewers through a session message, the way MsgCursor
  carries agent pointing today.
- Clients draw it over the video with an opacity slider, a swipe divider, and a
  difference blend. Handset already maps device pixels into the zoomed video for
  the inspect overlay, so placement reuses that code.

Clients then talk only to loupe, with its token and TLS, never to the
extension's port. The overlay doesn't need to know it came from Figma.

## Open questions

- Does Figma access go through the agent only, or should the extension hold a
  token so a person can compare without Claude?
- Should a failed comparison feed the PR feedback item in
  `docs/product-direction.md` (roadmap item 9), posting the diff to the PR?
- Are references kept per branch, so a PR's spec can differ from main's?
