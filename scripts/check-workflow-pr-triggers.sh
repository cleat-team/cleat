#!/usr/bin/env bash
# A workflow that filters pull_request by BRANCH must also trigger on `edited`,
# or it never runs for a PR that was retargeted onto one of those branches.
#
# THE FAILURE THIS PREVENTS, measured in the sibling ports repo on 2026-09-07.
# A PR opened against a feature branch and later retargeted to main reported:
#
#     base              main
#     mergeStateStatus  CLEAN
#     checks            1     "Validate branch name"  pass
#
# Green, mergeable, and the test suite had never been queued. Two triggers have
# to miss for that:
#
#   1. `branches: [main, develop]` excludes the PR at OPEN time, because its
#      base was a feature branch then.
#   2. Retargeting fires the `edited` activity type, which is NOT in the default
#      set [opened, synchronize, reopened]. So the base change re-evaluates
#      nothing.
#
# A workflow with no `branches:` filter is unaffected by (1) and needs nothing
# here -- which is why cleat's branch-naming.yml and dco-check.yml ran, and
# supplied the green checks that made the PR look finished.
#
# WHY THIS CANNOT BE CAUGHT DOWNSTREAM. Checks that never registered are not
# pending -- they do not exist. `pending == 0` is trivially true over an empty
# set, and `CLEAN` means "no REQUIRED check is failing", which is also true when
# none was created. Every term of the obvious merge gate is satisfied by a PR
# that ran nothing. Only branch protection's named required-check list can tell
# the difference, and only at merge time.
#
# THE COST, stated rather than waved at. `edited` also fires on title and body
# edits, so an author who edits a body three times outside the previous run's
# window pays three full suites. cleat runs ~52 checks; three body edits by one
# author in a day is not hypothetical. The trade is a bounded, visible,
# recurring cost against an unbounded, invisible, one-off one -- a silently
# unrun suite can merge anything.
#
# Parsed with a real YAML parser rather than grep. CLAUDE.md's recurring defect
# is a tool applied to a format it does not model, and `on:` is nested mapping
# with two spellings for every list; a line-oriented read of it would be the
# fourth instance in that table.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

python3 - "$REPO_ROOT" <<'PY'
import sys, os, glob

try:
    import yaml
except ImportError:
    # Loudly. A guard that skips when its parser is missing is a guard that
    # passes on every machine that cannot run it.
    print("ERROR: PyYAML is not installed, so this guard cannot parse any "
          "workflow. Install it (pip install pyyaml) rather than skipping: a "
          "check that cannot run must not report success.", file=sys.stderr)
    sys.exit(2)

root = sys.argv[1]
paths = sorted(glob.glob(os.path.join(root, ".github", "workflows", "*.yml")) +
               glob.glob(os.path.join(root, ".github", "workflows", "*.yaml")))
if not paths:
    print("ERROR: no workflow files found -- this guard would pass no matter "
          "what the workflows said.", file=sys.stderr)
    sys.exit(2)

def as_list(v):
    if v is None:
        return []
    return v if isinstance(v, list) else [v]

checked, offenders = 0, []
for p in paths:
    with open(p) as fh:
        doc = yaml.safe_load(fh) or {}
    # `on` is parsed by YAML 1.1 as the boolean True. Accept both spellings
    # rather than assuming which one this parser produced.
    on = doc.get("on", doc.get(True))
    if not isinstance(on, dict):
        continue
    pr = on.get("pull_request")
    if not isinstance(pr, dict):
        continue
    if not as_list(pr.get("branches")):
        continue  # no branch filter: a retarget cannot exclude it
    checked += 1
    types = [str(t) for t in as_list(pr.get("types"))]
    if "edited" not in types:
        offenders.append((os.path.basename(p),
                          as_list(pr.get("branches")),
                          types or ["<default: opened, synchronize, reopened>"]))

if checked == 0:
    print("ERROR: no workflow filters pull_request by branch, so this guard "
          "examined nothing. Either the filters were removed -- in which case "
          "delete this script -- or the parse is broken.", file=sys.stderr)
    sys.exit(2)

if offenders:
    print(f"{len(offenders)} of {checked} branch-filtered workflow(s) do not "
          f"trigger on `edited`, so they never run for a retargeted PR:\n",
          file=sys.stderr)
    for name, branches, types in offenders:
        print(f"    {name}\n        branches: {branches}\n        types:    {types}",
              file=sys.stderr)
    print("\nAdd `edited` to that workflow's pull_request.types. See the header "
          "of this script for what a retargeted PR reports without it.",
          file=sys.stderr)
    sys.exit(1)

print(f"OK: all {checked} branch-filtered workflow(s) trigger on `edited`.")
PY
