#!/usr/bin/env python3
"""Fail when a test in tests/plugin-harness is selected by no CI -run pattern.

Every job that runs this module selects tests with `-run`. A test that no
pattern matches is not run -- and nothing reports that, because the job is
green and the package looks covered. It has happened three times:

  TestHostCallTableCoversEveryWave1Call   'TestHostCalls' stops matching one
                                          character in: 's' against 'T'
  TestEverySDKImportIsAHostExport         the term was the sibling's name,
                                          diverging at 'I' against 'R'
  five at once (#1066)                    including two shipped as a PR's
                                          own compiled-binary evidence

All three were found by hand. scripts/check-ci-package-coverage.sh cannot see
them: it guards PACKAGE-level matrix drift and exempts `tests` as "driven by
their own dedicated CI jobs" -- and the selection is where the coverage
actually lives.

SCOPE, stated because a guard that overstates its reach is worse than none:
this checks tests/plugin-harness ONLY. That module is checkable precisely
because every job targeting it filters with -run. The root module is run by
the test-go matrix a package at a time with no filter, so "selected by a
pattern" is not the right question there; scripts/check-ci-package-coverage.sh
asks the right one instead.

Python rather than shell deliberately. This diffs two sets and has to be
trusted about an EMPTY result, and CLAUDE.md records `grep` here being a ugrep
shell function whose -c/-o semantics differ from the /usr/bin/grep a script
gets and the GNU grep CI gets.

Usage:
    scripts/check_ci_test_selection.py [workflows-dir]

The optional argument exists so the guard can be run against a doctored copy
of the workflows -- a known-positive. Passing a clean tree proves only that it
does not false-alarm; it must also be shown to REPORT a genuinely unselected
test, which is the property the three instances above needed and nothing had.
"""

import re
import subprocess
import sys
from pathlib import Path

MODULE = "tests/plugin-harness"


def repo_root() -> Path:
    out = subprocess.run(
        ["git", "rev-parse", "--show-toplevel"],
        capture_output=True, text=True, check=True,
    )
    return Path(out.stdout.strip())


def run_patterns(workflows_dir: Path) -> list[tuple[str, str]]:
    """Every -run pattern in every workflow file.

    Comment lines are dropped and line-continuations joined BEFORE looking for
    a command: a `-run` inside a comment is not a selection, and a continued
    command read to end-of-line looks unfiltered when it is not. Both
    directions have produced a wrong answer in this repo (#748, #749).
    """
    found = []
    for path in sorted(workflows_dir.glob("*.yml")) + sorted(workflows_dir.glob("*.yaml")):
        src = path.read_text()
        src = "\n".join(l for l in src.split("\n") if not l.strip().startswith("#"))
        src = src.replace("\\\n", " ")
        for m in re.finditer(r"-run\s+'([^']+)'", src):
            found.append((path.name, m.group(1)))
    return found


def go_list(module: Path, pattern: str) -> set[str]:
    """Test names -list reports for a pattern.

    -list, not -run: -run answers what it selected only by implication and is
    silent when the answer is less than you think, which is the exact property
    that let these hide.
    """
    out = subprocess.run(
        ["go", "test", "-list", pattern, "./..."],
        cwd=module, capture_output=True, text=True,
    )
    if out.returncode != 0:
        print(f"go test -list failed in {module}:\n{out.stderr}", file=sys.stderr)
        sys.exit(2)
    return {l for l in out.stdout.split("\n") if l.startswith("Test")}


def main() -> int:
    root = repo_root()
    workflows = Path(sys.argv[1]) if len(sys.argv) > 1 else root / ".github" / "workflows"
    module = root / MODULE

    pats = run_patterns(workflows)
    if not pats:
        print(f"FAIL: no -run patterns found under {workflows}. The parse is "
              f"broken, and this guard would report every test as unselected "
              f"or none at all depending on which way it failed.", file=sys.stderr)
        return 2

    every = go_list(module, ".*")
    if not every:
        print(f"FAIL: `go test -list` found no tests in {MODULE}. Nothing below "
              f"would be measuring anything.", file=sys.stderr)
        return 2

    union = "|".join(p for _, p in pats)
    selected = go_list(module, union)
    unselected = sorted(every - selected)

    print(f"{MODULE}: {len(every)} tests, {len(selected)} selected by "
          f"{len(pats)} -run pattern(s) across {len({f for f, _ in pats})} workflow file(s)")

    if unselected:
        print()
        print(f"FAIL: {len(unselected)} test(s) are selected by no CI -run pattern:")
        for t in unselected:
            print(f"    {t}")
        print()
        print("A test no pattern matches does not run, and nothing says so: the "
              "job is green and the package looks covered. Add a term to the "
              "appropriate job in .github/workflows/plugin-harness-ci.yml, and "
              "confirm with `go test -list '<pattern>' ./...` rather than -run.")
        return 1

    return 0


if __name__ == "__main__":
    sys.exit(main())
