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
parse its cleat-side and other-side (DBOS, or DBOS-isolated for the sandboxed
pair) `cloc` totals, and compare them against the numbers the pair's own
README quotes.

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
UNMEASURED rather than a false pass.

cleat#2632: `order-lifecycle` (the control pair) prints one section per
side -- "== cleat: app ==", "== DBOS: app ==" -- so its app total IS that
one section's SUM. `integration-hub` (the wedge) is not that shape: each
side's app total is the SUM of three separate role sections ("tenant
code", "host runner", "unit tests"), printed alongside a CONTROL row, an
e2e harness row and a platform row that are deliberately excluded from
the total (see scripts/dbos-pair-loc.sh's own header comment for why).
Registering it is therefore a second PARSER, not a second dict entry --
SCRIPT_PARSERS and README_PARSERS below dispatch on the pair name, and a
role section that is renamed, missing, or duplicated reports UNMEASURED
via the same tri-state as everything else here, never a partial total.

WHAT THIS DOES NOT DO: recompute cloc itself. scripts/dbos-pair-loc.sh is the
one pinned invocation (CLAUDE.md's own rule -- two counters "the same way,
slightly differently" produce numbers that look like a finding rather than a
tool disagreement), so this script is a consumer of it, not a second
implementation. That includes cloc's own SUM arithmetic: where cloc omits a
section's "SUM:" line because the section is a single file (see
section_code_sum below), this reads the one row cloc already printed rather
than summing anything itself.

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
    "integration-hub": "examples/integration-hub-dbos-port/README.md",
    "b2b-saas-control-plane": "examples/b2b-saas-control-plane-dbos-port/README.md",
}

# pair name -> what the README calls the non-cleat side, for problem messages
OTHER_SIDE_LABEL = {
    "order-lifecycle": "DBOS",
    "integration-hub": "DBOS-isolated",
    "b2b-saas-control-plane": "DBOS",
}

SUM_RE = re.compile(r"^SUM:\s+\d+\s+\d+\s+\d+\s+(\d+)\s*$", re.MULTILINE)
TOTAL_ROW_RE = re.compile(r"^\|\s*\*\*total\*\*\s*\|[^|]*\|\s*\*\*(\d+)\*\*\s*\|\s*$", re.MULTILINE)
CLEAT_SIDE_RE = re.compile(r"Against cleat's side.*?on the same date:\s*\*\*(\d+)\*\*", re.DOTALL)

# The wedge's role table: "| **app total** (tenant + host + unit tests) |
# **130** | **200** |" -- cleat's figure then DBOS-isolated's, both bold
# table cells rather than order-lifecycle's prose sentence.
WEDGE_APP_TOTAL_RE = re.compile(
    r"^\|\s*\*\*app total\*\*[^|]*\|\s*\*\*(\d+)\*\*\s*\|\s*\*\*(\d+)\*\*\s*\|\s*$",
    re.MULTILINE,
)

# The three role rows above the app total: "| tenant code | **55** | **24** |".
# cleat-review on cleat#2749: comparing only the app-total row lets
# compensating errors on the two sides of a single role (e.g. tenant code
# quoted 5 high, host runner quoted 5 low) pass, because their sum still
# matches. Each role name appears in exactly one such row.
WEDGE_ROLE_ROW_RE = re.compile(
    r"^\|\s*(tenant code|host runner|unit tests|behaviour assertions)\s*\|\s*\*\*(\d+)\*\*\s*\|\s*\*\*(\d+)\*\*\s*\|\s*$",
    re.MULTILINE,
)
INTEGRATION_HUB_ROLE_NAMES = ("tenant code", "host runner", "unit tests", "behaviour assertions")

SECTION_HEADER_RE = re.compile(r"^==\s*(.+?)\s*==\s*$", re.MULTILINE)

# A cloc language row: a name (letters, digits, and the handful of symbols
# cloc's own language names use -- "C/C++ Header", "Bourne Shell") followed
# by four whitespace-separated integers (files, blank, comment, code).
# Deliberately not anchored to a known language list: the point is to read
# whatever cloc printed, not to guess which languages it might use.
LANG_ROW_RE = re.compile(r"^[A-Za-z][A-Za-z0-9+#/ .]*?\s+\d+\s+\d+\s+\d+\s+(\d+)\s*$", re.MULTILINE)

