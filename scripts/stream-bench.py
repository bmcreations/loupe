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
does not cover: picture quality, and with the probe, a browser's decoder (the
8-frame Safari hold is modelled by wtprobe -ack-hold, not reproduced).

--client safari puts real Safari in the probe's place, for what the probe
cannot show (WebTransport credit, the page's decoder and frame order):

    python3 -I scripts/stream-bench.py --client safari --sim <udid> \
        --viewer-sim <udid> --ca ca.crt --cert leaf.crt --key leaf.key ...

Safari on --viewer-sim opens the viewer page through the shaper, which also
relays the page's TCP unshaped (linkshaper -tcp), so only WebTransport takes
the plan. The source simulator is swiped through the bench loupe every
--scroll-every (default 3 s, the phone's swipe-and-pause). Each run is scored
from the page's once-a-second client reports in the trace. The leaf cert's
SAN must include --ip, and --ca, its root, is added to the viewer
simulator's keychain. A run whose reports are not WebTransport, or that has
fewer than one report per two seconds, is marked INVALID.

    python3 -I scripts/stream-bench.py --rescore TRACE START END [--sim <udid>]

scores a saved Safari trace between two RFC 3339 times.

Use simulators nobody else uses, claimed on the live loupe first.
"""

import argparse
import datetime
import json
import os
import signal
import subprocess
import sys
import time
import urllib.parse
import urllib.request
import ssl

# Plans replay links measured on cellular on 2026-11-09. Delay is one way, so
# rtt is about twice it. queue is long because that link was bufferbloated:
# rtt grew to 1.1 s at 14:10 with nothing lost.
PLANS = {
    # The 14:10:30-14:11:06 browser link: about 2 Mbps, rtt 105-230 ms.
    "steady": "0s rate=2m delay=60ms jitter=30ms queue=1500ms",
    # The same link with headroom over the 2 Mbps floor: separates stalls the
    # link causes from stalls a cap at the link's own rate causes.
    "steady3": "0s rate=3m delay=60ms jitter=30ms queue=1500ms",
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
    # Baselines with no shaper. direct: the remote track over the tailnet
    # address, so anything left is loupe, simcap or the probe. local: loopback,
    # the main track the Mac's own Handset gets.
    "direct": None,
    "local": None,
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


def shot(sim, path):
    subprocess.run(["xcrun", "simctl", "io", sim, "screenshot", path], capture_output=True)


def pct(v, f):
    if not v:
        return None
    s = sorted(v)
    return s[min(int((len(s) - 1) * f + 0.5), len(s) - 1)]


def trace_summary(path, target, t0):
    caps, held, reasons, src = [], 0, {}, []
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
            src.append(r.get("remoteFrames", 0))
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
        # What the encoder produced. A run whose scroll did not move the
        # page shows a low source rate, and its delivery numbers mean
        # nothing about the build.
        "srcFpsP50": pct(src, .5) or 0,
    }


def load():
    """1-minute load average and the busiest process, so a run taken while
    something else hogged the Mac can be set aside."""
    la = subprocess.run(["sysctl", "-n", "vm.loadavg"], capture_output=True, text=True).stdout.split()
    top = subprocess.run(["ps", "-axo", "pcpu=,comm="], capture_output=True, text=True).stdout.splitlines()
    top = sorted(top, key=lambda l: -float(l.split()[0]) if l.split() else 0)[:1]
    return {"load1": float(la[1]) if len(la) > 1 else 0, "top": top[0].strip()[-60:] if top else ""}


def frame_summary(path):
    """Stalls the viewer saw, from the per-frame log. An arrival gap over
    200 ms only counts when the frames either side were captured less than
    100 ms further apart than they arrived: otherwise the screen was still and
    simcap had nothing to send. Delay is arrival minus capture time, less the
    run's smallest, so it is the time a frame waited beyond the quickest one;
    "settled" leaves out the first 15 s, where joining dominates."""
    try:
        fr = [json.loads(l) for l in open(path)]
    except OSError:
        return {}
    # loupe sends a 28-byte packet stamped pts 0 now and then; it is not a
    # frame, and counted as one it reads as a delay as long as the run.
    fr = [f for f in fr if not (f["pts"] == 0 and f["bytes"] < 64)]
    if len(fr) < 2:
        return {}
    stalls, stall_ms = 0, 0.0
    for a, b in zip(fr, fr[1:]):
        ga, gp = b["arrMs"] - a["arrMs"], (b["pts"] - a["pts"]) / 1000
        # gp < 0: two frames arrived out of order, not a stall.
        if ga > 200 and 0 <= gp <= ga - 100:
            stalls += 1
            stall_ms += ga
    base = min(f["arrMs"] - f["pts"] / 1000 for f in fr)

    def pct(xs, q):
        xs = sorted(xs)
        return round(xs[min(len(xs) - 1, int(len(xs) * q))]) if xs else 0
    d = [f["arrMs"] - f["pts"] / 1000 - base for f in fr]
    ds = [f["arrMs"] - f["pts"] / 1000 - base for f in fr if f["arrMs"] >= 15000]
    # Frames over 30 KB are 120+ ms of link time each at 2 Mbps. A continuous
    # scroll hides them; the phone's swipe-then-pause makes one per swipe
    # (reflection 2026-11-10), so the row shows them next to the fps.
    big = [f["bytes"] for f in fr if f["bytes"] > 30_000]
    return {"big": len(big), "maxKB": round(max(f["bytes"] for f in fr) / 1000),
            "stalls": stalls, "stallMs": round(stall_ms), "delayP50": pct(d, .5), "delayP95": pct(d, .95),
            "settledP50": pct(ds, .5), "settledP95": pct(ds, .95), "settledMax": pct(ds, 1)}


def duration(v):
    """A Go-style duration ("700ms", "3s") in seconds."""
    return float(v[:-2]) / 1000 if v.endswith("ms") else float(v.rstrip("s"))


def trace_time(r):
    try:
        return datetime.datetime.fromisoformat(r["t"]).timestamp()
    except (KeyError, ValueError):
        return None


def safari_summary(path, target, start, end):
    """Score a Safari viewer from the trace between two epoch times, compared
    in whole seconds. Client reports are the server's once-a-second records
    that carry the page's own report under "client". A rotate is any record
    with starvedMs: loupe moving the viewer to a new session when it ran out
    of stream credit. A planned rotate (credit-low) starts the new session
    before credit runs out and has starvedMs 0; "starved" counts the ones
    where frames waited."""
    def floor_pct(v, p):
        v = sorted(v)
        return v[min(len(v) - 1, int(len(v) * p))] if v else 0
    q, old, fps, first, last, transports = [], [], [], None, None, set()
    rotates = starved = starved_ms = 0
    lo, hi = int(start), int(end)
    try:
        f = open(path)
    except OSError:
        return {}
    with f:
        for line in f:
            try:
                r = json.loads(line)
            except ValueError:
                continue
            t = trace_time(r)
            if t is None or not lo <= int(t) <= hi:
                continue
            if target and r.get("target") not in (None, target):
                continue
            c = r.get("client")
            if isinstance(c, dict) and "decFps" in c:
                transports.add(r.get("transport"))
                q.append(r.get("queueMs") or 0)
                old.append(r.get("oldestMs") or 0)
                fps.append(c.get("fps") or 0)
                first, last = first or r, r
            if r.get("kind") in ("wt-rotate", "rotate", "wt-starve", "starve") or "starvedMs" in r:
                rotates += 1
                if r.get("starvedMs"):
                    starved += 1
                    starved_ms += r["starvedMs"]
    if not last:
        return {"reports": 0}
    moving = [f for f in fps if f > 5]

    def grew(k):
        return (last.get(k) or 0) - (first.get(k) or 0)
    c = last["client"]
    return {
        "reports": len(q), "transports": sorted(str(t) for t in transports),
        "queueP50": floor_pct(q, .5), "queueP95": floor_pct(q, .95), "queueMax": max(q),
        "oldestP50": floor_pct(old, .5), "oldestP95": floor_pct(old, .95), "oldestMax": max(old),
        "fpsP10": floor_pct(moving, .1), "fpsP50": floor_pct(moving, .5), "moving": len(moving),
        "lowFps": sum(1 for f in moving if f < 20), "rotates": rotates, "starved": starved, "starvedMs": starved_ms,
        "shed": grew("shed"), "shedAwaitKey": grew("shedAwaitKey"), "keysIn": grew("keysIn"),
        "decodeErrors": c.get("decodeErrors", 0), "dropped": c.get("dropped", 0),
        "resyncs": (c.get("frameOrder") or {}).get("resyncs", 0),
    }


def bench_api(host, port, path, body):
    req = urllib.request.Request(f"https://{host}:{port}{path}", data=json.dumps(body).encode(), method="POST",
                                 headers={"Authorization": f"Bearer {TOKEN}", "X-Loupe-Agent": "stream-bench",
                                          "Content-Type": "application/json"})
    urllib.request.urlopen(req, context=ssl._create_unverified_context(), timeout=5).read()


def first_report(path, target, after, budget=20):
    """When the viewer's first WebTransport report lands in the trace."""
    end = time.time() + budget
    while time.time() < end:
        try:
            with open(path) as f:
                for line in f:
                    try:
                        r = json.loads(line)
                    except ValueError:
                        continue
                    if (r.get("target") == target and isinstance(r.get("client"), dict)
                            and r.get("transport") == "webtransport" and (trace_time(r) or 0) >= after):
                        return trace_time(r)
        except OSError:
            pass
        time.sleep(0.5)
    return None


def drive_safari(args, host, port, dial, target, trace, out):
    """Open the viewer in Safari on the viewer simulator, wait for its first
    report, then swipe the source for --seconds. Returns the scoring window."""
    viewer = f"https://{dial}/?token={TOKEN}&target={urllib.parse.quote(target, safe='')}"
    opened = time.time()
    subprocess.run(["xcrun", "simctl", "openurl", args.viewer_sim, viewer], check=True, capture_output=True)
    start = first_report(trace, target, opened)
    if start is None:
        raise RuntimeError("Safari sent no WebTransport report within 20 s; see loupe.log and viewer.png")
    x, y1, y2, ms = (int(v) for v in args.swipe.split(","))
    every, n = duration(args.scroll_every), 0
    while time.time() < start + args.seconds:
        a, b = (y1, y2) if n % 2 == 0 else (y2, y1)
        bench_api(host, port, f"/api/input?target={target}",
                  {"action": "swipe", "x": x, "y": a, "x2": x, "y2": b, "ms": ms})
        n += 1
        time.sleep(every)
    shot(args.viewer_sim, os.path.join(out, "viewer.png"))
    return start, time.time()


def quit_safari(sim):
    subprocess.run(["xcrun", "simctl", "terminate", sim, "com.apple.mobilesafari"], capture_output=True)


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
    page = f"http://127.0.0.1:{PAGE_PORT}/index.html?run={time.time():.0f}" + ("&busy=1" if args.busy else "")
    safari = args.client == "safari"
    if safari:
        # A tab left from the last run would dial this run's loupe too, at
        # the same address and token, as a second viewer.
        quit_safari(args.viewer_sim)
    quit_safari(args.sim)
    subprocess.run(["xcrun", "simctl", "openurl", args.sim, page], check=True, capture_output=True)
    time.sleep(4)

    host = "127.0.0.1" if plan == "local" else args.ip
    loupe = spawn([
        build, "-addr", f"{host}:{port}",
        "-tls-cert", args.cert, "-tls-key", args.key,
        "-token", TOKEN, "-config", os.path.join(out, "config.json"),
        "-no-devices", "-no-ios-devices", "-shared-clipboard=false",
        "-synthetic-fallback=false", "-simcap", args.simcap, "-trace", trace,
    ], "loupe.log")
    shaper = None
    window = None
    try:
        if not wait_ready(f"https://{host}:{port}/healthz"):
            raise RuntimeError("bench loupe did not come up; see loupe.log")
        dial = f"{host}:{port}"
        if PLANS[plan] is not None:
            shaper = spawn([args.shaper] + (["-tcp"] if safari else []) +
                           ["-listen", f"{args.ip}:{sport}", "-to", f"{args.ip}:{port}",
                            "-plan", PLANS[plan], "-seed", "1"], "shaper.log")
            time.sleep(0.5)
            dial = f"{args.ip}:{sport}"
        shot(args.sim, os.path.join(out, "before.png"))
        t0 = time.time()
        load_start = load()
        if safari:
            window = drive_safari(args, host, port, dial, target, trace, out)
        else:
            url = f"https://{dial}/wt?target={target}&token={TOKEN}&start=1"
            probe = [args.probe, "-for", f"{args.seconds}s", "-acks", "-ack-hold", str(args.ack_hold),
                     "-scroll", args.scroll, "-scroll-every", args.scroll_every, "-tapafter", "2s",
                     "-summary", os.path.join(out, "probe.json"), "-jsonl", os.path.join(out, "frames.jsonl"), url]
            if transport == "quic":
                probe.insert(1, "-quic")
            with open(os.path.join(out, "probe.log"), "w") as f:
                subprocess.run(probe, stdout=f, stderr=subprocess.STDOUT, timeout=args.seconds + 60)
    finally:
        if safari:
            quit_safari(args.viewer_sim)
        for p in (shaper, loupe):
            if p and p.poll() is None:
                os.killpg(p.pid, signal.SIGTERM)
                try:
                    p.wait(10)
                except subprocess.TimeoutExpired:
                    os.killpg(p.pid, signal.SIGKILL)
        for f in logs:
            f.close()
        if safari and window:
            finish_safari(args, name, plan, target, trace, out, load_start, window)
    if safari:
        with open(os.path.join(out, "result.json")) as f:
            return json.load(f)
    shot(args.sim, os.path.join(out, "after.png"))
    res = {"build": name, "plan": plan, "transport": transport, "load": [load_start, load()]}
    try:
        with open(os.path.join(out, "probe.json")) as f:
            res["probe"] = json.load(f)
    except (OSError, ValueError):
        res["probe"] = None
    res["server"] = trace_summary(trace, target, t0)
    res["frames"] = frame_summary(os.path.join(out, "frames.jsonl"))
    with open(os.path.join(out, "result.json"), "w") as f:
        json.dump(res, f, indent=1)
    return res


def finish_safari(args, name, plan, target, trace, out, load_start, window):
    shot(args.sim, os.path.join(out, "after.png"))
    res = {"build": name, "plan": plan, "transport": "wt", "client": "safari", "load": [load_start, load()],
           "window": window, "safari": safari_summary(trace, target, *window),
           "server": trace_summary(trace, target, window[0])}
    with open(os.path.join(out, "result.json"), "w") as f:
        json.dump(res, f, indent=1)


def safari_invalid(sf, seconds):
    if sf.get("transports") != ["webtransport"]:
        return "  INVALID (not webtransport)"
    if sf.get("reports", 0) < seconds / 2:
        return "  INVALID (few reports)"
    return ""


def row_safari(r, seconds):
    sf, s = r.get("safari") or {}, r["server"]
    return (f"{r['build']:<10} {r['plan']:<7} safari"
            f" fps {sf.get('fpsP50', 0):>3.0f}/{sf.get('fpsP10', 0):>3.0f} low {sf.get('lowFps', 0):>2}s"
            f"  queue {sf.get('queueP50', 0):>5.0f}/{sf.get('queueP95', 0):>5.0f}/{sf.get('queueMax', 0):>5.0f}"
            f"  oldest {sf.get('oldestP50', 0):>5.0f}/{sf.get('oldestP95', 0):>5.0f}/{sf.get('oldestMax', 0):>5.0f}"
            f"  rotates {sf.get('rotates', 0):>2} starved {sf.get('starved', 0)} ({sf.get('starvedMs', 0) / 1000:.1f}s)  shed {sf.get('shed', 0):>4}  keys {sf.get('keysIn', 0):>3}"
            f"  decErr {sf.get('decodeErrors', 0)}  resyncs {sf.get('resyncs', 0)}"
            f"  cap {s.get('capP50kbps', 0):>5.0f}  src {s.get('srcFpsP50', 0):>3}fps"
            f"  load {max(l.get('load1', 0) for l in r.get('load') or [{}]):>4.1f}"
            f"  reports {sf.get('reports', 0)}{safari_invalid(sf, seconds)}")


def row(r):
    p, s, fs = r["probe"] or {}, r["server"], r.get("frames") or {}
    return (f"{r['build']:<10} {r['plan']:<7} {r['transport']:<5}"
            f" fps {p.get('fpsP50', 0):>3.0f}/{p.get('fpsP10', 0):>3.0f}"
            f"  stalls {fs.get('stalls', 0):>3} ({fs.get('stallMs', 0) / 1000:>4.1f}s)"
            f"  delay {fs.get('delayP50', 0):>4}/{fs.get('delayP95', 0):>5}"
            f"  settled {fs.get('settledP50', 0):>4}/{fs.get('settledP95', 0):>5}/{fs.get('settledMax', 0):>5}"
            f"  late {100 * p.get('late200Share', 0):>3.0f}%"
            f"  kbps {p.get('kbps', 0):>5.0f}"
            f"  big {fs.get('big', 0):>3} (max {fs.get('maxKB', 0):>3}KB)"
            f"  cap {s.get('capP50kbps', 0):>5.0f} (min {s.get('capMinKbps', 0):>4.0f})"
            f"  held {s.get('heldMs', 0) / 1000:>4.1f}s unstuck {s.get('unstuck', 0)}"
            f"  src {s.get('srcFpsP50', 0):>3}fps"
            f"  load {max(l.get('load1', 0) for l in r.get('load') or [{}]):>4.1f}"
            f"{'  INVALID' if r['plan'] != 'local' and s.get('srcFpsP50', 0) < 30 else ''}")


def rescore(argv):
    p = argparse.ArgumentParser(prog="stream-bench.py --rescore")
    p.add_argument("trace")
    p.add_argument("start", help="RFC 3339 time")
    p.add_argument("end", help="RFC 3339 time")
    p.add_argument("--sim", help="source simulator udid; default: every target")
    a = p.parse_args(argv)
    ts = [datetime.datetime.fromisoformat(v).timestamp() for v in (a.start, a.end)]
    print(json.dumps(safari_summary(a.trace, "sim:" + a.sim if a.sim else None, *ts), indent=1))


def main():
    if sys.argv[1:2] == ["--rescore"]:
        return rescore(sys.argv[2:])
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--sim", required=True, help="simulator udid nobody else is using")
    ap.add_argument("--ip", required=True, help="this Mac's tailnet IPv4")
    ap.add_argument("--build", action="append", required=True, help="name=path to a loupe binary")
    ap.add_argument("--plan", action="append", choices=sorted(PLANS), help="default: steady, dip, spike")
    ap.add_argument("--transport", action="append", choices=["wt", "quic"], help="default: wt and quic")
    ap.add_argument("--seconds", type=int, default=60)
    ap.add_argument("--busy", action="store_true", help="scroll the noisy page, which needs more than the link")
    ap.add_argument("--port", type=int, default=18556, help="bench loupe port; the shaper takes the next one")
    ap.add_argument("--ack-hold", type=int, default=0)
    # Negative units scroll the page down first; the runs alternate from there.
    ap.add_argument("--scroll", default="330,900,-8", help="x,y,units in stream pixels")
    # Pass 3s as well before a phone round: people swipe and pause, and a
    # continuous scroll never shows the first frame after a still screen.
    ap.add_argument("--scroll-every", help="default 700ms, or 3s with --client safari")
    ap.add_argument("--client", choices=["probe", "safari"], default="probe")
    ap.add_argument("--viewer-sim", help="simulator whose Safari is the viewer, with --client safari")
    ap.add_argument("--ca", help="root of --cert, trusted on --viewer-sim")
    ap.add_argument("--swipe", default="600,1900,700,120", help="x,y1,y2,ms in device pixels for --client safari")
    ap.add_argument("--simcap", required=True)
    ap.add_argument("--cert", required=True)
    ap.add_argument("--key", required=True)
    ap.add_argument("--out", default=os.path.join(ROOT, ".superpowers", "bench", "runs"))
    ap.add_argument("--probe", default=os.path.join(ROOT, ".build", "wtprobe"))
    ap.add_argument("--shaper", default=os.path.join(ROOT, ".build", "linkshaper"))
    args = ap.parse_args()
    if args.client == "safari":
        if not (args.viewer_sim and args.ca):
            ap.error("--client safari needs --viewer-sim and --ca")
        if args.transport and args.transport != ["wt"]:
            ap.error("--client safari is WebTransport only")
        if args.plan and "local" in args.plan:
            ap.error("--client safari cannot use plan local: Safari dials --ip")
        r = subprocess.run(["xcrun", "simctl", "keychain", args.viewer_sim, "add-root-cert", args.ca],
                           capture_output=True, text=True)
        if r.returncode:
            ap.error(f"could not add --ca to the viewer simulator: {r.stderr.strip()}")
    args.scroll_every = args.scroll_every or ("3s" if args.client == "safari" else "700ms")
    plans = args.plan or ["steady", "dip", "spike"]
    transports = ["wt"] if args.client == "safari" else args.transport or ["wt", "quic"]
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
                print("  " + (row_safari(r, args.seconds) if args.client == "safari" else row(r)), flush=True)
    if not results:
        return
    if args.client == "safari":
        print("\nfps p50/p10 while moving, low = seconds under 20 fps | queue, oldest = server send queue and oldest")
        print("unacked frame p50/p95/max ms | rotates = sessions replaced for stream credit; starved = frames waited")
        for r in results:
            print(row_safari(r, args.seconds))
        with open(os.path.join(args.out, stamp, "results.json"), "w") as f:
            json.dump(results, f, indent=1)
        return
    print("\nfps p50/p10 | stalls >200 ms | delay over best p50/p95/max ms | late = frames >200 ms over best")
    print("src = encoder frames/s p50; under 30 means the scroll did not drive the page, so the run is INVALID")
    print("cap = server remote cap p50 | held = encoder gate held | unstuck = stuck-viewer resyncs")
    for r in results:
        print(row(r))
    with open(os.path.join(args.out, stamp, "results.json"), "w") as f:
        json.dump(results, f, indent=1)


if __name__ == "__main__":
    sys.exit(main())
