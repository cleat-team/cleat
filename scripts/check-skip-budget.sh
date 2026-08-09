#!/usr/bin/env bash
#
# Fail a CI job whose tests skipped more than its budget allows.
#
# scripts/check-skips.sh guards the skips that are *written* in the tree. This
# guards the ones that actually *fire* when a job runs, which is a different
# question and the one that has repeatedly gone wrong here:
#
#   * The multi-DB job's postgres:16 service published no port. Every source
#     skip was legitimate and unchanged; what changed was that they all fired.
#     The job was green for its entire existence without connecting once.
#   * Every worker in the compose cluster was crash-looping while the job
#     reported success.
#
# A static scan cannot see either. Both show up here as "this job skipped far
# more tests than it is supposed to".
#
# Budgets live in scripts/skip-budget.txt as
#
#     <job key><TAB><max skipped tests><TAB># why
#
# A budget is a ceiling on a job that is expected to skip *nothing* once its
# services are up. Seed it from an observed green run, then tighten. Zero is
# the goal for any job that provisions everything its tests ask for.
#
# Usage:
#   scripts/check-skip-budget.sh <job-key> <test-report.json>
#
# The report is `go test -json` output, as already produced by the CI steps
# that tee to test-report.json.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUDGETS="$REPO_ROOT/scripts/skip-budget.txt"

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <job-key> <test-report.json>" >&2
  exit 2
fi

JOB="$1"
REPORT="$2"

if [ ! -f "$BUDGETS" ]; then
  echo "ERROR: $BUDGETS is missing." >&2
  exit 1
fi

# A missing or empty report is the failure mode this script exists to catch,
# not a reason to pass quietly. A job whose test step died before producing
# output has skipped *everything*, which is the most complete version of the
# bug. The CI steps run this with `if: always()`, so this branch is reachable
# and must be loud.
if [ ! -s "$REPORT" ]; then
  echo "ERROR: $REPORT is missing or empty for job '$JOB'." >&2
  echo "The test step produced no JSON output at all, so nothing ran. That is" >&2
  echo "a harder failure than any number of skips, not a pass." >&2
  exit 1
fi

# Count events with a "Test" field so that package-level skip events (a package
# with no test files) are not charged against a budget meant for tests.
count_action() {
  grep "\"Action\":\"$1\"" "$REPORT" | grep -c '"Test":"'
}

skipped="$(count_action skip)"
passed="$(count_action pass)"
failed="$(count_action fail)"

# Same reasoning as check-test-only-code.sh's zero-findings guard: a report
# containing no test outcomes whatsoever is a broken run, and reporting "0
# skips, within budget" would be a vacuous pass by the guard against vacuous
# passes.
if [ "$passed" -eq 0 ] && [ "$failed" -eq 0 ] && [ "$skipped" -eq 0 ]; then
  echo "ERROR: $REPORT contains no test results at all for job '$JOB'." >&2
  echo "Not a clean run -- a run that did not happen. First lines:" >&2
  head -5 "$REPORT" | sed 's/^/    /' >&2
  exit 1
fi

budget="$(grep -E "^${JOB}	" "$BUDGETS" | head -1 | cut -f2)"
if [ -z "$budget" ]; then
  echo "ERROR: no skip budget recorded for job '$JOB' in $BUDGETS." >&2
  echo "Add a line '<job key><TAB><max><TAB># why'. An unbudgeted job is one" >&2
  echo "whose skips can grow without anyone seeing it." >&2
  exit 1
fi

echo "job=$JOB skipped=$skipped budget=$budget (passed=$passed failed=$failed)"

# A skip count read off a failing run is not a measurement of anything: a test
# that dies early never reaches the subtests that would have skipped, so the
# number is low by an unknown amount. Whoever writes it into skip-budget.txt
# then records a ceiling nobody can reach, and this file's own history says
# that is how the guard stops being able to fail.
#
# Not fatal -- the test step that produced this report has already failed the
# job -- but loud, because the number printed above is about to be copied into
# a comment claiming it was measured.
if [ "$failed" -gt 0 ]; then
  echo >&2
  echo "WARNING: this report contains $failed failing test(s), so the skip count" >&2
  echo "above is not a usable measurement -- do not write it into" >&2
  echo "scripts/skip-budget.txt. Fix the failures and re-measure." >&2
  echo >&2
  echo "If you are running locally: './engine/...' matches two database-backed" >&2
  echo "packages, and without '-p 1' they run concurrently against one database" >&2
  echo "and delete each other's fixtures. See the header of skip-budget.txt." >&2
  echo >&2
fi

if [ "$skipped" -gt "$budget" ]; then
  echo >&2
  echo "ERROR: '$JOB' skipped $skipped tests, budget is $budget." >&2
  echo >&2
  echo "Skipped tests:" >&2
  # Filter to test events before rewriting: sed passes a non-matching line
  # through verbatim, so a package-level skip event would otherwise be printed
  # as raw JSON in the middle of the list.
  grep '"Action":"skip"' "$REPORT" |
    grep '"Test":"' |
    sed -E 's/.*"Package":"([^"]*)".*"Test":"([^"]*)".*/  \1\t\2/' |
    LC_ALL=C sort -u >&2
  echo >&2
  echo "This usually means a service or toolchain the job provisions did not" >&2
  echo "come up, and the tests that needed it evaporated rather than failed." >&2
  echo "Check the service is reachable from where the tests run -- a service" >&2
  echo "container with no published 'ports:' is unreachable from a job that" >&2
  echo "runs on the runner rather than inside a container, and 127.0.0.1 is" >&2
  echo "not interchangeable with localhost on GitHub runners." >&2
  exit 1
fi

if [ "$skipped" -lt "$budget" ]; then
  echo "NOTE: under budget by $((budget - skipped)). Lower '$JOB' in $BUDGETS to lock it in."
fi

echo "OK: '$JOB' skips are within budget."
