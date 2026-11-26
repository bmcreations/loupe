#!/usr/bin/env bash
# relayed-rig.sh: a spare loupe server, a shaped WebSocket link and two probe
# viewers, to reproduce a viewer on a relayed, spiky link next to one that is
# not, and keep everything the run wrote.
#
#   scripts/relayed-rig.sh -o /path/to/run1 -for 120
#   scripts/relayed-rig.sh -o /path/to/run2 -for 120 -unshaped
#   scripts/relayed-rig.sh -report /path/to/run1
#
# Viewer A attaches to the server directly over loopback, so the server judges
# it local. Viewer B goes through cmd/linkshaper -shape-tcp, which dials the
# server from the Mac's LAN address (-src), so the server judges B remote, as
# it does a viewer on another Mac. -unshaped keeps B on that same path with
# no impairment, as the control.
#
# Everything lands in the output directory: the binaries' build (build.txt),
# the server log and its own trace (trace.jsonl, this server's rows only), the
# shaper's log with each spike it drew, and each probe's JSON lines. The
# report is computed from those files and nothing else.
#
# The spare server listens on loopback with its own token, config and trace,
# and discovers no devices, simulators or iPhones, so it cannot touch the
# launchd server's devices or state. It never builds or runs anything in the
# live checkout. Each process is stopped by the PID this script started, never
# by name, because the live server is also called loupe.
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
out=""
dur=120
target=synthetic
seed=1
src=""
viewer_a=1
report_only=""
client=web
# The relay profile, from loupe traces of Handset over Tailscale DERP: rtt
# median 100-113 ms over an ack base of 83-99 ms, spikes to 500-1,900 ms. Both
# directions share one spike schedule, so a spike of D one way is about 2D of
# round trip. No rate cap: the traces give no bandwidth figure, and window=
# bounds what is in flight instead, so during a spike the server's writes block.
plan="0s delay=48ms jitter=8ms spike=250ms-950ms spikelen=200ms-2s spikeevery=5s-20s window=256k"

usage() {
	sed -n '2,8p' "$0" >&2
	cat >&2 <<EOF
flags: -o DIR (required)  -for SECONDS ($dur)  -plan PLAN  -unshaped
       -target ID ($target)  -seed N ($seed)  -src IP (default: the default route's address)
       -no-a (B only)  -client web|native ($client)  -report DIR
EOF
	exit 2
}

while [ $# -gt 0 ]; do
	case "$1" in
	-o) out=$2; shift 2 ;;
	-for) dur=$2; shift 2 ;;
	-plan) plan=$2; shift 2 ;;
	-unshaped) plan="0s rate=0"; shift ;;
	-target) target=$2; shift 2 ;;
	-seed) seed=$2; shift 2 ;;
	-src) src=$2; shift 2 ;;
	-no-a) viewer_a=0; shift ;;
	-client) client=$2; shift 2 ;;
	-report) report_only=$2; shift 2 ;;
	*) usage ;;
	esac
done

