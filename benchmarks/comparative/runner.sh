#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# runner.sh — Unified Cleat vs Temporal vs DBOS benchmark comparison runner
#
# Auto-detects available frameworks, runs each workload on each framework,
# extracts metrics programmatically, and generates a markdown comparison
# table plus a CSV results file.
#
# Usage:
#   ./runner.sh [--cleat-only] [--temporal-only] [--dbos-only]
#               [--warmup 10s] [--benchtime 60s] [--concurrency N]
#
#   Without flags: runs all available frameworks.
#   With framework flags: runs only the specified frameworks.
#
# Output:
#   benchmarks/comparative/results/results-YYYY-MM-DD-HHMMSS.md  (markdown)
#   benchmarks/comparative/results/results-YYYY-MM-DD-HHMMSS.csv  (tabular)
#
# Requirements:
#   - Go 1.22+ (for Cleat and Temporal benchmarks)
#   - temporal CLI (optional, for Temporal benchmarks)
#   - npx / ts-node + @dbos-inc/dbos-sdk (optional, for DBOS)
#   - jq (for metric extraction from JSON if needed)
#   - PostgreSQL (for DBOS benchmarks)
# ---------------------------------------------------------------------------
set -euo pipefail

# ---------------------------------------------------------------------------
# Constants & paths
# ---------------------------------------------------------------------------
SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
BENCH_DIR="$(cd "$SELF_DIR/.." && pwd)"
REPO_ROOT="$(cd "$BENCH_DIR/.." && pwd)"
RESULTS_DIR="$SELF_DIR/results"
TIMESTAMP="$(date +%Y-%m-%d-%H%M%S)"
RESULTS_MD="${RESULTS_DIR}/results-${TIMESTAMP}.md"
RESULTS_CSV="${RESULTS_DIR}/results-${TIMESTAMP}.csv"
TEMPLATE_MD="${RESULTS_DIR}/template.md"

# Default benchmark parameters
WARMUP="${WARMUP:-10s}"
BENCHTIME="${BENCHTIME:-60s}"
CONCURRENCY="${CONCURRENCY:-10}"

# Temp files for raw output capture
TMPDIR="$(mktemp -d /tmp/cleat-bench-XXXXXX)"
CLEAT_RAW="${TMPDIR}/cleat-raw.txt"
TEMPORAL_RAW="${TMPDIR}/temporal-raw.txt"
DBOS_RAW="${TMPDIR}/dbos-raw.txt"
PARSED="${TMPDIR}/parsed.txt"

