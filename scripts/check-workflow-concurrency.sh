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
# Re-derive what this reads:
#   grep -l 'push:' .github/workflows/*.yml
set -euo pipefail

cd "$(dirname "$0")/.."

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
    group=$(grep -E '^\s*group:' "$f" | head -1)
    case "$group" in
        *github.sha*|*github.run_id*) ;;
        *) bad_group+=("$f") ;;
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
