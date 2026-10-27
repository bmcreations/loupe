# loupe preview action

Installs a pull request's APK on a loupe target and leaves one comment on the
PR with a link that opens that target. Open the link on an iPhone and the
build is on the device in front of you.

```
GitHub runner          runner on the Mac                        same Mac
assembleDebug ──APK──> this action ──POST /api/install──> loupe ──> emulator
                            └── PR comment: link, target, package, install time
```

## What the comment links to, and what it does not carry

The link is `https://<server>/?target=<id>` with **no token**. A PR comment is
readable by everyone who can see the PR, and loupe's token is a single shared
secret that drives every target on the server, so putting it in the comment
would hand device control to all of them.

So the link only works in a browser that already holds loupe's cookie: Safari
on a phone that has opened the startup link once, or the Home Screen app
installed from it. Anyone else gets a 401. That stays true until scoped share
links exist.

## Inputs

| Input | Required | Default | |
| --- | --- | --- | --- |
| `server` | yes | | Base URL the phone uses, e.g. `https://my-mac.example.ts.net:18456` |
| `token` | | | The server's token, from a secret. Empty reads `token-file` |
| `token-file` | | `~/Library/Caches/loupe/token` | Where loupe keeps its token, for a runner on the same Mac |
| `connect-to` | | | `host:port` to dial instead, e.g. `127.0.0.1:18456`. TLS is still checked against `server`'s name |
| `target` | yes | | Target id, e.g. `avd:Pixel_10`. Must be running |
| `apk` | yes | | Paths or globs, whitespace separated. Two or more files go as one base-plus-splits install |
| `allow-test` | | `false` | Accept an `android:testOnly` APK (`adb install -t`) |
| `launch` | | `false` | Sends `?launch=1`, and the comment says whether the app opened |
| `package` | | | Package name, sent as `?package=` and shown in the comment. Read from the base APK with `aapt2` when empty. If the server reads a different one, nothing is launched |
| `comment` | | `true` | Post or update the PR comment |
| `insecure` | | `false` | Skip TLS verification, for a local test server only |
| `pr-number` | | the `pull_request` event's | The PR to comment on. Set it in a `workflow_run` job |
| `head-sha` | | the `pull_request` event's | The commit shown in the comment |
| `github-token` | | `github.token` | Needs `pull-requests: write` |

Outputs: `outcome` (`ok`, `refused`, `error`), `url`, `package`, `code` (the
`INSTALL_FAILED_*` reason on a refusal) and `took-ms`.

The comment is keyed on the target, so a rerun edits it in place and a matrix
over two targets keeps two comments. A failed install updates it too, so it
never keeps pointing at a build that is not on the device. The step still
fails the job.

## Limits

- **The target must already be running.** `/api/install` answers 502 for a
  shut-down AVD ("open it in the viewer to boot it"), and nothing in the API
  boots one without a viewer session. The comment shows that message.
- **A failed launch does not fail the job.** The APK is installed whatever
  the launch says, so a launch refused with `humanHeld` (someone is using the
  target) or `noLaunchActivity` shows in the comment and as a warning only.
- **This repo is a mirror.** The action is developed in loupe's own
  repository, next to the `/api/install` endpoint it calls, and copied here
  on every change. Open issues here; pull requests are applied there.

## Reaching the server from a runner

GitHub's hosted runners cannot reach a loupe server on someone's Mac, and
nothing should have to open that Mac to the internet for them to. So the
install runs on a self-hosted runner on the same Mac as loupe, and the build
stays on GitHub's runners. The runner connects out to GitHub; nothing
connects in.

That splits the work across two workflows:

| | `Build` | `Preview on loupe` |
| --- | --- | --- |
| Trigger | `pull_request` | `workflow_run`, when `Build` completes |
| Runs on | `ubuntu-latest` | `[self-hosted, loupe]`, the Mac |
| Code it runs | The PR's | The default branch's only |
| Secrets | None | None: the token is read from loupe's own file |
| Does | `assembleDebug`, uploads the APKs and the PR number | Downloads them, installs, comments |

A `workflow_run` workflow is always taken from the default branch, so a PR
cannot change what runs on the Mac by editing it. The PR's code only runs on
GitHub's VM. What reaches the Mac from the PR is the artifact: the APKs,
which run inside the emulator, and a PR number, which is cut to digits before
anything uses it.

The consumer repo stores no loupe secret. With `token` left empty the action
reads `~/Library/Caches/loupe/token`, where loupe keeps it, so the runner has
to run as the same macOS user as loupe.

