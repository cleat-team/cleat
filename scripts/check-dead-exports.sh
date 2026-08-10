#!/usr/bin/env bash
#
# Detect code that has zero callers anywhere -- not production, not tests.
#
# scripts/check-test-only-code.sh (which this script sits beside) runs
# staticcheck's U1000 with -tests=false, so anything an exported symbol's own
# _test.go file is the only caller of shows up as unused once that file is
# excluded from the analysis. That catches the "only tests call it" shape.
#
# It does NOT catch the "nothing calls it, including tests" shape, and it
# cannot: U1000 does not report exported identifiers in non-main packages at
# all, on the theory that a library's public API may be used by callers
# outside the scanned module. That theory is correct for cleat/ and
# pluginapi/, which genuinely are public API for external consumers. It is
# false for engine/, plugin/, wasm/, and friends: nothing outside this repo
# imports them, so an unreferenced exported symbol there is not "public API
# nobody has adopted yet", it is dead code that happens to start with a
# capital letter. engine/batch_flush.go (NewBatchFlusher, all-exported, all
# 0.0% coverage) and plugin/audit.go (NewAuditLog and 10 more, same story)
# both survived under check-test-only-code.sh's baseline for exactly this
# reason -- U1000 never saw them as findings to baseline in the first place.
#
# This script closes that gap with a blunter, complementary method: for each
# exported top-level func/method declared in an explicitly "internal" set of
# package roots (below), grep the whole tree for the bare identifier. If it
# appears in no file other than the one that declares it, nothing anywhere
# -- production or test -- ever spells its name, and it is reported.
#
# False-positive sources and how they're handled:
#
#   * Public API surface. cleat/, pluginapi/, crates/, python-sdk/, and
#     packages/ are deliberately excluded from the scanned roots below --
#     they exist specifically to be called from outside this repo, so an
#     internal grep finding nothing is the expected, correct state, not a
#     defect.
#
#   * Interface satisfaction with no textual call site. A method that exists
#     only to satisfy a standard interface (error, fmt.Stringer, io.Reader,
#     json.Marshaler, http.Handler, sql.Scanner, ...) can be invoked by the
#     runtime without any source line ever writing `.MethodName(`. Those
#     names are allowlisted in COMMON_INTERFACE_METHODS below and skipped
#     unconditionally -- they are common enough, and false-positive-prone
#     enough, that per-entry baseline review would not be worth much.
#
#   * Name collisions. A method named e.g. Deploy in one package is
#     indistinguishable, to a bare `grep -w`, from an unrelated Deploy
#     defined and called somewhere else in the tree. That direction of error
#     is a false NEGATIVE (a truly dead method hides behind an unrelated
#     same-named call) and is accepted: this script is intentionally
#     conservative about what it flags, per the same false-positive
#     discipline as check-test-only-code.sh.
#
#   * Build tags -- NOT a blind spot here, unlike check-test-only-code.sh's
#     staticcheck-based scan. scripts/finddeadexports.go extracts
#     declarations with go/parser.ParseFile directly, which parses a file's
#     syntax unconditionally and does not evaluate //go:build constraints at
#     all -- so engine/backend_wasmtime.go's declarations are seen and
#     grepped for the same as everything else, with or without
#     CGO_ENABLED=1. The same is true of the cross-reference grep, which is
#     plain text search over every .go file regardless of what would
#     actually compile together. A file that flat-out fails to parse (rare;
#     would mean a genuine syntax error, not a missing build tag) is skipped
#     with a warning on stderr rather than silently mis-scanned.
#
# Usage:
#   scripts/check-dead-exports.sh              # fail on entries not in the baseline
#   scripts/check-dead-exports.sh --update     # rewrite the baseline
#
# The baseline (scripts/deadexports-baseline.txt) exists for the same reason
# check-test-only-code.sh's does: there may be a backlog the day this lands.
# New entries fail the build; every baseline entry needs a reason recorded
# where it's added (commit message), same discipline as the sibling check.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT" || exit 1

