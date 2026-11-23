#!/usr/bin/env python3
"""The flake log for the Chaos workflow (cleat#4169): every ATTEMPT of every run, as a table re-derived from the API, so "no flaky failures for about
three days" is a command anyone re-runs and not a sentence somebody wrote.

    scripts/chaos-flake-log.py [--since 2026-11-20T00:00:00Z] [--repo cleat-team/cleat] [--workflow chaos.yml] [--limit 200]
    scripts/chaos-flake-log.py --self-test

ONE ROW PER ATTEMPT, not per run. `gh run list` returns one row per run carrying the LATEST attempt's conclusion, so a run whose attempt 1 failed
and attempt 2 passed reads as a clean success: the canonical flake was invisible (cleat-review3, measured on run 44604503158, Ecosystem CI:
attempt 1 = failure, attempt 2 = success). For every run with attempt > 1 each earlier attempt k is read from /actions/runs/<id>/attempts/<k>,
and its failed cases from `gh run view <id> --attempt <k> --log-failed`.

A FLAKE is two attempts on one head that disagree; the "heads that DISAGREE" line counts attempts, so a re-run that passed is one.
A run still in progress is listed and not counted. A failed attempt whose log yields no failed case is UNMEASURED, never clean.
Exit 0 on a table, 2 on UNMEASURED (could not list the runs), 1 on a failed self-test.
"""
import collections
import datetime
import json
import os
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import importlib.util  # noqa: E402

_spec = importlib.util.spec_from_file_location("chaos_failed_cases", os.path.join(HERE, "chaos-failed-cases.py"))
failed_cases = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(failed_cases)


def gh(args):
    p = subprocess.run(["gh"] + args, capture_output=True, text=True)
    return p.returncode, p.stdout


def ts(s):
    return datetime.datetime.fromisoformat(s.replace("Z", "+00:00"))


def cases_of(repo, run_id, attempt, fetch):
    rc, out = fetch(["run", "view", str(run_id), "--repo", repo, "--attempt", str(attempt), "--log-failed"])
    if rc != 0:
        return "UNMEASURED (could not read the log)"
    events, rows = failed_cases.parse(out)
    if events == 0:
        return "UNMEASURED (the log held no go test -json event: read it by hand)"
    names = failed_cases.render(rows)
    if not names:
        return "UNMEASURED (the attempt failed but no failed case was found: read the log)"
    return "; ".join(names[:6]) + (" ..." if len(names) > 6 else "")


def attempt_rows(repo, run, fetch):
    """Rows for every attempt of one `gh run list` entry: the earlier attempts from the attempts API, the latest from the entry itself."""
    rid, latest = run["databaseId"], int(run.get("attempt") or 1)
    rows = []
    for k in range(1, latest):
        rc, out = fetch(["api", "repos/%s/actions/runs/%s/attempts/%d" % (repo, rid, k)])
        if rc != 0:
            rows.append((run["createdAt"], rid, k, run["event"], run["headSha"][:8], "UNMEASURED", 0, "could not read attempt %d" % k))
            continue
        a = json.loads(out)
        concl = a.get("conclusion") or a.get("status")
        secs = int((ts(a["updated_at"]) - ts(a.get("run_started_at") or a["created_at"])).total_seconds())
        cases = cases_of(repo, rid, k, fetch) if concl == "failure" else ""
        rows.append((a["created_at"], rid, k, run["event"], run["headSha"][:8], concl, secs, cases))
    concl = run["status"] if run["status"] != "completed" else run["conclusion"]
    secs = int((ts(run["updatedAt"]) - ts(run["createdAt"])).total_seconds())
    cases = cases_of(repo, rid, latest, fetch) if concl == "failure" else ""
    rows.append((run["updatedAt"], rid, latest, run["event"], run["headSha"][:8], concl, secs, cases))
    return rows


def summarize(rows):
    done = [r for r in rows if r[5] in ("success", "failure")]
    byhead = collections.defaultdict(set)
    for r in done:
        byhead[r[4]].add(r[5])
    disagree = sorted(h for h, v in byhead.items() if len(v) > 1)
    return done, disagree


