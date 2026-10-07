#!/usr/bin/env python3
"""Guard: the documentation surfaces must not show the BARE `@cleat_entry` form.

cleat#3119, from cleat#3115. The SDK calls the bare `@cleat_entry` legacy and the
parenthesised `@cleat_entry("name")` preferred -- in cleat_sdk/entry.py's module
docstring, in its comment, and in `_resolve_dual_form`, which exists to accept
both. cleat#3115 converted the 21 sites that showed a reader the legacy form, and
NOTHING keeps them converted: `grep -rln "docs/migration" .github/ scripts/`
returns nothing, so those pages had drifted before and will drift again. That is
how python-sdk/README.md ended up 14 examples behind.

WHAT THIS IS NOT, and the reason it is not a one-line rule: the bare form is
SUPPORTED. Some surfaces must keep using it:

    python-sdk/tests/**        exercise the dual form, and a test that only ever
    testdata/vet-checks/**     writes the preferred form STOPS COVERING the
                               branch it exists to keep working

Converting those would delete coverage rather than move a preference, which is
worse than the drift. So the allowlist below is a list of the COVERAGE surfaces,
not a denylist of the documentation ones: they are few and enumerated, and a new
test that uses the bare form deliberately is a legitimate addition that a
denylist would reject.

THE ANCHORING IS LOAD-BEARING. `^[ \\t]*@cleat_entry$` -- not `@cleat_entry$`.
The package and decorator docstrings INDENT their examples, so a pattern without
the leading-whitespace class reports ZERO for them while looking correct, which is
the false-clean this whole class is made of. Measured on the cleat#3115 tree: the
anchored pattern reports 54 in python-sdk/tests/, the loose one 57, the three
extra being comment SUFFIXES such as
`ERROR_ASYNC_FUNC = "PY012"  # async def decorated with @cleat_entry` -- a
sentence about the form, not a use of it.

EXIT STATUS, because "I could not look" must not read as "clean":
    0  no bare form outside the allowlist
    1  a finding -- the sites are named, with the file and line
    2  the check could not establish what it was measuring
"""

from __future__ import annotations

import re
import subprocess
import sys
from pathlib import Path

# The whole line is the bare decorator, allowing indentation. `[ \t]` rather than
# `\s` on purpose: `\s` matches a newline, so with re.MULTILINE a blank line
# followed by the decorator would satisfy the pattern's leading part.
BARE = re.compile(r"^[ \t]*@cleat_entry[ \t]*$", re.MULTILINE)

PREFERRED = re.compile(r"@cleat_entry\s*\(", re.MULTILINE)

# The files the class lives in: the SDK's Python, and the documents that quote it.
EXTENSIONS = (".py", ".md", ".rst")

# ...and of those, the two the guard EXISTS for. A file count cannot check these:
# removing ".md" narrows the scope to the SDK's own source while leaving the
# count barely moved, so the live run asserts instead that each of these actually
# contributed a checked file. See the `scope_gaps` check in main().
REQUIRED_EXTENSIONS = (".py", ".md")

# Paths that use the bare form DELIBERATELY, because exercising a form is the
# point rather than demonstrating it. Keep this an allowlist of coverage
# surfaces -- see the module docstring for why it is not the other way round.
ALLOWED_PREFIXES = (
    "python-sdk/tests/",
    "testdata/vet-checks/python/",
)

# A floor, so a walk that has broken cannot report a clean tree.
#
# IT COUNTS THE FILES THE GUARD ACTUALLY CHECKS, not every file carrying a
# decorator, and that distinction is the whole value of it. The allowlisted
# coverage surfaces carry a decorator too -- they are exempt from being CHECKED,
# not from being counted -- so a floor on the total is satisfied by the exempt
# files alone. Measured 2026-10-04: 12 files carry a decorator inside the
# allowlist and 29 outside it, so removing ".md" from EXTENSIONS -- which stops
# this guard scanning the documentation surfaces, its entire purpose -- still
# left 37 files against a total floor of 10 and exited 0. cleat-review found it
# by trying exactly that; the floor could not see the failure it exists for.
MIN_CHECKED_FILES_WITH_DECORATORS = 20


