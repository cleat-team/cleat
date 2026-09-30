#!/usr/bin/env bash
#
# Guard the conditional-skip inventory.
#
# A `t.Skip` is reported by `go test` as neither a pass nor a failure, and by
# CI as neither. In practice that means it is read as a pass. This repo has
# already paid for that four separate times:
#
#   * Multi-DB CI declared a postgres:16 service with no published port, so
#     testutil.TestDB could not reach it and skipped. Green for months without
#     the workflow ever opening a PostgreSQL connection.
#   * test-go's Postgres service had the same missing `ports:`.
#   * DURABLE_TEST_DB was renamed. The test that read it skipped from then on
#     and nothing noticed.
#   * Every worker in the compose cluster was crash-looping while the job
#     reported success.
#
# In each case the code was broken, a test existed that would have said so, and
# the test skipped instead. The skip is not the bug -- the bug is that the skip
# count was free to grow silently.
#
# So: this is a set-membership guard, not a threshold. Every skip site in the
# tree is recorded in the baseline as
#
#     <package dir><TAB><enclosing func><TAB><count>
#
# and the build fails on any site that is not there, or on any function whose
# skip count has grown. Adding a skip is allowed; adding one *silently* is not.
#
# Keying on the enclosing function rather than the raw file:line means moving a
# test within its file does not churn the baseline -- the same reasoning as
# scripts/check-test-only-code.sh, which this script deliberately mirrors.
#
# A count that has gone *down* never fails. That is a skip being converted into
# a real assertion, which is the entire point of the exercise; it is reported so
# the baseline can be tightened, not treated as an error.
#
# Usage:
#   scripts/check-skips.sh              # fail on skips not in the baseline
#   scripts/check-skips.sh --update     # rewrite the baseline
#   scripts/check-skips.sh --list       # print the current inventory, no check
#
# Related: the per-job runtime guard in scripts/check-skip-budget.sh checks how
# many tests actually skipped when a job ran, which is the other half -- this
# script cannot see that a test skipped because a service was unreachable.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT" || exit 1

BASELINE="scripts/skip-baseline.txt"
LEDGER="scripts/skip-ledger.tsv"
LEDGER_D="scripts/skip-ledger.d"
CROSSCHECK_EXEMPT="scripts/skip-crosscheck-exempt.txt"
# cleat#2761 R1: a ceiling, same spirit as skip-ledger.tsv's __UNATTRIBUTED__
# line -- defense in depth alongside the staleness check below, in case that
# check itself has a bug. Moves down as entries get real ledger lines; never
# up. Re-derive with `grep -cE '^[^#]' scripts/skip-crosscheck-exempt.txt`.
CROSSCHECK_EXEMPT_MAX=49

# Emitted by scan() when it produced nothing, so callers can tell a failed scan
# from a clean tree across the command-substitution boundary. Same guard, and
# the same reasoning, as check-test-only-code.sh: a scan that finds zero skips
# in a tree with hundreds is a broken scan, and treating it as "clean" would be
# a vacuous pass by the guard against vacuous passes.
SCAN_FAILED="__scan_failed__"

