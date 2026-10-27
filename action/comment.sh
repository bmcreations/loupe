#!/usr/bin/env bash
# Writes the PR comment for an install.sh result, updating the one this
# action already posted for the same target rather than adding another.
#
#   comment.sh <result.json>            post or update, with gh
#   comment.sh --dry-run <result.json>  print the body only
#
# Posting reads GH_TOKEN, GITHUB_REPOSITORY and PR_NUMBER, and needs
# `pull-requests: write`. The link never carries the token: a PR comment is
# read by everyone who can see the PR, and the token drives every target.
set -euo pipefail

dry_run=false
if [ "${1:-}" = --dry-run ]; then dry_run=true; shift; fi
result=${1:?usage: comment.sh [--dry-run] <result.json>}

target=$(jq -r .target "$result")
marker="<!-- loupe-preview target=$target -->"
sha=${PR_HEAD_SHA:-${GITHUB_SHA:-}}

body=$(jq -r --arg marker "$marker" --arg sha "${sha:0:7}" '
  def pkg: if .package == "" then "unknown (set the `package` input)"
           else "`\(.package)`" + (if .version == "" then "" else " \(.version)" end) end;
  def secs: if .tookMs == null then "" else " in \(.tookMs / 100 | round / 10)s" end;
  [ $marker,
    if .outcome == "ok" then
      "### Open on \(.target)\n\n**[\(.url)](\(.url))**\n"
    elif .outcome == "refused" then
      "### Install refused on \(.target)\n\n`\(.code)`: \(.reason)\n"
    else
      "### Install failed on \(.target)\n\n\(.error | split("\n")[0])\n"
    end,
    "| | |", "| --- | --- |",
    "| Target | `\(.target)` |",
    "| Package | \(pkg) |",
    (if $sha != "" then "| Commit | \($sha) |" else empty end),
    (if .outcome == "ok" then "| Installed | \(.startedAt)\(secs), \(.apks) APK(s) |"
     else "| Attempted | \(.startedAt), \(.apks) APK(s) |" end),
    "",
    # A server older than ?launch=1 sends no verdict at all. A package it
    # could not name, or that did not match the APK, comes back as a launch
    # error that already starts with "not launched: ".
    (if .outcome == "ok" and .launchRequested then
       if .launch == null then
         "Installed but not launched: this loupe server does not launch on install. Open it from the launcher.\n"
       elif .launch.ok then
         "Launched" + (if (.launch.activity // "") != "" then " `\(.launch.activity)`" else "" end) +
         " in \(.launch.tookMs / 100 | round / 10)s.\n"
       else
         "Installed but not launched: " +
         (if (.launch.code // "") != "" then "`\(.launch.code)`: " else "" end) +
         "\(.launch.error // "no reason given" | ltrimstr("not launched: ")). Open it from the launcher.\n"
       end
     else empty end),
    (if .outcome == "ok" then
       "The link has no token in it. It opens for anyone who has already signed in to this loupe server in the same browser or Home Screen app; otherwise it answers 401."
     elif .output != "" then
       "<details><summary>adb output</summary>\n\n```\n\(.output)\n```\n</details>"
     else empty end)
  ] | join("\n")' "$result")

if $dry_run; then
  printf '%s\n' "$body"
  exit 0
fi

: "${GITHUB_REPOSITORY:?}" "${PR_NUMBER:?not a pull_request run; nothing to comment on}"
# In a workflow_run job the number comes from the build's artifact, which
# the PR's own code wrote, so it is checked before it reaches a URL.
[[ $PR_NUMBER =~ ^[0-9]+$ ]] || { echo "::error::pr-number is not a number" >&2; exit 2; }
existing=$(gh api --paginate "repos/$GITHUB_REPOSITORY/issues/$PR_NUMBER/comments" \
  --jq ".[] | select(.body | startswith(\"$marker\")) | .id")
existing=${existing%%$'\n'*}
if [ -n "$existing" ]; then
  gh api -X PATCH "repos/$GITHUB_REPOSITORY/issues/comments/$existing" -f body="$body" --jq .html_url
else
  gh api -X POST "repos/$GITHUB_REPOSITORY/issues/$PR_NUMBER/comments" -f body="$body" --jq .html_url
fi
