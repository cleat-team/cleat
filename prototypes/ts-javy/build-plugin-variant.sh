#!/usr/bin/env bash
# Build the Javy plugin with a set of determinism shims removed, for a control.
# usage: build-plugin-variant.sh <variant-name> [shim,shim,...] [fault]    (no list = the shipped plugin, every shim on)
# fault is a CLEAT_FAULT value (plugin/src/lib.rs), for a control that needs the host to misbehave.
# writes .work/plugin-<variant>.wasm. CLEAT_SHIMS_OFF is read by option_env! at compile time (plugin/src/lib.rs).
set -euo pipefail
cd "$(dirname "$0")"
v=$1; off=${2:-}; fault=${3:-}
CLEAT_SHIMS_OFF="$off" CLEAT_FAULT="$fault" ./plugin-cargo.sh
.tools/javy init-plugin ${PLUGIN_DETERMINISTIC_FLAG---deterministic} plugin/target/wasm32-wasip1/release/cleat_js_plugin.wasm -o ".work/plugin-$v.wasm"
python3 - "$v" "$off" "$fault" <<'PY'
import sys
v, off = sys.argv[1:3]; fault = sys.argv[3] if len(sys.argv) > 3 else ""
d = open(f".work/plugin-{v}.wasm", "rb").read()
print(f"plugin-{v}: off-list={off!r} fault={fault!r} bytes={len(d)} shim-source-present={b'Math.random = function random' in d}")
PY
