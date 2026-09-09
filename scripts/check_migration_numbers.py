#!/usr/bin/env python3
"""Reject two migrations sharing a version number within a dialect.

migration/runner.go derives a migration's version from its filename prefix
(`strconv.Atoi(parts[0])`) and schema_migrations records applied migrations BY
THAT VERSION. Two files with the same prefix are one version and one row.

On a fresh database both apply, so CI is green and stays green. On a database
that already recorded that version, the other file NEVER APPLIES -- it is
pending forever, and the failures are ordinary missing-column errors.

THE REASON IT SURVIVES IS THAT THE DOCUMENTED REMEDY FOR ITS SYMPTOM WORKS.
A developer sees `column "reclaim_count" does not exist`, applies CLAUDE.md's
"when a schema migration lands, recreate your test databases" -- and the
failures go away, because a fresh database applies both files. The correct
routine fix is EFFECTIVE, so it closes the investigation rather than merely
failing to open one. That is strictly worse than a remedy that is only
plausible: nothing about the outcome invites a second look.

Only the deployment upgrading an existing database sees it, and that is the
one place nobody is running the suite. See cleat#1071.

Three collisions landed simultaneously across three dialects because everyone
picks a number against the develop they can see and nothing rejected a
duplicate.

BASELINE: the three known collisions are listed below and must SHRINK. They are
recorded, not blessed -- renumbering them touches migrations owned by other
sessions and is tracked in #1071. A NEW collision fails immediately, and an
entry that stops colliding also fails, so the list cannot rot in either
direction.

Usage:
    scripts/check_migration_numbers.py [migrations-dir]

The optional argument exists so the guard can be run against a doctored tree --
a known-positive. Passing the real tree proves only that it agrees with the
baseline; it must also be shown to REPORT a collision the baseline does not
name.
"""

import re
import subprocess
import sys
from collections import defaultdict
from pathlib import Path

# dialect -> {version: [filenames]}, every entry tracked by cleat#1071.
KNOWN_COLLISIONS = {
    "postgres": {"051"},
    "mysql": {"050"},
    "mssql": {"054"},
}


def repo_root() -> Path:
    out = subprocess.run(["git", "rev-parse", "--show-toplevel"],
                         capture_output=True, text=True, check=True)
    return Path(out.stdout.strip())


def tracked_sql(migrations: Path) -> list[str]:
    """Migration paths, from git rather than from a filesystem walk.

    A walk descends into .claude/worktrees/ and any other scratch checkout,
    which hold whole second copies of migrations/ -- so a file belonging to
    another session's worktree gets attributed to this repo, and the error gets
    MORE likely as the working tree gets messier. CLAUDE.md records that as the
    third of three bugs in one guard, and the only one that was a scope mistake
    rather than a parsing mistake.

    The directory argument is resolved through `git -C`, so a doctored tree
    passed for a known-positive must be a git repo. That is deliberate: it
    keeps the tested path and the production path identical, rather than
    exercising a walk in the test and git in CI.
    """
    out = subprocess.run(
        ["git", "-C", str(migrations), "ls-files", "*/*.sql"],
        capture_output=True, text=True,
    )
    if out.returncode != 0:
        print(f"FAIL: `git ls-files` failed in {migrations}: {out.stderr.strip()}",
              file=sys.stderr)
        sys.exit(2)
    return [l for l in out.stdout.split("\n") if l.endswith(".sql")]


def collisions(migrations: Path) -> dict[str, dict[str, list[str]]]:
    found: dict[str, dict[str, list[str]]] = {}
    by_dialect: dict[str, list[str]] = defaultdict(list)
    for rel in tracked_sql(migrations):
        parts = rel.split("/")
        if len(parts) < 2:
            continue
        by_dialect[parts[-2]].append(parts[-1])
    for dialect, names in by_dialect.items():
        by_version: dict[str, list[str]] = defaultdict(list)
        for name in sorted(names):
            m = re.match(r"^(\d+)_", name)
            if not m:
                continue
            by_version[m.group(1)].append(name)
        dup = {v: n for v, n in by_version.items() if len(n) > 1}
        if dup:
            found[dialect] = dup
    return found


def main() -> int:
    root = repo_root()
    migrations = Path(sys.argv[1]) if len(sys.argv) > 1 else root / "migrations"

    tracked = tracked_sql(migrations)
    if not tracked:
        print(f"FAIL: `git ls-files` returned no migrations under {migrations}. "
              f"The scan is broken and would report no collisions however many "
              f"there are.", file=sys.stderr)
        return 2

    found = collisions(migrations)
    dialects = {rel.split("/")[-2] for rel in tracked if "/" in rel}
    print(f"migrations: {len(tracked)} tracked files across {len(dialects)} dialect(s)")

    rc = 0
    for dialect, dup in sorted(found.items()):
        known = KNOWN_COLLISIONS.get(dialect, set())
        for version, names in sorted(dup.items()):
            if version in known:
                continue
            rc = 1
            print(f"\nFAIL: {dialect} has {len(names)} migrations at version {version}:")
            for n in names:
                print(f"    {n}")

    # The other direction: an entry that no longer collides has been fixed, and
    # leaving it listed means the next real collision at that version is
    # silently exempt.
    for dialect, known in sorted(KNOWN_COLLISIONS.items()):
        actual = set(found.get(dialect, {}))
        for version in sorted(known - actual):
            rc = 1
            print(f"\nFAIL: KNOWN_COLLISIONS lists {dialect}/{version}, which no "
                  f"longer collides. Remove the entry -- a stale exemption would "
                  f"let the next collision at that version through unreported.")

    if rc == 0:
        n = sum(len(v) for v in KNOWN_COLLISIONS.values())
        print(f"no new collisions ({n} known, tracked in cleat#1071)")
    else:
        print("\nmigration/runner.go keys schema_migrations by the filename's "
              "numeric prefix, so two files at one version are one row: a "
              "database that recorded it never receives the other file. "
              "Renumber the later file.")
    return rc


if __name__ == "__main__":
    sys.exit(main())
