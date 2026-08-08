#!/usr/bin/env python3
"""Structural guards over .github/workflows/, run by the Lint job.

Four checks, each catching a *class* of defect that has already cost this repo
a session to find one instance of by hand:

  1. Every context in .github/required-checks.txt resolves to a job that
     exists.  A context naming a job that is gone blocks every PR forever.
  2. No job whose display name is a required context carries job-level
     `continue-on-error`.  A required check that cannot report failure is the
     shape of #370 and #373.
  3. No `services:` image uses a floating tag.  #377's shape: a required check
     running on whatever the registry served that morning.
  4. Every `--filter ancestor=<ref>` names a reference that is also a
     `services.*.image` in the same file.  Pinning `image:` alone silently
     breaks the lookup, because the filter matches on the pull reference.

Design note, since it is the whole point of the exercise: this script FAILS on
anything it cannot analyse rather than passing.  A matrix it cannot expand, an
expression it cannot resolve, a workflow it cannot parse -- all are errors.  A
guard that quietly skips the case it does not understand is the thing it was
written to prevent.

Usage:
    scripts/check-workflow-guards.py                     # the four guards
    scripts/check-workflow-guards.py --verify-against-api  # needs an admin token
"""

from __future__ import annotations

import argparse
import glob
import itertools
import json
import re
import subprocess
import sys

import yaml

WORKFLOW_GLOB = (".github/workflows/*.yml", ".github/workflows/*.yaml")
REQUIRED_CHECKS_FILE = ".github/required-checks.txt"
BRANCH = "develop"

# `${{ matrix.go-version }}` or `${{ matrix.package.name }}`
MATRIX_REF = re.compile(r"\$\{\{\s*matrix\.([A-Za-z0-9_.\-]+)\s*\}\}")
ANY_EXPRESSION = re.compile(r"\$\{\{")
# `ancestor=foo`, `ancestor=foo"`, `ancestor=foo'`, `ancestor=foo)`
ANCESTOR_REF = re.compile(r"ancestor=([^\s\"')]+)")


class Unexpandable(Exception):
    """A job name that cannot be resolved to concrete strings."""


def workflow_files() -> list[str]:
    files: list[str] = []
    for pattern in WORKFLOW_GLOB:
        files.extend(glob.glob(pattern))
    return sorted(files)


def load(path: str) -> dict:
    with open(path) as handle:
        doc = yaml.safe_load(handle)
    if not isinstance(doc, dict) or "jobs" not in doc:
        raise Unexpandable(f"{path}: parsed but has no top-level 'jobs:' key")
    return doc


def matrix_combinations(matrix: object, where: str) -> list[dict]:
    """Every concrete matrix combination, or raise if that cannot be known."""
    if matrix is None:
        return [{}]
    if not isinstance(matrix, dict):
        raise Unexpandable(f"{where}: strategy.matrix is not a mapping")
    for key in ("include", "exclude"):
        if key in matrix:
            raise Unexpandable(
                f"{where}: strategy.matrix uses '{key}', which this guard does "
                f"not expand. Teach it, or the guard is weaker than it looks."
            )
    keys = sorted(matrix)
    value_lists = []
    for key in keys:
        values = matrix[key]
        if not isinstance(values, list):
            raise Unexpandable(f"{where}: strategy.matrix.{key} is not a list")
        if ANY_EXPRESSION.search(str(values)):
            raise Unexpandable(
                f"{where}: strategy.matrix.{key} contains an expression, so the "
                f"job names it produces are not knowable from the file alone."
            )
        value_lists.append(values)
    return [dict(zip(keys, combo)) for combo in itertools.product(*value_lists)]


def substitute(name: str, combo: dict, where: str) -> str:
    def replace(match: re.Match) -> str:
        path = match.group(1).split(".")
        value = combo
        for part in path:
            if not isinstance(value, dict) or part not in value:
                raise Unexpandable(
                    f"{where}: name references matrix.{match.group(1)}, which "
                    f"the matrix does not define"
                )
            value = value[part]
        return str(value)

    resolved = MATRIX_REF.sub(replace, name)
    if ANY_EXPRESSION.search(resolved):
        raise Unexpandable(
            f"{where}: job name {name!r} still contains an expression after "
            f"matrix expansion, so this guard cannot tell what it is called"
        )
    return resolved


