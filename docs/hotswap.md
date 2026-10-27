# Hotswap extension

`extensions/hotswap` applies a code patch to a running debug app in one call, waits for the redraw,
and returns a screenshot plus the UI tree. It is its own binary (`make hotswap` writes
`.build/hotswap`), imports nothing from `internal/`, and reaches loupe only over HTTP
(`/api/screenshot`, `/api/ui?interactive=1`).

It replaces the spikes' `server.py` and serves the routes the debug apps already poll, unchanged:
`GET /seq`, `GET /next` (long poll), `POST /publish`, `GET /patches/<name>`, `POST /report`. No
app-side change is needed. Default port 18492.

**One app per instance.** The protocol carries no device identity, so every app that polls the
port gets every patch. Run one extension, with one build command, per app.

## Run

```sh
make hotswap
.build/hotswap -addr :18492 -build-dir ~/dev/app-repo \
  -build-cmd 'python3 scripts/hotswap/watch.py --once --module :apps:acmechat:shared:chat-ui --src apps/acmechat/shared/chat-ui/src/main'
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-build-cmd` | required | Run with `sh -c` in `-build-dir` |
| `-build-dir` | `.` | The app repo |
| `-loupe` | `https://localhost:18456` | Loupe base URL; certificates are not verified |
| `-token` | `LOUPE_TOKEN`, then `~/Library/Caches/loupe/token` | Loupe bearer token |
| `-data-dir` | `~/Library/Caches/loupe/hotswap` | Patches and screenshots |
| `-build-timeout` | 180s | A cold Gradle daemon takes 20s or more |
| `-report-timeout` | 20s | Wait for the app's `patch` report |
| `-poll-window` | 5s | An app with no `/next` poll this recent is "not polling" |
| `-settle-timeout` | 2.5s | After the report, screenshot until two in a row match or this passes; 0 takes one shot |

`/seq` is seeded from Unix time, so it is unique per process (iOS loads `Live_<seq>` modules) and
an Android app that reads it at launch skips patches from an earlier run.

## Builder contract

The build command runs with `HOTSWAP_SEQ` (this patch's sequence number) and `HOTSWAP_PATCHES`
(the directory the extension serves `/patches/` from). It writes its patch files there and prints
the notice JSON as the last line of stdout: everything the app's `/next` expects except `seq`.
A non-zero exit, a timeout or a last line that is not JSON is a build failure.

## `POST /apply?target=<loupe target id>`

Send `X-Loupe-Agent` and `X-Loupe-Session`; they are passed to loupe, so the agent's claim stays
its own. The extension has no claim logic. One apply runs at a time.

| Status | `code` | Meaning |
| --- | --- | --- |
| 200 | | Applied; `ui` inline, `screenshot.path` and `screenshot.url` (served at `/shots/`), `settle` (`stable`, `shots`, `settleMs`), `changed` |
| 400 | `badRequest` | No `target` |
| 409 | `appNotPolling` | No `/next` poll in `-poll-window`; the build was not run |
| 409 | `busy` | Another apply is running |
| 409 | `needsRebuild` | The app reported `ok:false`; `error` and `report` carry its reason |
| 422 | `buildFailed` | `output` has the compiler output; nothing was published |
| 502 | `loupeError` | The patch is on the device, the screenshot or tree read failed |
| 504 | `noReport` | The app sent no report for this seq |

A 200 carries `seq`, `buildMs`, `applyMs`, `totalMs`, `report` (the app's own fields),
`timings` (`downloadMs`, `redefineMs`, `recomposeMs`, ... when the app reports them) and `build`
(the notice).

`changed` says whether the screen after the apply differs from one taken while the build ran, and is
absent when either shot did not decode. A 200 with `changed: false` means the patch loaded and the
app redrew with the same pixels: the patched code is not on screen, or does not run until something
calls it. The status bar counts, so a clock that ticks during the apply reads as a change.

## Builders

- Android: `scripts/hotswap/watch.py --once` in the app repo. Its baseline and cumulative class
  set persist in `scripts/hotswap/serve/once-state/`. With no state the baseline is the classes
  directory before the compile, which is right only just after `assembleDebug` matches the
  installed APK; the notice carries `warning` when the classes are newer than the APK.
- iOS: `Scripts/hotswap/watch.py --once <identity>` builds the patch for `HOTSWAP_SEQ`. For a
  simulator, set `HOTSWAP_SDK=iphonesimulator` and pass `-` to sign ad hoc; the app must be a
  Debug build for the simulator, with its flags recovered by `flags.sh` from that build's log.

## When the screenshot is taken

The Android app reports a patch only after it has drawn it: it waits for a frame commit with every
`Recomposer` idle, then for 100ms with no commit, so crossfades and Haze's blur have finished
(`recomposeMs` in `timings`). The extension then takes screenshots until two in a row have the same
pixels and reads `/api/ui` after that. Two identical shots alone would not be enough, since the
blank screen left by disposing the composition is also stable.

On acmechat_blockstore, 10 applies with `-settle-timeout 0` (one shot straight after the report)
all showed the patched divider and none was blank. Six with the default each matched on the second
shot, `settleMs` 536-586, with `recomposeMs` 640-760.

The iOS app reports once the patch's dylib is loaded and its views invalidated, which takes
13-14ms; SwiftUI draws on the next frame. On simulator Acmechat (iOS 27.0, ForceUpgradeScreen
patched), 5 applies each matched on the second shot, `settleMs` 961-1050, most of it two
`simctl io screenshot` calls. `buildMs` was 835-866 and each apply took 1.9-2.0s end to end. The
fourth repeated the third's text and read `changed: false`; the other four read `true`.

**On iOS a patched view redraws only if something tells SwiftUI to evaluate it again.** The first
iOS apply returned 200, `stable: true`, with the old screen: handing the hosting controllers their
root view again skips views whose stored properties compare equal, and ForceUpgradeScreen has none.
Bumping the root's identity fixed that and reset every `@State`, navigation path and scroll position
below the root on each apply, so it was dropped. A per-view opt-in (an `@ObservedObject` inside a
`DynamicProperty`, InjectionIII's shape) worked and put an annotation on every view a patch might
touch, so that was dropped too.

The app now marks every value in each hosting view's AttributeGraph out of date, with
`AGGraphInvalidateAllValues`, so every `body` runs again in place and no view declares anything.
None of it is API. The graph is reached through `Mirror` as `_UIHostingView._base.viewGraph.viewGraph`
and then by scanning that object's first words for a CF object whose type ID is `AGGraphGetTypeID()`,
because `Mirror` does not show the field; that layout is iOS 27.0's and may move with any SDK. When
a step misses, the app hands the hosting controllers their root view as before and its report says
`redraw: "fallback"` instead of `"graph"`, which `/apply` returns under `report`.

On simulator Acmechat, with the annotation inert, a zero-input ForceUpgradeScreen body patch read
`changed: true` (`settleMs` 1907); the same patch without the graph call left the old screen. A
row patch on the chat list kept its scroll offset, and one applied inside an open chat kept the
pushed screen, the composer's typed draft and the raised keyboard. Invalidating everything costs
time: `settleMs` was 1.9s against 0.95s for the opt-in on the same screen, and a heavier screen
will cost more.
