#!/usr/bin/env bash
# A workflow that runs on push must not discard its own runs -- by cancelling
# them, or by queueing them where a later push will cancel them for it.
#
# WHY THIS IS A GUARD AND NOT A CONVENTION. `github.ref` is refs/heads/develop
# for every merge, so a workflow whose concurrency group is keyed on it puts
# consecutive merges into one group. There are then TWO ways the merge's own
# verification is thrown away, and this script checks for both, because the
# first version of it checked only the first and 11 runs were lost anyway.
#
#   1. cancel-in-progress: true -- a later merge kills a running one outright.
#
#   2. A shared group at all. GitHub allows one *pending* run per group, so
#      when a third push arrives at an occupied group, the one waiting is
#      cancelled before it starts. `cancel-in-progress: false` does not
#      prevent this: it governs the running run, not the queued one.
#
# (2) is not theoretical and is not rare. Measured on develop after #634 merged
# (18:20:48Z on 2026-09-03), which fixed (1), 5 of 24 `Tier 1 Gate` push runs
# were cancelled -- every one with zero jobs, killed within seconds of the next
# merge:
#
#   gh run list --workflow="Tier 1 Gate" --branch develop --event push \
#     --limit 60 --json conclusion,createdAt,databaseId
#   gh run view <cancelled-id> --json jobs --jq '.jobs | length'   # 0
#   gh run view <success-id>   --json jobs --jq '.jobs | length'   # 1
#
# The job count is what tells the two apart. Of 22 cancelled gate runs that day,
# the 17 before #634 are jobs=1 (killed mid-run, one jobs=0 exception) and all 5
# after it are jobs=0. Compare against the merge time in UTC: `git log --date=iso`
# prints local time with an offset, and reading 14:20:48 -0400 as though it were
# 14:20:48Z pulls six pre-fix runs into the window and inflates this count to
# 11 of 36. That is how it was first written down.
#
# The fix for both is to give each push its own group and keep cancellation
# scoped to pull requests, where it is genuinely wanted:
#
#     concurrency:
#       group: ${{ github.workflow }}-${{ github.event_name == 'pull_request' && github.ref || github.sha }}
#       cancel-in-progress: ${{ github.event_name == 'pull_request' }}
#
# THE SAME MECHANISM APPLIES TO `merge_group`, and this guard could not see it
# until cleat#1426. A merge queue creates one `gh-readonly-queue/...` ref per
# entry, so several entries are in flight at once -- exactly the condition (2)
# describes, with queue entries in place of pushes. cla-assistant.yml keyed its
# group on `github.event.pull_request.number || github.event.issue.number`, and
# BOTH are null on `merge_group`, so every entry collapsed into one group named
# "CLA Assistant-" and each new entry evicted an earlier one's pending run. The
# queue HEAD loses, having waited longest; `Contributor License Agreement` is a
# required context; so the head sat at AWAITING_CHECKS with its nine other
# checks green and everything behind it read UNMERGEABLE. Ten PRs, ~40 minutes,
# 2026-09-13, jobs=0 on the evicted run.
#
# NOTE THE CRITERION INVERTS BETWEEN THE TWO TRIGGERS, which is why this needs a
# second loop rather than a wider pattern. On push, `github.ref` is
# refs/heads/develop for every merge and therefore does NOT distinguish runs. On
# merge_group it is the per-entry queue ref and therefore DOES. A single rule
# covering both would have to reject `github.ref` for one and require it for the
# other.
#
# Re-derive what this reads:
#   PYTHONPATH=scripts/lib python3 -c "from workflow_on import load_triggers; import glob; [print(p) for p in sorted(glob.glob('.github/workflows/*.yml')) if 'push' in load_triggers(p)]"
#   grep -l 'merge_group:' .github/workflows/*.yml
#
# NOT `grep -l 'push:' .github/workflows/*.yml` -- that is the cleat#2079
# defect this script used to have, re-derived here as a demonstration rather
# than a recommendation: it overcounts by one, `release-image-dryrun-arm64.yml`,
# which has `push: false` on a docker/build-push-action step and no `on: push`
# at all.
set -euo pipefail

