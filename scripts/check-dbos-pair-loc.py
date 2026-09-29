#!/usr/bin/env python3
"""Refuse a DBOS-pair README whose quoted line counts disagree with
scripts/dbos-pair-loc.sh -- the pinned cloc invocation that is supposed to be
the one source of these numbers.

cleat#2622: examples/order-lifecycle-dbos-port/README.md quoted DBOS-side
**261** after `src/server.ts` grew to 70 lines and the table was not
re-derived -- the script itself already agreed with the tree (274) the whole
time it was wrong in the README. The counter existed; nothing checked it
against what it counts.

WHAT THIS DOES: for each pair scripts/dbos-pair-loc.sh knows about, run it,
parse its cleat-side and DBOS-side `cloc` SUM lines, and compare them against
the numbers the pair's own README quotes:

  - the DBOS-side **total** row of its "Measured" table
  - the cleat-side number in "Against cleat's side ... on the same date: **N**"

Both numbers, not just one -- the README asserts a parity claim between two
sides, and a check that only re-derives one of them could pass while the
OTHER side silently drifted.

WHAT THIS DOES NOT DO: recompute cloc itself. scripts/dbos-pair-loc.sh is the
one pinned invocation (CLAUDE.md's own rule -- two counters "the same way,
slightly differently" produce numbers that look like a finding rather than a
tool disagreement), so this script is a consumer of it, not a second
implementation.

EXIT STATUS, mirroring the script it wraps:

  0  every pair's README agrees with the script
  1  a real mismatch -- the numbers disagree
  2  UNMEASURED -- the script itself could not measure (cloc missing, a
     pair's source files missing), or this script could not find what it
     needed in the README to compare against. Never conflated with 0 or 1:
     a check that cannot look must not read as either a pass or a finding.
"""

import re
import subprocess
import sys

# pair name (as scripts/dbos-pair-loc.sh takes it) -> its README
PAIRS = {
    "order-lifecycle": "examples/order-lifecycle-dbos-port/README.md",
}

SUM_RE = re.compile(r"^SUM:\s+\d+\s+\d+\s+\d+\s+(\d+)\s*$", re.MULTILINE)
TOTAL_ROW_RE = re.compile(r"^\|\s*\*\*total\*\*\s*\|[^|]*\|\s*\*\*(\d+)\*\*\s*\|\s*$", re.MULTILINE)
CLEAT_SIDE_RE = re.compile(r"Against cleat's side.*?on the same date:\s*\*\*(\d+)\*\*", re.DOTALL)


def parse_script_output(text):
    """Split scripts/dbos-pair-loc.sh's stdout into (cleat_sum, dbos_sum).

    Returns (None, None, reason) on failure to parse -- UNMEASURED, not a
    mismatch, because a parse failure says nothing about whether the numbers
    agree.
    """
    if "== cleat side ==" not in text or "== DBOS side ==" not in text:
        return None, None, "script output has neither '== cleat side ==' nor '== DBOS side ==' markers"
    cleat_part, _, dbos_part = text.partition("== DBOS side ==")
    cleat_m = SUM_RE.search(cleat_part)
    dbos_m = SUM_RE.search(dbos_part)
    if not cleat_m:
        return None, None, "no SUM: line found in the cleat-side section of script output"
    if not dbos_m:
        return None, None, "no SUM: line found in the DBOS-side section of script output"
    return int(cleat_m.group(1)), int(dbos_m.group(1)), None


def parse_readme(text):
    """Return (dbos_total, cleat_side_total, reason). reason is set, and both
    numbers None, when either could not be found -- UNMEASURED, not a silent
    pass."""
    total_m = TOTAL_ROW_RE.search(text)
    cleat_m = CLEAT_SIDE_RE.search(text)
    if not total_m and not cleat_m:
        return None, None, "found neither the '**total**' table row nor the 'Against cleat's side' sentence"
    if not total_m:
        return None, None, "found no '| **total** | ... | **N** |' row in the README's Measured table"
    if not cleat_m:
        return None, None, "found no 'Against cleat's side ... on the same date: **N**' sentence in the README"
    return int(total_m.group(1)), int(cleat_m.group(1)), None


def check_pair(pair, readme_text, script_runner):
    """script_runner(pair) -> (returncode, stdout, stderr). Returns a list of
    problems (empty if the pair's README agrees with the script) and a status
    in {"ok", "mismatch", "unmeasured"}."""
    rc, out, err = script_runner(pair)
    if rc == 2:
        return [f"{pair}: scripts/dbos-pair-loc.sh reported UNMEASURED:\n" +
                "\n".join("    " + l for l in err.splitlines())], "unmeasured"
    if rc != 0:
        return [f"{pair}: scripts/dbos-pair-loc.sh exited {rc} unexpectedly:\n{err}"], "unmeasured"

    cleat_sum, dbos_sum, reason = parse_script_output(out)
    if reason:
        return [f"{pair}: could not parse scripts/dbos-pair-loc.sh's output -- {reason}"], "unmeasured"

    dbos_total, cleat_side_total, reason = parse_readme(readme_text)
    if reason:
        return [f"{pair}: could not parse {PAIRS[pair]} -- {reason}"], "unmeasured"

    problems = []
    if dbos_total != dbos_sum:
        problems.append(
            f"{pair}: README's DBOS-side **total** row says {dbos_total}, "
            f"scripts/dbos-pair-loc.sh says {dbos_sum}"
        )
    if cleat_side_total != cleat_sum:
        problems.append(
            f"{pair}: README's cleat-side figure says {cleat_side_total}, "
            f"scripts/dbos-pair-loc.sh says {cleat_sum}"
        )
    return problems, ("mismatch" if problems else "ok")


