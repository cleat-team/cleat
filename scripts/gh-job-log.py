#!/usr/bin/env python3
"""Fetch one Actions job's log -- without composing the call (cleat#3132).

WHAT THIS REPLACES. The call it wraps is

    gh api repos/O/R/actions/jobs/<id>/logs --allow-escape-sequences

and the flag is not optional. Without it `gh` refuses to print the log at all,
because the body contains ANSI escapes, and it says so **on stderr** while
exiting 1 with an empty stdout:

    gh api .../jobs/111553058881/logs > /tmp/x.log 2>/dev/null   # rc=1, 0 bytes
    gh api .../jobs/111553058881/logs --allow-escape-sequences   # rc=0, 38,835 bytes

So `... | grep <something>` against the first form returns an empty pipe with no
error, and an empty pipe reads as "the log contains no such lines" rather than
"the tool refused". That has cost a diagnosis twice in one hour, by two different
sessions, with the fact sitting in five memory files -- which is the whole
argument for a committed wrapper rather than another note: **recall is the thing
that fails, and a note beside the command only helps someone who reads it before
typing.**

WHAT THIS DOES NOT COVER, AND IT MATTERS MORE THAN THE SCRIPT. This removes the
composition for ONE recurring call. It does **not** tell you that a read you
composed returned less than the whole. The same day this was filed, a
`--paginate` read came back partial -- 127 runs reduced to 2 at the same head --
with *nothing on stderr*, nothing refusing, and the only tell a nonsense number
that had to be re-measured. No flag would have helped; there is no flag to
forget. The reflex that catches it is "what did I discard?", and this script
cannot ask it.

**A good artifact reads as the whole remedy, and then the next empty read that
isn't this call goes unexamined** -- which is the failure this exists to stop,
one level over. Hence this paragraph rather than a footnote.

WHICH ENDPOINT, BECAUSE THE TWO ARE ROUTINELY CONFUSED. `/actions/jobs/<id>/logs`
works as soon as the JOB completes. `gh run view --job <id> --log` needs the
whole RUN to be complete first, and refuses with "run is still in progress"
otherwise. This script uses the former, deliberately: it is usable while the rest
of the run is still going, which is exactly when a diagnosis is wanted.

RE-RUNS. A re-run replaces what the default view shows -- the same command,
minutes later, returns the new attempt's output, not the failed one's. So
**capture before re-running**: `scripts/gh-job-log.py <id> > /tmp/fail.log`. The
text is not deleted from the run's history, but it stops being what this returns,
and the person who read it first becomes the only holder of it.

EXIT STATUS (the third status is the point -- a check that cannot measure must
not be able to say "fine").
    0  the log was fetched, or the job has not finished and the log is legitimately empty
    1  the fetch cannot be trusted: gh refused, or the job has a TERMINAL
       conclusion and the fetch still produced nothing
    2  UNMEASURED -- the job's own state could not be read, so emptiness cannot
       be told apart from a refusal
"""

import argparse
import os
import subprocess
import sys

JOB_LOG = "repos/{repo}/actions/jobs/{job}/logs"
JOB = "repos/{repo}/actions/jobs/{job}"

# Conclusion values that mean the job is DONE. A terminal conclusion plus an
# empty fetch is the contradiction this script exists to refuse: a finished job
# has a log, so "0 bytes" is about the fetch, not about the log.
TERMINAL = {"success", "failure", "cancelled", "skipped", "timed_out", "neutral", "action_required"}


def gh(args, repo=None):
    """Run gh, returning (returncode, stdout, stderr). stderr is never discarded."""
    cmd = ["gh"] + args
    proc = subprocess.run(cmd, capture_output=True, text=True)
    return proc.returncode, proc.stdout, proc.stderr


def fetch(repo, job_id):
    return gh(["api", JOB_LOG.format(repo=repo, job=job_id), "--allow-escape-sequences"])


def job_state(repo, job_id):
    """(status, conclusion) or None if it could not be read."""
    rc, out, err = gh(["api", JOB.format(repo=repo, job=job_id), "--jq", ".status + \" \" + (.conclusion // \"none\")"])
    if rc != 0:
        sys.stderr.write(err)
        return None
    parts = out.split()
    return (parts[0], parts[1]) if len(parts) == 2 else None


def default_repo():
    rc, out, _ = gh(["repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner"])
    return out.strip() if rc == 0 and out.strip() else None


def run(repo, job_id, out_path):
    rc, out, err = fetch(repo, job_id)
    # Always surface stderr: the refusal above is reported there, and discarding
    # it is the first half of the failure this script exists to prevent.
    if err:
        sys.stderr.write(err)
    if rc != 0:
        sys.stderr.write(
            f"gh-job-log: the fetch FAILED (rc={rc}); that is a refusal, not an empty log.\n")
        return 1

    if out:
        if out_path:
            with open(out_path, "w") as fh:
                fh.write(out)
            sys.stderr.write(f"gh-job-log: {len(out)} bytes -> {out_path}\n")
        else:
            sys.stdout.write(out)
        return 0

    # Empty and rc=0. Ask the job whether that is even possible.
    state = job_state(repo, job_id)
    if state is None:
        sys.stderr.write(
            "UNMEASURED: the fetch returned nothing and the job's own state could not be "
            "read, so this cannot be told apart from a refusal. This is a failure of the "
            "check, not a finding about the log.\n")
        return 2
    status, conclusion = state
    if conclusion in TERMINAL:
        sys.stderr.write(
            f"gh-job-log: the fetch returned 0 bytes for a job that has COMPLETED "
            f"(status={status} conclusion={conclusion}).\n"
            f"A finished job has a log, so this is about the fetch and not about the "
            f"log -- do NOT read it as \"the log contains no such lines\".\n")
        return 1
    sys.stderr.write(
        f"gh-job-log: no log yet (job is {status}) -- nothing has been written, which is "
        f"expected this early. Not a finding.\n")
    return 0


