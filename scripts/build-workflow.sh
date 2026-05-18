#!/bin/bash
# build-workflow.sh
# Shared workflow WASM build script.
# Usage: build-workflow.sh <workflow-dir> [output-name]
#
# Builds a workflow directory to WASM using the standard Go WASI/WASM target.
# Output defaults to "workflow.wasm" if not specified.
set -euo pipefail

WF_DIR="$1"
OUTPUT="${2:-workflow.wasm}"

if [ ! -d "$WF_DIR" ]; then
    echo "ERROR: Workflow directory not found: $WF_DIR"
    exit 1
fi

cd "$WF_DIR"
echo "Building WASM workflow in $WF_DIR..."
tinygo build -target=wasip1 -o "$OUTPUT" .
echo "Built $WF_DIR/$OUTPUT ($(du -h "$OUTPUT" | cut -f1))"
