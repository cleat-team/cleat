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
# Budgets are DERIVED, not stored. scripts/skip-ledger.tsv holds one line per
# reason a test may skip,
#
#     <job key><TAB><count><TAB><test-name regex><TAB><why>
#
# and a job's budget is the SUM of its lines. There is no total to edit, which
# is the point: two streams each adding a skipping test add two distinct lines
# and git merges them. Each line is also checked on its own, so a line that
# matches nothing is a grant covering something that is not there.
#
# A budget is a ceiling on a job that is expected to skip *nothing* once its
# services are up. Zero is the goal for any job that provisions everything its
# tests ask for.
#
# Usage:
#   scripts/check-skip-budget.sh <job-key> <test-report.json>
#
# The report is `go test -json` output, as already produced by the CI steps
# that tee to test-report.json.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LEDGER="$REPO_ROOT/scripts/skip-ledger.tsv"

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <job-key> <test-report.json>" >&2
  exit 2
fi

JOB="$1"
REPORT="$2"

if [ ! -f "$LEDGER" ]; then
  echo "ERROR: $LEDGER is missing." >&2
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

# The budget is the SUM of this job's ledger lines, not a number anyone edits.
# See the header of scripts/skip-ledger.tsv for why the single number had to go.
lines="$(awk -F'\t' -v job="$JOB" '!/^#/ && NF>=3 && $1==job' "$LEDGER")"
if [ -z "$lines" ]; then
  echo "ERROR: no skip ledger lines for job '$JOB' in $LEDGER." >&2
  echo "Add '<job key><TAB><count><TAB><test regex><TAB><why>'. A job with no" >&2
  echo "ledger is one whose skips can grow without anyone seeing it." >&2
  exit 1
fi
budget="$(echo "$lines" | awk -F'\t' '{n+=$2} END{print n+0}')"

# The skipped test NAMES, which is what lets each line be checked on its own
# rather than only in aggregate.
names="$(grep '"Action":"skip"' "$REPORT" | grep -o '"Test":"[^"]*"' | sed 's/"Test":"//; s/"$//' | LC_ALL=C sort)"

fail=0
attributed=0
while IFS=$'\t' read -r _job count pattern why; do
  [ -z "$pattern" ] && continue
  [ "$pattern" = "__UNATTRIBUTED__" ] && continue
  got="$(printf '%s\n' "$names" | grep -cE "$pattern" || true)"
  attributed=$((attributed + got))
  if [ "$got" -ne "$count" ]; then
    echo "ERROR: ledger line for '$JOB' expects $count skip(s) matching /$pattern/, got $got." >&2
    if [ "$got" -lt "$count" ]; then
      echo "  Fewer than declared: the test stopped skipping, or was renamed. A line" >&2
      echo "  that matches nothing is a grant covering something that is not there." >&2
    else
      echo "  More than declared: something new is skipping under a reason written" >&2
      echo "  for something else. Give it its own line." >&2
    fi
    echo "  reason on file: $why" >&2
    fail=1
  fi
done <<EOF
$lines
EOF

# Whatever matched no pattern lands against the inherited remainder, which may
# only shrink. A new skip that nobody attributed pushes this over and fails --
# that is the forcing function, not a side effect.
legacy="$(echo "$lines" | awk -F'\t' '$3=="__UNATTRIBUTED__" {print $2+0; exit}')"
legacy="${legacy:-0}"
unattributed=$((skipped - attributed))
if [ "$unattributed" -gt "$legacy" ]; then
  echo "ERROR: '$JOB' has $unattributed skip(s) matching no ledger line; the" >&2
  echo "inherited allowance is $legacy and may only shrink." >&2
  echo >&2
  echo "Add a line naming the new skip and why, rather than raising a total:" >&2
  echo "    $JOB<TAB><count><TAB><test regex><TAB><why>" >&2
  echo >&2
  echo "Attribute by NAME, not by delta -- a registeredBackends test costs two" >&2
  echo "skips in a PostgreSQL-only job and a dialect-gated one costs one:" >&2
  echo "    go test ./<pkg>/ -run <Name> -json | grep '\"Action\":\"skip\"'" >&2
  fail=1
fi
if [ "$fail" -ne 0 ]; then
  exit 1
fi

echo "job=$JOB skipped=$skipped budget=$budget (passed=$passed failed=$failed)"

# A skip count read off a failing run is not a measurement of anything: a test
# that dies early never reaches the subtests that would have skipped, so the
# number is low by an unknown amount. Whoever writes it into the ledger then
# records a ceiling nobody can reach, and this file's own history says that is
# how the guard stops being able to fail.
#
# Not fatal -- the test step that produced this report has already failed the
# job -- but loud, because the number printed above is about to be copied into
# a comment claiming it was measured.
if [ "$failed" -gt 0 ]; then
  echo >&2
  echo "WARNING: this report contains $failed failing test(s), so the skip count" >&2
  echo "above is not a usable measurement -- do not write it into" >&2
  echo "scripts/skip-ledger.tsv. Fix the failures and re-measure." >&2
  echo >&2
  # NAME them. Every workflow step that produces one of these reports redirects
  # the test output into it -- `go test -json ... > test-report.json 2>&1` --
  # and no workflow uploads the file. So on a job whose tests fail, the failing
  # test names exist nowhere a reader can reach: not in the step log, which got
  # only the shell's exit code, and not in an artifact, because there is none.
  #
  # Measured 2026-09-04 on PR #672: `Layer 3 -- Multi-DB` reported
  # `passed=0 failed=4` and the four names were unavailable. The job could be
  # seen to have failed and not what failed, which is the same distance from a
  # usable result as a green run that measured nothing.
  #
  # This block is the cheapest place to fix it: this script already parses the
  # report for the skip list below, already runs in every job that writes one,
  # and already knows the count it is refusing to act on.
  echo "Failing tests:" >&2
  # Same filter discipline as the skip list: keep only test-level events before
  # rewriting, because sed passes a non-matching line through verbatim and a
  # package-level fail event would otherwise print as raw JSON.
  grep '"Action":"fail"' "$REPORT" |
    grep '"Test":"' |
    sed -E 's/.*"Package":"([^"]*)".*"Test":"([^"]*)".*/  \1\t\2/' |
    LC_ALL=C sort -u >&2
  echo >&2
  echo "If you are running locally: './engine/...' matches two database-backed" >&2
  echo "packages, and without '-p 1' they run concurrently against one database" >&2
  echo "and delete each other's fixtures. See the header of skip-ledger.tsv." >&2
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
  echo "NOTE: under budget by $((budget - skipped)). Lower the __UNATTRIBUTED__ line"
  echo "      for '$JOB' in $LEDGER to lock it in -- that line is a ratchet and this"
  echo "      is the direction it is allowed to move."
fi

echo "OK: '$JOB' skips are within budget."
