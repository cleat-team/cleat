#!/usr/bin/env python3
"""Report a tracked package-lock.json whose recorded copy of a `file:`-linked
local npm package has drifted from that package's own package.json.

cleat#2455 / cleat#2506: `examples/widget-store-as/package-lock.json` pinned
`@cleat/sdk@0.1.0` with `peerDependencies.assemblyscript ^0.27.0` while
`packages/cleat-as/package.json` had moved to `0.3.2` / `^0.28.19`, and
`npm ci` refused. Nothing in CI ran `npm ci` there, so nothing noticed.

Two guards existed before this one and neither reaches the case above:
`.github/workflows/plugin-harness-ci.yml`'s "Tests did not rewrite their own
fixtures" step diffs `tests/plugin-harness/testdata` for files a TEST RUN
changed -- it only sees drift a job's own steps introduce, in a job that
never touches `examples/` or `packages/cleat-as` in the first place. And
nothing else reads a lockfile's `file:` entries at all.

This script needs no npm and no network: it parses package.json/
package-lock.json directly and compares the two copies. The mapping from
consumer to source is DERIVED from every tracked package.json's own `file:`
dependencies, per cleat#2455's rule -- "any tracked lock that pins a local
package" -- rather than a hardcoded list of directories, so a new example or
package acquires the check by using the same `file:` convention everything
else here already uses.

    scripts/check-local-package-mirrors.py              # check the tree
    scripts/check-local-package-mirrors.py --self-test   # known-positive/negative

Exit codes: 0 clean, 1 a lock has drifted, 2 could not establish what to
check (a package.json or lock does not parse -- a defect in the check
itself, not a finding about the tree; see CLAUDE.md's "give 'I could not
look' its own exit status").
"""

import json
import pathlib
import subprocess
import sys
import tempfile

REPO_ROOT = pathlib.Path(__file__).resolve().parent.parent

# Fields compared between a source package.json and a consumer lock's copy of
# it. Both are things that have drifted here before: the version (cleat#2506's
# stale 0.1.0) and the peer range (cleat#2506: "the peerDependencies part is
# the half a version-mirror check would miss, and it is the half that has
# drifted furthest here").
COMPARED_FIELDS = ("version", "peerDependencies")


def tracked_files(pattern):
    out = subprocess.run(
        ["git", "ls-files", "--full-name", pattern],
        cwd=REPO_ROOT, capture_output=True, text=True, check=True,
    )
    return [REPO_ROOT / line for line in out.stdout.splitlines() if line]


def load_json(path):
    with open(path, encoding="utf-8") as f:
        return json.load(f)


def find_local_dependencies(package_json_path):
    """Yield (dep_name, source_package_json_path) for each file: dependency."""
    try:
        data = load_json(package_json_path)
    except (OSError, json.JSONDecodeError) as exc:
        raise CheckError(f"{package_json_path}: {exc}") from exc

    deps = {}
    deps.update(data.get("dependencies", {}) or {})
    deps.update(data.get("devDependencies", {}) or {})
    for name, spec in deps.items():
        if not isinstance(spec, str) or not spec.startswith("file:"):
            continue
        rel = spec[len("file:"):]
        source = (package_json_path.parent / rel / "package.json").resolve()
        yield name, source


def lockfile_entry(lock_data, dep_name):
    """Find dep_name's own recorded copy inside a v2/v3 lockfile's "packages" map.

    Two shapes exist in this repo's tracked locks for a `file:`-linked
    package, and this has to read both. The newer, workspace-`link: true`
    shape carries the package at its own relative-path key (with an explicit
    "name" field) and a separate `"node_modules/<name>": {"link": true}`
    pointer with no version of its own. The older shape -- what
    `examples/widget-store-as/package-lock.json` carried before cleat#2457 --
    has only `"node_modules/<name>"`, with the real "version" and
    "peerDependencies" right there and no "name" field at all (npm infers the
    name from the key in that shape). So identify the entry by name where one
    is given, and by the key's own `node_modules/<name>` suffix otherwise --
    never by the key alone, which is a filesystem path in the newer shape.
    """
    for key, entry in (lock_data.get("packages") or {}).items():
        if not isinstance(entry, dict) or "version" not in entry:
            continue
        name = entry.get("name")
        if name is None and key.startswith("node_modules/"):
            name = key[len("node_modules/"):]
        if name == dep_name:
            return entry
    return None


class CheckError(Exception):
    """The check could not establish what it was measuring."""


