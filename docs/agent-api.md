# Agent API

The `/api/*` endpoints agents drive, and how claims and the human hold
share a device between agents and a person. `docs/api.schema.json` is the request and response
schema; `docs/protocol.md` is the session wire protocol the viewers speak.

## Routes

Every route that touches a device takes `?target=`, so a process can drive it while a
human watches the same session.

- `GET /api/ui` returns the view hierarchy from `uiautomator dump`.
  `?interactive=1` prunes layout containers; on a Compose screen that was 6202
  bytes down to 3620. Each node carries `center`, precomputed, because it is
  the one derivation everything needs and leaving it to the caller invites
  rounding disagreements that land a tap on a neighbour.
- `GET /api/ui?compose=1` names nodes after the composables that drew them.
  It only works on a debuggable Android app: loupe attaches a JVMTI agent
  (`internal/composetree`) to the foreground app with `am attach-agent` and
  hosts the Compose inspector that ships inside the app's own `ui-android`
  version, downloaded once from Google's Maven into
  `~/Library/Caches/loupe/compose-inspector/<version>/`. Nodes inside a
  `ComposeView` gain `composable` (`MessageRow`) and `source`
  (`MessageList.kt:661`). The response adds `composables`, the full
  composable tree with bounds, including the wrappers that have no node.
  A node is named only when a composable's bounds equal its own. Each
  node anchors on a composable that carries semantics (`semantics` in
  `composables`) and takes the outermost name between that anchor and the
  one above it. Where uiautomator stacks nodes at one rectangle, a node
  anchors on the lowest such composable that encloses, in the composition,
  what its children anchored on, so a sibling branch at the same rectangle
  (a bottom bar's `AnimatedContent`) cannot take it. On acmechat's chat
  screen the full-screen stack reads three unnamed host Views, then
  `ProvidePermissionChecker`, `SharedTransitionLayout`, `ChatFlowScreen`,
  `FlowConversationScreen`, `OverlayBars`. Empty full-screen overlays and
  nodes clipped by another (their bounds shrink) stay unnamed.
  A failure never fails the request: the plain tree comes back with
  `composeError` saying why (`app is not debuggable`, a target with no
  inspector). It combines with `?interactive=1`. On emulator-5554 with
  acmechat the plain call took 2.27s, the first `compose=1` after a cold
  start 3.82s with the inspector already cached, and later ones about
  2.45s, with the body growing from 13 KB to 63 KB. The first download of
  an inspector version adds its fetch from Maven; that wasn't timed.
- `POST /api/input` takes `{"action":"tap|swipe|text|key", ...}`.
  `move` (`{"x": ..., "y": ...}`) touches nothing: it sends the agent's
  cursor there on every viewer, as a `MsgCursor` (`docs/protocol.md`), so
  a person watching sees where the agent is about to act. It is live only,
  kept out of the event log and `/api/events`, passes the human hold and
  leaves the banner alone, but another agent's claim still refuses it.
  `loupe mcp`'s `tap` and `swipe` send one before they touch. Once an
  agent has been quiet for 2.5s its cursor parks: it dims and pulses where
  it last acted for 20s more before it fades (`PARK_MS` in
  `web/cursor.js`). An agent thinks for seconds between calls, and a
  cursor that vanished each time would read as one that had left.

  A `tap` can name a node instead of a point:
  `{"action":"tap","match":{"text":"Sign in"}}`. `match` takes `id`,
  `text` and `desc`, and a node has to satisfy every field given. `id` is
  a resource-id on Android or an accessibility identifier on iOS, and a
  bare Android name (`send`) matches any package's `:id/send`. `text` and
  `desc` match exactly, or as case-insensitive substrings with
  `"contains": true`. On a simulator a button's label is its `text`. The
  server dumps the hierarchy, taps the node's `center`, sends the cursor
  there first, and answers `{"ok":true,"matched":{...}}` with the node it
  hit, without its children. Nodes with no area and nodes simcap marks
  `occluded` never match, and a match with the same bounds as one already
  found counts once, since uiautomator stacks a Compose node and its host
  at one rectangle with the same label. More than one match is 409
  `ambiguous` with `candidates` and nothing touched; `"index": n` picks
  one in tree order. No match is 404 `noMatch`. `"waitMs"` (at most
  20000) dumps again every 250ms until the node appears, which replaces
  a sleep-and-retry after a navigation. The lookup runs before
  arbitration, as a launch's name check does, so a missing node is a 404
  even while a person holds the target. A body with both `match` and
  `x`/`y` is a 400. Every match tap costs a full dump, about 2.3s on
  emulator-5554 per the `/api/ui` timing above; the tree is not cached,
  because a center read before an app scrolled by itself would land on
  a neighbour. `loupe mcp`'s `tap` takes the same fields as `text`, `id`,
  `desc`, `contains`, `index` and `waitMs`.

  `wait` touches nothing. `{"action":"wait","ms":1500}` pauses (at most
  20000), and `{"action":"wait","match":{"text":"Inbox"}}` dumps until the
  node appears and answers with it, looking for 10s unless the match sets
  `waitMs`. It is answered before arbitration and kept out of the event
  log, like a match lookup, so a person holding the target does not refuse
  it. In a sequence it holds the next step until a screen has loaded.
  On Android a tap or swipe the input dispatcher threw away answers 409
  `code: "touchRejected"` with its reason, since `input` exits 0 either way
  (trap 50).

  `launch` (`{"package": ..., "activity": ...}`) opens an installed app.
  On Android it is `cmd package resolve-activity` for the launcher
  activity, or the named one (qualified or starting with `.`), then
  `am start -W -n`. Not `monkey -p <pkg> -c LAUNCHER 1`: on emulator-5554
  it exited 252 with "No activities found to run" for a package that was
  not installed and for one with no launcher activity alike, and names
  nothing when it works. `pm path` tells those two apart, and they answer
  404 `notInstalled` and 422 `noLaunchActivity`; a named activity that
  does not exist is 404 `noSuchActivity`. `am` exits 1 on that, and older
  devices exit 0 after printing it, so an `Error:` line fails the launch
  whatever the status. An app already in front is a success. The package
  and activity reach the device shell only after a pattern check and
  `shellQuote`. The check runs before arbitration, so a bad name is a 400
  even while a person holds the target, not a 409 no retry would fix. On a simulator `package` is the bundle ID and goes to
  `xcrun simctl launch`; an activity there is a 400. Through a spare
  server on emulator-5554: Settings from the Home Screen in 0.34s, the
  mask sample cold in 1.5s and in front already in 0.10s, and each
  failure above with its code. Each launch is traced as `kind:"agent"`,
  `action:"launch"`, with `package`, `tookMs`, `activity` when named and
  `started`, the component am reports, which for a trampoline is not the
  one asked for. Through a spare server on `loupe-probe`, Settings came
  to the front from the Home Screen in 0.16s and Safari in 0.44s. simctl's
  error for a missing bundle names no cause, so a failed launch asks
  `simctl get_app_container`, which exits 2 for an app that is not there,
  and answers 404 `notInstalled` as Android does. Any other exit leaves
  the launch's own error, a 502. With a camera image staged (see
  `/api/camera`), a phone app the camera shim did not load into (not
  development-signed, or no local identity for its team) is 422
  `noCameraShim`, and a shim that could not be built or signed is the
  502, its error starting "camera shim:".

  `openURL` (`{"url": ...}`) hands a URL to whatever the device has
  registered for it: a deep link, a universal link or an https page. On
  Android it is `am start -W -a android.intent.action.VIEW -d`, the URL
  through `shellQuote`; a URL nothing resolves prints "unable to resolve
  Intent" and exits 1, which answers 422 `noHandler`. On a simulator it is
  `simctl openurl`, which exits 115 (LSApplicationWorkspaceErrorDomain)
  for the same case, also 422 `noHandler`. simctl never checks universal
  links, so an http or https URL an installed app claims goes to that app
  first, through the `simopen` helper, and `started` is its bundle ID (see
  "Universal links on a simulator" in `docs/ios.md`). A URL no app claims,
  or one the helper could not open, goes to `simctl openurl` as before. A physical iPhone answers 501:
  devicectl opens a URL only as the payload of launching a named app.
  `CheckURL` runs before arbitration, as `launch`'s name check does: a
  scheme is required, no host (`mailto:` has none), no whitespace or
  control bytes, at most 2048 bytes, so a bad URL is a 400 under a hold.
  **The URL's path and query never reach the trace or an error**, since a
  deep link carries its token there: the record has `scheme`, `host` and
  `urlLen`, an error names the URL as `scheme://host/…`, and on Android
  `started`, the activity am reports. Through a spare server, on Pixel_10
  (emulator-5556) an https URL opened Chrome in 0.10-0.20s and a scheme
  nothing handles was 422 in 26ms; on `loupe-probe` an https URL opened
  Safari in 0.48-0.54s and the unhandled scheme was 422 in 123ms.

  **An `am start -W` that times out still opened the URL** when it names
  an activity: am prints `Status: timeout` once its wait for the handler
  to draw runs out, and the intent was delivered all the same. That is a
  success, traced with `timedOut`. On `api34_emoji_check` (API 34, Chrome
  the only browser), two of three https opens first answered 502 after
  25.0s and 15.0s that way. After the fix the same three answered 200 in
  0.69-1.95s with `started` naming a Chrome activity, none timing out, so
  the `timedOut` path is covered by unit tests only, and the unhandled
  scheme was 422 in 0.18-0.37s. A `token=secret` query on every URL was in
  neither the trace nor the server log.