def job_display_names(path: str, job_id: str, job: dict) -> list[str]:
    """The concrete check-run names a job produces."""
    where = f"{path}:{job_id}"
    if not isinstance(job, dict):
        raise Unexpandable(f"{where}: job is not a mapping")
    name = job.get("name", job_id)
    matrix = (job.get("strategy") or {}).get("matrix")
    return [substitute(str(name), combo, where) for combo in matrix_combinations(matrix, where)]


def collect_jobs(errors: list[str]) -> dict[str, tuple[str, str, dict]]:
    """display name -> (workflow path, job id, job body)."""
    jobs: dict[str, tuple[str, str, dict]] = {}
    for path in workflow_files():
        try:
            doc = load(path)
        except Unexpandable as exc:
            errors.append(str(exc))
            continue
        except yaml.YAMLError as exc:
            errors.append(f"{path}: unparseable: {exc}")
            continue
        for job_id, job in (doc.get("jobs") or {}).items():
            try:
                names = job_display_names(path, job_id, job)
            except Unexpandable as exc:
                errors.append(str(exc))
                continue
            for name in names:
                jobs[name] = (path, job_id, job)
    return jobs


def read_required() -> list[str]:
    contexts = []
    with open(REQUIRED_CHECKS_FILE) as handle:
        for line in handle:
            line = line.strip()
            if line and not line.startswith("#"):
                contexts.append(line)
    return contexts


def service_images() -> dict[str, set[str]]:
    """workflow path -> the set of `services.*.image` values in it."""
    images: dict[str, set[str]] = {}
    for path in workflow_files():
        found: set[str] = set()
        try:
            doc = load(path)
        except (Unexpandable, yaml.YAMLError):
            continue  # already reported by collect_jobs
        for job in (doc.get("jobs") or {}).values():
            if not isinstance(job, dict):
                continue
            for service in (job.get("services") or {}).values():
                if isinstance(service, dict) and service.get("image"):
                    found.add(str(service["image"]))
        images[path] = found
    return images


def is_floating(ref: str) -> str | None:
    """Return the reason `ref` is a floating reference, or None if it is pinned."""
    if "@sha256:" in ref:
        return None
    last = ref.rsplit("/", 1)[-1]  # so a registry:port prefix is not read as a tag
    if ":" not in last:
        return "no tag at all, which Docker resolves as :latest"
    tag = last.rsplit(":", 1)[1]
    if tag == "latest":
        return "tag is :latest"
    if tag.endswith("-latest"):
        return f"tag {tag!r} ends in -latest, so it moves"
    return None


def guard_required_contexts_resolve(jobs, required, errors) -> None:
    known = set(jobs)
    for context in required:
        if context not in known:
            errors.append(
                f"{REQUIRED_CHECKS_FILE}: required context {context!r} does not "
                f"match any job in .github/workflows/. A required context naming "
                f"a job that does not exist blocks every PR forever."
            )


def guard_no_continue_on_error(jobs, required, errors) -> None:
    """Neither the job nor any of its steps may waive failure.

    Job level is #370's shape. Step level is the same defect one level down,
    and it is not hypothetical: when this guard was written, the required
    `Lint` job ran Ruff and ShellCheck with `continue-on-error: true`, so
    neither could ever fail a build. Both were passing, so the waiver was
    buying nothing except the inability to report.
    """
    for context in required:
        if context not in jobs:
            continue  # already reported by guard 1
        path, job_id, job = jobs[context]
        value = job.get("continue-on-error")
        if value not in (None, False):
            errors.append(
                f"{path}: job {job_id!r} ({context!r}) is a required status check "
                f"and sets continue-on-error: {value!r}. A required check that "
                f"cannot report failure is not a check."
            )
        for index, step in enumerate(job.get("steps") or []):
            if not isinstance(step, dict):
                continue
            value = step.get("continue-on-error")
            if value not in (None, False):
                label = step.get("name") or step.get("uses") or f"step {index}"
                errors.append(
                    f"{path}: step {label!r} of required job {context!r} sets "
                    f"continue-on-error: {value!r}, so it cannot fail the check "
                    f"it appears to be part of. Either let it fail, or move it "
                    f"to a job that is not required."
                )


