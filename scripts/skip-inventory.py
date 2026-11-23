#!/usr/bin/env python3
"""Print what actually skipped across Tier 1's engine shards (cleat#3949, step 1).

REPORT ONLY. This decides nothing and fails nothing. Its job is to supply the numbers
that `scripts/skip-ledger.tsv` needs before a budget can be turned on for this job: a
budget line is `<job key>\t<count>\t<test-name regex>\t<why>`, the count and the names
are whatever this prints, and the `why` is the one thing a machine cannot supply.

WHY A NEW KEY IS NEEDED, rather than reusing `test-go/engine`. check-skip-budget.sh fails
any ledger line that matches ZERO skips ("a grant covering something that is not there"),
and the existing lines were derived under `engine-race.yml`'s PostgreSQL-only DSN set.
These shards set all three dialects, so every line whose reason is "MySQL is not
available" would read zero here and fail a healthy run. Measured, not assumed: a report
with no engine tests run against `test-go/engine` fails on all 261 lines.

WHAT IT COUNTS. Top-level tests only, the same identity rule check-skip-budget.sh and
tier2-gate.sh use: a subtest reports separately, and counting subtests as skips inflates
the budget with names no ledger line would ever be written for. A test that skipped is
one whose final action is `skip`.

Usage:
  scripts/skip-inventory.py <report.json> [<report2.json> ...]
  scripts/skip-inventory.py --self-test
"""

import json
import sys


def inventory(paths, out):
    counts = {"tests": 0, "pass": 0, "fail": 0, "skip": 0}
    skipped = []
    malformed = 0
    for path in paths:
        with open(path, encoding="utf-8", errors="replace") as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                try:
                    ev = json.loads(line)
                except json.JSONDecodeError:
                    malformed += 1
                    continue
                test = ev.get("Test")
                action = ev.get("Action")
                if not test or action not in ("pass", "fail", "skip"):
                    continue
                if "/" in test:
                    continue  # a subtest; its parent carries the verdict
                counts["tests"] += 1
                counts[action] += 1
                if action == "skip":
                    skipped.append((ev.get("Package", ""), test))
    skipped.sort()
    out.write("skip inventory (report only, from %d report(s)): "
              "tests=%d pass=%d fail=%d skip=%d malformed-lines=%d\n"
              % (len(paths), counts["tests"], counts["pass"], counts["fail"],
                 counts["skip"], malformed))
    for pkg, name in skipped:
        out.write("  skip\t%s\t%s\n" % (pkg, name))
    return counts


def self_test():
    import io
    import tempfile
    import os

    # Real-shaped events, the same form check-skip-budget.sh's own fixtures use: a real
    # `go test -json` run, not a hand-written approximation of the format.
    fixture = "\n".join([
        '{"Action":"pass","Package":"p","Test":"TestRuns","Elapsed":0.1}',
        '{"Action":"skip","Package":"p","Test":"TestNoMySQL","Elapsed":0}',
        '{"Action":"pass","Package":"p","Test":"TestParent","Elapsed":0.2}',
        '{"Action":"skip","Package":"p","Test":"TestParent/child","Elapsed":0}',
        '{"Action":"fail","Package":"p","Test":"TestBroken","Elapsed":0.3}',
        "not json at all",
    ]) + "\n"
    fd, path = tempfile.mkstemp(suffix=".json")
    try:
        with os.fdopen(fd, "w") as f:
            f.write(fixture)
        out = io.StringIO()
        counts = inventory([path], out)
        text = out.getvalue()
    finally:
        os.unlink(path)

    cases = [
        ("counts the top-level tests only", counts["tests"] == 4),
        ("counts one skip", counts["skip"] == 1),
        ("counts two passes, one per top-level parent", counts["pass"] == 2),
        ("counts one fail", counts["fail"] == 1),
        ("names the skipped test", "skip\tp\tTestNoMySQL" in text),
        ("does not count the subtest as a skip", "TestParent/child" not in text),
        ("reports malformed lines rather than dropping them silently",
         "malformed-lines=1" in text),
    ]
    failed = [n for n, ok in cases if not ok]
    if failed:
        for n in failed:
            print("FAIL: %s" % n, file=sys.stderr)
        print("skip-inventory: %d/%d self-test cases pass" % (len(cases) - len(failed), len(cases)),
              file=sys.stderr)
        return 1
    print("OK: skip-inventory reads %d shapes (%d cases)" % (len(cases), len(cases)))
    return 0


def main(argv):
    if len(argv) == 2 and argv[1] == "--self-test":
        return self_test()
    if len(argv) < 2:
        print("usage: skip-inventory.py <report.json> [...] | --self-test", file=sys.stderr)
        return 1
    inventory(argv[1:], sys.stdout)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
