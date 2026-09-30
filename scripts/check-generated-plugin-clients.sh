#!/usr/bin/env bash
# A generated plugin client is a COPY of what cmd/cleat-gen plugin-client
# would produce from its plugin's current RegisterTyped call sites, not a
# live view of them. Nothing regenerated
# cleat/pluginclients/{webhookingest,email}/client.go
# and diffed them against their source in CI, so a plugin's registration
# could change -- a renamed field, a new operation, a widened Req struct --
# and the committed client would keep compiling against the OLD shape.
# `PluginCall` is string-and-JSON-keyed underneath, so that failure would
# land as a plugin error at runtime, not a build break. cleat#2626's own PR
# found exactly this failure mode from a hand-written payload (a "body"
# field that plugins/email's SendInput never had); a stale generated client
# reintroduces the same class of bug with a `// Code generated ... DO NOT
# EDIT` header sitting on top of it, which reads as more trustworthy than
# the hand-written JSON it replaced, not less.
#
# This regenerates every tracked client into a scratch directory and diffs
# it byte-for-byte against the committed file. Add a plugin client here when
# cmd/cleat-gen plugin-client gains one, in the CLIENTS array below --
# nothing else in this script is plugin-specific.
#
# Usage: scripts/check-generated-plugin-clients.sh [--self-test]
#
# --self-test mutates a committed client (deletes a field from its request
# struct) and asserts this script REPORTS the drift, then restores the
# file byte-for-byte and re-asserts a clean run. A script that only ever
# printed "up to date" on every tree it was run against would still print
# "up to date" here, on a clean checkout, whether or not the diff below
# actually compares anything -- see CLAUDE.md, "a check can tell you
# whether it is consistent with itself; it cannot tell you what it is not
# looking at". The self-test's known-positive is what rules that out.
#
# EXIT STATUS
#
#   0  every tracked client matches what the generator produces right now
#   1  at least one is stale -- a real finding, shown as a diff
#   2  UNMEASURED -- cleat-gen itself failed to run, or a tracked file is
#      missing. This is a failure of the check, not a finding about any
#      client's freshness, and must never be read as "0 stale clients".
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT" || exit 2

# plugin-name | source package | output package name | committed file
#
# Both live under cleat/pluginclients/ -- inside the `cleat` SDK module,
# not inside examples/ -- and that placement is load-bearing, not
# cosmetic. `cleat build` stages a WASM workflow by globbing *.go
# non-recursively in the workflow's own source directory (wasm/build.go,
# PrepareBuildDir) and does not pull in sibling packages at all; a client
# generated into its own subpackage of examples/order-lifecycle/ could
# never be staged, and cleat#2626's own PR proved it live in CI -- `cleat
# build` failed with "module ... found, but does not contain package
# .../webhookingestclient", because examples/ is cleat's own separate,
# unpublished Go module and nothing local replaces it. cleat/, by
# contrast, is the one module every `cleat build` ALREADY locally
# replaces for every workflow (wasm/build.go's generated go.mod), so a
# client placed anywhere under cleat/ resolves the same way the `cleat`
# package itself already does -- no staging involved, so cleat#2658 (the
# staging gap itself) doesn't need to be fixed for this case to work. See
# cleat#2658 for the staging gap as a general problem, which a future
# out-of-tree or multi-file client would still hit.
CLIENTS=(
  "webhook-ingest|./plugins/webhookingest|webhookingest|cleat/pluginclients/webhookingest/client.go"
  "email-notify|./plugins/email|email|cleat/pluginclients/email/client.go"
  "audit-log|./plugins/auditlog|auditlog|cleat/pluginclients/auditlog/client.go"
)

check_one() {
  local spec="$1"
  local name pkg outpkg file
  IFS='|' read -r name pkg outpkg file <<<"$spec"

  if [ ! -f "$file" ]; then
    echo "UNMEASURED: tracked generated client is missing: $file" >&2
    return 2
  fi

  local want
  want="$(go run ./cmd/cleat-gen plugin-client -plugin "$name" -p "$outpkg" "$pkg" 2>&1)"
  local rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "UNMEASURED: cleat-gen plugin-client failed regenerating $file:" >&2
    echo "$want" >&2
    return 2
  fi

  local got
  got="$(cat "$file")"
  if [ "$want" != "$got" ]; then
    echo "STALE: $file no longer matches what cleat-gen plugin-client produces from $pkg" >&2
    echo "  regenerate with:" >&2
    echo "    go run ./cmd/cleat-gen plugin-client -plugin \"$name\" -p $outpkg -o $file $pkg" >&2
    echo "  diff:" >&2
    diff <(echo "$got") <(echo "$want") >&2
    return 1
  fi
  return 0
}

run_all() {
  local failures=0 unmeasured=0
  for spec in "${CLIENTS[@]}"; do
    check_one "$spec"
    case $? in
      0) ;;
      1) failures=$((failures + 1)) ;;
      *) unmeasured=$((unmeasured + 1)) ;;
    esac
  done
  if [ "$unmeasured" -gt 0 ]; then
    return 2
  fi
  if [ "$failures" -gt 0 ]; then
    return 1
  fi
  return 0
}

self_test() {
  local target="cleat/pluginclients/webhookingest/client.go"
  local backup
  backup="$(mktemp)"
  cp "$target" "$backup"
  trap 'cp "$backup" "$target"; rm -f "$backup"' EXIT

  if ! run_all >/dev/null 2>&1; then
    echo "SELF-TEST FAILED: the check is not clean on an untouched tree" >&2
    exit 2
  fi

  # Known-positive: delete a field from the committed struct, which is
  # exactly what a plugin author forgetting to regenerate would leave
  # behind.
  #
  # MATCHED BY REGEX, NOT A FIXED-WHITESPACE STRING -- found on cleat#2649,
  # which added a Keys field to this same struct. gofmt re-aligns every
  # field's column to the widest type in the struct (`[]string` is wider
  # than `string`), so EventType's own line gained padding spaces the
  # instant a sibling field's type got longer -- through no fault of the
  # generator, which check_one's own non-self-test run confirmed was still
  # producing byte-identical output. A byte-exact marker string breaks on
  # the next legitimate field addition exactly the way this one did; a
  # regex tolerant of the whitespace gofmt owns does not.
  if ! python3 - "$target" <<'EOF'
import re, sys
path = sys.argv[1]
src = open(path).read()
pattern = re.compile(r'\tEventType +string +`json:"event_type,omitempty"`\n')
if not pattern.search(src):
    print("SELF-TEST SETUP FAILED: expected field line not found", file=sys.stderr)
    sys.exit(2)
open(path, "w").write(pattern.sub("", src, count=1))
EOF
  then
    exit 2
  fi

  run_all >/dev/null 2>&1
  rc=$?
  if [ "$rc" -ne 1 ]; then
    echo "SELF-TEST FAILED: mutated client was not reported stale (exit $rc, want 1)" >&2
    exit 2
  fi

  cp "$backup" "$target"
  rm -f "$backup"
  trap - EXIT

  if ! run_all >/dev/null 2>&1; then
    echo "SELF-TEST FAILED: restore did not return the tree to clean" >&2
    exit 2
  fi

  echo "self-test passed: clean tree is clean, mutated tree is reported stale, restore is clean"
  exit 0
}

if [ "${1:-}" = "--self-test" ]; then
  self_test
fi

run_all
rc=$?
if [ "$rc" -eq 0 ]; then
  echo "generated plugin clients: up to date (${#CLIENTS[@]} checked)"
fi
exit "$rc"
