#!/usr/bin/env bash
# Uploads a build to a loupe target with POST /api/install and records the
# verdict for comment.sh. It exits 0 whether or not the install worked, so
# the comment can say what failed; the action's last step fails the job.
#
# Inputs come from the environment (action.yml maps them):
#   LOUPE_SERVER      https://host:18456, no trailing path
#   LOUPE_TOKEN       the server's shared token; when empty, read from
#                     LOUPE_TOKEN_FILE (default ~/Library/Caches/loupe/token),
#                     which is where a runner on loupe's own Mac finds it
#   LOUPE_TARGET      a target id, e.g. avd:Pixel_10
#   LOUPE_APKS        APK paths or globs, whitespace separated
#   LOUPE_ALLOW_TEST  "true" adds ?allowTest=1 (Android Studio's testOnly builds)
#   LOUPE_LAUNCH      "true" adds ?launch=1
#   LOUPE_PACKAGE     optional, sent as ?package=; read from the base APK with
#                     aapt2 when empty
#   LOUPE_CONNECT     optional host:port to dial instead of the server's, e.g.
#                     127.0.0.1:18456; TLS is still checked against the
#                     server's name, and the comment link keeps it
#   LOUPE_INSECURE    "true" skips TLS verification, for a local test server
#   LOUPE_RESULT      where to write the result JSON
#
# Never run this with `set -x`: it would print the token.
set -euo pipefail

die() { echo "::error::$*" >&2; exit 2; }

: "${LOUPE_SERVER:?server is required}"
: "${LOUPE_TARGET:?target is required}"
: "${LOUPE_APKS:?apk is required}"
result=${LOUPE_RESULT:-${RUNNER_TEMP:-/tmp}/loupe-preview.json}
server=${LOUPE_SERVER%/}

