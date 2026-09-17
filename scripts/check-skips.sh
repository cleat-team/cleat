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
  local tmp ok=0 out
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

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
    if printf '%s\n' "$out" | grep -qF "$1"; then
      return 0
    fi
    echo "SELF-TEST FAILED: expected '$1' ($2)" >&2
    ok=1
  }
  _refute() {
    if printf '%s\n' "$out" | grep -qF "$1"; then
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