def bare_lines(text: str) -> list[int]:
    """1-based line numbers of bare `@cleat_entry` decorators in `text`."""
    return [text.count("\n", 0, m.start()) + 1 for m in BARE.finditer(text)]


def is_allowed(path: str) -> bool:
    return path.startswith(ALLOWED_PREFIXES)


def tracked_files(root: Path) -> list[str]:
    """Tracked files with the extensions this guard reads, as repo-relative paths.

    `git ls-files` rather than a filesystem walk: a walk descends into
    `.claude/worktrees/`, which is a second copy of the repository, and would
    attribute a finding to a path that does not exist in the tree being checked.
    """
    out = subprocess.run(
        ["git", "-C", str(root), "ls-files", "-z"],
        capture_output=True,
        timeout=120,
    )
    if out.returncode != 0:
        raise RuntimeError(
            f"git ls-files failed (rc={out.returncode}): {out.stderr.decode()[:400]}"
        )
    names = [n for n in out.stdout.decode().split("\0") if n]
    return [n for n in names if n.endswith(EXTENSIONS)]


def scan(root: Path) -> tuple[list[str], int, int, dict[str, int]]:
    """Return (findings, checked_files, allowed_files, checked_by_extension).

    A finding is `path:line`, for a bare form outside the allowlist.
    `checked_files` is the population the vacuity floor is about -- files
    carrying EITHER form outside the allowlist, i.e. the ones this guard exists
    to look at. `allowed_files` carries a decorator but is exempt, and is
    returned only so the summary can name both rather than conflate them.
    `checked_by_extension` is what the SCOPE check reads, because a count cannot
    tell a narrower scope from a smaller tree.
    """
    findings: list[str] = []
    checked = 0
    allowed = 0
    by_ext: dict[str, int] = {}
    for rel in tracked_files(root):
        p = root / rel
        try:
            text = p.read_text(encoding="utf-8", errors="replace")
        except OSError as e:
            raise RuntimeError(f"could not read {rel}: {e}") from e
        if not (BARE.search(text) or PREFERRED.search(text)):
            continue
        if is_allowed(rel):
            allowed += 1
            continue
        checked += 1
        ext = Path(rel).suffix
        by_ext[ext] = by_ext.get(ext, 0) + 1
        findings.extend(f"{rel}:{ln}" for ln in bare_lines(text))
    return findings, checked, allowed, by_ext


def self_test() -> int:
    """Both failure modes this class actually has, asserted rather than assumed."""
    failures: list[str] = []

    # 1. THE FALSE-CLEAN. An INDENTED bare decorator must be found -- this is the
    #    case a pattern without the leading-whitespace class reports as zero, and
    #    the docstrings that carry it are the most-copied examples in the SDK.
    indented = 'def f():\n    @cleat_entry\n    def place_order(h): ...\n'
    if bare_lines(indented) != [2]:
        failures.append(
            "the anchored pattern missed an INDENTED bare decorator "
            f"(got {bare_lines(indented)}, want [2]) -- it cannot see the examples "
            "the package and decorator docstrings indent, which is the false-clean "
            "this guard exists to prevent"
        )

    # 2. THE ALLOWLIST. A bare form inside a coverage surface must NOT be
    #    reported, or the guard pushes the next author into deleting coverage.
    if not is_allowed("python-sdk/tests/test_entry.py"):
        failures.append("python-sdk/tests/ is not allowlisted")
    if not is_allowed("testdata/vet-checks/python/x.py"):
        failures.append("testdata/vet-checks/python/ is not allowlisted")
    if is_allowed("python-sdk/examples/hello_workflow.py"):
        failures.append(
            "python-sdk/examples/ is allowlisted -- it is a DOCUMENTATION surface "
            "and the sharpest one, since README.md introduces those files as "
            "ready-to-run"
        )

    # 3. A COMMENT SUFFIX IS NOT A USE. The trailing-comment form must not be
    #    reported, because it describes the decorator rather than demonstrating
    #    it -- see cleat_sdk/vet.py:42.
    suffix = 'ERROR_ASYNC_FUNC = "PY012"  # async def decorated with @cleat_entry\n'
    if bare_lines(suffix) != []:
        failures.append(
            "a trailing-comment mention was reported as a bare decorator -- the "
            "pattern is not anchored, or is matching mid-line"
        )

    for f in failures:
        print(f"self-test FAILED: {f}", file=sys.stderr)
    if failures:
        return 1
    print("self-test ok: anchored on indentation, allowlist honoured, comments ignored")
    return 0


