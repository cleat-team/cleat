#!/usr/bin/env python3
"""Refuse a runner job that carries no `timeout-minutes` and is not on the reasoned
exemption list (cleat#3492).

A job with no `timeout-minutes` inherits GitHub's 360-minute default, so a hang
holds a runner for six hours. cleat#3479 capped 43 jobs and cleat#3482 raised five
more, by hand. Nothing stopped the next job added from landing uncapped, and the
first sign of one is a hang.

WHAT IT CHECKS, over every `.github/workflows/*.yml` and `*.yaml`:

  * UNCAPPED    a runner job (it has `runs-on:`) with no `timeout-minutes`, that is
                not listed in scripts/workflow-timeout-exemptions.txt.
  * INVALID     a `timeout-minutes` that is neither a positive number nor a
                `${{ }}` expression. GitHub reads `0` and a stray word as no cap.
  * STALE       an exemption naming a job that does not exist, that is a
                reusable-workflow caller (it has no runner of its own, so there is
                nothing to cap), or that now carries a cap. A grant that covers
                nothing is invisible otherwise.

A job that only calls a reusable workflow (`uses:` and no `runs-on:`) is skipped:
its runner jobs live in the called file and are checked there.

THE EXEMPTION LIST is explicit and every line carries a reason. It is
`<workflow-file>:<job-id> | <reason>`; a line with no reason is refused. An
exemption goes when its job gets a cap, and STALE says so.

READ WITH A PARSER, NOT A REGEX. `timeout-minutes` can sit behind a comment, on a
matrix job, or be mentioned inside a `run:` block; a text search cannot tell the
key from a sentence about it. The self-test also reads the same files a second,
deliberately different way (line by line, by indentation) and fails if the two
disagree about the counts it prints, because two readings that go wrong in
different directions are the only thing that can disagree.

EXIT STATUS. `0` clean, `1` a finding, `2` the check could not establish what it
was measuring (no workflow files, a file that does not parse, an exemption list
that is missing or malformed, no runner job at all). `0` and `2` must not be the
same status: a check that measured nothing agrees with every tree. Both `1` and
`2` fail CI; the difference is where to send the person who has to fix it.

THE COUNTS IT PRINTS ARE NOT ASSERTED. They are a census of a growing population
and would be wrong the next time a job is added. The self-test asserts that the
two readings agree, never what the number is.

Usage:
  scripts/check-workflow-job-timeouts.py               check this repository
  scripts/check-workflow-job-timeouts.py --self-test   check the checker; no network
  scripts/check-workflow-job-timeouts.py --root DIR --exemptions FILE
"""

from __future__ import annotations

import argparse
import contextlib
import io
import re
import shutil
import sys
import tempfile
from pathlib import Path

import yaml

REPO = Path(__file__).resolve().parent.parent
DEFAULT_EXEMPTIONS = REPO / "scripts" / "workflow-timeout-exemptions.txt"
EXEMPTION_KEY = re.compile(r"^[A-Za-z0-9._-]+\.ya?ml:[A-Za-z0-9_-]+$")
MIN_REASON_CHARS = 20


class Unmeasured(Exception):
    """The check cannot trust its own input; reported as exit 2."""


def workflow_files(root: Path) -> list[Path]:
    wf = root / ".github" / "workflows"
    files = sorted(p for p in wf.glob("*") if p.suffix in (".yml", ".yaml")) if wf.is_dir() else []
    if not files:
        raise Unmeasured(f"no workflow files under {wf}")
    return files


def load_jobs(path: Path) -> dict:
    try:
        doc = yaml.safe_load(path.read_text())
    except yaml.YAMLError as exc:
        raise Unmeasured(f"{path.name} does not parse: {str(exc).splitlines()[0]}") from exc
    jobs = doc.get("jobs") if isinstance(doc, dict) else None
    if not isinstance(jobs, dict) or not jobs:
        raise Unmeasured(f"{path.name} has no `jobs:` mapping")
    return jobs