# Enumerate every Skip call, attributed to the top-level function containing it.
#
# The receiver alternation covers t.Skip/t.Skipf plus the b.Skip forms used in
# the benchmarks; a bare `.Skip(` would also match strings.Skip-alikes and any
# future non-testing method of that name.
#
# The trailing `[^A-Za-z0-9_]` rather than a literal `(` is deliberate: it also
# catches `unavailable := t.Skipf`, where the skip is taken as a function value
# and called later. That is not a curiosity -- it is the shape of the *fixed*
# pattern in engine/testutil/schema.go, where the choice between t.Skipf and
# t.Fatalf is made by assigning one or the other. A guard that only saw direct
# calls would be blind to the exact construct this audit is spreading. It still
# excludes t.Skipped(), since "Skip" there is followed by an alphanumeric.
#
# testutil/ is scanned alongside _test.go files. Its files are not themselves
# tests, but they hold the most consequential skips in the tree -- TestDB alone
# decides whether every database-backed test in the repo runs or evaporates.
#
# LC_ALL=C pins the collation, which otherwise differs between a developer's
# locale and the runner's and makes the baseline diff-noisy.
scan() {
  local findings
  # The awk program below is single-quoted on purpose, so SC2016 does not
  # apply: its $1/$3/$NF are awk fields, and letting the shell expand them
  # would substitute this function's positional arguments instead -- almost
  # always the empty string, which would make every scan silently return
  # nothing. The directive sits here because a shellcheck directive has to
  # precede the whole command, not a stage inside its pipeline.
  # shellcheck disable=SC2016
  findings="$(
# `.claude/worktrees/` is excluded because it is gitignored (.gitignore:95)
# and holds full checkouts of this repo -- one per agent worktree. A bare
# `find .` walks into them and reports their contents as findings in this
# tree. CI never sees it (its checkout is clean), so this guard was only
# ever exercised where the bug could not appear, while anyone using the
# repo's own worktree convention hit it on every local run. The general
# rule, for the next `find .` added here: a gitignored directory holding a
# copy of the repo makes an unpruned walk report someone else's tree.
    find . \( -name '*_test.go' -o \( -path '*/testutil/*' -name '*.go' \) \) \
      -not -path './node_modules/*' -not -path '*/node_modules/*' \
      -not -path './.git/*' -not -path './.claude/*' -print0 |
      LC_ALL=C sort -z |
      xargs -0 awk '
        FNR == 1 {
          # Package directory, with the leading ./ stripped. "." for the root.
          dir = FILENAME
          sub(/\/[^\/]*$/, "", dir)
          sub(/^\.\//, "", dir)
          if (dir == "" || dir == ".") dir = "."
          fn = "<file scope>"
        }
        /^func[ \t]/ {
          # A METHOD IS A DECLARATION TOO, and this used to miss them. The
          # pattern was /^func [A-Za-z_]/, which cannot match
          # `func (m *MySQLBackend) Setup(`, because the character after
          # "func " is "(". A method therefore never updated fn, so every
          # skip inside one was credited to whatever plain function happened
          # to sit above it -- silently, and in a way that looks like a real
          # attribution rather than a missing one. cleat#1740.
          #
          # Measured on develop before the fix: 4 of 247 skip sites, all in
          # engine/store_backends_test.go, landing on TWO functions that
          # contain no skip at all. Both were live in the baseline:
          #
          #     engine	RegisterBackend	2          (3 lines long, no skip)
          #     engine	openMSSQLTenantStore	2   (no skip)
          #
          # RECEIVER-QUALIFIED, and that is not cosmetic. This file declares
          # MySQLBackend.Setup and MSSQLBackend.Setup, and likewise two
          # SetupForTenant. A bare method name collapses four distinct owners
          # into two entries, so the baseline could not tell a skip moving
          # between backends from one staying put -- which is the whole job of
          # a per-name baseline.
          if ($0 ~ /^func[ \t]+\(/) {
            recv = $0
            sub(/^func[ \t]+\(/, "", recv)
            sub(/\).*$/, "", recv)          # "m *MySQLBackend" | "*Foo" | "Foo"
            nparts = split(recv, rp, /[ \t]+/)
            rtype = rp[nparts]
            sub(/^\*/, "", rtype)

            name = $0
            sub(/^func[ \t]+\([^)]*\)[ \t]*/, "", name)
            sub(/[\(\[].*$/, "", name)      # drop params, and any type params
            fn = rtype "." name
          } else {
            fn = $0
            sub(/^func[ \t]+/, "", fn)
            sub(/[\(\[].*$/, "", fn)        # "[" so a generic func keeps its name
          }
        }
        {
          # Drop whole-line comments. This repo documents its skips heavily --
          # three separate files discuss t.Skipf in prose -- and counting that
          # prose would put phantom entries in the baseline that no code change
          # could ever remove. Only leading-// lines are stripped, never a //
          # appearing mid-line, so a URL inside a skip message (there are two)
          # cannot cause a real call to be missed.
          line = $0
          if (line ~ /^[[:space:]]*\/\//) next

          # Two patterns, because a trailing [^A-Za-z0-9_] cannot match at
          # end-of-line and the assignment form `unavailable := t.Skipf` ends
          # there. Both are needed; neither matches t.Skipped(), where "Skip"
          # is followed by an alphanumeric.
          if (line ~ /(^|[^A-Za-z0-9_.])(t|b|tb)\.Skipf?$/ ||
              line ~ /(^|[^A-Za-z0-9_.])(t|b|tb)\.Skipf?[^A-Za-z0-9_]/) {
            count[dir "\t" fn]++
          }
        }
        END {
          for (k in count) print k "\t" count[k]
        }
      ' |
      LC_ALL=C sort -u
  )"

  if [ -z "$findings" ]; then
    echo "ERROR: found no t.Skip sites at all in any _test.go file." >&2
    echo "This tree has hundreds, so that is a broken scan and not a clean" >&2
    echo "tree -- check that find/awk ran and that _test.go files are present." >&2
    # NOT exit: scan runs inside a command substitution, so exit would leave
    # only the subshell and the caller would carry on with an empty result and
    # report OK. Callers check for the sentinel instead.
    echo "$SCAN_FAILED"
    return
  fi

  printf '%s\n' "$findings"
}

die_if_scan_failed() {
  if [ "$1" = "$SCAN_FAILED" ]; then
    exit 1
  fi
}

total_skips() {
  awk -F'\t' '{ n += $3 } END { print n + 0 }' <<<"$1"
}

# ledger_lines prints every runtime skip-ledger declaration -- the single
# file plus every fragment in skip-ledger.d/ -- as one stream.
#
# DUPLICATED from scripts/check-skip-budget.sh rather than shared, on
# purpose: that script is under active edit elsewhere (cleat#2753) as this is
# written, and factoring both into one sourced file would make an edit to
# either collide with the other over a file neither owns alone. See that
# script's own comment on this same function for the full reasoning on why
# the fragment directory exists (cleat#1333, cleat#1395) and why `awk 1`,
# not `cat` (a fragment missing its trailing newline must not swallow the
# next file's declaration).
ledger_lines() {
  awk 1 "$LEDGER" 2>/dev/null
  if [ -d "$LEDGER_D" ]; then
    find "$LEDGER_D" -maxdepth 1 -name '*.tsv' -type f -print0 |
      sort -z | xargs -0 -r awk 1
  fi
}

# stale_entries prints the baseline lines whose (dir, function) key the current
# scan does not produce at all. cleat#1746.
#
# KEYED ON THE FIRST TWO FIELDS, not on the whole line. A key present with a
# different count is already handled as grown or shrunk; this is for a key that
# has vanished, which is the only case neither of those can see.
#
# A SEPARATE FUNCTION so the self-test can drive it against a fixture. The
# comparison it belongs to is inline in the script body and cannot be exercised
# without running the whole guard over a staged repo -- and a check whose
# correctness is only ever asserted by running it on a healthy tree is the
# defect this whole change is about.
#
# $1 is the current scan; the baseline path comes from $BASELINE, or $2 when
# given, which is what the self-test passes.
stale_entries() {
  local current="$1" baseline="${2:-$BASELINE}" line key out=""
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    case "$line" in \#*) continue ;; esac
    key="$(cut -f1,2 <<<"$line")"
    # A literal tab terminates the key, so `engine Setup` cannot match
    # `engine SetupForTenant` -- the prefix collision this file already has
    # four live instances of.
    # NOT `printf '%s\n' "$current" | grep -qF ...`. Under this script's own
    # `set -uo pipefail` (line 47) that reports a key it MATCHED as stale: grep -q
    # exits on the first match and closes its read end, printf dies of SIGPIPE (141),
    # pipefail takes the pipeline status from that non-zero member, and `if !` reads
    # the successful match as a failure. It is not even intermittent once $current is
    # large. A here-string has no producer process, so there is nothing for pipefail
    # to misread. Same defect and same fix as scripts/tier2-gate.sh.
    if ! grep -qF "$key	" <<< "$current"; then
      out="${out}${line}"$'\n'
    fi
  done < "$baseline"
  printf '%s' "$out"
}

# crosscheck_baseline_vs_ledger: cleat#2759.
#
# A new dialect-gated test has to be registered in TWO places -- this
# script's own static baseline (checked above, and it fails loudly if this
# one is missed) and the runtime skip-ledger scripts/check-skip-budget.sh
# reads. Registering only the first produces a green Lint run and a red CI
# job hours later, in whichever job runs ./engine/... without the DSN the
# test needs. That is not a hypothetical: cleat#2741, cleat#2746 and
# cleat#2756 all hit it in one night, each after the author believed the
# static registration alone was complete.
#
# This runs here, in Lint, with no database, so it can fail AT THE POINT THE
# SKIP IS INTRODUCED. check-skip-budget.sh cannot: it only sees the gap once
# a real per-job `go test -json` report exists, which is after the fact by
# construction -- the same discipline cleat#2158 asks for from the ledger's
# other direction (a cluster line implying a test-go/engine line).
#
# SCOPE IS DELIBERATELY NARROW, in two ways, and each is a false negative
# rather than a false positive when it declines:
#
#   1. Only dir=="engine". All three known-positives are there, and this
#      guard has no job table for any other package.
#   2. Only a function whose body names EXACTLY ONE of the four dialect
#      discriminators below. Zero or more than one is left unclassified.
#
# THIS USED TO ALSO EXCLUDE ANY FUNCTION CONTAINING t.Run/b.Run, meant to
# skip registeredBackends-style dialect dispatch -- and it was both
# unnecessary and wrong. Unnecessary: a registeredBackends test dispatches
# through backend.Setup(t), a SEPARATE function, so the dispatching test
# itself names zero dialects and rule 2 above already excludes it. Wrong:
# cleat#2746's own known-positive, TestMSSQLRetentionSweepConcurrentWriter
# LatencyStaysBounded, is gated on exactly one dialect at its top and then
# runs three UNRELATED t.Run arms under that one gate -- the excluded shape
# was one of the three cases this guard exists to catch. Caught by running
# this guard against that test's own pre-baseline commit (c56a89c1) as the
# known-positive control below: the first version reported no violation.
#
# THIS ALSO USED TO RUN AGAINST $added ($current minus the COMMITTED
# skip-baseline.txt) RATHER THAN THE FULL SCAN -- and that was wrong too,
# in the direction that matters most: it went silent on exactly the shape
# cleat#2756 shipped. $added is empty the instant `--update` has been run,
# and --update routinely lands in the SAME commit as the test itself
# (cleat#2756's 632107e1: test + baseline entry, one commit; the ledger
# line came two commits later, in d3e5203). At that commit scan() already
# matches the committed baseline exactly, so $added sees nothing -- checked
# by running the $added-only version directly against 632107e1 and getting
# "OK". See scripts/skip-crosscheck-exempt.txt's own header for what
# replaced it and why that file, not $added, is now the "already accounted
# for" set.
#
# Under-classifying leaves today's gap uncaught, no worse than before this
# guard existed. Over-classifying would fail an unrelated PR over a line
# this guard misread -- the wrong direction for a guard that runs on every
# PR touching no database at all.
#
# THE JOB TABLE IS HAND-MAINTAINED, the same way every existing ledger
# line's own "why" text already hardcodes which DSN a job sets. Verified
# against the workflow YAML on 2026-09-30, re-derive before trusting it
# stale:
#
#   grep -n "CLEAT_TEST_\|go test " .github/workflows/engine-race.yml
#   sed -n '/name: cluster/,/check-skip-budget.sh cluster /p' .github/workflows/ci.yml \
#     | grep -n "CLEAT_TEST_\|go test "
#   grep -n "CLEAT_TEST_\|go test " .github/workflows/multi-db-ci.yml
#
# Deliberately just these four. ci.yml's "Test Go (core)" leg ALSO runs
# ./... (engine/ included) with the same postgres-only env, and is left out
# -- nothing has hit it yet, and adding a job this guard cannot verify
# against a real failure is the runtime-side mistake cleat#2759 documents,
# arriving here instead. Add it the day core's budget does hit this.
#
# $1 is the FULL current scan (dir<TAB>fn<TAB>count lines) -- not $added.
# See the comment above for why $added went silent on cleat#2756's shape.
crosscheck_baseline_vs_ledger() {
  local current="$1" ledger_tmp current_tmp
  ledger_tmp="$(mktemp)"
  current_tmp="$(mktemp)"
  # RETURN, not EXIT: self_test() calls this once per case and the process
  # is not exiting between them.
  trap 'rm -f "$ledger_tmp" "$current_tmp"' RETURN
  ledger_lines > "$ledger_tmp"
  printf '%s\n' "$current" > "$current_tmp"

  # $current is passed as a FILE ARGUMENT, not piped to stdin. `python3 -`
  # already uses stdin to read the script text below (the heredoc), and a
  # pipe into the same command's stdin is not additive with that -- the
  # heredoc wins, `sys.stdin.read()` inside the script sees EOF immediately,
  # and $current is silently discarded. Found by running this against a real
  # known-positive (a pre-fix commit of #2746) and getting no output at all:
  # `bash -x` showed python3 running and returning cleanly with an empty
  # current_lines. Confirmed standalone: `printf 'hello\n' | python3 - foo
  # <<'EOF' ... sys.stdin.read() ... EOF` reads "hello" as the SCRIPT (a
  # NameError on the bare word), not as data -- the pipe's content becomes
  # python's source, never reaches the running script's own stdin read.
  python3 - "$ledger_tmp" "$current_tmp" "$CROSSCHECK_EXEMPT" "$CROSSCHECK_EXEMPT_MAX" <<'PYEOF'
import glob
import re
import sys

ledger_path = sys.argv[1]
current_lines = open(sys.argv[2], encoding='utf-8').read().splitlines()
exempt_max = int(sys.argv[4])
exempt = set()
with open(sys.argv[3], encoding='utf-8', errors='replace') as f:
    for line in f:
        line = line.rstrip('\n')
        if not line or line.startswith('#'):
            continue
        parts = line.split('\t')
        if len(parts) == 3:
            exempt.add((parts[0], parts[1]))

JOB_DIALECT = {
    "cluster": "postgres",
    "test-go/engine": "postgres",
    "multi-db/mysql": "mysql",
    "multi-db/mssql": "mssql",
}

DIALECT_MARKERS = {
    "postgres": [r'testutil\.DialectPostgres\b', r'CLEAT_TEST_POSTGRES\b', r'CLEAT_TEST_DB\b'],
    "mysql":    [r'testutil\.DialectMySQL\b', r'CLEAT_TEST_MYSQL\b'],
    "mssql":    [r'testutil\.DialectMSSQL\b', r'CLEAT_TEST_MSSQL\b'],
}


def function_bodies(path):
    """Yield (name, body) per top-level func, the same attribution rule as
    this script's own scan(): receiver-qualified for a method (Type.Method),
    plain otherwise. Deliberately re-implemented rather than shelling back
    into the awk in scan() -- this needs the BODY TEXT, which that program
    never retains, only a count."""
    try:
        text = open(path, encoding='utf-8', errors='replace').read()
    except OSError:
        return
    lines = text.split('\n')
    starts = []
    for i, line in enumerate(lines):
        if not re.match(r'^func[ \t]', line):
            continue
        m = re.match(r'^func[ \t]+\(([^)]*)\)[ \t]*([A-Za-z0-9_]+)', line)
        if m:
            recv_parts = m.group(1).split()
            rtype = recv_parts[-1].lstrip('*') if recv_parts else ''
            name = "%s.%s" % (rtype, m.group(2))
        else:
            m2 = re.match(r'^func[ \t]+([A-Za-z0-9_]+)', line)
            name = m2.group(1) if m2 else None
        if name:
            starts.append((i, name))
    for idx, (start, name) in enumerate(starts):
        end = starts[idx + 1][0] if idx + 1 < len(starts) else len(lines)
        yield name, '\n'.join(lines[start:end])


bodies = {}
for path in sorted(glob.glob('engine/*_test.go')):
    for name, body in function_bodies(path):
        # First definition wins. A real name collision is the baseline's own
        # limitation too -- it is keyed on this same (dir, fn) pair -- and
        # not something this guard can resolve any differently.
        bodies.setdefault(name, body)

ledger_by_job = {}
for line in open(ledger_path, encoding='utf-8', errors='replace'):
    line = line.rstrip('\n')
    if not line or line.startswith('#'):
        continue
    parts = line.split('\t')
    if len(parts) < 3:
        continue
    job, _count, pattern = parts[0], parts[1], parts[2]
    ledger_by_job.setdefault(job, []).append(pattern)


def covered(job, fn):
    # grep -cE semantics, same as check-skip-budget.sh's own match: a
    # SUBSTRING search, not a forced full-string anchor. Ledger lines in
    # this tree are written both ways (^Foo$ and bare Foo) and the runtime
    # checker does not care which -- this guard's notion of "covered" would
    # be wrong if it disagreed.
    for pattern in ledger_by_job.get(job, []):
        try:
            if re.search(pattern, fn):
                return True
        except re.error:
            continue
    return False


# cleat#2761 R2: only a name go test itself can run gets classified. A
# receiver-qualified method (MSSQLBackend.Setup) or a bare helper
# (newASVetProject) can be dialect-gated and skip-counted too, but a
# skip-ledger.d line has to name what go test -json ACTUALLY reports
# skipping -- the calling Test/Benchmark/Fuzz function, not the helper it
# calls. Naming the helper passes this static check and then fails
# check-skip-budget.sh with "expects 1 skip(s) matching /<helper>/, got 0",
# because runtime skip events are keyed on test names. Naming the ACTUAL
# test instead passes runtime and fails back here, since the helper is what
# is in skip-baseline.txt. There is no ledger line that satisfies both, so
# a helper is declined rather than demanded an unsatisfiable remedy.
# Measured against a fixture (newReviewMSSQLThing, single mssql gate,
# called from TestReviewUsesTheHelper): cleat-review's #2761 GAP, R2.
RUNNABLE = re.compile(r'^(Test|Benchmark|Fuzz)[A-Z0-9_]')


def classify(d, fn):
    """None if (d, fn) cannot be checked at all (wrong dir, a helper name,
    the source is unlocatable, or zero/multiple dialect markers). Otherwise
    (dialect, missing_jobs) -- missing_jobs is empty when fully covered."""
    if d != 'engine':
        return None
    if not RUNNABLE.match(fn):
        return None
    body = bodies.get(fn)
    if body is None:
        return None
    found = set()
    for dialect, markers in DIALECT_MARKERS.items():
        if any(re.search(m, body) for m in markers):
            found.add(dialect)
    if len(found) != 1:
        return None
    dialect = next(iter(found))
    missing = sorted(job for job, want in JOB_DIALECT.items()
                      if want != dialect and not covered(job, fn))
    return (dialect, missing)


current_keys = set()
violations = []
for line in current_lines:
    parts = line.split('\t')
    if len(parts) != 3:
        continue
    d, fn, _count = parts
    current_keys.add((d, fn))
    if (d, fn) in exempt:
        continue  # checked for staleness below, not for a fresh violation
    result = classify(d, fn)
    if result is None:
        continue
    dialect, missing = result
    if missing:
        violations.append((fn, dialect, missing))

# cleat#2761 R1: the exempt file is a ratchet in name only unless something
# checks it. Measured: appending #2756's own test to it, or a line for a
# test that does not exist, both left the guard at exit 0 -- the file was
# 304 lines (all of skip-baseline.txt) with only 62 (then 49, after R2's
# narrower classify()) actually suppressing anything. An exempt entry earns
# its place by being live: still present, still classifiable, and still
# genuinely uncovered. Anything else is dead weight that must be removed,
# not silently tolerated.
stale = []
for d, fn in sorted(exempt):
    if (d, fn) not in current_keys:
        stale.append((d, fn, "no longer in skip-baseline.txt (test renamed or deleted)"))
        continue
    result = classify(d, fn)
    if result is None:
        stale.append((d, fn, "no longer classifiable (not engine, not a Test/Benchmark/Fuzz name, or zero/multiple dialect markers)"))
        continue
    _dialect, missing = result
    if not missing:
        stale.append((d, fn, "already fully covered by the runtime ledger"))

problem = False

if violations:
    problem = True
    print("ERROR: dialect-gated engine test(s) are in scripts/skip-baseline.txt")
    print("but missing from the runtime skip ledger for the job(s) named:")
    print()
    for fn, dialect, missing in sorted(violations):
        print("  %s  (gated on %s; missing ledger line for: %s)" %
              (fn, dialect, ", ".join(missing)))
    print()
    print("Each missing job silently spends its __UNATTRIBUTED__ allowance")
    print("instead of a line naming this test -- cleat#2741, #2746 and #2756")
    print("all hit exactly this, hours after the static registration above")
    print("looked complete. Add a scripts/skip-ledger.d/<name>.tsv line for")
    print("each job named above; scripts/skip-ledger.d/reap-pins-a-fractional-")
    print("reclaim-timeout.tsv is a template with the same shape.")

if stale:
    problem = True
    if violations:
        print()
    print("ERROR: scripts/skip-crosscheck-exempt.txt has entries that no")
    print("longer need exempting:")
    print()
    for d, fn, why in stale:
        print("  %s\t%s  (%s)" % (d, fn, why))
    print()
    print("Remove them. skip-crosscheck-exempt.txt is a ratchet and may only")
    print("shrink -- an entry stays only while it is genuinely still")
    print("uncovered by the runtime ledger (cleat#2761 R1).")

if len(exempt) > exempt_max:
    problem = True
    if violations or stale:
        print()
    print("ERROR: scripts/skip-crosscheck-exempt.txt has %d entries, over its "
          "ceiling of %d (CROSSCHECK_EXEMPT_MAX in this script)." %
          (len(exempt), exempt_max))
    print("The ceiling only moves down, as entries get real ledger lines.")

if problem:
    sys.exit(1)
PYEOF
}

# self_test builds a fixture tree and asserts what scan() attributes a skip to.
#
# WHY THIS EXISTS AT ALL: until cleat#1740 this script had no self-test, and the
# defect it now pins survived for exactly that reason. `/^func [A-Za-z_]/`
# cannot match `func (m *MySQLBackend) Setup(`, so four skips were credited to
# two functions containing none -- and both wrong names sat in the committed
# baseline looking like ordinary entries. Nothing could notice, because nothing
# ever asserted what the scanner attributes; the guard only ever compared its
# own output to a baseline generated from that same output.
#
# THAT IS THE FAILURE MODE A REGENERATED BASELINE CANNOT CATCH. `--update`
# agrees with whatever scan() does, correct or not, so "the diff looks right"
# is a statement about the diff and not about the attribution. Hence a fixture
# whose correct answer is known independently of this script.
#
# IT RUNS THE REAL scan(), not a copy of its awk. scan() walks `find .` from
# the current directory, so cd-ing into the fixture is enough to point it at
# known input -- and a second copy of the program would be a second thing to
# keep correct, which is the defect this fix is about in another costume.
self_test() {
  local tmp xtmp ok=0 out
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp" "$xtmp"' RETURN

  mkdir -p "$tmp/pkg"
  cat > "$tmp/pkg/fixture_test.go" <<'FIXTURE'
package pkg

// A plain function, and the control: its skip must stay attributed to it.
func TestPlainFunction(t *testing.T) {
	t.Skip("control")
}

// The bug: a skip inside a METHOD used to be credited to the function above.
// This declaration sits directly above the methods for that reason.
func helperAboveTheMethods() {}

func (m *MySQLBackend) Setup(t *testing.T) {
	t.Skip("belongs to MySQLBackend.Setup")
}

// The collision case: a bare method name would merge these two into one entry.
func (m *MSSQLBackend) Setup(t *testing.T) {
	t.Skip("belongs to MSSQLBackend.Setup")
}

// A value receiver and an unnamed receiver must resolve to the type too.
func (v ValueBackend) Check(t *testing.T) {
	t.Skip("belongs to ValueBackend.Check")
}
FIXTURE

  out="$(cd "$tmp" && scan)"

  _expect() {   # _expect <line> <why>
    if grep -qF "$1" <<< "$out"; then
      return 0
    fi
    echo "SELF-TEST FAILED: expected '$1' ($2)" >&2
    ok=1
  }
  _refute() {
    if grep -qF "$1" <<< "$out"; then
      echo "SELF-TEST FAILED: did not expect '$1' ($2)" >&2
      ok=1
    fi
  }

  # The known-positive. Before cleat#1740 these four lines were absent and the
  # skips appeared under helperAboveTheMethods instead.
  _expect "pkg	MySQLBackend.Setup	1"      "a skip inside a method belongs to that method"
  _expect "pkg	MSSQLBackend.Setup	1"      "two Setup methods must not collapse into one entry"
  _expect "pkg	ValueBackend.Check	1"      "a value receiver resolves to its type"
  # The negative control: a plain function still works.
  _expect "pkg	TestPlainFunction	1"       "a plain function's skip is unchanged"
  # The defect itself, stated as a refusal.
  _refute "helperAboveTheMethods"            "no skip may be credited to the function above a method"

  # cleat#1746: a baseline key the scan no longer produces must be REPORTED.
  # Driven against a fixture baseline, because on a healthy tree "no stale
  # entries" and "the check does not run" print identically -- which is exactly
  # how this went unnoticed while develop carried one.
  local fixture="$tmp/baseline.txt"
  printf 'pkg\tTestPlainFunction\t1\npkg\tGoneAway\t2\npkg\tMySQLBackend.Setup\t1\n' > "$fixture"
  local scan_out
  scan_out="$(printf 'pkg\tTestPlainFunction\t1\npkg\tMySQLBackend.Setup\t1\n')"

  local got
  got="$(stale_entries "$scan_out" "$fixture")"
  if ! grep -qF 'GoneAway' <<< "$got"; then
    echo "SELF-TEST FAILED: a baseline entry the scan does not produce was not reported stale" >&2
    ok=1
  fi
  # The negative control, and it is the half that catches an over-eager check:
  # keys the scan DOES produce must not be reported, or every run fails and the
  # guard gets switched off.
  if grep -qE 'TestPlainFunction|MySQLBackend' <<< "$got"; then
    echo "SELF-TEST FAILED: a live baseline entry was reported stale" >&2
    ok=1
  fi
  # Prefix safety: `pkg Setup` must not be satisfied by `pkg SetupForTenant`.
  # This file has four live keys with that shape.
  printf 'pkg\tSetup\t1\n' > "$fixture"
  if ! grep -qF 'Setup	1' <<< "$(stale_entries "$(printf 'pkg\tSetupForTenant\t1\n')" "$fixture")"; then
    echo "SELF-TEST FAILED: a key matched a longer key sharing its prefix" >&2
    ok=1
  fi

  # cleat#2759: crosscheck_baseline_vs_ledger. LEDGER, LEDGER_D and
  # CROSSCHECK_EXEMPT are all paths relative to CWD, so this runs inside its
  # own fixture directory exactly like scan()'s fixture above, with a fixture
  # engine/, skip-ledger.tsv/skip-ledger.d and skip-crosscheck-exempt.txt.
  xtmp="$(mktemp -d)"
  mkdir -p "$xtmp/engine" "$xtmp/scripts/skip-ledger.d"
  cat > "$xtmp/engine/fixture_test.go" <<'XFIXTURE'
package engine

// The known-positive: single-dialect-gated, zero ledger coverage anywhere.
func TestKnownPositiveMSSQLGated(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set")
	}
}

// The negative control: identically shaped, but fully covered in the ledger.
func TestCoveredMSSQLGated(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set")
	}
}

// The exempt control: identically shaped and ledger-uncovered, but present
// in skip-crosscheck-exempt.txt -- grandfathered, must not be reported.
func TestExemptMSSQLGated(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set")
	}
}

// Declined: no dialect marker at all.
func TestUnclassifiedNoDialectMarker(t *testing.T) {
	t.Skip("some other reason")
}

// Declined: two dialect markers, not exactly one.
func TestUnclassifiedTwoDialects(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" || os.Getenv("CLEAT_TEST_MYSQL") == "" {
		t.Skip("needs both")
	}
}

// Declined: a helper, not itself a Test/Benchmark/Fuzz entry point
// (cleat#2761 R2) -- dialect-gated and uncovered, same as the
// known-positive, but a ledger line naming IT would not match any real
// go test -json skip event.
func newFixtureHelperMSSQLGated(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set")
	}
}
XFIXTURE
  : > "$xtmp/scripts/skip-ledger.tsv"
  cat > "$xtmp/scripts/skip-ledger.d/fixture.tsv" <<'XLEDGER'
cluster	1	TestCoveredMSSQLGated	fixture: covered on cluster
multi-db/mysql	1	TestCoveredMSSQLGated	fixture: covered on multi-db/mysql
test-go/engine	1	TestCoveredMSSQLGated	fixture: covered on test-go/engine
XLEDGER
  # Three exempt entries, one of each kind cleat#2761 R1 has to tell apart:
  # TestExemptMSSQLGated is LIVE (still uncovered, stays); TestCoveredMSSQLGated
  # is STALE because the ledger now covers it (same fixture line the earlier
  # "not reported as a fresh violation" check above already exercises, so
  # putting it in both is deliberate -- the two behaviours are not the same
  # check); TestGoneAwayMSSQLGated is STALE because it is not in $xcurrent at
  # all, i.e. no longer in skip-baseline.txt.
  cat > "$xtmp/scripts/skip-crosscheck-exempt.txt" <<'XEXEMPT'
engine	TestExemptMSSQLGated	1
engine	TestCoveredMSSQLGated	1
engine	TestGoneAwayMSSQLGated	1
XEXEMPT

  local xcurrent xout xstatus
  xcurrent="$(printf 'engine\tTestKnownPositiveMSSQLGated\t1\nengine\tTestCoveredMSSQLGated\t1\nengine\tTestExemptMSSQLGated\t1\nengine\tTestUnclassifiedNoDialectMarker\t1\nengine\tTestUnclassifiedTwoDialects\t1\nengine\tnewFixtureHelperMSSQLGated\t1\n')"
  xstatus=0
  xout="$(cd "$xtmp" && crosscheck_baseline_vs_ledger "$xcurrent")" || xstatus=$?

  if [ "$xstatus" -eq 0 ]; then
    echo "SELF-TEST FAILED: crosscheck_baseline_vs_ledger did not flag anything (expected the known-positive plus two stale exempt entries)" >&2
    ok=1
  fi
  if ! grep -qF 'TestKnownPositiveMSSQLGated  (gated on' <<< "$xout"; then
    echo "SELF-TEST FAILED: did not flag the known-positive as a fresh violation" >&2
    ok=1
  fi
  if grep -qF 'TestCoveredMSSQLGated  (gated on' <<< "$xout"; then
    echo "SELF-TEST FAILED: a fully ledger-covered test was reported as a FRESH violation" >&2
    ok=1
  fi
  if grep -qF 'TestExemptMSSQLGated  (gated on' <<< "$xout"; then
    echo "SELF-TEST FAILED: an exempted (still live) test was reported as a violation" >&2
    ok=1
  fi
  if grep -q 'TestExemptMSSQLGated.*(no longer\|TestExemptMSSQLGated.*(already fully' <<< "$xout"; then
    echo "SELF-TEST FAILED: a still-needed exempt entry was reported stale (cleat#2761 R1)" >&2
    ok=1
  fi
  if ! grep -qF 'TestCoveredMSSQLGated  (already fully covered by the runtime ledger)' <<< "$xout"; then
    echo "SELF-TEST FAILED: an exempt entry the ledger now covers was not reported stale (cleat#2761 R1)" >&2
    ok=1
  fi
  if ! grep -qF 'TestGoneAwayMSSQLGated  (no longer in skip-baseline.txt' <<< "$xout"; then
    echo "SELF-TEST FAILED: an exempt entry for a deleted test was not reported stale (cleat#2761 R1)" >&2
    ok=1
  fi
  if grep -qF 'TestUnclassified' <<< "$xout"; then
    echo "SELF-TEST FAILED: an unclassified test (zero or multiple dialect markers) was reported" >&2
    ok=1
  fi
  if grep -qF 'newFixtureHelperMSSQLGated' <<< "$xout"; then
    echo "SELF-TEST FAILED: a helper (not Test/Benchmark/Fuzz-named) was demanded a ledger line (cleat#2761 R2)" >&2
    ok=1
  fi

  # The ceiling, tested in isolation: one single live exempt entry, no
  # staleness at all, but a ceiling of 0 -- must still fail (cleat#2761 R1).
  printf 'engine\tTestExemptMSSQLGated\t1\n' > "$xtmp/scripts/skip-crosscheck-exempt.txt"
  local xceil_out xceil_status
  xceil_status=0
  xceil_out="$(cd "$xtmp" && CROSSCHECK_EXEMPT_MAX=0 crosscheck_baseline_vs_ledger "$xcurrent")" || xceil_status=$?
  if [ "$xceil_status" -eq 0 ]; then
    echo "SELF-TEST FAILED: an exempt file over its ceiling did not fail" >&2
    ok=1
  elif ! grep -qF 'over its ceiling of 0' <<< "$xceil_out"; then
    echo "SELF-TEST FAILED: exited non-zero but did not name the ceiling as the reason" >&2
    ok=1
  fi

  if [ "$ok" -eq 0 ]; then
    echo "self-test passed"
  fi
  return "$ok"
}

