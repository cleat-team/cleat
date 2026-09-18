#!/usr/bin/env bash
#
# Sweep history for doc comments that came unattached and STAYED unattached.
#
# check-doc-comment-reattachment.py is diff-scoped by design: it asks whether a
# declaration that had a doc comment between two refs ended up with none. That
# is the right default for CI -- it is fast, and it only ever blames the commit
# that caused the defect. It also means every instance committed before the
# guard landed on 2026-09-16 is permanently invisible to it, and stays in the
# tree forever.
#
# cleat#1852 found seven that way, by hand: 400 commits surveyed one
# `git rev-list` invocation at a time, then a second pass re-checking each hit
# against develop, because a hit at commit `c` may have been repaired since.
# That loop was never committed, so "re-run the sweep periodically" meant
# "rebuild the sweep first", which is why nobody would. This is that loop.
#
# WHAT IT DOES NOT DO, deliberately. It adds no new detection. Every judgement
# is the existing guard's, invoked over a different pair of refs, so a finding
# here has the same ground truth a CI finding has: a real base commit to diff
# against, and a declaration compared only to ITSELF across two trees. The
# alternative -- a whole-tree pass asking "does this comment describe this
# declaration" -- needs a heuristic with no reliable ground truth, and was
# declined on that basis (cleat#1861).
#
# THE TWO PASSES, and the second is the one that matters:
#
#   pass 1   guard(c~1, c)     for each commit -- what broke, and where
#   pass 2   guard(c~1, REF)   for each hit    -- is it STILL broken today
#
# Pass 1 alone over-reports. A defect introduced at `c` and repaired at `c+40`
# is a true finding about `c` and a false finding about the tree, and the tree
# is what a sweep is for. Pass 2 is the same guard asked the same question with
# today's tree as the head, so "repaired since" is measured rather than assumed.
#
# READING THE OUTPUT. "STILL LIVE" means the declaration had a doc comment at
# the offending commit's parent and has none today. It does NOT mean the loss
# was an accident: a doc deliberately deleted is indistinguishable from one that
# came unattached, and the guard says so in its own failure text. Over the last
# 400 commits exactly one finding is of that kind -- `PollSignal` in
# engine/store_signals.go, whose doc was removed on purpose at 7cdb7e09 -- so
# the live count is a list to read, not a defect count to act on.
#
# COST. Pass 1 is cheap and linear: one single-commit diff each, about 9 seconds
# for 60 commits. Pass 2 is not, because it diffs each offending commit against
# today's tree, so its cost grows with distance from HEAD -- 400 commits took
# over two minutes here, nearly all of it in pass 2. That is the right trade for
# a sweep run occasionally, and the reason this is not a CI gate.
#
# EXIT STATUS, matching the guard it wraps:
#     0  no finding is still live
#     1  at least one is
#     2  UNMEASURED -- a ref did not resolve or the guard is missing, so some
#        commit was not compared. Separate from 1: a sweep that skipped commits
#        silently and reported "0 live" is the reassuring answer to a question
#        nobody asked.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GUARD="${SCRIPT_DIR}/check-doc-comment-reattachment.py"

COMMITS=400
REF=HEAD
SELF_TEST=0

usage() {
    cat >&2 <<'USAGE'
usage: sweep-doc-comment-reattachment.sh [--commits N] [--ref REF] [--self-test]

  --commits N   how many commits back to survey (default 400)
  --ref REF     the tip to survey, and the tree to re-check against (default HEAD)
  --self-test   build a throwaway repo with a planted defect and a planted
                repair, and assert the sweep separates them
USAGE
}

while [ $# -gt 0 ]; do
    case "$1" in
        --commits) COMMITS="$2"; shift 2 ;;
        --ref)     REF="$2"; shift 2 ;;
        --self-test) SELF_TEST=1; shift ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown argument: $1" >&2; usage; exit 2 ;;
    esac
done

# Findings arrive as
#     path/to/file.go: `name` had a doc comment at X and has none at Y.
# and only that shape is extracted. A guard whose wording changes stops matching
# here and the sweep reports nothing -- which is why parse_findings' output is
# checked against a known-positive in --self-test rather than trusted.
parse_findings() {
    # shellcheck disable=SC2016  # the backticks are literal: the guard wraps each declaration name in them
    sed -n 's/^\(.*\): `\([^`]*\)` had a doc comment at .* and has none at .*$/\1	\2/p'
}

