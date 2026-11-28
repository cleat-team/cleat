#!/usr/bin/env python3
"""Render the tracking-issue text for a failed soak run (cleat#3736).

The weekly soak failed on 2026-11-02 and 2026-11-09 and nothing told anyone; it was found eleven days
later by reading a run history. The `report-failure` job in .github/workflows/soak.yml opens an issue,
or comments on the open one, when the soak job fails. This script is the half of that job that decides
WHAT the issue says. The other half, which opens or updates the issue, is the github-script step in the
workflow, and is proved live rather than here.

  soak-failure-report.py --print-title
  soak-failure-report.py --report soak-report.json --run-url URL --attempt N --sha SHA \\
                         --event EVENT --duration D --body-out FILE
  soak-failure-report.py --self-test

WHAT IT READS. soak-report.json is `go test -json` piped through `tee`, so it is JSON lines, one event
each, plus whatever the toolchain wrote to stderr (a compile error is not JSON). The report may be ABSENT
-- the job can fail before the test ever runs (the Postgres service, checkout, the build) -- and that is
a state with its own wording, not an empty list of failures. The job can also fail with a GREEN report:
`Skip budget` runs after the test and fails the job on its own. Those three are different readers'
problems and the body says which one it is.

WHAT IT SHOWS, and why not simply "every output line of the failing test": TestSoakWorkflowMix prints a
progress line every thirty seconds for the length of the run, so its output is mostly noise. The part a
reader wants is everything from `=== Soak Test Complete ===` onward -- the totals, then each distinct
workload error, then any leak finding -- so that is what is taken for a test that printed the marker.
A test that did not (killed by the 90-minute timeout, or any other test that failed) gets the lines
around its failure instead. Duplicates are dropped and the whole is capped, because the body is a public
issue and GitHub refuses one over 65,536 characters.

EXIT STATUS. 0 passed, 1 a finding (--self-test only: an expectation failed), 2 could not establish what
was being measured (a fixture is missing). They are different statuses on purpose: a check that measured
nothing must not be able to agree with every tree.
"""

import argparse
import json
import os
import re
import sys

# The ONE copy of the title. The workflow reads it through --print-title and never spells it again, so
# the dedupe key cannot drift from the title that was filed.
TITLE = "Soak test is failing"

MARKER = "=== Soak Test Complete ==="
MAX_LINES = 40  # lines shown in the failing-lines block
MAX_LINE = 300  # characters kept of any one line
MAX_BODY = 60000  # GitHub's limit is 65536; leave room for what a comment adds

FIXTURES = os.path.join(os.path.dirname(os.path.abspath(__file__)), "testdata", "soak-failure-report")


def parse(path):
    """Return (events, other_lines, state). state is 'absent', 'empty' or 'ok'."""
    if not path or not os.path.isfile(path):
        return [], [], "absent"
    events, other = [], []
    with open(path, encoding="utf-8", errors="replace") as f:
        for raw in f:
            line = raw.rstrip("\n")
            if not line.strip():
                continue
            if line.lstrip().startswith("{"):
                try:
                    events.append(json.loads(line))
                    continue
                except ValueError:
                    pass
            other.append(line)
    if not events and not other:
        return [], [], "empty"
    return events, other, "ok"


def clean(text):
    """One display line: no go-test file:line prefix, no trailing space, bounded."""
    text = re.sub(r"^\s*\S+\.go:\d+:\s?", "", text.rstrip("\n"))
    text = text.strip()
    if len(text) > MAX_LINE:
        text = text[: MAX_LINE - 1] + "…"
    return text


def outputs(events, test=None, no_test=False):
    """The Output strings of events, split to single lines, in order."""
    lines = []
    for e in events:
        if e.get("Action") != "output":
            continue
        if no_test and e.get("Test"):
            continue
        if test is not None and e.get("Test") != test:
            continue
        for part in e.get("Output", "").split("\n"):
            if part.strip():
                lines.append(part)
    return lines


def distinct(lines):
    seen, out = set(), []
    for line in lines:
        k = clean(line)
        if k and k not in seen:
            seen.add(k)
            out.append(k)
    return out


