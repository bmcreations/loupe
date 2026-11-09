#!/usr/bin/env python3
"""Run loupe builds through replayed links and score the remote stream.

    python3 -I scripts/stream-bench.py --sim <udid> --ip <tailnet ip> \
        --build remote8=/path/loupe.remote8 --build remote9=/path/loupe.remote9 \
        --plan steady --plan dip --transport wt --transport quic

Each run starts its own loupe (own port, config and token, no device or
iPhone discovery, no shared clipboard), puts cmd/linkshaper in front of it,
and drives cmd/wtprobe through the shaper for --seconds with acks on and a
scroll every 1.5 s, so the content is the same every run. The probe dials the
tailnet address, so loupe sees a non-loopback viewer and serves the remote
track, which is what the phone gets.

It replaces judging a build from a minute of someone scrolling on cellular:
the link there changes between runs, and every run needs a person. What it
does not cover: a browser's decoder (the 8-frame Safari hold is modelled by
wtprobe -ack-hold, not reproduced), and picture quality.

Use a simulator nobody else uses, claimed on the live loupe first.
"""

import argparse
import json
import os
import signal
import subprocess
import sys
import time
import urllib.request
import ssl

# Plans replay links measured on cellular on 2026-11-09. Delay is one way, so
# rtt is about twice it. queue is long because that link was bufferbloated:
# rtt grew to 1.1 s at 14:10 with nothing lost.
PLANS = {
    # The 14:10:30-14:11:06 browser link: about 2 Mbps, rtt 105-230 ms.
    "steady": "0s rate=2m delay=60ms jitter=30ms queue=1500ms",
    # 14:10:08-29: the link fell to about 0.5 Mbps for 20 s, then recovered.
    "dip": "0s rate=2m delay=60ms jitter=30ms queue=1500ms; "
           "20s rate=500k delay=60ms jitter=30ms queue=1500ms; "
           "40s rate=2m delay=60ms jitter=30ms queue=1500ms",
    # 12:38:36-42: acks stopped for about 7 s and rtt came back near 2 s.
    "spike": "0s rate=2m delay=60ms jitter=30ms queue=1500ms; "
             "25s rate=150k delay=900ms jitter=100ms queue=3000ms; "
             "32s rate=2m delay=60ms jitter=30ms queue=1500ms",
    # A clean fast link, to check a build does not regress where it should
    # be easy.
    "clean": "0s rate=8m delay=20ms jitter=5ms queue=500ms",
}

TOKEN = "stream-bench-token"
PAGE_PORT = 18559
HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)


def wait_ready(url, budget=30):
    ctx = ssl._create_unverified_context()
    end = time.time() + budget
    while time.time() < end:
        try:
            urllib.request.urlopen(url, context=ctx, timeout=2)
            return True
        except Exception:
            time.sleep(0.5)
    return False


def pct(v, f):
    if not v:
        return None
    s = sorted(v)
    return s[min(int((len(s) - 1) * f + 0.5), len(s) - 1)]


def trace_summary(path, target, t0):
    caps, held, reasons = [], 0, {}
    unstuck = keys = 0
    if not os.path.exists(path):
        return {}
    with open(path) as f:
        for line in f:
            try:
                r = json.loads(line)
            except ValueError:
                continue
            if r.get("kind") != "remote" or r.get("target") != target:
                continue
            if not r.get("viewers"):
                continue
            caps.append(r.get("remoteCapBps", 0))
            held += r.get("remoteHeldMs", 0)
            unstuck = max(unstuck, r.get("remoteUnstuck", 0))
            keys = max(keys, r.get("remoteKeys", 0))
            reason = r.get("remoteRateReason", "")
            reasons[reason] = reasons.get(reason, 0) + 1
    return {
        "capP50kbps": (pct(caps, .5) or 0) / 1000,
        "capMinKbps": (min(caps) if caps else 0) / 1000,
        "heldMs": held,
        "unstuck": unstuck,
        "keys": keys,
        "reasons": reasons,
    }