def guard_no_floating_service_images(errors) -> None:
    for path, images in service_images().items():
        for ref in sorted(images):
            reason = is_floating(ref)
            if reason:
                errors.append(
                    f"{path}: service image {ref!r} is not pinned -- {reason}. "
                    f"Pin it by digest; see tier1-gate.yml's mssql service."
                )


def run_scripts(doc: dict):
    """Every `run:` body in a workflow, so prose in YAML comments is not scanned.

    Scanning the raw file text instead would flag the worked example inside
    tier1-gate.yml's own comment explaining this guard, which is a good
    illustration of why a text-level check is the wrong tool here.
    """
    for job in (doc.get("jobs") or {}).values():
        if not isinstance(job, dict):
            continue
        for step in job.get("steps") or []:
            if isinstance(step, dict) and isinstance(step.get("run"), str):
                yield step["run"]


def guard_ancestor_filters_match_images(errors) -> None:
    images = service_images()
    for path in workflow_files():
        try:
            doc = load(path)
        except (Unexpandable, yaml.YAMLError):
            continue  # already reported by collect_jobs
        refs: set[str] = set()
        for script in run_scripts(doc):
            refs.update(ANCESTOR_REF.findall(script))
        for ref in sorted(refs):
            if ref not in images.get(path, set()):
                errors.append(
                    f"{path}: `--filter ancestor={ref}` names a reference that is "
                    f"not a services.*.image in this file. That filter matches on "
                    f"the pull reference, so the lookup will find nothing and "
                    f"`docker exec` will get an empty container ID."
                )


def verify_against_api() -> int:
    """Compare the checked-in list to branch protection. Needs an admin token."""
    try:
        raw = subprocess.run(
            [
                "gh", "api",
                f"repos/cleat-team/cleat/branches/{BRANCH}/protection/required_status_checks",
                "--jq", ".contexts[]",
            ],
            capture_output=True, text=True, check=True,
        ).stdout
    except (subprocess.CalledProcessError, FileNotFoundError) as exc:
        detail = getattr(exc, "stderr", "") or exc
        print(f"cannot read branch protection (this needs an admin token): {detail}")
        return 1

    live = sorted(line for line in raw.splitlines() if line.strip())
    checked_in = sorted(read_required())
    only_live = sorted(set(live) - set(checked_in))
    only_file = sorted(set(checked_in) - set(live))
    for context in only_live:
        print(f"::error::required on {BRANCH} but missing from {REQUIRED_CHECKS_FILE}: {context!r}")
    for context in only_file:
        print(f"::error::in {REQUIRED_CHECKS_FILE} but not required on {BRANCH}: {context!r}")
    if only_live or only_file:
        return 1
    print(f"{len(live)} required contexts, and {REQUIRED_CHECKS_FILE} matches exactly")
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--verify-against-api",
        action="store_true",
        help="compare .github/required-checks.txt to live branch protection (needs admin)",
    )
    args = parser.parse_args()

    if args.verify_against_api:
        return verify_against_api()

    files = workflow_files()
    if not files:
        print("::error::no workflow files found -- this guard would pass vacuously")
        return 1

    errors: list[str] = []
    jobs = collect_jobs(errors)
    required = read_required()
    if not required:
        print(f"::error::{REQUIRED_CHECKS_FILE} lists no contexts -- guards 1 and 2 would pass vacuously")
        return 1

    guard_required_contexts_resolve(jobs, required, errors)
    guard_no_continue_on_error(jobs, required, errors)
    guard_no_floating_service_images(errors)
    guard_ancestor_filters_match_images(errors)

    for error in errors:
        print(f"::error title=Workflow integrity::{error}")

    print(
        f"checked {len(files)} workflow files, {len(jobs)} distinct job names, "
        f"{len(required)} required contexts, {len(errors)} problems"
    )
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
