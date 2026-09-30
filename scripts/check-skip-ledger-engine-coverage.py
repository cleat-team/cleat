#!/usr/bin/env python3
"""Guard: a cluster-job skip-ledger line for an engine/ test must have a
matching test-go/engine line somewhere in the ledger, or an explicit
exemption.

cleat#2158. Since #2089 moved engine-race.yml (the only job that checks
skips under the test-go/engine key) off the per-PR trigger set, a missing
test-go/engine line for a dialect-gated engine/ test is invisible until that
job's next 4-hourly run. #2129, #2131 and #2137 each shipped a `cluster`
line for an engine/ test with no matching `test-go/engine` line -- a
previous coordinator session had (wrongly) told streams that a `cluster`
line alone was the post-#2089 convention. The first scheduled engine-race
run after those three merged went red on its skip budget (cleat#2155),
fixed in #2157 by adding the three missing lines.

WHAT THIS CHECKS. Every `cluster`-keyed line in scripts/skip-ledger.tsv or
scripts/skip-ledger.d/*.tsv whose test-name regex names a test declared
under engine/ must have a `test-go/engine`-keyed line SOMEWHERE in the
ledger (any file, not necessarily the same one) with the byte-identical
regex, or a matching entry in scripts/skip-ledger.d/.engine-coverage-exempt.tsv.

NOT SAME FILE. skip-ledger.d/README.md says a dialect-gated engine test
"usually" needs both lines "in the one file" -- but five live pairs on
develop today live in two files each (one file per job key, sharing a
`-test-go-engine` suffix convention), and a same-file rule would report
all five as false positives on a healthy tree. Verified by scanning every
`cluster` regex present today: 0 have no test-go/engine pair anywhere, and
5 of the 170 have their pair in a DIFFERENT file. This check tests file
BY REGEX MEMBERSHIP across the whole ledger, matching what is actually
there rather than the README's simplified prose.

WHY A REGEX, NOT A NAME. This check does not try to determine whether a
regex "means" the same test as another; it treats two lines as paired only
when their regex TEXT is byte-identical, which is the convention every
existing pair in the ledger already follows (verified the same way).

REGEX SHAPE EXTRACTION. A ledger regex targeting one or more test
functions has one of two shapes, both anchor-stripped (`^`...`$` treated
as literal characters in the field, not as this script's own anchors --
they are the LEDGER's regex syntax, not this parser's):

    <prefix>(<alt1>|<alt2>|...)[/<suffix>]   -- prefix may be empty
    <TestName>[/<suffix>]                     -- a single literal name

Candidates are `prefix+alt` for the first shape (prefix "" for a bare
top-level alternation of already-full names) and the bare name for the
second. Verified against every cluster regex in the ledger today: 169 of
170 parse this way; the lone holdout is `__UNATTRIBUTED__`, a sentinel
that is not a regex at all and is skipped rather than flagged, matching
how check-skip-budget.sh already treats that key.

A regex this parser cannot extract candidates from is reported separately
from a genuine missing-pair finding (a third outcome, not folded into
either), because "I could not tell" and "I checked and it needs a pair"
are different findings and conflating them would silently exempt whatever
this parser cannot parse -- the same failure this repo's own instructions
document as "a condition that never decides anything reads as done."

Usage:
  scripts/check-skip-ledger-engine-coverage.py              # fail on any gap
  scripts/check-skip-ledger-engine-coverage.py --self-test  # fixture-driven
  scripts/check-skip-ledger-engine-coverage.py --list       # print findings
"""
import glob
import os
import re
import shutil
import sys
import tempfile

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
LEDGER_GLOB_SUFFIX = "scripts/skip-ledger.d/*.tsv"
LEGACY_LEDGER_SUFFIX = "scripts/skip-ledger.tsv"
EXEMPT_SUFFIX = "scripts/skip-ledger.d/.engine-coverage-exempt.tsv"
ENGINE_DIR = "engine"

SENTINELS = {"__UNATTRIBUTED__"}

