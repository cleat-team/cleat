#!/usr/bin/env python3
"""Every file named in a checked TLA+ spec's "Implemented by" header must
also appear in tla.yml's `paths:` trigger list, or a PR that changes that
file never runs the model check against it -- the exact "checks never
started" failure CLAUDE.md warns about, arriving through a spec header that
grew without its CI trigger growing to match.

A spec counts as "checked" (and is included in this comparison) only if it
has a matching .cfg -- an unmaintained spec's stale "Implemented by" list is
not this script's concern; see specs/README.md.

Exit 0: every implementer of every checked spec is covered.
Exit 1: at least one is not -- named in the message.
"""
import re
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
SPECS_DIR = REPO_ROOT / "specs"
WORKFLOW = REPO_ROOT / ".github" / "workflows" / "tla.yml"

IMPLEMENTED_BY_RE = re.compile(r"Implemented by", re.IGNORECASE)
FILE_LINE_RE = re.compile(r"^\s*-\s+(\S+\.go)\b")


def implementers_of(tla_path: Path) -> set[str]:
    lines = tla_path.read_text().splitlines()
    files = set()
    in_block = False
    for line in lines:
        if IMPLEMENTED_BY_RE.search(line):
            in_block = True
            continue
        if in_block:
            m = FILE_LINE_RE.match(line)
            if m:
                files.add(m.group(1))
                continue
            # A line inside the (...) header that isn't a "- path" bullet
            # (continuation prose) doesn't end the block; a block that never
            # produced a bullet, or a completely blank line, does.
            if line.strip() == "" or line.strip().startswith(")"):
                if files:
                    break
                # blank line with nothing collected yet -- keep scanning,
                # tolerates a header that wraps before the first bullet.
                continue
    return files


def paths_in_workflow(workflow_path: Path) -> set[str]:
    text = workflow_path.read_text()
    m = re.search(r"paths:\n((?:\s+- .*\n)+)", text)
    if not m:
        print(f"UNMEASURED: could not find a paths: block in {workflow_path}", file=sys.stderr)
        sys.exit(2)
    block = m.group(1)
    return set(re.findall(r'-\s+"([^"]+)"', block))


def path_is_covered(rel_path: str, patterns: set[str]) -> bool:
    if rel_path in patterns:
        return True
    for pat in patterns:
        if pat.endswith("/**") and rel_path.startswith(pat[:-2]):
            return True
    return False


def main() -> int:
    checked_specs = sorted(
        p for p in SPECS_DIR.glob("*.tla") if p.with_suffix(".cfg").exists()
    )
    if not checked_specs:
        print("UNMEASURED: no specs/*.tla has a matching .cfg -- nothing to compare", file=sys.stderr)
        return 2

    patterns = paths_in_workflow(WORKFLOW)
    missing: list[tuple[str, str]] = []
    for spec in checked_specs:
        implementers = implementers_of(spec)
        if not implementers:
            print(f"UNMEASURED: {spec.name} has a .cfg but no parseable "
                  "\"Implemented by\" header -- cannot check its coverage", file=sys.stderr)
            return 2
        for f in sorted(implementers):
            if not path_is_covered(f, patterns):
                missing.append((spec.name, f))

    if missing:
        print("tla.yml's paths: list is missing files named in a checked spec's header:")
        for spec_name, f in missing:
            print(f"  {spec_name} implements {f}, not in tla.yml paths:")
        print("\nAdd the missing path(s) to .github/workflows/tla.yml's paths: list.")
        return 1

    print(f"OK: {len(checked_specs)} checked spec(s), every implementer covered by tla.yml's paths:")
    return 0


if __name__ == "__main__":
    sys.exit(main())
