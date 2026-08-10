# Rust SDK (cleat-sdk) — WASM build Makefile
#
# Builds the Rust SDK and stamp cleat.metadata custom sections.
#
# Prerequisites:
#   cargo (Rust 1.75+)
#   wasm32-wasip1 target: rustup target add wasm32-wasip1
#
# For wasm-pack builds:
#   wasm-pack >= 0.12.0 (install: curl https://rustwasm.github.io/wasm-pack/installer/init.sh -sSf | sh)

# Version stamping variables (override on command line or in environment).
WORKFLOW_NAME            ?= unknown
WORKFLOW_VERSION         ?= 0
MIN_COMPATIBLE_VERSION   ?= 1
ABI_VERSION              ?= 1
PLUGIN_DEPS              ?= {}
CHILD_BINDING_POLICY     ?=

.PHONY: all wasm wasm-stamped check test clean help

all: check

# ------------------------------------------------------------------
# wasm — build WASM package with wasm-pack
# ------------------------------------------------------------------
wasm:
	@echo "Building WASM package with wasm-pack..."
	CLEAT_WORKFLOW_NAME="$(WORKFLOW_NAME)" \
	CLEAT_WORKFLOW_VERSION=$(WORKFLOW_VERSION) \
	CLEAT_MIN_COMPATIBLE_VERSION=$(MIN_COMPATIBLE_VERSION) \
	CLEAT_ABI_VERSION=$(ABI_VERSION) \
	CLEAT_PLUGIN_DEPS='$(PLUGIN_DEPS)' \
	wasm-pack build --target web
	@echo "WASM build complete. Output in pkg/"
	@ls -lh pkg/*.wasm 2>/dev/null || echo "No .wasm file found in pkg/"
	@echo ""
	@echo "To stamp metadata, run: make wasm-stamped"

# ------------------------------------------------------------------
# wasm-stamped — build WASM and then stamp metadata
# ------------------------------------------------------------------
wasm-stamped: wasm
	@echo "Stamping cleat metadata..."
	cd .. && cargo run --bin inject_metadata -- \
		cleat-sdk/pkg/*.wasm \
		--name "$(WORKFLOW_NAME)" \
		--version $(WORKFLOW_VERSION) \
		--min-version $(MIN_COMPATIBLE_VERSION) \
		--abi-version $(ABI_VERSION) \
		--plugin-deps '$(PLUGIN_DEPS)' \
		--child-binding-policy '$(CHILD_BINDING_POLICY)'
	@echo "Metadata stamped."

# ------------------------------------------------------------------
# check — run cargo check (fast compilation check)
# ------------------------------------------------------------------
check:
	cargo check
	@echo "cargo check passed."

# ------------------------------------------------------------------
# test — run cargo test (host-target tests, not WASM)
# ------------------------------------------------------------------
test:
	CLEAT_WORKFLOW_NAME="test" \
	CLEAT_WORKFLOW_VERSION=1 \
	CLEAT_MIN_COMPATIBLE_VERSION=1 \
	CLEAT_ABI_VERSION=1 \
	CLEAT_PLUGIN_DEPS='{}' \
	cargo test
	@echo "cargo test passed."

# ------------------------------------------------------------------
# clean — remove all build artifacts
# ------------------------------------------------------------------
clean:
	rm -rf pkg/
	cargo clean
	@echo "Cleaned all build artifacts (pkg/ and target/)."

# ------------------------------------------------------------------
# help — show available targets
# ------------------------------------------------------------------
help:
	@echo "Cleat Rust SDK Makefile"
	@echo ""
	@echo "Targets:"
	@echo "  wasm         Build WASM package with wasm-pack"
	@echo "  wasm-stamped Build WASM and stamp cleat metadata"
	@echo "  check        Run cargo check (fast compilation check)"
	@echo "  test         Run cargo test"
	@echo "  clean        Remove pkg/ and target/"
	@echo ""
	@echo "Version stamping variables (set on command line):"
	@echo "  WORKFLOW_NAME=PlaceOrder"
	@echo "  WORKFLOW_VERSION=3"
	@echo "  MIN_COMPATIBLE_VERSION=1"
	@echo "  ABI_VERSION=1"
	@echo "  PLUGIN_DEPS='{"llm":">=1.2.0"}'"
	@echo ""
	@echo "Prerequisites:"
	@echo "  wasm-pack >= 0.12.0"
	@echo "  cargo (Rust 1.75+)"
	@echo "  rustup target add wasm32-wasip1"
	@echo ""
	@echo "Expected wasm-pack version: >= 0.12.0"
