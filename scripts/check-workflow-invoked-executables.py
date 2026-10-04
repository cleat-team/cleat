#!/usr/bin/env python3
"""Refuse a workflow that runs a repo file by path when the tree does not have
it executable (cleat#3111).

Measured 2026-10-04 on cleat#2603: a two-commit PR dropped
`scripts/check-playbook-claims.py` from 100755 to 100644. `ci.yml` invokes it BY
PATH from its own shebang, with no interpreter:

    scripts/check-playbook-claims.py --self-test

so at 0644 the step dies with `Permission denied` (exit 126), and `Lint` -- a
required context on develop -- cannot pass. Nothing else in the toolchain could
see it: a mode change has no bytes to diff and no content to review, and
invoking a script as `python3 scripts/x.py` cannot observe the bit either. Two
readings agreed while both were blind in the same direction.

WHAT COUNTS AS AN INVOCATION. The FIRST whitespace-delimited token of a line,
after stripping a leading `- ` and a leading `run:`/`shell:` (the inline form),
when it names a path under a directory that holds runnable things. Two of this
check's properties fall out of taking the first token rather than being handled
by compatibility heuristics, and both are pinned by the self-test:

  * a YAML or shell comment line starts with `#`, so its first token is never a
    path. Prose cannot be mistaken for an invocation, and a line that merely
    MENTIONS a script is not a command that runs it.

CONTINUATIONS, THOUGH, MUST BE JOINED FIRST, and this check was wrong about that
until its own real run said so. A `\\`-continued command's ARGUMENT lines begin
at column 1 of their own line, so a first-token rule reads them as commands:
soak.yml continues `go test ... \\` onto a line beginning `./tests/soak/...`,
and the first version reported that as a by-path invocation of a script named
`tests/soak/...`. That is the trap CLAUDE.md records for reading shell out of a
workflow -- the command name is on the FIRST line, not the last -- and it is
pinned by a self-test case now.

Exit is three-valued, per this repo's convention: 0 pass, 1 a finding, 2
UNMEASURED -- the workflows or the index could not be read, so nothing was
checked. A check that measures nothing agrees with every tree, correct or not.
"""

import re
import subprocess
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
WORKFLOW_DIR = REPO / ".github" / "workflows"

# A repo-relative path under a directory that holds runnable things. Deliberately
# NOT "any tracked path": a workflow that names a document in an argument is not
# invoking it, and widening the pattern to every tracked file would flag those.
INVOKABLE = re.compile(r"^(?:\./)?((?:scripts|bin|hack|tools|tests)/[A-Za-z0-9_./-]+)$")


class Unmeasured(Exception):
    """The check could not establish what it was measuring."""


def first_token(line):
    """The command token of a workflow line, or '' if the line has none."""
    s = line.strip()
    if s.startswith("- "):
        s = s[2:].lstrip()
    for prefix in ("run:", "shell:"):
        if s.startswith(prefix):
            s = s[len(prefix):].lstrip()
            break
    parts = s.split()
    return parts[0] if parts else ""


def logical_lines(text):
    """(first_lineno, joined_text), with `\\`-continuations joined.

    The line number reported is the FIRST line's, so a finding points at the
    command rather than at an argument line a reader cannot act on.
    """
    out, buf, start = [], None, 0
    for i, line in enumerate(text.splitlines(), start=1):
        if buf is None:
            buf, start = line, i
        else:
            buf += " " + line.strip()
        if buf.rstrip().endswith("\\"):
            buf = buf.rstrip()[:-1]
            continue
        out.append((start, buf))
        buf = None
    if buf is not None:
        out.append((start, buf))
    return out


def invocations(text):
    """(lineno, path) for each by-path invocation in one workflow file."""
    for lineno, line in logical_lines(text):
        m = INVOKABLE.match(first_token(line))
        if m:
            yield lineno, m.group(1)


def tracked_modes():
    """{path: '100755'} for every tracked path, read from the index."""
    try:
        out = subprocess.run(
            ["git", "-C", str(REPO), "ls-files", "-s"],
            capture_output=True, text=True, check=True,
        ).stdout
    except (subprocess.CalledProcessError, FileNotFoundError) as exc:
        raise Unmeasured(f"`git ls-files -s` could not be read: {exc}") from exc
    modes = {}
    for line in out.splitlines():
        meta, sep, path = line.partition("\t")
        fields = meta.split()
        if sep and len(fields) >= 2:
            modes[path] = fields[0]
    return modes


def check(invoked, modes):
    """Complaints for {label: [(lineno, path), ...]} against {path: mode}.

    `modes` is injected so the self-test can supply its known-positive and
    known-negative without touching the tree.
    """
    problems = []
    for label, sites in sorted(invoked.items()):
        for lineno, path in sites:
            mode = modes.get(path)
            if mode is None:
                problems.append(
                    f"{label}:{lineno}: invokes {path} by path, but it is not tracked "
                    f"in this tree.\n"
                    f"    A by-path invocation of something git does not carry is a "
                    f"renamed file or a typo; either way the step cannot run, and "
                    f"skipping it would be silent exactly where it is most likely wrong."
                )
            elif mode != "100755":
                problems.append(
                    f"{label}:{lineno}: invokes {path} by path, but the tree records it "
                    f"{mode}, not 100755.\n"
                    f"    A workflow that runs a file by path needs the execute bit; "
                    f"without it the step dies with `Permission denied` (exit 126) and "
                    f"its job cannot pass. Fix with `chmod +x <path> && git add <path>` -- "
                    f"a mode change is invisible to `diff`, which is why this check exists."
                )
    return problems