cd "$(dirname "$0")/.."

# cleat#2079. `grep -qE '^\s*push:'` allows any indentation, so it also
# matches docker/build-push-action's `push: false` step INPUT, deeply nested
# under `with:` -- a build option, not an `on: push:` trigger.
# release-image-dryrun-arm64.yml (schedule + workflow_dispatch only) has
# exactly that input and was counted as push-triggered by the line-oriented
# read. Two of the three files with this input (ci.yml, release.yml) are
# genuinely push-triggered anyway, which is how it went unnoticed: the false
# positive only shows up on a workflow that ISN'T.
#
# Read the trigger from the parsed `on:` mapping instead, the same move
# check-workflow-pr-triggers.sh already made for the same reason (CLAUDE.md's
# recurring defect: a tool applied to a format it does not model). `on` is
# YAML's bare boolean True under YAML 1.1, and can be a bare string
# (`on: push`), a list (`on: [push, ...]`), or a mapping -- this repo's own
# workflows are all the mapping form today (`git grep -c '^on: push$'
# .github/workflows/` and `'^on: \[' .github/workflows/` -> 0, 0), but all
# three are handled rather than assuming the one shape observed.
#
# The parse itself lives in scripts/lib/workflow_on.py, shared with
# check-workflow-pr-triggers.sh (cleat#2737) -- this guard used to carry its
# own copy of the same normalization, free to drift from the other one if
# corrected in just one place.
PUSH_FILES_OUT="$(python3 - "$(pwd)" <<'PY'
import sys, os, glob

sys.path.insert(0, os.path.join(sys.argv[1], "scripts", "lib"))
try:
    from workflow_on import load_triggers
except ImportError as e:
    print(f"ERROR: could not import scripts/lib/workflow_on.py ({e}) -- "
          "either PyYAML is not installed (pip install pyyaml) or the lib "
          "path is wrong. A check that cannot run must not report success.",
          file=sys.stderr)
    sys.exit(2)

root = sys.argv[1]
paths = sorted(glob.glob(os.path.join(root, ".github", "workflows", "*.yml")) +
               glob.glob(os.path.join(root, ".github", "workflows", "*.yaml")))
if not paths:
    print("ERROR: no workflow files found -- this guard would pass no matter "
          "what the workflows said.", file=sys.stderr)
    sys.exit(2)

for p in paths:
    triggers = load_triggers(p)
    if "push" in triggers:
        print(p)
PY
)"
rc=$?
if [ "$rc" -ne 0 ]; then
    exit "$rc"
fi
push_files=()
while IFS= read -r line; do
    [ -n "$line" ] && push_files+=("$line")
done <<< "$PUSH_FILES_OUT"

