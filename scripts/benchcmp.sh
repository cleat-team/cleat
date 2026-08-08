#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# benchcmp.sh — Benchmark regression detection
#
# Runs the full Go benchmark suite and compares results against a stored
# baseline.  If the delta for any benchmark metric exceeds 5 % the script
# exits non-zero (a regression).
#
# Usage:
#   ./scripts/benchcmp.sh                   # compare or create baseline
#   ./scripts/benchcmp.sh --force-save      # overwrite baseline unconditionally
#
# Baseline files are stored under:
#   .benchmarks/baseline-$(arch)-go$(MAJOR.MINOR).txt
#
# Requirements:
#   - Go 1.22+
#   - benchstat (optional — installed automatically if missing)
#     go install golang.org/x/perf/cmd/benchstat@latest
# ---------------------------------------------------------------------------
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
BENCH_DIR="$REPO_ROOT/benchmarks"
BASELINE_DIR="$REPO_ROOT/.benchmarks"

BENCHTIME="${BENCHTIME:-30s}"
ARCH="$(uname -m)"
GO_VERSION="$(go version | cut -d' ' -f3 | cut -d'.' -f1,2)"     # e.g. 1.24
BASELINE_FILE="$BASELINE_DIR/baseline-${ARCH}-go${GO_VERSION}.txt"
CURRENT_FILE=$(mktemp /tmp/bench-current-XXXXXX.txt)
FORCE_SAVE=false

# Colours
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------
# Invoked indirectly via the `trap` below, which the linter cannot see.
# Both codes are listed because they are one check under two names:
# 0.11 reports SC2329 (function never invoked); older builds report
# SC2317 (command appears unreachable). CI's apt build was the older one,
# so an SC2329-only suppression passed locally and failed there. The
# version is pinned in ci.yml now; the pair stays until that pin is old
# enough that nothing runs a pre-0.10 build.
# (No line above may begin with the linter's own name -- it would be
# parsed as a directive rather than read as prose. SC1073.)
# shellcheck disable=SC2329,SC2317
cleanup() {
    rm -f "$CURRENT_FILE"
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Helper: print a coloured status line
# ---------------------------------------------------------------------------
pass() { echo -e "  ${GREEN}[PASS]${NC} $*"; }
fail() { echo -e "  ${RED}[FAIL]${NC} $*"; }
warn() { echo -e "  ${YELLOW}[WARN]${NC} $*"; }

# ---------------------------------------------------------------------------
# Step 1 — run benchmarks
# ---------------------------------------------------------------------------
run_benchmarks() {
    echo ""
    echo "============================================"
    echo "  Running benchmarks (benchtime=${BENCHTIME})..."
    echo "============================================"
    echo ""

    cd "$REPO_ROOT"
    go test -bench="." -benchtime="${BENCHTIME}" -benchmem "$BENCH_DIR" | tee "$CURRENT_FILE"

    local exit_code="${PIPESTATUS[0]}"
    if [ "$exit_code" -ne 0 ]; then
        echo ""
        fail "Benchmarks failed with exit code $exit_code"
        return "$exit_code"
    fi

    echo ""
    pass "Benchmarks completed successfully"
    echo ""
}

# ---------------------------------------------------------------------------
# Step 2 — ensure benchstat is available
# ---------------------------------------------------------------------------
ensure_benchstat() {
    if command -v benchstat &>/dev/null; then
        return 0
    fi

    warn "benchstat not found — attempting to install..."
    go install golang.org/x/perf/cmd/benchstat@latest

    local gopath
    gopath="$(go env GOPATH)"
    if [ -x "$gopath/bin/benchstat" ]; then
        export PATH="$gopath/bin:$PATH"
        pass "benchstat installed"
        return 0
    fi

    warn "benchstat installation failed — will fall back to manual comparison"
    return 1
}

# ---------------------------------------------------------------------------
# Step 2a — compare with benchstat
# ---------------------------------------------------------------------------
compare_with_benchstat() {
    local baseline="$1"
    local current="$2"

    echo ""
    echo "--- benchstat comparison ---"
    echo "  Baseline: $baseline"
    echo "  Current:  $current"
    echo ""

    local benchstat_output
    benchstat_output=$(benchstat "$baseline" "$current")
    echo "$benchstat_output"

    # Check if any delta exceeds ±5 %
    local regressions
    regressions=$(echo "$benchstat_output" \
        | grep -E '^[a-zA-Z]' \
        | awk '{
            for (i=1; i<=NF; i++) {
                if ($i ~ /^[+-]/ && $i ~ /%/) {
                    gsub(/[+%]/, "", $i)
                    if ($i + 0 > 5.0) print
                }
            }
        }' \
    )

    if [ -n "$regressions" ]; then
        echo ""
        fail "Regressions (>5 %) detected:"
        echo "$regressions"
        return 1
    fi

    echo ""
    pass "No regressions detected (all deltas ≤5 %)"
    return 0
}

# ---------------------------------------------------------------------------
# Step 2b — manual comparison (fallback when benchstat unavailable)
# ---------------------------------------------------------------------------
compare_manual() {
    local baseline="$1"
    local current="$2"

    echo ""
    echo "--- Manual comparison (benchstat unavailable) ---"
    echo "  Baseline: $baseline"
    echo "  Current:  $current"
    echo ""

    # Extract benchmark name and ns/op from both files.
    # Go output format:
    #   BenchmarkXxx-N         1000    123456 ns/op    65432 B/op    123 allocs/op
    parse_nsop() {
        grep -E '^Benchmark' "$1" \
            | awk '{
                name = $1
                sub(/-[0-9]+$/, "", name)          # strip GOMAXPROCS suffix
                ns = $3
                gsub(/[^0-9.]/, "", ns)            # keep only numeric part
                if (ns != "") print name, ns
            }'
    }

    local tmp_baseline tmp_current
    tmp_baseline=$(mktemp)
    tmp_current=$(mktemp)
    parse_nsop "$baseline" > "$tmp_baseline"
    parse_nsop "$current"  > "$tmp_current"

    local any_regression=false

    while read -r name new_ns; do
        old_ns=$(grep "^$name " "$tmp_baseline" | awk '{print $2}' || true)
        if [ -z "$old_ns" ]; then
            warn "No baseline for $name — skipping"
            continue
        fi

        # Compute delta as percentage
        delta=$(awk -v old="$old_ns" -v new="$new_ns" 'BEGIN {
            if (old + 0 == 0) { print 0; exit }
            printf "%.2f", (new - old) / old * 100
        }')

        if awk -v d="$delta" 'BEGIN { exit (d > 5.0 || d < -5.0) ? 0 : 1 }'; then
            fail "Regression: $name  (${old_ns} → ${new_ns} ns/op, ${delta}%)"
            any_regression=true
        else
            pass "$name  (${old_ns} → ${new_ns} ns/op, ${delta}%)"
        fi
    done < "$tmp_current"

    rm -f "$tmp_baseline" "$tmp_current"

    if [ "$any_regression" = true ]; then
        fail "Regressions (>5 %) detected — see above"
        return 1
    fi

    pass "No regressions detected (all deltas ≤5 %)"
    return 0
}

