#!/usr/bin/env python3
"""Structural guards over .github/workflows/, run by the Lint job.

Six checks, each catching a *class* of defect that has already cost this repo
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
  5. Every `mssql/server` service sets `MSSQL_MEMORY_LIMIT_MB`.  SQL Server on
     Linux sizes its buffer pool to 80% of VISIBLE host memory by default,
     with no awareness that it shares a runner with Postgres, MySQL and
     whatever else the job also starts (cleat#2101).  A service missing the
     cap does not fail loudly -- it runs fine alone and contends for memory
     only once the runner is busy.
  6. No `run:` or `actions/github-script` `script:` in a workflow triggered by
     one of PRIVILEGED_TRIGGERS (`workflow_run`, `pull_request_target`,
     `issue_comment`, `issues`, `pull_request_review`,
     `pull_request_review_comment`, `discussion`, `discussion_comment`)
     splices ANY `${{ }}` expression directly into its text, unless that
     expression is one of a short safe list (SAFE_EXPRESSION_BODIES:
     github.sha/run_id/run_attempt/repository/workflow, runner.*).  Every one
     of those triggers can execute in the base repository's trusted context on
     content authored by someone who is not a committer, so a spliced
     expression becomes part of the shell or script TEXT itself before
     anything parses it -- not data handed to an already-running program.
     cleat#2150 found exactly this shape in review (`workflow_run`'s
     `head_branch`, a fork-controlled string, headed for a `run:` block) and
     #2309 fixed it by routing everything through `env:` instead. This is an
     ALLOWLIST rather than a denylist of dangerous patterns on purpose: this
     guard's own first version denylisted four named shapes and cleat-review
     broke it with a fifth on the guard's own PR -- `${{ env.X }}` where X was
     itself assigned from event data one step earlier, which is still a
     YAML-level splice into the run: text and not a shell variable read
     despite looking like the fix. See SAFE_EXPRESSION_BODIES' doc comment for
     the rest. cleat#2310.

Design note, since it is the whole point of the exercise: this script FAILS on
anything it cannot analyse rather than passing.  A matrix it cannot expand, an
expression it cannot resolve, a workflow it cannot parse -- all are errors.  A
guard that quietly skips the case it does not understand is the thing it was
written to prevent.

Usage:
    scripts/check-workflow-guards.py                       # the six guards
    scripts/check-workflow-guards.py --verify-against-api  # needs an admin token
    scripts/check-workflow-guards.py --self-test            # known-positive/negative pairs
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
    """Return the reason `ref` is a floating reference, or None if it is pinned.

    A DIGEST IS NOW THE ONLY THING THAT COUNTS AS PINNED, and that is a
    tightening made on 2026-08-08 after this function missed a real one.

    It used to reject `:latest` and tags ending in `-latest` and accept
    everything else. So `postgres:16` and `mysql:8.4` both passed it -- and
    both move. `postgres:16` walked from 16.0 to 16.14 over the life of this
    repo while every required check that touches a database resolved whatever
    the registry served that morning. That is the same defect the guard was
    written for (#377, a `2022-latest` SQL Server tag), one level less obvious:
    a major-only tag is a moving tag that happens not to say so in its name.

    Enumerating the moving-tag *spellings* was always the wrong shape. There is
    no rule over tag text that separates `8.4` (moves) from `8.4.11` (does
    not) without knowing the upstream's versioning policy, and even a full
    `16.14` is only immutable by the publisher's convention -- Docker Hub tags
    are mutable by design and can be repointed. The property actually wanted is
    "this resolves to the same bytes every time", and only a digest gives it.

    Every `services:` image in this repo satisfies this today, so the rule
    costs nothing to hold. The readable `name:tag@sha256:...` form is
    encouraged -- Docker resolves by digest and ignores the tag, so the tag is
    there for the reader -- but the digest is what is checked.
    """
    if "@sha256:" in ref:
        return None
    last = ref.rsplit("/", 1)[-1]  # so a registry:port prefix is not read as a tag
    if ":" not in last:
        return "no tag at all, which Docker resolves as :latest"
    tag = last.rsplit(":", 1)[1]
    return (
        f"tag {tag!r} is not a digest. Docker Hub tags are mutable, so a tag "
        f"pins nothing -- `postgres:16` moved from 16.0 to 16.14 under this "
        f"repo's required checks. Pin it: "
        f"`docker buildx imagetools inspect <ref>` prints the digest, and "
        f"`<name>:<version>@sha256:<digest>` keeps it readable"
    )


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


MSSQL_IMAGE_MARKER = "mssql/server"


def guard_mssql_services_have_memory_cap(errors) -> None:
    """Every `mssql/server` service sets `MSSQL_MEMORY_LIMIT_MB`.

    SQL Server on Linux sizes its buffer pool to 80% of VISIBLE host memory by
    default, with no awareness that it is one of several DB containers a job
    starts on a shared runner (cleat#2101). A service missing the cap is not
    one that fails loudly: it runs fine alone in isolation and only contends
    for memory once the runner is under load from everything else in the job.
    """
    for path in workflow_files():
        try:
            doc = load(path)
        except (Unexpandable, yaml.YAMLError):
            continue  # already reported by collect_jobs
        for job_id, job in (doc.get("jobs") or {}).items():
            if not isinstance(job, dict):
                continue
            for service_id, service in (job.get("services") or {}).items():
                if not isinstance(service, dict):
                    continue
                image = str(service.get("image") or "")
                if MSSQL_IMAGE_MARKER not in image:
                    continue
                env = service.get("env")
                if not isinstance(env, dict) or "MSSQL_MEMORY_LIMIT_MB" not in env:
                    errors.append(
                        f"{path}: job {job_id!r} service {service_id!r} "
                        f"({image}) has no MSSQL_MEMORY_LIMIT_MB. SQL Server "
                        f"sizes its buffer pool to 80% of visible host memory "
                        f"by default, which contends with every other "
                        f"service on a shared runner (cleat#2101)."
                    )


# cleat#2310. Every one of these triggers can execute in the base
# repository's context -- with its secrets and its write token -- on content
# authored by someone who is not a committer: workflow_run and
# pull_request_target for a fork's pull request; issue_comment, issues,
# pull_request_review and pull_request_review_comment for a comment, issue or
# review body anyone with read access can write; discussion and
# discussion_comment the same way, if discussions are enabled. A plain
# `pull_request` workflow from a fork gets a read-only token and no secrets,
# so the same splice there is a much smaller hole; this guard is deliberately
# scoped to the triggers where it is a real one.
#
# cleat-review found the first version of this set too narrow (workflow_run
# and pull_request_target only) reviewing cleat#2310's own PR: cla-assistant.yml
# runs on issue_comment today and was entirely unscanned. discussion,
# discussion_comment and pull_request_review are not used by anything in this
# repo yet, added anyway on the same reasoning -- the identical trust shape,
# at zero cost against the real tree, closes the hole before it needs finding
# twice.
PRIVILEGED_TRIGGERS = {
    "workflow_run",
    "pull_request_target",
    "issue_comment",
    "issues",
    "pull_request_review",
    "pull_request_review_comment",
    "discussion",
    "discussion_comment",
}

# Every `${{ ... }}` in a `run:` or `actions/github-script` `script:` body, not
# anchored to the start -- an untrusted reference wrapped in `fromJSON(...)` or
# combined with `&&`/`||` is exactly as dangerous, and anchoring would miss it
# the same way a call-site-only SQL scan missed queries reached through a
# struct literal (CLAUDE.md's "pair first, filter after").
EXPRESSION_BODY = re.compile(r"\$\{\{(.*?)\}\}", re.DOTALL)

# ALLOWLIST, NOT A DENYLIST -- an expression in a privileged run:/script: body
# is a violation unless its ENTIRE body (after stripping whitespace) is one of
# these, matched exactly rather than by substring.
#
# This file's first version denied four named patterns
# (github.event.*/github.head_ref/steps.*.outputs.*/needs.*.outputs.*) and let
# everything else through. cleat-review broke it on cleat#2310's own PR with
# four cases that pattern could not see, none of them exotic:
#
#   - the env: HOP: `env: {B: ${{ github.event.workflow_run.head_branch }}}`
#     then `run: echo ${{ env.B }}` -- the untrusted value is one indirection
#     away from the pattern, but `${{ env.B }}` is STILL a YAML-level splice
#     into the run: TEXT, happening before the shell ever sees it, so it is
#     exactly the same injection with an extra hop -- and it is the shape
#     someone reaches for FIRST trying to satisfy a denylisted guard, since it
#     looks like the fix (env:) without being one (the value must be read as
#     $B, a real shell/env variable, never as ${{ env.B }}, which is still an
#     Actions-level substitution);
#   - `${{ toJSON(github.event) }}` -- no trailing dot after `github.event`,
#     so `github\.event\.` does not match it, and it dumps the entire event
#     payload;
#   - `${{ github['event']['workflow_run']['head_branch'] }}` -- Actions
#     expressions support index syntax as an alternative to dot notation, and
#     none of the four patterns matched a bracket;
#   - `${{ github.event.comment.body }}` on an issue_comment-triggered
#     workflow WOULD have matched `github\.event\.` -- the miss there was in
#     PRIVILEGED_TRIGGERS, not the expression pattern, and is fixed above.
#
# A denylist has to name every shape an attacker can reach the same value
# through, and expression languages have more than one grammar for "read a
# nested field" -- this is the same trap CLAUDE.md's "Build" section names for
# a text search generally: enumerating what is UNSAFE is an open set; what is
# SAFE, for what this guard needs, is short and closed. Nothing in either real
# privileged workflow needs anything beyond this list -- both already route
# everything else through `env:`.
SAFE_EXPRESSION_BODIES = {
    "github.sha",
    "github.run_id",
    "github.run_attempt",
    "github.repository",
    "github.workflow",
}
SAFE_RUNNER_PROPERTY = re.compile(r"^runner\.[A-Za-z_]+$")


def is_safe_expression(body: str) -> bool:
    body = body.strip()
    return body in SAFE_EXPRESSION_BODIES or bool(SAFE_RUNNER_PROPERTY.match(body))


# (workflow path, job id, step index) -> reason a human has read the spliced
# expression and judged it safe despite not being on the list above. Empty
# today: neither privileged workflow in this repo needs one (see the guard's
# own audit, cleat#2310's acceptance criteria). Add an entry only after
# reading the specific expression, the same discipline as
# eventHistoryInsertSites in the encoder-routing guard this docstring
# references -- a name and a reason next to the site, not a blanket waiver.
EXPRESSION_ALLOWLIST: dict[tuple[str, str, int], str] = {}


def workflow_triggers(doc: dict) -> set[str]:
    """The top-level `on:` trigger names, in whichever of its three YAML
    shapes this file uses: a bare string, a list of strings, or a mapping.

    PyYAML's default (YAML 1.1) resolver reads the bare scalar `on` as the
    boolean True, not the string "on" -- confirmed against every real
    workflow file in this repo, where the key is always spelled bare. So
    `doc.get("on")` is None on every one of them, and this guard would pass
    vacuously everywhere -- caught by this file's own self-test before it
    ever ran against a real workflow (its first two cases both came back
    empty). `doc.get(True)` is the same key PyYAML actually produced.
    """
    on = doc.get("on")
    if on is None:
        on = doc.get(True)
    if isinstance(on, str):
        return {on}
    if isinstance(on, list):
        return {str(item) for item in on}
    if isinstance(on, dict):
        return {str(key) for key in on}
    return set()


def privileged_scripts(doc: dict):
    """(job_id, step_index, kind, text) for every `run:` or github-script
    `script:` body in doc, kind being "run" or "script" for the message."""
    for job_id, job in (doc.get("jobs") or {}).items():
        if not isinstance(job, dict):
            continue
        for index, step in enumerate(job.get("steps") or []):
            if not isinstance(step, dict):
                continue
            run = step.get("run")
            if isinstance(run, str):
                yield job_id, index, "run", run
            uses = str(step.get("uses") or "").split("@", 1)[0]
            if uses == "actions/github-script":
                script = (step.get("with") or {}).get("script")
                if isinstance(script, str):
                    yield job_id, index, "script", script


def find_privileged_expression_violations(path: str, doc: dict) -> list[str]:
    """The testable core of guard 6: given one workflow's parsed YAML, return
    one violation string per `${{ }}` expression spliced into a run:/script:
    body of a privileged (PRIVILEGED_TRIGGERS) workflow, unless the whole
    expression is on SAFE_EXPRESSION_BODIES or the site is on
    EXPRESSION_ALLOWLIST."""
    triggers = workflow_triggers(doc) & PRIVILEGED_TRIGGERS
    if not triggers:
        return []
    violations = []
    for job_id, index, kind, text in privileged_scripts(doc):
        for match in EXPRESSION_BODY.finditer(text):
            body = match.group(1).strip()
            if is_safe_expression(body):
                continue
            reason = EXPRESSION_ALLOWLIST.get((path, job_id, index))
            if reason is not None:
                continue
            violations.append(
                f"{path}: job {job_id!r} step {index} ({kind}) splices "
                f"`${{{{ {body} }}}}` directly into its {kind} text. This "
                f"workflow is triggered by {'/'.join(sorted(triggers))}, "
                f"which runs in the base repository's trusted context on "
                f"content someone other than a committer can write, so any "
                f"expression spliced here becomes part of the "
                f"{'shell' if kind == 'run' else 'script'} TEXT itself before "
                f"anything parses it -- not data handed to an already-running "
                f"program. Only a short list of provably constant-per-run "
                f"values (SAFE_EXPRESSION_BODIES: github.sha, github.run_id, "
                f"github.run_attempt, github.repository, github.workflow, "
                f"runner.*) may appear here directly; everything else, "
                f"INCLUDING an env: value that itself came from the event "
                f"(`${{{{ env.X }}}}` is still an Actions-level splice into "
                f"this text, not a shell variable read), must be assigned to "
                f"`env:` and read back as a real shell/env variable ($X, or "
                f"`process.env.X`/`context.*` inside a github-script step) -- "
                f"see .github/workflows/tier1-push-failure-notifier.yml's "
                f"'Quiet on green' step. If this specific splice has been "
                f"read and judged safe, add "
                f"({path!r}, {job_id!r}, {index}) to EXPRESSION_ALLOWLIST "
                f"with a reason."
            )
    return violations


def guard_no_untrusted_expressions_in_privileged_workflows(errors) -> None:
    for path in workflow_files():
        try:
            doc = load(path)
        except (Unexpandable, yaml.YAMLError):
            continue  # already reported by collect_jobs
        errors.extend(find_privileged_expression_violations(path, doc))


def self_test() -> int:
    """Known-positive/known-negative pairs for guard 6 -- see CLAUDE.md's
    known-positive discipline: a check that has never been shown capable of
    failing is not yet a check."""
    failures: list[str] = []
    cases = 0

    def check(label: str, yaml_text: str, want: list[str]):
        nonlocal cases
        cases += 1
        doc = yaml.safe_load(yaml_text)
        got = find_privileged_expression_violations("workflow.yml", doc)
        stripped = [g.split(". This workflow is triggered by")[0] for g in got]  # ignore the boilerplate
        if stripped != want:
            failures.append(f"{label}: got {stripped}, want {want}")

    # KNOWN-POSITIVE: cleat#2310's own acceptance criterion -- the exact shape
    # cleat#2150 found in review of #2309, before it ever ran.
    check(
        "planted github.event.workflow_run.head_branch in run:, workflow_run trigger",
        """
        on:
          workflow_run:
            workflows: ["Tier 1 Gate"]
            types: [completed]
        jobs:
          notify:
            runs-on: ubuntu-latest
            steps:
              - run: echo "${{ github.event.workflow_run.head_branch }}"
        """,
        ["workflow.yml: job 'notify' step 0 (run) splices "
         "`${{ github.event.workflow_run.head_branch }}` directly into its run text"],
    )

    # KNOWN-NEGATIVE: the fix for the case above -- the value goes through
    # env: and the run: body only ever refers to the shell variable it names.
    # This is cleat#2310's other acceptance criterion, and it is the actual
    # shape of tier1-push-failure-notifier.yml's "Quiet on green" step today.
    check(
        "same value routed through env: and read as a shell variable, workflow_run trigger",
        """
        on:
          workflow_run:
            workflows: ["Tier 1 Gate"]
            types: [completed]
        jobs:
          notify:
            runs-on: ubuntu-latest
            steps:
              - env:
                  HEAD_BRANCH: ${{ github.event.workflow_run.head_branch }}
                run: echo "$HEAD_BRANCH"
        """,
        [],
    )

    # KNOWN-NEGATIVE: the identical splice, but the workflow has no privileged
    # trigger. A `pull_request` workflow from a fork gets a read-only token
    # and no secrets, so this is a much smaller hole and out of this guard's
    # stated scope -- proves the guard is trigger-conditioned, not a blanket
    # ban on `${{ github.event.* }}` in run: everywhere.
    check(
        "same splice, pull_request trigger only -- not privileged",
        """
        on: pull_request
        jobs:
          build:
            runs-on: ubuntu-latest
            steps:
              - run: echo "${{ github.event.pull_request.title }}"
        """,
        [],
    )

    # KNOWN-POSITIVE: needs.*.outputs.*, inside an actions/github-script
    # script: rather than run:, and pull_request_target rather than
    # workflow_run -- both listed in the issue, neither exercised above.
    check(
        "needs.*.outputs.* in an actions/github-script script:, pull_request_target trigger",
        """
        on:
          pull_request_target:
            types: [opened]
        jobs:
          comment:
            runs-on: ubuntu-latest
            steps:
              - uses: actions/github-script@v9
                with:
                  script: |
                    core.notice("${{ needs.build.outputs.summary }}");
        """,
        ["workflow.yml: job 'comment' step 0 (script) splices "
         "`${{ needs.build.outputs.summary }}` directly into its script text"],
    )

    # KNOWN-POSITIVE: steps.*.outputs.*, wrapped in fromJSON() to confirm the
    # body scan is not anchored to the start of the expression
    # (EXPRESSION_BODY's own doc comment).
    check(
        "steps.*.outputs.* wrapped in fromJSON(), workflow_run trigger",
        """
        on:
          workflow_run:
            workflows: ["Tier 1 Gate"]
            types: [completed]
        jobs:
          notify:
            runs-on: ubuntu-latest
            steps:
              - id: resolve
                run: echo done
              - run: echo "${{ fromJSON(steps.resolve.outputs.payload).branch }}"
        """,
        ["workflow.yml: job 'notify' step 1 (run) splices "
         "`${{ fromJSON(steps.resolve.outputs.payload).branch }}` directly into its run text"],
    )

    # KNOWN-POSITIVE (cleat-review, reviewing this guard's first version on
    # cleat#2310's own PR): the env: HOP. B is assigned from event data one
    # step earlier, then read back as `${{ env.B }}` rather than `$B` -- still
    # an Actions-level splice into the run: text, not a shell variable read,
    # and it is the shape someone reaches for FIRST trying to satisfy a
    # denylisted version of this guard, because it looks like the fix.
    check(
        "env: hop -- env value itself set from event data, then spliced as ${{ env.X }}",
        """
        on:
          workflow_run:
            workflows: ["Tier 1 Gate"]
            types: [completed]
        jobs:
          notify:
            runs-on: ubuntu-latest
            steps:
              - env:
                  B: ${{ github.event.workflow_run.head_branch }}
                run: echo ${{ env.B }}
        """,
        ["workflow.yml: job 'notify' step 0 (run) splices `${{ env.B }}` directly into its run text"],
    )

    # KNOWN-POSITIVE (cleat-review): toJSON(github.event) has no trailing dot
    # after `github.event`, so a denylist pattern anchored on `github\.event\.`
    # cannot see it -- and it dumps the entire event payload, comment bodies
    # and all.
    check(
        "toJSON(github.event) -- no trailing dot, dumps the whole payload",
        """
        on:
          pull_request_target:
            types: [opened]
        jobs:
          build:
            runs-on: ubuntu-latest
            steps:
              - run: echo '${{ toJSON(github.event) }}'
        """,
        ["workflow.yml: job 'build' step 0 (run) splices "
         "`${{ toJSON(github.event) }}` directly into its run text"],
    )

    # KNOWN-POSITIVE (cleat-review): Actions expressions support index syntax
    # as an alternative to dot notation for the identical field.
    check(
        "bracket/index syntax for the same field a dot-notation denylist would have caught",
        """
        on:
          workflow_run:
            workflows: ["Tier 1 Gate"]
            types: [completed]
        jobs:
          notify:
            runs-on: ubuntu-latest
            steps:
              - run: echo "${{ github['event']['workflow_run']['head_branch'] }}"
        """,
        ["workflow.yml: job 'notify' step 0 (run) splices "
         "`${{ github['event']['workflow_run']['head_branch'] }}` directly into its run text"],
    )

    # KNOWN-POSITIVE (cleat-review and coordinator): issue_comment is
    # privileged too -- a comment body is authored by anyone with read access,
    # and the workflow runs with the base repo's write token. This is
    # cla-assistant.yml's own trigger, unscanned by this guard's first
    # version because PRIVILEGED_TRIGGERS only named workflow_run and
    # pull_request_target.
    check(
        "github.event.comment.body on an issue_comment trigger",
        """
        on:
          issue_comment:
            types: [created]
        jobs:
          react:
            runs-on: ubuntu-latest
            steps:
              - run: echo "${{ github.event.comment.body }}"
        """,
        ["workflow.yml: job 'react' step 0 (run) splices "
         "`${{ github.event.comment.body }}` directly into its run text"],
    )

    # KNOWN-NEGATIVE: SAFE_EXPRESSION_BODIES and runner.* are the only
    # expressions this guard allows directly in a privileged run:/script:, and
    # they must not be flagged or this guard would fail on ordinary, safe
    # workflows and nobody would keep it green. secrets.* is NOT on that
    # list -- it goes through env: like everything else, which is also
    # exercised here.
    check(
        "SAFE_EXPRESSION_BODIES, runner.*, and secrets.* via env: are not flagged",
        """
        on:
          workflow_run:
            workflows: ["Tier 1 Gate"]
            types: [completed]
        jobs:
          build:
            runs-on: ubuntu-latest
            steps:
              - env:
                  TOKEN: ${{ secrets.GITHUB_TOKEN }}
                run: >-
                  echo "${{ github.sha }} ${{ github.run_id }} ${{ github.run_attempt }}
                  ${{ github.repository }} ${{ github.workflow }} ${{ runner.os }}"
        """,
        [],
    )

    # KNOWN-NEGATIVE: a trusted expression used OUTSIDE run:/script: -- in an
    # `if:` condition -- is never spliced into shell or script text at all; it
    # is evaluated by the Actions runner itself. Proves this guard is scoped
    # to the two vectors named in cleat#2310, not a blanket ban on
    # `${{ github.event.* }}` anywhere in a privileged workflow.
    check(
        "github.event.* in an if: condition is out of scope, not a run:/script: splice",
        """
        on:
          pull_request_target:
            types: [opened]
        jobs:
          build:
            runs-on: ubuntu-latest
            steps:
              - if: github.event.pull_request.draft == false
                run: echo ok
        """,
        [],
    )

    if failures:
        for f in failures:
            print(f"SELF-TEST FAILED: {f}")
        return 1
    print(f"self-test passed: {cases} cases")
    return 0


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
    parser.add_argument(
        "--self-test",
        action="store_true",
        help="run known-positive/known-negative cases for guard 6 against synthetic YAML, not the real tree",
    )
    args = parser.parse_args()

    if args.self_test:
        return self_test()

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
    guard_mssql_services_have_memory_cap(errors)
    guard_no_untrusted_expressions_in_privileged_workflows(errors)

    for error in errors:
        print(f"::error title=Workflow integrity::{error}")

    print(
        f"checked {len(files)} workflow files, {len(jobs)} distinct job names, "
        f"{len(required)} required contexts, {len(errors)} problems"
    )
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