if [ -z "${LOUPE_TOKEN:-}" ]; then
  token_file=${LOUPE_TOKEN_FILE:-$HOME/Library/Caches/loupe/token}
  token_file=${token_file/#\~/$HOME}
  [ -r "$token_file" ] || die "no token given and $token_file is not readable"
  LOUPE_TOKEN=$(<"$token_file")
  LOUPE_TOKEN=${LOUPE_TOKEN%$'\n'}
fi
[ -n "$LOUPE_TOKEN" ] || die "the token is empty"
[ -n "${GITHUB_ACTIONS:-}" ] && echo "::add-mask::$LOUPE_TOKEN"
command -v jq >/dev/null || die "jq is required"

# Expand globs here rather than in YAML, so `app/build/outputs/apk/debug/*.apk`
# works as an input. A pattern that matches nothing is an error, not a
# literal path for curl to fail on.
apks=()
set -f
patterns=($LOUPE_APKS)
set +f
shopt -s nullglob
for pattern in "${patterns[@]}"; do
  matched=($pattern)
  [ ${#matched[@]} -gt 0 ] || die "no APK matches $pattern"
  apks+=("${matched[@]}")
done
shopt -u nullglob
[ ${#apks[@]} -gt 0 ] || die "apk names no files"
[ ${#apks[@]} -le 64 ] || die "${#apks[@]} APKs; the server takes at most 64 in one install"

# The package goes to the server as ?package=, which it uses to launch when
# its own aapt2 cannot read the APK, and checks against the APK when it can.
# Read here from the base APK, the one whose badging has no split='...'.
find_aapt2() {
  local sdk
  for sdk in "${ANDROID_HOME:-}" "${ANDROID_SDK_ROOT:-}" "$HOME/Library/Android/sdk" "$HOME/Android/Sdk"; do
    [ -n "$sdk" ] && [ -d "$sdk/build-tools" ] || continue
    ls -d "$sdk"/build-tools/*/aapt2 2>/dev/null | sort -V | tail -1
    return
  done
}
package=${LOUPE_PACKAGE:-}
version=""
aapt2=$(find_aapt2 || true)
if [ -n "$aapt2" ]; then
  for apk in "${apks[@]}"; do
    line=$("$aapt2" dump badging "$apk" 2>/dev/null | head -1 || true)
    case "$line" in
      package:*split=*|"") continue ;;
    esac
    [ -n "$package" ] || package=$(sed -n "s/.* name='\([^']*\)'.*/\1/p" <<<"$line")
    version=$(sed -n "s/.* versionName='\([^']*\)'.*/\1/p" <<<"$line")
    break
  done
fi

query="target=$(jq -rn --arg v "$LOUPE_TARGET" '$v|@uri')"
[ "${LOUPE_ALLOW_TEST:-false}" = true ] && query+="&allowTest=1"
# The launch verdict comes back nested in the install's response, and the
# status stays the install's whatever it says.
[ "${LOUPE_LAUNCH:-false}" = true ] && query+="&launch=1"
[ -z "$package" ] || query+="&package=$(jq -rn --arg v "$package" '$v|@uri')"

# One APK goes as the raw body; base plus splits as one multipart request,
# which the server hands to `adb install-multiple`.
body_args=()
if [ ${#apks[@]} -eq 1 ]; then
  body_args=(-H "Content-Type: application/vnd.android.package-archive" --data-binary "@${apks[0]}")
else
  for apk in "${apks[@]}"; do body_args+=(-F "apk=@$apk"); done
fi
# The server allows itself 5 minutes to install, after the upload. Over a
# DERP relay the upload of a large APK takes minutes of its own, so the
# overall limit leaves room for both.
curl_args=(-sS -w '\n%{http_code}' --connect-timeout 30 --max-time 900
  -H "X-Loupe-Agent: loupe-preview")
[ "${LOUPE_INSECURE:-false}" = true ] && curl_args+=(-k)
# --connect-to swaps only the address dialled. SNI and certificate checks
# still use the server's name, so a runner on loupe's own Mac can use
# loopback with the tailnet certificate verified.
if [ -n "${LOUPE_CONNECT:-}" ]; then
  host_port=${server#*://}; host_port=${host_port%%/*}
  [[ $host_port == *:* ]] || host_port+=":443"
  curl_args+=(--connect-to "$host_port:$LOUPE_CONNECT")
fi

echo "Installing ${#apks[@]} APK(s) on $LOUPE_TARGET via $server"
started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
# Not --fail: that would throw away the 422 body, which is the verdict.
# The status is the last line after the body, so a failed curl leaves only
# its own "000".
# The token goes in through curl's config on stdin, from the printf builtin,
# so it never appears in a process's argv.
out=$(printf 'header = "Authorization: Bearer %s"\n' "$LOUPE_TOKEN" |
  curl -K - "${curl_args[@]}" "${body_args[@]}" -X POST "$server/api/install?$query") || true
status=${out##*$'\n'}
raw=""
[ "$out" = "$status" ] || raw=${out%$'\n'*}
[ -n "$status" ] || status=000
resp=$(jq -c 'objects' <<<"$raw" 2>/dev/null) || resp=null
[ -n "$resp" ] || resp=null
outcome=error
case "$status" in
  200) outcome=ok ;;
  422) outcome=refused ;;
esac
# A 401 is plain text and a dead connection has no body at all, so the error
# falls back to the raw body, then to the status.
error=$(jq -r '.error // empty' <<<"$resp" 2>/dev/null || true)
[ -n "$error" ] || [ "$outcome" != error ] || error=${raw:-"no response (HTTP $status)"}
error=${error%$'\n'}
# The server's reading of the package is the one it launched, or refused to
# launch for not matching, so it is the one the comment shows. It also
# names the package for a runner without aapt2.
srv_package=$(jq -r '.package // empty' <<<"$resp" 2>/dev/null || true)
[ -z "$srv_package" ] || package=$srv_package

jq -n \
  --arg outcome "$outcome" --arg status "$status" --arg error "$error" \
  --argjson resp "$resp" \
  --arg server "$server" --arg target "$LOUPE_TARGET" \
  --arg package "$package" --arg version "$version" \
  --arg started "$started" --argjson apks "${#apks[@]}" \
  --arg launch "${LOUPE_LAUNCH:-false}" \
  --arg url "$server/?target=$(jq -rn --arg v "$LOUPE_TARGET" '$v|@uri')" \
  '{outcome: $outcome, status: ($status|tonumber), error: $error,
    code: ($resp.code // ""), reason: ($resp.reason // ""), output: ($resp.output // ""),
    tookMs: ($resp.tookMs // null), bytes: ($resp.bytes // null), apks: $apks,
    server: $server, target: $target, url: $url,
    package: $package, version: $version, startedAt: $started,
    launchRequested: ($launch == "true"), launch: ($resp.launch // null),
    packageError: ($resp.packageError // "")}' >"$result"

case "$outcome" in
  ok)
    echo "Installed in $(jq -r .tookMs "$result")ms"
    jq -r 'select(.launchRequested) | .launch |
      if . == null then "Not launched: the server sent no launch verdict"
      elif .ok then "Launched in \(.tookMs)ms"
      else "::warning::not launched: " +
        (if (.code // "") != "" then "\(.code): " else "" end) +
        (.error // "no reason given" | ltrimstr("not launched: ")) end' "$result"
    ;;
  refused) echo "::error::$LOUPE_TARGET refused the install: $(jq -r '.code + " " + .reason' "$result")" ;;
  *) echo "::error::install failed (HTTP $status): $error" ;;
esac

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  {
    echo "outcome=$outcome"
    echo "url=$(jq -r .url "$result")"
    echo "package=$package"
    echo "code=$(jq -r .code "$result")"
    echo "took-ms=$(jq -r '.tookMs // ""' "$result")"
    echo "result=$result"
  } >>"$GITHUB_OUTPUT"
fi
