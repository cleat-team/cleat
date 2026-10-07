#!/usr/bin/env bash
#
# Refuse a reachability answer from a checkout that cannot give one.
#
# cleat#2463. This repo has worked around a shallow checkout in four separate
# places (migration/idempotency_tenant_test.go, migration/
# two_entities_record_when_they_were_created_test.go, scripts/
# check-doc-comment-reattachment.py, scripts/check-section-numbers.sh), each
# fixed locally, and nothing checked for the CONDITION itself -- `.git/shallow`
# is one line, and nothing in the output of `git describe`, `git merge-base
# --is-ancestor` or `git branch -r --contains` mentions it.
#
# THE INSTANCE THAT SURFACED IT. A release-lineage readiness check -- run by
# hand, the exact two commands this script replaces, straight from
# docs/project/release-process.md -- reported that a tag was not reachable
# from develop and that `git describe` on develop was fatal, and concluded
# develop's history might have been rewritten. All of it was the clone:
# `.git/shallow`, one graft line, origin/develop holding 15 commits against
# the real 1901. Both commands below answered correctly about the truncated
# object they were given; the clone was the defect, not the branch model.
#
#                                reported   truth
#   is-ancestor v0.1.0 develop   exit 1     exit 0
#   describe --tags develop     fatal      v0.1.0
#
# WHY THE FAILURE IS WORSE THAN A WRONG ANSWER. The original report had two
# readings agreeing -- `describe` fatal AND `is-ancestor` exit 1 -- and the
# agreement was taken as corroboration. They shared a source (the same
# truncated clone), so they could not have disagreed. Corroboration from a
# shared scope is one derivation run twice, and where the shared scope is a
# truncated object, the agreement is structural, not evidence.
#
# WHY A STANDALONE `is-shallow-repository` HELPER WAS NOT SHIPPED ON ITS OWN.
# cleat#2463's own prior investigation (WS-3, 2026-09-30) swept scripts/ for
# every live reachability call site (`is-ancestor`, `git describe`, `branch -r
# --contains`, `git log -S`) and found exactly one that needed this guard and
# did not already have it -- NONE: check-doc-comment-reattachment.py already
# refuses correctly (see its own UNMEASURED/exit-2 comment), and the four
# cited call sites are prose, not live code. A mechanism with no caller reads
# as done while doing nothing (CLAUDE.md, "a mechanism that exists and is
# wired to nothing reads as done") -- so this script's first and only caller
# is the real one: the two ad hoc commands in docs/project/release-process.md
# that caused the incident above, replaced with calls to this script.
#
# Usage:
#   scripts/check-ref-ancestry.sh <ancestor-ref> <descendant-ref>
#   scripts/check-ref-ancestry.sh --self-test
#
# Exit codes (the same three-way status check-doc-comment-reattachment.py and
# the migration tooling already use -- not invented for this script):
#   0  <ancestor-ref> IS an ancestor of <descendant-ref>
#   1  <ancestor-ref> is NOT an ancestor of <descendant-ref> -- both refs
#      resolved in a non-shallow repo, so this is a real negative, not a guess
#   2  UNMEASURED -- the precondition could not be established (a shallow
#      repository, a ref that does not resolve, or bad arguments). This is a
#      failure of the check, not a finding about the tree.

set -uo pipefail

usage() {
  echo "usage: $0 <ancestor-ref> <descendant-ref>" >&2
  echo "       $0 --self-test" >&2
}

# check_ancestry ANCESTOR DESCENDANT — the guarded check, factored out so
# --self-test can call it against a deliberately shallow clone without
# re-execing this script.
check_ancestry() {
  local ancestor="$1" descendant="$2"

  if [ "$(git rev-parse --is-shallow-repository 2>/dev/null)" = "true" ]; then
    echo "UNMEASURED: this is a shallow repository (.git/shallow is present)."
    echo "This is a failure of the check, not a finding about the tree."
    echo "Every reachability answer (git describe, git merge-base --is-ancestor,"
    echo "git branch -r --contains) is suspect here -- a truncated clone answers"
    echo "confidently and wrong, which is the exact incident cleat#2463 records."
    echo "Run 'git fetch --unshallow' and re-run this check."
    return 2
  fi

  for ref in "$ancestor" "$descendant"; do
    if ! git rev-parse --verify --quiet "${ref}^{commit}" >/dev/null; then
      echo "UNMEASURED: '$ref' does not resolve to a commit, so no comparison was made."
      echo "This is a failure of the check, not a finding about the tree."
      echo "Fetch the ref (git fetch origin '$ref'), or pass a ref that resolves here."
      return 2
    fi
  done

  if git merge-base --is-ancestor "$ancestor" "$descendant"; then
    echo "$ancestor IS an ancestor of $descendant"
    return 0
  else
    echo "$ancestor is NOT an ancestor of $descendant"
    return 1
  fi
}

