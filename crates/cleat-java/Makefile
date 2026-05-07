# Java SDK (durable-java) — WASM build verification Makefile
#
# Provides Gradle build targets for the Java SDK, including WASM
# compilation via TeaVM and metadata stamping.
#
# Prerequisites:
#   Java 11+ (JDK)
#   Gradle is provided by the Gradle Wrapper (./gradlew) in this directory.
#   If ./gradlew is not present, run: gradle wrapper
#
# TeaVM gradle plugin version: 0.10.2 (see build.gradle / build.gradle.kts)

# Version stamping variables (override on command line or in environment).
WORKFLOW_NAME            ?= unknown
WORKFLOW_VERSION         ?= 0
MIN_COMPATIBLE_VERSION   ?= 1
ABI_VERSION              ?= 1
PLUGIN_DEPS              ?= {}

.PHONY: all wasm wasm-stamped classes test clean help

all: classes

# ------------------------------------------------------------------
# wasm — compile Java workflows to WASM via TeaVM
# ------------------------------------------------------------------
wasm:
	@echo "Building WASM with TeaVM via Gradle..."
	CLEAT_WORKFLOW_NAME="$(WORKFLOW_NAME)" \
	CLEAT_WORKFLOW_VERSION=$(WORKFLOW_VERSION) \
	CLEAT_MIN_COMPATIBLE_VERSION=$(MIN_COMPATIBLE_VERSION) \
	CLEAT_ABI_VERSION=$(ABI_VERSION) \
	CLEAT_PLUGIN_DEPS='$(PLUGIN_DEPS)' \
	./gradlew wasm
	@echo "WASM build complete."
	@ls -lh build/wasm/*.wasm 2>/dev/null || echo "Warning: No .wasm file found in build/wasm/"

# ------------------------------------------------------------------
# wasm-stamped — compile Java workflows to WASM and stamp metadata
# ------------------------------------------------------------------
wasm-stamped: wasm
	@echo "Stamping cleat metadata on WASM binaries..."
	for wasm in build/wasm/*.wasm; do \
		echo "  Stamping $$wasm..."; \
		bash scripts/inject-metadata.sh "$$wasm" \
			--name "$(WORKFLOW_NAME)" \
			--version $(WORKFLOW_VERSION) \
			--min-version $(MIN_COMPATIBLE_VERSION) \
			--abi-version $(ABI_VERSION) \
			--plugin-deps '$(PLUGIN_DEPS)'; \
	done
	@echo "Metadata stamped."

# ------------------------------------------------------------------
# classes — compile Java classes (without WASM)
# ------------------------------------------------------------------
classes:
	@echo "Compiling Java classes..."
	./gradlew classes
	@echo "Classes compiled."

# ------------------------------------------------------------------
# test — run Java unit tests
# ------------------------------------------------------------------
test:
	@echo "Running Java tests..."
	./gradlew test
	@echo "Tests complete."

# ------------------------------------------------------------------
# clean — remove all build artifacts
# ------------------------------------------------------------------
clean:
	@echo "Cleaning build artifacts..."
	./gradlew clean
	@echo "Clean complete."

# ------------------------------------------------------------------
# help — show available targets
# ------------------------------------------------------------------
help:
	@echo "Cleat Java SDK (durable-java) Makefile"
	@echo ""
	@echo "Targets:"
	@echo "  wasm         Compile Java workflows to WASM (./gradlew wasm)"
	@echo "  wasm-stamped Compile and stamp cleat metadata"
	@echo "  classes      Compile Java classes only (./gradlew classes)"
	@echo "  test         Run Java unit tests (./gradlew test)"
	@echo "  clean        Remove all build artifacts (./gradlew clean)"
	@echo ""
	@echo "Version stamping variables (set on command line):"
	@echo "  WORKFLOW_NAME=PlaceOrder"
	@echo "  WORKFLOW_VERSION=3"
	@echo "  MIN_COMPATIBLE_VERSION=1"
	@echo "  ABI_VERSION=1"
	@echo "  PLUGIN_DEPS='{"llm":">=1.2.0"}'"
	@echo ""
	@echo "Prerequisites:"
	@echo "  Java 11+ (JDK)"
	@echo "  Gradle Wrapper (./gradlew)"
	@echo ""
	@echo "TeaVM plugin version: 0.10.2"
