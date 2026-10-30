# Recording

`POST /api/record` writes a target's video to an MP4 on the server, straight
from the encoder. Every route here takes the token, like the rest of
[agent-api.md](agent-api.md).

## What you get

The file is the encoder's output unchanged: H.264 at the session's size and
bitrate, no re-encode, timed by the device's own clock. It is not the viewer's
experience; the link and the phone do not affect it.

- It follows the session's bitrate, so adaptive bitrate can lower quality
  partway through. The metadata lists each change in `bitrateChanges`.
- It shows only frames the encoder produced.
- It starts at the first keyframe after the stream config; `firstKeyMs` is how
  long that took.

## Routes

| Route | Result |
| --- | --- |
| `POST /api/record?target=` | Starts one; 201 `{"id"}`. |
| `GET /api/record` | Lists every recording, newest first, in progress or not. |
| `GET /api/record/{id}` | One recording's metadata. |
| `POST /api/record/{id}/stop` | 200 with the metadata once the file is closed, so the download can start straight after. Stopping one that has already ended answers its metadata again. |
| `GET /api/record/{id}.mp4` | The file. Range requests answer 206, so Safari can seek. |
| `GET /api/record/{id}/frames?from=&to=` | A JPEG contact sheet. |
| `DELETE /api/record/{id}` | 204. One in progress is 409 `code: "recording"`. |

**Start.** A person's hold does not refuse it. It opens the session, so a
shut-down AVD boots here. Errors:

- 409 `code: "recording"` with that recording's `id`: the target is already
  recording. One recording per target at a time.
- 503: the server is shutting down.
- 502: the target cannot open.

**Metadata** fields: `target`, `started`, `ended`, `reason`, `frames`, `bytes`,
`durationMs`, `firstKeyMs`, `width`, `height`, `bitrateChanges`, `inProgress`,
and `error` after a failed write.

**Contact sheet.** The samples between `from` and `to` (a Go duration or bare
seconds), tiled 8 across, with `X-Loupe-Frames` listing each tile's PTS in
milliseconds.

- `fps` thins the samples to that rate; absent means every frame. At most 60
  tiles are kept, sampled evenly.
- `fps` of 0, below 0, above 240 or not a number is 400.
- A range over 10s, or with no frames in it, is 422.
- It needs ffmpeg on the server. Without it the sheet answers 501
  `code: "noFFmpeg"` and everything else still works.

## A recording needs no viewer

A recording holds its session open, so the phone leaving does not end it.

## How a recording ends

`reason` in the metadata says why:

| `reason` | Cause |
| --- | --- |
| `stopped` | `/stop` |
| `limit` | 10 minutes, or the file reaching the store's cap |
| `resized` | A config with another picture size, which is a rotation: an MP4 track has one size |
| `fellBehind` | The server could not write fast enough, so it ended rather than leave a hole in the file |
| `sessionEnded` | The session closed, including at server shutdown |
| `writeError` | The disk failed, with `error` set |

The end is also traced as a `kind:"record"`, `action:"end"` record in the trace.

## Where files go

Recordings live in `~/Library/Caches/loupe/recordings` as `<id>.mp4` and
`<id>.json`.

- Finished recordings older than 7 days are deleted at startup, along with an
  `.mp4` that has no `.json` (a crash).
- The store is capped at 1 GiB. When a recording starts or ends, the oldest
  finished ones go first. One in progress is never deleted.
- The cap is also the most one recording may write. At the highest `bitRate`
  (100 Mbps) ten minutes would be about 7.5 GB, so such a recording ends as
  `limit`. The store can exceed the cap by what recordings in progress hold
  until the next check.
- Shutdown ends every recording as `sessionEnded` and waits for the files to
  close.

## In the viewer

Record in the More menu starts one on the current target. The pill shows a red
dot and the elapsed time, and a tap on it stops.

- Stopping downloads the file, then shows a banner with Share and Save.
- Switching target stops this page's recording.
- A reload brings the pill back if this tab's recording is still in progress on
  the target. A recording an agent or another viewer started is never adopted,
  so switching cannot stop it.
- Pressing Record while the target is already recording adopts that recording.
- With `sessionStorage` blocked, a reload forgets the recording.
