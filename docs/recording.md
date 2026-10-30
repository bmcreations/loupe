# Recording

`POST /api/record` writes a target's video to an MP4 on the server, straight
from the encoder. Every route here takes the token, like the rest of
[agent-api.md](agent-api.md).


**A recording is the encoder's output, unchanged, in an MP4.** The session
already holds every access unit the device produced, stamped with the
device's own capture time (trap 31), so the server writes those to a file:
H.264 at the session's size and bitrate, one sample per access unit, each
sample's duration the gap to the next PTS, no re-encode. That gives a
recording at the encoder's frame rate on the device's clock, whatever the
link or the phone did, which an iOS screen recording of the viewer cannot:
that one has been through the link, the jitter buffer and `present()`.

What it does not show: a frame the encoder never produced, and the viewer's
experience, which is `wtprobe -dump` and `jitter.replay.mjs`. It follows the
session's bitrate, so adaptive bitrate can lower quality partway through;
the metadata lists each change in `bitrateChanges`. It does not pin the
bitrate, which would starve the viewers the controller is protecting.

The frames come from a `Tap` (`internal/session/tap.go`): every frame after
the PTS offset and before broadcast, not paced, not shed, not counted by the
bitrate controller and not arbitrated. It holds 512 frames, about 8s at
60fps. One that fills is closed rather than skipping a frame, and the
recording ends as `fellBehind`, so a file never has a silent hole. The
recorder starts writing at the first keyframe after the config;
`firstKeyMs` is how long that took. A PTS that goes backwards is clamped to
one tick (`TestPTSGoingBackwardsIsClampedToOneTick`). The muxer
(`internal/mp4`) puts `moov` at the end and patches a 64-bit `mdat` size at
`Close`.

Seven routes, all behind the token:

- `POST /api/record?target=` starts one and answers 201 `{"id"}`. It is a
  read, so it is not arbitrated and a person's hold does not refuse it. It
  opens the session the way `/wt` does, so a shut-down AVD boots here. A
  target already recording answers 409 `code: "recording"` with that
  recording's `id`. A server shutting down answers 503, a target that cannot
  open 502.
- `GET /api/record` lists every recording, newest first, in progress or not.
- `GET /api/record/{id}` is one recording's metadata: `target`, `started`,
  `ended`, `reason`, `frames`, `bytes`, `durationMs`, `firstKeyMs`, `width`,
  `height`, `bitrateChanges`, `inProgress`, and `error` after a failed write.
- `POST /api/record/{id}/stop` answers 200 with the metadata once the file
  is closed, so the download can start straight after. Stopping one that has
  already ended answers its metadata again.
- `GET /api/record/{id}.mp4` serves the file through `ServeContent`, so a
  Range request answers 206 and Safari can seek.
- `GET /api/record/{id}/frames?from=&to=` is a JPEG contact sheet, through
  ffmpeg, of the samples between two times (a Go duration or bare seconds),
  with `X-Loupe-Frames` listing each tile's PTS in milliseconds. `fps`
  thins the samples to that rate; absent means every frame. Either way at
  most 60 tiles are kept, sampled evenly. `fps` given as 0, below 0, above
  240 or not a number is 400; a range over 10s or with no frames in it is
  422. Tiles are 8 across. ffmpeg is found on PATH, then `/opt/homebrew/bin`
  and `/usr/local/bin`, since launchd's PATH reaches neither; without it the
  sheet answers 501 `code: "noFFmpeg"` and everything else still works.
- `DELETE /api/record/{id}` answers 204; one in progress is 409
  `code: "recording"`.

**A recording holds its session open with no viewer attached.** `POST`
takes a `Session.Hold`, and `Manager.Release` and the idle reaper count
holds with viewers (`ViewerCount()+Holds()`), so the phone leaving does not
end it after `idleGrace`. The recorder's end gives the hold and the acquire
back, whichever way it ends (`TestRecordingSurvivesTheLastViewerLeaving`).

It ends for one of six reasons, in `reason` and on the `kind:"record"`,
`action:"end"` trace record:

| `reason` | Cause |
| --- | --- |
| `stopped` | `/stop` |
| `limit` | 10 minutes, or the file reaching the store's cap |
| `resized` | A config with another picture size, which is a rotation: an MP4 track has one size |
| `fellBehind` | The tap filled |
| `sessionEnded` | The session closed, including at server shutdown |
| `writeError` | The disk, or the metadata write after the file closed, with `error` set (`TestWriteErrorEndsAsWriteError`) |

