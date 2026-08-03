# ---------------------------------------------------------------------------
# Cleat / Durable — Comprehensive Makefile
#
# Targets are grouped:
#   all / ci          — top-level convenience
#   lint / lint-*     — static analysis
#   test / test-*     — run test suites
#   bench / bench-*   — benchmarks & regression detection
#   build / build-*   — compile all targets
#   cluster-*         — Docker Compose cluster lifecycle
#   fmt / fmt-*       — auto-formatting
# ---------------------------------------------------------------------------
SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c

# ----- helpers --------------------------------------------------------------
GO_PACKAGES ?= ./cleat/... ./internal/... ./plugins/... ./cmd/...
GO_BENCHTIME ?= 30s

# ---- convenience -----------------------------------------------------------

.PHONY: all
all: lint test build

.PHONY: ci
ci: lint test bench build

# ---- lint ------------------------------------------------------------------

.PHONY: lint
lint: lint-go lint-python lint-rust lint-sh

.PHONY: lint-go
lint-go:
	go vet ./...

.PHONY: lint-python
lint-python:
	ruff check python-sdk/

.PHONY: lint-rust
lint-rust:
	cd crates/cleat-macro && cargo clippy --all-targets -- -D warnings
	cd crates/cleat-sdk && cargo clippy --all-targets -- -D warnings

