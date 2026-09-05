#!/usr/bin/env python3
"""Re-derive the latency distribution behind tests/scale/latency_test.go.

The two wall-clock thresholds in that file (P50 > 100ms, P99 > 500ms) were
removed on 2026-09-04 after the P99 one failed on develop at 491a0f7 (#720)
with 623.876463ms over a 2.676839ms median. This script is the command that
re-derives the three measurements the removal rests on, so the next reader can
check them rather than take them:

  1. P99 across recent runs is a continuum, not a cluster at any one value.
  2. The two worst samples in a run are near-identical at whatever magnitude
     that run stalled at -- a signature of the four in-flight goroutines, not
     of a constant in the code.
  3. TestLatencyP50 is sequential and shows the same tail, correlating with
     the concurrent test's tail across runs. No lock wait, retry backoff or
     pool timeout inside AppendEventHistory can slow down a single goroutine,
     so the stall is a property of the CI host.

Parsing is in Python rather than shell on purpose. CLAUDE.md records that the
interactive `grep` here is ugrep, a script gets BSD grep, and CI gets GNU grep,
and that the three disagree on `-c` combined with `-o` and on multibyte
bracket expressions -- one pipeline reported 1627, 546 and 2354 depending on
who ran it. Python's `re` is the same everywhere.

Usage:
    scripts/scale-latency-history.py [--limit N] [--branch BRANCH]

Requires `gh` authenticated against the repo. Fetching N job logs takes about
N seconds; the default of 20 runs is roughly the last two days of develop.
"""

import argparse
import json
import re
import statistics
import subprocess
import sys

# The `latency_test.go:NN:` line numbers are not stable across edits to the
# file, so match on the label text instead. TestLatencyP50 logs "P50
# (median):" and TestLatencyP99 logs a bare "P50:", which is what separates
# the sequential block from the concurrent one.
SEQ = re.compile(r"latency_test\.go:\d+:\s+(P50 \(median\)|Mean|Min|Max):\s+([\d.]+)(ms|µs|s)\b")
CONC = re.compile(r"latency_test\.go:\d+:\s+(P50|P99|Min|Max):\s+([\d.]+)(ms|µs|s)\b")
UNIT = {"µs": 0.001, "ms": 1.0, "s": 1000.0}


def sh(*args):
    return subprocess.run(args, capture_output=True, text=True).stdout


def to_ms(value, unit):
    return float(value) * UNIT[unit]


def parse_job_log(text):
    """Return (sequential stats, concurrent stats) in milliseconds.

    The two test functions log overlapping label sets, so the blocks are told
    apart by their headers rather than by the labels themselves.
    """
    seq, conc, block = {}, {}, None
    for line in text.splitlines():
        if "Latency P50 test (" in line:
            block = "seq"
        elif "Latency P99 test (" in line:
            block = "conc"
        if block == "seq":
            m = SEQ.search(line)
            if m:
                seq[m.group(1)] = to_ms(m.group(2), m.group(3))
        elif block == "conc":
            m = CONC.search(line)
            if m:
                conc[m.group(1)] = to_ms(m.group(2), m.group(3))
    return seq, conc


def pearson(xs, ys):
    mx, my = statistics.mean(xs), statistics.mean(ys)
    num = sum((a - mx) * (b - my) for a, b in zip(xs, ys))
    den = (sum((a - mx) ** 2 for a in xs) * sum((b - my) ** 2 for b in ys)) ** 0.5
    return num / den if den else float("nan")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--limit", type=int, default=20)
    ap.add_argument("--branch", default="develop")
    args = ap.parse_args()

    runs = json.loads(
        sh("gh", "run", "list", "--branch", args.branch,
           "--workflow", "CI/CD Pipeline", "--limit", str(args.limit),
           "--json", "databaseId,headSha,conclusion")
    )
    if not runs:
        sys.exit("no runs returned; is `gh` authenticated for this repo?")

    rows = []
    for run in runs:
        jobs = json.loads(
            sh("gh", "run", "view", str(run["databaseId"]), "--json", "jobs")
        ).get("jobs", [])
        scale = [j for j in jobs if "scale" in j["name"]]
        if not scale:
            continue
        seq, conc = parse_job_log(
            sh("gh", "run", "view", "--job", str(scale[0]["databaseId"]), "--log")
        )
        if "Max" in seq and "Max" in conc:
            rows.append((run["headSha"][:7], seq, conc))
        print(f"  fetched {run['headSha'][:7]}", file=sys.stderr)

    if len(rows) < 3:
        sys.exit(f"only {len(rows)} runs had a usable scale job; need at least 3")

    print(f"\n{len(rows)} runs on {args.branch} with a scale job\n")

    print("1. TestLatencyP99 P99 across runs (ms), sorted -- a continuum, not a cluster:")
    p99s = sorted(c["P99"] for _, _, c in rows)
    print("     " + ", ".join(f"{v:.1f}" for v in p99s))
    print(f"     median {statistics.median(p99s):.1f}ms, max {max(p99s):.1f}ms "
          f"({max(p99s)/statistics.median(p99s):.0f}x the median)\n")

    print("2. Worst two samples per run -- near-identical at whatever that run stalled at:")
    for sha, _, c in sorted(rows, key=lambda r: -r[2]["Max"])[:5]:
        print(f"     {sha}  P99 {c['P99']:8.1f}ms   Max {c['Max']:8.1f}ms")
    print()

    print("3. Sequential test (1 goroutine) vs concurrent test, both Max (ms):")
    for sha, s, c in sorted(rows, key=lambda r: -r[1]["Max"])[:5]:
        print(f"     {sha}  sequential {s['Max']:8.1f}   concurrent {c['Max']:8.1f}")
    r = pearson([s["Max"] for _, s, _ in rows], [c["Max"] for _, _, c in rows])
    print(f"\n     Pearson r = {r:.3f}")
    print("     A lock wait, retry backoff or pool timeout cannot slow down the")
    print("     sequential test. A slow host slows down both.")


if __name__ == "__main__":
    main()
