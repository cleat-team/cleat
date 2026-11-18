#!/usr/bin/env bash
# Regenerates probe-<minor>.wasm: one tiny component per WASI 0.2 minor, each importing wasi:clocks/wall-clock and
# wasi:random/random at that exact version and exporting the cleat run / run-deferred shape. cleat#4078.
#
# The .wasm files are checked in so the test that reads them needs no toolchain; this script is the only way they
# are made and needs `wasm-tools`. Usage: ./build.sh [minor ...]   (default: the set below)
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
MINORS=("$@"); [ ${#MINORS[@]} -gt 0 ] || MINORS=(0.2.0 0.2.3 0.2.6 0.2.9 0.2.10)
for V in "${MINORS[@]}"; do
  work=$(mktemp -d)
  mkdir -p "$work/wit/deps/clocks" "$work/wit/deps/random"
  cat > "$work/wit/world.wit" <<WIT
package test:wasiminor;
world probe {
  import wasi:clocks/wall-clock@$V;
  import wasi:random/random@$V;
  variant run-outcome { completed(string), suspended, failed(string) }
  export run: func(args: string) -> run-outcome;
  export run-deferred: func() -> u32;
}
WIT
  cat > "$work/wit/deps/clocks/wall-clock.wit" <<WIT
package wasi:clocks@$V;
interface wall-clock {
  record datetime { seconds: u64, nanoseconds: u32 }
  now: func() -> datetime;
  resolution: func() -> datetime;
}
WIT
  cat > "$work/wit/deps/random/random.wit" <<WIT
package wasi:random@$V;
interface random {
  get-random-bytes: func(len: u64) -> list<u8>;
  get-random-u64: func() -> u64;
}
WIT
  sed "s/@VERSION@/$V/g" probe.wat.tmpl > "$work/core.wat"
  wasm-tools component embed "$work/wit" "$work/core.wat" --world probe -o "$work/core.wasm"
  wasm-tools component new "$work/core.wasm" -o "probe-$V.wasm"
  rm -rf "$work"
  echo "probe-$V.wasm: $(wc -c < "probe-$V.wasm") bytes, imports: $(wasm-tools component wit "probe-$V.wasm" | grep -oE 'import wasi:[a-z/-]+@[0-9.]+' | tr '\n' ' ')"
done

# bigmem.wasm: needs 44 MiB at instantiation, imports nothing. Tests the per-language memory default.
work=$(mktemp -d); mkdir -p "$work/wit"
cat > "$work/wit/world.wit" <<WIT
package test:bigmem;
world bigmem {
  variant run-outcome { completed(string), suspended, failed(string) }
  export run: func(args: string) -> run-outcome;
  export run-deferred: func() -> u32;
}
WIT
wasm-tools component embed "$work/wit" bigmem.wat --world bigmem -o "$work/core.wasm"
wasm-tools component new "$work/core.wasm" -o bigmem.wasm
rm -rf "$work"
echo "bigmem.wasm: $(wc -c < bigmem.wasm) bytes"