# Cleanup on exit
cleanup() {
    rm -rf "$TMPDIR"
    if [ -n "${TEMPORAL_PID:-}" ]; then
        kill "$TEMPORAL_PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT

# Colors (disabled if not a terminal)
if [ -t 1 ]; then
    RED='\033[0;31m'
    GREEN='\033[0;32m'
    YELLOW='\033[1;33m'
    BLUE='\033[0;34m'
    CYAN='\033[0;36m'
    NC='\033[0m'
else
    RED=''; GREEN=''; YELLOW=''; BLUE=''; CYAN=''; NC=''
fi

# ---------------------------------------------------------------------------
# Helper: parse BENCHMARK_RESULT lines
# Format: BENCHMARK_RESULT  name=WorkloadName  config=key=val  count=N  elapsed=T  wf_per_sec=X  steps_per_sec=Y
# ---------------------------------------------------------------------------
declare -A RESULTS  # associative array keyed by "workload|config" -> "framework|steps_per_sec|wf_per_sec"

store_result() {
    local framework="$1"
    local workload="$2"
    local config="$3"
    local steps_s="$4"
    local wf_s="$5"
    local key="${workload}|${config}"
    # Append to existing if there are multiple runs (one per line). We keep
    # all runs and compute median later.
    echo "${framework}|${workload}|${config}|${steps_s}|${wf_s}" >> "$PARSED"
}

# ---------------------------------------------------------------------------
# Helper: compute median of a list of numbers
# ---------------------------------------------------------------------------
median() {
    local -a nums=("$@")
    local count="${#nums[@]}"
    if [ "$count" -eq 0 ]; then
        echo "0"
        return
    fi
    # Sort numerically
    IFS=$'\n' nums=($(sort -n <<<"${nums[*]}")); unset IFS
    local mid=$((count / 2))
    if ((count % 2 == 0)); then
        # Even: average of two middle values
        awk "BEGIN { printf \"%.2f\", (${nums[mid-1]} + ${nums[mid]}) / 2 }"
    else
        echo "${nums[mid]}"
    fi
}

# ---------------------------------------------------------------------------
# Helper: compute ratio with significance flag
# ---------------------------------------------------------------------------
format_ratio() {
    local cleat_val="$1"
    local other_val="$2"
    if [ "$(echo "$other_val == 0" | bc -l 2>/dev/null)" = "1" ] || [ -z "$other_val" ] || [ "$other_val" = "0" ]; then
        echo "N/A"
        return
    fi
    local ratio
    ratio=$(echo "scale=2; $cleat_val / $other_val" | bc 2>/dev/null || echo "0")
    if [ -z "$ratio" ] || [ "$ratio" = "0" ]; then
        echo "N/A"
        return
    fi
    # Check if within 10% (ratio 0.91-1.10)
    local within_noise
    within_noise=$(echo "$ratio >= 0.91 && $ratio <= 1.10" | bc -l 2>/dev/null || echo "0")
    if [ "$within_noise" = "1" ]; then
        echo "~ (${ratio}x)"
    elif [ "$(echo "$ratio > 1.0" | bc -l)" = "1" ]; then
        echo "${ratio}x faster"
    else
        # ratio < 1.0, invert for "slower"
        local inv
        inv=$(echo "scale=2; 1.0 / $ratio" | bc 2>/dev/null || echo "0")
        echo "${inv}x slower"
    fi
}

# ---------------------------------------------------------------------------
# System info collector
# ---------------------------------------------------------------------------
collect_sysinfo() {
    cat <<EOF
## System Information

- **Date**: $(date -u)
- **Hostname**: $(hostname 2>/dev/null || echo "N/A")
- **CPU**: $(awk -F': ' '/model name/ {print $2; exit}' /proc/cpuinfo 2>/dev/null || echo "N/A")
- **Cores**: $(nproc 2>/dev/null || echo "N/A")
- **RAM**: $(free -h | awk '/^Mem:/ {print $2}' 2>/dev/null || echo "N/A")
- **Disk**: $(lsblk -d -o MODEL 2>/dev/null | tail -1 || echo "N/A")
- **Kernel**: $(uname -r 2>/dev/null || echo "N/A")
- **Go**: $(go version 2>/dev/null || echo "N/A")
- **Node**: $(node --version 2>/dev/null || echo "N/A")

### Benchmark configuration

- **Warm-up**: $WARMUP
- **Measurement window**: $BENCHTIME
- **Concurrency**: $CONCURRENCY
- **Frameworks tested**: ${FRAMEWORKS_RUN:-none}

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
        echo -e "${RED}[!] Cleat compilation failed. Skipping.${NC}"
        return 1
    fi
    echo -e "${GREEN}[+] Compilation OK${NC}"

    # Warm-up run (results discarded)
    echo -e "${YELLOW}[*] Warm-up (10s)...${NC}"
    go test -bench="." -benchtime="${WARMUP}" -count=1 ./benchmarks/ > /dev/null 2>&1 || true
    echo -e "${GREEN}[+] Warm-up complete${NC}"

    # Measurement run
    echo -e "${YELLOW}[*] Measurement (${BENCHTIME})...${NC}"
    go test -bench="." -benchtime="${BENCHTIME}" -benchmem -count=1 ./benchmarks/ 2>&1 | tee "$CLEAT_RAW"

    echo ""
    echo -e "${GREEN}[+] Cleat benchmarks complete${NC}"
}

# ---------------------------------------------------------------------------
# Parse Cleat output
# ---------------------------------------------------------------------------
parse_cleat() {
    if [ ! -f "$CLEAT_RAW" ]; then
        return
    fi

    echo -e "${YELLOW}[*] Parsing Cleat results...${NC}"

    # Go benchmark lines look like:
    # BenchmarkSimpleWorkflow/steps=10-128         59320  20210 ns/op  48577 wf/s  485766 steps/s
    # We extract: name, config, wf/s, steps/s

    while IFS= read -r line; do
        # Match lines with "wf/s" and "steps/s"
        if [[ "$line" =~ ^Benchmark([A-Za-z]+)/([^ ]+)[[:space:]]+[0-9]+[[:space:]]+[0-9]+[[:space:]]+ns/op[[:space:]]+([0-9.]+)[[:space:]]+wf/s[[:space:]]+([0-9.]+)[[:space:]]+steps/s ]]; then
            local name="${BASH_REMATCH[1]}"
            local config="${BASH_REMATCH[2]}"
            local wf_s="${BASH_REMATCH[3]}"
            local steps_s="${BASH_REMATCH[4]}"
            store_result "Cleat" "$name" "$config" "$steps_s" "$wf_s"
        fi
    done < "$CLEAT_RAW"
}

# ---------------------------------------------------------------------------
# Run Temporal benchmarks
# ---------------------------------------------------------------------------
run_temporal() {
    echo ""
    echo -e "${BLUE}========================================${NC}"
    echo -e "${BLUE}  Running Temporal benchmarks${NC}"
    echo -e "${BLUE}========================================${NC}"
    echo ""

    # Check if temporal server is reachable
    if command -v temporal &>/dev/null; then
        if ! temporal server health 2>/dev/null; then
            echo -e "${YELLOW}[!] Temporal dev server not running. Starting...${NC}"
            temporal server start-dev --db-file /tmp/temporal-bench.db &
            TEMPORAL_PID=$!
            sleep 5
            echo -e "${GREEN}[+] Temporal dev server started (PID ${TEMPORAL_PID})${NC}"
        fi
    else
        echo -e "${YELLOW}[!] temporal CLI not found. Check if a Temporal server is already running.${NC}"
    fi

    local temporal_dirs=(
        "01-sequential"
        "02-fanout"
        "03-saga"
        "04-llm-agent"
    )

    local all_output=""

    for dir in "${temporal_dirs[@]}"; do
        local temporal_path="$SELF_DIR/workflows/${dir}/temporal"
        if [ ! -f "$temporal_path/main.go" ]; then
            echo -e "${YELLOW}[!] No Temporal main.go in ${dir}/temporal. Skipping.${NC}"
            continue
        fi

        echo -e "${YELLOW}[*] Running ${dir}/temporal...${NC}"

        # Change to the directory so go.mod is picked up
        cd "$temporal_path"

        # Build first
        if ! go build -o /dev/null . 2>&1; then
            echo -e "${RED}[!] Build failed for ${dir}/temporal. Skipping.${NC}"
            continue
        fi

        # Run
        local output
        output=$(go run . \
            -warmup="$WARMUP" \
            -benchtime="$BENCHTIME" \
            -concurrency="$CONCURRENCY" \
            -address="localhost:7233" 2>&1) || true
        all_output+="${output}"$'\n'
        echo "$output"
    done

    echo "$all_output" > "$TEMPORAL_RAW"
    echo ""
    echo -e "${GREEN}[+] Temporal benchmarks complete${NC}"
}

# ---------------------------------------------------------------------------
# Parse Temporal output
# ---------------------------------------------------------------------------
parse_temporal() {
    if [ ! -f "$TEMPORAL_RAW" ]; then
        return
    fi

    echo -e "${YELLOW}[*] Parsing Temporal results...${NC}"

    while IFS= read -r line; do
        # Match BENCHMARK_RESULT lines
        if [[ "$line" =~ ^BENCHMARK_RESULT[[:space:]]+name=([^[:space:]]+)[[:space:]]+config=([^[:space:]]+)[[:space:]]+count=([0-9]+)[[:space:]]+elapsed=([0-9.]+)s[[:space:]]+wf_per_sec=([0-9.]+)[[:space:]]+steps_per_sec=([0-9.]+) ]]; then
            local name="${BASH_REMATCH[1]}"
            local config="${BASH_REMATCH[2]}"
            local steps_s="${BASH_REMATCH[6]}"
            local wf_s="${BASH_REMATCH[5]}"
            store_result "Temporal" "$name" "$config" "$steps_s" "$wf_s"
        fi
    done < "$TEMPORAL_RAW"
}

# ---------------------------------------------------------------------------
# Run DBOS benchmarks
# ---------------------------------------------------------------------------
run_dbos() {
    echo ""
    echo -e "${BLUE}========================================${NC}"
    echo -e "${BLUE}  Running DBOS benchmarks${NC}"
    echo -e "${BLUE}========================================${NC}"
    echo ""

    # Detect ts-node or node with compiled JS
    local runner=""
    if command -v npx &>/dev/null; then
        runner="npx ts-node"
    elif command -v ts-node &>/dev/null; then
        runner="ts-node"
    else
        echo -e "${RED}[!] ts-node not found. Try: npm install -g ts-node${NC}"
        echo -e "${YELLOW}    Also required: @dbos-inc/dbos-sdk in each workload directory.${NC}"
        return 1
    fi

    local dbos_dirs=(
        "01-sequential"
        "02-fanout"
        "03-saga"
        "04-llm-agent"
    )

    local all_output=""

    for dir in "${dbos_dirs[@]}"; do
        local dbos_path="$SELF_DIR/workflows/${dir}/dbos"
        if [ ! -f "$dbos_path/main.ts" ]; then
            echo -e "${YELLOW}[!] No main.ts in ${dir}/dbos. Skipping.${NC}"
            continue
        fi

        echo -e "${YELLOW}[*] Running ${dir}/dbos...${NC}"
        cd "$dbos_path"

        # Check for package.json / node_modules
        if [ ! -d "node_modules" ]; then
            echo -e "${YELLOW}[!] No node_modules in ${dir}/dbos. Run 'npm install' first.${NC}"
            echo -e "${YELLOW}    Skipping DBOS ${dir}.${NC}"
            continue
        fi

        local output
        output=$($runner main.ts \
            --warmup "$(echo $WARMUP | sed 's/s//g')000" \
            --benchtime "$(echo $BENCHTIME | sed 's/s//g')000" \
            --concurrency "$CONCURRENCY" 2>&1) || true
        all_output+="${output}"$'\n'
        echo "$output"
    done

    echo "$all_output" > "$DBOS_RAW"
    echo ""
    echo -e "${GREEN}[+] DBOS benchmarks complete${NC}"
}

# ---------------------------------------------------------------------------
# Parse DBOS output
# ---------------------------------------------------------------------------
parse_dbos() {
    if [ ! -f "$DBOS_RAW" ]; then
        return
    fi

    echo -e "${YELLOW}[*] Parsing DBOS results...${NC}"

    while IFS= read -r line; do
        # Same format as Temporal
        if [[ "$line" =~ ^BENCHMARK_RESULT[[:space:]]+name=([^[:space:]]+)[[:space:]]+config=([^[:space:]]+)[[:space:]]+count=([0-9]+)[[:space:]]+elapsed=([0-9.]+)s[[:space:]]+wf_per_sec=([0-9.]+)[[:space:]]+steps_per_sec=([0-9.]+) ]]; then
            local name="${BASH_REMATCH[1]}"
            local config="${BASH_REMATCH[2]}"
            local steps_s="${BASH_REMATCH[6]}"
            local wf_s="${BASH_REMATCH[5]}"
            store_result "DBOS" "$name" "$config" "$steps_s" "$wf_s"
        fi
    done < "$DBOS_RAW"
}

# ---------------------------------------------------------------------------
# Generate comparison table and save results
# ---------------------------------------------------------------------------
generate_results() {
    echo ""
    echo -e "${BLUE}========================================${NC}"
    echo -e "${BLUE}  Generating comparison table${NC}"
    echo -e "${BLUE}========================================${NC}"
    echo ""

    # Build a map from "workload|config|framework" -> list of steps_s values
    declare -A STEPS_MAP
    declare -A WF_MAP
    declare -A ALL_KEYS
    declare -A FRAMEWORKS_SET

    while IFS='|' read -r framework workload config steps_s wf_s; do
        local key="${workload}|${config}|${framework}"
        STEPS_MAP["$key"]="${STEPS_MAP[$key]:+${STEPS_MAP[$key]}$'\n'}${steps_s}"
        WF_MAP["$key"]="${WF_MAP[$key]:+${WF_MAP[$key]}$'\n'}${wf_s}"
        ALL_KEYS["${workload}|${config}"]=1
        FRAMEWORKS_SET["$framework"]=1
    done < "$PARSED"

    # Determine which frameworks ran
    FRAMEWORKS_RUN=""
    for fw in Cleat Temporal DBOS; do
        if [ -n "${FRAMEWORKS_SET[$fw]:-}" ]; then
            FRAMEWORKS_RUN="${FRAMEWORKS_RUN}${FRAMEWORKS_RUN:+ }${fw}"
        fi
    done

    # Collect unique workloads maintaining order
    local -a WORKLOAD_ORDER=("SimpleWorkflow" "FanOutWorkflow" "SagaWorkflow" "SagaWithCompensationWorkflow" "LLMWorkflow")
    local -a ALL_KEYS_SORTED=()
    for wl in "${WORKLOAD_ORDER[@]}"; do
        for key in "${!ALL_KEYS[@]}"; do
            if [[ "$key" == "${wl}|"* ]]; then
                ALL_KEYS_SORTED+=("$key")
            fi
        done
    done
    # Add any remaining keys not in the predefined order
    for key in "${!ALL_KEYS[@]}"; do
        local found=0
        for sk in "${ALL_KEYS_SORTED[@]}"; do
            if [ "$sk" = "$key" ]; then
                found=1
                break
            fi
        done
        if [ "$found" -eq 0 ]; then
            ALL_KEYS_SORTED+=("$key")
        fi
    done

    # ---- Write CSV ----
    {
        echo "workload,config,cleat_steps_s,temporal_steps_s,dbos_steps_s,cleat_vs_temporal,cleat_vs_dbos"
        for key in "${ALL_KEYS_SORTED[@]}"; do
            IFS='|' read -r workload config <<< "$key"

            cleat_key="${workload}|${config}|Cleat"
            temporal_key="${workload}|${config}|Temporal"
            dbos_key="${workload}|${config}|DBOS"

            # Get median values
            cleat_median=""
            if [ -n "${STEPS_MAP[$cleat_key]:-}" ]; then
                cleat_median=$(median ${STEPS_MAP[$cleat_key]})
            fi
            temporal_median=""
            if [ -n "${STEPS_MAP[$temporal_key]:-}" ]; then
                temporal_median=$(median ${STEPS_MAP[$temporal_key]})
            fi
            dbos_median=""
            if [ -n "${STEPS_MAP[$dbos_key]:-}" ]; then
                dbos_median=$(median ${STEPS_MAP[$dbos_key]})
            fi

            [ -z "$cleat_median" ] && cleat_median="N/A"
            [ -z "$temporal_median" ] && temporal_median="N/A"
            [ -z "$dbos_median" ] && dbos_median="N/A"

            local ct_ratio="N/A"
            local cd_ratio="N/A"
            [ "$cleat_median" != "N/A" ] && [ "$temporal_median" != "N/A" ] && \
                ct_ratio=$(format_ratio "$cleat_median" "$temporal_median")
            [ "$cleat_median" != "N/A" ] && [ "$dbos_median" != "N/A" ] && \
                cd_ratio=$(format_ratio "$cleat_median" "$dbos_median")

            echo "${workload},${config},${cleat_median},${temporal_median},${dbos_median},\"${ct_ratio}\",\"${cd_ratio}\""
        done
    } > "$RESULTS_CSV"
    echo -e "${GREEN}[+] CSV saved to: ${RESULTS_CSV}${NC}"

    # ---- Write Markdown ----
    {
        # Title
        echo "# Benchmark Comparison Results"
        echo ""
        echo "**Generated**: $(date -u)"
        echo ""

        # System info
        collect_sysinfo
        echo ""

        # Comparison table
        echo "## Comparison Table"
        echo ""
        echo "| Workload | Config | Cleat (steps/s) | Temporal (steps/s) | DBOS (steps/s) | Cleat vs Temporal | Cleat vs DBOS |"
        echo "|----------|--------|-----------------|--------------------|----------------|-------------------|---------------|"

        for key in "${ALL_KEYS_SORTED[@]}"; do
            IFS='|' read -r workload config <<< "$key"

            cleat_key="${workload}|${config}|Cleat"
            temporal_key="${workload}|${config}|Temporal"
            dbos_key="${workload}|${config}|DBOS"

            cleat_median=""
            [ -n "${STEPS_MAP[$cleat_key]:-}" ] && cleat_median=$(median ${STEPS_MAP[$cleat_key]})
            temporal_median=""
            [ -n "${STEPS_MAP[$temporal_key]:-}" ] && temporal_median=$(median ${STEPS_MAP[$temporal_key]})
            dbos_median=""
            [ -n "${STEPS_MAP[$dbos_key]:-}" ] && dbos_median=$(median ${STEPS_MAP[$dbos_key]})

            [ -z "$cleat_median" ] && cleat_median="N/A"
            [ -z "$temporal_median" ] && temporal_median="N/A"
            [ -z "$dbos_median" ] && dbos_median="N/A"

            local ct_ratio="N/A"
            local cd_ratio="N/A"
            [ "$cleat_median" != "N/A" ] && [ "$temporal_median" != "N/A" ] && \
                ct_ratio=$(format_ratio "$cleat_median" "$temporal_median")
            [ "$cleat_median" != "N/A" ] && [ "$dbos_median" != "N/A" ] && \
                cd_ratio=$(format_ratio "$cleat_median" "$dbos_median")

            # Format numbers with commas for readability
            cleat_fmt=$(printf "%.0f" "$cleat_median" 2>/dev/null || echo "$cleat_median")
            temporal_fmt=$(printf "%.0f" "$temporal_median" 2>/dev/null || echo "$temporal_median")
            dbos_fmt=$(printf "%.0f" "$dbos_median" 2>/dev/null || echo "$dbos_median")

            echo "| ${workload} | ${config} | ${cleat_fmt} | ${temporal_fmt} | ${dbos_fmt} | ${ct_ratio} | ${cd_ratio} |"
        done

        echo ""
        echo "> **Note**: Ratios computed as Cleat / Other. Values > 1.0 mean Cleat is faster."
        echo "> Values < 1.0 mean Cleat is slower. A \"~\" denotes no significant difference"
        echo "> (ratio within 0.91-1.10). \"N/A\" means the framework did not produce results."
        echo ""

        # Per-workload detailed breakdown
        echo "---"
        echo ""
        echo "## Per-Workload Detailed Results"
        echo ""

        for key in "${ALL_KEYS_SORTED[@]}"; do
            IFS='|' read -r workload config <<< "$key"

            echo "### ${workload} (${config})"
            echo ""
            echo "| Run | Cleat (steps/s) | Temporal (steps/s) | DBOS (steps/s) |"
            echo "|-----|-----------------|--------------------|----------------|"

            cleat_key="${workload}|${config}|Cleat"
            temporal_key="${workload}|${config}|Temporal"
            dbos_key="${workload}|${config}|DBOS"

            # Find max number of runs across all frameworks for this key
            local max_runs=0
            [ -n "${STEPS_MAP[$cleat_key]:-}" ] && {
                local n=$(echo "${STEPS_MAP[$cleat_key]}" | wc -l)
                [ "$n" -gt "$max_runs" ] && max_runs=$n
            }
            [ -n "${STEPS_MAP[$temporal_key]:-}" ] && {
                local n=$(echo "${STEPS_MAP[$temporal_key]}" | wc -l)
                [ "$n" -gt "$max_runs" ] && max_runs=$n
            }
            [ -n "${STEPS_MAP[$dbos_key]:-}" ] && {
                local n=$(echo "${STEPS_MAP[$dbos_key]}" | wc -l)
                [ "$n" -gt "$max_runs" ] && max_runs=$n
            }

            # Get arrays
            local cleat_runs=()
            [ -n "${STEPS_MAP[$cleat_key]:-}" ] && mapfile -t cleat_runs <<< "${STEPS_MAP[$cleat_key]}"
            local temporal_runs=()
            [ -n "${STEPS_MAP[$temporal_key]:-}" ] && mapfile -t temporal_runs <<< "${STEPS_MAP[$temporal_key]}"
            local dbos_runs=()
            [ -n "${STEPS_MAP[$dbos_key]:-}" ] && mapfile -t dbos_runs <<< "${STEPS_MAP[$dbos_key]}"

            for (( run_idx=0; run_idx<max_runs; run_idx++ )); do
                local c_val="${cleat_runs[$run_idx]:-N/A}"
                local t_val="${temporal_runs[$run_idx]:-N/A}"
                local d_val="${dbos_runs[$run_idx]:-N/A}"
                c_val=$(printf "%.0f" "$c_val" 2>/dev/null || echo "$c_val")
                t_val=$(printf "%.0f" "$t_val" 2>/dev/null || echo "$t_val")
                d_val=$(printf "%.0f" "$d_val" 2>/dev/null || echo "$d_val")
                echo "| Run $((run_idx + 1)) | ${c_val} | ${t_val} | ${d_val} |"
            done
            echo ""
        done

        # Methodology notes
        echo "---"
        echo ""
        echo "## Methodology Notes"
        echo ""
        echo "- All frameworks benchmarked on the same hardware."
        echo "- Warm-up: ${WARMUP} (results discarded)."
        echo "- Measurement window: ${BENCHTIME}."
        echo "- Concurrency: ${CONCURRENCY} concurrent workers (Temporal and DBOS)."
        echo "- Cleat benchmarks run in-process with \`go test -bench\`."
        echo "- Temporal benchmarks run against Temporal dev server at localhost:7233."
        echo "- DBOS benchmarks run against a local PostgreSQL database."
        echo "- Each row shows individual run values. Median is used for the summary table."
        echo "- Variance above 10% between runs suggests noisy measurement conditions."
        echo ""

        # Raw output
        echo "---"
        echo ""
        echo "## Raw Output"
        echo ""

        if [ -f "$CLEAT_RAW" ]; then
            echo "### Cleat"
            echo '```'
            cat "$CLEAT_RAW"
            echo '```'
            echo ""
        fi

        if [ -f "$TEMPORAL_RAW" ]; then
            echo "### Temporal"
            echo '```'
            cat "$TEMPORAL_RAW"
            echo '```'
            echo ""
        fi

        if [ -f "$DBOS_RAW" ]; then
            echo "### DBOS"
            echo '```'
            cat "$DBOS_RAW"
            echo '```'
            echo ""
        fi

    } > "$RESULTS_MD"

    echo -e "${GREEN}[+] Markdown saved to: ${RESULTS_MD}${NC}"

    # Print summary table to stdout
    echo ""
    echo -e "${CYAN}============================================${NC}"
    echo -e "${CYAN}  Comparison Summary${NC}"
    echo -e "${CYAN}============================================${NC}"
    echo ""
    # Extract and print the table portion
    awk '/^\| Workload/,/^$/' "$RESULTS_MD" | head -n -1
    echo ""
    echo -e "${GREEN}Full results: ${RESULTS_MD}${NC}"
    echo -e "${GREEN}CSV results:  ${RESULTS_CSV}${NC}"
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
    echo ""
    echo -e "${GREEN}╔══════════════════════════════════════╗${NC}"
    echo -e "${GREEN}║  Cleat / Temporal / DBOS Benchmark   ║${NC}"
    echo -e "${GREEN}╚══════════════════════════════════════╝${NC}"
    echo ""

    # Parse flags
    local run_cleat_flag=false
    local run_temporal_flag=false
    local run_dbos_flag=false
    local all_flag=true

    for arg in "$@"; do
        case "$arg" in
            --cleat-only)   run_cleat_flag=true; all_flag=false ;;
            --temporal-only) run_temporal_flag=true; all_flag=false ;;
            --dbos-only)    run_dbos_flag=true; all_flag=false ;;
            --warmup)       shift; WARMUP="$1" ;;
            --benchtime)    shift; BENCHTIME="$1" ;;
            --concurrency)  shift; CONCURRENCY="$1" ;;
            --help|-h)
                echo "Usage: $0 [--cleat-only] [--temporal-only] [--dbos-only]"
                echo "          [--warmup 10s] [--benchtime 60s] [--concurrency N]"
                exit 0
                ;;
        esac
    done

    if [ "$all_flag" = true ]; then
        run_cleat_flag=true
        run_temporal_flag=true
        run_dbos_flag=true
    fi

    # Ensure results directory exists
    mkdir -p "$RESULTS_DIR"

    # Check for bc (needed for ratio calculations)
    if ! command -v bc &>/dev/null; then
        echo -e "${YELLOW}[!] bc not found. Ratio calculations may be limited. Install bc for best results.${NC}"
        # Provide a bc fallback using awk
        bc() { awk "BEGIN { printf \"%.2f\", $* }"; }
        export -f bc 2>/dev/null || true
    fi

    # Initialize parsed results file
    true > "$PARSED"

    # Run benchmarks
    if [ "$run_cleat_flag" = true ]; then
        run_cleat && parse_cleat || echo -e "${RED}[!] Cleat benchmarks failed.${NC}"
    fi

    if [ "$run_temporal_flag" = true ]; then
        run_temporal && parse_temporal || echo -e "${RED}[!] Temporal benchmarks failed.${NC}"
    fi

    if [ "$run_dbos_flag" = true ]; then
        run_dbos && parse_dbos || echo -e "${RED}[!] DBOS benchmarks failed.${NC}"
    fi

    # Generate results
    generate_results

    echo ""
    echo -e "${GREEN}========================================${NC}"
    echo -e "${GREEN}  All done.${NC}"
    echo -e "${GREEN}========================================${NC}"
    echo ""
}

main "$@"