def self_test():
    """A fake `gh` on PATH, so the flag and the empty-read rule are both checked.

    The known-positive is `test_flag_is_passed`: without it, every other case here
    would still pass against a script that had dropped the flag, because the fake
    would happily serve a log either way.
    """
    import shutil
    import tempfile

    fake = '''#!/usr/bin/env bash
echo "$@" >> "$GH_JOB_LOG_ARGV"
case "$1 $2" in
  "api repos/"*"/logs"*) [ -n "$FAKE_LOG" ] && printf '%s' "$FAKE_LOG"; [ -n "$FAKE_ERR" ] && echo "$FAKE_ERR" >&2; exit "${FAKE_RC:-0}" ;;
  "repo view"*) echo "cleat-team/cleat" ;;
  "api repos/"*) printf '%s' "${FAKE_JOB_SUMMARY:-in_progress none}" ;;
esac
exit 0
'''

    def case(name, want_rc, expect_argv_flag=None, expect_stderr=None, log="hello\n",
             job_summary="in_progress none", rc=0, err=""):
        d = tempfile.mkdtemp()
        argv_file = os.path.join(d, "argv")
        bin_dir = os.path.join(d, "bin")
        os.makedirs(bin_dir)
        with open(os.path.join(bin_dir, "gh"), "w") as fh:
            fh.write(fake)
        os.chmod(os.path.join(bin_dir, "gh"), 0o755)
        env = dict(os.environ, PATH=bin_dir + os.pathsep + os.environ["PATH"],
                   GH_JOB_LOG_ARGV=argv_file, FAKE_LOG=log, FAKE_JOB_SUMMARY=job_summary,
                   FAKE_RC=str(rc), FAKE_ERR=err)
        proc = subprocess.run([sys.executable, __file__, "12345", "-R", "cleat-team/cleat"],
                              capture_output=True, text=True, env=env)
        argv = open(argv_file).read() if os.path.exists(argv_file) else ""
        problems = []
        if proc.returncode != want_rc:
            problems.append(f"rc={proc.returncode} want {want_rc}; stderr={proc.stderr.strip()[:200]}")
        if expect_argv_flag and expect_argv_flag not in argv:
            problems.append(f"{expect_argv_flag!r} not in the argv gh received: {argv.strip()!r}")
        if expect_stderr and expect_stderr not in proc.stderr:
            problems.append(f"stderr lacks {expect_stderr!r}: {proc.stderr.strip()[:200]}")
        shutil.rmtree(d, ignore_errors=True)
        print(f"{'ok  ' if not problems else 'FAIL'} {name}" + (f"  -- {'; '.join(problems)}" if problems else ""))
        return 1 if problems else 0

    fails = 0
    # The known-positive: the flag this whole issue is about.
    fails += case("flag_is_passed", 0, expect_argv_flag="--allow-escape-sequences")
    # The textbook failure the issue measured: empty because gh refused.
    fails += case("refusal_is_not_an_empty_log", 1, rc=1,
                  err="the response contains terminal escape sequences",
                  expect_stderr="that is a refusal, not an empty log")
    # Empty AND terminal: the contradiction.
    fails += case("empty_plus_terminal_fails_loudly", 1, log="",
                  job_summary="completed failure",
                  expect_stderr="do NOT read it as")
    # Empty AND still running: legitimate, not a finding.
    fails += case("empty_plus_in_progress_is_not_a_finding", 0, log="",
                  job_summary="in_progress none", expect_stderr="Not a finding")
    # A real log is passed through unchanged.
    fails += case("a_log_is_passed_through", 0, log="line one\nline two\n")

    if fails:
        print(f"gh-job-log --self-test: {fails} case(s) FAILED", file=sys.stderr)
        return 1
    print("gh-job-log --self-test: all cases passed")
    return 0


def main():
    ap = argparse.ArgumentParser(description="Fetch an Actions job log without composing the call.")
    ap.add_argument("job_id", nargs="?", help="the job id")
    ap.add_argument("-R", "--repo", help="owner/repo (default: this checkout's)")
    ap.add_argument("-o", "--out", help="write the log here instead of stdout")
    ap.add_argument("--self-test", action="store_true", help="exercise the wrapper against a fake gh")
    args = ap.parse_args()

    if args.self_test:
        return self_test()
    if not args.job_id:
        ap.error("a job id is required (or --self-test)")

    repo = args.repo or default_repo()
    if not repo:
        sys.stderr.write("gh-job-log: could not determine the repository; pass -R owner/repo\n")
        return 2
    return run(repo, args.job_id, args.out)


if __name__ == "__main__":
    sys.exit(main())
