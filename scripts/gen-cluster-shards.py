#!/usr/bin/env python3
"""Regenerate the Cluster Integration Tests shard lists (cleat#3935).

WHY THESE FILES EXIST. `engine` is ONE package with ~3,800 top-level tests, so `go test ./engine/...` cannot be split by
package the way `cmd/...` is. It splits by test NAME, and 1,265 names per shard cannot live in a YAML `case` -- so each
shard's names live in a checked-in file here and the job builds a `-run` regex from it.

HOW THE SPLIT IS MADE, in two passes, because one is not enough:

  1. the tests at or above --long-seconds, longest first, each onto the shard with the least test time so far;
  2. everything else, onto the shard with the FEWEST TESTS first, with time as the tie-break.

Pass 2 is not a refinement. Assigning by time alone puts the thousands of near-zero durations into whichever shard wins
the first tie, which produced shards of 3,126 and 335 tests on the first attempt -- even time and a five-fold count
imbalance, and the runner pays per-test overhead that a duration of 0.00s does not show.

The durations come from a real `-json` report, never from a division of the job's total: cleat#3939's projection divided
802.9s by three and predicted ~268s a shard against a measured 302-306s, because a per-shard fixed cost lands inside the
package timing. Feed this the report from a run with the databases attached, or the split is a guess.

    usage: scripts/gen-cluster-shards.py <report.json> [--shards 3] [--package engine] [--long-seconds 1.0]

`<report.json>` may be `go test -json` output, or a CI job log containing it (the parser looks for the JSON object on each
line, so a log with timestamps works). Only top-level tests are counted; a subtest's duration is already inside its
parent's.

It writes scripts/cluster-shards/shard-<n>.txt and prints the per-shard totals it printed into that header.
"""

import argparse
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(HERE, "cluster-shards")


def read_durations(path, package_suffix):
    """Top-level test name -> last recorded Elapsed, for the wanted package."""
    best = {}
    with open(path, errors="replace") as f:
        for line in f:
            i = line.find("{")
            if i < 0 or '"Action"' not in line:
                continue
            try:
                ev = json.loads(line[i:])
            except ValueError:
                continue
            if ev.get("Action") not in ("pass", "fail"):
                continue
            name, pkg = ev.get("Test"), ev.get("Package", "")
            if not name or "/" in name or not pkg.endswith(package_suffix):
                continue
            best[name] = max(best.get(name, 0.0), float(ev.get("Elapsed") or 0.0))
    return best


def partition(tests, shards, long_seconds):
    load, count, assign = [0.0] * shards, [0] * shards, [[] for _ in range(shards)]
    long_t = sorted([kv for kv in tests.items() if kv[1] >= long_seconds], key=lambda kv: -kv[1])
    short_t = sorted([kv for kv in tests.items() if kv[1] < long_seconds], key=lambda kv: -kv[1])
    for name, d in long_t:
        i = min(range(shards), key=lambda k: (load[k], count[k]))
        assign[i].append((name, d)); load[i] += d; count[i] += 1
    for name, d in short_t:
        i = min(range(shards), key=lambda k: (count[k], load[k]))
        assign[i].append((name, d)); load[i] += d; count[i] += 1
    return assign, load, count


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("report")
    ap.add_argument("--shards", type=int, default=3)
    ap.add_argument("--package", default="engine", help="suffix of the package to shard (default: engine)")
    ap.add_argument("--long-seconds", type=float, default=1.0)
    a = ap.parse_args()

    tests = read_durations(a.report, a.package)
    if not tests:
        # A split generated from nothing would be three empty lists that pass every count check downstream.
        sys.exit(f"UNMEASURED: no top-level {a.package} test durations found in {a.report}\n"
                 f"  (this is a failure of the generator's input, not a finding about the suite)")
    assign, load, count = partition(tests, a.shards, a.long_seconds)
    os.makedirs(OUT, exist_ok=True)

    total = sum(tests.values())
    for k, rows in enumerate(assign, start=1):
        path = os.path.join(OUT, f"shard-{k}.txt")
        with open(path, "w") as f:
            f.write(f"# Shard {k} of {a.shards} for Cluster Integration Tests (cleat#3935). GENERATED -- do not hand-edit.\n")
            f.write(f"# Regenerate: scripts/gen-cluster-shards.py <report.json> --shards {a.shards}\n")
            f.write(f"# Source: {len(tests)} top-level {a.package} tests, {total:.1f}s of test time in the report.\n")
            f.write(f"# This shard: {len(rows)} tests, {load[k-1]:.1f}s.\n")
            f.write("# name<TAB>measured-seconds, longest first. The job builds one -run regex from column 1.\n")
            for name, d in sorted(rows, key=lambda r: -r[1]):
                f.write(f"{name}\t{d:.2f}\n")
        print(f"shard-{k}.txt: {len(rows)} tests, {load[k-1]:.1f}s")
    print(f"total {total:.1f}s over {len(tests)} tests; time spread {max(load)-min(load):.1f}s, "
          f"count spread {max(count)-min(count)}")


if __name__ == "__main__":
    main()
