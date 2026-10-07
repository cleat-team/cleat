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

USE A PARSER WHERE THE SYNTAX IS PARSEABLE; fall back to a token match only
where no parser exists. TOML and JSON are read with `tomllib` and `json`; the
stampers are source files, where there is nothing to parse, so they get a
pattern on the field token. This rule was learned by breaking it -- see
"THE BUG THIS TOOL WAS BORN FROM" below.

THE BUG THIS TOOL WAS BORN FROM, kept because it is the failure mode the whole
script exists to avoid, one level up. Its first version matched a regex rooted
at `version` against every file. Against `package.json` that read
`"version": "0.1.0"` as ZERO matches -- the key's closing quote sits between
the name and the colon -- while SIMULTANEOUSLY matching a dependency constraint
and a classifier as if they were versions. It did not merely fail to see its
target; it invented three substitutes. So loudness was not available as a fix:
the output was a confident, specific, wrong answer, and no amount of volume
helps with a value that is present and incorrect. Use the parser.

TWO FLOORS THAT LOOK LIKE ONE, distinguished because a reader will collapse
them. This script needs Python 3.11+ for `tomllib`; that is the runner's 3.12
and says nothing about the SDK. The SDK's own floor is `requires-python =
">=3.10"`, and the Python stamper this checks needs 3.10+ as well -- its
signatures use `bytes | None`, which is 3.10 syntax -- and that is CONSISTENT
with the package it ships inside, so leave the signature alone rather than
widening it to run on an older interpreter.

The Go SDK is deliberately absent: `wasm/build.go` DERIVES its `sdkVersion`
from `sdkRequiredVersion(cfg.ProjectRoot)` rather than hardcoding it, so it has
no literal to keep in step. That is worth leaving a note about, per #2454, so
the next reader does not "fix" it by adding one.

Usage:
    scripts/check-sdk-version-agreement.py                    # agreement only
    scripts/check-sdk-version-agreement.py --expected 0.3.0   # ...and the version
    scripts/check-sdk-version-agreement.py --self-test        # synthetic trees
"""

from __future__ import annotations

import argparse
import json
import re
import sys
import tempfile
import tomllib
from pathlib import Path

# Source files only (the stampers): the field token, with an optional closing
# quote so the same pattern reads `sdk_version = "x"`, `"sdk_version": "x"` and
# the Python SDK's uppercase constant `SDK_VERSION = "x"`. Case-insensitive
# because all three spellings are in use, and a case-sensitive name pattern
# reports the third as an ABSENCE -- which is how this script's first run
# reported version.py as carrying no version at all.
SDK_VERSION_FIELD = re.compile(r'sdk_version"?\s*[:=]\s*"([^"]+)"', re.IGNORECASE)

# Gradle is not a data format anything here can parse, so `.kts` is matched at
# the line start -- precise enough that a dependency constraint cannot match.
GRADLE_VERSION = re.compile(r'(?m)^\s*version\s*=\s*"([^"]+)"')

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

# Synthetic files for --self-test, one per site above. The Java stamper
# deliberately carries the version TWICE, matching the real one: a fixture with
# one site could not express "one of the two went stale", which is a real shape.
SITES: dict[str, str] = {
    "python-sdk/pyproject.toml": '[project]\nname = "cleat-sdk"\nversion = "%s"\n',
    "python-sdk/cleat_sdk/version.py": 'SDK_VERSION = "%s"\n',
    "python-sdk/scripts/stamp_metadata.py": '    "sdk_version": "%s",\n',
    "crates/cleat-sdk/Cargo.toml": '[package]\nname = "cleat-sdk"\nversion = "%s"\n',
    "crates/cleat-sdk/src/bin/inject_metadata.rs": '        "sdk_version": "%s",\n',
    "crates/cleat-java/build.gradle.kts": 'plugins { id("java") }\nversion = "%s"\n',
    "crates/cleat-java/scripts/inject-metadata.sh": '  "sdk_version": "%s",\n  "sdk_version": "%s",\n',
    "packages/cleat-as/package.json": '{\n  "name": "cleat-as",\n  "version": "%s"\n}\n',
    "packages/cleat-as/scripts/inject-metadata.js": '    sdk_version: "%s",\n',
}


# Where a stamper can live and what one looks like on disk. Discovery is narrow
# on purpose: the SDK trees only, code extensions only, and anchored on the
# ASSIGNMENT rather than a bare mention -- a document quoting `"sdk_version"` in
# an example is not a site, and a search that cannot tell a thing from a
# sentence about the thing is a trap this repo has paid for more than once.
DISCOVERY_ROOTS = ("python-sdk", "crates", "packages")
DISCOVERY_SUFFIXES = {".py", ".rs", ".sh", ".js", ".ts"}
DISCOVERY_SKIP = ("node_modules", "/build/", "/target/", "/dist/")

# Files that match SDK_VERSION_FIELD but are not a stamper -- test DATA that
# happens to assign the same-shaped field, not something a real build emits.
# cleat#3183: python-sdk/tests/test_stamp_metadata.py's "sdk_version": "0.3.2"
# is one entry of a hand-built dict literal proving inject_metadata/
# read_metadata round-trip a WHOLE metadata dict correctly (cleat#2936) -- the
# value is arbitrary test data with no reason to track python-sdk/pyproject.
# toml's real version, and adding it to SDKS would make every future version
# bump fail this test file's assertion for a reason unrelated to what it
# tests. Each entry needs a comment like this one: an exclusion list with no
# reason attached is indistinguishable from one that is silently growing to
# cover a real, un-checked stamper.
DISCOVERY_EXCLUDE_FILES = {
    "python-sdk/tests/test_stamp_metadata.py",
}


def discovered_sites(root: Path) -> set[str]:
    """Every file under the SDK trees that ASSIGNS a version."""
    found: set[str] = set()
    for base in DISCOVERY_ROOTS:
        base_dir = root / base
        if not base_dir.is_dir():
            continue
        for path in base_dir.rglob("*"):
            if not path.is_file() or path.suffix not in DISCOVERY_SUFFIXES:
                continue
            rel = str(path.relative_to(root))
            if any(part in rel for part in DISCOVERY_SKIP):
                continue
            if rel in DISCOVERY_EXCLUDE_FILES:
                continue
            if SDK_VERSION_FIELD.search(path.read_text(encoding="utf-8", errors="replace")):
                found.add(rel)
    return found


def manifest_version(path: Path) -> list[str]:
    """The version a manifest pins, read by a parser that knows its format."""
    suffix = path.suffix
    if suffix == ".json":
        value = json.loads(path.read_text(encoding="utf-8")).get("version")
        return [value] if value else []
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


def check(root: Path, expected: str) -> tuple[list[tuple], list[str]]:
    """(rows, problems) for the tree at `root`."""
    problems: list[str] = []
    rows: list[tuple[str, str, str, str]] = []

    for sdk, (manifest_rel, stamper_rels) in sorted(SDKS.items()):
        manifest = root / manifest_rel
        if not manifest.is_file():
            problems.append(f"{sdk}: manifest {manifest_rel} is missing")
            continue
        versions = manifest_version(manifest)
        if len(versions) != 1:
            problems.append(
                f"{sdk}: {manifest_rel} yielded {versions!r}; the manifest must pin "
                f"exactly one version"
            )
            continue
        pinned = versions[0]
        if expected and pinned != expected:
            problems.append(
                f"{sdk}: {manifest_rel} is {pinned}, expected {expected}"
            )

        for stamper_rel in stamper_rels:
            stamper = root / stamper_rel
            if not stamper.is_file():
                problems.append(f"{sdk}: stamper {stamper_rel} is missing")
                continue
            stamped = SDK_VERSION_FIELD.findall(stamper.read_text(encoding="utf-8"))
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
    return rows, problems


def write_tree(root: Path, overrides: dict[str, str] | None = None) -> None:
    """A minimal tree with one file per site, all at 0.3.0 unless overridden."""
    overrides = overrides or {}
    for rel, template in SITES.items():
        path = root / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        # .replace rather than `%`: the templates carry one `%s` or two (the Java
        # stamper has two sites), and `%` raises on the wrong arity.
        path.write_text(overrides.get(rel, template.replace("%s", "0.3.0")))


def self_test() -> int:
    """Known-positive/negative trees, so the partial-bump control is a mechanism.

    The hand-run that first proved this script works -- bump one manifest, watch
    exactly one problem name the site, restore -- was correct at that moment and
    proved nothing about the next edit. A control that runs once is a claim; one
    that runs every time is a check. These cases are that check.
    """
    failures: list[str] = []
    cases = 0

    def run(label: str, overrides: dict[str, str], expected: str,
            want_problems: int, want_substrings: list[str]):
        nonlocal cases
        cases += 1
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            write_tree(root, overrides)
            _, problems = check(root, expected)
        missing = [w for w in want_substrings if not any(w in p for p in problems)]
        if missing or len(problems) != want_problems:
            failures.append(
                f"{label}: {len(problems)} problems, want {want_problems}; "
                f"missing {missing}; got {problems}"
            )

    # KNOWN-NEGATIVE: a consistent tree. If this ever reports a problem the
    # script is inventing defects, which is the failure mode it was born from.
    run("consistent tree", {}, "", 0, [])

    # KNOWN-POSITIVE, and this is the case the whole script exists for: ONE
    # manifest bumped, its stamper left behind. Without this as a case, an edit
    # that stops detecting it reports 0 problems -- exactly what a healthy run
    # reports, so nothing would ever say so.
    run(
        "assemblyscript manifest bumped without its stamper",
        {"packages/cleat-as/package.json":
            SITES["packages/cleat-as/package.json"].replace("%s", "0.9.9")},
        "",
        1,
        ["packages/cleat-as/scripts/inject-metadata.js stamps 0.3.0 but "
         "packages/cleat-as/package.json is 0.9.9"],
    )

    # KNOWN-POSITIVE: the Java stamper carries the version TWICE, so only the
    # second site going stale is a distinct shape -- and it is the one a
    # hand-check of "the file mentions 0.3.0" would pass.
    run(
        "java stamper's SECOND site left behind",
        {"crates/cleat-java/scripts/inject-metadata.sh":
            '  "sdk_version": "0.3.0",\n  "sdk_version": "0.1.0",\n'},
        "",
        1,
        ["inject-metadata.sh stamps 0.1.0"],
    )

    # KNOWN-POSITIVE: --expected is set and the tree is not at it. This is the
    # release-branch invocation, where the branch name names the version.
    run(
        "release version expected, tree still on the old one",
        {},
        "0.4.0",
        4,
        ["is 0.3.0, expected 0.4.0"],
    )

    # KNOWN-POSITIVE: a site that cannot be read must be a problem, not silence.
    run(
        "python stamper deleted",
        {"python-sdk/scripts/stamp_metadata.py": ""},
        "",
        1,
        ["nothing version-shaped matched in python-sdk/scripts/stamp_metadata.py"],
    )

    # THE DECLARATIONS MUST RESEMBLE THE TREE, which is a different claim from
    # every case above. Those run against a SYNTHETIC tree, so a fixture that had
    # drifted from the repo would pass all of them while saying nothing about it
    # -- WS-2's lockfile shape, where one pattern matched one of two layouts and
    # reported zero for the other.
    #
    # Both directions, because they fail differently:
    #
    #   declared -> fixture  catches "a site was added to SDKS and to nothing
    #                        else", which is how the fixture silently stops
    #                        covering the thing it was extended for.
    #   tree -> declared     catches "a stamper exists in the repo and nothing
    #                        checks it" -- the SILENT one, because the script
    #                        then reports 0 problems, which is what a healthy
    #                        run reports.
    #
    # An earlier version of this case compared the ROWS each tree produced. That
    # was wrong and a mutation showed it: a declared-but-absent file produces a
    # problem and no row on BOTH sides, so the sets matched and the case passed.
    # Comparing what was READ cannot notice a site that was never read.
    repo_root = Path(__file__).resolve().parent.parent

    declared = {rel for rel, _ in SDKS.values()}
    declared |= {rel for _, rels in SDKS.values() for rel in rels}
    cases += 1
    if declared != set(SITES):
        failures.append(
            "SDKS and SITES disagree, so the fixture does not cover the "
            f"declarations. Only declared: {sorted(declared - set(SITES))}; only "
            f"in the fixture: {sorted(set(SITES) - declared)}"
        )

    cases += 1
    undeclared = discovered_sites(repo_root) - declared
    if undeclared:
        failures.append(
            f"these files assign a version and NOTHING checks them: {sorted(undeclared)}. "
            f"Add each to SDKS (and to SITES) or every release could ship it stale -- "
            f"this is the direction that reports 0 problems when it goes wrong."
        )

    # THE EXCLUSION MECHANISM ITSELF, on a synthetic tree rather than the real
    # repo (cleat#3183): the check above proves today's exclusion list matches
    # today's tree, which says nothing about whether excluding a NAME actually
    # suppresses it, or whether exclusion is scoped to that name rather than
    # swallowing everything. Both directions, because an exclusion that is too
    # wide is the more dangerous failure -- it reads as "nothing new to
    # declare" exactly like a correct one does.
    cases += 1
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        excluded_rel = next(iter(DISCOVERY_EXCLUDE_FILES))
        for rel in (excluded_rel, "python-sdk/tests/not_excluded.py"):
            path = root / rel
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text('sdk_version = "9.9.9"\n')
        found = discovered_sites(root)
        if excluded_rel in found:
            failures.append(
                f"discovery-exclusion case: {excluded_rel} was discovered despite "
                f"being in DISCOVERY_EXCLUDE_FILES -- the exclusion does nothing"
            )
        if "python-sdk/tests/not_excluded.py" not in found:
            failures.append(
                "discovery-exclusion case: python-sdk/tests/not_excluded.py was not "
                "discovered although it is not excluded -- the exclusion is swallowing "
                "files it should not, which would silently hide a real, un-checked stamper"
            )

    if failures:
        for f in failures:
            print(f"SELF-TEST FAILED: {f}")
        return 1
    print(f"self-test passed: {cases} cases")
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--expected",
        help="the release version to require. Default: whatever each manifest says, "
        "which checks AGREEMENT rather than a specific number. An empty string "
        "means the same as omitting it.",
    )
    parser.add_argument("--root", help="tree to check (default: the repo root)")
    parser.add_argument(
        "--self-test",
        action="store_true",
        help="run known-positive/known-negative cases against synthetic trees",
    )
    args = parser.parse_args()

    if args.self_test:
        return self_test()

    root = Path(args.root).resolve() if args.root else Path(__file__).resolve().parent.parent
    rows, problems = check(root, args.expected or "")

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
