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
	go test -coverprofile=coverage.out -covermode=atomic ./cleat/... ./internal/... ./plugins/... ./cmd/...

.PHONY: coverage-python
coverage-python:
	cd python-sdk && python -m pytest --cov=cleat_sdk --cov-report=term --cov-report=html:tests/coverage

.PHONY: coverage
coverage: coverage-go coverage-python
	@echo "Coverage reports generated"

.PHONY: coverage-report
coverage-report:
	go tool cover -func=coverage.out

# Thresholds (non-blocking, informational only):
#   cleat/            70%
#   internal/host/    60%
#   internal/plugin/  50%
#   plugins/          50%
.PHONY: coverage-check
coverage-check: coverage-go
	@go tool cover -func=coverage.out | awk 'BEGIN { \
	    printf "=== Coverage by Package ===\n"; \
	    printf "%-25s %8s\n\n", "Package", "Coverage"; \
	} \
	/^total:/ { next } \
	{ \
	    path = $$1; \
	    sub(/:[0-9]+:$$/, "", path); \
	    sub(/\/[^/]+\.go$$/, "", path); \
	    gsub(/%$$/, "", $$NF); \
	    cov[path] += $$NF; \
	    cnt[path]++; \
	} \
	END { \
	    for (p in cov) { \
	        printf "%-25s %7.2f%%\n", p, cov[p] / cnt[p]; \
	    } \
	    printf "\n=== Threshold Check ===\n"; \
	    printf "%-25s %8s %10s  %s\n\n", "Package", "Coverage", "Threshold", "Result"; \
	    pkgs["cleat"] = 70; \
	    pkgs["internal/host"] = 60; \
	    pkgs["internal/plugin"] = 50; \
	    pkgs["plugins"] = 50; \
	    for (p in cov) { \
	        avg = cov[p] / cnt[p]; \
	        t = pkgs[p]; \
	        if (t == "") continue; \
	        if (avg < t) { \
	            printf "\033[31m%-25s %7.2f%% %5d%%      FAIL\033[0m\n", p, avg, t; \
	        } else { \
	            printf "\033[32m%-25s %7.2f%% %5d%%      PASS\033[0m\n", p, avg, t; \
	        } \
	    } \
	}'
	@echo "Coverage check complete (non-blocking, exit 0)"

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