.PHONY: lint-sh
lint-sh:
	shellcheck scripts/*.sh benchmarks/*.sh

# ---- test ------------------------------------------------------------------

.PHONY: test
test: test-go test-python test-java test-as test-plugin-harness

.PHONY: test-go
test-go:
	go test -race -count=1 $(GO_PACKAGES)

.PHONY: test-all-dbs
test-all-dbs:
	@echo "=== Testing PostgreSQL ==="
	@CLEAT_TEST_DB="postgres://localhost:5432/cleat?sslmode=disable" go test -count=1 -timeout=300s ./internal/host/...
	@echo "=== Testing MySQL ==="
	@CLEAT_TEST_MYSQL="root:cleat@tcp(127.0.0.1:3306)/cleat" go test -count=1 -timeout=300s ./internal/host/...
	@echo "=== Testing SQL Server ==="
	@CLEAT_TEST_MSSQL="sqlserver://sa:CleatTest123!@127.0.0.1:1433?database=master" go test -count=1 -timeout=300s ./internal/host/...
	@echo "All multi-database tests complete."

.PHONY: test-python
test-python:
	cd python-sdk && python -m pytest -v

.PHONY: test-java
test-java:
	cd crates/cleat-java && if [ -x gradlew ]; then ./gradlew test; else gradle test; fi

.PHONY: test-as
test-as:
	cd packages/cleat-as && npm test

# ./engine/... , not ./internal/host/... : commit 3eeb74e moved internal/host/
# to engine/, so this target has been running `go test` against a path that
# does not exist. `go test` on a non-existent pattern exits non-zero, so the
# target has been failing rather than silently passing -- but it means the
# cluster suite has had no working local entry point since that rename.
#
# -p 1 for the same reason ci.yml's cluster job uses it: ./engine/... is two
# packages, engine and engine/testutil, and both build their schema into, and
# wipe rows from, the single database this target points them at. Run in
# parallel they race on the DDL in 001_schema.sql and delete each other's
# fixtures.
#
# Note this does NOT run ./tests/cluster/... . That suite is currently run by
# nothing at all -- see the UNWIRED_SUITES list in
# scripts/check-ci-package-coverage.sh. Pointing this target at it without
# first establishing that it passes would be trading a visibly broken target
# for a quietly broken one.
.PHONY: test-cluster
test-cluster: build-go cluster-up
	@echo "Waiting for cluster to be ready..."
	@sleep 10
	go test -p 1 -count=1 -timeout=180s ./engine/...
	$(MAKE) cluster-down

# ---- plugin harness -------------------------------------------------------

.PHONY: test-plugin-harness
test-plugin-harness:
	go test -race -count=1 -timeout=300s ./tests/plugin-harness/...

.PHONY: test-plugin-harness-all-dbs
test-plugin-harness-all-dbs:
	@echo "=== Plugin harness - PostgreSQL ==="
	@CLEAT_TEST_POSTGRES="$${CLEAT_TEST_POSTGRES:-postgres://localhost:5432/cleat?sslmode=disable}" \
		go test -count=1 -timeout=300s ./tests/plugin-harness/ -run 'MultiDB' -v
	@echo "=== Plugin harness - MySQL ==="
	@CLEAT_TEST_MYSQL="$${CLEAT_TEST_MYSQL:-root:cleat@tcp(127.0.0.1:3306)/cleat?tls=false&parseTime=true}" \
		go test -count=1 -timeout=300s ./tests/plugin-harness/ -run 'MultiDB' -v
	@echo "=== Plugin harness - MSSQL ==="
	@CLEAT_TEST_MSSQL="$${CLEAT_TEST_MSSQL:-sqlserver://sa:CleatTest123!@127.0.0.1:1433?database=master}" \
		go test -count=1 -timeout=300s ./tests/plugin-harness/ -run 'MultiDB' -v

.PHONY: test-plugin-harness-check
test-plugin-harness-check:
	@echo "Checking plugin harness..."
	@go build ./tests/plugin-harness/...
	@echo "OK"

# ---- coverage -------------------------------------------------------------

.PHONY: coverage-go
coverage-go:
	-go test -coverprofile=coverage.out -covermode=atomic ./internal/... ./plugins/... ./cmd/... ./engine/... ./plugin/... ./wasm/... ./auth/...
	cd cleat && go test -coverprofile=../coverage_cleat.out -covermode=atomic ./...

.PHONY: coverage-python
coverage-python:
	cd python-sdk && python -m pytest --cov=cleat_sdk --cov-report=term --cov-report=html:tests/coverage

.PHONY: coverage
coverage: coverage-go coverage-python
	@echo "Coverage reports generated"

.PHONY: coverage-report
coverage-report:
	go tool cover -func=coverage.out

# Thresholds (enforced via prefix matching; measured 2026-06-10):
#   engine/testutil    0%     (test helper)
#   engine/           70%     (actual 70.0% PG-only; 77.6% with all backends)
#   internal/         65%     (lowest: telemetry 65.7%)
#   plugin/           70%     (actual 71.7%)
#   cleat/wasmtest    60%     (actual 62.6%)
#   cleat/            60%     (lowest: wasmtest 62.6%)
#   plugins/          80%     (actual 84.9% overall; lowest: slacknotify 70.3%)
#   cmd/cleat-plugin-verify   0%  (utility, no tests)
#   cmd/deploy-workflow       0%  (utility, no tests)
#   cmd/wit-rewrite           0%  (utility, no tests)
#   cmd/              40%     (lowest non-zero: cleat-worker 42.8%)
#   wasm/             75%     (actual 79.4%)
#   auth/             90%     (actual 90.9%)
.PHONY: coverage-check
coverage-check: coverage-go
	@cat coverage_cleat.out 2>/dev/null | grep -v "^mode:" >> coverage.out 2>/dev/null; \
	go tool cover -func=coverage.out 2>/dev/null | awk 'BEGIN { \
	    fail = 0; \
	    printf "=== Coverage by Package ===\n"; \
	    printf "%-40s %8s\n\n", "Package", "Coverage"; \
	    n = split("engine/testutil engine internal plugin cleat/wasmtest cleat plugins cmd/cleat-plugin-verify cmd/deploy-workflow cmd/wit-rewrite cmd/cleatctl cmd wasm auth", prefixes, " "); \
	    thresh["engine/testutil"] = 0; \
	    thresh["engine"] = 70; \
	    thresh["internal"] = 65; \
	    thresh["plugin"] = 70; \
	    thresh["cleat/wasmtest"] = 60; \
	    thresh["cleat"] = 60; \
	    thresh["plugins"] = 80; \
	    thresh["cmd/cleat-plugin-verify"] = 0; \
	    thresh["cmd/deploy-workflow"] = 0; \
	    thresh["cmd/wit-rewrite"] = 0; \
		    thresh["cmd/cleatctl"] = 75; \
	    thresh["cmd"] = 40; \
	    thresh["wasm"] = 75; \
	    thresh["auth"] = 90; \
	} \
	/^total:/ { next } \
	{ \
	    path = $$1; \
	    sub(/:[0-9]+:$$/, "", path); \
	    sub(/\/[^/]+\.go$$/, "", path); \
	    sub(/^github\.com\/cleat-team\/cleat\//, "", path); \
	    gsub(/%$$/, "", $$NF); \
	    cov[path] += $$NF; \
	    cnt[path]++; \
	} \
	END { \
	    for (p in cov) { \
	        avg = cov[p] / cnt[p]; \
	        printf "%-40s %7.2f%%\n", p, avg; \
	    } \
	    printf "\n=== Threshold Check ===\n"; \
	    printf "%-40s %8s %10s  %s\n\n", "Package", "Coverage", "Threshold", "Result"; \
	    for (p in cov) { \
	        avg = cov[p] / cnt[p]; \
	        t = -1; \
	        for (i = 1; i <= n; i++) { \
	            prefix = prefixes[i]; \
	            if (p == prefix || index(p, prefix "/") == 1) { \
	                t = thresh[prefix]; \
	                break; \
	            } \
	        } \
	        if (t < 0) continue; \
	        if (avg < t) { \
	            printf "\033[31m%-40s %7.2f%% %5d%%      FAIL\033[0m\n", p, avg, t; \
	            fail = 1; \
	        } else { \
	            printf "\033[32m%-40s %7.2f%% %5d%%      PASS\033[0m\n", p, avg, t; \
	        } \
	    } \
	    printf "\n"; \
	    if (fail) { \
	        printf "\033[31mCoverage check FAILED\033[0m\n"; \
	        exit 1; \
	    } else { \
	        printf "\033[32mCoverage check PASSED\033[0m\n"; \
	    } \
	}'

# ---- bench -----------------------------------------------------------------

.PHONY: bench
bench:
	go test -bench=. -benchmem -benchtime=$(GO_BENCHTIME) ./benchmarks/

.PHONY: bench-compare
bench-compare:
	./benchmarks/compare.sh

.PHONY: bench-save
bench-save:
	@mkdir -p .benchmarks
	@go test -bench=. -benchmem -benchtime=$(GO_BENCHTIME) ./benchmarks/ \
		| tee .benchmarks/current-$$(uname -m)-go$$(go version | cut -d' ' -f3 | cut -d'.' -f1,2).txt

# ---- build -----------------------------------------------------------------

.PHONY: build
build: build-go build-python build-java build-as

.PHONY: build-go
build-go:
	go build ./cmd/...
	cd wasm-demo && go build ./...

.PHONY: build-python
build-python:
	cd python-sdk && pip install --upgrade pip && pip install .

.PHONY: build-java
build-java:
	cd crates/cleat-java && if [ -x gradlew ]; then ./gradlew build; else gradle build; fi

.PHONY: build-as
build-as:
	cd packages/cleat-as && npm ci && npm run build

# ---- cluster ---------------------------------------------------------------

.PHONY: cluster-up
cluster-up:
	go build -o cleat-worker ./cmd/cleat-worker
	docker build -t cleat-worker:latest .
	docker compose -f docker-compose.cluster.yml up -d

.PHONY: cluster-down
cluster-down:
	docker compose -f docker-compose.cluster.yml down -v

.PHONY: cluster-logs
cluster-logs:
	docker compose -f docker-compose.cluster.yml logs -f

# ---- fmt -------------------------------------------------------------------

.PHONY: fmt
fmt: fmt-go fmt-python fmt-rust

.PHONY: fmt-go
fmt-go:
	go fmt ./...
	@if [ -n "$$(go fmt ./...)" ]; then echo "[!] Some files were reformatted by go fmt"; fi

.PHONY: fmt-python
fmt-python:
	ruff format python-sdk/

.PHONY: fmt-rust
fmt-rust:
	cd crates/cleat-macro && cargo fmt
	cd crates/cleat-sdk && cargo fmt

# ---- clew (Neon-backed durable dev) -----------------------------------------

.PHONY: clew
clew:
	@go build -o cleat-worker ./cmd/cleat-worker
	@if [ -z "$${CLEW_DATABASE_URL:-}" ]; then \
		echo "ERROR: CLEW_DATABASE_URL is not set."; \
		echo ""; \
		echo "Create a .env file in the cleat repo root with:"; \
		echo '  CLEW_DATABASE_URL="postgresql://user:pass@ep-xxx.us-east-1.aws.neon.tech/neondb?sslmode=require"'; \
		echo ""; \
		echo "Or export it directly:"; \
		echo "  export CLEW_DATABASE_URL=\"...\""; \
		exit 1; \
	fi
	@echo "Starting cleat-worker against Neon (migrations run automatically)..."
	./cleat-worker --db "$${CLEW_DATABASE_URL}" --api-addr=:8080

# ---- tools ------------------------------------------------------------------

GO_MIN_VERSION := 1.25
.PHONY: tools
tools: tools-go tools-rust tools-python tools-java tools-as
	@echo "=== All toolchains checked ==="

.PHONY: tools-go
tools-go:
	@if command -v go >/dev/null 2>&1; then \
		VER=$$(go version | grep -oP 'go\K[0-9]+\.[0-9]+'); \
		MAJOR=$$(echo $$VER | cut -d. -f1); \
		MINOR=$$(echo $$VER | cut -d. -f2); \
		MIN_MAJOR=$$(echo $(GO_MIN_VERSION) | cut -d. -f1); \
		MIN_MINOR=$$(echo $(GO_MIN_VERSION) | cut -d. -f2); \
		if [ "$$MAJOR" -gt "$$MIN_MAJOR" ] || ([ "$$MAJOR" -eq "$$MIN_MAJOR" ] && [ "$$MINOR" -ge "$$MIN_MINOR" ]); then \
			echo "[OK] Go $$(go version | head -1)"; \
		else \
			echo "[OLD] Go $$(go version | head -1) — need $(GO_MIN_VERSION)+. Install from https://go.dev/dl/"; \
		fi; \
	else \
		echo "[MISSING] Go $(GO_MIN_VERSION)+ — install from https://go.dev/dl/"; \
		echo "  Linux:   wget https://go.dev/dl/go1.25.7.linux-amd64.tar.gz && sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.25.7.linux-amd64.tar.gz"; \
		echo "  macOS:   brew install go@1.25"; \
	fi


.PHONY: tools-rust
tools-rust:
	@if command -v cargo >/dev/null 2>&1; then \
		echo "[OK] cargo $$(cargo --version | cut -d' ' -f2)"; \
		rustup target list --installed | grep -q wasm32-unknown-unknown && echo "[OK] rust target wasm32-unknown-unknown" || \
			(echo "[ADDING] rust target wasm32-unknown-unknown" && rustup target add wasm32-unknown-unknown); \
	else \
		echo "[MISSING] Rust — install from https://rustup.rs"; \
		echo "  curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh"; \
	fi

.PHONY: tools-python
tools-python:
	@if command -v python3 >/dev/null 2>&1; then \
		echo "[OK] python3 $$(python3 --version | cut -d' ' -f2)"; \
	else \
		echo "[MISSING] Python 3 — install from https://python.org"; \
	fi
	@if python3 -c "import componentize" 2>/dev/null; then \
		echo "[OK] componentize-py"; \
	else \
		echo "[MISSING] componentize-py — install with: pip install componentize-py"; \
	fi
	@cd python-sdk && python3 -c "import cleat_sdk" 2>/dev/null && echo "[OK] cleat_sdk (editable)" || \
		echo "[MISSING] cleat_sdk — install with: cd python-sdk && pip install -e ."

.PHONY: tools-java
tools-java:
	@if command -v gradle >/dev/null 2>&1; then \
		echo "[OK] Gradle $$(gradle --version | grep '^Gradle ' | cut -d' ' -f2)"; \
	elif [ -x crates/cleat-java/gradlew ]; then \
		echo "[OK] Gradle wrapper (gradlew)"; \
	else \
		echo "[MISSING] Gradle — install from https://gradle.org/install/"; \
	fi
	@if command -v java >/dev/null 2>&1; then \
		echo "[OK] Java $$(java -version 2>&1 | head -1)"; \
	else \
		echo "[MISSING] JDK 11+ — install from https://adoptium.net"; \
	fi

.PHONY: tools-as
tools-as:
	@if command -v npm >/dev/null 2>&1; then \
		echo "[OK] npm $$(npm --version)"; \
	else \
		echo "[MISSING] Node.js/npm — install from https://nodejs.org"; \
	fi
	@if command -v npx >/dev/null 2>&1 && npx --yes asc --version >/dev/null 2>&1; then \
		echo "[OK] AssemblyScript compiler (asc)"; \
	else \
		echo "[MISSING] asc — install with: npm install -g assemblyscript"; \
	fi

.PHONY: tools-check
tools-check:
	@echo "=== WASM Compilation Toolchains ==="
	@echo ""
	@$(MAKE) --no-print-directory tools-rust
	@echo ""
	@$(MAKE) --no-print-directory tools-python
	@echo ""
	@$(MAKE) --no-print-directory tools-java
	@echo ""
	@$(MAKE) --no-print-directory tools-as
	@echo ""
	@echo "=== Done ==="

.PHONY: setup
setup:
	@echo "=== Cleat Setup ==="
	@echo ""
	@$(MAKE) --no-print-directory tools-go
	@echo ""
	@echo "Next steps:"
	@echo "  1. Start PostgreSQL:  docker compose -f docker-compose.partner.yml up -d postgres"
	@echo "  2. Build CLI:         go build -o cleat ./cmd/cleat && go build -o cleat-worker ./cmd/cleat-worker"
	@echo "  3. Verify:            make tools"
	@echo "  4. Run dev mode:      ./cleat dev start"
	@echo ""
	@echo "See docs/tutorials/quick-start.md for a full walkthrough."

.PHONY: setup-full
setup-full:
	@echo "=== Cleat Full Setup ==="
	@echo ""
	@$(MAKE) --no-print-directory tools
	@go build ./...
	@echo ""
	@echo "Full toolchain ready. Run 'make test' to verify."
