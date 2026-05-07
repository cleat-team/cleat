# Rust SDK (durable-sdk) — WASM build Makefile
#
# Builds the Rust SDK as a WASM module using wasm-pack, and provides
# standard Rust development targets.
#
# Prerequisites:
#   wasm-pack >= 0.12.0   (install: curl https://rustwasm.github.io/wasm-pack/installer/init.sh -sSf | sh)
#   cargo (Rust 1.75+)
#
# Check wasm-pack version:
#   wasm-pack --version
#
# Expected wasm-pack version: >= 0.12.0

.PHONY: all wasm check test clean help

all: check

# ------------------------------------------------------------------
# wasm — build WASM package with wasm-pack
# ------------------------------------------------------------------
wasm:
	@echo "Building WASM package with wasm-pack..."
	wasm-pack build --target web
	@echo "WASM build complete. Output in pkg/"
	@ls -lh pkg/*.wasm 2>/dev/null || echo "No .wasm file found in pkg/"
	@echo ""
	@echo "Note: If wasm-pack is not installed, run:"
	@echo "  curl https://rustwasm.github.io/wasm-pack/installer/init.sh -sSf | sh"

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
	@echo "  wasm        Build WASM package with wasm-pack (--target web)"
	@echo "  check       Run cargo check (fast compilation check)"
	@echo "  test        Run cargo test (host target)"
	@echo "  clean       Remove pkg/ and target/"
	@echo ""
	@echo "Prerequisites:"
	@echo "  wasm-pack >= 0.12.0"
	@echo "  cargo (Rust 1.75+)"
	@echo ""
	@echo "Expected wasm-pack version: >= 0.12.0"
