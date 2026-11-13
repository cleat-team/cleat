#!/usr/bin/env bash
# Phase 4: build the benchmark workflow (n recorded random host calls, then an optional sleep) in TypeScript (static Javy), Go,
# Rust and Python into .work/bench/<lang>.wasm. Python needs componentize-py on PATH (CLEAT_PROTO_COMPONENTIZE_PY_DIR=<bin dir>);
# without it the Python module is reported UNMEASURED by the harness, never silently left out.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
ROOT=$(cd ../.. && pwd); X=.work/bench; rm -rf "$X"; mkdir -p "$X"
./build.sh bench bench_steps | tail -1; cp .work/bench.wasm "$X/ts.wasm"
# Dynamic mode, SIZE ONLY: the stock engine cannot instantiate it (PHASE1.md, "Dynamic mode"), so this is measured, never run.
.tools/javy build .work/bench.js -C plugin=.work/cleat-plugin.wasm -C dynamic=y -C wit=.work/bench.wit -C wit-world=w -o "$X/ts_dynamic_not_runnable.wasm"
TMPGO=$(mktemp -d)
GOWORK=off GOFLAGS=-mod=mod GOPROXY=off go run github.com/cleat-team/cleat/cmd/cleat build -o "$TMPGO" ./bench/gobench/ >/dev/null
cp "$TMPGO"/*.wasm "$X/go.wasm"; rm -rf "$TMPGO"
# Through `cleat build --target rust`, so the module carries the metadata and entry-point sections `cleatctl deploy` needs (the raw
# cargo output the Q4 harness uses does not); the stamped module is ~160 bytes larger.
TMPRS=$(mktemp -d)
( cd "$ROOT" && env -u GOWORK -u GOFLAGS -u GOPROXY go run ./cmd/cleat build --target rust -o "$TMPRS" prototypes/ts-javy/bench/rust >/dev/null 2>&1 )
cp "$TMPRS"/bench_rust.wasm "$X/rust.wasm"; rm -rf "$TMPRS"
if [ -n "${CLEAT_PROTO_COMPONENTIZE_PY_DIR:-}" ]; then
  TMPPY=$(mktemp -d)
  ( cd bench/python && PATH="$CLEAT_PROTO_COMPONENTIZE_PY_DIR:$PATH" env -u GOWORK -u GOFLAGS -u GOPROXY go run -C "$ROOT" ./cmd/cleat build --target python --entry "$PWD/bench_workflow.py:bench_steps" -o "$TMPPY" 2>&1 | tail -3 )
  cp "$TMPPY"/*.wasm "$X/python.wasm"; rm -rf "$TMPPY"
else
  echo "python: UNMEASURED (CLEAT_PROTO_COMPONENTIZE_PY_DIR not set)"
fi
ls -l "$X"/*.wasm | awk '{print $5, $9}'
