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
PLUGIN=${PLUGIN:-$W/cleat-plugin.wasm}   # a determinism control build passes its own plugin variant
mkdir -p "$W"

# Static determinism check (tools/tscheck). Refuses a workflow that names a global this runtime does not define. The binary is
# built and run directly, not through `go run`, because `go run` reports every non-zero exit as 1 and this check has three
# outcomes: 0 clean, 1 finding, 2 UNMEASURED (the check could not look). Both 1 and 2 stop the build.
GOWORK=off GOFLAGS=-mod=mod GOPROXY=off go build -o "$W/tscheck" ./tools/tscheck
"$W/tscheck" "workflows/$name/$name.ts" || { rc=$?; echo "build refused by tscheck (exit $rc: 1 = finding, 2 = UNMEASURED)"; exit "$rc"; }

# The plugin: rebuilt when its sources are newer than the initialised module, or on request.
if [ -n "${REBUILD_PLUGIN:-}" ] || [ ! -f "$W/cleat-plugin.wasm" ] || [ -n "$(find plugin/src plugin/Cargo.toml ../../crates/cleat-sdk/src -newer "$W/cleat-plugin.wasm" -type f | head -1)" ]; then
  (cd plugin && CLEAT_SHIMS_OFF="" cargo build --target wasm32-wasip1 --release 2>&1 | grep -E '^(error|warning: unused)|Finished' || true)
  $J init-plugin --deterministic plugin/target/wasm32-wasip1/release/cleat_js_plugin.wasm -o "$W/cleat-plugin.wasm"
fi

define=()
if [ -n "${SUSPEND_MODE:-}" ]; then define=(--define:SUSPEND_MODE="\"$SUSPEND_MODE\""); fi
if [ -n "${RESULT_CANONICAL:-}" ]; then define+=(--define:RESULT_CANONICAL="$RESULT_CANONICAL"); fi
if [ -n "${NAN_POLICY:-}" ]; then define+=(--define:NAN_POLICY="\"$NAN_POLICY\""); fi
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

# -C deterministic: fixed clocks and a zero RNG during Wizer pre-initialisation, so identical source gives identical bytes
# (without it two builds of one bundle differ in 3 bytes of a pre-initialised data segment; evidence/phase2/repro.txt).
$J build "$W/$out.js" -C plugin="$PLUGIN" -C deterministic=${JAVY_DETERMINISTIC:-y} -C wit="$wit" -C wit-world=w -o "$W/$out.javy.wasm"
link/target/release/cleat-js-link "$W/$out.javy.wasm" "$W/$out.linked.wasm" "${pairs[@]}"
# The engine pin: what this module carries, computed from the real artefacts (nothing typed). QuickJS comes from the plugin's
# lockfile, the plugin and SDK by content hash. A rebuild with any of them changed is a different pin.
python3 - "$J" "$PLUGIN" "$W/$out.pin.json" "${JAVY_DETERMINISTIC:-y}" <<'PY'
import hashlib, json, re, subprocess, sys
javy, plugin, out, det = sys.argv[1:5]
sha = lambda p: hashlib.sha256(open(p, "rb").read()).hexdigest()
lock = open("plugin/Cargo.lock").read()
qjs = re.search(r'name = "rquickjs-core"\nversion = "([^"]+)"', lock).group(1)
json.dump({"pin_version": 1, "javy": subprocess.check_output([javy, "--version"], text=True).strip(), "rquickjs_core": qjs,
           "plugin_sha256": sha(plugin), "sdk_sha256": sha("packages/cleat-ts/index.ts"), "deterministic_build": det == "y"},
          open(out, "w"), sort_keys=True, indent=1)
PY
GOWORK=off GOFLAGS=-mod=mod GOPROXY=off go run ./tools/tsmeta -in "$W/$out.linked.wasm" -out "$W/$out.wasm" -name "$out" -entries "$entries" -language javascript -pin "$W/$out.pin.json"
ls -l "$W/$out.wasm" | awk '{print "built", $9, $5, "bytes"}'