def main() -> int:
    argv = sys.argv[1:]
    if argv == ["--self-test"]:
        return self_test()
    if argv:
        print(f"usage: {sys.argv[0]} [--self-test]", file=sys.stderr)
        return 2

    root = Path(__file__).resolve().parent.parent
    try:
        findings, checked, allowed, by_ext = scan(root)
    except RuntimeError as e:
        print(f"UNMEASURED: {e}", file=sys.stderr)
        print(
            "This is a failure of the CHECK, not a finding about the tree.",
            file=sys.stderr,
        )
        return 2

    # SCOPE, ASSERTED DIRECTLY, because the floor below cannot do it. A count
    # cannot tell a narrower scope from a smaller tree: removing ".md" from
    # EXTENSIONS stops this guard scanning the documentation surfaces -- its
    # entire purpose -- while moving the checked population by only 4 (29 -> 25),
    # which clears any floor loose enough to survive ordinary churn. cleat-review
    # found exactly that: the guard still exited 0. So each extension this guard
    # EXISTS for must have contributed a checked file.
    gaps = [e for e in REQUIRED_EXTENSIONS if by_ext.get(e, 0) == 0]
    if gaps:
        print(
            f"UNMEASURED: no CHECKED file with {', '.join(gaps)} carries a "
            f"@cleat_entry decorator, so this guard is not looking at a surface it "
            f"exists for (checked: "
            f"{', '.join(f'{k}={v}' for k, v in sorted(by_ext.items())) or 'none'}). "
            "A file count cannot see this; the scope is asserted instead.",
            file=sys.stderr,
        )
        return 2

    if checked < MIN_CHECKED_FILES_WITH_DECORATORS:
        print(
            f"UNMEASURED: only {checked} file(s) OUTSIDE the allowlist carry a "
            f"@cleat_entry decorator, want at least "
            f"{MIN_CHECKED_FILES_WITH_DECORATORS} ({allowed} more are exempt). The "
            "walk, the extension list or the repository layout has changed, so a "
            "clean result below would not mean anything. The floor counts the "
            "CHECKED population on purpose: a floor on every decorated file is "
            "satisfied by the exempt ones alone, which is how removing a file "
            "extension from this script used to leave it reporting clean.",
            file=sys.stderr,
        )
        return 2

    if findings:
        print(
            f"{len(findings)} bare `@cleat_entry` decorator(s) outside the "
            "allowlist. The SDK calls this form LEGACY; the parenthesised form is "
            "preferred, and cleat#3115 converted the sites a reader copies:",
            file=sys.stderr,
        )
        for f in findings:
            print(f"  {f}", file=sys.stderr)
        print(
            "\nWrite `@cleat_entry(\"<the function's own name>\")` -- the string the "
            "bare form resolves to, per _resolve_dual_form in cleat_sdk/entry.py. "
            "If the site is one that EXERCISES the dual form deliberately, add its "
            "directory to ALLOWED_PREFIXES in this script instead, and say why.",
            file=sys.stderr,
        )
        return 1

    print(
        f"ok: no bare @cleat_entry outside the allowlist "
        f"({checked} file(s) checked, {allowed} exempt)"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
