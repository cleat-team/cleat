#!/usr/bin/env bash
#
# Enforce tier 2 from tiers.yaml.
#
# Tier 2's contract is different from tier 1's, and the difference is the whole design:
#
#   tier 1   must pass. A skip is a failure. No known-failure list exists.
#   tier 2   must RUN. May fail, against `known_failures` -- a list that can only shrink.
#
# So this script does not ask "did everything pass". It asks three questions that tier 1
# never has to:
#
#   1. Did every declared PATTERN actually run a test? "Must run" is the only part of
#      tier 2's contract that is unconditional, and a suite that compiles and executes
#      nothing is the exact failure this repo keeps paying for. `go test` prints "ok" for
#      a package with zero tests, and "no test files" is not an error.
#
#      Deliberately per-pattern, not per-package. Measured 2026-08-07: ./examples/...
#      contains about a dozen packages that are stubs or generated specs and carry no
#      tests by design (fooddash/clients/*, fooddash/spec/*, onboarding, travel, ...).
#      Failing on those would make the gate permanently red for a condition nobody
#      intends to change, and a gate that is always red is a gate nobody reads. What
#      matters is that ./examples/... as a whole runs something -- that catches the real
#      failure, which is an entire suite disappearing behind a build tag or an env
#      switch.
#
#   2. Is every failure already known? A failure not in the list is a regression, and
#      fails the gate.
#
#   3. Does every known failure still fail? This is the half that makes "can only shrink"
#      true rather than aspirational. When a known failure starts passing, the entry is
#      a false statement in the manifest, and the gate fails until it is deleted. Without
#      this, the list only ever grows and tier 2 decays into "whatever is red today".
#
# Skips are allowed here -- tier2.rules.skips_allowed is true -- but they are counted and
# printed, because a tier-2 suite that skips everything satisfies "must run" only in the
# most literal sense.
#
# Usage:
#   scripts/tier2-gate.sh            enforce (exit non-zero on a new failure or a fixed one)
#   scripts/tier2-gate.sh --measure  report only, never fail -- use this to build the baseline
#
# Deliberately a separate script rather than a mode inside scripts/tier-gate.sh: that one
# is now the required `Tier 1 Gate` check, and refactoring it to serve two contracts would
# risk tier 1 in order to add tier 2.
#
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TIERS="$REPO_ROOT/tiers.yaml"
MEASURE=0
[ "${1:-}" = "--measure" ] && MEASURE=1

fail() { echo "tier2-gate: FAIL: $*" >&2; FAILED=1; }
note() { echo "tier2-gate: $*"; }
FAILED=0

[ -f "$TIERS" ] || { echo "tier2-gate: $TIERS not found" >&2; exit 2; }
command -v python3 >/dev/null || { echo "tier2-gate: python3 is required to read go test -json" >&2; exit 2; }

# --- 1. CGO must be on --------------------------------------------------------------
# Same reason as tier 1, and it applies here too: CGO_ENABLED=0 removes
# NewWasmtimeBackend (//go:build cgo) from the build entirely and silently runs
# everything on wazero. tier2 names wazero as its own component precisely because it is
# NOT the backend of record, so a tier-2 run on wazero-by-accident measures the wrong
# component and says nothing about the others.
if [ "${CGO_ENABLED:-1}" = "0" ]; then
  fail "CGO_ENABLED=0 -- this removes the wasmtime backend entirely and silently
       substitutes wazero. Unset it or set it to 1."
fi

# --- 2. Read the manifest -----------------------------------------------------------
# Same awk shape as tier-gate.sh, including the load-bearing `next`: sub() rewrites $0,
# so without it the stripped line no longer matches /^    - / and the following rule's
# exit fires after the first entry. That bug shipped once in the tier-1 gate and made it
# test 1 of 11 packages while reporting healthily.
PKGS=$(awk '/^tier2:/{t=1} t&&/^  packages:/{p=1;next} p&&/^    - /{sub(/^    - /,"");print;next} p&&/^ *#/{next} p&&/^$/{next} p{exit}' "$TIERS")
[ -n "$PKGS" ] || { echo "tier2-gate: could not read tier2.packages from $TIERS" >&2; exit 2; }

