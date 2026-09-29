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

cleat#2597 (the integration-hub pair, in the same PR that split
dbos-pair-loc.sh's output into "app" and "platform" groups): the section
markers this file's SELF-TEST FIXTURE reproduced by hand ("== cleat side
==") drifted from what the script actually prints ("== cleat: app ==")
the moment that PR renamed them, and the self-test still passed --
because it is a fixture, not the real script's output, so it can drift
independently and never notice. Only running the REAL check against the
tree (this file's own `main()`, not `--self-test`) caught it, reporting
UNMEASURED rather than a false pass. Extending PAIRS to a pair whose
output shape differs further is tracked separately (cleat#2632) rather
than attempted here.

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
    if "== cleat: app ==" not in text or "== DBOS: app ==" not in text:
        return None, None, "script output has neither '== cleat: app ==' nor '== DBOS: app ==' markers"
    cleat_part, _, dbos_part = text.partition("== DBOS: app ==")
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


def interface_failures(pair, runner):
    """Run `runner(pair)` (same signature as a script_runner) and check
    whether parse_script_output can find its markers in the result --
    the INTERFACE question, never the numbers.

    Returns (failures, unmeasured): `failures` is non-empty only when the
    runner produced output but this file's parser could not read it (a
    real interface break); `unmeasured` is non-empty only when the runner
    itself could not produce output at all (cloc missing, or an
    unexpected non-zero exit). The two are never mixed in one list, so a
    caller can tell "the interface changed" from "this check could not
    run" without inspecting message text.

    Used by self_test() for BOTH the real check (against real_script_runner)
    and its known-negatives (against a runner that simulates a broken
    producer) -- the same function on both sides is what stops a negative
    from silently exercising a different code path than the real check, the
    exact way cleat#2633 itself first slipped past.
    """
    rc, out, err = runner(pair)
    if rc == 2:
        return [], [f"UNMEASURED calling scripts/dbos-pair-loc.sh {pair}: {err.strip()}"]
    if rc != 0:
        return [], [f"scripts/dbos-pair-loc.sh {pair} exited {rc} unexpectedly: {err.strip()}"]
    _, _, reason = parse_script_output(out)
    if reason:
        return [
            f"INTERFACE BROKEN: scripts/dbos-pair-loc.sh {pair}'s real output no longer has "
            f"the markers this parser needs ({reason}). dbos-pair-loc.sh's output labels are "
            f"an interface this script parses -- renaming one is a breaking edit to it."
        ], []
    return [], []


# --------------------------------------------------------------------------


SELF_TEST_SCRIPT_OUT_MATCHED = """== cleat: app ==
Language                     files          blank        comment           code
Go                               3            125            403            729
SUM:                             3            125            403            729

== DBOS: app ==
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

    # cleat#2633 -- everything above tests the PARSER against fixtures this
    # file writes and controls, which is exactly how cleat#2621 went
    # undetected: SELF_TEST_SCRIPT_OUT_MATCHED is a hand-written COPY of
    # dbos-pair-loc.sh's markers, and when the producer renamed them
    # ("== cleat side ==" -> "== cleat: app =="), the copy did not follow,
    # so this self-test kept passing while the real check against the tree
    # returned UNMEASURED. A hand-written fixture can confirm the parser
    # handles a shape; it cannot notice that the shape stopped being
    # produced.
    #
    # So this checks the INTERFACE rather than the fixture: run the REAL
    # dbos-pair-loc.sh for every registered pair and confirm this file's
    # parser can still find the markers it needs in what that script
    # ACTUALLY prints today -- never comparing against a copy of it.
    #
    # DELIBERATELY DOES NOT CHECK WHETHER THE NUMBERS AGREE. That is
    # main()'s job, and it is allowed to fail on a genuinely stale README
    # without that reading as a broken self-test -- conflating "the
    # interface still parses" with "the README is still accurate" would
    # make an ordinary drift look like this script itself being broken.
    #
    # A first version of this self-test ran the loop over `real_script_runner`
    # inline, then separately fed a renamed-producer string straight into
    # parse_script_output for its negative -- which meant the negative
    # re-tested the parser (already covered by known negative #5) and never
    # went through the loop's own call site at all. Factoring the shared
    # "call runner, check markers" logic into `interface_failures` and
    # calling it from both the real loop and the negatives below means A
    # VACUOUS `interface_failures` TRIPS THE NEGATIVES -- verified by making
    # its body return `[], []` unconditionally and watching both negatives
    # report MISSED.
    #
    # THAT DOES NOT COVER EVERY WAY THE LOOP ITSELF COULD GO VACUOUS --
    # cleat-review's re-review measured two mutations the function-level fix
    # does not reach: `for pair in PAIRS` narrowed to iterate nothing, and
    # this call site's `real_script_runner` argument swapped for a fixture
    # runner. Both leave `interface_failures` itself correct and untested by
    # the negatives below, which call it directly with their own runners.
    # The count check just below closes the first (a loop that checks zero
    # pairs is itself a finding); the second is a one-line call-site swap in
    # plain sight, left as a residual rather than guarded, the same
    # trade this file's own header makes for scripts/dbos-pair-loc.sh's
    # numbers ("recompute cloc" is out of scope; so is guarding the
    # `interface_failures(pair, real_script_runner)` call below).
    all_unmeasured = []
    pairs_checked = 0
    for pair in PAIRS:
        f, u = interface_failures(pair, real_script_runner)
        failures.extend(f)
        all_unmeasured.extend(u)
        pairs_checked += 1
    if not PAIRS:
        failures.append("  PAIRS is empty -- the interface check has nothing to run against")
    elif pairs_checked != len(PAIRS):
        failures.append(f"  the interface check loop ran for {pairs_checked} of {len(PAIRS)} "
                        f"registered pairs -- something short-circuited it")

    # Known negative -- simulates exactly the cleat#2621 regression: a
    # producer that renamed its own markers. Goes through interface_failures
    # like the real check above, not a separate hand-rolled call.
    def renamed_producer(pair):
        return 0, SELF_TEST_SCRIPT_OUT_MATCHED.replace("cleat: app", "cleat side")\
            .replace("DBOS: app", "DBOS side"), ""
    f, u = interface_failures("order-lifecycle", renamed_producer)
    if not f:
        failures.append("  MISSED: a producer that renamed its section markers "
                        "(simulating cleat#2621) was not detected as an interface break")

    # Known negative -- the interface check's OWN precondition can fail
    # (cloc missing when scripts/dbos-pair-loc.sh runs for real), and that
    # must come back UNMEASURED, never as a failure and never silently
    # ignored. Before this, an UNMEASURED here was appended to `failures`
    # and returned exit 1 -- indistinguishable from a real interface break,
    # which is the same status-conflation this file's own header warns
    # against for scripts/dbos-pair-loc.sh's exit codes.
    def cloc_missing_runner(pair):
        return 2, "", "UNMEASURED: cloc is not installed\n"
    f, u = interface_failures("order-lifecycle", cloc_missing_runner)
    if f or not u:
        failures.append("  MISSED: an UNMEASURED interface-check runner was not propagated "
                        "as unmeasured (got failures=%r unmeasured=%r)" % (f, u))

    if failures:
        print("self-test FAILED:\n" + "\n".join(failures), file=sys.stderr)
        return 1
    if all_unmeasured:
        print("self-test UNMEASURED: " + "; ".join(all_unmeasured), file=sys.stderr)
        print("UNMEASURED: this is a failure of the self-test's own environment "
              "(e.g. cloc missing), not a finding about the checker", file=sys.stderr)
        return 2
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