case "${1:-}" in
  --self-test)
    self_test
    exit $?
    ;;
  --update)
    fresh="$(scan)"
    die_if_scan_failed "$fresh"
    printf '%s\n' "$fresh" > "$BASELINE"
    echo "Wrote $(wc -l < "$BASELINE" | tr -d ' ') entries ($(total_skips "$fresh") skip sites) to $BASELINE"
    exit 0
    ;;
  --list)
    current="$(scan)"
    die_if_scan_failed "$current"
    printf '%s\n' "$current"
    exit 0
    ;;
  "")
    ;;
  *)
    echo "usage: $0 [--update|--list]" >&2
    exit 2
    ;;
esac

if [ ! -f "$BASELINE" ]; then
  echo "ERROR: $BASELINE is missing. Generate it with:" >&2
  echo "  scripts/check-skips.sh --update" >&2
  exit 1
fi

current="$(scan)"
die_if_scan_failed "$current"

# Set membership, not `comm`. comm requires both inputs sorted in the same
# collation it uses and silently emits garbage when they disagree -- which is
# exactly what bit check-test-only-code.sh between a darwin-generated baseline
# and the CI runner.
new="$(printf '%s\n' "$current" | grep -Fxv -f "$BASELINE" || true)"

# AND THE OTHER DIRECTION, which this script did not compute until cleat#1746.
# `new` is `current - BASELINE`. A baseline key the scanner no longer produces
# AT ALL appears in neither set: not added, not grown, not even shrunk, because
# every one of those is derived from a line that is present in `current`.
#
# So a grant covering something that is not there was invisible, and the guard
# exited 0 over it. The skip LEDGER next door has always checked this -- "a line
# matching fewer is stale ... and fails" -- and the baseline did not.
stale="$(stale_entries "$current")"