TEST_FUNC_RE = re.compile(r"^func\s+(Test[A-Za-z0-9_]+)\s*\(", re.MULTILINE)


def strip_anchors(pattern):
    p = pattern
    if p.startswith("^"):
        p = p[1:]
    if p.endswith("$"):
        p = p[:-1]
    return p


def extract_candidates(pattern):
    """Returns a list of candidate top-level test names, or None if the
    regex's shape is not one this parser recognises."""
    if pattern in SENTINELS:
        return []
    s = strip_anchors(pattern)
    head = s.split("/")[0]
    m = re.match(r"^([A-Za-z0-9_]*)\(([^()]+)\)$", head)
    if m:
        prefix, alts = m.group(1), m.group(2)
        return [prefix + a for a in alts.split("|")]
    m2 = re.match(r"^(Test[A-Za-z0-9_]+)$", head)
    if m2:
        return [m2.group(1)]
    return None


def collect_ledger_lines(root):
    """Yields (file, job, count, regex, why) for every non-comment,
    non-blank line across the legacy ledger and every skip-ledger.d
    fragment. Mirrors check-skip-budget.sh's own reader: every *.tsv in
    skip-ledger.d plus the single legacy file, same format."""
    paths = sorted(glob.glob(os.path.join(root, LEDGER_GLOB_SUFFIX)))
    legacy = os.path.join(root, LEGACY_LEDGER_SUFFIX)
    if os.path.isfile(legacy):
        paths.append(legacy)
    for path in paths:
        with open(path, encoding="utf-8", errors="replace") as f:
            for line in f:
                line = line.rstrip("\n")
                if not line.strip() or line.startswith("#"):
                    continue
                parts = line.split("\t")
                if len(parts) < 4:
                    continue
                job, count, regex, why = parts[0], parts[1], parts[2], parts[3]
                yield path, job, count, regex, why


def engine_test_names(root):
    """The set of top-level Test* function names declared anywhere under
    engine/*_test.go, recursively. A static, source-level scan -- no
    database, no `go test`, matching the issue's "fast" requirement."""
    names = set()
    engine_dir = os.path.join(root, ENGINE_DIR)
    for dirpath, _dirnames, filenames in os.walk(engine_dir):
        for fn in filenames:
            if not fn.endswith("_test.go"):
                continue
            path = os.path.join(dirpath, fn)
            with open(path, encoding="utf-8", errors="replace") as f:
                content = f.read()
            for m in TEST_FUNC_RE.finditer(content):
                names.add(m.group(1))
    return names


def read_exemptions(root):
    """<file-glob-hint ignored><TAB><regex><TAB><why>. Only the regex field
    is load-bearing -- the exemption is keyed on the exact cluster-line
    regex it excuses, wherever that line lives, matching how pairing
    itself is keyed."""
    path = os.path.join(root, EXEMPT_SUFFIX)
    exempt = set()
    if not os.path.isfile(path):
        return exempt
    with open(path, encoding="utf-8", errors="replace") as f:
        for line in f:
            line = line.rstrip("\n")
            if not line.strip() or line.startswith("#"):
                continue
            parts = line.split("\t")
            if len(parts) < 2:
                continue
            exempt.add(parts[1])
    return exempt


def check(root):
    """Returns (missing, unparseable, stale_exemptions) where:
    missing         -- (file, regex) pairs needing a test-go/engine line
    unparseable      -- (file, regex) whose shape this parser cannot read
    stale_exemptions -- regexes exempted that no longer need it
    """
    cluster = []          # (file, regex)
    engine_regexes = set()
    for path, job, _count, regex, _why in collect_ledger_lines(root):
        if job == "cluster":
            cluster.append((path, regex))
        elif job == "test-go/engine":
            engine_regexes.add(regex)

    names = engine_test_names(root)
    exempt = read_exemptions(root)

    missing = []
    unparseable = []
    needed_exemptions = set()

    for path, regex in cluster:
        candidates = extract_candidates(regex)
        if candidates is None:
            unparseable.append((path, regex))
            continue
        if not any(c in names for c in candidates):
            continue  # does not target an engine/ test; no pairing required
        if regex in engine_regexes:
            continue  # paired, somewhere in the ledger
        if regex in exempt:
            needed_exemptions.add(regex)
            continue
        missing.append((path, regex))

    stale_exemptions = sorted(exempt - needed_exemptions)

    return missing, unparseable, stale_exemptions


