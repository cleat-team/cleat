#!/usr/bin/env python3
"""Generator and drift check for the TLA+ docs/CI surface.

Three things are DERIVED from what's on disk rather than hand-maintained,
because hand-maintaining them is what caused cleat#2044 (four
dequeue-and-rebase collisions on specs/README.md in one day, two of them
against the same PR): a checked spec's "Implemented by" header is the single
source of truth, and everything below is regenerated from it plus the set of
specs/*.cfg and specs/*.md files actually present.

  1. .github/workflows/tla.yml's `paths:` trigger list -- the fixed entries
     plus the union of every checked spec's "Implemented by" files.
  2. The generated index table in specs/README.md, between the
     "BEGIN/END GENERATED: spec-index" markers.
  3. The generated `java -cp tla2tools.jar ...` invocation list in
     specs/README.md, between the "BEGIN/END GENERATED: tlc-invocations"
     markers.

Run with --write to regenerate all three in place. Run with no flag (what CI
does) to check the committed files already match a fresh generation --
exit 0 if they do, exit 1 naming the drift if they don't, exit 2
(UNMEASURED) if a checked spec's header can't be parsed at all, per
CLAUDE.md's "give 'I could not look' its own exit status" rule: 0 and 2 must
not collide, or a spec this script can't read passes as though covered.

A spec counts as "checked" (and is included in the paths-list comparison and
the tlc-invocations list) only if it has a matching .cfg -- an unmaintained
spec's stale "Implemented by" list is not this script's concern. A spec
counts as having a status file (and gets a link in the index) only if
specs/<Spec>.md exists, front-mattered with a first line reading
`<!-- tla-index: issue=NNNN -->` naming its tracking issue.
"""
from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
SPECS_DIR = REPO_ROOT / "specs"
WORKFLOW = REPO_ROOT / ".github" / "workflows" / "tla.yml"
README = SPECS_DIR / "README.md"

IMPLEMENTED_BY_RE = re.compile(r"Implemented by", re.IGNORECASE)
FILE_LINE_RE = re.compile(r"^\s*-\s+(\S+\.go)\b")
ISSUE_FRONTMATTER_RE = re.compile(r"^<!--\s*tla-index:\s*issue=(\d+)\s*-->\s*$")

FIXED_PATHS = ["specs/**", "Makefile", ".github/workflows/tla.yml"]

GENERATE_HINT = "python3 scripts/check-tla-ci-paths.py --write"


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


def issue_of(md_path: Path) -> str | None:
    if not md_path.exists():
        return None
    lines = md_path.read_text().splitlines()
    if not lines:
        return None
    m = ISSUE_FRONTMATTER_RE.match(lines[0])
    return m.group(1) if m else None


def all_specs() -> list[Path]:
    return sorted(SPECS_DIR.glob("*.tla"))


def checked_specs() -> list[Path]:
    return sorted(p for p in all_specs() if p.with_suffix(".cfg").exists())


def generate_workflow_paths(checked: list[Path]) -> list[str]:
    implementers: set[str] = set()
    for spec in checked:
        implementers |= implementers_of(spec)
    return FIXED_PATHS + sorted(implementers)


def generate_index_block(specs: list[Path]) -> list[str]:
    lines = [
        "<!-- BEGIN GENERATED: spec-index -->",
        f"<!-- Regenerate with: {GENERATE_HINT} -->",
        "| spec | issue | checked by TLC | maintained | status file |",
        "|---|---|---|---|---|",
    ]
    for spec in specs:
        checked = spec.with_suffix(".cfg").exists()
        md = spec.with_suffix(".md")
        issue = issue_of(md)
        if checked:
            issue_cell = f"cleat#{issue}" if issue else "—"
            status_file = f"[`{md.name}`]({md.name})" if md.exists() else "—"
            lines.append(
                f"| `{spec.name}` | {issue_cell} | Yes | Yes | {status_file} |"
            )
        else:
            lines.append(
                f"| `{spec.name}` | — | No | **No — superseded, see above** | — |"
            )
    lines.append("<!-- END GENERATED: spec-index -->")
    return lines


def generate_tlc_block(checked: list[Path]) -> list[str]:
    lines = [f"# BEGIN GENERATED: tlc-invocations ({GENERATE_HINT})"]
    for spec in checked:
        lines.append(
            f"java -cp tla2tools.jar tlc2.TLC -config specs/{spec.with_suffix('.cfg').name} specs/{spec.name}"
        )
    lines.append("# END GENERATED: tlc-invocations")
    return lines


def replace_marked_block(text: str, begin: str, end: str, new_lines: list[str]) -> str:
    pattern = re.compile(
        re.escape(begin) + r".*?" + re.escape(end), re.DOTALL
    )
    if not pattern.search(text):
        print(f"UNMEASURED: could not find a {begin} ... {end} block in {README}", file=sys.stderr)
        sys.exit(2)
    return pattern.sub("\n".join(new_lines), text)