# An entry can be "new" for two different reasons, and they deserve different
# messages: a function that had no skips before, or one whose count changed.
# A count that fell is progress, so it is reported and not failed on.
added=""
grown=""
shrunk=""
while IFS= read -r line; do
  [ -n "$line" ] || continue
  key="$(cut -f1,2 <<<"$line")"
  now="$(cut -f3 <<<"$line")"
  was="$(grep -F "$key	" "$BASELINE" | cut -f3 | head -1)"
  if [ -z "$was" ]; then
    added="${added}${line}"$'\n'
  elif [ "$now" -gt "$was" ]; then
    grown="${grown}${key}	${was} -> ${now}"$'\n'
  else
    shrunk="${shrunk}${key}	${was} -> ${now}"$'\n'
  fi
done <<<"$new"

# cleat#2759: baseline registration alone does not mean the runtime budget
# knows about this skip -- see crosscheck_baseline_vs_ledger's own comment.
#
# THE FULL SCAN, not $added: an existing skip site is already accounted for
# in whichever job's __UNATTRIBUTED__ allowance was measured while it already
# existed (scripts/skip-ledger.tsv's own comment: "inherited from the
# single-number budget on 2026-09-04"), so it adds nothing to a job's runtime
# skip count today -- but "already existed" means "was in skip-baseline.txt
# before that measurement", which scripts/skip-crosscheck-exempt.txt records
# and $added does not: $added is empty the moment --update has run, which
# routinely happens in the SAME commit as the test (cleat#2756's 632107e1),
# so an $added-only version of this check went silent on exactly that PR --
# see crosscheck_baseline_vs_ledger's own header for the measurement. Running
# the full scan with no exemption at all was the very first version of this
# guard, and it reported roughly fifty pre-existing engine tests as
# violations on an otherwise-clean develop; the exempt file is what excludes
# those fifty while still seeing a same-commit cleat#2756-shaped addition.
crosscheck_out=""
crosscheck_status=0
if [ -n "$current" ]; then
  if ! crosscheck_out="$(crosscheck_baseline_vs_ledger "$current")"; then
    crosscheck_status=1
  fi
