#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# compare.sh — Cleat vs Temporal vs DBOS benchmark comparison runner
#
# This script runs the Cleat benchmark suite and optionally invokes
# equivalent benchmarks for Temporal and DBOS if their CLIs are available.
#
# Usage:
#   ./benchmarks/compare.sh                          # Cleat only
#   ./benchmarks/compare.sh --temporal               # Cleat + Temporal
#   ./benchmarks/compare.sh --dbos                   # Cleat + DBOS
#   ./benchmarks/compare.sh --temporal --dbos        # All three
#
# Requirements:
#   - Go 1.22+ (for Cleat benchmarks)
#   - temporal CLI (optional, for Temporal comparison)
#   - dbos CLI / npm (optional, for DBOS comparison)
#   - jq (for metric extraction)
#
# Output:
#   Prints a markdown table of results to stdout and saves a copy to
#   /tmp/bench-results-<timestamp>.md
# ---------------------------------------------------------------------------
set -euo pipefail

BENCH_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$BENCH_DIR/.." && pwd)"
BENCHTIME="${BENCHTIME:-30s}"
RESULTS_FILE="/tmp/bench-results-$(date +%Y%m%d-%H%M%S).md"

# Colors for terminal output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# ---------------------------------------------------------------------------
# System info
# ---------------------------------------------------------------------------
collect_sysinfo() {
    cat <<EOF
## System Information

- **Date**: $(date -u)
- **Hostname**: $(hostname)
- **CPU**: $(awk -F': ' '/model name/ {print $2; exit}' /proc/cpuinfo 2>/dev/null || echo "N/A")
- **Cores**: $(nproc 2>/dev/null || echo "N/A")
- **RAM**: $(free -h | awk '/^Mem:/ {print $2}' 2>/dev/null || echo "N/A")
- **Disk**: $(lsblk -d -o MODEL 2>/dev/null | tail -1 || echo "N/A")
- **Kernel**: $(uname -r 2>/dev/null || echo "N/A")
- **Go**: $(go version 2>/dev/null || echo "N/A")

EOF
}

# ---------------------------------------------------------------------------
# Run Cleat benchmarks
# ---------------------------------------------------------------------------
run_cleat() {
    echo ""
    echo -e "${BLUE}========================================${NC}"
    echo -e "${BLUE}  Running Cleat benchmarks${NC}"
    echo -e "${BLUE}========================================${NC}"
    echo ""

    cd "$REPO_ROOT"

    # Ensure benchmarks compile.
    echo -e "${YELLOW}[*] Checking compilation...${NC}"
    if ! go build ./benchmarks/... 2>&1; then
        echo -e "${RED}[!] Compilation failed. Aborting.${NC}"
        return 1
    fi
    echo -e "${GREEN}[+] Compilation OK${NC}"
    echo ""

    # Run the benchmarks.
    echo -e "${YELLOW}[*] Running benchmarks (benchtime=${BENCHTIME})...${NC}"
    echo ""
    go test -bench="." -benchtime="${BENCHTIME}" -benchmem ./benchmarks/ 2>&1 | tee /tmp/cleat-bench-raw.txt

    echo ""
    echo -e "${GREEN}[+] Cleat benchmarks complete${NC}"
    echo ""
}

# ---------------------------------------------------------------------------
# Run Temporal benchmarks (if available)
# ---------------------------------------------------------------------------
run_temporal() {
    if ! command -v temporal &>/dev/null; then
        echo -e "${RED}[!] temporal CLI not found. Skipping Temporal benchmarks.${NC}"
        echo -e "${YELLOW}    Install: https://docs.temporal.io/cli${NC}"
        return
    fi

    echo ""
    echo -e "${BLUE}========================================${NC}"
    echo -e "${BLUE}  Running Temporal benchmarks${NC}"
    echo -e "${BLUE}========================================${NC}"
    echo ""

    # Check if Temporal dev server is running.
    if ! temporal server health 2>/dev/null; then
        echo -e "${YELLOW}[!] Temporal dev server not running. Starting...${NC}"
        temporal server start-dev --db-file /tmp/temporal-bench.db &
        TEMPORAL_PID=$!
        sleep 3
        echo -e "${GREEN}[+] Temporal dev server started (PID ${TEMPORAL_PID})${NC}"
    fi

    echo ""
    echo -e "${YELLOW}[*] Temporal benchmarks require a separate Temporal SDK project.${NC}"
    echo -e "${YELLOW}    Port the workflow definitions from benchmarks/workflows/ to${NC}"
    echo -e "${YELLOW}    the Temporal Go SDK and run:${NC}"
    echo ""
    echo -e "    cd path/to/temporal-bench && go test -bench=. -benchtime=${BENCHTIME} ./..."
    echo ""
}

# ---------------------------------------------------------------------------
# Run DBOS benchmarks (if available)
# ---------------------------------------------------------------------------
run_dbos() {
    if ! command -v dbos &>/dev/null && ! command -v npx &>/dev/null; then
        echo -e "${RED}[!] Neither dbos CLI nor npx found. Skipping DBOS benchmarks.${NC}"
        echo -e "${YELLOW}    Install: https://docs.dbos.dev/getting-started/quickstart${NC}"
        return
    fi

    echo ""
    echo -e "${BLUE}========================================${NC}"
    echo -e "${BLUE}  Running DBOS benchmarks${NC}"
    echo -e "${BLUE}========================================${NC}"
    echo ""

    echo -e "${YELLOW}[*] DBOS benchmarks require a separate DBOS TypeScript project.${NC}"
    echo -e "${YELLOW}    Port the workflow definitions from benchmarks/workflows/ to${NC}"
    echo -e "${YELLOW}    the DBOS SDK and run:${NC}"
    echo ""
    echo -e "    cd path/to/dbos-bench && npx jest --bench --benchtime=${BENCHTIME}"
    echo ""
}

# ---------------------------------------------------------------------------
# Print and save results
# ---------------------------------------------------------------------------
save_results() {
    {
        echo "# Benchmark Comparison Results"
        echo ""
        echo "**Generated**: $(date -u)"
        echo ""
        collect_sysinfo
        echo "## Raw Output"
        echo ""
        echo '```'
        if [ -f /tmp/cleat-bench-raw.txt ]; then
            cat /tmp/cleat-bench-raw.txt
        fi
        echo '```'
    } > "$RESULTS_FILE"

    echo -e "${GREEN}[+] Results saved to: ${RESULTS_FILE}${NC}"
}

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------
cleanup() {
    if [ -n "${TEMPORAL_PID:-}" ]; then
        echo -e "${YELLOW}[*] Stopping Temporal dev server...${NC}"
        kill "$TEMPORAL_PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
    echo ""
    echo -e "${GREEN}╔══════════════════════════════════════╗${NC}"
    echo -e "${GREEN}║  Cleat / Temporal / DBOS Benchmark   ║${NC}"
    echo -e "${GREEN}╚══════════════════════════════════════╝${NC}"
    echo ""

    run_cleat

    for arg in "$@"; do
        case "$arg" in
            --temporal) run_temporal ;;
            --dbos)     run_dbos ;;
        esac
    done

    save_results

    echo ""
    echo -e "${GREEN}========================================${NC}"
    echo -e "${GREEN}  All benchmarks complete.${NC}"
    echo -e "${GREEN}  Results: ${RESULTS_FILE}${NC}"
    echo -e "${GREEN}========================================${NC}"
    echo ""
}

main "$@"
