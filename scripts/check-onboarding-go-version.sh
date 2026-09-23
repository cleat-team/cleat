#!/usr/bin/env bash
#
# go.mod's `go` directive is the project's real Go minimum. Two onboarding
# files restate it in prose instead of reading it, and both drifted:
# docs/tutorials/quick-start.md said "Go 1.25+" and .devcontainer/devcontainer.json
# pinned image tag "1.25", while go.mod had already moved to 1.26.0 (cleat#2070).
#
# Neither drift breaks a build -- go.work's floor guard (check-go-work-floor.sh)
# covers the workspace files that DO break a build. These two are read by a
# human deciding whether to bother installing a newer toolchain, or by a
# container image tag nothing else verifies, so nothing red tells you they are
# wrong. This check exists because nothing else would.
#
# WHY IT FAILS WHEN IT FINDS NOTHING, same as check-go-work-floor.sh: a scan
# that cannot find the line it means to check agrees with every tree, stale or
# not. Each of the three reads below is a known-positive/negative pair in
# --self-test, and a parse that matches neither exits 2, not 0.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
GOMOD="${1:-$ROOT/go.mod}"
QUICKSTART="${2:-$ROOT/docs/tutorials/quick-start.md}"
DEVCONTAINER="${3:-$ROOT/.devcontainer/devcontainer.json}"

# go_minor FILE -- the go.mod `go` directive's MAJOR.MINOR, e.g. "1.26" from
# "go 1.26.0". Anchored to the start of the line, like check-go-work-floor.sh:
# a `toolchain go1.26.0` line or a `go` token inside a require block must not
# match.
go_minor() {
  sed -n 's/^go[[:space:]]\{1,\}\([0-9][0-9]*\.[0-9][0-9]*\)\.[0-9][0-9]*[[:space:]]*$/\1/p' "$1" | head -1
}

# quickstart_minor FILE -- the "Go X.Y+" prerequisite line's MAJOR.MINOR.
quickstart_minor() {
  grep -oE '\*\*Go [0-9]+\.[0-9]+\+\*\*' "$1" | head -1 | grep -oE '[0-9]+\.[0-9]+'
}

# devcontainer_minor FILE -- the devcontainers/go image tag's MAJOR.MINOR.
devcontainer_minor() {
  grep -oE '"image":[[:space:]]*"mcr\.microsoft\.com/devcontainers/go:[0-9]+\.[0-9]+"' "$1" |
    head -1 | grep -oE '[0-9]+\.[0-9]+"$' | tr -d '"'
}

check_onboarding() {
  local gomod="$1" quickstart="$2" devcontainer="$3"
  local floor qs dc failed=0

  if [ ! -f "$gomod" ]; then
    echo "CHECK FAILED: no go.mod at $gomod -- this guard could not run." >&2
    return 2
  fi
  floor="$(go_minor "$gomod")"
  if [ -z "$floor" ]; then
    echo "CHECK FAILED: no 'go' directive found in $gomod -- this guard could not run." >&2
    return 2
  fi

  if [ ! -f "$quickstart" ]; then
    echo "CHECK FAILED: no quick-start doc at $quickstart -- this guard could not run." >&2
    return 2
  fi
  qs="$(quickstart_minor "$quickstart")"
  if [ -z "$qs" ]; then
    echo "CHECK FAILED: no '**Go X.Y+**' prerequisite line found in $quickstart -- this guard could not run." >&2
    return 2
  fi

  if [ ! -f "$devcontainer" ]; then
    echo "CHECK FAILED: no devcontainer.json at $devcontainer -- this guard could not run." >&2
    return 2
  fi
  dc="$(devcontainer_minor "$devcontainer")"
  if [ -z "$dc" ]; then
    echo "CHECK FAILED: no devcontainers/go image tag found in $devcontainer -- this guard could not run." >&2
    return 2
  fi

  if [ "$qs" != "$floor" ]; then
    echo "$quickstart states Go $qs+, but go.mod's floor is $floor." >&2
    echo "  A newcomer following the quick-start installs a toolchain go.mod already rejects." >&2
    failed=1
  fi
  if [ "$dc" != "$floor" ]; then
    echo "$devcontainer pins devcontainers/go:$dc, but go.mod's floor is $floor." >&2
    echo "  postCreateCommand's 'go build ./...' then pays for the gap via toolchain auto-download," >&2
    echo "  on every container build, instead of the image already matching." >&2
    failed=1
  fi

  [ "$failed" -eq 0 ] || return 1
  echo "quick-start.md and devcontainer.json both state Go $floor, matching go.mod's floor. OK"
  return 0
}

