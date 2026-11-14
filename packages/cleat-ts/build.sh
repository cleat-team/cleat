#!/usr/bin/env bash
# Build a TypeScript workflow module: workflow.ts -> static check -> esbuild -> javy (static, cleat-js plugin) -> link -> stamp.
#
#   usage: [OUT_DIR=<dir>] [OUT_NAME=<name>] [SOURCEMAP=0] [REBUILD_PLUGIN=1] packages/cleat-ts/build.sh <path/to/workflow.ts> <abi_entry>[,<abi_entry>...]
#
# The source may live anywhere, and may `import { workflow } from "@cleat/ts"`. Each ABI entry is snake_case; the
# module's export for it is the camelCase form (place_order -> placeOrder) and its WIT name the kebab-case form
# (place-order). The language label stamped into the module is "javascript" (engine.WasmtimeLanguages).
#
# Run packages/cleat-ts/bootstrap.sh first: it installs the pinned javy and esbuild, verified against toolchain.lock.
#
# Exit: 0 built; 1 the static check refused the source; 2 UNMEASURED (a tool or input is missing, or a build step
# failed): a failure of the setup, not a finding about the workflow.
#
# Build-time switches for determinism red controls, all empty in a shipped build: SUSPEND_MODE, RESULT_CANONICAL,
# NAN_POLICY (read by index.ts) and CLEAT_SHIMS_OFF / CLEAT_FAULT (read by the plugin crate).
set -euo pipefail
unmeasured() { echo "UNMEASURED: $*" >&2; echo "  (this is a failure of the setup, not a finding about the workflow)" >&2; exit 2; }

[ "$#" -eq 2 ] || { echo "usage: build.sh <path/to/workflow.ts> <abi_entry>[,<abi_entry>...]" >&2; exit 2; }
# The source path is resolved BEFORE the cd below, against the caller's directory.
[ -f "$1" ] || unmeasured "no such workflow source: $1"
SRC_ABS="$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"
case "$SRC_ABS" in *.ts) ;; *) unmeasured "the workflow source must be a .ts file, got $1" ;; esac
entries=$2
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
cd "$HERE"
name=$(basename "$SRC_ABS" .ts)
export SRC_ROOT=${SRC_ROOT:-$(dirname "$SRC_ABS")}
export NODE_PATH="$HERE/node_modules${NODE_PATH:+:$NODE_PATH}"
out=${OUT_NAME:-$name}
J=.tools/javy; W=.work
PLUGIN_DIR=$ROOT/crates/cleat-js-plugin; LINK_DIR=$ROOT/crates/cleat-js-link
PLUGIN=${PLUGIN:-$W/cleat-plugin.wasm}   # a determinism control build passes its own plugin variant
mkdir -p "$W"
[ -x "$J" ] || unmeasured "$HERE/$J is missing: run packages/cleat-ts/bootstrap.sh"
[ -x node_modules/.bin/esbuild ] || unmeasured "esbuild is missing: run packages/cleat-ts/bootstrap.sh"

# Static determinism check. Built and run directly, not through `go run`, because `go run` reports every non-zero exit
# as 1 and this check has three outcomes: 0 clean, 1 finding, 2 UNMEASURED. Both 1 and 2 stop the build.
( cd "$ROOT" && go build -o "$HERE/$W/cleat-ts-check" ./cmd/cleat-ts-check ) || unmeasured "building cmd/cleat-ts-check failed"
"$W/cleat-ts-check" "$SRC_ABS" || { rc=$?; echo "build refused by cleat-ts-check (exit $rc: 1 = finding, 2 = UNMEASURED)" >&2; exit "$rc"; }

# The plugin: rebuilt when its sources are newer than the initialised module, or on request.
newer=""
if [ -f "$W/cleat-plugin.wasm" ]; then
  newer=$(find "$PLUGIN_DIR/src" "$PLUGIN_DIR/Cargo.toml" "$ROOT/crates/cleat-sdk/src" -newer "$W/cleat-plugin.wasm" -type f -print -quit)
