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
     `workflow_run` or `pull_request_target` splices `github.event.*`,
     `github.head_ref`, `steps.*.outputs.*` or `needs.*.outputs.*` directly
     into its text via `${{ }}`.  Both triggers execute in the base
     repository's trusted context even when a fork's pull request is what
     triggered them, so a value that traces back to the triggering event,
     spliced this way, becomes part of the shell or script TEXT itself before
     anything parses it -- not data handed to an already-running program.
     cleat#2150 found exactly this shape in review (`workflow_run`'s
     `head_branch`, a fork-controlled string, headed for a `run:` block) and
     #2309 fixed it by routing everything through `env:` instead. cleat#2310.

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


# cleat#2310. workflow_run and pull_request_target both execute in the base
# repository's context -- with its secrets and its write token -- even when a
# fork's pull request is what triggered them. A `pull_request` workflow from a
# fork gets a read-only token and no secrets, so the same splice there is a
# much smaller hole; this guard is deliberately scoped to the two triggers
# where it is a real one.
PRIVILEGED_TRIGGERS = {"workflow_run", "pull_request_target"}

# Every `${{ ... }}` in a `run:` or `actions/github-script` `script:` body, not
# anchored to the start -- an untrusted reference wrapped in `fromJSON(...)` or
# combined with `&&`/`||` is exactly as dangerous, and anchoring would miss it
# the same way a call-site-only SQL scan missed queries reached through a
# struct literal (CLAUDE.md's "pair first, filter after").
EXPRESSION_BODY = re.compile(r"\$\{\{(.*?)\}\}", re.DOTALL)

# What makes a `${{ }}` body untrusted: it traces back to the event that
# triggered this run, which a fork's pull request controls. Matched by
# substring within the body, not required to start it, for the same reason
# EXPRESSION_BODY is not anchored.
#
# steps.*.outputs.* and needs.*.outputs.* are flagged WHOLESALE, not only the
# specific outputs that provably derive from event data -- the issue this
# guard exists for (cleat#2310, quoting cleat#2150's review of #2309) asks for
# outputs "that derive from the event", but telling those apart from a step's
# other, harmless outputs needs real interprocedural analysis: does this
# specific output's assignment read `context.payload` or `github.event`
# anywhere upstream. That is the same "condition that never decides anything"
# risk the tree-scanning guard in
# engine/every_event_history_write_routes_through_the_encoder_test.go's own
# doc comment explains for call-graph reachability -- being subtly wrong about
# which output is safe is worse than flagging one that is not, and the fixed
# example this guard is modeled on
# (.github/workflows/tier1-push-failure-notifier.yml's "Quiet on green" step)
# already routes every step output through `env:` uniformly, including ones
# that do not themselves derive from the event. This guard enforces that same
# uniform discipline rather than trying to out-think it per output.
UNTRUSTED_EXPRESSION_PATTERNS = (
    re.compile(r"github\.event\."),
    re.compile(r"github\.head_ref\b"),
    re.compile(r"steps\.[A-Za-z0-9_-]+\.outputs\."),
    re.compile(r"needs\.[A-Za-z0-9_-]+\.outputs\."),
)

# (workflow path, job id, step index) -> reason a human has read the spliced
# expression and judged it safe despite matching a pattern above. Empty today:
# neither privileged workflow in this repo needs one (see the guard's own
# audit, cleat#2310's acceptance criteria). Add an entry only after reading
# the specific expression, the same discipline as
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
    one violation string per untrusted expression spliced into a run:/script:
    body of a workflow_run or pull_request_target workflow."""
    triggers = workflow_triggers(doc) & PRIVILEGED_TRIGGERS
    if not triggers:
        return []
    violations = []
    for job_id, index, kind, text in privileged_scripts(doc):
        for match in EXPRESSION_BODY.finditer(text):
            body = match.group(1).strip()
            hit = next((p for p in UNTRUSTED_EXPRESSION_PATTERNS if p.search(body)), None)
            if hit is None:
                continue
            reason = EXPRESSION_ALLOWLIST.get((path, job_id, index))
            if reason is not None:
                continue
            violations.append(
                f"{path}: job {job_id!r} step {index} ({kind}) splices "
                f"`${{{{ {body} }}}}` directly into its {kind} text -- matches "
                f"{hit.pattern!r}. This workflow is triggered by "
                f"{'/'.join(sorted(triggers))}, which runs in the base "
                f"repository's trusted context even for a fork's pull request, "
                f"so a value from the triggering event spliced here becomes "
                f"part of the {'shell' if kind == 'run' else 'script'} TEXT "
                f"itself before anything parses it. Route it through `env:` "
                f"(or read it from `context`/`process.env` inside a "
                f"github-script step), quoted as a variable -- see "
                f".github/workflows/tier1-push-failure-notifier.yml's "
                f"'Quiet on green' step. If this specific splice has been read "
                f"and judged safe, add "
                f"({path!r}, {job_id!r}, {index}) to EXPRESSION_ALLOWLIST with "
                f"a reason."
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
        stripped = [g.split(" -- matches ")[0] for g in got]  # ignore the pattern detail
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
        "same value routed through env:, workflow_run trigger",
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

    # KNOWN-POSITIVE: steps.*.outputs.*, the pattern needs.*.outputs.* above
    # does not exercise -- the same tier1-push-failure-notifier.yml shape,
    # unfixed, wrapped in fromJSON() to confirm the body scan is not anchored
    # to the start of the expression (EXPRESSION_BODY's own doc comment).
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

    # KNOWN-NEGATIVE: a trusted expression -- secrets, matrix, github.sha --
    # is not on the untrusted-pattern list and must not be flagged, or this
    # guard would fail on ordinary, safe workflows and nobody would keep it
    # green.
    check(
        "trusted expressions (secrets, matrix, github.sha) are not flagged",
        """
        on:
          workflow_run:
            workflows: ["Tier 1 Gate"]
            types: [completed]
        jobs:
          build:
            runs-on: ubuntu-latest
            strategy:
              matrix:
                go: ["1.25"]
            steps:
              - env:
                  TOKEN: ${{ secrets.GITHUB_TOKEN }}
                run: go build ./... && echo "${{ matrix.go }} ${{ github.sha }}"
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
