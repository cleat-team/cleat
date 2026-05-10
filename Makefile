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
GO_PACKAGES ?= ./durable/... ./internal/... ./plugins/... ./cmd/...
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
test: test-go test-python test-java test-as

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

.PHONY: test-cluster
test-cluster: build-go cluster-up
	@echo "Waiting for cluster to be ready..."
	@sleep 10
	go test -count=1 -timeout=120s ./internal/host/...
	$(MAKE) cluster-down

# ---- coverage -------------------------------------------------------------

.PHONY: coverage-go
coverage-go:
	-go test -coverprofile=coverage.out -covermode=atomic ./cleat/... ./internal/... ./plugins/... ./cmd/...

.PHONY: coverage-python
coverage-python:
	cd python-sdk && python -m pytest --cov=cleat_sdk --cov-report=term --cov-report=html:tests/coverage

.PHONY: coverage
coverage: coverage-go coverage-python
	@echo "Coverage reports generated"

.PHONY: coverage-report
coverage-report:
	go tool cover -func=coverage.out

# Thresholds (enforced via prefix matching):
#   cleat/            15%     (lowest: localdev  15.4%)
#   internal/         50%     (lowest: wasm      77.3%)
#   internal/host/    15%     (lowest: host      22.5%)
#   internal/plugin/  50%     (current: plugin   56.3%)
#   plugins/          20%     (lowest: kafkaconnect  24.2%)
#   cmd/               0%     (entry points, no gating)
.PHONY: coverage-check
coverage-check: coverage-go
	@go tool cover -func=coverage.out 2>/dev/null | awk 'BEGIN { \
	    fail = 0; \
	    printf "=== Coverage by Package ===\n"; \
	    printf "%-40s %8s\n\n", "Package", "Coverage"; \
	    n = split("internal/host internal/plugin internal cleat plugins cmd", prefixes, " "); \
	    thresh["internal/host"] = 15; \
	    thresh["internal/plugin"] = 50; \
	    thresh["internal"] = 50; \
	    thresh["cleat"] = 15; \
	    thresh["plugins"] = 20; \
	    thresh["cmd"] = 0; \
	} \
	/^total:/ { next } \
	{ \
	    path = $$1; \
	    sub(/:[0-9]+:$$/, "", path); \
	    sub(/\/[^/]+\.go$$/, "", path); \
	    sub(/^github\.com\/rcownie\/cleat\//, "", path); \
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