def classify(wf: str, jid: str, job) -> str:
    """'runner' (has runs-on), 'caller' (uses, no runs-on), else Unmeasured."""
    if isinstance(job, dict) and "runs-on" in job:
        return "runner"
    if isinstance(job, dict) and "uses" in job:
        return "caller"
    raise Unmeasured(f"{wf}:{jid} has neither `runs-on:` nor `uses:`; cannot tell what it is")


def cap_problem(value) -> str | None:
    """None if a usable cap, else why it is not one."""
    if isinstance(value, bool):
        return f"`{value}` is not a number"
    if isinstance(value, (int, float)):
        return None if value > 0 else f"`{value}` is not a positive number"
    if isinstance(value, str) and "${{" in value:
        return None
    return f"`{value}` is neither a positive number nor a ${{{{ }}}} expression"


def read_exemptions(path: Path) -> dict[str, str]:
    if not path.is_file():
        raise Unmeasured(f"exemption list {path} is missing")
    out: dict[str, str] = {}
    for n, raw in enumerate(path.read_text().splitlines(), 1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        key, sep, reason = (part.strip() for part in line.partition("|"))
        if not sep or not EXEMPTION_KEY.match(key):
            raise Unmeasured(f"{path.name}:{n} is not `<workflow>.yml:<job-id> | <reason>`: {line[:80]}")
        if len(reason) < MIN_REASON_CHARS:
            raise Unmeasured(f"{path.name}:{n} gives {key} no real reason (under {MIN_REASON_CHARS} characters)")
        if key in out:
            raise Unmeasured(f"{path.name}:{n} lists {key} twice")
        out[key] = reason
    return out


def check(root: Path, exemptions_path: Path) -> tuple[int, str, dict]:
    """Returns (exit status, report text, counts)."""
    counts = {"files": 0, "runner": 0, "capped": 0, "exempt": 0, "uncapped": 0, "callers": 0}
    findings: list[str] = []
    try:
        exemptions = read_exemptions(exemptions_path)
        files = workflow_files(root)
        seen: dict[str, tuple[str, bool]] = {}  # key -> (kind, capped)
        for path in files:
            counts["files"] += 1
            for jid, job in load_jobs(path).items():
                kind = classify(path.name, jid, job)
                key = f"{path.name}:{jid}"
                if kind == "caller":
                    counts["callers"] += 1
                    seen[key] = (kind, False)
                    continue
                counts["runner"] += 1
                value = job.get("timeout-minutes")
                capped = value is not None
                seen[key] = (kind, capped)
                if capped:
                    problem = cap_problem(value)
                    if problem:
                        findings.append(f"INVALID  {key}: timeout-minutes {problem}")
                    else:
                        counts["capped"] += 1
                elif key in exemptions:
                    counts["exempt"] += 1
                else:
                    counts["uncapped"] += 1
                    findings.append(f"UNCAPPED {key}: no timeout-minutes, and it is not in {exemptions_path.name}")
        if counts["runner"] == 0:
            raise Unmeasured("no runner job found in any workflow, so nothing was checked")
        for key, reason in sorted(exemptions.items()):
            kind, capped = seen.get(key, (None, False))
            if kind is None:
                findings.append(f"STALE    {key}: exempted, but no such job exists")
            elif kind == "caller":
                findings.append(f"STALE    {key}: exempted, but it only calls a reusable workflow; nothing to cap here")
            elif capped:
                findings.append(f"STALE    {key}: exempted, but it now carries a timeout-minutes; remove the exemption")
    except Unmeasured as exc:
        return 2, (
            f"UNMEASURED: {exc}\n"
            "This is a failure of the check, not a finding about the tree.\n"
        ), counts
    head = (
        f"read {counts['files']} workflow files: {counts['runner']} runner jobs "
        f"({counts['capped']} capped, {counts['exempt']} exempt, {counts['uncapped']} uncapped and not exempt), "
        f"{counts['callers']} reusable-workflow callers skipped"
    )
    if findings:
        return 1, head + "\n" + "\n".join(findings) + f"\n{len(findings)} finding(s).\n", counts
    return 0, head + "\nOK\n", counts


# --- the second reading, used only by the self-test --------------------------------

JOB_KEY = re.compile(r"^  ([A-Za-z0-9_-]+):\s*(#.*)?$")
FIELD = re.compile(r"^    (runs-on|uses|timeout-minutes):")


def line_reading(root: Path) -> dict[str, dict[str, bool]]:
    """Per `<file>:<job>`, which of runs-on / uses / timeout-minutes sit at job level,
    read by indentation with no YAML parser. Deliberately a different mistake from
    `check`: it cannot be fooled by a key mentioned in prose at a deeper indent, and
    it would be fooled by an unusual indent, which the parser would not."""
    out: dict[str, dict[str, bool]] = {}
    for path in workflow_files(root):
        in_jobs, cur = False, None
        for line in path.read_text().splitlines():
            if re.match(r"^jobs:\s*(#.*)?$", line):
                in_jobs = True
                continue
            if in_jobs and re.match(r"^\S", line) and not line.startswith("#"):
                in_jobs = False
            if not in_jobs:
                continue
            m = JOB_KEY.match(line)
            if m:
                cur = f"{path.name}:{m.group(1)}"
                out[cur] = {"runs-on": False, "uses": False, "timeout-minutes": False}
                continue
            f = FIELD.match(line)
            if f and cur:
                out[cur][f.group(1)] = True
    return out


def line_counts(root: Path, exemptions: dict[str, str]) -> dict:
    reading = line_reading(root)
    runner = {k: v for k, v in reading.items() if v["runs-on"]}
    return {
        "runner": len(runner),
        "capped": sum(1 for v in runner.values() if v["timeout-minutes"]),
        "exempt": sum(1 for k, v in runner.items() if not v["timeout-minutes"] and k in exemptions),
        "uncapped": sum(1 for k, v in runner.items() if not v["timeout-minutes"] and k not in exemptions),
        "callers": sum(1 for v in reading.values() if v["uses"] and not v["runs-on"]),
    }


# --- self-test ----------------------------------------------------------------------

WF = "name: t\non: push\njobs:\n{jobs}"
JOB_CAPPED = "  {id}:\n    runs-on: ubuntu-latest\n    timeout-minutes: 10\n    steps:\n      - run: echo hi\n"
JOB_BARE = "  {id}:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"
EX_REASON = "a reason that is long enough to count"


def fixture(files: dict[str, str], exemptions: str | None) -> tuple[Path, Path]:
    root = Path(tempfile.mkdtemp(prefix="jobtimeouts-"))
    (root / ".github" / "workflows").mkdir(parents=True)
    for name, body in files.items():
        (root / ".github" / "workflows" / name).write_text(body)
    ex = root / "exemptions.txt"
    if exemptions is not None:
        ex.write_text(exemptions)
    return root, ex


def self_test() -> int:
    failures: list[str] = []

    def expect(label: str, files: dict[str, str], exemptions: str | None, want: int, *texts: str) -> None:
        root, ex = fixture(files, exemptions)
        try:
            code, report, _ = check(root, ex)
        finally:
            shutil.rmtree(root, ignore_errors=True)
        miss = [t for t in texts if t not in report]
        if code != want or miss:
            failures.append(f"{label}: exit {code} (want {want}); report lacks {miss}; report was:\n{report}")

    capped = lambda *ids: WF.format(jobs="".join(JOB_CAPPED.format(id=i) for i in ids))
    ex = lambda *lines: "\n".join(lines) + "\n"

    expect("clean tree", {"a.yml": capped("build", "test")}, ex(), 0, "2 runner jobs", "OK")
    # The KNOWN-POSITIVE: the case the guard exists for. Asserting the TEXT as well as the
    # status, because a status alone is also what a different mechanism would produce.
    expect("known positive: an uncapped job", {"a.yml": WF.format(jobs=JOB_CAPPED.format(id="ok") + JOB_BARE.format(id="build"))},
           ex(), 1, "UNCAPPED a.yml:build", "1 uncapped and not exempt")
    expect("an exempted uncapped job passes", {"a.yml": WF.format(jobs=JOB_BARE.format(id="build"))},
           ex(f"a.yml:build | {EX_REASON}"), 0, "1 exempt", "OK")
    expect("an exemption for a job that is gone", {"a.yml": capped("build")},
           ex(f"a.yml:gone | {EX_REASON}"), 1, "STALE    a.yml:gone", "no such job exists")
    expect("an exemption for a job that now has a cap", {"a.yml": capped("build")},
           ex(f"a.yml:build | {EX_REASON}"), 1, "STALE    a.yml:build", "now carries a timeout-minutes")
    caller = "  call:\n    uses: ./.github/workflows/b.yml\n"
    expect("an exemption for a reusable-workflow caller",
           {"a.yml": WF.format(jobs=caller + JOB_CAPPED.format(id="x")), "b.yml": capped("y")},
           ex(f"a.yml:call | {EX_REASON}"), 1, "STALE    a.yml:call", "only calls a reusable workflow")
    expect("a reusable-workflow caller is skipped, not flagged",
           {"a.yml": WF.format(jobs=caller + JOB_CAPPED.format(id="x")), "b.yml": capped("y")},
           ex(), 0, "1 reusable-workflow callers skipped")
    expect("an expression is a usable cap",
           {"a.yml": WF.format(jobs="  j:\n    runs-on: x\n    timeout-minutes: ${{ inputs.minutes }}\n")}, ex(), 0, "1 capped")
    expect("zero is not a cap",
           {"a.yml": WF.format(jobs="  j:\n    runs-on: x\n    timeout-minutes: 0\n")}, ex(), 1, "INVALID  a.yml:j", "not a positive number")
    expect("a word is not a cap",
           {"a.yml": WF.format(jobs="  j:\n    runs-on: x\n    timeout-minutes: soon\n")}, ex(), 1, "INVALID  a.yml:j")
    # A sentence about the key is not the key: the reason this reads YAML rather than text.
    prose = "  j:\n    runs-on: x\n    steps:\n      - run: |\n          # timeout-minutes: 5 would go here\n          echo hi\n"
    expect("prose mentioning the key is not a cap", {"a.yml": WF.format(jobs=prose)}, ex(), 1, "UNCAPPED a.yml:j")
    matrix = "  m:\n    runs-on: x\n    strategy:\n      matrix:\n        n: [1, 2]\n    timeout-minutes: 15\n"
    expect("a matrix job counts once", {"a.yml": WF.format(jobs=matrix)}, ex(), 0, "1 runner jobs", "1 capped")
    expect("no workflow files", {}, ex(), 2, "UNMEASURED: no workflow files", "not a finding about the tree")
    expect("a file that does not parse", {"a.yml": "jobs: [unclosed\n"}, ex(), 2, "UNMEASURED: a.yml does not parse")
    expect("a workflow with no jobs", {"a.yml": "name: t\non: push\n"}, ex(), 2, "UNMEASURED: a.yml has no `jobs:`")
    expect("only reusable callers means nothing was checked", {"a.yml": WF.format(jobs=caller)}, ex(), 2, "nothing was checked")
    expect("a job that is neither a runner nor a caller", {"a.yml": WF.format(jobs="  j:\n    name: x\n")}, ex(), 2, "neither `runs-on:` nor `uses:`")
    expect("a missing exemption list", {"a.yml": capped("build")}, None, 2, "exemption list", "is missing")
    expect("an exemption with no reason", {"a.yml": capped("build")}, ex("a.yml:build |"), 2, "no real reason")
    expect("an exemption with a token reason", {"a.yml": capped("build")}, ex("a.yml:build | todo"), 2, "no real reason")
    expect("a malformed exemption", {"a.yml": capped("build")}, ex("not an exemption"), 2, "is not `<workflow>.yml:<job-id> | <reason>`")
    expect("a duplicated exemption", {"a.yml": WF.format(jobs=JOB_BARE.format(id="b"))},
           ex(f"a.yml:b | {EX_REASON}", f"a.yml:b | {EX_REASON}"), 2, "lists a.yml:b twice")
    expect("comments and blank lines in the list are ignored", {"a.yml": WF.format(jobs=JOB_BARE.format(id="b"))},
           "# a comment\n\n" + ex(f"a.yml:b | {EX_REASON}"), 0, "1 exempt")

    # This repository's own tree, read two ways. Not asserting 71 or 10 -- asserting that the
    # parser and the line reader, which fail in different directions, agree, and printing both.
    code, report, counts = check(REPO, DEFAULT_EXEMPTIONS)
    if code != 0:
        failures.append(f"this repository's own tree does not pass:\n{report}")
    else:
        second = line_counts(REPO, read_exemptions(DEFAULT_EXEMPTIONS))
        if any(counts[k] != second[k] for k in second):
            failures.append(f"the two readings of this tree disagree: parser {counts} vs line reader {second}")
        print(f"self-test: this tree, parser {{runner={counts['runner']} capped={counts['capped']} "
              f"exempt={counts['exempt']} uncapped={counts['uncapped']} callers={counts['callers']}}}; "
              f"line reader {second}")

        # A known-positive in the REAL shape: strip one real job's cap from a copy of this
        # tree and require the guard to name that job. Fixtures are tidy; the real files are not.
        root = Path(tempfile.mkdtemp(prefix="jobtimeouts-real-"))
        try:
            shutil.copytree(REPO / ".github" / "workflows", root / ".github" / "workflows")
            reading = line_reading(root)
            victim = next((k for k, v in reading.items() if v["runs-on"] and v["timeout-minutes"]
                           and k not in read_exemptions(DEFAULT_EXEMPTIONS)), None)
            if victim is None:
                failures.append("real-shape positive: the line reader found no capped job to strip")
            else:
                vfile, vjob = victim.split(":")
                wf_path = root / ".github" / "workflows" / vfile
                lines, in_job, removed = wf_path.read_text().splitlines(keepends=True), False, False
                for i, line in enumerate(lines):
                    m = JOB_KEY.match(line)
                    if m:
                        in_job = m.group(1) == vjob
                    if in_job and line.startswith("    timeout-minutes:"):
                        del lines[i]
                        removed = True
                        break
                wf_path.write_text("".join(lines))
                if not removed:
                    failures.append(f"real-shape positive: could not remove the cap from {victim}")
                else:
                    code, report, _ = check(root, DEFAULT_EXEMPTIONS)
                    if code != 1 or f"UNCAPPED {victim}" not in report:
                        failures.append(f"real-shape positive: stripping {victim}'s cap gave exit {code}:\n{report}")
        finally:
            shutil.rmtree(root, ignore_errors=True)

    if failures:
        print("self-test FAILED:\n" + "\n\n".join(failures), file=sys.stderr)
        return 1
    print("self-test: ok")
    return 0


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--self-test", action="store_true")
    ap.add_argument("--root", type=Path, default=REPO)
    ap.add_argument("--exemptions", type=Path, default=None)
    args = ap.parse_args(argv)
    if args.self_test:
        return self_test()
    code, report, _ = check(args.root, args.exemptions or args.root / "scripts" / "workflow-timeout-exemptions.txt")
    print(report, end="", file=sys.stderr if code else sys.stdout)
    return code


if __name__ == "__main__":
    sys.exit(main())