def _print_report(missing, unparseable, stale_exemptions, root):
    if missing:
        print("ERROR: cluster line(s) for an engine/ test with no test-go/engine pair:", file=sys.stderr)
        print(file=sys.stderr)
        for path, regex in missing:
            print(f"  {os.path.relpath(path, root)}\t{regex}", file=sys.stderr)
        print(file=sys.stderr)
        print("engine-race.yml is the only job that checks the test-go/engine key, and", file=sys.stderr)
        print("it runs 4-hourly, off the per-PR trigger set (#2089) -- a missing line here", file=sys.stderr)
        print("is invisible until that scheduled run, hours after this merges (cleat#2155).", file=sys.stderr)
        print(file=sys.stderr)
        print("Add a test-go/engine line with the IDENTICAL regex, in this file or any", file=sys.stderr)
        print("other skip-ledger.d fragment (see #2157 for the shape), or add an entry to", file=sys.stderr)
        print(f"{EXEMPT_SUFFIX} naming the regex and why no pairing is needed.", file=sys.stderr)

    if unparseable:
        print(file=sys.stderr)
        print("NOTE: cluster line(s) with a regex shape this guard cannot parse", file=sys.stderr)
        print("(not a finding -- this guard could not determine whether they need a", file=sys.stderr)
        print("test-go/engine pair, so check them by hand):", file=sys.stderr)
        for path, regex in unparseable:
            print(f"  {os.path.relpath(path, root)}\t{regex}", file=sys.stderr)

    if stale_exemptions:
        print(file=sys.stderr)
        print("ERROR: stale entries in " + EXEMPT_SUFFIX + " (regex no longer needs one):", file=sys.stderr)
        for regex in stale_exemptions:
            print(f"  {regex}", file=sys.stderr)
        print("Either the pairing test-go/engine line now exists, or the cluster line", file=sys.stderr)
        print("no longer matches an engine/ test. Delete the stale exemption line.", file=sys.stderr)


# --------------------------------------------------------------------------
# self-test
# --------------------------------------------------------------------------