sweep() {
    local ref="$1" commits="$2"
    local unmeasured=0 surveyed=0 with_finding=0
    local hits_file live_file
    hits_file="$(mktemp)"; live_file="$(mktemp)"
    trap 'rm -f "$hits_file" "$live_file"' RETURN

    local c parent out rc
    for c in $(git rev-list --max-count="$commits" "$ref"); do
        parent="$(git rev-parse -q --verify "${c}^" 2>/dev/null)" || continue
        surveyed=$((surveyed + 1))
        out="$(python3 "$GUARD" "$parent" "$c" 2>&1)"; rc=$?
        case "$rc" in
            0) ;;
            1)
                with_finding=$((with_finding + 1))
                printf '%s\n' "$out" | parse_findings | while IFS=$'\t' read -r f n; do
                    printf '%s\t%s\t%s\n' "$c" "$f" "$n" >> "$hits_file"
                done
                ;;
            *) unmeasured=1 ;;
        esac
    done

    # Pass 2, grouped by the commit's parent so the guard runs once per
    # offending commit rather than once per finding.
    local seen_parents="" p key
    while IFS=$'\t' read -r c f n; do
        p="$(git rev-parse -q --verify "${c}^" 2>/dev/null)" || { unmeasured=1; continue; }
        key=" ${p} "
        case "$seen_parents" in
            *"$key"*) ;;
            *)
                seen_parents="${seen_parents}${key}"
                out="$(python3 "$GUARD" "$p" "$ref" 2>&1)"; rc=$?
                [ "$rc" = 2 ] && unmeasured=1
                printf '%s\n' "$out" | parse_findings | sed "s|^|${p}	|" >> "$live_file"
                ;;
        esac
    done < "$hits_file"

    local n_hits n_live
    n_hits=$(wc -l < "$hits_file" | tr -d ' ')
    while IFS=$'\t' read -r c f n; do
        p="$(git rev-parse -q --verify "${c}^" 2>/dev/null)"
        if grep -qF "$(printf '%s\t%s\t%s' "$p" "$f" "$n")" "$live_file" 2>/dev/null; then
            # shellcheck disable=SC2016  # the backticks are literal: the guard wraps each declaration name in them
            printf 'STILL LIVE  %s `%s`  (broke at %s)\n' "$f" "$n" "${c:0:8}"
        else
            # shellcheck disable=SC2016  # the backticks are literal: the guard wraps each declaration name in them
            printf 'repaired    %s `%s`  (broke at %s, fixed since)\n' "$f" "$n" "${c:0:8}"
        fi
    done < "$hits_file" | sort -r

    echo
    echo "commits surveyed                 ${surveyed}"
    echo "commits with a finding           ${with_finding}"
    echo "findings total                   ${n_hits}"

    n_live=0
    while IFS=$'\t' read -r c f n; do
        p="$(git rev-parse -q --verify "${c}^" 2>/dev/null)"
        grep -qF "$(printf '%s\t%s\t%s' "$p" "$f" "$n")" "$live_file" 2>/dev/null && n_live=$((n_live + 1))
    done < "$hits_file"
    printf 'still live on %-18s %s\n' "$ref" "$n_live"

    if [ "$unmeasured" = 1 ]; then
        echo
        echo "UNMEASURED: at least one commit could not be compared, so this count is a"
        echo "floor rather than a result. Fix that before reading the number above."
        return 2
    fi
    [ "$n_live" -gt 0 ] && return 1
    return 0
}

self_test() {
    local tmp ok=0
    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' RETURN
    (
        cd "$tmp" || exit 1
        git init -q .
        git config user.email sweep@test.invalid
        git config user.name  "sweep self-test"

        # c1: two documented declarations, nothing wrong.
        cat > live.go <<'GO'
package p

// Alpha does the alpha thing.
func Alpha() {}
GO
        cat > fixed.go <<'GO'
package p

// Delta does the delta thing.
func Delta() {}
GO
        git add -A && git commit -qm c1

        # c2: THE PLANTED DEFECT. Gamma is inserted between Alpha's doc comment
        # and Alpha, so the comment now documents Gamma. Never repaired.
        cat > live.go <<'GO'
package p

// Alpha does the alpha thing.
func Gamma() {}

func Alpha() {}
GO
        git add -A && git commit -qm c2

        # c3: THE PLANTED REPAIR, first half -- same defect, different file.
        cat > fixed.go <<'GO'
package p

// Delta does the delta thing.
func Epsilon() {}

func Delta() {}
GO
        git add -A && git commit -qm c3

        # c4: and repaired. Pass 1 must still see c3; pass 2 must classify it
        # as repaired, which is the whole reason pass 2 exists.
        cat > fixed.go <<'GO'
package p

// Epsilon does the epsilon thing.
func Epsilon() {}

// Delta does the delta thing.
func Delta() {}
GO
        git add -A && git commit -qm c4
    ) || { echo 'SELF-TEST UNMEASURED: could not build the fixture repo'; return 2; }

    local out rc
    out="$(cd "$tmp" && sweep HEAD 50)"; rc=$?

    # The known-positive: a defect that is still live must be reported as such.
    # shellcheck disable=SC2016  # the backticks are literal: the guard wraps each declaration name in them
    if ! printf '%s\n' "$out" | grep -q 'STILL LIVE.*live.go.*`Alpha`'; then
        echo 'SELF-TEST FAILED: the sweep did not report a defect that is still live'
        ok=1
    fi
    # The known-negative, and the one a sweep without pass 2 gets wrong: a
    # defect that was repaired must NOT be reported as live.
    if printf '%s\n' "$out" | grep -q 'STILL LIVE.*fixed.go'; then
        echo 'SELF-TEST FAILED: a repaired defect was reported as still live -- pass 2 is not working'
        ok=1
    fi
    # ...but it must still be SEEN, or pass 1 is broken and the sweep would be
    # silent for the wrong reason.
    # shellcheck disable=SC2016  # the backticks are literal: the guard wraps each declaration name in them
    if ! printf '%s\n' "$out" | grep -q 'repaired.*fixed.go.*`Delta`'; then
        echo 'SELF-TEST FAILED: the sweep did not see the repaired defect at all'
        ok=1
    fi
    # Exit status must distinguish "found something live" from "clean".
    if [ "$rc" != 1 ]; then
        echo "SELF-TEST FAILED: exit status was ${rc}, want 1 -- one finding is still live"
        ok=1
    fi

    if [ "$ok" = 0 ]; then
        echo 'self-test passed'
        return 0
    fi
    printf '%s\n' "$out"
    echo 'self-test FAILED'
    return 1
}

if [ ! -f "$GUARD" ]; then
    echo "UNMEASURED: ${GUARD} is missing, so nothing was compared." >&2
    exit 2
fi

if [ "$SELF_TEST" = 1 ]; then
    self_test
    exit $?
fi

sweep "$REF" "$COMMITS"
exit $?
