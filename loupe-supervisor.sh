#!/bin/bash
# Supervise the loupe server under launchd.
#
# Two things make this more than a `while true` wrapper:
#
#   1. It runs a COPY of ./loupe, never ./loupe itself. macOS returns ETXTBSY
#      when you write to a running executable, so `make build` would fail for
#      as long as the server was up. Running a copy keeps ./loupe replaceable
#      at any moment.
#
#   2. It watches ./loupe's sha256 and restarts onto the new build when it
#      changes. That is the other half of the same idea: rebuilding is the
#      deploy step, and nothing else has to be run by hand.
#
# launchd owns "start at login" and is the backstop if this script itself dies.
# The restart-on-crash loop lives here, because launchd's ThrottleInterval is a
# blunt 10s and this can be smarter about why the child went away.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT" || exit 1
export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/local/go/bin:$PATH"

HOSTNAME_TS="my-mac.tailnet-0000.ts.net"
PORT=18456
BINARY="$ROOT/loupe"
RUNDIR="$ROOT/.run"
CERT="$ROOT/$HOSTNAME_TS.crt"
KEY="$ROOT/$HOSTNAME_TS.key"
TRACE="$ROOT/loupe-trace.jsonl"
TRACE_KEEP=10      # rotated traces retained

POLL=2             # seconds between liveness and hash checks
CERT_MAX_AGE=30    # days before the tailnet cert is reissued
FAST_EXIT=15       # a child gone sooner than this counts as a crash loop
BACKOFF_MAX=60

