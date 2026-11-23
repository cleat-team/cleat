#!/bin/bash
cd /src/engine; export GOFLAGS=-buildvcs=false
echo "--- large allocations under 4 MiB segments (standalone, 'alloc' mode keeps mb x 1 MiB arrays):"
for mb in 1 16 64 128; do
  HELDMEM_INIT_ARG="{\"mode\":\"alloc\",\"mb\":\"$mb\"}" HELDMEM_INIT_WASM=/tmp/wasm/cs_g_un/unbundled-module0.wasm /tmp/engine12.test -test.run '^TestMeasureStandaloneInit$' -test.count=1 -test.v 2>&1 | grep -E "^INIT step: after run|^--- " | tr '\n' ' ' | cut -c1-230; echo
done
echo "--- known-positive and fit through the real residency path (variant g):"
rm -f /tmp/seg_fit.jsonl /tmp/seg_pos.jsonl
for rep in 1 2 3; do for n in 1 2 4 8 16 32 64; do HELDMEM_CS_WASM=../testdata/heldmem_cs_g.wasm bash /tmp/hm8.sh csharp hold $n 0 0 /tmp/seg_fit.jsonl; done; done
for rep in 1 2; do for n in 2 8 32; do HELDMEM_CS_WASM=../testdata/heldmem_cs_g.wasm bash /tmp/hm8.sh csharp hold $n 0 16 /tmp/seg_pos.jsonl; done; done
echo SEG-DONE
