#!/usr/bin/env python3
"""Render a `go test -json` stream back to the text form `go test -v` prints (cleat#3949).

WHY THIS EXISTS. Tier 1's engine shards need a `go test -json` report, because
scripts/check-skip-budget.sh takes one (its usage line: `<job-key> <test-report.json>`)
and nothing else will do. But scripts/tier-gate.sh judges its run by grepping a TEXT log
for `^=== RUN`, `--- PASS`, `--- FAIL` and `--- SKIP`, and by naming individual skips
against `tier1.exclude_tests`. Running the suite twice to produce both is not an option,
and rewriting the gate's judging is a much larger change than this issue needs.

The JSON stream already carries the text: every line the test binary printed arrives as
an `Output` field, `=== RUN` and `--- FAIL` framing included. So this prints those fields
back, in order, and the log tier-gate.sh has always read is the same log.

WHAT IT IS NOT. It is not a judge. It does not count anything, decide anything, or drop
anything: a line it cannot parse is printed unchanged rather than skipped, because a
renderer that silently drops the odd line turns "the test printed a panic" into "the gate
found nothing" -- the failure mode both this repo's CLAUDE.md and tier2-gate.sh:367 are
about. The judge stays in tier-gate.sh, where it can still be read by someone who has
never heard of test2json.

Usage:
  scripts/test2json-text.py <report.json>          render to stdout
  scripts/test2json-text.py --self-test            offline check, exit 0/1
"""

import json
import sys


def render(stream, out):
    """Print each event's Output text; pass through anything that is not JSON."""
    for line in stream:
        stripped = line.rstrip("\n")
        if not stripped:
            continue
        try:
            ev = json.loads(stripped)
        except json.JSONDecodeError:
            # stderr can be interleaved with the JSON stream by a `2>&1`. Dropping these
            # is exactly the "a filter that can only see one kind of failure reports the
            # other kind as success" trap, so they are printed verbatim.
            out.write(stripped + "\n")
            continue
        if not isinstance(ev, dict):
            out.write(stripped + "\n")
            continue
        text = ev.get("Output")
        if text:
            # test2json already terminates each Output with a newline; writing it
            # verbatim is what makes the rendering byte-comparable to `go test -v`.
            out.write(text)


def self_test():
    import io

    # Fixtures are shaped like REAL go test -json events, copied from the form
    # tier2-gate.sh:199-204 uses, because a fixture invented from the docs would
    # test this script against my reading of the format rather than the format.
    fixture = "\n".join([
        '{"Action":"run","Package":"p","Test":"TestA"}',
        '{"Action":"output","Package":"p","Test":"TestA","Output":"=== RUN   TestA\\n"}',
        '{"Action":"output","Package":"p","Test":"TestA","Output":"    a_test.go:9: setting up\\n"}',
        '{"Action":"output","Package":"p","Test":"TestA","Output":"--- PASS: TestA (0.01s)\\n"}',
        '{"Action":"pass","Package":"p","Test":"TestA","Elapsed":0.01}',
        '{"Action":"run","Package":"p","Test":"TestB"}',
        '{"Action":"output","Package":"p","Test":"TestB","Output":"=== RUN   TestB\\n"}',
        '{"Action":"output","Package":"p","Test":"TestB","Output":"    b_test.go:3: no database\\n"}',
        '{"Action":"output","Package":"p","Test":"TestB","Output":"--- SKIP: TestB (0.00s)\\n"}',
        '{"Action":"skip","Package":"p","Test":"TestB","Elapsed":0}',
        '{"Action":"output","Package":"p","Output":"FAIL\\n"}',
        '{"Action":"fail","Package":"p","Elapsed":0.02}',
        # A build failure, which carries no Test field -- the shape CLAUDE.md records
        # as invisible to a count keyed on '"Test":'.
        '{"Action":"output","Package":"p","Output":"# p [p.test]\\n"}',
        '{"Action":"fail","Package":"p","Elapsed":0,"FailedBuild":"p [p.test]"}',
        # Not JSON at all: a stray line from a `2>&1` or a Go warning on stdout.
        "go: downloading example.com/x v1.2.3",
    ]) + "\n"

    out = io.StringIO()
    render(io.StringIO(fixture), out)
    text = out.getvalue()

    want = [
        "=== RUN   TestA",
        "    a_test.go:9: setting up",
        "--- PASS: TestA (0.01s)",
        "=== RUN   TestB",
        "    b_test.go:3: no database",
        "--- SKIP: TestB (0.00s)",
        "FAIL",
        "# p [p.test]",
        "go: downloading example.com/x v1.2.3",
    ]
    cases = []
    for w in want:
        cases.append((f"renders {w!r}", w in text))
    # The judging markers tier-gate.sh greps for, asserted as their own cases so that a
    # regression in the renderer names the count it broke rather than the whole text.
    cases.append(("a RUN marker reaches the log", text.count("=== RUN") == 2))
    cases.append(("one SKIP marker reaches the log", text.count("--- SKIP") == 1))
    cases.append(("one PASS marker reaches the log", text.count("--- PASS") == 1))
    cases.append(("a non-JSON line is preserved, not dropped", "go: downloading" in text))
    cases.append(("a Test-less event still contributes its Output", "# p [p.test]" in text))

    failed = [name for name, ok in cases if not ok]
    if failed:
        for name in failed:
            print(f"FAIL: {name}", file=sys.stderr)
        print(f"test2json-text: {len(cases) - len(failed)}/{len(cases)} self-test cases pass", file=sys.stderr)
        return 1
    print(f"OK: test2json-text renders {len(want)} shapes ({len(cases)} cases)")
    return 0


def main(argv):
    if len(argv) == 2 and argv[1] == "--self-test":
        return self_test()
    if len(argv) != 2:
        print("usage: test2json-text.py <report.json> | --self-test", file=sys.stderr)
        return 1
    with open(argv[1], encoding="utf-8", errors="replace") as f:
        render(f, sys.stdout)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