def _self_test():
    ok = True

    # extract_candidates: every shape actually observed in the ledger.
    cases = [
        ("^TestFoo$", ["TestFoo"]),
        ("^TestFoo/(mysql|mssql)$", ["TestFoo"]),
        ("^Test(A|B)/(mysql|mssql)$", ["TestA", "TestB"]),
        ("^TestPlugin(A|B)/mssql$", ["TestPluginA", "TestPluginB"]),
        ("^(TestA|TestB)$", ["TestA", "TestB"]),
        ("__UNATTRIBUTED__", []),
    ]
    for pattern, want in cases:
        got = extract_candidates(pattern)
        if got != want:
            print(f"SELF-TEST FAILED: extract_candidates({pattern!r}) = {got!r}, want {want!r}", file=sys.stderr)
            ok = False

    unparseable_case = extract_candidates("^Test[A-Z]+Weird(Nested(Group))$")
    if unparseable_case is not None:
        print(f"SELF-TEST FAILED: expected None for an unrecognised shape, got {unparseable_case!r}", file=sys.stderr)
        ok = False

    # Fixture repo: an engine/ test, a ledger with a missing pair, a
    # ledger with a satisfied pair, and an exemption.
    tmp = tempfile.mkdtemp()
    try:
        os.makedirs(os.path.join(tmp, "engine"))
        with open(os.path.join(tmp, "engine", "fixture_test.go"), "w") as f:
            f.write(
                "package engine\n\n"
                "func TestNeedsAPair(t *testing.T) {}\n\n"
                "func TestAlreadyPaired(t *testing.T) {}\n\n"
                "func TestExempted(t *testing.T) {}\n"
            )

        os.makedirs(os.path.join(tmp, "scripts", "skip-ledger.d"))
        with open(os.path.join(tmp, "scripts", "skip-ledger.d", "a.tsv"), "w") as f:
            f.write(
                "cluster\t2\t^TestNeedsAPair/(mysql|mssql)$\tfixture, no pair\n"
                "cluster\t2\t^TestAlreadyPaired/(mysql|mssql)$\tfixture, has a pair\n"
                "cluster\t2\t^TestExempted/(mysql|mssql)$\tfixture, exempted\n"
                "cluster\t2\t^TestNotAnEngineTest/(mysql|mssql)$\tfixture, not under engine/\n"
            )
        # The pair for TestAlreadyPaired lives in a DIFFERENT file, on
        # purpose -- this is the case the same-file reading of the README
        # would get wrong (see the module docstring).
        with open(os.path.join(tmp, "scripts", "skip-ledger.d", "b.tsv"), "w") as f:
            f.write("test-go/engine\t2\t^TestAlreadyPaired/(mysql|mssql)$\tfixture pair\n")
        with open(os.path.join(tmp, "scripts", "skip-ledger.d", ".engine-coverage-exempt.tsv"), "w") as f:
            f.write("hint\t^TestExempted/(mysql|mssql)$\tfixture reason\n")

        missing, unparseable, stale = check(tmp)

        missing_regexes = {r for _p, r in missing}
        if missing_regexes != {"^TestNeedsAPair/(mysql|mssql)$"}:
            print(f"SELF-TEST FAILED: missing = {missing_regexes!r}, want exactly TestNeedsAPair's line", file=sys.stderr)
            ok = False
        if unparseable:
            print(f"SELF-TEST FAILED: unexpected unparseable entries: {unparseable!r}", file=sys.stderr)
            ok = False
        if stale:
            print(f"SELF-TEST FAILED: exemption for TestExempted reported stale when it is still needed: {stale!r}", file=sys.stderr)
            ok = False

        # Now make the exemption stale: give TestExempted its pair too,
        # and confirm the exemption is reported as no longer needed.
        with open(os.path.join(tmp, "scripts", "skip-ledger.d", "b.tsv"), "a") as f:
            f.write("test-go/engine\t2\t^TestExempted/(mysql|mssql)$\tfixture pair, added\n")
        missing2, _unparseable2, stale2 = check(tmp)
        if "^TestExempted/(mysql|mssql)$" not in stale2:
            print("SELF-TEST FAILED: exemption for TestExempted not reported stale once it has a real pair", file=sys.stderr)
            ok = False
        if any(r == "^TestExempted/(mysql|mssql)$" for _p, r in missing2):
            print("SELF-TEST FAILED: TestExempted's now-paired line reported missing", file=sys.stderr)
            ok = False
    finally:
        shutil.rmtree(tmp)

    if ok:
        print("self-test passed")
    return 0 if ok else 1


# --------------------------------------------------------------------------

def main():
    args = sys.argv[1:]
    if args == ["--self-test"]:
        sys.exit(_self_test())

    root = REPO_ROOT
    missing, unparseable, stale = check(root)

    if args == ["--list"]:
        for path, regex in missing:
            print(f"MISSING\t{os.path.relpath(path, root)}\t{regex}")
        for path, regex in unparseable:
            print(f"UNPARSEABLE\t{os.path.relpath(path, root)}\t{regex}")
        for regex in stale:
            print(f"STALE-EXEMPTION\t{regex}")
        sys.exit(0)

    if args:
        print(f"usage: {sys.argv[0]} [--self-test|--list]", file=sys.stderr)
        sys.exit(2)

    _print_report(missing, unparseable, stale, root)

    if missing or stale:
        sys.exit(1)

    print(
        f"OK: every cluster-job skip-ledger line for an engine/ test has a "
        f"test-go/engine pair or exemption ({len(unparseable)} line(s) of unrecognised "
        f"shape, not flagged)."
    )


if __name__ == "__main__":
    main()
