#!/usr/bin/env bash
#
# Run the `cleat build` command each example's README documents, and require it
# to succeed. cleat#1814.
#
# WHY THIS EXISTS
#
# Measured 2026-09-17: 18 example directories, 10 whose README documents a
# `cleat build …`, and ZERO built by any workflow. Every `cleat build` string in
# .github/workflows/ is inside a comment. So the commands we tell users to run
# were the commands nothing ran.
#
# That was load-bearing, twice in one day. Two independent implementations of the
# Python determinism gate both refused `examples/python-langchain`'s documented
# command, producing 24 false positives from a test harness (cleat#1813). One was
# caught by running the command by hand; the other (cleat#1809) reached 51
# passing checks with one failure, and that failure was an unrelated lint. Remove
# that lint and it merges.
#
# A green CI run was evidence that the unit tests pass. It was not evidence that
# the product does what the documentation says.
#
# DERIVED, NOT LISTED
#
# The set comes from the READMEs, not from a list in this file. A hand-maintained
# list is the thing that goes stale silently -- ci.yml:810 already carries that
# lesson about a shellcheck file list that had been missing four scripts for
# months. Add an example with a documented build and it is covered; nobody has to
# remember this script.
#
# EXCLUSIONS ARE NAMED, WITH AN ISSUE
#
# An example whose documented command is known broken is listed in
# KNOWN_BROKEN with the issue that tracks it. That keeps this job green and
# honest at once: the exclusion is visible, it names why, and when the issue is
# fixed the entry is deleted and the example is covered. A job that silently
# skipped what it could not build would report the same green as one that built
# everything.
set -uo pipefail

cd "$(git rev-parse --show-toplevel)" || exit 2

# Examples this job does not run, as "dir<TAB>reason" lines.
#
# EMPTY as of cleat#1854: the Python toolchain is now installed in the job
# above, so the last two entries -- both Python examples, excluded only
# because nothing here could build them -- are gone. All 10 documented
# examples run.
#
# TWO KINDS OF REASON, and they are not interchangeable. An entry may be here
# because the documented command is BROKEN (an issue against the product), or
# because THIS JOB cannot run it (an issue against the job). "known broken"
# over an example that builds perfectly well by hand is a claim about the
# product that nothing would ever recheck -- say which kind an entry is.
#
# NOT an associative array: macOS ships bash 3.2, which has none, and a script
# the author cannot run locally is how this job would come to be trusted without
# being exercised -- which is the failure this whole file exists to prevent.
# KNOWN_BROKEN is the only exclusion this script has, and examples/ts-workflow is NOT broken: it is built elsewhere. The entry's
# text says so first, so the SKIP line is not read as a defect. It is here because THIS JOB cannot run its documented command: `cleat
# build --target ts` needs Node, a pinned Javy and a wasm32-wasip1 Rust toolchain, and this job installs none of them. The
# command IS run, on every pull request that touches the example, by examples/ts-workflow/workflow.test.ts in the
# TypeScript tier-2 job (typescript-tier2.yml), which has the toolchain. Delete this entry if this job ever gains it.
KNOWN_BROKEN="examples/ts-workflow\tBUILT ELSEWHERE, NOT BROKEN: the command works, and typescript-tier2.yml runs it via examples/ts-workflow/workflow.test.ts on every PR that touches the example. THIS JOB has no Node, Javy or wasm32-wasip1 toolchain and is deliberately not given one (it runs on many PRs)"

known_broken_reason() {
  printf '%b\n' "$KNOWN_BROKEN" | while IFS="$(printf '\t')" read -r d reason; do
    [ "$d" = "$1" ] && printf '%s' "$reason"
  done
}

CLEAT_BIN="${CLEAT_BIN:-$(pwd)/.bin/cleat}"
if [[ ! -x "$CLEAT_BIN" ]]; then
  echo "UNMEASURED: no cleat binary at $CLEAT_BIN. Build it first:" >&2
  echo "  go build -o .bin/cleat ./cmd/cleat" >&2
  exit 2