INTEGRATION_HUB_CLEAT_ROLE_PREFIXES = (
    "cleat: tenant code", "cleat: host runner", "cleat: unit tests",
    # cleat#2642: summed on BOTH sides. Before it, a behaviour's test cost
    # landed in cleat's e2e harness (never summed) and in DBOS's unit-test
    # file (summed), so identical work moved the comparison one way only.
    "cleat: behaviour assertions",
)
INTEGRATION_HUB_DBOS_ROLE_PREFIXES = (
    "DBOS-isolated: tenant code", "DBOS-isolated: host runner", "DBOS-isolated: unit tests",
    "DBOS-isolated: behaviour assertions",
)


def split_sections(text):
    """Split scripts/dbos-pair-loc.sh's stdout into {header label: body text},
    in encounter order. A section runs from one '== label ==' line to the
    next such line, or to the end of the output."""
    headers = list(SECTION_HEADER_RE.finditer(text))
    sections = {}
    for i, m in enumerate(headers):
        start = m.end()
        end = headers[i + 1].start() if i + 1 < len(headers) else len(text)
        sections[m.group(1)] = text[start:end]
    return sections


def section_code_sum(body):
    """Return a cloc report section's total **code** column, or None if this
    section's body has neither a 'SUM:' line nor exactly one language row.

    cloc omits its own SUM line when a section is a single file -- observed
    on scripts/dbos-pair-loc.sh integration-hub's single-file groups
    (DBOS-isolated's host runner, DBOS-isolated's unit tests, and either
    side's e2e harness): the language row already IS the total, so cloc
    does not repeat it under a SUM label. Falling back to that lone row's
    code column reads the number cloc already printed; it does not
    recompute anything cloc did not already say.
    """
    if "(no files -- 0 lines" in body:
        return 0
    m = SUM_RE.search(body)
    if m:
        return int(m.group(1))
    rows = LANG_ROW_RE.findall(body)
    if len(rows) == 1:
        return int(rows[0])
    return None


def _role_section_values(sections, prefixes):
    """Return ([value, ...], reason), one value per prefix in `prefixes`, in
    order. reason is set, and the list None, unless every prefix matches
    exactly one section whose body itself parses cleanly -- a renamed,
    missing, or duplicated role section, or one cloc could not summarise, is
    UNMEASURED rather than a partial result."""
    values = []
    for prefix in prefixes:
        matches = [label for label in sections if label.startswith(prefix)]
        if len(matches) != 1:
            return None, f"expected exactly one section starting with {prefix!r}, found {len(matches)}"
        value = section_code_sum(sections[matches[0]])
        if value is None:
            return None, f"could not read a code total from the {matches[0]!r} section"
        values.append(value)
    return values, None


def _sum_role_sections(sections, prefixes):
    """Sum section_code_sum() over exactly one section per prefix in
    `prefixes`. Returns (total, reason); see _role_section_values, which
    this wraps -- the per-role values themselves are integration_hub_role_
    mismatches' job, not this one's."""
    values, reason = _role_section_values(sections, prefixes)
    if reason:
        return None, reason
    return sum(values), None


def parse_order_lifecycle_script_output(text):
    """Split scripts/dbos-pair-loc.sh order-lifecycle's stdout into
    (cleat_total, dbos_total). Returns (None, None, reason) on failure to
    parse -- UNMEASURED, not a mismatch, because a parse failure says
    nothing about whether the numbers agree."""
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


def parse_integration_hub_script_output(text):
    """Split scripts/dbos-pair-loc.sh integration-hub's stdout into
    (cleat_total, dbos_isolated_total), each the SUM of that side's three
    role sections (tenant code, host runner, unit tests) -- the CONTROL,
    e2e harness and platform sections are deliberately excluded, mirroring
    the README's own 'app total' row."""
    sections = split_sections(text)
    if not sections:
        return None, None, "no '== ... ==' section markers found in script output"
    cleat_total, reason = _sum_role_sections(sections, INTEGRATION_HUB_CLEAT_ROLE_PREFIXES)
    if reason:
        return None, None, f"cleat side: {reason}"
    dbos_total, reason = _sum_role_sections(sections, INTEGRATION_HUB_DBOS_ROLE_PREFIXES)
    if reason:
        return None, None, f"DBOS-isolated side: {reason}"
    return cleat_total, dbos_total, None