# The baseline key is <file><TAB>func <Recv.Name> -- deliberately NO line number.
#
# It had one, and that made the guard fail on edits that changed nothing about
# what it measures: adding 18 lines anywhere above a baselined entry shifted its
# line number, the exact-line comparison below saw a string it had never seen,
# and it reported a function that had been dead and baselined for weeks as a
# brand-new dead export. That happened on the very PR that introduced this
# script -- 12 monitoring/prometheus Metrics methods, all already in the
# baseline, all flagged because unrelated edits moved them down the file.
#
# scripts/check-skips.sh and scripts/check-test-only-code.sh both key on the
# enclosing function for this reason, and say so; this script mirrors them, and
# now actually does. A baseline that churns on unrelated edits trains people to
# regenerate it without reading it, which is how a guard stops guarding.
BASELINE="scripts/deadexports-baseline.txt"

# Package roots scanned for exported declarations. Deliberately excludes:
#   cleat/        -- "Public Go API" (CLAUDE.md); has its own go.mod
#   pluginapi/    -- "Public re-exports for external plugin authors"
#   crates/       -- Rust + Java SDKs, not Go
#   python-sdk/   -- Python SDK, not Go
#   packages/     -- AssemblyScript SDK, not Go
#   examples/     -- example workflows, not library code with callers to find
#   tests/        -- integration suites; their exported helpers are consumed
#                    by other files under tests/, which this script does not
#                    special-case, and false positives there are cheap to
#                    hit given the suite layout -- left out rather than
#                    fought over
#   web/          -- Svelte, not Go
#   benchmarks/   -- comparative benchmark harnesses, own go.mod files
ROOTS=(engine wasm wasmrw plugin auth migration monitoring internal cmd plugins)

# Method names that can be invoked by the runtime (interface dispatch,
# encoding/json, encoding/sql, net/http, ...) with no source line anywhere
# ever writing `.Name(`. Skipped unconditionally, not baselined -- see the
# "Interface satisfaction" note above.
COMMON_INTERFACE_METHODS='^(Error|String|Unwrap|Is|As|Format|GoString|MarshalJSON|UnmarshalJSON|MarshalText|UnmarshalText|MarshalBinary|UnmarshalBinary|MarshalYAML|UnmarshalYAML|ServeHTTP|Read|Write|ReadFrom|WriteTo|Close|Len|Less|Swap|Scan|Value|Sort|Cap|Seek|Lock|Unlock|RLock|RUnlock|Done|Err|Deadline|Value|Visit|Walk|Init|Reset)$'

if [ "${1:-}" != "--update" ] && [ ! -f "$BASELINE" ]; then
  echo "ERROR: $BASELINE is missing. Generate it with:" >&2
  echo "  scripts/check-dead-exports.sh --update" >&2
  exit 1
fi

TMPDECLS="$(mktemp)"
TMPSTDERR="$(mktemp)"
trap 'rm -f "$TMPDECLS" "$TMPSTDERR"' EXIT

if ! go run scripts/finddeadexports.go "${ROOTS[@]}" > "$TMPDECLS" 2>"$TMPSTDERR"; then
  echo "ERROR: finddeadexports.go failed:" >&2
  cat "$TMPSTDERR" >&2
  exit 1
fi

if [ ! -s "$TMPDECLS" ]; then
  echo "ERROR: finddeadexports.go produced no declarations at all." >&2
  echo "That almost certainly means the scan is broken (wrong roots, parser" >&2
  echo "failure on everything) rather than that there are zero exported" >&2
  echo "top-level funcs/methods in ${ROOTS[*]}. Treating that as a clean" >&2
  echo "scan would be a vacuous pass -- exactly what this guard exists to" >&2
  echo "refuse. stderr from the scan:" >&2
  cat "$TMPSTDERR" >&2
  exit 1
fi