fi
if [ -n "${REBUILD_PLUGIN:-}" ] || [ ! -f "$W/cleat-plugin.wasm" ] || [ -n "$newer" ]; then
  # --remap-path-prefix keeps the cargo home directory (a user's name) out of the module's panic-location strings.
  # No `|| true`: a failed compile must stop the build, not fall through to a stale artefact.
  ( cd "$PLUGIN_DIR" && RUSTFLAGS="${RUSTFLAGS:-} --remap-path-prefix=${CARGO_HOME:-$HOME/.cargo}=/cargo" \
      cargo build --locked --target wasm32-wasip1 --release ) || unmeasured "building crates/cleat-js-plugin failed"
  "$J" init-plugin --deterministic "$PLUGIN_DIR/target/wasm32-wasip1/release/cleat_js_plugin.wasm" -o "$W/cleat-plugin.wasm" \
    || unmeasured "javy init-plugin failed"
fi
if [ ! -x "$LINK_DIR/target/release/cleat-js-link" ]; then
  ( cd "$LINK_DIR" && cargo build --locked --release ) || unmeasured "building crates/cleat-js-link failed"
fi

define=()
[ -z "${SUSPEND_MODE:-}" ] || define+=(--define:SUSPEND_MODE="\"$SUSPEND_MODE\"")
[ -z "${RESULT_CANONICAL:-}" ] || define+=(--define:RESULT_CANONICAL="$RESULT_CANONICAL")
[ -z "${NAN_POLICY:-}" ] || define+=(--define:NAN_POLICY="\"$NAN_POLICY\"")
# Javy finds a module's exports SYNTACTICALLY: `export function f()` and `export { f }` where f is a function
# DECLARATION with no parameters. `export const f = workflow(...)` is not seen. So the bundle's entry is a generated
# file that wraps each workflow in a parameterless function declaration and exports that.
entry="$W/$out.entry.ts"
python3 - "$entries" "$entry" "$SRC_ABS" <<'PY'
import sys
entries, path, src_abs = sys.argv[1:4]
camel = lambda e: (lambda p: p[0] + "".join(x.title() for x in p[1:]))(e.split("_"))
lines = [f'import * as m from "{src_abs[:-3]}";']
for e in entries.split(","):
    c = camel(e)
    lines.append(f"function {c}() {{ return m.{c}(); }}")
lines.append("export { " + ", ".join(camel(e) for e in entries.split(",")) + " };")
open(path, "w").write("\n".join(lines) + "\n")
PY
# Source maps (SOURCEMAP=0 turns them off): esbuild writes an external map; it is trimmed to {sources, mappings} and
# APPENDED to the bundle as a global, after everything else, so no line number of the bundle moves. The SDK uses it to
# turn `function.mjs:L:C` into `src.ts:L:C`.
smflag=(--sourcemap=external --sources-content=false)
[ "${SOURCEMAP:-1}" != "0" ] || smflag=()
npx esbuild "$entry" --bundle --format=esm --target=es2020 ${define[@]+"${define[@]}"} ${smflag[@]+"${smflag[@]}"} \
  --alias:@cleat/ts="$HERE/index.ts" --outfile="$W/$out.js" --log-level=warning || unmeasured "esbuild failed"
# Path hygiene (cleat#3833), ALWAYS: esbuild writes a `// <path>` comment above each file it bundles, relative to this
# directory, and Javy embeds the whole bundle in the module as `javy_source`. For a source outside this repository that
# comment is a run of `../` whose length is the distance between the two, so the same source built in two places was two
# different modules. Every file is relabelled by ONE function, used for those comments and for the source map alike, so a
# label never depends on where the file lives:
#   under the source root        -> relative to it           (src/orders.ts)
#   under this toolchain dir     -> relative to this dir     (index.ts, node_modules/...)
#   reached through node_modules -> from that segment on     (node_modules/zod/lib/index.js, wherever it is hoisted)
#   anything else                -> external/<file name>
# REAL paths on every side: on macOS /var is a symlink to /private/var, the source root comes from `pwd` (logical) and
# esbuild reports the real path, so comparing them as written never matches.
SOURCEMAP=${SOURCEMAP:-1} python3 - "$W/$out.js" "$W/$out.js.map" <<'PY'
import json, os, re, sys
js, mp = sys.argv[1:3]
here = os.path.realpath(os.getcwd())
root = os.path.realpath(os.environ.get("SRC_ROOT", here))
def under(a, d):
    r = os.path.relpath(a, d)
    return None if r == ".." or r.startswith(".." + os.sep) else r
def label(a):
    a = os.path.realpath(a)
    r = under(a, root)
    if r is not None:
        return r
    r = under(a, here)
    if r is not None:
        return r
    parts = a.split(os.sep)
    if "node_modules" in parts:
        return "/".join(parts[parts.index("node_modules"):])
    return "external/" + os.path.basename(a)