self_test() {
  local fail=0

  # --- known-positive: a genuinely shallow clone must be refused, not answered ---
  local shallow_dir
  shallow_dir="$(mktemp -d)"
  trap 'rm -rf "$shallow_dir"' RETURN
  # --depth is silently IGNORED on a local-path clone ("--depth is ignored in
  # local clones; use file:// instead" -- git's own warning, found running
  # this self-test for the first time: the known-positive below reported a
  # false "ok" because the "shallow" clone it built was not shallow at all.
  # The file:// URL form forces the real, network-shaped clone path.
  if ! git clone --quiet --depth 1 --no-single-branch \
      "file://$(git rev-parse --show-toplevel)" "$shallow_dir" >/dev/null 2>&1; then
    echo "self-test: could not create a shallow clone to test against"
    return 2
  fi
  local out rc
  out="$(cd "$shallow_dir" && check_ancestry HEAD HEAD 2>&1)"
  rc=$?
  if [ "$rc" -ne 2 ] || ! echo "$out" | grep -q "^UNMEASURED: this is a shallow repository"; then
    echo "self-test FAILED: a shallow clone was not refused (rc=$rc)"
    echo "$out"
    fail=1
  else
    echo "self-test: ok   shallow clone refused, rc=2"
  fi

  # --- known-positive: an unresolvable ref must be refused, not answered ---
  out="$(check_ancestry HEAD this-ref-does-not-exist-anywhere 2>&1)"
  rc=$?
  if [ "$rc" -ne 2 ] || ! echo "$out" | grep -q "does not resolve to a commit"; then
    echo "self-test FAILED: an unresolvable ref was not refused (rc=$rc)"
    echo "$out"
    fail=1
  else
    echo "self-test: ok   unresolvable ref refused, rc=2"
  fi

  # --- known-negative: HEAD is trivially its own ancestor, in THIS (non-shallow) repo ---
  if [ "$(git rev-parse --is-shallow-repository 2>/dev/null)" = "true" ]; then
    echo "self-test: ok   skipped the positive/negative pair -- this checkout is itself"
    echo "                shallow, so there is nothing further this self-test can measure"
    echo "                honestly here; the shallow-clone case above already covers it"
  else
    out="$(check_ancestry HEAD HEAD 2>&1)"
    rc=$?
    if [ "$rc" -ne 0 ]; then
      echo "self-test FAILED: HEAD was not reported as its own ancestor (rc=$rc)"
      echo "$out"
      fail=1
    else
      echo "self-test: ok   HEAD is its own ancestor, rc=0"
    fi

    # known-negative: HEAD cannot be an ancestor of its own first parent (a real
    # negative, distinguishing "exit 1" from "exit 2" -- see why that distinction
    # is load-bearing in the header above).
    local parent
    parent="$(git rev-parse HEAD^ 2>/dev/null || true)"
    if [ -n "$parent" ]; then
      out="$(check_ancestry HEAD "$parent" 2>&1)"
      rc=$?
      if [ "$rc" -ne 1 ]; then
        echo "self-test FAILED: HEAD was not reported as NOT an ancestor of its own parent (rc=$rc)"
        echo "$out"
        fail=1
      else
        echo "self-test: ok   HEAD is not an ancestor of HEAD^, rc=1"
      fi
    fi
  fi

  if [ "$fail" -ne 0 ]; then
    echo "self-test: FAILED"
    return 2
  fi
  echo "self-test: all cases passed"
  return 0
}

if [ "${1:-}" = "--self-test" ]; then
  self_test
  exit $?
fi

if [ $# -ne 2 ]; then
  usage
  exit 2
fi

check_ancestry "$1" "$2"
exit $?
