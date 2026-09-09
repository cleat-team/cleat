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

BASELINE: empty, and it should stay that way. cleat#1073 renumbered the three
collisions that existed when this guard was written, so nothing is exempt.

An entry here would mean "this collision is known and tolerated", which is
almost never the right answer -- the whole point is that a collision is
invisible until it reaches a deployment. The mechanism is kept because the
alternative is that someone facing a red build deletes the check instead of
recording the exception, and because it fails in BOTH directions: a new
collision, and an entry that has stopped colliding. A one-way allowlist rots
silently.

Note what an empty baseline does to this file's own testability: the
"stopped colliding" direction has no entries to exercise, and on a clean tree
"no new collisions" is the same output a check that does nothing would print.
--self-test is what keeps both directions honest.

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
KNOWN_COLLISIONS: dict[str, set[str]] = {}


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


def stale_entries(found: dict, baseline: dict) -> list[tuple[str, str]]:
    """Baseline entries that no longer describe a real collision.

    Extracted so --self-test can exercise it. With the baseline empty this
    direction is inert on the real tree -- there are no entries to go stale --
    so the fixture is the only thing keeping it honest.
    """
    out = []
    for dialect, known in sorted(baseline.items()):
        actual = set(found.get(dialect, {}))
        for version in sorted(known - actual):
            out.append((dialect, version))
    return out


def self_test() -> int:
    """Prove the guard can still say NO.

    Necessary because of what this check looks like when it passes. With
    KNOWN_COLLISIONS empty -- the intended end state once #1073 lands -- the
    "an entry stopped colliding" direction has no entries to exercise, so the
    only live direction is "a new collision appeared". And on a clean tree,
    `no new collisions` is the same output a check that does nothing would
    print. Passing proves the guard did not false-alarm; it does not prove the
    guard is capable of firing.

    Running this in CI rather than by hand during development is the whole
    point: a known-positive that lives in a PR description is a claim about a
    tree that no longer exists.
    """
    import shutil
    import tempfile

    failures = []

    def build(tmp: Path) -> Path:
        """A minimal two-dialect migrations tree, tracked by git."""
        mig = tmp / "migrations"
        for dialect in ("postgres", "mysql"):
            (mig / dialect).mkdir(parents=True)
            for name in ("001_first.sql", "002_second.sql"):
                (mig / dialect / name).write_text("-- fixture\n")
        subprocess.run(["git", "init", "-q"], cwd=mig, check=True)
        subprocess.run(["git", "add", "-A"], cwd=mig, check=True,
                       capture_output=True)
        return mig

    # 1. A clean fixture must pass. Without this, a guard that reported
    #    everything as a collision would satisfy case 2 and look correct.
    with tempfile.TemporaryDirectory() as d:
        mig = build(Path(d))
        if collisions(mig):
            failures.append("a clean fixture reported collisions")

    # 2. THE ONE THAT MATTERS: a duplicate prefix must be reported.
    with tempfile.TemporaryDirectory() as d:
        mig = build(Path(d))
        shutil.copy(mig / "postgres" / "001_first.sql",
                    mig / "postgres" / "001_duplicate.sql")
        subprocess.run(["git", "add", "-A"], cwd=mig, check=True,
                       capture_output=True)
        found = collisions(mig)
        names = found.get("postgres", {}).get("001", [])
        if sorted(names) != ["001_duplicate.sql", "001_first.sql"]:
            failures.append(f"a duplicate prefix was not reported: {found!r}")

    # 3. An untracked copy must be ignored -- the git-ls-files property. A
    #    filesystem walk would report this as a collision.
    with tempfile.TemporaryDirectory() as d:
        mig = build(Path(d))
        scratch = mig / ".claude" / "worktrees" / "other" / "postgres"
        scratch.mkdir(parents=True)
        shutil.copy(mig / "postgres" / "001_first.sql", scratch / "001_other.sql")
        if collisions(mig):
            failures.append("an untracked worktree copy was counted")

    # 4. A baseline entry that no longer collides must be reported. With
    #    KNOWN_COLLISIONS empty this direction cannot be exercised by the real
    #    tree at all, so without this case it is dead code that nobody would
    #    notice had stopped working.
    with tempfile.TemporaryDirectory() as d:
        mig = build(Path(d))
        found = collisions(mig)                      # clean: no collisions
        stale = stale_entries(found, {"postgres": {"001"}})
        if stale != [("postgres", "001")]:
            failures.append(f"a stale baseline entry was not reported: {stale!r}")
        if stale_entries(found, {}):
            failures.append("an empty baseline reported a stale entry")

    # 5. The control for case 4, and it is not redundant: with a collision-free
    #    fixture, `known - actual` and `known` are the same expression, so case
    #    4 alone passes an implementation that reports EVERY baseline entry as
    #    stale. Measured -- that mutation survived case 4 and is caught here.
    #    An entry that still describes a real collision must NOT be reported.
    with tempfile.TemporaryDirectory() as d:
        mig = build(Path(d))
        shutil.copy(mig / "postgres" / "001_first.sql",
                    mig / "postgres" / "001_duplicate.sql")
        subprocess.run(["git", "add", "-A"], cwd=mig, check=True,
                       capture_output=True)
        found = collisions(mig)                      # 001 IS colliding here
        stale = stale_entries(found, {"postgres": {"001"}})
        if stale:
            failures.append(f"an entry that still collides was reported as "
                            f"stale: {stale!r}")

    for f in failures:
        print(f"SELF-TEST FAIL: {f}", file=sys.stderr)
    if failures:
        return 1
    print("self-test (5 cases): clean tree passes; a duplicate is reported; an "
          "untracked copy is ignored; a stale baseline entry is reported; an "
          "entry that still collides is not")
    return 0


def main() -> int:
    if "--self-test" in sys.argv[1:]:
        return self_test()

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
    for dialect, version in stale_entries(found, KNOWN_COLLISIONS):
        rc = 1
        print(f"\nFAIL: KNOWN_COLLISIONS lists {dialect}/{version}, which no "
              f"longer collides. Remove the entry -- a stale exemption would "
              f"let the next collision at that version through unreported.")

    if rc == 0:
        n = sum(len(v) for v in KNOWN_COLLISIONS.values())
        print("no collisions" if n == 0 else f"no new collisions ({n} exempt)")
    else:
        print("\nmigration/runner.go keys schema_migrations by the filename's "
              "numeric prefix, so two files at one version are one row: a "
              "database that recorded it never receives the other file. "
              "Renumber the later file.")
    return rc


if __name__ == "__main__":
    sys.exit(main())