fi

status=0

if [ -n "$added" ]; then
  echo "ERROR: new conditional skips:" >&2
  echo >&2
  printf '%s' "$added" | sed 's/^/  /' >&2
  status=1
fi

if [ -n "$grown" ]; then
  echo "ERROR: skip count grew in:" >&2
  echo >&2
  printf '%s' "$grown" | sed 's/^/  /' >&2
  status=1
fi

if [ -n "$stale" ]; then
  echo "ERROR: baseline entries that match nothing in the tree:" >&2
  echo >&2
  printf '%s' "$stale" | sed 's/^/  /' >&2
  echo >&2
  echo "Each of these grants a skip for a function the scan no longer reports." >&2
  echo "That is either progress -- the test was deleted or stopped skipping --" >&2
  echo "or a stale regeneration that silently reinstated an older scan. Both" >&2
  echo "are fixed the same way, and the point of failing is that the second" >&2
  echo "one is otherwise invisible:" >&2
  echo "  scripts/check-skips.sh --update" >&2
  echo >&2
  echo "cleat#1746: a baseline regenerated from a base that predates a change" >&2
  echo "to this scanner merges CLEANLY over the newer one and reverts it." >&2
  status=1
fi

if [ "$crosscheck_status" -ne 0 ]; then
  printf '%s\n' "$crosscheck_out" >&2
  status=1
