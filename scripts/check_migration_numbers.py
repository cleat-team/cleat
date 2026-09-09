#!/usr/bin/env python3
"""Reject two migrations sharing a version number within a dialect.

migration/runner.go derives a migration's version from its filename prefix
(`strconv.Atoi(parts[0])`) and schema_migrations records applied migrations BY
THAT VERSION. Two files with the same prefix are one version and one row.

On a fresh database both apply, so CI is green and stays green. On a database
that already recorded that version, the other file NEVER APPLIES -- it is
pending forever, and the failures look like ordinary missing-column errors,
which is indistinguishable from the stale-test-database case CLAUDE.md tells
you to expect after a migration lands. See cleat#1071.

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


def collisions(migrations: Path) -> dict[str, dict[str, list[str]]]:
    found: dict[str, dict[str, list[str]]] = {}
    for dialect_dir in sorted(p for p in migrations.iterdir() if p.is_dir()):
        by_version: dict[str, list[str]] = defaultdict(list)
        for sql in sorted(dialect_dir.glob("*.sql")):
            m = re.match(r"^(\d+)_", sql.name)
            if not m:
                continue
            by_version[m.group(1)].append(sql.name)
        dup = {v: names for v, names in by_version.items() if len(names) > 1}
        if dup:
            found[dialect_dir.name] = dup
    return found


def main() -> int:
    root = repo_root()
    migrations = Path(sys.argv[1]) if len(sys.argv) > 1 else root / "migrations"

    dialects = [p for p in migrations.iterdir() if p.is_dir()]
    if not dialects:
        print(f"FAIL: no dialect directories under {migrations}. The scan is "
              f"broken and would report no collisions however many there are.",
              file=sys.stderr)
        return 2
    total = sum(len(list(d.glob('*.sql'))) for d in dialects)
    if total == 0:
        print(f"FAIL: no .sql files under {migrations}.", file=sys.stderr)
        return 2

    found = collisions(migrations)
    print(f"migrations: {total} files across {len(dialects)} dialect(s)")

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