def summarise(events):
    """What failed, in the terms a reader needs. Pure function of the events."""
    failed_tests = []
    for e in events:
        if e.get("Action") == "fail" and e.get("Test") and e["Test"] not in failed_tests:
            failed_tests.append(e["Test"])
    # A subtest failure is reported with its parent; the parent is the one worth naming first.
    failed_tests.sort(key=lambda t: t.count("/"))

    panic = []
    all_lines = outputs(events)
    for i, line in enumerate(all_lines):
        if line.startswith("panic:"):
            panic.append(clean(line))
            # `running tests:` and the indented test names under it, until the goroutine dump.
            for follow in all_lines[i + 1 : i + 6]:
                if follow.strip() == "" or follow.lstrip().startswith("goroutine"):
                    break
                panic.append(clean(follow))
            break

    shown = []
    for t in failed_tests:
        lines = outputs(events, test=t)
        marker = next((i for i, l in enumerate(lines) if MARKER in l), None)
        if marker is not None:
            body = lines[marker:]
        elif panic:
            body = []  # the panic block says it better than the progress lines before it
        else:
            body = lines[-15:]
        for l in body:
            if re.match(r"^\s*--- (FAIL|PASS|SKIP)", l) or re.match(r"^=== ", l):
                continue
            shown.append(l)
    shown = distinct(shown)

    # Output that belongs to no test: the package FAIL line and, for a build that did not
    # compile, the compiler's own complaint.
    pkg = [clean(l) for l in outputs(events, no_test=True)]
    pkg = [l for l in distinct(pkg) if l not in ("FAIL", "PASS")]

    return {
        "failed_tests": failed_tests,
        "shown": shown,
        "panic": panic,
        "pkg": pkg,
        "has_failure": bool(failed_tests or panic or any(l.startswith("FAIL") for l in pkg)),
    }


def fence(lines):
    """A code block that the lines inside it cannot close."""
    safe = [l.replace("```", "'''") for l in lines]
    return ["```"] + safe + ["```"]


def render(path, run_url, attempt, sha, event, duration):
    events, other, state = parse(path)
    out = [
        f"Run: {run_url} (attempt {attempt})",
        f"Commit: {sha}",
        f"Trigger: {event}" + (f", duration {duration}" if duration else ""),
        "",
    ]

    if state != "ok":
        reason = "was not attached to the run" if state == "absent" else "was attached and is empty"
        out += [
            f"The soak report {reason}, so there are no failing lines to show. The job most likely "
            "failed before the test ran (the Postgres service, checkout, the build) or while "
            "starting it. Read the job log on the run above.",
        ]
    elif not events:
        out += [
            "The soak report is not `go test -json`, so the failure could not be parsed. Its first lines:",
            *fence([clean(l) for l in other[:20]]),
        ]
    else:
        s = summarise(events)
        if not s["has_failure"]:
            out += [
                "The job failed but the soak report shows no failing test: the test passed, so the "
                "failure is in another step of the job (for example the skip budget, or the upload). "
                "Read the job log on the run above.",
            ]
        else:
            if s["failed_tests"]:
                out += ["Failing: " + ", ".join(f"`{t}`" for t in s["failed_tests"]), ""]
            lines = s["panic"] + s["shown"] + [l for l in s["pkg"] if l not in s["panic"]]
            lines = distinct(lines)
            extra = len(lines) - MAX_LINES
            if lines:
                out += [
                    "Distinct failing lines from the end-of-run summary:",
                    *fence(lines[:MAX_LINES]),
                ]
                if extra > 0:
                    out += [f"({extra} more distinct line(s) not shown; they are in the run's `soak-report` artifact.)"]
            if other:
                out += ["", "Non-JSON output in the report (a compile error is not JSON):",
                        *fence([clean(l) for l in other[:10]])]

    out += [
        "",
        "Opened by the `report-failure` job in `.github/workflows/soak.yml` (cleat#3736). A later "
        "failure is added here as a comment; close this issue when the soak is green again.",
    ]
    body = "\n".join(out)
    if len(body) > MAX_BODY:
        body = body[: MAX_BODY - 80].rstrip() + "\n\n(truncated: the body hit the issue size limit)"
        if body.count("```") % 2:
            body += "\n```"
    return body


# ---------------------------------------------------------------------------------------------------