src = open(js).read()
# esbuild resolves these against its working directory, which is this one.
def comment(m):
    p = os.path.join(here, m.group(1))
    return "// " + label(p) if os.path.isfile(p) else m.group(0)
src = re.sub(r"(?m)^// (\S.*)$", comment, src)
# A CommonJS (or lazily initialised ESM) dependency is wrapped as `__commonJS({ "<path>"(exports, module) { ... } })`: the
# module's KEY is the same relative path, so it needs the same label.
def wrapped(m):
    p = os.path.join(here, m.group(2))
    return m.group(1) + '"' + label(p) + '"' + m.group(3) if os.path.isfile(p) else m.group(0)
src = re.sub(r'(?m)^(\s*)"([^"\n]+\.(?:[cm]?[jt]sx?|json))"(\()', wrapped, src)
if os.environ.get("SOURCEMAP", "1") != "0":
    m = json.load(open(mp))
    base = os.path.dirname(mp)
    srcs = [label(os.path.join(base, x)) for x in m["sources"]]
    # esbuild's own comment: the map is carried in-bundle instead, APPENDED after everything else so no line number of the
    # bundle moves. The SDK uses it to turn `function.mjs:L:C` into `src.ts:L:C`.
    src = re.sub(r"\n?//# sourceMappingURL=.*\n?$", "\n", src)
    payload = json.dumps({"s": srcs, "m": m["mappings"]}, separators=(",", ":"))
    src = src + "globalThis.__cleat_sm = " + json.dumps(payload) + ";\n"
    print(f"source map embedded: {len(payload):,} bytes for {len(srcs)} source file(s)")
open(js, "w").write(src)
PY

# WIT world and link pairs from the ABI entry names.
wit="$W/$out.wit"; pairs=()
{ echo "package cleat:ts;"; echo "world w {"; } > "$wit"
IFS=',' read -ra E <<< "$entries"
for e in "${E[@]}"; do k=${e//_/-}; echo "  export $k: func();" >> "$wit"; pairs+=("$e=$k"); done
echo "}" >> "$wit"

# -C deterministic: fixed clocks and a zero RNG during Wizer pre-initialisation, so identical source gives identical
# bytes (without it two builds of one bundle differ in 3 bytes of a pre-initialised data segment).
"$J" build "$W/$out.js" -C plugin="$PLUGIN" -C deterministic="${JAVY_DETERMINISTIC:-y}" -C wit="$wit" -C wit-world=w \
  -o "$W/$out.javy.wasm" || unmeasured "javy build failed"
"$LINK_DIR/target/release/cleat-js-link" "$W/$out.javy.wasm" "$W/$out.linked.wasm" "${pairs[@]}" || unmeasured "cleat-js-link failed"
# The engine pin: what this module carries, computed from the real artefacts (nothing typed). QuickJS comes from the
# plugin's lockfile, the plugin and SDK by content hash. A rebuild with any of them changed is a different pin.
python3 - "$J" "$PLUGIN" "$W/$out.pin.json" "${JAVY_DETERMINISTIC:-y}" "$PLUGIN_DIR/Cargo.lock" "$HERE/index.ts" <<'PY'
import hashlib, json, re, subprocess, sys
javy, plugin, out, det, lockfile, sdk = sys.argv[1:7]
sha = lambda p: hashlib.sha256(open(p, "rb").read()).hexdigest()
lock = open(lockfile).read()
qjs = re.search(r'name = "rquickjs-core"\nversion = "([^"]+)"', lock).group(1)
json.dump({"pin_version": 1, "javy": subprocess.check_output([javy, "--version"], text=True).strip(), "rquickjs_core": qjs,
           "plugin_sha256": sha(plugin), "sdk_sha256": sha(sdk), "deterministic_build": det == "y"},
          open(out, "w"), sort_keys=True, indent=1)
PY
( cd "$ROOT" && go run ./cmd/cleat-ts-meta -in "$HERE/$W/$out.linked.wasm" -out "$HERE/$W/$out.wasm" -name "$out" \
    -entries "$entries" -language javascript -pin "$HERE/$W/$out.pin.json" ) || unmeasured "cmd/cleat-ts-meta failed"
if [ -n "${OUT_DIR:-}" ]; then mkdir -p "$OUT_DIR" && cp "$W/$out.wasm" "$OUT_DIR/$out.wasm"; fi
echo "built $W/$out.wasm $(wc -c < "$W/$out.wasm" | tr -d ' ') bytes"
