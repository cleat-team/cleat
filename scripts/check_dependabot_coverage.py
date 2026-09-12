#!/usr/bin/env python3
"""Assert .github/dependabot.yml covers the manifests that ship, and only those.

Two failure directions, and they are different defects.

A CONFIGURED DIRECTORY WITH NO MANIFEST does nothing. Dependabot skips it
silently -- no error, no PR, no signal anywhere -- which is exactly the shape
that let `renovate.json` sit in this repository configuring a bot that had never
run (cleat#1321: 0 Renovate PRs ever, against 29 from Dependabot). A config that
is not executed is worse than none, because it reads as an answer.

A SHIPPED MANIFEST WITH NO ENTRY drifts out of coverage. Before #1321 every Go
module was in that state: `github-actions` was the only ecosystem with a
schedule, so the main module -- where wasmtime lives, on the sandbox boundary --
got updates only when a security advisory happened to fire.

Neither direction is visible in a green CI run, which is why this is a guard and
not a comment in the config.

Usage:
    scripts/check_dependabot_coverage.py [--self-test]
"""

from __future__ import annotations

import os
import subprocess
import sys

try:
    import yaml
except ImportError:  # pragma: no cover - the runner installs it
    print("ERROR: PyYAML is required (pip install pyyaml)", file=sys.stderr)
    sys.exit(2)

CONFIG = ".github/dependabot.yml"

# The manifest filename that makes a directory real for each ecosystem.
MANIFEST = {
    "gomod": "go.mod",
    "npm": "package.json",
    "cargo": "Cargo.toml",
    "pip": "pyproject.toml",
}

# Path prefixes deliberately left uncovered, with the reason.
#
# These are fixtures and samples: versions are pinned on purpose, a bump is
# churn rather than a release, and nothing here is a shipped artifact. Security
# updates still reach them regardless of dependabot.yml -- #1033 landed in
# tests/cross-language while that directory was in no entry at all -- so
# excluding them costs no advisory coverage.
EXCLUDED_PREFIXES = (
    "examples/",
    "testdata/",
    "tests/",
    "benchmarks/",
    "cmd/cleat/templates/",
)


def tracked_files(pattern: str) -> list[str]:
    """Files git tracks matching pattern.

    git ls-files rather than a filesystem walk: a walk descends into
    .claude/worktrees/, a second copy of the repository, and attributes a
    manifest to a directory that exists only in a scratch checkout. CLAUDE.md
    records that one costing a guard its correctness.
    """
    out = subprocess.run(
        ["git", "ls-files", pattern], capture_output=True, text=True, check=True
    ).stdout
    return [line for line in out.splitlines() if line]


def configured(config_path: str) -> dict[str, set[str]]:
    """ecosystem -> set of repo-relative directories the config names."""
    with open(config_path) as fh:
        doc = yaml.safe_load(fh)
    if not doc or doc.get("version") != 2:
        raise SystemExit(f"ERROR: {config_path} is not a version 2 config")
    out: dict[str, set[str]] = {}
    for entry in doc.get("updates", []):
        eco = entry.get("package-ecosystem")
        dirs = entry.get("directories") or ([entry["directory"]] if entry.get("directory") else [])
        out.setdefault(eco, set()).update(d.lstrip("/") for d in dirs)
    return out


def check(config_path: str, excluded: tuple[str, ...]) -> list[str]:
    """Return a list of problems. Empty means the config and the tree agree."""
    problems: list[str] = []
    cfg = configured(config_path)

    for eco, manifest in MANIFEST.items():
        present = {os.path.dirname(p) for p in tracked_files(f"*{manifest}")}
        # git ls-files '*go.mod' also matches a root go.mod, whose dirname is "".
        present = {d for d in present if not any(
            (d + "/").startswith(x) for x in excluded)}
        named = cfg.get(eco, set())

        for d in sorted(named):
            target = os.path.join(d, manifest) if d else manifest
            if not os.path.exists(target):
                problems.append(
                    f"{config_path} names {eco} directory '/{d}', which has no {manifest}.\n"
                    f"  Dependabot skips an entry whose directory does not exist, silently. "
                    f"The entry does nothing and nothing says so."
                )

        for d in sorted(present - named):
            shown = "/" + d if d else "/"
            problems.append(
                f"{shown} has a {manifest} and is in no {eco} entry in {config_path}.\n"
                f"  It gets security updates only -- no scheduled ones. Add it, or add its "
                f"prefix to EXCLUDED_PREFIXES with the reason."
            )
    return problems


def self_test() -> int:
    """Prove the guard reports both directions, on doctored fixtures.

    A guard that passes a clean tree has demonstrated nothing: every broken
    version of it does that too. These two cases are known-positives -- trees
    that ARE wrong, where the guard must say so.
    """
    import tempfile
    import textwrap

    failures = 0

    def run_case(name: str, config: str, want_substring: str) -> None:
        nonlocal failures
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "dependabot.yml")
            with open(path, "w") as fh:
                fh.write(textwrap.dedent(config))
            # Exclude everything real so only the doctored entry is judged.
            problems = check(path, excluded=("",))
            if not any(want_substring in p for p in problems):
                failures += 1
                print(f"  FAIL {name}: expected a problem containing {want_substring!r}, "
                      f"got {problems!r}")
            else:
                print(f"  ok   {name}")

    print("self-test (2 known-positives):")
    run_case(
        "a configured directory with no manifest is reported",
        """\
        version: 2
        updates:
          - package-ecosystem: "gomod"
            directory: "/no/such/place"
            schedule:
              interval: "weekly"
        """,
        "which has no go.mod",
    )

    # The reverse direction, with nothing excluded, so the real tree's manifests
    # count as uncovered against an empty config.
    with open(os.devnull, "w"):
        pass
    problems = check(CONFIG, excluded=())
    if not any("is in no" in p for p in problems):
        failures += 1
        print("  FAIL an uncovered manifest is reported: with no exclusions the real "
              "tree has uncovered fixture manifests and the guard found none")
    else:
        print("  ok   an uncovered manifest is reported")

    if failures:
        print(f"self-test: {failures} case(s) failed", file=sys.stderr)
        return 1
    print("self-test: the guard reports both directions")
    return 0


def main() -> int:
    if "--self-test" in sys.argv[1:]:
        return self_test()
    problems = check(CONFIG, EXCLUDED_PREFIXES)
    if problems:
        print(f"ERROR: {CONFIG} does not match the tree:\n", file=sys.stderr)
        for p in problems:
            print("  " + p + "\n", file=sys.stderr)
        return 1
    cfg = configured(CONFIG)
    covered = sum(len(v) for v in cfg.values())
    print(f"OK: {CONFIG} covers {covered} director(ies) across "
          f"{len(cfg)} ecosystem(s), and every shipped manifest is in one.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