def render(rows):
    out = ["| created | run | attempt | event | head | result | secs | failed cases |", "|---|---|---|---|---|---|---|---|"]
    out += ["| %s | %s | %s | %s | %s | %s | %s | %s |" % r for r in sorted(rows)]
    done, disagree = summarize(rows)
    out.append("")
    out.append("completed attempts: %d (success %d, failure %d); heads where attempts DISAGREE (a flake): %s" % (
        len(done), sum(1 for r in done if r[5] == "success"), sum(1 for r in done if r[5] == "failure"), ", ".join(disagree) or "none"))
    return "\n".join(out)


def self_test():
    """A run whose attempt 1 failed and attempt 2 passed: the shape the per-run listing hid. The canned API stands in for gh."""
    attempt1 = {"conclusion": "failure", "status": "completed", "created_at": "2026-11-19T20:02:20Z", "run_started_at": "2026-11-19T20:02:20Z", "updated_at": "2026-11-19T20:10:37Z"}
    log = open(os.path.join(HERE, "..", "tests", "chaos", "testdata", "failing-job-log.txt"), encoding="utf-8").read()

    def fetch(args):
        if args[0] == "api" and args[1].endswith("/attempts/1"):
            return 0, json.dumps(attempt1)
        if args[0] == "run" and "--attempt" in args and args[args.index("--attempt") + 1] == "1":
            return 0, log
        return 1, ""

    run = {"databaseId": 7, "attempt": 2, "event": "pull_request", "headSha": "0f0979c0aaaa", "status": "completed", "conclusion": "success",
           "createdAt": "2026-11-19T20:02:20Z", "updatedAt": "2026-11-19T22:36:50Z"}
    rows = attempt_rows("o/r", run, fetch)
    done, disagree = summarize(rows)
    bad = []
    if len(rows) != 2:
        bad.append("a run with 2 attempts gave %d rows" % len(rows))
    if [r[5] for r in sorted(rows, key=lambda r: r[2])] != ["failure", "success"]:
        bad.append("attempt conclusions %r, want failure then success" % [r[5] for r in rows])
    if disagree != ["0f0979c0"]:
        bad.append("heads that disagree: %r, want the head whose attempt 1 failed and attempt 2 passed" % disagree)
    if "TestEveryGoFixtureMatchesItsExpectedVerification" not in " ".join(r[7] for r in rows):
        bad.append("the failed attempt's cases were not read")
    # A failed attempt with an unreadable log is UNMEASURED, never clean.
    rows2 = attempt_rows("o/r", run, lambda a: (1, "") if a[0] == "run" else fetch(a))
    if not any("UNMEASURED" in r[7] for r in rows2):
        bad.append("an unreadable failed-attempt log was not reported UNMEASURED")
    # A single-attempt run is one row.
    one = dict(run, attempt=1)
    if len(attempt_rows("o/r", one, fetch)) != 1:
        bad.append("a one-attempt run did not give one row")
    if bad:
        print("SELF-TEST FAILED:\n  " + "\n  ".join(bad))
        return 1
    print("OK: chaos-flake-log self-test (a failure-then-success run is two attempts and a flake; unreadable log is UNMEASURED)")
    return 0


def main(argv):
    if argv == ["--self-test"]:
        return self_test()
    opts = {"--repo": "cleat-team/cleat", "--since": "", "--limit": "200", "--workflow": os.environ.get("CHAOS_WORKFLOW", "chaos.yml")}
    i = 0
    while i < len(argv):
        if argv[i] not in opts or i + 1 >= len(argv):
            print("usage: chaos-flake-log.py [--since ISO8601] [--repo owner/name] [--workflow file] [--limit N] | --self-test", file=sys.stderr)
            return 2
        opts[argv[i]] = argv[i + 1]
        i += 2
    repo = opts["--repo"]
    rc, out = gh(["run", "list", "--repo", repo, "--workflow", opts["--workflow"], "--limit", opts["--limit"],
                  "--json", "databaseId,attempt,event,headSha,conclusion,status,createdAt,updatedAt"])
    if rc != 0:
        print("UNMEASURED: gh run list failed", file=sys.stderr)
        return 2
    rows = []
    for run in json.loads(out):
        if opts["--since"] and run["createdAt"] < opts["--since"]:
            continue
        rows += attempt_rows(repo, run, gh)
    print(render(rows))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