log() { printf '%s loupe-supervisor: %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*"; }

# The run log and the traces are the owner's alone. launchd opens
# loupe-run.log before this script runs, with its own umask, so the plist
# cannot set that file's mode; it is tightened here instead, along with traces
# an older build left 0644, which can hold the token in cleartext. New traces
# are 0600 from trace.Open, and rotate_trace's mv keeps that.
chmod 600 "$ROOT/loupe-run.log" "$ROOT"/loupe-trace*.jsonl* 2>/dev/null

# ---------------------------------------------------------------- child state
CHILD=""

stop_child() {
  [ -z "$CHILD" ] && return 0
  kill -0 "$CHILD" 2>/dev/null || { CHILD=""; return 0; }
  log "stopping child pid=$CHILD"
  kill -TERM "$CHILD" 2>/dev/null
  for _ in $(seq 1 20); do
    kill -0 "$CHILD" 2>/dev/null || break
    sleep 0.25
  done
  kill -0 "$CHILD" 2>/dev/null && { log "child ignored SIGTERM; SIGKILL"; kill -9 "$CHILD" 2>/dev/null; }
  wait "$CHILD" 2>/dev/null
  CHILD=""
}

trap 'log "supervisor exiting"; stop_child; exit 0' INT TERM
trap 'stop_child' EXIT

# ------------------------------------------------------------------- binary
sha_of() { [ -f "$1" ] || return 1; shasum -a 256 "$1" 2>/dev/null | awk '{print $1}'; }

# `go build` writes its output in place, so a hash read mid-build can catch a
# partial file. Require the same hash twice before trusting it.
stable_sha() {
  local a b
  while true; do
    a=$(sha_of "$BINARY") || { log "waiting for $BINARY to exist"; sleep 3; continue; }
    sleep 1
    b=$(sha_of "$BINARY") || continue
    [ "$a" = "$b" ] && { printf '%s' "$a"; return 0; }
    log "binary still being written; waiting"
  done
}

# ------------------------------------------------------------------- cert
ensure_cert() {
  if [ -f "$CERT" ] && [ -f "$KEY" ] && [ -z "$(find "$CERT" -mtime +$CERT_MAX_AGE 2>/dev/null)" ]; then
    return 0
  fi
  local ts=""
  for c in "$(command -v tailscale 2>/dev/null)" /opt/homebrew/bin/tailscale \
           /usr/local/bin/tailscale /Applications/Tailscale.app/Contents/MacOS/Tailscale; do
    if [ -n "$c" ] && [ -x "$c" ]; then ts="$c"; break; fi
  done
  [ -z "$ts" ] && { log "ERROR no cert and no tailscale CLI"; return 1; }
  log "issuing certificate for $HOSTNAME_TS"
  ( cd "$ROOT" && "$ts" cert "$HOSTNAME_TS" ) || { log "ERROR cert issuance failed"; return 1; }
}

# ------------------------------------------------------------------- port
# Trap 11: lsof reports the port free while the kernel is still tearing the
# listener down, and binding into that window is what fails.
wait_for_port() {
  for _ in $(seq 1 30); do
    [ -z "$(lsof -ti:"$PORT" 2>/dev/null)" ] && return 0
    sleep 0.5
  done
  log "WARNING port $PORT still held by pid(s): $(lsof -ti:"$PORT" 2>/dev/null | tr '\n' ' ')"
  return 1
}

# ------------------------------------------------------------------- trace
# internal/trace appends and rolls one .1 aside past 64 MiB. This keeps a file
# per run on top of that, so a crash loop's runs can be read one at a time.
#
# Rotate to a timestamped name rather than a single -prev slot: under KeepAlive
# a crash loop restarts several times a minute, and one slot means the second
# restart overwrites the evidence the first one just saved.
rotate_trace() {
  [ -s "$TRACE" ] || return 0
  mv -f "$TRACE" "$ROOT/loupe-trace-$(date '+%Y%m%d-%H%M%S').jsonl" 2>/dev/null
  # Keep the newest TRACE_KEEP rotations, discard older ones. The glob matches
  # the timestamp shape only, so hand-named keepsakes like loupe-trace-1911.jsonl
  # are never pruned.
  ls -1t "$ROOT"/loupe-trace-[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]-[0-9][0-9][0-9][0-9][0-9][0-9].jsonl \
    2>/dev/null | tail -n +$(( TRACE_KEEP + 1 )) | while read -r old; do
    rm -f "$old"
  done
  return 0
}

# ------------------------------------------------------------------- start
start_child() {
  # Two statements, not one: bash expands every word of a `local` before the
  # builtin assigns any of them, so `copy=...${sha:0:12}` on the same line
  # reads an unset sha and silently produces a constant filename.
  local sha="$1"
  local copy="$RUNDIR/loupe-${sha:0:12}"
  mkdir -p "$RUNDIR"
  # Name the copy after the build it came from, so a new build is a new path
  # and never an overwrite of something running.
  if [ ! -x "$copy" ]; then
    cp -f "$BINARY" "$copy.tmp" && chmod +x "$copy.tmp" && mv -f "$copy.tmp" "$copy" || {
      log "ERROR could not stage $copy"; rm -f "$copy.tmp"; return 1; }
  fi
  find "$RUNDIR" -maxdepth 1 -type f -name 'loupe-*' ! -name "$(basename "$copy")" -delete 2>/dev/null

  wait_for_port
  rotate_trace

  log "starting ${sha:0:12} on :$PORT"
  # -synthetic stays: the test pattern is the control case, and comparing a
  # real device against it is how a transport problem gets told apart from an
  # encoder one. Device discovery runs once at startup, so an emulator booted
  # later needs a restart to appear -- `make build` is that restart.
  # Encoder settings are deliberately NOT passed here. A flag beats the config
  # file, so every flag on this line is a setting `/api/config` reports as
  # `pinned: true` and cannot change -- a PUT persists to disk and does
  # nothing until this script does. -max-size and -bitrate used to sit here
  # and were exactly that: the two settings worth tuning per link, unreachable
  # from the API. What stays is what the supervisor genuinely owns -- the
  # port, the cert it issues, and the trace file it rotates.
  "$copy" -addr ":$PORT" -synthetic \
          -tls-cert "$CERT" -tls-key "$KEY" \
          -trace "$TRACE" &
  CHILD=$!
  log "child pid=$CHILD"
}

# ------------------------------------------------------------------- main
log "supervisor up, root=$ROOT"
backoff=1

while true; do
  ensure_cert || { sleep 30; continue; }

  running_sha="$(stable_sha)"
  started_at=$(date +%s)
  reason=""
  start_child "$running_sha" || { sleep 10; continue; }

  while true; do
    sleep "$POLL"

    if ! kill -0 "$CHILD" 2>/dev/null; then
      wait "$CHILD" 2>/dev/null; rc=$?
      CHILD=""
      log "child exited rc=$rc after $(( $(date +%s) - started_at ))s"
      reason="exit"
      break
    fi

    current_sha="$(sha_of "$BINARY")" || continue
    if [ -n "$current_sha" ] && [ "$current_sha" != "$running_sha" ]; then
      log "binary changed ${running_sha:0:12} -> ${current_sha:0:12}; restarting"
      stop_child
      reason="swap"
      break
    fi
  done

  # Back off only for a child that died on its own. A swap is a deliberate
  # restart and a fast one is the point, so it must not look like a crash loop.
  if [ "$reason" = "exit" ] && [ $(( $(date +%s) - started_at )) -lt "$FAST_EXIT" ]; then
    log "child died within ${FAST_EXIT}s; backing off ${backoff}s"
    sleep "$backoff"
    backoff=$(( backoff * 2 )); [ "$backoff" -gt "$BACKOFF_MAX" ] && backoff=$BACKOFF_MAX
  else
    backoff=1
    sleep 1
  fi
done
