# Java SDK (durable-java) — WASM build verification Makefile
#
# Provides Gradle build targets for the Java SDK, including WASM
# compilation via TeaVM and standard Java development targets.
#
# Prerequisites:
#   Java 11+ (JDK)
#   Gradle is provided by the Gradle Wrapper (./gradlew) in this directory.
#   If ./gradlew is not present, run: gradle wrapper
#
# TeaVM gradle plugin version: 0.10.2 (see build.gradle / build.gradle.kts)

.PHONY: all wasm classes test clean help

all: classes

# ------------------------------------------------------------------
# wasm — compile Java workflows to WASM via TeaVM
# ------------------------------------------------------------------
wasm:
	@echo "Building WASM with TeaVM via Gradle..."
	./gradlew wasm
	@echo "WASM build complete."
	@ls -lh build/wasm/*.wasm 2>/dev/null || echo "Warning: No .wasm file found in build/wasm/"

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
	@echo "  wasm        Compile Java workflows to WASM (./gradlew wasm)"
	@echo "  classes     Compile Java classes only (./gradlew classes)"
	@echo "  test        Run Java unit tests (./gradlew test)"
	@echo "  clean       Remove all build artifacts (./gradlew clean)"
	@echo ""
	@echo "Prerequisites:"
	@echo "  Java 11+ (JDK)"
	@echo "  Gradle Wrapper (./gradlew)"
	@echo ""
	@echo "TeaVM plugin version: 0.10.2"
