#!/usr/bin/env bash
# A workflow that runs on push must not cancel its own runs.
#
# WHY THIS IS A GUARD AND NOT A CONVENTION. `github.ref` is refs/heads/develop
# for every merge, so a workflow with
#
#     concurrency:
#       group: ${{ github.workflow }}-${{ github.ref }}
#       cancel-in-progress: true
#
# puts consecutive merges in one group and lets each cancel the one before it.
# The merge's own verification is discarded, and `cancelled` is not `success` --
# nothing downstream reads it as a failure, so it disappears silently.
#
# That matters most for the checks whose entire subject is the merge. The plan
# section-number guard exists to catch collisions that "exist only in the merge"
# (see the comment on its step in ci.yml). It cannot see them from a pre-merge
# base by construction, so the post-merge run is the only place it can -- and
# that is exactly the run being cancelled. Measured 2026-09-03: three develop
# runs cancelled and two failed unnoticed while duplicate section numbers sat on
# develop for two hours.
#
# The fix is to scope the cancellation to pull requests, where it is genuinely
# wanted (pushing twice to a branch should not run the suite twice):
#
#     cancel-in-progress: ${{ github.event_name == 'pull_request' }}
#
# Re-derive what this checks:
#   grep -l 'push:' .github/workflows/*.yml | xargs grep -l 'cancel-in-progress: true'
set -euo pipefail

cd "$(dirname "$0")/.."

bad=()
scanned=0
for f in .github/workflows/*.yml; do
    # Only workflows that run on push are affected; a pull_request-only
    # workflow cancelling itself is correct.
    grep -qE '^\s*push:' "$f" || continue
    scanned=$((scanned + 1))
    if grep -qE '^\s*cancel-in-progress:\s*true\s*$' "$f"; then
        bad+=("$f")
    fi
done

# A scan that matched nothing would pass no matter what the workflows said --
# the same vacuous-pass failure the rest of this repo's guards are shaped
# against. There are push-triggered workflows; if this finds none, the glob or
# the grep is broken rather than the tree being clean.
if [ "$scanned" -eq 0 ]; then
    echo "ERROR: no push-triggered workflows found in .github/workflows/." >&2
    echo "This guard is reading the wrong files and would pass whatever they said." >&2
    exit 1
fi

if [ ${#bad[@]} -gt 0 ]; then
    echo "ERROR: these workflows run on push and cancel their own runs:" >&2
    printf '    %s\n' "${bad[@]}" >&2
    cat >&2 <<'MSG'

github.ref is refs/heads/develop for every merge, so consecutive merges share
one concurrency group and each cancels the previous one. The merge's own
verification is discarded, and `cancelled` is not `success`.

Scope it to pull requests instead:

    cancel-in-progress: ${{ github.event_name == 'pull_request' }}

MSG
    exit 1
fi

echo "OK: $scanned push-triggered workflows, none cancels its own runs."