def parse_order_lifecycle_readme(text):
    """Return (cleat_total, dbos_total, reason). reason is set, and both
    numbers None, when either could not be found -- UNMEASURED, not a
    silent pass."""
    total_m = TOTAL_ROW_RE.search(text)
    cleat_m = CLEAT_SIDE_RE.search(text)
    if not total_m and not cleat_m:
        return None, None, "found neither the '**total**' table row nor the 'Against cleat's side' sentence"
    if not total_m:
        return None, None, "found no '| **total** | ... | **N** |' row in the README's Measured table"
    if not cleat_m:
        return None, None, "found no 'Against cleat's side ... on the same date: **N**' sentence in the README"
    return int(cleat_m.group(1)), int(total_m.group(1)), None


def parse_integration_hub_readme(text):
    """Return (cleat_total, dbos_isolated_total, reason) from the wedge's
    role table's '| **app total** ... | **N** | **N** |' row."""
    m = WEDGE_APP_TOTAL_RE.search(text)
    if not m:
        return None, None, ("found no '| **app total** ... | **N** | **N** |' row "
                             "(cleat, then DBOS-isolated) in the README")
    return int(m.group(1)), int(m.group(2)), None


SCRIPT_PARSERS = {
    "order-lifecycle": parse_order_lifecycle_script_output,
    "integration-hub": parse_integration_hub_script_output,
    # Same shape as order-lifecycle: one app section per side, and the pair's
    # whole README-side claim is the single total -- so the parser is reused
    # rather than re-implemented ("a second dict entry, not a second parser"),
    # even though its name says order-lifecycle. It keys on the section
    # markers, not on the pair.
    "b2b-saas-control-plane": parse_order_lifecycle_script_output,
}

README_PARSERS = {
    "order-lifecycle": parse_order_lifecycle_readme,
    "integration-hub": parse_integration_hub_readme,
    # Likewise the same README shape: a "| **total** | | **N** |" row plus an
    # "Against cleat's side ... on the same date: **N**" sentence.
    "b2b-saas-control-plane": parse_order_lifecycle_readme,
}


def integration_hub_role_mismatches(script_out, readme_text):
    """Compare the wedge's three role rows (tenant code, host runner, unit
    tests) individually, cleat and DBOS-isolated -- not just their sum (the
    app-total row parse_integration_hub_readme/parse_integration_hub_script_
    output already compare). cleat-review on cleat#2749: without this, a
    role quoted high on one row and low on another by the same amount
    passes, because the total each check already makes is unaffected.

    Returns (problems, reason): reason is set, problems empty, only on an
    interface break (a renamed/missing role section, or no role table in
    the README) -- UNMEASURED, same convention as everywhere else here.
    """
    sections = split_sections(script_out)
    if not sections:
        return [], "no '== ... ==' section markers found in script output"
    cleat_values, reason = _role_section_values(sections, INTEGRATION_HUB_CLEAT_ROLE_PREFIXES)
    if reason:
        return [], f"cleat side: {reason}"
    dbos_values, reason = _role_section_values(sections, INTEGRATION_HUB_DBOS_ROLE_PREFIXES)
    if reason:
        return [], f"DBOS-isolated side: {reason}"

    # A dict keyed by role name would let a second matching row (anywhere
    # else in the README) silently overwrite the first rather than being
    # noticed -- cleat-review on cleat#2749. Count occurrences instead of
    # collapsing them, so more than one is UNMEASURED, not a silent pick.
    readme_rows = {}
    role_counts = {}
    for m in WEDGE_ROLE_ROW_RE.finditer(readme_text):
        role = m.group(1)
        role_counts[role] = role_counts.get(role, 0) + 1
        readme_rows[role] = (int(m.group(2)), int(m.group(3)))
    problems = []
    for i, role in enumerate(INTEGRATION_HUB_ROLE_NAMES):
        if role_counts.get(role, 0) > 1:
            return [], f"found {role_counts[role]} '| {role} | **N** | **N** |' rows in the README, expected exactly one"
        if role not in readme_rows:
            return [], f"found no '| {role} | **N** | **N** |' row in the README's role table"
        readme_cleat, readme_dbos = readme_rows[role]
        if readme_cleat != cleat_values[i]:
            problems.append(
                f"integration-hub: README's {role!r} row says cleat={readme_cleat}, "
                f"scripts/dbos-pair-loc.sh says {cleat_values[i]}"
            )
        if readme_dbos != dbos_values[i]:
            problems.append(
                f"integration-hub: README's {role!r} row says DBOS-isolated={readme_dbos}, "
                f"scripts/dbos-pair-loc.sh says {dbos_values[i]}"
            )
    return problems, None