# --self-test builds a fixture this check MUST reject (both onboarding files
# behind go.mod, the actual cleat#2070 state) and one it MUST accept (all
# three agreeing), and fails if either verdict is wrong.
self_test() {
  local tmp rc status=0
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  mkdir -p "$tmp/docs/tutorials" "$tmp/.devcontainer"
  printf 'module fixture\n\ngo 1.26.0\n' > "$tmp/go.mod"

  # KNOWN-POSITIVE: the actual cleat#2070 state before the fix. rc=1.
  printf -- '- **Go 1.25+** -- [Download](https://go.dev/dl/)\n' > "$tmp/docs/tutorials/quick-start.md"
  printf '{\n  "image": "mcr.microsoft.com/devcontainers/go:1.25"\n}\n' > "$tmp/.devcontainer/devcontainer.json"
  rc=0
  check_onboarding "$tmp/go.mod" "$tmp/docs/tutorials/quick-start.md" "$tmp/.devcontainer/devcontainer.json" \
    >/dev/null 2>&1 || rc=$?
  if [ "$rc" -ne 1 ]; then
    echo "SELF-TEST FAILED: both files behind go.mod returned rc=$rc, expected 1." >&2
    status=1
  fi

  # PARTIAL: only the devcontainer is behind. Still rc=1.
  printf -- '- **Go 1.26+** -- [Download](https://go.dev/dl/)\n' > "$tmp/docs/tutorials/quick-start.md"
  rc=0
  check_onboarding "$tmp/go.mod" "$tmp/docs/tutorials/quick-start.md" "$tmp/.devcontainer/devcontainer.json" \
    >/dev/null 2>&1 || rc=$?
  if [ "$rc" -ne 1 ]; then
    echo "SELF-TEST FAILED: devcontainer alone behind go.mod returned rc=$rc, expected 1." >&2
    status=1
  fi

  # NEGATIVE CONTROL: all three agree. rc=0.
  printf '{\n  "image": "mcr.microsoft.com/devcontainers/go:1.26"\n}\n' > "$tmp/.devcontainer/devcontainer.json"
  rc=0
  check_onboarding "$tmp/go.mod" "$tmp/docs/tutorials/quick-start.md" "$tmp/.devcontainer/devcontainer.json" \
    >/dev/null 2>&1 || rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "SELF-TEST FAILED: all three agreeing returned rc=$rc, expected 0." >&2
    status=1
  fi

  # UNPARSEABLE quick-start line: must be rc=2, a failure of the check, not a pass.
  printf -- 'Go is required, any recent version.\n' > "$tmp/docs/tutorials/quick-start.md"
  rc=0
  check_onboarding "$tmp/go.mod" "$tmp/docs/tutorials/quick-start.md" "$tmp/.devcontainer/devcontainer.json" \
    >/dev/null 2>&1 || rc=$?
  if [ "$rc" -ne 2 ]; then
    echo "SELF-TEST FAILED: an unparseable quick-start line returned rc=$rc, expected 2." >&2
    status=1
  fi

  if [ "$status" -eq 0 ]; then
    echo "self-test: 4 cases, all verdicts as expected. OK"
  fi
  return "$status"
}

if [ "${1:-}" = "--self-test" ]; then
  self_test
  exit $?
fi

check_onboarding "$GOMOD" "$QUICKSTART" "$DEVCONTAINER"