Frames already in the tap when a reason arrives are written before it
ends. The start is traced as `kind:"record"`, `action:"start"`, with `id`,
`target`, `tookMs` (which includes opening the session) and the caller.

**The store is `~/Library/Caches/loupe/recordings`**, `<id>.mp4` beside
`<id>.json`, the json written only once the file is closed, so the
directory is the record and only recordings in progress live in memory.
At startup an `.mp4` with no `.json` (a crash) and anything that ended more
than 7 days ago are deleted. The cap is 1 GiB, checked when a recording
starts and when one ends: the oldest finished ones go first, and one in
progress is never deleted (`TestCapDeletesTheOldestFinishedRecordings`).
The cap is also the most one recording may write, since `bitRate` goes to
100 Mbps and ten minutes of that is about 7.5 GB: a recording whose samples
pass it ends as `limit` (`TestCapEndsARecordingAsLimit`). So the total can
exceed the cap by what the recordings in progress hold, each at most the
cap, until the next check.
On shutdown, closing the sessions ends every recording as `sessionEnded`
and main waits for the files to close; a start after that is refused
(`TestWaitReturnsOnceRecordersClose`). One recording per target at a time.

**In the viewer**, Record in the More menu starts one on the current
target. The pill shows a red dot and the elapsed time, and a tap on it
stops. The page polls `/api/record/{id}` every 2s to learn of an end it did
not ask for. Stopping downloads the file with a running count, then shows a
banner: Share calls `navigator.share` with the file inside that tap, since
Safari allows it only there and the file arrived after an await, and Save
is a link to an object URL that lives until the banner goes, because iOS
may ask before it saves and read the blob after the answer. Switching
target stops this page's recording. The page keeps the id it started in
`sessionStorage`, per tab, so a reload brings the pill back by adopting that
recording if it is still in progress on the target. A recording an agent or
another viewer started is never adopted, so a switch cannot stop it. On a 409
from Record the page adopts the id the body names, since the person pressed
Record, and stores it. With `sessionStorage` blocked, a reload forgets the
recording.

Measured through spare servers, each run once on 15 October:

- **Synthetic**, `-no-devices -no-simulators -no-auth` on :18473, recording
  started and stopped with `curl`: 1051 frames in 17.5s at 720x1560, 7.6MB,
  every sample 16.7ms by `ffprobe -show_packets`, 18 keyframes,
  `firstKeyMs` 45, start `tookMs` 2.
- **Pixel_10 on emulator-5554**, `-no-simulators -no-ios-devices` on :18475,
  `avd:Pixel_10` from the Home Screen with no viewer attached, Settings
  opened halfway through by `/api/input` `launch` (answered in 605ms): 97
  frames in 9.48s at 480x1080, 559KB, 2 keyframes, `firstKeyMs` 108, start
  `tookMs` 893, which includes opening the session. The still screen is not
  one long sample. The first second is ten 100ms samples, which matches the
  `repeat-previous-frame-after` the pinned jar sets on the encoder; then one
  sample of 4.87s, the Home Screen at rest; then the launch's transition at
  16-40ms a sample from 5.87s, a keyframe at 6.78s, and 100ms samples again
  with one of 357ms. Why the encoder stopped repeating for those 4.87s is
  not explained.
- **Contact sheets** of that recording: `from=0&to=2` answered in 56ms, 11
  identical Home Screen tiles at PTS 0-1000, 2160x1216 with the empty cells
  green. `from=5.8&to=7` was 56 tiles in 65ms. With `fps=10` it was 12:
  the Home Screen, the Settings splash growing from its icon, the splash
  alone, the list fading in and Settings' home. `to=11` was 422, `fps=0`
  and `fps=241` 400.
- A second `/stop` answered the metadata, the start while recording 409
  with the `id`, and a Range request on the `.mp4` 206.

Not run: QuickTime was not opened on either file; nothing was recorded from
a browser, so the menu, pill, banner, Share, Save and the re-adopt are
covered by node tests only, as is the `.recBanner` layout rule; and the
emulator run was 9.5s with no viewer, under `idleGrace`, so it does not
show the hold. The hold, `fellBehind`, `limit`, `resized`, `sessionEnded`,
`writeError`, the cap and bitrate changes during a recording are covered
by unit tests, not live runs.