fi

if [ "$status" -ne 0 ]; then
  echo >&2
  echo "A skip is indistinguishable from a pass. Before adding one, check" >&2
  echo "which of these it is:" >&2
  echo >&2
  echo "  (a) the resource is genuinely optional and nobody asked for it" >&2
  echo "      -- skip is correct. Guard on 'was it requested', not on 'is it" >&2
  echo "      reachable'. See engine/testutil/schema.go TestDB." >&2
  echo "  (b) a DSN/env var/CI service WAS configured and is unreachable" >&2
  echo "      -- this must be t.Fatalf naming the redacted config, not a skip." >&2
  echo "  (c) the precondition is always satisfiable in this repo" >&2
  echo "      -- this must be t.Fatal." >&2
  echo >&2
  echo "If it really is (a), record it with" >&2
  echo "  scripts/check-skips.sh --update" >&2
  echo "and say in the commit message what makes the resource optional." >&2
  exit 1
fi

if [ -n "$shrunk" ]; then
  echo "NOTE: skip count fell in the following. Tighten the baseline with"
  echo "'scripts/check-skips.sh --update' to lock the improvement in:"
  echo
  printf '%s' "$shrunk" | sed 's/^/  /'
  echo
fi

echo "OK: no new conditional skips ($(total_skips "$current") skip sites across $(printf '%s\n' "$current" | grep -c .) functions)."