def gather():
    """(invoked, total_sites) over the real tree."""
    if not WORKFLOW_DIR.is_dir():
        raise Unmeasured(f"{WORKFLOW_DIR} does not exist")
    files = sorted(p for p in WORKFLOW_DIR.iterdir() if p.suffix in (".yml", ".yaml"))
    if not files:
        raise Unmeasured(f"no workflow files under {WORKFLOW_DIR}")
    invoked, total = {}, 0
    for path in files:
        try:
            sites = list(invocations(path.read_text(encoding="utf-8")))
        except OSError as exc:
            raise Unmeasured(f"{path} could not be read: {exc}") from exc
        if sites:
            invoked[f".github/workflows/{path.name}"] = sites
            total += len(sites)
    return invoked, total


def self_test():
    failures = []
    ONE = {".github/workflows/wf.yml": [(10, "scripts/example.sh")]}

    # KNOWN-POSITIVE FIRST, and deliberately: a check that matches nothing reports
    # a clean tree, which reads exactly like this case passing. If this is green
    # and the case below is green, the pair says the comparison discriminates.
    if not check(ONE, {"scripts/example.sh": "100644"}):
        failures.append("  MISSED a by-path script recorded at 100644")
    if check(ONE, {"scripts/example.sh": "100755"}):
        failures.append("  FALSE POSITIVE on a by-path script recorded at 100755")
    if not check(ONE, {}):
        failures.append("  MISSED a by-path invocation of an untracked file")

    # The parser, asserted as behaviour rather than described. The two exclusions
    # the docstring claims -- comments, and the command name of a continuation --
    # are properties of taking the FIRST token, so they are pinned here.
    cases = [
        ("      scripts/example.sh --flag", "scripts/example.sh", "a plain invocation"),
        ("        - scripts/example.sh", "scripts/example.sh", "a YAML list item"),
        ("        run: scripts/example.sh", "scripts/example.sh", "the inline run: form"),
        ("          ./scripts/example.sh", "scripts/example.sh", "an explicit ./ prefix"),
        ("      # scripts/example.sh is described here", None, "a YAML comment"),
        ("      echo 'scripts/example.sh'", None, "prose naming a script"),
        ("      bash -e <<'EOF'", None, "a heredoc opener"),
        ("      git ls-files 'scripts/*.sh'", None, "an argument, not the command"),
    ]
    for line, want, why in cases:
        m = INVOKABLE.match(first_token(line))
        got = m.group(1) if m else None
        if got != want:
            failures.append(f"  parse {why}: {line.strip()!r} -> {got!r}, want {want!r}")

    # The continuation case, taken from the real tree rather than invented: this
    # is soak.yml's `go test` continued onto a line that begins with an ARGUMENT
    # path. Without the join it reports `tests/soak/...` as an invocation, which
    # is how the need for the join was found -- by running the check, not by
    # reasoning about it.
    cont = (
        "          go test -tags=soak_test -count=1 \\\n"
        "            ./tests/soak/... 2>&1 | tee soak-report.json\n"
        "          scripts/real.sh\n"
    )
    got_sites = list(invocations(cont))
    if got_sites != [(3, "scripts/real.sh")]:
        failures.append(
            f"  MIS-PARSED a line-continuation: {got_sites!r}, want [(3, 'scripts/real.sh')]"
        )

    if failures:
        print("self-test FAILED:\n" + "\n".join(failures), file=sys.stderr)
        return 1
    print("self-test passed")
    return 0


def main():
    if "--self-test" in sys.argv:
        return self_test()
    try:
        invoked, total = gather()
        modes = tracked_modes()
    except Unmeasured as exc:
        print(
            f"UNMEASURED: {exc}.\n"
            f"            This is a failure of the CHECK, not a finding about the tree: "
            f"no invocation was examined.",
            file=sys.stderr,
        )
        return 2
    if total == 0:
        print(
            "UNMEASURED: no workflow invokes a repo file by path, so this check "
            "measured nothing.\n"
            "            That is not the same as a clean tree, and in this repo it "
            "would mean the parser stopped matching.",
            file=sys.stderr,
        )
        return 2
    problems = check(invoked, modes)
    if problems:
        print("\n".join(problems), file=sys.stderr)
        print(
            f"\nChecked {total} by-path invocation(s) across {len(invoked)} workflow "
            f"file(s). A workflow that runs a file by path needs the execute bit in "
            f"the tree, and nothing else in the toolchain checks it (cleat#3111).",
            file=sys.stderr,
        )
        return 1
    print(
        f"OK: all {total} by-path invocation(s) across {len(invoked)} workflow file(s) "
        f"name files the tree records as 100755"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
