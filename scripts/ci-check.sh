#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# ci-check.sh — the cheap local checks, before pushing
#
# This is NOT the CI pipeline. CI runs around fifty checks; this runs the
# handful that are quick locally and catch the most common reasons a push goes
# red. Passing here is not evidence CI will pass.
#
# It also does not replace the dedicated gates, which have their own scripts and
# their own reasons to exist:
#   scripts/tier-gate.sh          tier 1 (needs all three DSNs; see tiers.yaml)
#   scripts/check-skip-budget.sh  per-job skip ceilings
#   scripts/check-skips.sh        conditional-skip guard
#   scripts/check-test-only-code.sh
#
# Usage:
#   ./scripts/ci-check.sh
#
# Exit code:
#   0 — all checks pass
#   1 — one or more checks failed
#
# HISTORY, because this file rotted badly and silently. Until 2026-08-09 it
# tested ./durable/... (renamed to ./engine/ by the 2026-06-01 package move)
# and built crates/durable-{macro,sdk,java} (renamed to cleat-*). Five of its
# steps pointed at paths that no longer existed, so it exited 1 at the first
# test step and had done for months, while its header claimed to run the full
# pipeline. The preflight below is the fix for the class, not just the
# instance: a path that disappears now fails loudly here instead of rotting.
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

# ---------------------------------------------------------------------------
# Preflight: every path this script names must exist.
#
# This is the anti-rot check. Without it a renamed directory turns a step into
# a silent no-op or a confusing failure a hundred lines later, which is exactly
# how this script came to spend months unable to run at all.
# ---------------------------------------------------------------------------
REQUIRED_PATHS=(
    "engine" "internal" "plugins" "cmd" "wasm"
    "cleat" "python-sdk" "packages/cleat-as"
    "crates/cleat-macro" "crates/cleat-sdk"
)
missing=()
for p in "${REQUIRED_PATHS[@]}"; do
    [ -e "$REPO_ROOT/$p" ] || missing+=("$p")
done
if [ ${#missing[@]} -gt 0 ]; then
    echo -e "${RED}ci-check.sh is out of date: these paths do not exist:${NC}" >&2
    printf '  %s\n' "${missing[@]}" >&2
    echo "" >&2
    echo "Fix the paths in this script rather than deleting the check -- a step" >&2
    echo "pointing at a missing directory does not test anything." >&2
    exit 1
fi

# A note this script cannot enforce but every reader needs (see CLAUDE.md):
# an unset CLEAT_TEST_* DSN makes that dialect's tests SKIP, and the suite
# still prints ok. The Go tests below are worth far less without them.
for v in CLEAT_TEST_POSTGRES CLEAT_TEST_MYSQL CLEAT_TEST_MSSQL; do
    [ -n "${!v:-}" ] || echo -e "${YELLOW}[warn]${NC} $v is unset — its dialect will skip silently"
done

# ===========================================================================
# 1. LINT
# ===========================================================================
section "LINT"

# CI's lint-go job runs this and golangci-lint does not, so it has to be here:
# a struct field that changes an alignment block passes every other check and
# still fails the push.
# shellcheck disable=SC2016  # the $() must run inside the child shell, not here
run_step "gofmt -l ." \
    bash -c 'unformatted=$(gofmt -l . | grep -v "/testdata/" || true)
        if [ -n "$unformatted" ]; then
            echo "These files are not gofmt-clean:" >&2
            echo "$unformatted" >&2
            echo "Run: gofmt -w <file>" >&2
            exit 1
        fi
        echo "OK: all Go files are gofmt-clean."'

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

run_step "clippy (cleat-macro)" \
    bash -c "cd '$REPO_ROOT/crates/cleat-macro' && cargo clippy --all-targets -- -D warnings"

run_step "clippy (cleat-sdk)" \
    bash -c "cd '$REPO_ROOT/crates/cleat-sdk' && cargo clippy --all-targets -- -D warnings"

# ===========================================================================
# 2. TEST
# ===========================================================================
section "TEST"

run_step "go test (engine) ./engine/..." \
    go test -race -count=1 ./engine/...

run_step "go test (wasm) ./wasm/..." \
    go test -race -count=1 ./wasm/...

run_step "go test (cleat module)" \
    bash -c "cd '$REPO_ROOT/cleat' && go test -race -count=1 ./..."

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
if command -v gradle &>/dev/null || [ -x "$REPO_ROOT/crates/cleat-java/gradlew" ]; then
    run_step "java tests" \
        bash -c "cd '$REPO_ROOT/crates/cleat-java' && if [ -x gradlew ]; then ./gradlew test; else gradle test; fi"
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

run_step "cargo build (cleat-macro)" \
    bash -c "cd '$REPO_ROOT/crates/cleat-macro' && cargo build"

run_step "cargo build (cleat-sdk)" \
    bash -c "cd '$REPO_ROOT/crates/cleat-sdk' && cargo build"

# NOT `npm ci`. packages/cleat-as/node_modules is COMMITTED (139 tracked files
# as of 2026-08-09, `git ls-files packages/cleat-as/node_modules | wc -l`), and
# npm ci wipes node_modules before installing -- so the old step deleted tracked
# files out of the developer's checkout as a side effect of a pre-push check.
run_step "assemblyscript build" \
    bash -c "cd '$REPO_ROOT/packages/cleat-as' && npm run build"

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