- `POST /api/sequence?target=<id>` runs several `/api/input` bodies in
  order: `{"steps":[{"action":"tap","match":{"text":"Email"}},
  {"action":"text","text":"a@b.co"},{"action":"tap","match":{"text":"Sign
  in"}},{"action":"wait","match":{"text":"Inbox"}}]}`. An agent pays one
  round trip for the flow instead of one model turn per step, and its
  cursor moves from step to step with no pause for thought between. Each
  step goes through `handleInput` itself with the sequence's query and
  headers, so it is validated, arbitrated, logged and traced as if sent
  alone, and a person who takes the device back partway stops the
  sequence at the next step. Before a coordinate `tap` or `swipe` the
  runner sends a `move`, as `loupe mcp` does for one tap. The first step
  that answers anything but 200 stops the rest: the response has that
  step's status, `"failed": <index>`, its `error` (prefixed `step N:`) and
  `code` at the top level so a caller that keys on `humanHeld` or
  `claimedBy` finds them where a single input puts them, and `steps` with
  each step's `status` and `body` up to and including it. All 200 is
  `{"ok":true,"steps":[...]}`. 1 to 50 steps (`MaxSequenceSteps`). A step
  whose node only exists after an earlier step should look for it with a
  `wait` and a `match`, or with `waitMs` on its own `match`.
- `GET /api/screenshot` returns one PNG of the framebuffer, at the device's own
  resolution and with no session open. 266ms on emulator-5554 (1080x2424),
  444ms on an Acme One (1200x2670). `Screenshotter` is optional the way
  `Inspector` is, so synthetic answers 501.

  `adb exec-out`, not `adb shell`: shell runs its output through a pty on older
  transports, which rewrites 0x0a and corrupts every PNG. screencap also writes
  its errors to stdout and exits zero on some devices, so the PNG signature is
  checked rather than trusted.

  Reading a still off the video path instead would answer a different question.
  A canvas frame has been through `MaxSize` and through a lossy encoder, and on
  a stalled link it is whatever was painted last rather than what is on the
  device now. The web client's button does fall back to the canvas, but only
  for a target with no adb behind it.
