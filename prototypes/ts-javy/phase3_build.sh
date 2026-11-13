#!/usr/bin/env bash
# Build the three place_order modules for the Q4 harness into .work/xlang/: Go (testdata/crosslang, via cleat build), Rust
# (examples/rust-workflow, wasm32-unknown-unknown, the shape `cleat build --target rust` ships) and the TypeScript twin.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
ROOT=$(cd ../.. && pwd); X=.work/xlang; rm -rf "$X"; mkdir -p "$X/go"
./build.sh crosslang place_order | tail -1; cp .work/crosslang.wasm "$X/ts.wasm"
./build.sh jsoncanon jsoncanon | tail -1; cp .work/jsoncanon.wasm "$X/jsoncanon.wasm"
# The output directory must be OUTSIDE the repo: `cleat build` generates a go.mod there, and under the repo tree it becomes a nested
# module the root cannot resolve ("main module does not contain package ...").
TMPGO=$(mktemp -d)
# The repo root builds WITH its go.work (it resolves the cleat/ SDK module through it); the nested module builds WITHOUT. Set each
# explicitly: the script must not depend on the caller's GOWORK.
( cd "$ROOT" && env -u GOWORK -u GOFLAGS -u GOPROXY go run ./cmd/cleat build -o "$TMPGO" ./testdata/crosslang >/dev/null )
cp "$TMPGO"/crosslang.wasm "$X/go.wasm"; rm -rf "$TMPGO"
( cd "$ROOT/examples/rust-workflow" && cargo build --target wasm32-unknown-unknown --release 2>&1 | tail -1 )
cp "$ROOT/examples/rust-workflow/target/wasm32-unknown-unknown/release/rust_workflow.wasm" "$X/rust.wasm"
./build.sh nan nan | tail -1; cp .work/nan.wasm "$X/nan_ts.wasm"
./build.sh xjourney x_journey | tail -1; cp .work/xjourney.wasm "$X/xjourney_ts.wasm"
# The multi-key result pair: TS with the SDK's canonical result (default), TS with raw JSON.stringify (the red control), and the Go twin.
./build.sh multikey multikey | tail -1; cp .work/multikey.wasm "$X/multikey_ts.wasm"
RESULT_CANONICAL=false OUT_NAME=multikey_raw ./build.sh multikey multikey | tail -1; cp .work/multikey_raw.wasm "$X/multikey_ts_raw.wasm"
TMPG2=$(mktemp -d)
GOWORK=off GOFLAGS=-mod=mod GOPROXY=off go run github.com/cleat-team/cleat/cmd/cleat build -o "$TMPG2" ./gocontrol/ >/dev/null
# one Go module holds every Go twin (go_journey, go_sleeper, go_multi_key, x_journey)
cp "$TMPG2"/*.wasm "$X/gocontrol.wasm"; rm -rf "$TMPG2"
ls -l "$X"/*.wasm | awk '{print $5, $9}'
