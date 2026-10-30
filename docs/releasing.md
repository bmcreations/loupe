# Releasing

A tag starting `v` and a version number (`v0.2.0`) runs
`.github/workflows/release.yml`. GoReleaser (`.goreleaser.yaml`) builds
`loupe` for macOS and Linux on arm64 and amd64 with `CGO_ENABLED=0`,
stamps `loupe version` from the tag, attaches the archives and
`checksums.txt` to a GitHub release, and commits the updated `loupe.rb`
to [bmcreations/homebrew-tap](https://github.com/bmcreations/homebrew-tap).

```sh
git tag v0.2.0 && git push origin v0.2.0
```

The tap push needs the `HOMEBREW_TAP_TOKEN` repository secret: a token
with contents write on `bmcreations/homebrew-tap`, the same one trace's
release uses. `v1` is the GitHub Action's moving tag and does not match
the workflow's pattern.

Try a release without publishing it:

```sh
HOMEBREW_TAP_TOKEN=x goreleaser release --snapshot --clean --skip=publish
```

`dist/homebrew/loupe.rb` is the formula it would push.

**simcap is not released here.** Its formula in the tap is head-only
(`brew install --HEAD bmcreations/tap/simcap`) and builds with the
user's Xcode, because it links private frameworks from that Xcode
install and a prebuilt binary would break with the next Xcode. The
formula builds it ad hoc; physical iPhones need it signed by a team
(`make simcap TEAM=<team id>` from a checkout).

The formula is generated with GoReleaser's `brews`, which it marks
deprecated in favor of casks. A cask cannot declare a `brew services`
block, so loupe stays a formula.
