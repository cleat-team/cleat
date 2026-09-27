#!/usr/bin/env python3
"""Every SDK's stamped version must equal the version in its OWN manifest.

Run before a release tag is cut, and by .github/workflows/release-smoke.yml.

WHY THIS EXISTS. Each SDK stamps its own version into the metadata of every
workflow it compiles, and nothing reads those values except a consumer
inspecting a compiled module. cleat#2454 is the gap: `release-process.md`'s
step-3 search is `--include="*.go" --include="*.rs" --include="*.mod"`, which
reaches ONE of the six places a version lives -- the only `.rs` stamper -- and
is structurally blind to the Python, Java and AssemblyScript ones, and to
`pyproject.toml` (TOML). `publish-pypi.yml`'s tag-vs-pyproject guard reads
another one. So the prescribed sequence is: run a grep that cannot see four of
the files, bump the one file the guard reads, tag, and publish a package that
reports the OLD version to every workflow it compiles, with every check green.

WHAT IT ASSERTS, and the word is AGREEMENT rather than freshness: for each SDK,
every site that carries that SDK's version -- its manifest and each of its
stampers -- holds the SAME string. Asserting that each site is individually
plausible is not the invariant; asserting that no two disagree is. A version
bumped in the manifest and missed in the stamper is exactly the defect, and it
is invisible to any check that looks at one site at a time.

MANIFESTS ARE PARSED, SOURCE IS MATCHED. TOML and JSON are read with `tomllib`
and `json`, not with a pattern: a regex is a weak model of both, and the first
draft of this script proved it by reading `"version": "0.1.0"` in package.json
as ZERO matches (the key's closing quote sits between the name and the colon)
and by matching a dependency constraint and a classifier as if they were
versions. The stampers are SOURCE files, where there is nothing to parse, so
they are matched on the field token -- `sdk_version` with an optional closing
quote -- which does not care about nesting or surrounding structure. The same
distinction bit the other way when reading a version back out of a compiled
module: a pattern anchored on balanced braces found nothing, because
`plugin_deps: {}` nests one, so the pattern could only see the shape it assumed.

REQUIRES PYTHON 3.11+ for `tomllib` (stdlib from 3.11). That is the runner's
3.12 and is NOT a claim about the SDK, whose floor is `requires-python =
">=3.10"`. The Python stamper this checks does require 3.10+ itself -- its
signatures use `bytes | None`, which is 3.10 syntax -- and that is consistent
with the package it ships inside, so leave it alone rather than widening the
signature to make it run on an older interpreter.

The Go SDK is deliberately absent: `wasm/build.go` DERIVES its `sdkVersion`
from `sdkRequiredVersion(cfg.ProjectRoot)` rather than hardcoding it, so it has
no literal to keep in step. That is worth leaving a note about, per #2454, so
the next reader does not "fix" it by adding one.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
import tomllib
from pathlib import Path

# Source files only (the stampers): the field token, with an optional closing
# quote so the same pattern reads `sdk_version = "x"`, `"sdk_version": "x"` and
# the Python SDK's uppercase constant `SDK_VERSION = "x"`. Case-insensitive
# because those three spellings are all in use, and a name-keyed pattern that is
# case-sensitive silently reports the third as an absence -- which is how this
# script's first run reported version.py as having no version at all.
SDK_VERSION_FIELD = re.compile(r'sdk_version"?\s*[:=]\s*"([^"]+)"', re.IGNORECASE)

# Gradle is not a data format anything here can parse, so `.kts` is matched at
# the line start -- precise enough that a dependency constraint cannot match.
GRADLE_VERSION = re.compile(r'(?m)^\s*version\s*=\s*"([^"]+)"')


def manifest_version(path: Path) -> list[str]:
    """The version a manifest pins, read by a parser that knows its format."""
    suffix = path.suffix
    if suffix == ".json":
        return [json.loads(path.read_text(encoding="utf-8")).get("version", "")]
    if suffix == ".toml":
        data = tomllib.loads(path.read_text(encoding="utf-8"))
        # pyproject.toml pins it under [project]; Cargo.toml under [package].
        for section in ("project", "package"):
            block = data.get(section)
            if isinstance(block, dict) and block.get("version"):
                return [block["version"]]
        return []
    if suffix == ".kts":
        return GRADLE_VERSION.findall(path.read_text(encoding="utf-8"))
    raise ValueError(f"no reader for {path.suffix}: {path}")

# SDK -> (manifest, [stampers]). The manifest is the authority for that SDK.
SDKS: dict[str, tuple[str, list[str]]] = {
    "python": (
        "python-sdk/pyproject.toml",
        [
            "python-sdk/cleat_sdk/version.py",
            "python-sdk/scripts/stamp_metadata.py",
        ],
    ),
    "rust": (
        "crates/cleat-sdk/Cargo.toml",
        ["crates/cleat-sdk/src/bin/inject_metadata.rs"],
    ),
    "java": (
        "crates/cleat-java/build.gradle.kts",
        ["crates/cleat-java/scripts/inject-metadata.sh"],
    ),
    "assemblyscript": (
        "packages/cleat-as/package.json",
        ["packages/cleat-as/scripts/inject-metadata.js"],
    ),
}


def read_version(path: Path, pattern: re.Pattern[str]) -> list[str]:
    """Every match of `pattern` in the file, in order.

    ALL of them, not the first: a stamper may carry the version more than once
    (the Java stamper has two sites) and one of them going stale while the other
    is bumped is the defect, not an edge case.
    """
    return pattern.findall(path.read_text(encoding="utf-8"))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--expected",
        help="the release version to require. Default: whatever each manifest says, "
        "which checks AGREEMENT rather than a specific number.",
    )
    args = parser.parse_args()

    root = Path(__file__).resolve().parent.parent
    problems: list[str] = []
    rows: list[tuple[str, str, str, str]] = []

    for sdk, (manifest_rel, stamper_rels) in sorted(SDKS.items()):
        manifest = root / manifest_rel
        if not manifest.is_file():
            problems.append(f"{sdk}: manifest {manifest_rel} is missing")
            continue
        versions = manifest_version(manifest)
        if len(versions) != 1 or not versions[0]:
            problems.append(
                f"{sdk}: {manifest_rel} yielded {versions!r}; the manifest must pin "
                f"exactly one version"
            )
            continue
        pinned = versions[0]
        if args.expected and pinned != args.expected:
            problems.append(
                f"{sdk}: {manifest_rel} is {pinned}, expected {args.expected}"
            )

        for stamper_rel in stamper_rels:
            stamper = root / stamper_rel
            if not stamper.is_file():
                problems.append(f"{sdk}: stamper {stamper_rel} is missing")
                continue
            stamped = read_version(stamper, SDK_VERSION_FIELD)
            if not stamped:
                problems.append(
                    f"{sdk}: nothing version-shaped matched in {stamper_rel} -- either "
                    f"the stamper changed shape or the pattern no longer sees it"
                )
                continue
            for value in stamped:
                ok = value == pinned
                rows.append((sdk, stamper_rel, value, "ok" if ok else "MISMATCH"))
                if not ok:
                    problems.append(
                        f"{sdk}: {stamper_rel} stamps {value} but {manifest_rel} is "
                        f"{pinned} -- a workflow compiled from this SDK would report "
                        f"the wrong version to every consumer"
                    )

    for sdk, rel, value, status in rows:
        print(f"  {status:>9}  {sdk:<14} {value:<10} {rel}")
        # Printed rather than summarised: the point is which SITE is wrong, and a
        # count cannot say that.

    for problem in problems:
        print(f"::error title=SDK version agreement::{problem}")

    print(
        f"checked {len(SDKS)} SDKs; {len(rows)} stamping site(s); "
        f"{len(problems)} problem(s)"
    )
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
