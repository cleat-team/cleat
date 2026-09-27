# Java SDK (durable-java) — WASM build verification Makefile
#
# Provides Gradle build targets for the Java SDK, including WASM
# compilation via TeaVM and metadata stamping.
#
# Prerequisites:
#   Java 11+ (JDK)
#   Gradle. This repo does NOT vendor a wrapper -- there is no ./gradlew here
#   and none is tracked -- so a system gradle is what actually runs. Every
#   target used to hardcode ./gradlew, so `make test` failed with
#   "./gradlew: No such file or directory" while `gradle test` worked, and the
#   Makefile was read as evidence that the Java SDK had no local toolchain.
#   That misreading cost a real scope decision (see cleat#1136).
#
# TeaVM gradle plugin version: 0.10.2 (see build.gradle / build.gradle.kts)

# Version stamping variables (override on command line or in environment).
WORKFLOW_NAME            ?= unknown
WORKFLOW_VERSION         ?= 0
MIN_COMPATIBLE_VERSION   ?= 1
ABI_VERSION              ?= 1
PLUGIN_DEPS              ?= {}

# Prefer a vendored wrapper if one ever appears -- it pins the gradle version,
# which a system install does not -- and fall back to whatever is on PATH.
GRADLE ?= $(shell if [ -x ./gradlew ]; then echo ./gradlew; else command -v gradle 2>/dev/null; fi)

.PHONY: all wasm wasm-stamped classes test clean help require-gradle

# An empty GRADLE must not reach a recipe. `$(GRADLE) test` with GRADLE unset
# runs the shell command `test`, which exits 1 silently -- a build failure
# reported as nothing at all. Fail here instead, naming both ways to fix it.
require-gradle:
	@test -n "$(GRADLE)" || { \
		echo "No gradle found. This repo vendors no ./gradlew wrapper, so a" >&2; \
		echo "system gradle is required: install one (brew install gradle, or" >&2; \
		echo "your package manager), or generate a wrapper here with" >&2; \
		echo "  gradle wrapper" >&2; \
		exit 1; \
	}

all: classes

# ------------------------------------------------------------------
# wasm — compile Java workflows to WASM via TeaVM
# ------------------------------------------------------------------
wasm: require-gradle
	@echo "Building WASM with TeaVM via Gradle..."
	CLEAT_WORKFLOW_NAME="$(WORKFLOW_NAME)" \
	CLEAT_WORKFLOW_VERSION=$(WORKFLOW_VERSION) \
	CLEAT_MIN_COMPATIBLE_VERSION=$(MIN_COMPATIBLE_VERSION) \
	CLEAT_ABI_VERSION=$(ABI_VERSION) \
	CLEAT_PLUGIN_DEPS='$(PLUGIN_DEPS)' \
	$(GRADLE) wasm
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
classes: require-gradle
	@echo "Compiling Java classes..."
	$(GRADLE) classes
	@echo "Classes compiled."

# ------------------------------------------------------------------
# test — run Java unit tests
# ------------------------------------------------------------------
test: require-gradle
	@echo "Running Java tests..."
	$(GRADLE) test
	@echo "Tests complete."

# ------------------------------------------------------------------
# clean — remove all build artifacts
# ------------------------------------------------------------------
clean: require-gradle
	@echo "Cleaning build artifacts..."
	$(GRADLE) clean
	@echo "Clean complete."

# ------------------------------------------------------------------
# help — show available targets
# ------------------------------------------------------------------
help:
	@echo "Cleat Java SDK (durable-java) Makefile"
	@echo ""
	@echo "Targets:"
	@echo "  wasm         Compile Java workflows to WASM (\$$(GRADLE) wasm)"
	@echo "  wasm-stamped Compile and stamp cleat metadata"
	@echo "  classes      Compile Java classes only (\$$(GRADLE) classes)"
	@echo "  test         Run Java unit tests (\$$(GRADLE) test)"
	@echo "  clean        Remove all build artifacts (\$$(GRADLE) clean)"
	@echo ""
	@echo "Version stamping variables (set on command line):"
	@echo "  WORKFLOW_NAME=PlaceOrder"
	@echo "  WORKFLOW_VERSION=3"
	@echo "  MIN_COMPATIBLE_VERSION=1"
	@echo "  ABI_VERSION=1"
	@echo "  PLUGIN_DEPS='{\"llm\":\">=1.2.0\"}'"
	@echo ""
	@echo "Prerequisites:"
	@echo "  Java 11+ (JDK)"
	@echo "  Gradle -- no wrapper is vendored here; using: $(GRADLE)"
	@echo ""
	@echo "TeaVM plugin version: 0.10.2"