# pair name -> an extra check run beyond the total-vs-total comparison every
# pair already gets. Absent for order-lifecycle, which has no per-role rows
# to compare -- its whole README-side claim IS the one total.
EXTRA_CHECKERS = {
    "integration-hub": integration_hub_role_mismatches,
}


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

    cleat_sum, other_sum, reason = SCRIPT_PARSERS[pair](out)
    if reason:
        return [f"{pair}: could not parse scripts/dbos-pair-loc.sh's output -- {reason}"], "unmeasured"

    cleat_readme, other_readme, reason = README_PARSERS[pair](readme_text)
    if reason:
        return [f"{pair}: could not parse {PAIRS[pair]} -- {reason}"], "unmeasured"

    other_label = OTHER_SIDE_LABEL[pair]
    problems = []
    if cleat_readme != cleat_sum:
        problems.append(
            f"{pair}: README's cleat-side figure says {cleat_readme}, "
            f"scripts/dbos-pair-loc.sh says {cleat_sum}"
        )
    if other_readme != other_sum:
        problems.append(
            f"{pair}: README's {other_label}-side figure says {other_readme}, "
            f"scripts/dbos-pair-loc.sh says {other_sum}"
        )

    extra_checker = EXTRA_CHECKERS.get(pair)
    if extra_checker:
        extra_problems, reason = extra_checker(out, readme_text)
        if reason:
            return problems + [f"{pair}: could not check per-role rows -- {reason}"], "unmeasured"
        problems.extend(extra_problems)

    return problems, ("mismatch" if problems else "ok")


def real_script_runner(pair):
    p = subprocess.run(["bash", "scripts/dbos-pair-loc.sh", pair], capture_output=True, text=True)
    return p.returncode, p.stdout, p.stderr


def interface_failures(pair, runner):
    """Run `runner(pair)` (same signature as a script_runner) and check
    whether this pair's SCRIPT_PARSERS entry can find its markers in the
    result -- the INTERFACE question, never the numbers.

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
    _, _, reason = SCRIPT_PARSERS[pair](out)
    if reason:
        return [
            f"INTERFACE BROKEN: scripts/dbos-pair-loc.sh {pair}'s real output no longer has "
            f"the markers this parser needs ({reason}). dbos-pair-loc.sh's output labels are "
            f"an interface this script parses -- renaming one is a breaking edit to it."
        ], []
    return [], []


# --------------------------------------------------------------------------


# MODELS cloc's real output, including its single-file quirk: cloc omits the
# "SUM:" line when a section is one file, so the two scenario-harness sections
# cleat#2642 appended to scripts/dbos-pair-loc.sh order-lifecycle carry NO
# "SUM:" line (measured: those sections print 0 of them, the two app sections
# print 1 each). A first version of this fixture gave them a SUM line anyway --
# which made the fixture disagree with the program it models, so a
# falsification run against it (switch the parser to the LAST SUM) "caught" a
# defect in the fixture rather than in the parse. cleat-review's GAP on PR
# #3040. The trailing-SUM case that control was reaching for is now its own
# deliberately-unfaithful fixture below, where it can actually discriminate.
SELF_TEST_SCRIPT_OUT_MATCHED = """== cleat: app ==
Language                     files          blank        comment           code
Go                               3            125            403            729
SUM:                             3            125            403            729

== DBOS: app ==
Language                     files          blank        comment           code
TypeScript                       3             43            109            274
SUM:                             3             43            109            274

== cleat: scenario harness (own line -- never summed into the app total) ==
Language                     files          blank        comment           code
Bourne Shell                     1             63            343            438