def self_test():
    problems = []

    def need(cond, what):
        if not cond:
            problems.append(what)

    def fixture(name):
        p = os.path.join(FIXTURES, name)
        if not os.path.isfile(p):
            print(f"UNMEASURED: fixture {p} is missing. This is a failure of the check, not a finding "
                  "about soak-failure-report.py.")
            sys.exit(2)
        return p

    args = ("https://github.com/o/r/actions/runs/1", "2", "a" * 40, "schedule", "1h")

    # The cleat#3733 failure, real: ten distinct workload errors and `Failed: 10`.
    a = render(fixture("failing-workload-errors.jsonl"), *args)
    need("`TestSoakWorkflowMix`" in a, "the failing test is not named")
    need("Failed: 10" in a, "the end-of-run totals are missing")
    # Ten events, nine distinct lines: two are byte-identical (same worker, same workflow id), measured
    # with `grep | sort -u` and not with this script. `Failed: 10` above still carries the raw count.
    need(a.count("workload error:") == 9, f"expected 9 distinct workload errors, got {a.count('workload error:')}")
    need("converting NULL to string is unsupported" in a, "the cause of the workload errors is missing")
    need("[sample" not in a, "progress lines leaked into the failing lines")
    need("FAIL\tgithub.com/cleat-team/cleat/tests/soak" in a, "the package FAIL line is missing")
    need("soak_test.go:" not in a, "a go-test file:line prefix was not removed")
    need(a.count("```") == 2, "the code fence is not exactly one block")
    need("Run: https://github.com/o/r/actions/runs/1 (attempt 2)" in a, "the run key is missing or has no attempt")

    # The 90-minute timeout, real: no summary, a panic.
    b = render(fixture("timeout-panic.jsonl"), *args)
    need("panic: test timed out after 1h30m0s" in b, "the timeout panic is missing")
    need("TestSoakWorkflowMix (1h30m0s)" in b, "the running test under the panic is missing")
    need("[sample" not in b, "progress lines leaked into the timeout report")

    # The control: a passing report must not be reported as a failing one. The job can still have
    # failed in a later step, and the body must say that instead of inventing a failing test.
    c = render(fixture("green.jsonl"), *args)
    need("shows no failing test" in c, "a green report was not described as green")
    need("Failing:" not in c and "```" not in c, "a green report produced failing lines")

    # A report that is not there is a different state from a report with no failures.
    d = render(os.path.join(FIXTURES, "no-such-file.jsonl"), *args)
    need("was not attached" in d, "an absent report was not described as absent")
    need("Failing:" not in d and "shows no failing test" not in d, "an absent report read as a report")

    # A report that is not go test -json (a compile error) is shown, not swallowed.
    tmp = os.path.join(os.environ.get("TMPDIR", "/tmp"), f"soak-failure-report-selftest-{os.getpid()}.txt")
    try:
        with open(tmp, "w") as f:
            f.write("# github.com/cleat-team/cleat/tests/soak\ntests/soak/soak_test.go:10:2: undefined: nope\n")
        e = render(tmp, *args)
        need("not `go test -json`" in e and "undefined: nope" in e, "a non-JSON report was not shown")

        # Truncation: 500 distinct failing lines must stay under the issue limit with one fence.
        with open(tmp, "w") as f:
            f.write(json.dumps({"Action": "output", "Test": "T", "Output": MARKER + "\n"}) + "\n")
            for i in range(500):
                f.write(json.dumps({"Action": "output", "Test": "T", "Output": f"x_test.go:1: workload error {i} " + "y" * 200 + "\n"}) + "\n")
            f.write(json.dumps({"Action": "fail", "Test": "T"}) + "\n")
        big = render(tmp, *args)
        need(len(big) <= MAX_BODY, f"the body is {len(big)} characters, over {MAX_BODY}")
        need("more distinct line(s) not shown" in big, "the cap was not announced")
        need(big.count("```") % 2 == 0, "the cap left an unclosed code fence")

        # A line that holds a fence must not be able to close ours.
        with open(tmp, "w") as f:
            f.write(json.dumps({"Action": "output", "Test": "T", "Output": MARKER + "\n"}) + "\n")
            f.write(json.dumps({"Action": "output", "Test": "T", "Output": "x_test.go:1: has ``` inside\n"}) + "\n")
            f.write(json.dumps({"Action": "fail", "Test": "T"}) + "\n")
        g = render(tmp, *args)
        need(g.count("```") == 2, "a fence inside a failing line broke the code block")
    finally:
        if os.path.exists(tmp):
            os.remove(tmp)

    need(TITLE == "Soak test is failing", "the title changed; the workflow and any open issue key on it")

    if problems:
        print("FAIL: soak-failure-report.py --self-test")
        for p in problems:
            print("  -", p)
        sys.exit(1)
    print("self-test: all controls passed")


def main(argv):
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--print-title", action="store_true")
    ap.add_argument("--self-test", action="store_true")
    ap.add_argument("--report")
    ap.add_argument("--run-url")
    ap.add_argument("--attempt", default="1")
    ap.add_argument("--sha", default="")
    ap.add_argument("--event", default="")
    ap.add_argument("--duration", default="")
    ap.add_argument("--body-out")
    a = ap.parse_args(argv)

    if a.print_title:
        print(TITLE)
        return 0
    if a.self_test:
        self_test()
        return 0
    if not a.run_url or not a.body_out:
        ap.error("--run-url and --body-out are required")
    body = render(a.report, a.run_url, a.attempt, a.sha, a.event, a.duration)
    with open(a.body_out, "w", encoding="utf-8") as f:
        f.write(body + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
