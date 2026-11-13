#!/bin/bash
# Build a workflow: workflow.ts -> esbuild -> javy (static, cleat-js plugin) -> cleat-js-link -> tsmeta.
# usage: [SUSPEND_MODE=throw] [OUT_NAME=x] [REBUILD_PLUGIN=1] build.sh <name> <abi_entry>[,<abi_entry>...]
#   reads workflows/<name>/<name>.ts. Each ABI entry (snake_case) is the module's export; its WIT/Javy name is the kebab-case
#   form and the TypeScript export must be the camelCase form (place_order -> place-order -> placeOrder).
#   Language label: "javascript" (engine.WasmtimeLanguages on this branch).
set -euo pipefail
cd "$(dirname "$0")"
name=$1; entries=$2
out=${OUT_NAME:-$name}
J=.tools/javy; W=.work
mkdir -p "$W"

# The plugin: rebuilt when its sources are newer than the initialised module, or on request.
if [ -n "${REBUILD_PLUGIN:-}" ] || [ ! -f "$W/cleat-plugin.wasm" ] || [ -n "$(find plugin/src plugin/Cargo.toml ../../crates/cleat-sdk/src -newer "$W/cleat-plugin.wasm" -type f | head -1)" ]; then
  (cd plugin && cargo build --target wasm32-wasip1 --release 2>&1 | grep -E '^(error|warning: unused)|Finished' || true)
  $J init-plugin plugin/target/wasm32-wasip1/release/cleat_js_plugin.wasm -o "$W/cleat-plugin.wasm"
fi

define=()
if [ -n "${SUSPEND_MODE:-}" ]; then define=(--define:SUSPEND_MODE="\"$SUSPEND_MODE\""); fi
# Javy finds a module's exports SYNTACTICALLY (crates/codegen/src/js.rs): `export function f()` and `export { f }` where f is
# a function DECLARATION with no parameters. `export const f = workflow(...)` is not seen. So the bundle's entry is a
# generated file that wraps each workflow in a parameterless function declaration and exports that.
entry="$W/$out.entry.ts"
python3 - "$name" "$entries" "$entry" <<'PY'
import sys
name, entries, path = sys.argv[1:4]
camel = lambda e: (lambda p: p[0] + "".join(x.title() for x in p[1:]))(e.split("_"))
lines = [f'import * as m from "../workflows/{name}/{name}";']
for e in entries.split(","):
    c = camel(e)
    lines.append(f"function {c}() {{ return m.{c}(); }}")
lines.append("export { " + ", ".join(camel(e) for e in entries.split(",")) + " };")
open(path, "w").write("\n".join(lines) + "\n")
PY
npx esbuild "$entry" --bundle --format=esm --target=es2020 ${define[@]+"${define[@]}"} --outfile="$W/$out.js" --log-level=warning

# WIT world and link pairs from the ABI entry names.
wit="$W/$out.wit"; pairs=()
{ echo "package cleat:ts;"; echo "world w {"; } > "$wit"
IFS=',' read -ra E <<< "$entries"
for e in "${E[@]}"; do k=${e//_/-}; echo "  export $k: func();" >> "$wit"; pairs+=("$e=$k"); done
echo "}" >> "$wit"

$J build "$W/$out.js" -C plugin="$W/cleat-plugin.wasm" -C wit="$wit" -C wit-world=w -o "$W/$out.javy.wasm"
link/target/release/cleat-js-link "$W/$out.javy.wasm" "$W/$out.linked.wasm" "${pairs[@]}"
GOWORK=off GOFLAGS=-mod=mod GOPROXY=off go run ./tools/tsmeta -in "$W/$out.linked.wasm" -out "$W/$out.wasm" -name "$out" -entries "$entries" -language javascript
ls -l "$W/$out.wasm" | awk '{print "built", $9, $5, "bytes"}'
