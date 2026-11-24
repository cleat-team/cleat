#!/usr/bin/env bash
# Does a pull request's diff touch what the Chaos sample exists to guard? (cleat#4169)
#
# Reads changed file paths on stdin, one per line (`git diff --name-only --no-renames`), and prints exactly one word on stdout:
#
#   true    at least one path is in the replay machinery, so the sample must run;
#   false   there is at least one path and none of them is, so the sample is skipped.
#
# An EMPTY diff prints `true`. A diff we cannot read is not evidence that nothing relevant changed, and a sample that ran when it did not need
# to costs seven minutes where one that was skipped when it did need to costs a regression the sample exists to find. The caller (chaos.yml's
# `changes` job) applies the same rule to a failure of this script: no output is read as "run it".
#
# WHY THIS REPLACED `on.pull_request.paths`. A workflow-level path filter means the workflow does not RUN for a pull request that touches none
# of the paths, so none of its checks exist for that PR. That is invisible while the check is optional and becomes a PR that can never merge
# once `Chaos sample` is a required context: a required check that never reports is pending forever, not passing. Detecting inside the workflow
# means the job always exists, and a job skipped by `if` reports SKIPPED, which a required check accepts.
#
# THE LIST IS THE OLD `paths:` LIST, unchanged, plus this script (a change to the detector must itself run the sample). Bash `case` globs: `*`
# matches `/`, so `engine/*` is the whole subtree, as `engine/**` was. Prefixes carry their `/` so `enginex/foo` is not `engine/`.
#
#   scripts/chaos-sample-relevant.sh --self-test     known positives and negatives, exit 0 or 1
set -uo pipefail

relevant_path() {
  case "$1" in
    engine/* | wasm/* | cmd/cleat-worker/* | migrations/* | crates/cleat-sdk/* | python-sdk/* | tests/chaos/*) return 0 ;;
    testdata/chaosscript*/*) return 0 ;;
    .github/workflows/chaos.yml | scripts/chaos-sample-relevant.sh) return 0 ;;
  esac
  return 1
}

classify() {
  local seen=0 path
  while IFS= read -r path || [ -n "$path" ]; do
    [ -z "$path" ] && continue
    seen=1
    if relevant_path "$path"; then
      echo true
      return 0
    fi
  done
  if [ "$seen" -eq 0 ]; then
    echo true # an empty diff proves nothing: run
  else
    echo false
  fi
}

self_test() {
  local fail=0 got
  check() { # check <want> <description> <paths...>
    local want="$1" what="$2"
    shift 2
    got=$(printf '%s\n' "$@" | classify)
    if [ "$got" != "$want" ]; then
      echo "self-test FAIL: $what: want $want, got $got" >&2
      fail=1
    fi
  }
  # Known positives: each is in the old `paths:` list.
  check true "engine" engine/executor.go
  check true "wasm" wasm/exports.go
  check true "the worker" cmd/cleat-worker/server.go
  check true "migrations" migrations/postgres/001_schema.sql
  check true "the rust sdk" crates/cleat-sdk/src/lib.rs
  check true "the python sdk" python-sdk/cleat_sdk/__init__.py
  check true "the harness" tests/chaos/env_test.go
  check true "a chaosscript guest" testdata/chaosscript/main.go
  check true "a chaosscript python guest" testdata/chaosscript_py/chaosscript.py
  check true "this workflow" .github/workflows/chaos.yml
  check true "this detector" scripts/chaos-sample-relevant.sh
  check true "one relevant path among irrelevant ones" docs/a.md README.md engine/x.go CHANGELOG.md
  check true "the relevant path last" docs/a.md engine/x.go
  # Known negatives: the cases the filter exists to let through.
  check false "docs only" docs/reference/worker-config.md
  check false "the chart" charts/cleat/values.yaml
  check false "another workflow" .github/workflows/ci.yml
  check false "a changelog fragment" changelog.d/4397-x.md
  check false "a script that is not this one" scripts/check-skips.sh
  check false "a prefix that is not the directory" enginex/foo.go
  check false "a path that merely contains the directory" docs/engine/notes.md
  check false "another testdata directory" testdata/other/x.go
  check false "several irrelevant paths" docs/a.md charts/b.yaml changelog.d/c.md
  # An empty diff and blank lines prove nothing: run.
  got=$(printf '' | classify)
  [ "$got" = true ] || { echo "self-test FAIL: an empty diff: want true, got $got" >&2; fail=1; }
  got=$(printf '\n\n' | classify)
  [ "$got" = true ] || { echo "self-test FAIL: only blank lines: want true, got $got" >&2; fail=1; }
  # A last line with no trailing newline is still read.
  got=$(printf 'docs/a.md\nengine/x.go' | classify)
  [ "$got" = true ] || { echo "self-test FAIL: an unterminated last line: want true, got $got" >&2; fail=1; }
  if [ "$fail" -eq 0 ]; then
    echo "chaos-sample-relevant: self-test ok"
  fi
  return "$fail"
}

case "${1:-}" in
  --self-test) self_test ;;
  "") classify ;;
  *)
    echo "usage: $0 [--self-test]   (paths on stdin)" >&2
    exit 2
    ;;
esac