### Setting up the runner

1. In the consumer repo, Settings, Actions, Runners, New self-hosted runner,
   macOS. Follow its steps on the Mac, and give the runner a `loupe` label.
2. Install it as a service (`./svc.sh install`, `./svc.sh start`) so it comes
   back after a reboot, the way loupe does under launchd.
3. Leave fork PRs out unless you want strangers' APKs on your emulator. The
   example below only previews PRs from branches in the repo. Their builds run
   in the emulator rather than on the Mac, but they still run.

### Reaching loupe from the Mac itself

Set `server` to the address the phone uses, because that is the link the
comment carries, and set `connect-to: 127.0.0.1:18456`. curl then dials
loopback but still checks TLS against the server's name, so the tailnet
certificate verifies without `insecure`. It is needed on the Mac this was
built on: the Mac cannot resolve its own `*.ts.net` name, and without
`connect-to` the install fails with HTTP 000.

## Example: a Gradle Android project

`.github/workflows/build.yml`:

```yaml
name: Build

on:
  pull_request:

permissions:
  contents: read

jobs:
  apk:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-java@v4
        with:
          distribution: temurin
          java-version: 21

      - uses: gradle/actions/setup-gradle@v4

      - run: ./gradlew assembleDebug

      - run: echo "${{ github.event.pull_request.number }}" > app/build/outputs/apk/debug/pr-number

      - uses: actions/upload-artifact@v4
        with:
          name: preview
          path: app/build/outputs/apk/debug/
          retention-days: 3
```

`.github/workflows/preview.yml`, which only takes effect once it is on the
default branch:

```yaml
name: Preview on loupe

on:
  workflow_run:
    workflows: [Build]
    types: [completed]

permissions:
  actions: read
  pull-requests: write

concurrency:
  group: loupe-preview-${{ github.event.workflow_run.head_branch }}
  cancel-in-progress: true

jobs:
  preview:
    if: >-
      github.event.workflow_run.conclusion == 'success' &&
      github.event.workflow_run.event == 'pull_request' &&
      github.event.workflow_run.head_repository.full_name == github.repository
    runs-on: [self-hosted, loupe]
    steps:
      - uses: actions/download-artifact@v4
        with:
          name: preview
          path: ${{ runner.temp }}/preview
          run-id: ${{ github.event.workflow_run.id }}
          github-token: ${{ github.token }}

      - id: pr
        run: echo "number=$(tr -dc 0-9 < '${{ runner.temp }}/preview/pr-number' | head -c 10)" >> "$GITHUB_OUTPUT"

      - uses: bmcreations/loupe-action@v1
        with:
          server: https://my-mac.example.ts.net:18456
          connect-to: 127.0.0.1:18456
          target: avd:Pixel_10
          apk: ${{ runner.temp }}/preview/*.apk
          pr-number: ${{ steps.pr.outputs.number }}
          head-sha: ${{ github.event.workflow_run.head_sha }}
```

`assembleDebug` writes one universal APK, so `apk` matches one file. A build
from an App Bundle produces a base and config splits; point `apk` at all of
them and they go up as one `install-multiple`, since the base alone is refused
with `INSTALL_FAILED_MISSING_SPLIT`.

The PR number travels in the artifact because `workflow_run`'s own
`pull_requests` list is empty for some PRs, those from forks among them. The
download leaves the runner's workspace untouched, so the job never checks out
the PR.

### Without a runner on the Mac

The action only needs an HTTPS URL that reaches loupe, so a hosted runner
works wherever loupe is reachable from one, for example by joining the
runner to the tailnet with `tailscale/github-action`. That needs a tailnet
account, an OAuth client and an ACL per user, which is why it is no longer
the default. A route that needs neither, where loupe fetches the build from
GitHub itself, is planned.

## Running the scripts locally

`install.sh` and `comment.sh` hold all the logic; `action.yml` only maps
inputs to environment variables. From the action's directory, against a
spare server:

```sh
LOUPE_SERVER=https://127.0.0.1:18470 LOUPE_TOKEN=... LOUPE_INSECURE=true \
  LOUPE_TARGET=avd:Pixel_10 LOUPE_APKS='build/*.apk' \
  LOUPE_RESULT=/tmp/r.json ./install.sh
./comment.sh --dry-run /tmp/r.json
```

Never run `install.sh` under `set -x`: it would print the token. The token
reaches curl on stdin, not its command line, so `ps` on a shared runner does
not show it.