# For each declaration, grep the whole tree (all .go files) for the bare
# identifier and list which files contain a match. If the only file is the
# declaring file itself, nothing else in the tree ever spells the name.
findings=""
while IFS=$'\t' read -r file line recv name; do
  if [[ "$name" =~ $COMMON_INTERFACE_METHODS ]]; then
    continue
  fi

  # -w: whole word, so Deploy does not match Deployment.
  # -l: filenames only. Normalize the leading "./" grep prints (searching
  # from ".") away before comparing against $file, which finddeadexports.go
  # reports without one -- without this the declaring file never matches
  # itself in the comparison, every declaration looks like it has an
  # "other" reference (itself), and the whole check passes vacuously.
  # Caught by the red/green probe: see the commit that added this script.
  matches="$(grep -rlw --include='*.go' -- "$name" . 2>/dev/null | sed 's#^\./##')"
  other_files="$(printf '%s\n' "$matches" | grep -Fxv "$file" || true)"

  used=1
  if [ -n "$(printf '%s' "$other_files" | tr -d '[:space:]')" ]; then
    used=0
  else
    # No other file mentions it. It can still be genuinely used by another
    # function declared in the SAME file (e.g. a small package-private
    # helper called from one exported wrapper in the same file) -- check
    # for a match on any line of the declaring file other than the
    # declaration line itself. Without this, PostgresRLSDSN (called from
    # PostgresTestDSN two lines down in the same file, engine/testutil/
    # schema.go) was a false positive: its only match was its own file, and
    # the "other file" test alone treated that as zero callers.
    #
    # Every exported Go declaration conventionally has a doc comment that
    # repeats its own name on the first line ("// Foo does X."). A first
    # version of this fix counted that comment line as "another line", so
    # every commented declaration looked used by its own doc comment and
    # the check went silently vacuous -- caught by re-checking
    # SetWasmCacheEntries (monitoring/prometheus/metrics.go), which has no
    # caller anywhere yet stopped being reported the moment this fix
    # landed. Comment-only lines (line's first non-blank characters are
    # "//") are therefore excluded from the evidence.
    all_lines="$(grep -nw -- "$name" "$file" 2>/dev/null || true)"
    while IFS=: read -r ln content; do
      [ -z "$ln" ] && continue
      [ "$ln" = "$line" ] && continue   # the declaration line itself
      trimmed="${content#"${content%%[![:space:]]*}"}"
      case "$trimmed" in
        //*) ;;                 # doc/line comment -- not real usage evidence
        *) used=0 ;;
      esac
    done <<< "$all_lines"
  fi

  if [ "$used" -eq 0 ]; then
    continue
  fi

  label="$name"
  if [ "$recv" != "-" ]; then
    label="${recv}.${name}"
  fi
  findings="${findings}${file}	func ${label}"$'\n'
done < "$TMPDECLS"

findings="$(printf '%s' "$findings" | grep -v '^$' | LC_ALL=C sort -u || true)"

if [ "${1:-}" = "--update" ]; then
  printf '%s\n' "$findings" > "$BASELINE"
  echo "Wrote $(grep -c . "$BASELINE" || true) entries to $BASELINE"
  exit 0
fi

new="$(printf '%s\n' "$findings" | grep -Fxv -f "$BASELINE" || true)"
new="$(printf '%s' "$new" | grep -v '^$' || true)"

if [ -n "$new" ]; then
  echo "ERROR: exported code with zero callers anywhere in the tree (not even tests):" >&2
  echo >&2
  printf '%s\n' "$new" | sed 's/^/  /' >&2
  echo >&2
  echo "Either wire it into production, delete it, or -- if it is genuinely" >&2
  echo "meant as public API not yet adopted -- move it under cleat/ or" >&2
  echo "pluginapi/ (the packages this script treats as public surface), or" >&2
  echo "add it to $BASELINE with a reason in the commit message via" >&2
  echo "  scripts/check-dead-exports.sh --update" >&2
  exit 1
fi

echo "OK: no new dead exports ($(grep -c . "$BASELINE" || true) known entries in the baseline)."