fi

# How much of a failed build's log is printed. A real Gradle failure of the
# shape cleat#3592 hit is ~30 lines; this is generous enough for a build with a
# long compiler diagnostic and small enough not to bury the other examples.
FAIL_LOG_LINES="${BDE_FAIL_LOG_LINES:-400}"

# A TRANSIENT DEPENDENCY FETCH IS RETRIED, AND NOTHING ELSE IS. cleat#4254.
#
# examples/java-workflow's Gradle build fetches the TeaVM plugin's own dependencies (teavm-core, teavm-tooling) through the Gradle Plugin
# Portal, which reaches Maven Central. Measured 2026-11-20: four completed failures of this job in one day, all of them that example and those
# two artifacts; one log says `Received status code 429 from server: Too Many Requests` from repo.maven.apache.org, three say `Could not find
# org.teavm:teavm-core:0.10.2` at plugins.gradle.org with no status shown. Nothing in the repository changed between a green run and a red
# one. Gradle does not retry these, and a cache cannot help (this job is excluded from the develop push, so nothing seeds one).
#
# THE SIGNATURE IS NARROW ON PURPOSE. A compile error, a vet failure, a determinism-gate refusal or a missing toolchain must fail on the first
# attempt, exactly as before: retrying "any failure" would turn this job into one that cannot go red. The signature is a failed FETCH: an HTTP
# 429 or 5xx status from a repository, or `Could not find <group>:<name>:<version>`. That second form also matches a dependency that really does
# not exist (a typo in a version), which is retried and then fails after the cap -- the cost is the backoff, not a wrong answer.
#
# NO SILENT FLAKES. Every retry prints the lines that matched, so the cause of the first failure is in the log whether or not the build later
# passes, and an example that needed a retry is listed in the summary even when it is OK. A run that recovered is not the same as a run that
# never failed, and the job's output must not say they are.
#
# BDE_FETCH_RETRIES is the number of retries after the first attempt (0 disables them); BDE_RETRY_SLEEP is the base wait in seconds, multiplied
# by the attempt number. Defaults: 2 retries, 30s -- at most 90s of waiting per example, inside the job's 10 minute timeout.
FETCH_RETRIES="${BDE_FETCH_RETRIES:-2}"
RETRY_SLEEP="${BDE_RETRY_SLEEP:-30}"
TRANSIENT_FETCH='Received status code (429|5[0-9][0-9])|Could not find [^[:space:]]+:[^[:space:]]+:[^[:space:]]+'

examined=0 built=0 failed=0 skipped=0
declare -a FAILURES=()
declare -a RETRIED=()

