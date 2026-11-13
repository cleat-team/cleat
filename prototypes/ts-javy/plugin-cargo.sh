#!/usr/bin/env bash
# The ONE place the Javy plugin is compiled, so every build of it (shipped, control variants, the path-independence check) gets
# the same flags. usage: [CLEAT_SHIMS_OFF=..] [CLEAT_FAULT=..] [PLUGIN_DIR=<dir>] plugin-cargo.sh
# Flags: --remap-path-prefix keeps the cargo home directory (a user's name) out of the module's panic-location strings. It does
# not affect symbol hashes (measured: see PHASE3.md); the path dependency in plugin/Cargo.toml is what did.
set -euo pipefail
dir=${PLUGIN_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/plugin}
cd "$dir"
export RUSTFLAGS="${RUSTFLAGS:-} --remap-path-prefix=${CARGO_HOME:-$HOME/.cargo}=/cargo"
cargo build --offline --target wasm32-wasip1 --release 2>&1 | grep -E '^(error|warning: unused)|Finished'