def check_repo():
    """Return (findings, unmeasured). Each is a list of strings."""
    findings = []
    unmeasured = []

    source_cache = {}

    for consumer in tracked_files("*/package.json") + tracked_files("package.json"):
        lock = consumer.parent / "package-lock.json"
        if not lock.exists():
            continue
        try:
            local_deps = list(find_local_dependencies(consumer))
        except CheckError as exc:
            unmeasured.append(str(exc))
            continue
        if not local_deps:
            continue

        try:
            lock_data = load_json(lock)
        except (OSError, json.JSONDecodeError) as exc:
            unmeasured.append(f"{lock}: {exc}")
            continue

        for dep_name, source_path in local_deps:
            if not source_path.exists():
                unmeasured.append(
                    f"{consumer.relative_to(REPO_ROOT)}: {dep_name} points at "
                    f"{source_path}, which does not exist"
                )
                continue

            if source_path not in source_cache:
                try:
                    source_cache[source_path] = load_json(source_path)
                except (OSError, json.JSONDecodeError) as exc:
                    unmeasured.append(f"{source_path}: {exc}")
                    source_cache[source_path] = None
            source_data = source_cache[source_path]
            if source_data is None:
                continue

            entry = lockfile_entry(lock_data, dep_name)
            if entry is None:
                findings.append(
                    f"{lock.relative_to(REPO_ROOT)}: no entry for {dep_name} "
                    f"carrying a version -- npm ci will refuse here"
                )
                continue

            for field in COMPARED_FIELDS:
                want = source_data.get(field)
                got = entry.get(field)
                if field == "peerDependencies":
                    # Only compare the peer this dependency itself declares a
                    # range for; an unrelated peer in one or the other is not
                    # this check's business.
                    want = {k: v for k, v in (want or {}).items()}
                    got = {k: v for k, v in (got or {}).items()}
                    common = set(want) & set(got)
                    want = {k: want[k] for k in common}
                    got = {k: got[k] for k in common}
                if want and want != got:
                    findings.append(
                        f"{lock.relative_to(REPO_ROOT)}: {dep_name}.{field} is "
                        f"{got!r}, but {source_path.relative_to(REPO_ROOT)} says {want!r}"
                    )

    return findings, unmeasured


def run_self_test():
    """A known-positive (a manufactured mismatch) and a known-negative
    (the current tree, which must be clean today) -- CLAUDE.md's rule that a
    probe written because a bug is suspected needs a known-positive, not just
    a clean run on the case it was aimed at.
    """
    ok = True

    findings, unmeasured = check_repo()
    if unmeasured:
        print("KNOWN-NEGATIVE FAILED: check_repo() could not measure the real tree:")
        for u in unmeasured:
            print(f"  {u}")
        ok = False
    elif findings:
        print("KNOWN-NEGATIVE FAILED: the real tree should be clean right now and is not:")
        for f in findings:
            print(f"  {f}")
        ok = False
    else:
        print("KNOWN-NEGATIVE passed: the real tree reports clean.")

    with tempfile.TemporaryDirectory() as tmp:
        tmp = pathlib.Path(tmp).resolve()
        (tmp / "source").mkdir()
        (tmp / "source" / "package.json").write_text(json.dumps({
            "name": "@acme/sdk", "version": "2.0.0",
            "peerDependencies": {"widget": "^9.0.0"},
        }))
        (tmp / "consumer").mkdir()
        (tmp / "consumer" / "package.json").write_text(json.dumps({
            "name": "consumer-app",
            "devDependencies": {"@acme/sdk": "file:../source"},
        }))
        # New (workspace link:true) shape -- the entry carries an explicit "name".
        (tmp / "consumer" / "package-lock.json").write_text(json.dumps({
            "packages": {
                "../source": {
                    "name": "@acme/sdk", "version": "1.0.0",
                    "peerDependencies": {"widget": "^8.0.0"},
                },
                "node_modules/@acme/sdk": {"resolved": "../source", "link": True},
            }
        }))

        # Old shape -- exactly what examples/widget-store-as/package-lock.json
        # carried before cleat#2457/#2506: only "node_modules/<name>", no
        # "name" field, name inferred from the key.
        (tmp / "consumer-old-shape").mkdir()
        (tmp / "consumer-old-shape" / "package.json").write_text(json.dumps({
            "name": "consumer-old-shape-app",
            "devDependencies": {"@acme/sdk": "file:../source"},
        }))
        (tmp / "consumer-old-shape" / "package-lock.json").write_text(json.dumps({
            "packages": {
                "node_modules/@acme/sdk": {
                    "version": "1.0.0", "resolved": "file:../source",
                    "peerDependencies": {"widget": "^8.0.0"},
                },
            }
        }))

        global REPO_ROOT, tracked_files
        real_repo_root, real_tracked = REPO_ROOT, tracked_files
        REPO_ROOT = tmp
        tracked_files = lambda pattern: list(tmp.glob(pattern))  # noqa: E731
        try:
            findings, unmeasured = check_repo()
        finally:
            REPO_ROOT, tracked_files = real_repo_root, real_tracked

        if unmeasured:
            print("KNOWN-POSITIVE FAILED: the manufactured mismatches were UNMEASURED:")
            for u in unmeasured:
                print(f"  {u}")
            ok = False
        elif len(findings) != 4:
            print(f"KNOWN-POSITIVE FAILED: expected 4 findings (version+peerDeps, "
                  f"new shape and old shape), got {len(findings)}:")
            for f in findings:
                print(f"  {f}")
            ok = False
        else:
            print("KNOWN-POSITIVE passed: caught all 4 manufactured mismatches "
                  "(both shapes, version and peerDependencies):")
            for f in findings:
                print(f"  {f}")

    return ok


def main():
    if "--self-test" in sys.argv[1:]:
        sys.exit(0 if run_self_test() else 1)

    findings, unmeasured = check_repo()

    for u in unmeasured:
        print(f"UNMEASURED: {u}", file=sys.stderr)

    for f in findings:
        print(f"DRIFT: {f}")

    if findings:
        print(f"\n{len(findings)} local package lock(s) have drifted from the "
              f"package.json they mirror. Regenerate with "
              f"`npm install --package-lock-only --ignore-scripts` in the "
              f"listed directory.")
        sys.exit(1)
    if unmeasured:
        sys.exit(2)
    print("OK: every tracked lock's file: entries match their source package.json.")
    sys.exit(0)


if __name__ == "__main__":
    main()
