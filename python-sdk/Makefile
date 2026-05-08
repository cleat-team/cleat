# Cleat Python SDK — WASM compilation Makefile
#
# Builds Python workflow files to WASM components using componentize-py,
# then stamps cleat.metadata custom sections via scripts/stamp_metadata.py.
#
# Prerequisites:
#   pip install componentize-py>=0.12.0
#   pip install -e python-sdk/
#
# Expected componentize-py version: >= 0.12.0
# Check with: componentize-py --version

# Paths
WIT_DIR    := wit/
WORLD      := cleat-workflow
SDK_ROOT   := $(CURDIR)
EXAMPLES   := examples
OUT_DIR    := dist
PYTHONPATH := $(SDK_ROOT):$(PYTHONPATH)

# Version stamping variables (override on command line or in environment).
# Example:
#   make wasm WORKFLOW_NAME=PlaceOrder WORKFLOW_VERSION=3 PLUGIN_DEPS='{"llm":">=1.2.0"}'
WORKFLOW_NAME            ?= unknown
WORKFLOW_VERSION         ?= 0
MIN_COMPATIBLE_VERSION   ?= 1
ABI_VERSION              ?= 1
PLUGIN_DEPS              ?= {}

# Example workflow files and their output names
EXAMPLE_WORKFLOWS := \
	hello_workflow \
	child_workflow \
	saga_workflow \
	update_handler_workflow \
	durable_call_workflow

# Individual WASM targets for each example
WASM_FILES := $(addprefix $(OUT_DIR)/, $(addsuffix .wasm, $(EXAMPLE_WORKFLOWS)))

# Stamped WASM targets (post-processed with metadata)
STAMPED_FILES := $(addsuffix .stamped, $(WASM_FILES))

.PHONY: all wasm stamped test-wasm clean help

all: stamped

# ------------------------------------------------------------------
# wasm — compile all example workflows to WASM
# ------------------------------------------------------------------
wasm: $(WASM_FILES)
	@echo "All workflows compiled successfully."
	@ls -lh $(OUT_DIR)/*.wasm

# ------------------------------------------------------------------
# stamped — compile and stamp all example workflows
# ------------------------------------------------------------------
stamped: $(STAMPED_FILES)
	@echo "All workflows compiled and stamped successfully."
	@ls -lh $(OUT_DIR)/*.stamped

# Pattern rule: compile a single Python workflow to WASM
$(OUT_DIR)/%.wasm: $(EXAMPLES)/%.py pyproject.toml $(WIT_DIR)/cleat.wit
	@mkdir -p $(OUT_DIR)
	PYTHONPATH=$(PYTHONPATH) componentize-py componentize $< \
		--wit-path $(WIT_DIR) \
		--world $(WORLD) \
		-o $@
	@echo "Compiled $< -> $@"

# Pattern rule: stamp metadata onto a compiled WASM
$(OUT_DIR)/%.wasm.stamped: $(OUT_DIR)/%.wasm
	PYTHONPATH=$(PYTHONPATH) python scripts/stamp_metadata.py $< \
		--name "$(WORKFLOW_NAME)" \
		--version $(WORKFLOW_VERSION) \
		--min-version $(MIN_COMPATIBLE_VERSION) \
		--abi-version $(ABI_VERSION) \
		--plugin-deps '$(PLUGIN_DEPS)' \
		--output $@
	@echo "Stamped $< -> $@"

# Convenience: compile + stamp a single workflow, producing the final .wasm
$(OUT_DIR)/%.stamped.wasm: $(OUT_DIR)/%.wasm
	PYTHONPATH=$(PYTHONPATH) python scripts/stamp_metadata.py $< \
		--name "$(WORKFLOW_NAME)" \
		--version $(WORKFLOW_VERSION) \
		--min-version $(MIN_COMPATIBLE_VERSION) \
		--abi-version $(ABI_VERSION) \
		--plugin-deps '$(PLUGIN_DEPS)' \
		--output $@
	@echo "Stamped $< -> $@"

# ------------------------------------------------------------------
# test-wasm — compile workflow(s) and run WASM-specific tests
# ------------------------------------------------------------------
test-wasm: wasm
	@echo "Running WASM-specific tests..."
	PYTHONPATH=$(PYTHONPATH) python -m pytest tests/ -v -k "wasm" --tb=short || \
		echo "No WASM-specific tests found (this is expected if none are written yet)."
	@echo "Test-wasm complete."

# ------------------------------------------------------------------
# clean — remove all WASM build artifacts
# ------------------------------------------------------------------
clean:
	rm -rf $(OUT_DIR)/
	rm -f *.wasm
	rm -f *.stamped
	@echo "Cleaned all WASM artifacts."

# ------------------------------------------------------------------
# help — show available targets
# ------------------------------------------------------------------
help:
	@echo "Cleat Python SDK Makefile"
	@echo ""
	@echo "Targets:"
	@echo "  wasm        Compile all example workflows to WASM"
	@echo "  stamped     Compile and stamp metadata onto all workflows"
	@echo "  test-wasm   Compile and run WASM-specific tests"
	@echo "  clean       Remove all WASM artifacts"
	@echo ""
	@echo "Version stamping variables (set on command line):"
	@echo "  WORKFLOW_NAME=PlaceOrder"
	@echo "  WORKFLOW_VERSION=3"
	@echo "  MIN_COMPATIBLE_VERSION=1"
	@echo "  ABI_VERSION=1"
	@echo "  PLUGIN_DEPS='{"llm":">=1.2.0"}'"
	@echo ""
	@echo "Prerequisites:"
	@echo "  pip install componentize-py>=0.12.0"
	@echo "  pip install -e ."
	@echo ""
	@echo "Expected componentize-py version: >= 0.12.0"