# --self-test: the known-positive/negative-control pair from cleat#2079,
# checked against the real committed workflows rather than a synthetic
# fixture -- both files are part of this repo's own tree, not test data that
# could drift from what the guard actually reads.
if [ "${1:-}" = "--self-test" ]; then
    self_test_fail=0
    is_push_triggered() {
        local want="$1" f
        for f in "${push_files[@]}"; do
            [ "$(basename "$f")" = "$want" ] && return 0
        done
        return 1
    }
    # Known-positive: has `push: false` on a docker/build-push-action step
    # and no `on: push` at all. The pre-cleat#2079 grep counted this file as
    # push-triggered; this must not.
    #
    # cleat-review on #2734: is_push_triggered returns false both for
    # "genuinely not push-triggered" and for "this file no longer exists" or
    # "this file no longer has the trap" -- renaming the fixture or dropping
    # its `push: false` line made the assertion below pass vacuously, exit 0,
    # silently. The fixture can drift exactly as a synthetic one could; this
    # confirms it still exercises #2079 before trusting what it reports:
    # the file must exist AND still trip the OLD grep this guard replaced.
    known_positive=".github/workflows/release-image-dryrun-arm64.yml"
    known_positive_name=$(basename "$known_positive")
    if [ ! -e "$known_positive" ]; then
        echo "FAIL: self-test fixture $known_positive does not exist." >&2
        echo "      Renamed or removed -- this self-test proves nothing until" >&2
        echo "      it is repointed at a file with the same shape." >&2
        self_test_fail=1
    elif ! grep -qE '^\s*push:' "$known_positive"; then
        echo "FAIL: self-test fixture $known_positive no longer trips the" >&2
        echo "      pre-cleat#2079 grep ('^\s*push:'), so it no longer" >&2
        echo "      exercises the defect this self-test exists to catch --" >&2
        echo "      find another file with a nested push: input, or add one." >&2
        self_test_fail=1
    elif is_push_triggered "$known_positive_name"; then
        echo "FAIL: $known_positive_name is reported push-triggered." >&2
        echo "      It has 'push: false' on a build step and no 'on: push' -- the" >&2
        echo "      cleat#2079 defect this self-test exists to catch." >&2
        self_test_fail=1
    else
        echo "ok  $known_positive_name is correctly NOT push-triggered"
    fi
    # Negative control: genuinely push-triggered, and also has a `push:`
    # build-step input elsewhere in the file. A fix that stopped detecting
    # push triggers entirely -- not just the false positive -- would pass
    # the check above vacuously and only this catches it.
    if is_push_triggered "ci.yml"; then
        echo "ok  ci.yml is correctly push-triggered"
    else
        echo "FAIL: ci.yml is not reported push-triggered, but it is (on: push)." >&2
        echo "      A check that cannot see a real positive is not a check." >&2
        self_test_fail=1
    fi
    if [ "$self_test_fail" -ne 0 ]; then
        echo "self-test: FAILED" >&2
        exit 1
    fi
    echo "self-test: known-positive and negative control both correct"
    exit 0
fi

# Extract THE group expression, or say we cannot. cleat#1426 review (WS-3):
# `grep '^\s*group:' | head -1` silently assumes one top-level concurrency
# block per file. Add a job-level block and head -1 analyses whichever came
# first, so the guard checks the wrong expression and passes -- a PARTIAL skip,
# which neither vacuity control below can see because they only fire at zero.
#
# Measured 2026-09-13: 9 merge_group workflows, all with a single top-level
# block, 0 job-level blocks anywhere. So this is unreached today and is here to
# stay unreached: it turns a future silent misread into a loud refusal.
# NOTE THE SHAPE: the count happens in the LOOP's shell, not in a helper called
# through $(...). The first version of this was a group_expr() function that
# appended to `ambiguous` and was invoked as `group=$(group_expr "$f")` -- a
# command substitution runs in a SUBSHELL, so every append was discarded and the
# guard passed a deliberately ambiguous tree. Caught by the known-positive
# below, which is the whole argument for having one: the refusal was written,
# reviewed, and inert.
ambiguous=()

bad_cancel=()
bad_group=()
scanned=0
with_concurrency=0

for f in "${push_files[@]}"; do
    scanned=$((scanned + 1))

    if grep -qE '^\s*cancel-in-progress:\s*true\s*$' "$f"; then
        bad_cancel+=("$f")
    fi

    # No concurrency block means no group, so no queue and nothing to discard.
    grep -qE '^concurrency:' "$f" || continue
    with_concurrency=$((with_concurrency + 1))

    # The group must vary per commit on a push. github.sha and github.run_id
    # both do; github.ref alone does not.
    n_group=$(grep -cE '^[[:space:]]*group:' "$f")
    if [ "$n_group" -ne 1 ]; then
        ambiguous+=("$f ($n_group group: lines)")
        continue
    fi
    group=$(grep -E '^[[:space:]]*group:' "$f")
    case "$group" in
        *github.sha*|*github.run_id*) ;;
        *) bad_group+=("$f") ;;
    esac
done

