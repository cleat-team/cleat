#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# ci-check.sh — All-in-one local CI check
#
# Runs the full suite of lint, test, and build steps that the GitHub
# Actions pipeline would run.  Designed for developers to run before
# pushing to avoid CI failures.
#
# Usage:
#   ./scripts/ci-check.sh
#
# Exit code:
#   0 — all checks pass
#   1 — one or more checks failed
# ---------------------------------------------------------------------------
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Colours
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

# ---------------------------------------------------------------------------
# State
# ---------------------------------------------------------------------------
FAILED=0
RESULTS=()           # list of "ok|fail|skip  description"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
section() {
    echo ""
    echo -e "${BLUE}============================================${NC}"
    echo -e "${BLUE}  $*${NC}"
    echo -e "${BLUE}============================================${NC}"
    echo ""
}

run_step() {
    local label="$1"
    shift
    echo -e "  ${YELLOW}[RUN]${NC} ${label}..."
    echo ""

    set +e
    (
        set -e
        "$@"
    )
    local rc=$?
    set -e

    if [ "$rc" -eq 0 ]; then
        echo ""
        echo -e "  ${GREEN}[PASS]${NC} ${label}"
        RESULTS+=("ok   ${label}")
    else
        echo ""
        echo -e "  ${RED}[FAIL]${NC} ${label} (exit code ${rc})"
        RESULTS+=("fail ${label}")
        FAILED=1
    fi
    echo ""
}

# ===========================================================================
# 1. LINT
# ===========================================================================
section "LINT"

run_step "go vet ./..." \
    go vet ./...

run_step "cleat vet (go) ./... (blocking)" \
    go run ./cmd/cleat vet --lang go --json ./...

# shellcheck disable=SC2016
run_step "cleat vet (go) testdata/vet-checks/e001 (expect errors)" \
    bash -c '
        output=$(go run ./cmd/cleat vet --lang go ./testdata/vet-checks/go/e001_goroutine/ 2>&1)
        rc=$?
        if [ "$rc" -eq 1 ]; then
            echo "Found expected errors (vet correctly validated testdata)"
            echo "$output"
            exit 0
        elif [ "$rc" -eq 0 ]; then
            echo "ERROR: expected errors but none found — vet may be broken"
            echo "$output"
            exit 1
        else
            echo "ERROR: vet failed with unexpected exit code $rc"
            echo "$output"
            exit 1
        fi
    '

run_step "ruff check python-sdk/" \
    ruff check python-sdk/

run_step "shellcheck scripts/*.sh benchmarks/*.sh" \
    shellcheck scripts/*.sh benchmarks/*.sh

run_step "clippy (durable-macro)" \
    bash -c "cd '$REPO_ROOT/crates/durable-macro' && cargo clippy --all-targets -- -D warnings"

run_step "clippy (durable-sdk)" \
    bash -c "cd '$REPO_ROOT/crates/durable-sdk' && cargo clippy --all-targets -- -D warnings"

# ===========================================================================
# 2. TEST
# ===========================================================================
section "TEST"

run_step "go test (core) ./durable/..." \
    go test -race -count=1 ./durable/...

run_step "go test (internal) ./internal/..." \
    go test -race -count=1 ./internal/...

run_step "go test (plugins) ./plugins/..." \
    go test -race -count=1 ./plugins/...

run_step "go test (commands) ./cmd/..." \
    go test -race -count=1 ./cmd/...

run_step "python tests" \
    bash -c "cd '$REPO_ROOT/python-sdk' && python -m pytest -v"

run_step "assemblyscript tests" \
    bash -c "cd '$REPO_ROOT/packages/cleat-as' && npm test"

# Java tests are optional locally — skip if no Java/Gradle
if command -v gradle &>/dev/null || [ -x "$REPO_ROOT/crates/durable-java/gradlew" ]; then
    run_step "java tests" \
        bash -c "cd '$REPO_ROOT/crates/durable-java' && if [ -x gradlew ]; then ./gradlew test; else gradle test; fi"
else
    echo -e "  ${YELLOW}[SKIP]${NC} java tests (gradle not available)"
    RESULTS+=("skip java tests")
fi

# ===========================================================================
# 3. BUILD
# ===========================================================================
section "BUILD"

run_step "go build ./cmd/..." \
    go build ./cmd/...

run_step "cargo build (durable-macro)" \
    bash -c "cd '$REPO_ROOT/crates/durable-macro' && cargo build"

run_step "cargo build (durable-sdk)" \
    bash -c "cd '$REPO_ROOT/crates/durable-sdk' && cargo build"

run_step "assemblyscript build" \
    bash -c "cd '$REPO_ROOT/packages/cleat-as' && npm ci && npm run build"

run_step "python install" \
    bash -c "cd '$REPO_ROOT/python-sdk' && pip install --upgrade pip && pip install ."

# ===========================================================================
# SUMMARY
# ===========================================================================
section "SUMMARY"

echo ""
for result in "${RESULTS[@]}"; do
    case "$result" in
        ok*)   echo -e "  ${GREEN}[PASS]${NC} ${result#ok   }" ;;
        fail*) echo -e "  ${RED}[FAIL]${NC} ${result#fail }" ;;
        skip*) echo -e "  ${YELLOW}[SKIP]${NC} ${result#skip }" ;;
    esac
done

echo ""
if [ "$FAILED" -eq 0 ]; then
    echo -e "${GREEN}============================================${NC}"
    echo -e "${GREEN}  ALL CHECKS PASSED${NC}"
    echo -e "${GREEN}============================================${NC}"
else
    echo -e "${RED}============================================${NC}"
    echo -e "${RED}  SOME CHECKS FAILED — see above for details${NC}"
    echo -e "${RED}============================================${NC}"
fi
echo ""

exit "$FAILED"