def read_marked_block(text: str, begin: str, end: str) -> str:
    pattern = re.compile(re.escape(begin) + r".*?" + re.escape(end), re.DOTALL)
    m = pattern.search(text)
    if not m:
        print(f"UNMEASURED: could not find a {begin} ... {end} block in {README}", file=sys.stderr)
        sys.exit(2)
    return m.group(0)


def paths_block_span(text: str) -> re.Match:
    m = re.search(r"paths:\n((?:\s+- .*\n)+)", text)
    if not m:
        print(f"UNMEASURED: could not find a paths: block in {WORKFLOW}", file=sys.stderr)
        sys.exit(2)
    return m


def do_write() -> int:
    checked = checked_specs()
    if not checked:
        print("UNMEASURED: no specs/*.tla has a matching .cfg -- nothing to generate", file=sys.stderr)
        return 2
    for spec in checked:
        if not implementers_of(spec):
            print(f"UNMEASURED: {spec.name} has a .cfg but no parseable "
                  "\"Implemented by\" header -- cannot generate its coverage", file=sys.stderr)
            return 2

    wf_text = WORKFLOW.read_text()
    m = paths_block_span(wf_text)
    indent = re.match(r"\s+", m.group(1).splitlines()[0]).group(0)
    new_block = "".join(f'{indent}- "{p}"\n' for p in generate_workflow_paths(checked))
    wf_text = wf_text[: m.start(1)] + new_block + wf_text[m.end(1):]
    WORKFLOW.write_text(wf_text)

    readme_text = README.read_text()
    readme_text = replace_marked_block(
        readme_text,
        "<!-- BEGIN GENERATED: spec-index -->",
        "<!-- END GENERATED: spec-index -->",
        generate_index_block(all_specs()),
    )
    readme_text = replace_marked_block(
        readme_text,
        "# BEGIN GENERATED: tlc-invocations",
        "# END GENERATED: tlc-invocations",
        generate_tlc_block(checked),
    )
    README.write_text(readme_text)

    print(f"Wrote {WORKFLOW.relative_to(REPO_ROOT)} and {README.relative_to(REPO_ROOT)} "
          f"from {len(checked)} checked spec(s).")
    return 0


def do_check() -> int:
    checked = checked_specs()
    if not checked:
        print("UNMEASURED: no specs/*.tla has a matching .cfg -- nothing to compare", file=sys.stderr)
        return 2
    for spec in checked:
        if not implementers_of(spec):
            print(f"UNMEASURED: {spec.name} has a .cfg but no parseable "
                  "\"Implemented by\" header -- cannot check its coverage", file=sys.stderr)
            return 2

    problems: list[str] = []

    wf_text = WORKFLOW.read_text()
    m = paths_block_span(wf_text)
    actual_paths = set(re.findall(r'-\s+"([^"]+)"', m.group(1)))
    expected_paths = set(generate_workflow_paths(checked))
    missing = expected_paths - actual_paths
    extra = actual_paths - expected_paths
    for p in sorted(missing):
        problems.append(f"tla.yml's paths: is missing {p!r} (named in a checked spec's header)")
    for p in sorted(extra):
        problems.append(f"tla.yml's paths: has {p!r}, which no checked spec's header names any more")

    readme_text = README.read_text()
    actual_index = read_marked_block(
        readme_text, "<!-- BEGIN GENERATED: spec-index -->", "<!-- END GENERATED: spec-index -->"
    )
    expected_index = "\n".join(generate_index_block(all_specs()))
    if actual_index.strip() != expected_index.strip():
        problems.append("specs/README.md's spec-index block is stale")

    actual_tlc = read_marked_block(
        readme_text, "# BEGIN GENERATED: tlc-invocations", "# END GENERATED: tlc-invocations"
    )
    expected_tlc = "\n".join(generate_tlc_block(checked))
    if actual_tlc.strip() != expected_tlc.strip():
        problems.append("specs/README.md's tlc-invocations block is stale")

    if problems:
        print("TLA+ docs/CI surface has drifted from what's on disk:")
        for p in problems:
            print(f"  {p}")
        print(f"\nRegenerate with: {GENERATE_HINT}")
        return 1

    print(f"OK: {len(checked)} checked spec(s), tla.yml paths: and specs/README.md's "
          "generated blocks all match a fresh generation")
    return 0


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--write", action="store_true",
                         help="regenerate tla.yml and specs/README.md's generated blocks in place")
    args = parser.parse_args()
    return do_write() if args.write else do_check()


if __name__ == "__main__":
    sys.exit(main())
