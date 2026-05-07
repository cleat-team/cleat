#!/usr/bin/env bash
# inject-metadata.sh — Post-compile metadata stamping for Java WASM workflows.
#
# Reads a compiled WASM binary (produced by TeaVM) and injects (or replaces)
# the "cleat.metadata" custom section with workflow identity information.
#
# Usage:
#   bash scripts/inject-metadata.sh <wasm-file> \
#       --name PlaceOrder \
#       --version 3 \
#       [--output <output.wasm>]
#
# Environment variables (all optional, with defaults):
#   CLEAT_WORKFLOW_NAME=PlaceOrder
#   CLEAT_WORKFLOW_VERSION=3
#   CLEAT_MIN_COMPATIBLE_VERSION=1
#   CLEAT_ABI_VERSION=1
#   CLEAT_PLUGIN_DEPS='{"llm":">=1.2.0"}'
#
# This script prefers to use:
#   1. The cleat CLI (from the Go SDK) if available — reads/modifies metadata
#   2. Python stamp_metadata.py (from the Python SDK) as fallback
#   3. wasm-tools as a last resort

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SDK_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# ---- Resolve paths ----
CLEAT_CLI="${CLEAT_CLI:-}"  # Override with path to cleat CLI binary
PYTHON_STAMP="${PYTHON_STAMP:-}"  # Override with path to stamp_metadata.py

if [ -z "$CLEAT_CLI" ]; then
    # Try to find cleat in PATH or project.
    CLEAT_CLI="$(command -v cleat 2>/dev/null || true)"
    if [ -z "$CLEAT_CLI" ]; then
        CLEAT_CLI="$SDK_ROOT/../../../cmd/cleat/cleat"
        [ -x "$CLEAT_CLI" ] || CLEAT_CLI=""
    fi
fi

if [ -z "$PYTHON_STAMP" ]; then
    PYTHON_STAMP="$SDK_ROOT/../../../python-sdk/scripts/stamp_metadata.py"
    [ -f "$PYTHON_STAMP" ] || PYTHON_STAMP=""
    if [ -z "$PYTHON_STAMP" ]; then
        # Try to find it relative to the script.
        PYTHON_STAMP="$SCRIPT_DIR/../../../python-sdk/scripts/stamp_metadata.py"
        [ -f "$PYTHON_STAMP" ] || PYTHON_STAMP=""
    fi
fi

# ---- Helper: show usage ----
usage() {
    cat <<EOF
Usage: inject-metadata.sh <wasm-file> [options]

Options:
  --name <name>         Workflow name (or CLEAT_WORKFLOW_NAME env var)
  --version <n>         Workflow version (or CLEAT_WORKFLOW_VERSION env var)
  --min-version <n>     Min compatible version (or CLEAT_MIN_COMPATIBLE_VERSION env var)
  --abi-version <n>     ABI version (or CLEAT_ABI_VERSION env var)
  --plugin-deps <json>  Plugin dependencies JSON (or CLEAT_PLUGIN_DEPS env var)
  --output, -o <file>   Output WASM path (default: overwrite input)
  --read                Read and display metadata instead of writing
  --verbose, -v         Verbose output
EOF
    exit 0
}

# ---- Parse arguments ----
WASM_FILE=""
OUTPUT=""
NAME="${CLEAT_WORKFLOW_NAME:-unknown}"
VERSION="${CLEAT_WORKFLOW_VERSION:-0}"
MIN_VERSION="${CLEAT_MIN_COMPATIBLE_VERSION:-1}"
ABI_VERSION="${CLEAT_ABI_VERSION:-1}"
PLUGIN_DEPS="${CLEAT_PLUGIN_DEPS:-{}}"
READ_ONLY=0
VERBOSE=0

while [ $# -gt 0 ]; do
    case "$1" in
        --help|-h) usage ;;
        --read) READ_ONLY=1; shift ;;
        --verbose|-v) VERBOSE=1; shift ;;
        --output|-o) OUTPUT="$2"; shift 2 ;;
        --name) NAME="$2"; shift 2 ;;
        --version) VERSION="$2"; shift 2 ;;
        --min-version) MIN_VERSION="$2"; shift 2 ;;
        --abi-version) ABI_VERSION="$2"; shift 2 ;;
        --plugin-deps) PLUGIN_DEPS="$2"; shift 2 ;;
        -*)
            echo "Unknown option: $1"
            usage
            ;;
        *)
            WASM_FILE="$1"
            shift
            ;;
    esac
done

if [ -z "$WASM_FILE" ]; then
    echo "Error: <wasm-file> is required"
    usage
fi