def run_one(args, name, build, plan, transport, out):
    os.makedirs(out, exist_ok=True)
    port, sport = args.port, args.port + 1
    target = "sim:" + args.sim
    trace = os.path.join(out, "trace.jsonl")
    logs = []

    def spawn(argv, log):
        f = open(os.path.join(out, log), "w")
        logs.append(f)
        return subprocess.Popen(argv, stdout=f, stderr=subprocess.STDOUT, start_new_session=True)

    # Same page, same place, every run: reopen it at the top.
    page = f"http://127.0.0.1:{PAGE_PORT}/index.html?run={time.time():.0f}"
    subprocess.run(["xcrun", "simctl", "terminate", args.sim, "com.apple.mobilesafari"], capture_output=True)
    subprocess.run(["xcrun", "simctl", "openurl", args.sim, page], check=True, capture_output=True)
    time.sleep(4)

    loupe = spawn([
        build, "-addr", f"{args.ip}:{port}",
        "-tls-cert", args.cert, "-tls-key", args.key,
        "-token", TOKEN, "-config", os.path.join(out, "config.json"),
        "-no-devices", "-no-ios-devices", "-shared-clipboard=false",
        "-synthetic-fallback=false", "-simcap", args.simcap, "-trace", trace,
    ], "loupe.log")
    shaper = None
    try:
        if not wait_ready(f"https://{args.ip}:{port}/healthz"):
            raise RuntimeError("bench loupe did not come up; see loupe.log")
        shaper = spawn([args.shaper, "-listen", f"{args.ip}:{sport}", "-to", f"{args.ip}:{port}",
                        "-plan", PLANS[plan], "-seed", "1"], "shaper.log")
        time.sleep(0.5)
        url = f"https://{args.ip}:{sport}/wt?target={target}&token={TOKEN}&start=1"
        probe = [args.probe, "-for", f"{args.seconds}s", "-acks", "-ack-hold", str(args.ack_hold),
                 "-scroll", args.scroll, "-scroll-every", args.scroll_every, "-tapafter", "2s",
                 "-summary", os.path.join(out, "probe.json"), "-jsonl", os.path.join(out, "frames.jsonl"), url]
        if transport == "quic":
            probe.insert(1, "-quic")
        t0 = time.time()
        with open(os.path.join(out, "probe.log"), "w") as f:
            subprocess.run(probe, stdout=f, stderr=subprocess.STDOUT, timeout=args.seconds + 60)
    finally:
        for p in (shaper, loupe):
            if p and p.poll() is None:
                os.killpg(p.pid, signal.SIGTERM)
                try:
                    p.wait(10)
                except subprocess.TimeoutExpired:
                    os.killpg(p.pid, signal.SIGKILL)
        for f in logs:
            f.close()
    res = {"build": name, "plan": plan, "transport": transport}
    try:
        with open(os.path.join(out, "probe.json")) as f:
            res["probe"] = json.load(f)
    except (OSError, ValueError):
        res["probe"] = None
    res["server"] = trace_summary(trace, target, t0)
    with open(os.path.join(out, "result.json"), "w") as f:
        json.dump(res, f, indent=1)
    return res


def row(r):
    p, s = r["probe"] or {}, r["server"]
    return (f"{r['build']:<10} {r['plan']:<7} {r['transport']:<5}"
            f" fps {p.get('fpsP50', 0):>3.0f}/{p.get('fpsP10', 0):>3.0f}"
            f"  stalls {p.get('stalls200', 0):>3} ({p.get('stallMs', 0) / 1000:>4.1f}s)"
            f"  delay {p.get('delayP50', 0):>4.0f}/{p.get('delayP95', 0):>5.0f}/{p.get('delayMax', 0):>5.0f}"
            f"  late {100 * p.get('late200Share', 0):>3.0f}%"
            f"  kbps {p.get('kbps', 0):>5.0f}"
            f"  cap {s.get('capP50kbps', 0):>5.0f} (min {s.get('capMinKbps', 0):>4.0f})"
            f"  held {s.get('heldMs', 0) / 1000:>4.1f}s unstuck {s.get('unstuck', 0)}")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--sim", required=True, help="simulator udid nobody else is using")
    ap.add_argument("--ip", required=True, help="this Mac's tailnet IPv4")
    ap.add_argument("--build", action="append", required=True, help="name=path to a loupe binary")
    ap.add_argument("--plan", action="append", choices=sorted(PLANS), help="default: steady, dip, spike")
    ap.add_argument("--transport", action="append", choices=["wt", "quic"], help="default: wt and quic")
    ap.add_argument("--seconds", type=int, default=60)
    ap.add_argument("--port", type=int, default=18556, help="bench loupe port; the shaper takes the next one")
    ap.add_argument("--ack-hold", type=int, default=0)
    # Negative units scroll the page down first; the runs alternate from there.
    ap.add_argument("--scroll", default="330,900,-8", help="x,y,units in stream pixels")
    ap.add_argument("--scroll-every", default="700ms")
    ap.add_argument("--simcap", required=True)
    ap.add_argument("--cert", required=True)
    ap.add_argument("--key", required=True)
    ap.add_argument("--out", default=os.path.join(ROOT, ".superpowers", "bench", "runs"))
    ap.add_argument("--probe", default=os.path.join(ROOT, ".build", "wtprobe"))
    ap.add_argument("--shaper", default=os.path.join(ROOT, ".build", "linkshaper"))
    args = ap.parse_args()
    plans = args.plan or ["steady", "dip", "spike"]
    transports = args.transport or ["wt", "quic"]
    stamp = time.strftime("%Y%m%d-%H%M%S")
    pages = subprocess.Popen([sys.executable, "-I", "-m", "http.server", str(PAGE_PORT), "--bind", "127.0.0.1",
                              "--directory", os.path.join(HERE, "stream-bench-page")],
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        run_all(args, plans, transports, stamp)
    finally:
        pages.terminate()


def run_all(args, plans, transports, stamp):
    results = []
    for spec in args.build:
        name, _, path = spec.partition("=")
        for plan in plans:
            for tr in transports:
                out = os.path.join(args.out, stamp, f"{name}-{plan}-{tr}")
                print(f"running {name} {plan} {tr} -> {out}", flush=True)
                try:
                    r = run_one(args, name, path, plan, tr, out)
                except Exception as e:  # one bad run should not lose the table
                    print(f"  failed: {e}", flush=True)
                    continue
                results.append(r)
                print("  " + row(r), flush=True)
    if not results:
        return
    print("\nfps p50/p10 | stalls >200 ms | delay over best p50/p95/max ms | late = frames >200 ms over best")
    print("cap = server remote cap p50 | held = encoder gate held | unstuck = stuck-viewer resyncs")
    for r in results:
        print(row(r))
    with open(os.path.join(args.out, stamp, "results.json"), "w") as f:
        json.dump(results, f, indent=1)


if __name__ == "__main__":
    sys.exit(main())