# report prints the per-viewer table for a finished run, from its files.
report() {
	local dir=$1
	command -v jq >/dev/null || { echo "report needs jq" >&2; return 1; }
	echo "== $dir"
	cat "$dir/build.txt" 2>/dev/null || true
	echo "plan: $(cat "$dir/plan.txt" 2>/dev/null)"
	local v
	for v in a b; do
		[ -f "$dir/$v.jsonl" ] || continue
		echo "-- viewer $v probe summary ($dir/$v.jsonl, the summary:true line)"
		jq -c 'select(.summary) | {seconds, frames, keys, configs, bytes, ptsMissing, ptsBack, stallSeconds,
			delayP50Ms, delayP90Ms, delayMaxMs, gapMaxMs, rttNowP50Ms, rttNowMaxMs, health, keepMin, keepP50, ended}' "$dir/$v.jsonl"
	done
	# Viewer rows carry no kind; they are the ones with pacerKeep. local tells
	# A (true) from B (false). Shed, keysIn and produced are the session's
	# running totals, so they are reported as last minus first over the rows,
	# not per viewer.
	echo "-- trace viewer rows by local ($dir/trace.jsonl, rows with pacerKeep)"
	jq -s -c '
		def q(p): sort | .[((length * p) | ceil) - 1 | if . < 0 then 0 else . end];
		map(select(has("pacerKeep"))) | group_by(.local)[] |
		{local: .[0].local, rows: length,
		 health: (group_by(.health) | map({(.[0].health): length}) | add),
		 keepMin: (map(.pacerKeep) | min), keepP50: (map(.pacerKeep) | q(0.5)),
		 baseP50Ms: (map(.pacerBaseMs) | q(0.5)),
		 rttSource: (map(.rttSource) | unique),
		 clientRttP50: (map(.client.rttNowMs // .client.rttMs // empty) | if length > 0 then q(0.5) else null end),
		 queueMsP50: (map(.queueMs // empty) | if length > 0 then q(0.5) else null end),
		 ackBpsP50: (map(.ackBps // empty) | if length > 0 then q(0.5) else null end)}' "$dir/trace.jsonl"
	echo "-- session counters over the run (last - first viewer row; session-wide, not per viewer)"
	jq -s -c '
		map(select(has("pacerKeep"))) | sort_by(.t) | (.[0]) as $f | (.[-1]) as $l |
		{from: $f.t, to: $l.t, produced: ($l.produced - $f.produced), keysIn: ($l.keysIn - $f.keysIn),
		 shedPacer: ($l.shedPacer - $f.shedPacer), shedStale: ($l.shedStale - $f.shedStale),
		 shedAwaitKey: ($l.shedAwaitKey - $f.shedAwaitKey), shedAbandon: ($l.shedAbandon - $f.shedAbandon),
		 shed: ($l.shed - $f.shed)}' "$dir/trace.jsonl"
	echo "-- bitrate rows ($dir/trace.jsonl, kind bitrate)"
	jq -s -c '
		map(select(.kind == "bitrate")) |
		{rows: length, gates: (group_by(.gate) | map({(.[0].gate): length}) | add),
		 currentMin: (map(.current // empty) | min), currentMax: (map(.current // empty) | max),
		 changes: (map(.changes // empty) | max)}' "$dir/trace.jsonl"
	if [ -f "$dir/shaper.log" ]; then
		echo "-- spikes the shaper drew ($dir/shaper.log)"
		# grep exits 1 on no match, which an unshaped run always is.
		echo "spikes: $(grep -c ' spike at ' "$dir/shaper.log" || true)"
		grep ' spike at ' "$dir/shaper.log" | head -20 || true
	fi
}

if [ -n "$report_only" ]; then
	report "$report_only"
	exit 0
fi
[ -n "$out" ] || usage
case "$client" in web | native) ;; *) usage ;; esac
mkdir -p "$out/bin" "$out/state"
out=$(cd "$out" && pwd)
if [ -z "$src" ]; then
	ifc=$(route -n get default 2>/dev/null | awk '/interface:/{print $2}')
	[ -n "$ifc" ] && src=$(ipconfig getifaddr "$ifc" 2>/dev/null || true)
fi
if [ -z "$src" ]; then
	echo "no address on the default route's interface; pass -src IP (any non-loopback address of this Mac)" >&2
	exit 1
fi

free_port() {
	local p=$1
	while [ "$p" = 18456 ] || lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1; do
		p=$((p + 1))
	done
	echo "$p"
}

echo "building into $out/bin"
(cd "$repo" && go build -o "$out/bin/loupe" ./cmd/loupe &&
	go build -o "$out/bin/linkshaper" ./cmd/linkshaper &&
	go build -o "$out/bin/wsprobe" ./cmd/wsprobe)
{
	echo "commit: $(cd "$repo" && git rev-parse --short HEAD)$(cd "$repo" && git diff --quiet HEAD -- . || echo ' +uncommitted changes')"
	echo "built: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
	echo "target: $target  src: $src  seed: $seed  for: ${dur}s  client: $client  viewerA: $viewer_a"
} >"$out/build.txt"
echo "$plan" >"$out/plan.txt"

port=$(free_port 18466)
sport=$(free_port $((port + 1)))
token="rig-$$-$RANDOM"

pids=()
stop_all() {
	local pid
	for pid in "${pids[@]:-}"; do
		[ -n "$pid" ] && kill "$pid" 2>/dev/null || true
	done
	for pid in "${pids[@]:-}"; do
		[ -n "$pid" ] && wait "$pid" 2>/dev/null || true
	done
	local p
	for p in "$port" "$sport"; do
		if lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1; then
			echo "port $p: still listening" | tee -a "$out/teardown.txt"
		else
			echo "port $p: free" | tee -a "$out/teardown.txt"
		fi
	done
}
trap stop_all EXIT

"$out/bin/loupe" -addr 127.0.0.1:"$port" -token "$token" -config "$out/state/cfg.json" \
	-trace "$out/trace.jsonl" -synthetic -no-devices -no-simulators -no-ios-devices \
	-emulator-pause 0 -emulator-shutdown 0 >"$out/server.log" 2>&1 &
pids+=($!)
ready=""
for _ in $(seq 1 100); do
	if grep -q 'address already in use' "$out/server.log"; then
		echo "spare server: port $port already in use" >&2
		exit 1
	fi
	code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $token" "http://127.0.0.1:$port/api/targets" || true)
	if [ "$code" = 200 ]; then
		ready=1
		break
	fi
	sleep 0.2
done
[ -n "$ready" ] || { echo "spare server never answered on $port; see $out/server.log" >&2; exit 1; }
curl -s -H "Authorization: Bearer $token" "http://127.0.0.1:$port/api/targets" >"$out/targets.json"
echo "spare server pid ${pids[0]} on 127.0.0.1:$port"

"$out/bin/linkshaper" -shape-tcp -listen 127.0.0.1:"$sport" -to 127.0.0.1:"$port" -src "$src" \
	-plan "$plan" -seed "$seed" -report 5s >"$out/shaper.log" 2>&1 &
pids+=($!)
for _ in $(seq 1 50); do
	lsof -nP -iTCP:"$sport" -sTCP:LISTEN >/dev/null 2>&1 && break
	sleep 0.1
done
echo "linkshaper pid ${pids[1]} on 127.0.0.1:$sport -> $port from $src"

date -u +%Y-%m-%dT%H:%M:%S.%3NZ >"$out/start.txt" 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ >"$out/start.txt"
probe_pids=()
if [ "$viewer_a" = 1 ]; then
	"$out/bin/wsprobe" -for "${dur}s" -token "$token" -client "$client" -out "$out/a.jsonl" \
		"ws://127.0.0.1:$port/ws?target=$target" >/dev/null 2>"$out/a.err" &
	probe_pids+=($!)
fi
"$out/bin/wsprobe" -for "${dur}s" -token "$token" -client "$client" -out "$out/b.jsonl" \
	"ws://127.0.0.1:$sport/ws?target=$target" >/dev/null 2>"$out/b.err" &
probe_pids+=($!)
pids+=("${probe_pids[@]}")
echo "probes running for ${dur}s"
for pid in "${probe_pids[@]}"; do
	wait "$pid" || echo "probe $pid exited $?" >&2
done
date -u +%Y-%m-%dT%H:%M:%SZ >"$out/end.txt"
# One more stats tick so the trace's last rows cover the probes' last second.
sleep 2
stop_all
trap - EXIT
report "$out" | tee "$out/report.txt"
