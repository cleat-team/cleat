#!/bin/bash
cd /tmp/wasm
for opt in "" "-O memory-guaranteed-dense-image-size=1048576" "-O memory-guaranteed-dense-image-size=0" "-O memory-guaranteed-dense-image-size=4294967296"; do
  for f in t4/two_tiny_1KiB_and_16MiB t4/two_tiny_1KiB_and_20MiB; do
    n=$(strace -f -e trace=memfd_create wasmtime run -W unknown-imports-trap=y $opt $f.wasm 2>&1 | grep -c memfd_create)
    echo "opt=[${opt:-default}]  $(basename $f): memfd_create=$n"
  done
done
echo "--- same, with a separate compile step (the knob at compile time) then run the precompiled file:"
for opt in "" "-O memory-guaranteed-dense-image-size=67108864"; do
  wasmtime compile $opt t4/two_tiny_1KiB_and_20MiB.wasm -o /tmp/k20.cwasm 2>&1 | head -2
  n=$(strace -f -e trace=memfd_create wasmtime run --allow-precompiled -W unknown-imports-trap=y /tmp/k20.cwasm 2>&1 | grep -c memfd_create)
  echo "compile opt=[${opt:-default}] 20MiB-gap: memfd_create at run=$n"
done
