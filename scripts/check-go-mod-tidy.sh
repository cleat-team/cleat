#!/usr/bin/env bash
#
# Guard against cross-module go.mod drift.
#
# This repo has several Go modules, four of which are wired together with
# `replace` directives:
#
#     go.mod                 replace github.com/cleat-team/cleat/cleat => ./cleat
#     cleat/go.mod           replace github.com/cleat-team/cleat => ../
#     cleat/backendkit/go.mod replace github.com/cleat-team/cleat => ../..
#     wasm-demo/go.mod       replace ... => ../ and ../cleat
#
# Because of those replaces, changing a dependency in the root module can
# leave the others stale. `go build`/`go vet` at the root will not notice --
# each module is only checked when a command runs *inside* it.
#
# That is not hypothetical. Commit 93f8abf bumped google.golang.org/grpc in
# the root module for GO-2026-6061; cleat/go.mod went stale, and
# `go test ./...` inside cleat/ failed with "updates to go.mod needed".
# Nothing caught it, because the CI lane that runs inside cleat/ had never
# worked (it failed at setup and was reported green by a `tee` without
# `set -o pipefail`). It surfaced only once that lane was repaired.
#
# `go mod tidy -diff` (Go 1.23+) reports what tidy *would* change without
# writing anything, and exits non-zero when a module is untidy. Run it in
# every module so the drift becomes a build failure instead of a silence.
#
# Usage: scripts/check-go-mod-tidy.sh

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# benchmarks/comparative/** are standalone modules that exist to pin specific
# competitor releases (Temporal, DBOS) for like-for-like measurement. Their
# dependency sets are deliberate and tidying them would require network access
# to unrelated ecosystems, so they are checked out of scope here.
EXEMPT_PREFIX="benchmarks/comparative/"

failed=0
checked=0

# Portable: no `mapfile` (bash 4+), no `find -printf` (GNU only), so this also
# runs on a stock macOS shell.
# The root module's directory would reduce to the empty string, which word
# splitting in the loop below would silently drop -- so it is emitted as "."
# instead. (It was dropped, in the first version of this script, which
# reported "3 modules" when there are 4.)
mods="$(find . -name go.mod -not -path './node_modules/*' -not -path '*/node_modules/*' 2>/dev/null |
  sed 's|^\./||; s|/\{0,1\}go\.mod$||; s|^$|.|' | sort)"

for dir in $mods; do
  case "$dir/" in "$EXEMPT_PREFIX"*) continue ;; esac

  checked=$((checked + 1))
  out="$(cd "$REPO_ROOT/$dir" && go mod tidy -diff 2>&1)"
  if [ $? -ne 0 ]; then
    echo "ERROR: $dir/go.mod is not tidy. \`cd $dir && go mod tidy\` would change it:" >&2
    echo "$out" | head -40 >&2
    echo >&2
    failed=1
  fi
done

if [ "$failed" -ne 0 ]; then
  echo "Modules in this repo are linked by replace directives, so a dependency" >&2
  echo "change in one can leave another stale. Run \`go mod tidy\` in the module" >&2
  echo "reported above and commit the result." >&2
  exit 1
fi

echo "OK: all $checked Go modules are tidy."