# ---------------------------------------------------------------------------
# Step 3 — save baseline (if none exists or --force-save)
# ---------------------------------------------------------------------------
save_baseline() {
    mkdir -p "$BASELINE_DIR"
    cp "$CURRENT_FILE" "$BASELINE_FILE"
    pass "Baseline saved to $BASELINE_FILE"
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
    for arg in "$@"; do
        case "$arg" in
            --force-save) FORCE_SAVE=true ;;
            --help|-h)
                echo "Usage: $0 [--force-save]"
                echo ""
                echo "  --force-save    Overwrite baseline unconditionally"
                exit 0
                ;;
        esac
    done

    echo ""
    echo "============================================"
    echo "  benchcmp — Benchmark Regression Detection"
    echo "============================================"
    echo "  Arch:       $ARCH"
    echo "  Go version: $GO_VERSION"
    echo "  Benchtime:  $BENCHTIME"
    echo "  Baseline:   ${BASELINE_FILE:-<none>}"
    echo ""

    # Step 1 — run benchmarks
    if ! run_benchmarks; then
        exit 1
    fi

    # Decide: compare or save baseline?
    if [ -f "$BASELINE_FILE" ] && [ "$FORCE_SAVE" = false ]; then
        # Step 2 — compare against existing baseline
        if ensure_benchstat; then
            compare_with_benchstat "$BASELINE_FILE" "$CURRENT_FILE"
            rc=$?
        else
            compare_manual "$BASELINE_FILE" "$CURRENT_FILE"
            rc=$?
        fi
    else
        # No baseline yet (or --force-save) — save current as baseline
        save_baseline
        warn "No baseline existed — saved current results as baseline."
        warn "Run again to detect regressions."
        rc=0
    fi

    echo ""
    if [ "$rc" -eq 0 ]; then
        echo -e "${GREEN}============================================${NC}"
        echo -e "${GREEN}  All checks passed${NC}"
        echo -e "${GREEN}============================================${NC}"
    else
        echo -e "${RED}============================================${NC}"
        echo -e "${RED}  Regressions detected — see above${NC}"
        echo -e "${RED}============================================${NC}"
    fi
    echo ""

    exit "$rc"
}

main "$@"