bad_queue_group=()
mg_scanned=0
mg_with_concurrency=0

for f in .github/workflows/*.yml; do
    grep -qE '^\s*merge_group:' "$f" || continue
    mg_scanned=$((mg_scanned + 1))

    grep -qE '^concurrency:' "$f" || continue
    mg_with_concurrency=$((mg_with_concurrency + 1))

    # The group must contain something that differs between queue entries.
    # github.ref is the per-entry gh-readonly-queue ref, so it qualifies here
    # even though it does not on push. A group built only from
    # github.event.pull_request.* or github.event.issue.* is the cleat#1426
    # defect: both are null on merge_group, so the expression is a constant.
    n_group=$(grep -cE '^[[:space:]]*group:' "$f")
    if [ "$n_group" -ne 1 ]; then
        ambiguous+=("$f ($n_group group: lines)")
        continue
    fi
    group=$(grep -E '^[[:space:]]*group:' "$f")
    # github.ref is the per-entry gh-readonly-queue ref here, so it qualifies on
    # merge_group even though it does not on push. github.run_id also passes,
    # but note WHY: it is unique per run, so it satisfies this by making the
    # concurrency block inert. That is safe, not good -- this is a list of keys
    # that cannot collapse, not a list of recommendations.
    case "$group" in
        *github.ref*|*github.sha*|*github.run_id*|*merge_group.head_sha*) ;;
        *) bad_queue_group+=("$f") ;;
    esac
done

# A scan that matched nothing would pass no matter what the workflows said --
# the same vacuous-pass failure the rest of this repo's guards are shaped
# against. Both counts are negative controls: there are push-triggered
# workflows, and they do declare concurrency. If either reads zero, this script
# is looking at the wrong thing rather than the tree being clean.
if [ "$scanned" -eq 0 ]; then
    echo "ERROR: no push-triggered workflows found in .github/workflows/." >&2
    echo "This guard is reading the wrong files and would pass whatever they said." >&2
    exit 1
fi
if [ "$with_concurrency" -eq 0 ]; then
    echo "ERROR: no push-triggered workflow declares a concurrency block." >&2
    echo "The group check below matched nothing and would pass whatever they said." >&2
    exit 1
fi

if [ "$mg_scanned" -eq 0 ]; then
    echo "ERROR: no merge_group-triggered workflows found in .github/workflows/." >&2
    echo "Every required context has to report in queue context, so zero here means" >&2
    echo "this guard is reading the wrong files rather than the tree being clean." >&2
    exit 1
fi
if [ "$mg_with_concurrency" -eq 0 ]; then
    echo "ERROR: no merge_group-triggered workflow declares a concurrency block." >&2
    echo "The queue-group check matched nothing and would pass whatever they said." >&2
    exit 1
fi

fail=0

if [ ${#bad_cancel[@]} -gt 0 ]; then
    echo "ERROR: these workflows run on push and cancel their own running runs:" >&2
    printf '    %s\n' "${bad_cancel[@]}" >&2
    fail=1
fi

if [ ${#bad_group[@]} -gt 0 ]; then
    echo "ERROR: these workflows run on push and share one concurrency group across pushes:" >&2
    printf '    %s\n' "${bad_group[@]}" >&2
    echo >&2
    echo "GitHub keeps at most one pending run per group, so the next merge cancels" >&2
    echo "the queued one before it starts -- with zero jobs, and 'cancelled' is not" >&2
    echo "'success'. cancel-in-progress: false does not prevent this." >&2
    fail=1
fi

# --- 3. a comment must not evict a push's check --------------------------
# Second eviction from the same file, measured 2026-09-13. A workflow triggered
# by BOTH issue_comment and pull_request(_target), keyed on the PR number, puts
# a comment on a PR into the same concurrency group as that PR's push. The
# comment's run reports against develop; the push's run reports against the PR
# head. Evict the second and the required context never attaches to the commit,
# so the PR reads BLOCKED with zero failing and zero pending checks -- one short
# of the total, which looks like nothing is wrong at all.
#
# Only cla-assistant.yml has both triggers today. The check is here because the
# combination is what makes it possible, not because the file is special.
bad_event_group=()
ce_scanned=0

for f in .github/workflows/*.yml; do
    grep -qE '^[[:space:]]*issue_comment:' "$f" || continue
    grep -qE '^[[:space:]]*pull_request(_target)?:' "$f" || continue
    ce_scanned=$((ce_scanned + 1))

    grep -qE '^concurrency:' "$f" || continue
    n_group=$(grep -cE '^[[:space:]]*group:' "$f")
    [ "$n_group" -eq 1 ] || continue        # already reported as ambiguous

    # The group must separate the two triggers. github.event_name does it
    # directly; head.sha does it as a side effect, being null on issue_comment.
    group=$(grep -E '^[[:space:]]*group:' "$f")
    case "$group" in
        *github.event_name*|*pull_request.head.sha*) ;;
        *) bad_event_group+=("$f") ;;
    esac
done

if [ "$ce_scanned" -eq 0 ]; then
    echo "ERROR: no workflow triggers on both issue_comment and pull_request(_target)." >&2
    echo "cla-assistant.yml did when this check was written; if that changed on purpose," >&2
    echo "delete this check rather than leaving it matching nothing." >&2
    exit 1
fi

if [ ${#bad_event_group[@]} -gt 0 ]; then
    echo "ERROR: these workflows run on both issue_comment and pull_request(_target)" >&2
    echo "but do not separate the two in their concurrency group:" >&2
    printf '    %s\n' "${bad_event_group[@]}" >&2
    echo >&2
    echo "A comment then shares a group with that PR's push and can evict it. The" >&2
    echo "comment's run reports on the base branch, so the required context never" >&2
    echo "attaches to the PR head and the PR is BLOCKED with nothing red. cleat#1426." >&2
    echo >&2
    echo "Add github.event_name, or key on github.event.pull_request.head.sha." >&2
    fail=1
fi

if [ ${#ambiguous[@]} -gt 0 ]; then
    echo "ERROR: these workflows have zero or several 'group:' lines, so this guard" >&2
    echo "cannot tell which concurrency expression governs the run:" >&2
    printf '    %s\n' "${ambiguous[@]}" >&2
    echo >&2
    echo "Refusing rather than analysing the first one, which would check the wrong" >&2
    echo "expression and pass. Teach this script about job-level concurrency blocks." >&2
    fail=1
fi

if [ ${#bad_queue_group[@]} -gt 0 ]; then
    echo "ERROR: these workflows run on merge_group and put every queue entry in one group:" >&2
    printf '    %s\n' "${bad_queue_group[@]}" >&2
    echo >&2
    echo "github.event.pull_request.* and github.event.issue.* are BOTH null on a" >&2
    echo "merge_group event, so a group built only from those is a constant. Queue" >&2
    echo "entries then evict each other's pending runs and the queue HEAD -- which" >&2
    echo "has waited longest -- can never satisfy a required context. cleat#1426." >&2
    echo >&2
    echo "Add a per-entry term; github.ref is the gh-readonly-queue ref and works:" >&2
    echo "    group: \${{ github.workflow }}-\${{ github.event.pull_request.number || github.event.issue.number || github.ref }}" >&2
    fail=1
fi

if [ "$fail" -ne 0 ]; then
    cat >&2 <<'MSG'

Key the group on the commit for pushes, and keep cancellation for PRs:

    group: ${{ github.workflow }}-${{ github.event_name == 'pull_request' && github.ref || github.sha }}
    cancel-in-progress: ${{ github.event_name == 'pull_request' }}

MSG
    exit 1
fi

echo "OK: $scanned push-triggered workflows ($with_concurrency with a concurrency group);"
echo "    none cancels a running run, none shares a group across pushes."
echo "    $mg_scanned merge_group-triggered ($mg_with_concurrency with a group); none shares a group across queue entries."