NPKG=$(echo "$PKGS" | grep -c .)
NDECL=$(awk '/^tier2:/{t=1} t&&/^  packages:/{p=1;next} p&&/^    - /{n++;next} p&&/^ *#/{next} p&&/^$/{next} p{exit} END{print n+0}' "$TIERS")
if [ "$NPKG" != "$NDECL" ]; then
  echo "tier2-gate: extracted $NPKG package pattern(s) but tiers.yaml declares $NDECL" >&2
  exit 2
fi
note "tier2.packages: $(echo "$PKGS" | tr '\n' ' ')"

# known_failures entries are flat strings, "importpathsuffix::TestName", so that this
# gate can read them with awk and a reviewer can read them without a parser. The nested
# form (package: / tests: [...]) needs a real YAML library, and pyyaml is not installed
# on this repo's runners -- a gate that cannot parse its own manifest is worse than one
# whose manifest is slightly repetitive.
KNOWN=$(awk '/^tier2:/{t=1} t&&/^  known_failures:/{p=1;next} p&&/^    - /{sub(/^    - /,"");sub(/ *#.*$/,"");gsub(/"/,"");print;next} p&&/^ *#/{next} p&&/^$/{next} p{exit}' "$TIERS")
NKNOWN=$(printf '%s\n' "$KNOWN" | grep -c . || true)
note "tier2.known_failures: $NKNOWN entr(ies)"