while IFS= read -r readme; do
  dir="$(dirname "$readme")"
  # The first documented `cleat build …` line, taken verbatim. A `$ ` prompt is
  # stripped; nothing else is rewritten, because rewriting the command would
  # test something other than what the README says.
  cmd="$(grep -m1 -oE '(^|\$ )cleat build[^`]*' "$readme" | sed 's/^\$ //' | sed 's/[[:space:]]*$//')"
  [[ -z "$cmd" ]] && continue
  examined=$((examined + 1))

  reason="$(known_broken_reason "$dir")"
  if [[ -n "$reason" ]]; then
    echo "SKIP  $dir"
    echo "      $reason"
    skipped=$((skipped + 1))
    continue
  fi

  # Run it from the example's own directory when the command names no path, and
  # from the repository root when it does -- which is how each README presents
  # it. Both are user-facing forms and both must work.
  if [[ "$cmd" == *"./examples/"* ]]; then
    workdir="."
  else
    workdir="$dir"
  fi

  out="$(mktemp -d)"
  run="${cmd/cleat build/$CLEAT_BIN build}"
  run="${run/-o \/tmp\/out/-o $out}"
  attempt=1
  while :; do
    (cd "$workdir" && eval "$run") >/tmp/bde.log 2>&1 && ok=1 || ok=0
    if (( ok == 1 )); then break; fi
    if (( attempt <= FETCH_RETRIES )) && grep -qE "$TRANSIENT_FETCH" /tmp/bde.log; then
      wait_s=$((RETRY_SLEEP * attempt))
      echo "RETRY $dir (attempt $((attempt + 1)) of $((FETCH_RETRIES + 1)), after ${wait_s}s): the build failed fetching a dependency, which is retried. The failure:"
      grep -E "$TRANSIENT_FETCH" /tmp/bde.log | sed 's/^/      /' | head -4
      sleep "$wait_s"
      attempt=$((attempt + 1))
      continue
    fi
    break
  done
  if (( ok == 1 )); then
    if (( attempt > 1 )); then
      echo "OK    $dir (after $((attempt - 1)) retry(ies): see the RETRY lines above)"
      RETRIED+=("$dir")
    else
      echo "OK    $dir"
    fi
    built=$((built + 1))
  else
    echo "FAIL  $dir"
    if (( attempt > 1 )); then
      echo "      failed after $attempt attempts; the last attempt's log follows (the first failure's cause is in the RETRY lines above)"
    fi
    echo "      \$ $cmd"
    # The log, not its last eight lines (cleat#3592). Gradle prints the cause
    # ABOVE its own epilogue -- the compiler error, then `* What went wrong:` and
    # the exception -- and the epilogue is the part a tail keeps: `> Run with
    # --info`, `BUILD FAILED`. On the run that prompted this the cause was not
    # recoverable from CI at all. The cap is a bound, not a window onto the
    # cause: past it the earliest lines are dropped and the output says how many.
    total="$(wc -l </tmp/bde.log | tr -d ' ')"
    if (( total > FAIL_LOG_LINES )); then
      echo "      ... $((total - FAIL_LOG_LINES)) earlier line(s) of $total omitted; showing the last $FAIL_LOG_LINES"
    fi
    tail -n "$FAIL_LOG_LINES" /tmp/bde.log | sed 's/^/      /'
    FAILURES+=("$dir")
    failed=$((failed + 1))
  fi
  rm -rf "$out"
done < <(git ls-files 'examples/*/README.md')

echo
echo "examples with a documented build: $examined   built: $built   skipped: $skipped   failed: $failed   needed a retry: ${#RETRIED[@]}"
if (( ${#RETRIED[@]} > 0 )); then
  echo "RETRIED and then built (a transient dependency fetch failed; not a clean run for these):"
  printf '  %s\n' "${RETRIED[@]}"
fi

# A run that examined nothing is a broken check, not a clean tree. Ten READMEs
# document a build today; a scan finding none means the pattern or the path
# stopped matching, and "0 failures out of 0" is what that looks like from
# outside.
if (( examined < 8 )); then
  echo "UNMEASURED: found only $examined examples with a documented cleat build; expected at least 8." >&2
  echo "The scan stopped matching, so a clean result here would mean nothing." >&2
  exit 2
fi

# An exclusion that matches no example is a grant covering something that no
# longer exists -- the same arm every other guard in this repo carries.
while IFS="$(printf '\t')" read -r d _; do
  [ -z "$d" ] && continue
  if [[ ! -f "$d/README.md" ]]; then
    echo "STALE EXCLUSION: $d is listed in KNOWN_BROKEN but has no README.md; delete the entry." >&2
    exit 1
  fi
done < <(printf '%b\n' "$KNOWN_BROKEN")

if (( failed > 0 )); then
  echo >&2
  echo "These examples document a cleat build command that does not work:" >&2
  printf '  %s\n' "${FAILURES[@]}" >&2
  echo >&2
  echo "Fix the command, fix the build, or add the example to KNOWN_BROKEN with the" >&2
  echo "issue that tracks it. Do not delete the documented command." >&2
  exit 1
fi