- `POST /api/install` installs the request body as an APK with
  `adb install -r`, or a `multipart/form-data` body as a base and its splits
  with `adb install-multiple -r` (`curl -F apk=@base.apk -F apk=@split.apk`,
  up to 64 file parts, any field name). **Splits are the normal case**, not
  an edge: anything built from an App Bundle arrives as a base plus config
  splits, and the base alone is refused with `INSTALL_FAILED_MISSING_SPLIT`.
  `com.google.android.contactkeys` pulled off emulator-5554 was a base and
  three splits (ABI, language, density), 5.6MB, and went back in 533ms as
  one multipart request. Each file is checked for a zip signature (400 naming
  the part), the whole upload is capped at 1 GiB (413, before staging when
  `Content-Length` says so), and everything is staged in one temp directory
  under index names, never the caller's filenames, because adb takes paths
  and not stdin. A refusal is 422 with adb's
  output and the `INSTALL_FAILED_*` code split out, since the code is what a
  caller branches on. **The exit status is not the verdict.** Older adb exits
  0 after printing `Failure [...]`, so any Failure line fails the install, and
  so does a zero exit with no `Success` line. adb 35 exits 1 and prints the
  Failure after `adb: failed to install <path>: `, which the same pattern
  reads.

  `?allowTest=1` adds `-t`. Android Studio's Run marks its APK
  `android:testOnly`, and without `-t` the package manager refuses it with
  `INSTALL_FAILED_TEST_ONLY`: both `dev.bmcreations.mask.sample` builds on
  emulator-5554 did. With it, a 9.2MB reinstall took 128ms through the
  server. Each install is traced as `kind:"agent"`, `action:"install"`, with
  `bytes`, `apks`, `tookMs`, `outcome` (`ok`, `refused`, `tooLarge`, `error`),
  `code` and `package`.

  **A successful install names its package**, read with `aapt2 dump
  packagename` from the newest SDK build-tools (found as the emulator is,
  since launchd's PATH lacks the SDK). A config split carries its base's
  name -- all four contactkeys APKs read `com.google.android.contactkeys`
  -- so the first part answers for the set. `?package=` stands in when
  aapt2 is missing, and when aapt2 reads a different name the response
  carries `packageError` and nothing is launched, since that would open
  some other app.

  `?launch=1` then runs the `launch` action on that package, arbitrated
  like `/api/input` (a held target is `launch.code: "humanHeld"`). Its
  outcome is a nested `launch` object (`ok`, `activity`, `error`, `code`,
  `retryAfterMs`, `tookMs`), and **the status stays the install's**: the
  APK is on the device whatever the launch says, and a 409 or 502 would
  read as the install failing. The 9.2MB mask sample went in and came to
  the front in 284ms plus 1.5s through a spare server on emulator-5554.
- `POST /api/media?target=<id>` adds the request body, a photo or a video,
  to the device's library, so it shows in Photos or Google Photos, and
  answers 200 `{target, ok, name, kind, bytes, tookMs}`. Nothing carries a
  drag through adb or HID; a picture goes into a simulator's text field by
  paste, through `/api/drop`. The type
  comes from the file's leading bytes (PNG, JPEG, GIF, WebP, HEIC, MP4, MOV),
  never the name; anything else is 400 before it is pushed. 1 GiB cap (413).
  Not arbitrated, as `/api/install` is not: it changes no screen. On Android
  it is `adb push` to `/sdcard/Pictures` or `/sdcard/Movies` and a
  `MEDIA_SCANNER_SCAN_FILE` broadcast. On API 34 (`loupe_headless`) the push
  alone was indexed, since shared storage is FUSE; on API 29 (`loupe_api29`)
  only the broadcast indexed it, and `content call --method scan_file` threw
  a NullPointerException there, so the broadcast is sent on every level. A
  PNG and a video showed in Google Photos on API 34. On a simulator it is
  `simctl addmedia`, which needs the simulator booted; a PNG and a 2s video
  showed in Photos. Physical iOS devices and synthetic answer 501. Traced as
  `kind:"agent"`, `action:"media"`, with `ext`, `bytes`, `tookMs` and
  `outcome`; the file's name is not. In the viewer, dropping a file on the
  page outlines the stage and routes it by type (`web/drop.js`). A drop
  dispatched in the built-in browser against a throwaway simulator lit the
  outline and answered "Added to Photos" in 914ms; a real drag from Finder
  has not been tried.
- `POST /api/drop?target=<id>&x=<px>&y=<px>` pastes the request body, an
  image, into the text field at x, y in device pixels, as `/api/input`
  takes them, and answers 200 `{target, ok, tookMs}`. It is how a picture
  dropped from the Mac onto a field gets there. The type comes from the
  leading bytes as on `/api/media`: PNG, JPEG, GIF, WebP or HEIC, anything
  else (a video included) 400; over 50 MiB 413; a missing or non-integer
  x or y 400. iOS simulators only; every other target is 501 with
  `code:"unsupported"`. The image is set on the simulator's pasteboard, the
  point is tapped twice to raise the edit menu, and its Paste is tapped;
  when no Paste appears, after one long press too, it is 422 with
  `code:"noPaste"`: the point is not a field that takes an image, and a
  client falls back to `/api/media`. The field's value is read before and
  after Paste; when it has not changed 1.5s later the menu is raised again
  and Paste tapped once more, and a second miss is 422 with
  `code:"pasteFailed"`. A field that exposes no value is taken on trust. A
  simcap whose `-caps` lacks `pasteboardimage` is 501 with
  `code:"simcapOutdated"` before anything is tapped; `make simcap` fixes
  it. It is input: claims, hands-off and the human hold apply as to a tap,
  and `X-Loupe-Viewer` makes it a person's. Viewers see it as an agent's
  tap: the cursor moves to x, y first, and a `tap` event there follows,
  with `err` when it failed. Traced as `kind:"agent"`, `action:"drop"`,
  with the caller, `x`, `y`, `ext`, `bytes`, `longPressed`, `retried`,
  `verified`, `published` (a session was there to see it), `tookMs` and
  `outcome`; never the image. MCP tool `drop` takes a path on disk.
  The mechanism and its gotchas are under Clipboard in `docs/targets.md`.
- `POST /api/camera?target=<id>` points the device's back camera at the
  request body, a PNG or JPEG, raw; `DELETE` gives the camera back. Both
  answer 200 `{target, ok, applies, tookMs}`, POST with the image's `width`
  and `height` too, and `applies` says when an app sees it. AVDs, iOS simulators and
  physical iPhones; Android devices, `emulator-` serials and synthetic
  answer 501. Anything that is not a PNG
  or JPEG, or is over 8192px a side, is 400; over 32 MiB is 413. The image
  is staged as PNG under `~/Library/Caches/loupe/camera/`, one per target,
  and a JPEG is re-encoded: the emulator reads the file's type from its
  extension, and JPEG bytes in a `.png` showed its magenta error frame. Not
  arbitrated, as `/api/media` is not. Traced as `kind:"agent"`,
  `action:"camera"` with `method`, `applies`, `width`, `height` and `outcome`.

  **AVDs.** The emulator only takes an image as a boot flag,
  `-camera-back imagefile:<path>`; the console (`virtualscene-image` swaps a
  poster in the 3D scene, not the frame) and the gRPC API have nothing that
  replaces the frame on a running emulator. So loupe passes the flag on any
  boot it starts while an image is staged, and the emulator reads the file
  each time an app opens the camera. `applies` is `cameraOpen` when the
  running emulator has the flag: a preview already showing keeps the old
  image until the app closes and reopens the camera (Home, then launch).
  It is `boot` when it does not, or is not running: shut it down and boot
  it through loupe. A short image sequence is one POST and one camera
  reopen per image. `DELETE` on an AVD running with the flag stages black,
  since a missing file is the magenta error frame, and the next boot drops
  the flag. A snapshot saved with the flag does not load without it, and
  the reverse ("The emulator hardware cannot load snapshot: loupe-warm"),
  so turning the camera image on or off costs one cold boot, 14 to 15s on
  `loupe_camera` against 3.4s from the snapshot; loupe saves `loupe-warm`
  again after it, and a new image at the same path loads warm. Left to
  itself the emulator shows the image off center: with the back sensor at
  90 degrees (the default, and every AVD here) it center-crops to the
  sensor's 4:3, then gives the app only the left 3:4 of that crop. A
  1920x1080 grid showed x 240-1050 and nothing right of center; Flipcash's
  Scan tab showed the left half of an image. So loupe stages a 1280x960
  canvas with the image fitted whole into its left 3:4 on white, and the
  app sees the whole image, centered and upright (stock camera and Flipcash
  on `Pixel_7a_API_36`, emulator 37.2.12). Other `hw.camera.back.orientation`
  values are staged as sent, unmeasured. On `loupe_camera`
  (Pixel 6, API 36) a QR, then a swap and a clear, each showed in the
  stock camera app after a reopen.

  **iOS simulators.** The simulator has no camera: `AVCaptureDevice`
  answers nil for every type, simctl has no camera verb, and neither
  Simulator.app nor CoreSimulator carries a camera backend (Xcode 27.1).
  Loupe builds a shim (`internal/target/loupecam/loupecam.m`) with `xcrun
  clang` on the first POST, in about 4s, and from then on `launch` (the
  `/api/input` action and the MCP tool) starts the app with it in
  `DYLD_INSERT_LIBRARIES` and the staged file in `LOUPE_CAMERA_FILE`,
  **relaunching it** if it is running without them. A launch of an app
  that already has the shim only brings it to the front. The shim answers
  one back camera; keeps `AVCaptureSession`'s inputs, outputs and running
  state itself; hands every `AVCaptureVideoDataOutput` delegate the image
  30 times a second in the format its `videoSettings` ask for (420v by
  default, 420f or BGRA); reports QR codes found by CIDetector to an
  `AVCaptureMetadataOutput`; and draws the image in each
  `AVCaptureVideoPreviewLayer` with its `videoGravity`. It polls the file
  four times a second, so `applies` is `live` while an app this server
  launched with it runs, and `launch` until one does. `DELETE` removes the
  file: apps with the shim see black, and later launches are plain ones.
  Frames are 1920x1080, `activeFormat`'s size, landscape as a back
  camera's are: the image is held upright and fitted whole on white, so a
  code is never cropped, as on an emulator. Known
  differences from a phone: `videoRotationAngle` is kept but not applied;
  QR bounds are in that landscape frame's coordinates, and the preview
  layer's conversions are not faked; metadata is QR only, and asking for
  another type throws as a phone does for an unsupported one; there is no
  photo or movie output.
  An app that needs something the shim does not fake can read
  `LOUPE_CAMERA_FILE` itself in a debug build: when the variable names a
  file (relative to the app's home on a phone), feed the scanner from it
  and skip `AVCaptureDevice`. Re-read it when its modification date moves,
  as the shim does, to follow swaps. A sketch, not exercised in any app:

  ```swift
  #if DEBUG
  /// Frames from the image loupe stages, for a build loupe launched.
  final class LoupeCameraFrames {
      let url: URL
      init?() {
          guard let path = ProcessInfo.processInfo.environment["LOUPE_CAMERA_FILE"] else { return nil }
          url = URL(fileURLWithPath: path, relativeTo: URL(fileURLWithPath: NSHomeDirectory(), isDirectory: true))
      }
      /// The current image, or nil when it is cleared: treat as a covered lens.
      func image() -> CGImage? {
          guard let src = CGImageSourceCreateWithURL(url as CFURL, nil) else { return nil }
          return CGImageSourceCreateImageAtIndex(src, 0, nil)
      }
  }
  #endif
  ```

  The scanner calls `image()` on a timer and hands it to the same decode
  step its `AVCaptureVideoDataOutput` delegate feeds, converted to the
  pixel buffer format that step expects. On a throwaway iPhone 17
  simulator (iOS 27.1), a test app built on Flipcash's capture setup
  (back wide camera, video data output, QR metadata output, preview layer)
  got 420v frames, decoded the QR, and saw a swap to a 1280x960 JPEG and a
  clear within a second, without a relaunch. After the 1920x1080 change,
  the same through a spare server on an iPhone 17 simulator (iOS 27.0).

  **Physical iPhones.** The same shim, for **development-signed** apps
  only: dyld honours `DYLD_INSERT_LIBRARIES` in an app with
  `get-task-allow`, the way Xcode's Main Thread Checker gets in, and
  ignores the whole launch environment in an App Store, TestFlight or
  enterprise build. No app code changes. On the first POST loupe builds
  the shim for `iphoneos` and signs a copy with each Apple Development
  identity in the keychain, one per team: dyld refuses a library signed by
  a team other than the app's (library validation) and skips it, and a
  missing path, without a word, so every copy goes in the insert list and
  the matching one loads. A `launch` with an image staged then finds the
  app's data container (devicectl does not say where it is, so loupe
  launches the app once with `DYLD_PRINT_ENV` and reads `HOME` off the
  console, which also closes it), copies the shims and the image into it
  with `devicectl device copy to`, and relaunches the app with
  `DEVICECTL_CHILD_DYLD_INSERT_LIBRARIES` and
  `DEVICECTL_CHILD_LOUPE_CAMERA_FILE`. The shim writes the pid it loaded
  into to `Library/Caches/loupe-camera.png.loaded`, and loupe reads it back:
  a pid that does not match is the launch's 422 `noCameraShim`, as is an
  app dyld takes no environment from. So the app must be signed by a team
  this Mac has an Apple Development identity for, which is the case when
  it was built and installed from this Mac. A later POST or DELETE copies
  the image, or an empty file for black (devicectl cannot delete a file),
  into each app still running with the shim, so `applies` is `live` for
  those and `launch` otherwise. Each devicectl call is 1 to 5s: on an
  iPhone 16 Pro (iOS 27.2) over Wi-Fi a shimmed launch with the container
  already known took 2.7s and a swap or clear 1.0 to 1.4s; the first
  launch of an app adds the console launch that finds its container. A
  launch's budget is 2 minutes while an image is staged.
  The phone must be unlocked, as for any launch. Measured there through a
  spare server with a test app signed by a second team: the QR decoded, a
  swap to red and a clear to black showed live, and Settings answered
  422 `noCameraShim` in 5s.
- `POST /api/devices/shutdown?target=<id>` stops an AVD or simulator and
  answers 200 with its `TargetInfo`; `DELETE /api/devices?target=<id>` stops it
  if running, deletes it and answers 204. The target must be an `avd:` or
  `sim:` id that is not `creating` and is available, and for shutdown not
  already `bootsOnOpen`; anything else is 422 `notSupported` with the reason
  (a `device`, `iosDevice`, `synthetic`, `sim-new:` or bare `emulator-NNNN`
  row, or an unknown id). 409 `busy` is a second request while a shutdown or
  delete of that target runs, 409 `recording` (with the recording's `id`) a
  recording in progress, which is never ended as a side effect, and 409
  `humanHeld` a person holding the target, arbitrated as `launch` is. A
  failed stop or delete is 502 and lifts the reopen guard again. Each is
  traced as `kind:"agent"`, `action:"shutdown"` or `"delete"`, with
  `platform`, `tookMs`, `outcome` (`ok`, `error` or `refused`) and `code` on
  a refusal. Viewers are sent `MsgEnded` before the session closes.
  With `emulatorKeepState` on, an AVD's shutdown saves its state first and
  the trace carries `saved`.
- `POST /api/devices/boot?target=<id>` boots an AVD or simulator without
  opening a stream, and answers 200 with its `TargetInfo` once it is up; a
  target already running answers 200 at once. It lifts a `shutDown` reopen
  guard as `start=1` does, is refused 409 `busy` while a shutdown or delete
  of that target runs, and is arbitrated as `launch` is. The same 422
  `notSupported` rows as shutdown apply. A failed boot is 502. Nothing holds
  the emulator afterwards, so an AVD booted this way is paused and shut down
  by the idle clock like any other. Traced as `kind:"agent"`,
  `action:"boot"`, with `platform`, `tookMs`, `outcome` and `wasRunning`.
  Through a spare server: `loupe_keep` from `loupe-warm` in 2.2s, and a new
  throwaway simulator (iPhone 17) in 33.6s on its first boot, then 0.4s and
  `wasRunning` true for a second call. A second call on the running
  `loupe_keep` took 4.1s, though the trace put `Boot` at 29ms: the handler
  reads the target's row before and after through `Manager.Info`, which
  builds the whole `/api/targets` list, and that took about 2s at a load
  average over 60.
- `POST /api/devices/reset?target=avd:<name>` is Reset above: it stops the
  AVD without saving, discards `loupe-warm` and wipes its data on the next
  boot. It works on a stopped AVD, answers 200 with its `TargetInfo`, and is
  refused and arbitrated as shutdown is. A simulator is 422 `notSupported`.
- `GET /api/logcat` returns `logcat -v threadtime` as plain text: the last
  `?lines=` (default 500), or everything since `?since=`, which takes a
  duration back from now (`5m`) or an RFC 3339 time and is trimmed to
  `lines` here, since `-T` has no limit of its own. At most one of `?pid=`,
  `?package=` and `?foreground=1` narrows it to a process; the last goes
  through `ForegroundApp`, the same read as the Tools sheet's app card. The
  pid it used and the package, when there is one, come back in
  `X-Loupe-Pid` and `X-Loupe-Package`. A package with no process is 409.
  `?follow=1` keeps it open and flushes each read: a `log -t` line reached
  curl 62ms after it was written on emulator-5554, and hanging up killed the
  adb child. The pid is fixed when the log opens, so a follow outlives an app
  restart by reading the process that died.

  A package reaches the device's shell only through `pidof`, and only after
  `ValidPackageName` and `shellQuote`. Everything else passed to logcat is
  digits formatted here.

  `Installer` and `Logcatter` are optional the way `Inspector` is: an adb
  device has both, a running AVD delegates to its `ADBTarget`, a shut-down
  AVD answers "open it in the viewer to boot it" (502, as `/api/screenshot`
  does), simulators and physical iOS devices have their own (below), and
  synthetic answers 501.

  **A simulator's log is its unified log**, `simctl spawn <udid> log show`
  for a dump and `log stream` for a follow, `--style compact` as text and
  `?format=ndjson` as `--style ndjson`, one JSON object an entry with the
  unified log's own fields (`application/x-ndjson`). A package is a bundle ID
  (`ValidBundleID`, hyphens allowed), resolved to a pid from `launchctl list`
  inside the simulator; one that is listed with no pid is 409, as on
  Android. A lines-only dump tries `--last` 1m, 10m and 1h in turn and stops
  at the first that holds enough lines, because `log show` with no window
  reads the whole store: on loupe-probe 10m was 545k lines in 10.1s. A follow
  has no backlog, so `since` with `follow` is 422 `notSupported` rather than
  ignored. `?format=ndjson` on an adb target is 422 too.

  **A physical iPhone's log is lockdownd's `com.apple.syslog_relay`**,
  reached through usbmuxd over a cable or Wi-Fi (`internal/usbmux/lockdown.go`):
  the pair record from usbmuxd, held in memory only, a lockdown session
  upgraded to TLS with its certificate, then `StartService`. The relay sends
  each line NUL-terminated and vis-encodes every byte that is not printable
  ASCII (`\M-b\M^@\M-&` for an ellipsis), which `unvis` undoes. On connect
  it sends a backlog, 64KB in under a second on bmc-iphone, so a dump reads
  until 500ms of quiet, at most 3s, and keeps the last `lines`; through a
  spare server on bmc-iphone (iOS 27.2, Wi-Fi) `lines=5` answered in 3.6s,
  the cap, since a busy phone is never quiet. `?pid=` filters on the `[pid]`
  in each line, and a continuation line goes with the message above it.
  It is text only, with no history to query by time, and nothing maps a
  bundle ID to a pid here, so `format=ndjson`, `since`, `package` and
  `foreground` are 422 `notSupported`. A phone usbmuxd does not list is 502.
  Brandon's iPhone (iOS 26.6) was listed by devicectl on Wi-Fi and not by
  usbmuxd, so it has not been read.

- `GET /api/crashes?target=sim:<udid>` lists a simulator's crash reports,
  newest first: the last `?limit=` (20 by default) or those since `?since=`,
  which takes what logcat's does. Each row is `id` (the report's
  `incident_id`), `kind` (`crash`), `time`, `process`, `bundleID` (empty for
  a daemon), `version`, `build`, `pid` and `summary`, the exception and
  termination (`EXC_CRASH (SIGABRT), Abort trap: 6`). `GET
  /api/crashes/<id>?target=` is the `.ips` as written, as `text/plain`, since
  it is two JSON documents. Reports are host files in
  `~/Library/Logs/DiagnosticReports`, only `bug_type` 309, and a row belongs
  to a simulator when its `coalitionName` names that UDID. That directory
  also holds every other simulator's reports and the Mac's own, so **a fetch
  re-checks the target** and answers 404 for another's report as for a
  missing one. The id must be a UUID. A shut-down simulator's reports are
  read without booting it. Synthetic answers 501. Traced as
  `kind:"agent"`, `action:"crashes"` with `count` and `tookMs`, or
  `"crash"`; no report content.

- `GET /api/clipboard?target=<id>` reads the device's clipboard and answers
  `{"target","text"}`. `POST` with `{"text": ..., "paste": true}` sets it,
  and `paste` also puts the text into whatever has focus. `paste` is
  optional. A simulator's paste is typed rather than sent as Cmd+V, because
  iOS asks before an app reads what another process set
  (`docs/targets.md`, Clipboard). The text must be UTF-8 and at most
  64 KiB. Text just over that answers 413. A body more than 1 KiB past the
  cap fails the read instead and answers 400.
  - **Android needs a session open.** scrcpy-server is the only thing on
    the device that reads or sets the clipboard, so with no viewer,
    recording or other holder of the session the call answers 409. A read
    the device doesn't answer within 2s fails with 502.
  - **A simulator needs only to be booted.** The call goes through simcap's
    input process, which starts on demand, so no session is needed. A
    shut-down simulator answers 502.
  - Physical iPhones and synthetic targets answer 501.
  - **A peer target's call goes to its owner**, and the owner's refusal
    comes back unchanged.
  - **A set changes that target only**, even with `sharedClipboard` on:
    the target drops its own write as an echo, so it is not shared
    (`docs/targets.md`, Clipboard).
  - **Neither claims nor the human hold gate it.** `handleClipboard` goes
    straight to the target, so an agent can set or paste while a person is
    driving.
  - It is traced as `kind:"agent"` with `action` `clipboard-read`,
    `clipboard-set` or `clipboard-paste`. The text is never traced.
  - The phone's own clipboard UI is in `web/clipboard.js` and uses this
    route.

- `GET /api/clipboard/shared?after=<seq>&waitMs=<ms>` long-polls the shared
  clipboard (`sharedClipboard`, `docs/targets.md`, Clipboard). It answers
  `200 {"seq","id","text","origin","at"}` once a value with a `seq` past
  `after` is there, at once when one already is, and `204` when none arrives
  within `waitMs` (default 25000, at most 30000). `after` absent or 0 gets
  the current value. An `after` past every `seq` the server has given out
  came from before it restarted and counts as 0. `seq` counts values on that
  server only; `id` (`<instance>-<n>`) is the value's identity across
  servers, `origin` is `target:<id>`, `mac`, `participant` or
  `peer:<name>`, and `at` is when this server accepted the value, in its own
  clock's Unix milliseconds.
  - `POST /api/clipboard/shared {"text", "id", "at"}` offers text and
    answers 204. `id` and `at` are optional and filled in. Every POST is
    stored with origin `participant`, whatever the body says. A value is
    dropped, still with 204, when its `id` or its text is the current one's,
    or when it arrives in the same millisecond as the current one with a
    smaller `id`. A sent `at` is ignored: the server stamps its own clock,
    so values order by arrival and a peer's clock skew cannot win or lose
    a copy. Empty text or text that isn't UTF-8 answers 400,
    and text over 64 KiB 413.
  - With `sharedClipboard` off both answer 409 with `code: "disabled"`, and
    a GET already waiting when it is turned off gets that 409 too. A server
    without the service answers 404.
  - **A hub bridges it to each peer** through the peer's pinned client, so
    the token only goes to a peer whose certificate verified. One loop
    long-polls the peer and publishes what arrives as `peer:<name>` under
    the peer's `id` and `at`; each change that didn't come from that peer is
    posted to it. When the peer first answers, or answers after being off,
    the hub posts its current value, so the peer catches up; after a failed
    exchange it posts only a value accepted since the peer last answered. A peer that
    answers 409 is asked again after 30s, one that answers 404 (an older
    build) after 5 min, and a failed exchange after 5s; nothing is posted to
    it meanwhile. Two servers that hold each other as peers settle, because
    a value coming back carries the id the server already holds.
  - Handset joins as a participant unless its server mirrors that same Mac
    (Device > Share Clipboard, which writes `sharedClipboard` through
    `/api/config`). `GET /api/config` carries `hostPasteboard`, a hash of the
    server Mac's hardware UUID (`clipshare.HostID`), while the server can
    mirror its Mac's pasteboard, and omits it otherwise; Handset hashes its
    own the same way and compares, so a tunnel to a remote Mac's loopback
    port still joins.

- `GET /api/handoff?target=<id>&target=<id>` returns `{"url"}`, the web
  client's link for up to four targets, the first one focused: the startup
  banner's phone link, on the certificate's host, with the token. The Mac
  app's Continue in Browser opens it, copies it, or shows it as a QR code,
  so a session moves to a browser on another device. A server with no
  address another device can open (no TLS certificate) answers 404, and a
  fifth target 400 (`TestHandoff*`).

- `GET`, `POST` and `DELETE /api/overlays?target=<id>` put an
  extension's output in front of everyone watching the target: an image
  over the video (a design to compare against), labelled boxes (where it
  differs), or both. loupe neither makes nor reads them; it keeps the
  latest set per target, sends it to every viewer as `MsgOverlay`, and
  serves each image at `GET /api/overlays/image?target=&id=&v=`, so a
  client only ever talks to loupe. POST takes `{id, source, label?, image?
  (base64 PNG or JPEG), screen?: [w, h], bounds?: [x1, y1, x2, y2],
  opacity?, boxes?: [{bounds, label?, kind?}]}` in device pixels; posting
  an id again replaces it in place. At most 8 per target, 200 boxes each,
  12 MiB a post. `DELETE ?id=` or `?source=` removes some; neither removes
  all. The web client and the Mac app compare by fade, swipe or
  difference.
  (`internal/httpapi/overlay_test.go`).

- `POST /api/extensions` registers an extension that has something for
  viewers to read: `{name, url, label?}`, where `url` must be http or https
  on `127.0.0.1`, `::1` or `localhost` (anything else is 400, since anyone
  with the token could otherwise point loupe at another host). A
  registration lasts 90 seconds, so an extension re-registers on a timer.
  `GET /api/extensions` lists them. `/api/ext/{name}/...` passes through to
  the extension with the prefix, loupe's `Authorization` header and the
  `token` query parameter removed, so a client reads it with loupe's token
  alone. An extension that doesn't answer gets 502 and drops out of the
  list. loupe never reads what passes through; docs/extensions.md is the contract for writing one
  (`internal/httpapi/extension_test.go`).

  **A report lands well after the crash.** The row's `time` is the body's
  `captureTime`; the header's `timestamp`, and the file, come later: 22s on
  a Acmechat crash already on disk, and 36s on `loupe-probe` when Calendar
  was sent SIGABRT from the host, which was listed 37s after the signal.
  SIGKILL leaves no report. A listing of 239 reports took 17ms, so nothing
  is cached.

  **A physical iPhone's reports are copied off it through devicectl**:
  `devicectl device info files --domain-type systemCrashLogs` lists the
  phone's crash directory and its `Retired`, and `device copy from` fetches
  one file (`internal/target/iosdevicecrashes.go`). Each copy is its own
  devicectl run, about 1.7s whatever the size, and the directory is mostly
  not crashes: on Brandon's iPhone it held 101 `.ips` files and one crash, the
  rest analytics, Siri feedback, Jetsam and `.cpu_resource` reports, beside
  spindumps of 2-12MB. So only `.ips` files under 4MB whose process part is
  not one of those names are copied, four at a time, into
  `~/Library/Caches/loupe/ios-crashes/<udid>` under their own names, once;
  a file the phone no longer has leaves the cache. The cache is then read
  by the simulator parser with no coalition, since the phone holds only its
  own reports, and `bug_type` 309 still decides. Through a spare server on
  Brandon's iPhone (iOS 26.6, Wi-Fi): the first listing took 2.9s and
  copied one report, the next 0.8s, a 19KB fetch 0.49s, and a missing id
  was 404. Only an XCUITest runner's SIGABRT was there to read, so an app
  crash on the phone and a crash on bmc-iphone (27.2) have not been listed.

  **On Android the reports are the dropbox's**, read with `adb exec-out
  dumpsys dropbox --print --file <tag>`, no root needed, for six tags:
  `data_app_`, `system_app_` and `system_server_` with `crash`,
  `native_crash` and `anr`. One call per tag, in parallel, since dumpsys
  matches every positional argument at once. The tags are constants, so
  nothing a caller sends reaches the device shell. The id is the tag and
  the entry's epoch millis from its file name
  (`data_app_crash-1792722095531`), and that millis is the row's `time`,
  since the header's clock time is the device's local time. `kind` is
  `crash`, `nativeCrash` or `anr`; `bundleID` is the package, `build` its
  versionCode and `version` its versionName, from the body's `Package:`
  line. The summary is the exception line for a crash, the `signal` line
  and the abort message for a native crash, and `Subject:` for an ANR. A
  fetch is that entry's text as dumpsys prints it, header first. The body
  opens with `Process:` on API 29 and with `SystemUptimeMs:` on API 37;
  both are read. A running AVD delegates to its serial, a shut-down one
  answers "open it in the viewer to boot it", and a call resumes a paused
  emulator. **Dropbox is under `/data`**, so without `emulatorKeepState` a
  boot from `loupe-warm` discards every report since it was saved. Its
  limits are set per device, so how many entries a busy session keeps is
  not known. A listing prints whole entries, ANR traces included, and one
  ANR was 197KB.

## Claims and the human hold

**A person at a viewer holds the target against agents.** Any viewer input
-- a finger down, a key, text, a scroll, or the viewer's own `/api/input` --
holds it while a finger is down and for `humanHoldMs` (5000 by default, in
the config file) after the last input. While held, an agent's `tap`,
`swipe`, `pinch`, `text`, `key`, `button`, `rotate`, `reclaim`, `launch` or
`openURL` answers 409
`code: "humanHeld"` with `retryAfterMs` and a `Retry-After` header, and
nothing reaches the device. There is no override: an agent that could
force its way past the person would put the fight back. Reads,
`hardwareKeyboard` and `move` are never refused, and with no session open there is no
hold at all.
An interrupt flag holds the target until the person sends it, and then
refuses the interrupted agent with `code: "interrupted"` until it reads the
flag (see [flags.md](flags.md)).

The shared token cannot tell callers apart, so they say who they are. The
viewer sends `X-Loupe-Viewer` with init's viewer id; a live id makes the
request the person's, and anything else is an agent's. An agent may send
`X-Loupe-Agent: <name>` (64 printable bytes at most), which names it in the
trace and on the banner. The rules are in `internal/session/arbiter.go`,
a clock-driven state machine with no I/O.

Viewers see it as `MsgControl` (0x08, JSON `{held, holder, agent,
agentName}`), sent on every change and after init: `web/control.js` shows
"claude is driving" while an agent's input landed in the last 10s, and
"claude is waiting for you" while one was refused and no action of its
has settled since. A handover is one change from waiting to driving; going
idle in between made the banner vanish for a moment. Each refusal is a
`kind:"agent"` record with `refused`, `retryAfterMs` and `heldBy`; every
`/api/input` record carries `caller` and `viewer` or `agent`; and each hold
or banner change is one `kind:"control"` record, never one per touch.

**Agents claim a device, and the person can mark one hands off.** The human
hold keeps agents off a device a person is touching; it says nothing about two
Claude sessions on one device, and it ends with the session. `claims.go` covers
both. An agent sends `POST /api/claims?target=` with `X-Loupe-Agent: <name>`
and, to tell two sessions of one agent apart, `X-Loupe-Session: <id>`; a body
`{"agent","session","link"}` may say the same, and `link` must be an https URL
of at most 512 bytes. While the claim lasts, any other agent's input on that
target answers 409 `code: "claimedBy"` with the holder in `claim`, before
anything reaches the device. It is a lease: every call from the holder,
`/api/ui` and `/api/screenshot` included, renews it, and 10 minutes after its
last call it lapses (`claimLease`). `DELETE /api/claims?target=` releases it;
someone else's needs `?force=1`, which is what the picker's Release sends.
`GET /api/claims` lists them. Claims are in memory, so a restart drops them.

Hands off is `POST /api/devices/handsoff?target=&on=1|0`, the picker's swipe
action. It refuses every agent, a claim's holder included, with 409
`code: "handsOff"` and no retry time. It is saved to `handsoff.json` beside the
config file, because every `make build` restarts the server. The order is
hands off, then another agent's claim, then the human hold, so an agent is told
the reason that will last longest. Neither refuses a person: a live viewer id
in `X-Loupe-Viewer` for any session, not only this target's, is the picker
acting for them. The shutdown, delete and install `?launch=1` routes go
through the same check as `/api/input`. Reads are never refused.

`/api/targets` carries `claim`, `handsOff` and `drivenBy`, the agent whose
input went through in the last 10s, recorded with or without an open session,
which the arbiter's banner cannot see. The picker shows them as a second line
under the row's detail (hands off outranks a claim, a claim outranks driving)
and draws the claim's `link` as Open session beside the row. There is no URL
known to open a Claude session from Safari on a phone, so the link is
whatever the agent sent, and drawn only when it is https. Each claim, release
and lapse is traced as `kind:"agent"`, `action:"claim"`, `"release"` or
`"claimExpired"` (traced when the lapse is next read, not on a timer), a refusal carries `refused: "claimedBy"` or `"handsOff"`, and
the client reports `picker-handsoff` and `picker-release` with `target`, `on`
and `outcome`.

## See also

- [mcp.md](mcp.md): `loupe mcp`, its tools, and pushing flags over channels.
- [flags.md](flags.md): flags, watches and interrupts, and where they can be sent.
- [security.md](security.md): pairing a device with its own session, and revoking it.
- [recording.md](recording.md): recording a target to MP4.
