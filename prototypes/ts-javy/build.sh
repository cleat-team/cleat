#!/bin/bash
# Phase 0 build: workflow.ts -> esbuild -> javy (static, cleat-js plugin) -> cleat-js-link -> tsmeta.
# usage: build.sh <name> <entry>[,<entry>...]    (reads workflows/<name>/<name>.ts and <name>.wit)
set -euo pipefail
cd "$(dirname "$0")"
name=$1; entries=$2
J=.tools/javy; W=.work
mkdir -p "$W"
npx esbuild "workflows/$name/$name.ts" --bundle --format=esm --target=es2020 --outfile="$W/$name.js" --log-level=warning
world=$(grep -oE 'world [a-z0-9-]+' "workflows/$name/$name.wit" | head -1 | cut -d' ' -f2)
$J build "$W/$name.js" -C plugin="$W/cleat-plugin.wasm" -C wit="workflows/$name/$name.wit" -C wit-world="$world" -o "$W/$name.javy.wasm"
link/target/release/cleat-js-link "$W/$name.javy.wasm" "$W/$name.linked.wasm" ${entries//,/ }
(cd ../.. && go run ./prototypes/ts-javy/tools/tsmeta -in "prototypes/ts-javy/$W/$name.linked.wasm" -out "prototypes/ts-javy/$W/$name.wasm" -name "$name" -entries "$entries")
ls -l "$W/$name.wasm" | awk '{print "built", $9, $5, "bytes"}'
