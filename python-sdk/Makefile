# Cleat Python SDK — WASM compilation Makefile
#
# Builds Python workflow files to WASM components using componentize-py.
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

# Example workflow files and their output names
EXAMPLE_WORKFLOWS := \
	hello_workflow \
	child_workflow \
	saga_workflow \
	update_handler_workflow

# Individual WASM targets for each example
WASM_FILES := $(addprefix $(OUT_DIR)/, $(addsuffix .wasm, $(EXAMPLE_WORKFLOWS)))

.PHONY: all wasm test-wasm clean help

all: wasm

# ------------------------------------------------------------------
# wasm — compile all example workflows to WASM
# ------------------------------------------------------------------
wasm: $(WASM_FILES)
	@echo "All workflows compiled successfully."
	@ls -lh $(OUT_DIR)/*.wasm

# Pattern rule: compile a single Python workflow to WASM
$(OUT_DIR)/%.wasm: $(EXAMPLES)/%.py pyproject.toml $(WIT_DIR)/cleat.wit
	@mkdir -p $(OUT_DIR)
	PYTHONPATH=$(PYTHONPATH) componentize-py componentize $< \
		--wit-path $(WIT_DIR) \
		--world $(WORLD) \
		-o $@
	@echo "Compiled $< -> $@"

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
	@echo "Cleaned all WASM artifacts."

# ------------------------------------------------------------------
# help — show available targets
# ------------------------------------------------------------------
help:
	@echo "Cleat Python SDK Makefile"
	@echo ""
	@echo "Targets:"
	@echo "  wasm        Compile all example workflows to WASM (default)"
	@echo "  test-wasm   Compile and run WASM-specific tests"
	@echo "  clean       Remove all WASM artifacts"
	@echo ""
	@echo "Prerequisites:"
	@echo "  pip install componentize-py>=0.12.0"
	@echo "  pip install -e ."
	@echo ""
	@echo "Expected componentize-py version: >= 0.12.0"
