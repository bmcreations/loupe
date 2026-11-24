# Roadmap

What to take next, ranked within three groups. Each entry gives why it matters,
rough size, dependencies and a first step. The issue is the source of truth for
detail. This page is [#114](https://github.com/bmcreations/loupe/issues/114), the roadmap itself, not work to do.

Sizes are rough: S is under a day, M is a few days, L is a week or more. Where
an issue doesn't say enough to size it, the entry says so. As of this writing no
open pull request addresses any item below (the open PRs cover the AVD
microphone route and its gRPC boot flag, the adb touch log read, and a probes
skill edit).

## Ready now

Nothing here waits on a decision or special hardware.

1. **[#99](https://github.com/bmcreations/loupe/issues/99) A lapsed claim lets the former holder's input through without telling it**
   - Why: an agent that waits past the 10-minute lease on a build or a person comes back driving an unclaimed device and doesn't know it.
   - Size: S to M. The change is in `refusal` and the claim table in `internal/httpapi/claims.go`; the new error code needs an entry in `docs/agent-api.md` and `docs/api.schema.json`.
   - Depends on: none.
   - First step: remember a lapsed holder for a window in `claims.go` and answer its next input with its own error code, as the issue proposes. How long to remember is open.

2. **[#108](https://github.com/bmcreations/loupe/issues/108) A long recording reports bytes written but leaves no MP4**
   - Why: a 9-minute recording reported 22,992,221 bytes and then 404'd on download, so the footage was lost silently.
   - Size: unknown until reproduced. The report hasn't been reproduced, and the cause could be anywhere from a temp-file rename to a cleanup or a rotation.
   - Depends on: none.
   - First step: record for longer than a few minutes on a device and find where the file is removed or never renamed (`docs/recording.md` describes the MP4 writer).

3. **[#110](https://github.com/bmcreations/loupe/issues/110) `submit_feedback` doesn't say `target` must be a target id**
   - Why: two agents sent free text and had to resend.
   - Size: S. A field description and a 400 message.
   - Depends on: none.
   - First step: edit the `target` field description in `internal/mcpserver/tools.go` and the 400 text.

4. **[#112](https://github.com/bmcreations/loupe/issues/112) loupe doesn't build for Windows, but `make dist` ships a Windows binary**
   - Why: the shipped `loupe-windows-amd64.exe` comes from a build that fails, and `docs/getting-started.md` says it works.
   - Size: S to drop Windows from `make dist` and the doc; M to add build tags, split the Unix-only process code and keep a `GOOS=windows go build ./...` check. Running on Windows is a separate, larger piece the issue defers.
   - Depends on: none.
   - First step: run `GOOS=windows go build ./...` and pick between dropping the target and fixing it. The issue lists both; the first is the smaller change.

5. **[#107](https://github.com/bmcreations/loupe/issues/107) `/api/install` can't downgrade or uninstall an app**
   - Why: an agent can't put an older branch build on a device with a newer one without `adb`, which bypasses claims and the event log. Two agent reports hit it.
   - Size: M. Two routes or options, both behind the claim check and the package pattern check.
   - Depends on: none.
   - First step: add `downgrade=1` to `/api/install` (`-r -d`) and put the choice in the 422 body.

6. **[#106](https://github.com/bmcreations/loupe/issues/106) Agents can't list or call extensions from MCP or the CLI**
   - Why: every extension has to be wired into each agent by hand, and calling one directly bypasses loupe's token.
   - Size: M. Two MCP tools and two CLI commands, modelled on `apply_preset`.
   - Depends on: none. It reads the contract in `docs/extensions.md`.
   - First step: add `list_extensions` to `internal/mcpserver/tools.go`.

7. **[#109](https://github.com/bmcreations/loupe/issues/109) No route to read a debuggable app's private files**
   - Why: agents fall back to `adb exec-out run-as`, which bypasses claims and the audit trail.
   - Size: M. A new read route with `run-as`, plus path containment to the app's data directory, which is the part to get right.
   - Depends on: none.
   - First step: write the path-containment check and its tests before the route.

8. **[#100](https://github.com/bmcreations/loupe/issues/100) No route to delete a flag**
   - Why: the only way to clear flags is restarting loupe, which gets in the way when resetting a device for a demo.
   - Size: S. `DELETE /api/flags` with `?id=` or `?target=`.
   - Depends on: none.
   - First step: add the method to the route at `internal/httpapi/server.go:323` and document it in `docs/flags.md`.

## Needs a decision

1. **[#113](https://github.com/bmcreations/loupe/issues/113) Scope tokens per holder**
   - Why: everyone with the shared token can drive every target, the trace can't tell holders apart, and an extension can't tell who called it. That blocks giving anyone view-only access.
   - Size: L. It touches auth, the trace, the extension proxy and pairing; the issue gives no estimate.
   - Depends on: none.
   - Decision: which scopes exist (the issue names view-only, drive, admin and per-extension) and how pairing issues a scoped session.
   - First step: write the scopes against the routes in `docs/agent-api.md`.

2. **[#115](https://github.com/bmcreations/loupe/issues/115) Design exploration for non-technical people, as an extension**
   - Why: a designer or PM could try an app on a real device without a dev setup. The issue marks it low priority.
   - Size: L, and it is not sized in the issue. It needs a place for extensions in the web client and presets from the web UI.
   - Depends on: #113, so this person can't drive every device.
   - Decision: whether spec-compare can run without Claude, with the extension holding its own Figma token.
   - First step: settle that decision. It stays an extension, not loupe core.

## Needs hardware

1. **[#111](https://github.com/bmcreations/loupe/issues/111) Run loupe on a Linux host, Android first**
   - Why: it is the smallest step past macOS for hosting devices on a CI runner or cloud VM. loupe builds for linux/amd64 but has never run there.
   - Size: unknown until run. The issue lists four known macOS-only pieces (`ps -axww`, `lsof`, no keep-awake, launchd only), and running it may turn up more.
   - Depends on: none.
   - Hardware: a Linux machine with adb on PATH and a USB device or AVD.
   - First step: run `loupe -no-simulators -no-ios-devices -addr 127.0.0.1:18456` there and fix what breaks. Then add a systemd unit and a Linux row to `docs/getting-started.md`.

2. **[#116](https://github.com/bmcreations/loupe/issues/116) Spike: serve an iOS simulator from a Linux host**
   - Why: it would put iOS simulators on a Linux host. The candidates the issue found are unconfirmed, and none is known to expose what simcap needs.
   - Size: M for the spike. Any resulting work is unsized.
   - Depends on: #111 for the server side.
   - Hardware: a Linux host able to run the candidate simulator.
   - First step: check whether the leading candidate exposes frames, touch and keyboard input, and an accessibility tree (`docs/ios.md` describes what simcap provides today).

## Carried over without an issue

Two items from the old list have no issue yet: the simulator rotation spike
(the "iOS simulators have no rotation" entry in Known gaps, `docs/architecture.md`)
and pull-based PR previews, whose planned `docs/pr-preview-pull.md` was never
written. Open an issue for each before ranking it here.