def real_script_runner(pair):
    p = subprocess.run(["bash", "scripts/dbos-pair-loc.sh", pair], capture_output=True, text=True)
    return p.returncode, p.stdout, p.stderr


# --------------------------------------------------------------------------


SELF_TEST_SCRIPT_OUT_MATCHED = """== cleat side ==
Language                     files          blank        comment           code
Go                               3            125            403            729
SUM:                             3            125            403            729

== DBOS side ==
Language                     files          blank        comment           code
TypeScript                       3             43            109            274
SUM:                             3             43            109            274
"""

SELF_TEST_README_MATCHED = """
| role | file | code lines |
|---|---|---|
| workflow and compensation | `src/workflow.ts` | 114 |
| HTTP backend | `src/server.ts` | 70 |
| tests | `src/order.test.ts` | 90 |
| **total** | | **274** |

Against cleat's side, `cloc examples/order-lifecycle/{order.go,backend/main.go,order_test.go}`
on the same date: **729**. Re-derive both with `scripts/dbos-pair-loc.sh`, not
by re-quoting these numbers.
"""


def self_test():
    failures = []

    def matched_runner(pair):
        return 0, SELF_TEST_SCRIPT_OUT_MATCHED, ""

    problems, status = check_pair("order-lifecycle", SELF_TEST_README_MATCHED, matched_runner)
    if problems or status != "ok":
        failures.append(f"  FALSE POSITIVE on a matched fixture: {problems}")

    # Known negative #1 -- the actual cleat#2622 bug: README stuck at 261/57.
    stale_readme = SELF_TEST_README_MATCHED.replace("**70**", "**57**") \
        .replace("| `src/server.ts` | 70 |", "| `src/server.ts` | 57 |") \
        .replace("**274**", "**261**")
    problems, status = check_pair("order-lifecycle", stale_readme, matched_runner)
    if status != "mismatch" or not any("274" in p and "261" in p for p in problems):
        failures.append(f"  MISSED: cleat#2622's own bug (README total 261, script says 274) "
                        f"was not reported precisely: {problems}")

    # Known negative #2 -- the cleat side drifts instead of the DBOS side.
    stale_cleat = SELF_TEST_README_MATCHED.replace("on the same date: **729**", "on the same date: **700**")
    problems, status = check_pair("order-lifecycle", stale_cleat, matched_runner)
    if status != "mismatch" or not any("700" in p and "729" in p for p in problems):
        failures.append(f"  MISSED: a cleat-side-only drift was not reported: {problems}")

    # Known negative #3 -- the underlying script itself is UNMEASURED (cloc
    # missing). Must propagate as unmeasured, never as a pass or a mismatch.
    def unmeasured_runner(pair):
        return 2, "", "UNMEASURED: cloc is not installed\n"
    problems, status = check_pair("order-lifecycle", SELF_TEST_README_MATCHED, unmeasured_runner)
    if status != "unmeasured":
        failures.append(f"  MISSED: an UNMEASURED script result was not propagated as unmeasured "
                        f"(got status={status!r}): {problems}")

    # Known negative #4 -- this script's own README parse fails (structure
    # changed under it). Must also be unmeasured, not a silent pass.
    problems, status = check_pair("order-lifecycle", "# nothing recognizable here\n", matched_runner)
    if status != "unmeasured":
        failures.append(f"  MISSED: an unparseable README was not reported as unmeasured "
                        f"(got status={status!r}): {problems}")

    # Known negative #5 -- this script's own script-output parse fails.
    problems, status = check_pair("order-lifecycle", SELF_TEST_README_MATCHED,
                                    lambda pair: (0, "not the expected format at all", ""))
    if status != "unmeasured":
        failures.append(f"  MISSED: unparseable script output was not reported as unmeasured "
                        f"(got status={status!r}): {problems}")

    if failures:
        print("self-test FAILED:\n" + "\n".join(failures), file=sys.stderr)
        return 1
    print("self-test passed")
    return 0


def main():
    if "--self-test" in sys.argv:
        return self_test()

    all_problems = []
    any_unmeasured = False
    for pair, readme_path in PAIRS.items():
        try:
            with open(readme_path, encoding="utf-8") as fh:
                readme_text = fh.read()
        except OSError as e:
            all_problems.append(f"{pair}: could not read {readme_path}: {e}")
            any_unmeasured = True
            continue
        problems, status = check_pair(pair, readme_text, real_script_runner)
        all_problems.extend(problems)
        if status == "unmeasured":
            any_unmeasured = True

    if all_problems:
        print("\n".join(all_problems), file=sys.stderr)
        if any_unmeasured:
            print("\nUNMEASURED: at least one pair could not be checked (see above) -- "
                  "this is a failure of the check, not necessarily a finding about the pair.",
                  file=sys.stderr)
            return 2
        print("\nA DBOS pair's README quotes a number scripts/dbos-pair-loc.sh no longer "
              "produces. Re-derive with the script and update the README, noting what changed "
              "(see the README's own 'Corrected' convention) rather than silently replacing the "
              "figure.", file=sys.stderr)
        return 1

    print(f"OK: {len(PAIRS)} pair(s) agree with scripts/dbos-pair-loc.sh")
    return 0


if __name__ == "__main__":
    sys.exit(main())