== DBOS: scenario harness (own line -- never summed into the app total) ==
Language                     files          blank        comment           code
Bourne Shell                     1             26             63            165
"""

# DELIBERATELY UNFAITHFUL, and that is the point: the matched fixture above
# models what cloc prints today, so it cannot test what happens if a trailing
# section ever DOES print a "SUM:" -- and that is the only thing that would
# put a second SUM in a side's part and make the parser's first-vs-last choice
# matter. This variant adds those two SUM lines back and is fed to the parser
# in its own self-test case: the app totals must still come out, which fails
# for a last-SUM read. Its job is to disagree, like the loose parse in
# CLAUDE.md's second-reading table -- not to model the program.
SELF_TEST_SCRIPT_OUT_TRAILING_SUM = SELF_TEST_SCRIPT_OUT_MATCHED.replace(
    "Bourne Shell                     1             63            343            438",
    "Bourne Shell                     1             63            343            438\n"
    "SUM:                             1             63            343            438",
).replace(
    "Bourne Shell                     1             26             63            165",
    "Bourne Shell                     1             26             63            165\n"
    "SUM:                             1             26             63            165",
)

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

# The wedge's shape, matching what scripts/dbos-pair-loc.sh integration-hub
# actually prints: three role sections per side, and -- deliberately, to
# exercise section_code_sum's single-file fallback the way the real script
# does -- the DBOS-isolated side's host-runner and unit-tests sections carry
# no 'SUM:' line, because each is exactly one file. If this fixture instead
# hand-added a SUM line cloc would not really print, the fallback would ship
# with no self-test coverage at all and rely solely on live cloc behaving
# the way this file assumes.
SELF_TEST_WEDGE_SCRIPT_OUT_MATCHED = """== CONTROL (excluded from the comparison total): bare DBOS.runStep, no sandbox ==
Language                     files          blank        comment           code
TypeScript                       2             18            115            104
SUM:                             2             18            115            104

== cleat: tenant code ==
Language                     files          blank        comment           code
Go                               3             11             73             55
SUM:                             3             11             73             55

== cleat: host runner (extracted from hub.go -- dispatch block + every other TenantStepName-related line) ==
Language                     files          blank        comment           code
Go                               5              0             23             25
SUM:                             5              0             23             25

== cleat: unit tests (extracted from hub_test.go) ==
Language                     files          blank        comment           code
Go                               2              8              2             50
SUM:                             2              8              2             50

== DBOS-isolated: tenant code (extracted template literals) ==
Language                     files          blank        comment           code
TypeScript                       3              0              0             24
SUM:                             3              0              0             24

== DBOS-isolated: host runner (isolated-workflow.ts minus tenant code) ==
Language                     files          blank        comment           code
TypeScript                       1             12             99             54

== DBOS-isolated: unit tests (isolated-wedge.test.ts -- its three behaviour functions are counted in the behaviour row below) ==
Language                     files          blank        comment           code
TypeScript                       1             14            129             72