if [ ! -f "$WASM_FILE" ]; then
    echo "Error: WASM file not found: $WASM_FILE"
    exit 1
fi

# ---- Read-only mode ----
if [ "$READ_ONLY" = 1 ]; then
    if command -v cleat &>/dev/null 2>&1; then
        # Use cleat deploy --dry-run to show metadata
        echo "=== cleat.metadata from $WASM_FILE ==="
        cleat deploy --name "read-check" "$WASM_FILE" 2>&1 || true
    elif [ -n "$PYTHON_STAMP" ] && command -v python3 &>/dev/null 2>&1; then
        python3 "$PYTHON_STAMP" --read "$WASM_FILE"
    else
        echo "No tool available to read metadata. Install cleat CLI or Python SDK."
        echo "You can also use: wasm-tools dump $WASM_FILE | grep -A 20 cleat.metadata"
        exit 1
    fi
    exit 0
fi

# ---- Stamp metadata ----
if [ -n "$PYTHON_STAMP" ] && command -v python3 &>/dev/null 2>&1; then
    # Use Python stamp_metadata.py (preferred).
    STAMP_ARGS=("$PYTHON_STAMP" "$WASM_FILE")
    STAMP_ARGS+=(--name "$NAME" --version "$VERSION" --min-version "$MIN_VERSION")
    STAMP_ARGS+=(--abi-version "$ABI_VERSION" --plugin-deps "$PLUGIN_DEPS")
    if [ -n "$OUTPUT" ]; then
        STAMP_ARGS+=(--output "$OUTPUT")
    fi
    if [ "$VERBOSE" = 1 ]; then
        STAMP_ARGS+=(--verbose)
    fi
    python3 "${STAMP_ARGS[@]}"
    echo "Stamped cleat metadata (via Python) into ${OUTPUT:-$WASM_FILE}"

elif [ -x "$CLEAT_CLI" ]; then
    # Use the cleat CLI for stamping.
    echo "Note: cleat CLI stamping not yet implemented as a separate command."
    echo "Use 'cleat deploy --dry-run' to validate metadata."
    echo "Falling back to wasm-tools..."

    # Use wasm-tools if available.
    if command -v wasm-tools &>/dev/null 2>&1; then
        # Build metadata JSON.
        META_JSON=$(cat <<ENDJSON
{
  "workflow_name": "$NAME",
  "workflow_version": $VERSION,
  "min_compatible_version": $MIN_VERSION,
  "abi_version": $ABI_VERSION,
  "plugin_deps": $PLUGIN_DEPS,
  "sdk_language": "java",
  "sdk_version": "0.1.0",
  "created_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
ENDJSON
)
        OUTPUT_FLAG=""
        if [ -n "$OUTPUT" ]; then
            OUTPUT_FLAG="-o $OUTPUT"
        fi
        echo "$META_JSON" | wasm-tools metadata add "$WASM_FILE" cleat.metadata --payload - $OUTPUT_FLAG
        echo "Stamped cleat metadata (via wasm-tools) into ${OUTPUT:-$WASM_FILE}"
    else
        echo "Error: No stamping tool available."
        echo "Install one of:"
        echo "  - Python SDK (python-sdk/) — provides stamp_metadata.py"
        echo "  - wasm-tools — 'cargo install wasm-tools'"
        echo "  - cleat CLI — build cmd/cleat"
        exit 1
    fi
else
    # Fallback: use wasm-tools.
    if command -v wasm-tools &>/dev/null 2>&1; then
        META_JSON=$(cat <<ENDJSON
{
  "workflow_name": "$NAME",
  "workflow_version": $VERSION,
  "min_compatible_version": $MIN_VERSION,
  "abi_version": $ABI_VERSION,
  "plugin_deps": $PLUGIN_DEPS,
  "sdk_language": "java",
  "sdk_version": "0.1.0",
  "created_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
ENDJSON
)
        OUTPUT_FLAG=""
        if [ -n "$OUTPUT" ]; then
            OUTPUT_FLAG="-o $OUTPUT"
        fi
        echo "$META_JSON" | wasm-tools metadata add "$WASM_FILE" cleat.metadata --payload - $OUTPUT_FLAG
        echo "Stamped cleat metadata (via wasm-tools) into ${OUTPUT:-$WASM_FILE}"
    else
        echo "Error: No stamping tool available."
        echo "Install one of:"
        echo "  - Python SDK (python-sdk/) — provides stamp_metadata.py"
        echo "  - wasm-tools — 'cargo install wasm-tools'"
        echo "  - cleat CLI — build cmd/cleat"
        exit 1
    fi
fi
