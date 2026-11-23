#!/bin/bash
cd /tmp/wasm
for f in t4/two_tiny_1KiB_and_20MiB t4/two_tiny_1KiB_and_16MiB synth/S2_replica_2539segs_same_offsets py_main; do
  for opt in "" "-O memory-guaranteed-dense-image-size=67108864" "-O memory-guaranteed-dense-image-size=1073741824"; do
    n=$(strace -f -e trace=memfd_create wasmtime run -W unknown-imports-trap=y $opt $f.wasm 2>&1 | grep -c "memfd_create")
    echo "module=$(basename $f) opt=[${opt:-default}] memfd_create=$n"
  done
done
