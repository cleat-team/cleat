#!/usr/bin/env python3
"""Names the failed test cases in a `go test -json` job log, as `gh run view --log-failed` prints it (cleat#4169).

Reads the log on stdin, prints one failure per line:
    TEST    <package> <test name>      a test failed
    PACKAGE <package>                  a package failed with no test named: it did not build, or it timed out
    TIMEOUT <package>                  `panic: test timed out`

Exit 0 when it parsed the log (zero failures is a legitimate answer for a passing log), 2 when the log held no `go test -json` event at all:
a log it could not read says nothing, and "no failures" from it would be the false green this exists to prevent.

`--self-test` runs it against tests/chaos/testdata/failing-job-log.txt, which is REAL output: lines copied from a failed CI job
(cleat 44596704364, `Test Go (commands cleat 2 of 2)`), including the `<job>\\t<step>\\t<timestamp> ` prefix gh adds and one test failure and
one package-level failure with no test. That fixture is the known-positive: a parser that silently found nothing would fail it.
"""
import json
import os
import sys


def parse(text):
    events = 0
    out = []
    for line in text.splitlines():
        i = line.find('{"')
        if i < 0:
            continue
        try:
            ev = json.loads(line[i:])
        except ValueError:
            continue
        if not isinstance(ev, dict) or "Action" not in ev:
            continue
        events += 1
        pkg, test, act = ev.get("Package", "?"), ev.get("Test"), ev["Action"]
        if act == "fail":
            out.append(("TEST", pkg, test) if test else ("PACKAGE", pkg, None))
        elif act == "output" and "panic: test timed out" in ev.get("Output", ""):
            out.append(("TIMEOUT", pkg, None))
    return events, out


def render(rows):
    return ["%s %s%s" % (k, p, (" " + t) if t else "") for k, p, t in rows]


def self_test():
    here = os.path.dirname(os.path.abspath(__file__))
    fixture = os.path.join(here, "..", "tests", "chaos", "testdata", "failing-job-log.txt")
    text = open(fixture, encoding="utf-8").read()
    events, rows = parse(text)
    want = [
        ("TEST", "github.com/cleat-team/cleat/cmd/cleat", "TestEveryGoFixtureMatchesItsExpectedVerification"),
        ("PACKAGE", "github.com/cleat-team/cleat/cmd/cleat", None),
    ]
    bad = []
    if rows != want:
        bad.append("real failing log: got %r, want %r" % (rows, want))
    # A log with no go test events must be UNREADABLE, never "no failures".
    ev2, rows2 = parse("some unrelated build output\nanother line\n")
    if ev2 != 0 or rows2:
        bad.append("an unrelated log parsed as events=%d rows=%r" % (ev2, rows2))
    # A passing -json log parses to zero failures and a non-zero event count.
    ev3, rows3 = parse('x\ty\t2026 {"Action":"pass","Package":"p","Test":"T","Elapsed":1}\n')
    if ev3 != 1 or rows3:
        bad.append("a passing log parsed as events=%d rows=%r" % (ev3, rows3))
    # A timeout is named as one.
    ev4, rows4 = parse('{"Action":"output","Package":"p","Output":"panic: test timed out after 5m0s\\n"}\n')
    if rows4 != [("TIMEOUT", "p", None)]:
        bad.append("a timeout parsed as %r" % (rows4,))
    if bad:
        print("SELF-TEST FAILED:\n  " + "\n  ".join(bad))
        return 1
    print("OK: chaos-failed-cases self-test (real failing log: %d events, 1 test + 1 package failure; unreadable log refused; pass and timeout cases)" % events)
    return 0


if __name__ == "__main__":
    if sys.argv[1:] == ["--self-test"]:
        sys.exit(self_test())
    events, rows = parse(sys.stdin.read())
    if events == 0:
        print("UNMEASURED: no `go test -json` event in the log; this is a failure of the parse, not a finding about the run", file=sys.stderr)
        sys.exit(2)
    print("\n".join(render(rows)))