# --- 2b. tier2.gated_by must still describe real jobs --------------------------------
# Some tier-2 suites are gated by a dedicated workflow rather than by this script,
# because that workflow enforces something stricter (a skip budget of 0). The mapping
# lives in tiers.yaml so the coverage is auditable, and the risk of any such mapping is
# that it rots: rename the job and the manifest goes on claiming coverage while the
# required status check it names is never reported, which blocks every pull request and
# looks exactly like a check that is merely slow.
#
# So assert the job name still exists in the workflow file the manifest names. This
# cannot verify the job is still *required* -- that is branch-protection state, not in
# the tree -- and saying so is the point rather than implying a stronger check.
GATED=$(awk '/^tier2:/{t=1} t&&/^  gated_by:/{p=1;next}
             p&&/^    - suite: /{s=$0; sub(/^    - suite: /,"",s); next}
             p&&/^      job: /{j=$0; sub(/^      job: /,"",j); gsub(/"/,"",j); next}
             p&&/^      workflow: /{w=$0; sub(/^      workflow: /,"",w); print j "\t" w; next}
             p&&/^  [a-z_]+:/{exit} p{next}' "$TIERS")

if [ -n "$GATED" ]; then
  NG=$(printf '%s\n' "$GATED" | grep -c . || true)
  note "tier2.gated_by: $NG suite(s) gated by a dedicated job"
  while IFS=$'\t' read -r job wf; do
    [ -n "$job" ] || continue
    if [ ! -f "$REPO_ROOT/$wf" ]; then
      fail "tier2.gated_by names workflow '$wf', which does not exist"
    elif ! grep -qF "$job" "$REPO_ROOT/$wf"; then
      fail "tier2.gated_by claims '$job' gates a suite, but no job by that name exists in
       $wf. Either the job was renamed and the manifest was not, or the coverage is
       gone. A required status check nobody reports blocks every pull request."
    else
      note "  gated by '$job' ($wf)"
    fi
  done <<< "$GATED"
fi

if [ "$FAILED" = "1" ] && [ "$MEASURE" = "0" ]; then
  echo "tier2-gate: refusing to run -- tier2.gated_by is not describing this tree." >&2
  exit 1
fi

# --- 2c. Report unset DSNs, but do not fail on them -----------------------------------
# Tier 1 asserts its dialects connect and refuses to run otherwise. Tier 2 cannot: it
# permits skips by contract, so an absent DSN is a legitimate reason for a test not to
# run and turning that into a failure would misread the tier.
#
# But the asymmetry CLAUDE.md opens with still applies here, just more quietly. Measured
# 2026-08-07 on the same tree, same command: with all DSNs set this gate reports
# skip=7; with none set it reports skip=33 and still exits 0. Both print "tier 2 green".
# So name what is unset, next to the skip count, rather than letting a reader infer that
# a green run exercised three dialects when it exercised one.
UNSET_DSNS=""
for v in CLEAT_TEST_POSTGRES CLEAT_TEST_DB CLEAT_TEST_MYSQL CLEAT_TEST_MSSQL; do
  [ -z "$(eval "echo \"\${$v:-}\"")" ] && UNSET_DSNS="$UNSET_DSNS $v"
done
if [ -n "$UNSET_DSNS" ]; then
  note "NOTE: unset DSN(s):$UNSET_DSNS"
  note "      Tier 2 permits skips, so this will not fail -- but tests keyed on those"
  note "      dialects will skip and the run will still say green. Compare the skip"
  note "      count below against a run with them set before trusting it."
fi

# --- 3. Run ---------------------------------------------------------------------------
LOG="${TIER2_GATE_LOG:-$REPO_ROOT/tier2-gate.log}"
JSON="${TIER2_GATE_JSON:-$REPO_ROOT/tier2-gate.json}"
: > "$LOG"; : > "$JSON"

# -p 1 for the same reason tier 1 needs it: engine/testutil's CleanupPostgresTestData is
# an unqualified DELETE FROM across eleven tables, so packages run concurrently against
# one database delete each other's fixtures mid-test.
#
# -json rather than -v: attributing a test to its package from -v output means tracking
# interleaved state across lines, and gets it wrong for parallel subtests. -json carries
# Package and Test on every event.
#
# MODDIRS is read here, before the root run, so the root run can exclude any declared
# pattern that is actually a separate module (e.g. "./examples/..." once examples/ got
# its own go.mod). `go test ./examples/...` from the root does not silently match zero
# packages -- it fails outright: "directory prefix examples does not contain main module
# or its selected dependencies", reported as a synthetic failing "package" with no Test
# field. go test still runs every OTHER pattern in the same invocation (verified: the
# other five run and report normally), so this does not starve them, but there is no
# reason to invite the noise when the module is about to be tested for real, on its own,
# below. That loop writes into the same $JSON, so the "must run" check after this section
# still finds real results under the excluded pattern's prefix -- it reads combined JSON
# by package-path prefix, not by which command produced a given line.
MODDIRS=$(awk '/^tier2:/{t=1} t&&/^  modules:/{p=1;next} p&&/^    - dir: /{sub(/^    - dir: /,"");print;next} p&&/^      /{next} p&&/^ *#/{next} p&&/^$/{next} p{exit}' "$TIERS")

ROOT_PKGS=""
for p in $PKGS; do
  pre="$(echo "$p" | sed 's|^\./||; s|/\.\.\.$||')"
  in_moddir=0
  for md in $MODDIRS; do
    case "$pre" in
      "$md" | "$md"/*) in_moddir=1 ;;
    esac
  done
  [ "$in_moddir" = 1 ] || ROOT_PKGS="$ROOT_PKGS
$p"
done

note "running: go test -json -count=1 -p 1 ..."
if [ -n "$(printf '%s' "$ROOT_PKGS" | tr -d '[:space:]')" ]; then
  # shellcheck disable=SC2086
  (cd "$REPO_ROOT" && go test -json -count=1 -p 1 $ROOT_PKGS) >> "$JSON" 2>>"$LOG"
else
  note "no root-module patterns left after excluding declared tier2.modules dirs"
fi

for md in $MODDIRS; do
  [ -f "$REPO_ROOT/$md/go.mod" ] || { fail "tiers.yaml names tier-2 module '$md' but $md/go.mod does not exist"; continue; }
  note "running module: $md"
  (cd "$REPO_ROOT/$md" && go test -json -count=1 -p 1 ./...) >> "$JSON" 2>>"$LOG"
done

# --- 4. Judge --------------------------------------------------------------------------
python3 - "$JSON" "$PKGS" <<'PY' > "$REPO_ROOT/.tier2-verdict" 2>&1
import json, sys, collections

path = sys.argv[1]
# Declared patterns, e.g. "./examples/..." -> prefix "examples/". A pattern that names a
# single package ("./migration") maps to that exact path.
patterns = [p.strip() for p in sys.argv[2].splitlines() if p.strip()]
MOD = "github.com/cleat-team/cleat/"

def prefix_of(pat):
    s = pat.lstrip("./")
    return s[:-3].rstrip("/") if s.endswith("...") else s

results = {}          # (pkg, test) -> action
pkg_tests = collections.defaultdict(int)
pkg_seen = set()
malformed = 0

with open(path) as fh:
    for line in fh:
        line = line.strip()
        if not line or not line.startswith("{"):
            continue
        try:
            ev = json.loads(line)
        except json.JSONDecodeError:
            malformed += 1
            continue
        pkg = (ev.get("Package") or "").replace(MOD, "")
        act = ev.get("Action")
        test = ev.get("Test")
        if pkg:
            pkg_seen.add(pkg)
        if test and act in ("pass", "fail", "skip"):
            # Subtests report separately; count only the top-level test for identity, but
            # let a failing subtest fail its parent (go test already does this).
            if "/" in test:
                continue
            results[(pkg, test)] = act
            pkg_tests[pkg] += 1

# "Must run", evaluated per declared pattern rather than per package -- see the header.
# A pattern that ran nothing means the whole suite is absent, which is the failure worth
# catching; a stub package inside a live suite is not.
empty_patterns = []
for pat in patterns:
    pre = prefix_of(pat)
    ran = sum(n for p, n in pkg_tests.items() if p == pre or p.startswith(pre + "/"))
    if ran == 0:
        empty_patterns.append(pat)

out = {
    "malformed": malformed,
    "packages_seen": sorted(pkg_seen),
    "empty_patterns": empty_patterns,
    "failed": sorted(f"{p}::{t}" for (p, t), a in results.items() if a == "fail"),
    "passed": sorted(f"{p}::{t}" for (p, t), a in results.items() if a == "pass"),
    "skipped": sorted(f"{p}::{t}" for (p, t), a in results.items() if a == "skip"),
    "counts": {
        "tests": len(results),
        "pass": sum(1 for a in results.values() if a == "pass"),
        "fail": sum(1 for a in results.values() if a == "fail"),
        "skip": sum(1 for a in results.values() if a == "skip"),
    },
}
print(json.dumps(out))
PY

VERDICT="$REPO_ROOT/.tier2-verdict"
python3 -c "import json,sys; json.load(open(sys.argv[1]))" "$VERDICT" 2>/dev/null || {
  echo "tier2-gate: could not parse the run. First 40 lines of stderr:" >&2
  head -40 "$LOG" >&2
  exit 2
}

read_field() { python3 -c "
import json,sys
d=json.load(open('$VERDICT'))
v=d
for k in sys.argv[1].split('.'): v=v[k]
print('\n'.join(v) if isinstance(v,list) else v)" "$1"; }

TESTS=$(read_field counts.tests)
PASSN=$(read_field counts.pass)
FAILN=$(read_field counts.fail)
SKIPN=$(read_field counts.skip)
MALFORMED=$(read_field malformed)

echo
note "ran=$TESTS pass=$PASSN fail=$FAILN skip=$SKIPN   (json: $JSON)"
[ "$MALFORMED" = "0" ] || note "warning: $MALFORMED unparseable line(s) in the json stream"

# 4a. "Must run" -- a declared pattern under which nothing at all executed.
EMPTY=$(read_field empty_patterns)
if [ -n "$EMPTY" ]; then
  note "declared patterns that ran ZERO tests:"
  printf '%s\n' "$EMPTY" | while IFS= read -r p; do [ -n "$p" ] && note "    $p"; done
  fail "tier 2's contract is 'must run'. Nothing at all executed under the pattern(s)
       above, which go test reports as ok. Either the suite has no tests, or a build tag
       or env switch excluded all of them -- both are the failure this gate exists to
       catch. Do not silence this by deleting the entry from tier2.packages: a component
       nobody runs is a tier-3 component, and moving it there is the honest fix."
fi

if [ "$TESTS" = "0" ]; then
  fail "tier 2 ran zero tests in total. That is not a pass."
fi

# 4b. Every failure must be known.
FAILED_LIST=$(read_field failed)
NEWFAIL=""
if [ -n "$FAILED_LIST" ]; then
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    if grep -qxF "$f" <<< "$KNOWN"; then
      note "known failure (allowed): $f"
    else
      NEWFAIL="$NEWFAIL$f"$'\n'
    fi
  done <<< "$FAILED_LIST"
fi

if [ -n "$NEWFAIL" ]; then
  echo
  note "NEW failures, not in tier2.known_failures:"
  printf '%s' "$NEWFAIL" | while IFS= read -r f; do [ -n "$f" ] && note "    $f"; done
  fail "the failures above are regressions. Fix them, or add them to
       tier2.known_failures with an item reference and an owner and say in the PR why
       the list is growing -- tier2.contract requires a written justification for that."
fi

# 4c. Every known failure must still fail. This is what makes "can only shrink" real.
#
# THE MEMBERSHIP TESTS BELOW USE A HERE-STRING, NOT `printf ... | grep -q`, AND THAT
# IS NOT A STYLE CHOICE.
#
# They read `if printf '%s\n' "$PASSED_LIST" | grep -qxF "$k"` until 2026-08-08, and
# that construct reports the OPPOSITE of the truth under this script's own `set -o
# pipefail` (line 47), whenever the match is near the front of a large list:
#
#   * printf writes $PASSED_LIST -- 74 KB, 1565 lines -- in one write() into the pipe;
#   * grep -q finds the match early and closes its read end while most of it is still
#     unwritten;
#   * printf dies of SIGPIPE (141);
#   * pipefail takes the pipeline's status from the non-zero member, so a SUCCESSFUL
#     match reports failure.
#
# So a known failure that had been FIXED fell through to the `else` and was reported as
# "did not run at all -- renamed, deleted, or filtered out", sending the reader after a
# filtering bug that does not exist. Observed on the six examples/dag entries the moment
# they were fixed: they sort to lines 14-26 of 1565, which is exactly the shape that
# triggers it.
#
# Three things make this hard to catch, all of them worth knowing before "simplifying"
# this back:
#
#   * It is data-dependent, not flaky. A short list does not trigger it (printf finishes
#     first) and a match on the LAST line does not either (grep must read everything).
#     Only "large list, early match" does -- and it is then 100% reproducible.
#   * zsh does not reproduce it. An interactive check pasted into a zsh prompt reports
#     `pipestatus=(0 0)` and looks fine; the script runs under bash, where it is (141 0).
#   * The gate still FAILED, correctly -- only its explanation was wrong. Which is why it
#     survived: nobody re-reads the reason a red build is red.
#
# A here-string has no producer process, so there is nothing for pipefail to misread.
# Line 295 has the identical shape and got the same fix; $KNOWN is small enough today
# that it was not yet triggering, which is not a reason to leave it.
PASSED_LIST=$(read_field passed)
SKIPPED_LIST=$(read_field skipped)
FIXED=""
if [ -n "$KNOWN" ]; then
  while IFS= read -r k; do
    [ -n "$k" ] || continue
    if grep -qxF "$k" <<< "$PASSED_LIST"; then
      FIXED="$FIXED$k (now passes)"$'\n'
    elif ! grep -qxF "$k" <<< "$FAILED_LIST"; then
      if grep -qxF "$k" <<< "$SKIPPED_LIST"; then
        FIXED="$FIXED$k (now skips -- a skip is not a failure, so this entry is wrong)"$'\n'
      else
        FIXED="$FIXED$k (did not run at all -- renamed, deleted, or filtered out)"$'\n'
      fi
    fi
  done <<< "$KNOWN"
fi

if [ -n "$FIXED" ]; then
  echo
  note "stale tier2.known_failures entries:"
  printf '%s' "$FIXED" | while IFS= read -r f; do [ -n "$f" ] && note "    $f"; done
  fail "the entries above no longer describe this tree. tier2.contract says the list may
       only shrink; an entry that does not fail is a false statement in the manifest and
       hides the next regression in the same test. Delete them."
fi

echo
if [ "$FAILED" = "1" ]; then
  if [ "$MEASURE" = "1" ]; then
    note "--measure: reporting only, not failing the build"
    note "baseline candidates for tier2.known_failures:"
    printf '%s\n' "$FAILED_LIST" | while IFS= read -r f; do [ -n "$f" ] && echo "    - \"$f\""; done
    exit 0
  fi
  exit 1
fi

note "tier 2 green: $PASSN passed, $FAILN failed (all known), $SKIPN skipped, 0 regressions"
