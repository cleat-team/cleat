#!/bin/bash
cd /tmp/wasm
for f in t4/two_tiny_1KiB_and_16MiB t4/two_tiny_1KiB_and_20MiB synth/S2_replica_2539segs_same_offsets; do
  for opt in "" "-O memory-guaranteed-dense-image-size=67108864" "-O memory-guaranteed-dense-image-size=0" "-O memory-init-cow=n"; do
    wasmtime compile $opt $f.wasm -o /tmp/k.cwasm >/dev/null 2>&1
    echo "$(basename $f)  opt=[${opt:-default}]  precompiled size: $(stat -c %s /tmp/k.cwasm)"
  done
done
