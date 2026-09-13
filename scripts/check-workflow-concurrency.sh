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
#   grep -l 'push:' .github/workflows/*.yml
#   grep -l 'merge_group:' .github/workflows/*.yml
set -euo pipefail

cd "$(dirname "$0")/.."

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

for f in .github/workflows/*.yml; do
    # Only workflows that run on push are affected; a pull_request-only
    # workflow sharing and cancelling its own group is correct.
    grep -qE '^\s*push:' "$f" || continue
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