== BEHAVIOUR ASSERTIONS, both sides -- SUMMED INTO BOTH APP TOTALS (cleat#2642) ==
== cleat: behaviour assertions (the three behaviour blocks of the e2e harness) ==
Language                     files          blank        comment           code
Bourne Shell                     3              3              8             85
SUM:                             3              3              8             85

== DBOS-isolated: behaviour assertions (the three test functions of isolated-wedge.test.ts) ==
Language                     files          blank        comment           code
TypeScript                       3              0              8             50
SUM:                             3              0              8             50

== E2E HARNESS MACHINERY, both sides -- shown, NOT summed into either app total (see header comment) ==
== cleat: e2e harness machinery (the harness minus its three behaviour blocks -- drives a real deployed worker over HTTP) ==
Language                     files          blank        comment           code
Bourne Shell                     1             16            108            154

== DBOS-isolated: e2e harness (npm install/build/test wrapper -- the assertions live in the unit test above; no runtime code intake to exercise) ==
Language                     files          blank        comment           code
Bourne Shell                     1             10             39             42

== cleat: platform (own line -- never summed into the app total) ==
Language                     files          blank        comment           code
Go                               2             14            108            124
SUM:                             2             14            108            124

== DBOS-isolated: platform (own line -- never summed into the app total) ==
(no files -- 0 lines, see header comment for why this is not a gap)
"""

SELF_TEST_WEDGE_README_MATCHED = """
| role | cleat | DBOS-isolated |
|---|---:|---:|
| tenant code | **55** | **24** |
| host runner | **25** | **54** |
| unit tests | **50** | **72** |
| behaviour assertions | **85** | **50** |
| **app total** (tenant + host + unit tests + behaviour assertions) | **215** | **200** |
| platform (own line -- not summed above) | **124** | **0** |
| e2e harness machinery (own line -- not summed above, see below) | **154** | **42** |
"""


def self_test():
    failures = []

    def matched_runner(pair):
        return 0, SELF_TEST_SCRIPT_OUT_MATCHED, ""

    problems, status = check_pair("order-lifecycle", SELF_TEST_README_MATCHED, matched_runner)
    if problems or status != "ok":
        failures.append(f"  FALSE POSITIVE on a matched fixture: {problems}")

    # cleat#2642 / cleat-review's GAP on PR #3040: sections appended AFTER
    # "== DBOS: app ==" must not displace the totals the parser reports. The
    # matched fixture above cannot test this -- its trailing sections carry no
    # SUM line (cloc prints none for a single file), so each side's part holds
    # exactly one SUM however the parse is written. This case feeds the
    # deliberately-unfaithful variant that DOES give them SUM lines, which is
    # the only shape where a last-SUM read would return the harness's figure
    # instead of the app total. Falsified by switching the parser to the last
    # SUM: this case then reports DBOS as 165, not 274.
    #
    # THE DERIVATION ITSELF NEEDS THE ASSERTION BELOW, and this case is the
    # only one that does. SELF_TEST_SCRIPT_OUT_TRAILING_SUM is built by
    # .replace() on the faithful fixture, and this case asserts a PASS -- so an
    # edit to those harness rows makes the derivation a NO-OP, leaves the
    # variant identical to the faithful one, and the case then passes on a
    # fixture with no trailing SUM at all: a control that silently stopped
    # controlling, which is the same shape as the defect it exists to catch.
    # Every other derived fixture in this self-test asserts a MISMATCH, so a
    # no-op makes those fail loudly and they need no such guard. cleat-review's
    # note on PR #3040.
    if SELF_TEST_SCRIPT_OUT_TRAILING_SUM == SELF_TEST_SCRIPT_OUT_MATCHED:
        failures.append("  the trailing-SUM fixture is identical to the faithful one -- the "
                        "derivation no-opped, so the case below is testing nothing")

    problems, status = check_pair("order-lifecycle", SELF_TEST_README_MATCHED,
                                    lambda pair: (0, SELF_TEST_SCRIPT_OUT_TRAILING_SUM, ""))
    if problems or status != "ok":
        failures.append(f"  MISSED: a SUM line appended below the app sections displaced the parsed "
                        f"totals (a trailing section must not become the total): {problems}")

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

    # cleat#2632 -- the wedge shape: a matched fixture (which also exercises
    # section_code_sum's single-file, no-'SUM:'-line fallback on the
    # DBOS-isolated host-runner and unit-tests sections, the real shape
    # cloc produces for them), then one drift on each side.
    def wedge_matched_runner(pair):
        return 0, SELF_TEST_WEDGE_SCRIPT_OUT_MATCHED, ""

    problems, status = check_pair("integration-hub", SELF_TEST_WEDGE_README_MATCHED, wedge_matched_runner)
    if problems or status != "ok":
        failures.append(f"  FALSE POSITIVE on the wedge's matched fixture: {problems}")

    wedge_stale_cleat = SELF_TEST_WEDGE_README_MATCHED.replace(
        "| **app total** (tenant + host + unit tests + behaviour assertions) | **215** | **200** |",
        "| **app total** (tenant + host + unit tests + behaviour assertions) | **209** | **200** |")
    problems, status = check_pair("integration-hub", wedge_stale_cleat, wedge_matched_runner)
    if status != "mismatch" or not any("209" in p and "215" in p for p in problems):
        failures.append(f"  MISSED: a cleat-side drift in the wedge's app total was not reported: {problems}")

    wedge_stale_dbos = SELF_TEST_WEDGE_README_MATCHED.replace(
        "| **app total** (tenant + host + unit tests + behaviour assertions) | **215** | **200** |",
        "| **app total** (tenant + host + unit tests + behaviour assertions) | **215** | **194** |")
    problems, status = check_pair("integration-hub", wedge_stale_dbos, wedge_matched_runner)
    if status != "mismatch" or not any("194" in p and "200" in p for p in problems):
        failures.append(f"  MISSED: a DBOS-isolated-side drift in the wedge's app total was not reported: {problems}")

    # Known negative -- cleat-review on cleat#2749: two role rows drift by
    # equal and opposite amounts, so their SUM (the app-total row) still
    # matches and the check above alone would pass. tenant code 55 -> 60,
    # host runner 25 -> 20; app total stays 215.
    wedge_compensating = SELF_TEST_WEDGE_README_MATCHED.replace(
        "| tenant code | **55** | **24** |", "| tenant code | **60** | **24** |").replace(
        "| host runner | **25** | **54** |", "| host runner | **20** | **54** |")
    problems, status = check_pair("integration-hub", wedge_compensating, wedge_matched_runner)
    if status != "mismatch" or not any("tenant code" in p and "60" in p and "55" in p for p in problems) \
            or not any("host runner" in p and "20" in p and "25" in p for p in problems):
        failures.append(f"  MISSED: compensating errors on two role rows (app total unaffected) "
                        f"were not reported: {problems}")

    # cleat#2642 -- the row THIS PR adds must be guarded exactly as the
    # three above are, or adding it would have widened the table without
    # widening the check that reads it. Compensating error between the
    # DBOS-isolated unit-tests and behaviour-assertions rows: 72 -> 67 and
    # 50 -> 55, so the app total (200) is unchanged and only the per-role
    # check can catch it. Verified to be a real negative by reverting
    # WEDGE_ROLE_ROW_RE/INTEGRATION_HUB_ROLE_NAMES to their pre-#2642
    # values: this case is then NOT reported (it is exactly the drift an
    # unguarded new row would hide).
    wedge_compensating_new_row = SELF_TEST_WEDGE_README_MATCHED.replace(
        "| unit tests | **50** | **72** |", "| unit tests | **50** | **67** |").replace(
        "| behaviour assertions | **85** | **50** |", "| behaviour assertions | **85** | **55** |")
    problems, status = check_pair("integration-hub", wedge_compensating_new_row, wedge_matched_runner)
    if status != "mismatch" \
            or not any("unit tests" in p and "67" in p and "72" in p for p in problems) \
            or not any("behaviour assertions" in p and "55" in p and "50" in p for p in problems):
        failures.append(f"  MISSED: compensating errors on the NEW behaviour-assertions row "
                        f"(app total unaffected) were not reported: {problems}")

    # Known negative -- cleat-review's nit on cleat#2749: a second row
    # matching a role name elsewhere in the README must not silently win as
    # "the" value for that role.
    wedge_duplicate_role = SELF_TEST_WEDGE_README_MATCHED + "\n| tenant code | **999** | **999** |\n"
    problems, status = check_pair("integration-hub", wedge_duplicate_role, wedge_matched_runner)
    if status != "unmeasured":
        failures.append(f"  MISSED: a duplicated wedge role row was not reported as unmeasured "
                        f"(got status={status!r}): {problems}")

    # Known negative -- a role section renamed or removed (simulating
    # dbos-pair-loc.sh relabelling one of the three role groups on either
    # side) must be unmeasured, never a silently short total.
    wedge_missing_role = SELF_TEST_WEDGE_SCRIPT_OUT_MATCHED.replace(
        "== cleat: host runner (extracted from hub.go -- dispatch block + every other TenantStepName-related line) ==",
        "== cleat: dispatcher (renamed) ==")
    problems, status = check_pair("integration-hub", SELF_TEST_WEDGE_README_MATCHED,
                                    lambda pair: (0, wedge_missing_role, ""))
    if status != "unmeasured":
        failures.append(f"  MISSED: a renamed/missing wedge role section was not reported as "
                        f"unmeasured (got status={status!r}): {problems}")

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
