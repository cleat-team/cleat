#!/bin/bash
# For each variant: the least cap at which the guest still starts (N=1 hold), and the per-instance cost at N=8 under the default cap.
cd /src/engine; export GOFLAGS=-buildvcs=false CGO_ENABLED=1
for v in a b d; do
  for cap in 36 40 44 48 50 52 54 56 64 96; do
    rm -f /tmp/cap_$v_$cap.jsonl
    HELDMEM_CS_WASM=../testdata/heldmem_cs_$v.wasm HELDMEM_MEMLIMIT_MB=$cap HELDMEM_LANG=csharp HELDMEM_MODE=hold HELDMEM_N=1 HELDMEM_OUT=/tmp/cap_${v}_$cap.jsonl /tmp/engine6.test -test.run '^TestMeasureHeldInstanceMemory$' -test.count=1 -test.timeout 5m > /tmp/cap_${v}_$cap.out 2>&1
    if [ -s /tmp/cap_${v}_$cap.jsonl ]; then echo "variant=$v cap=${cap}MiB STARTS"; else echo "variant=$v cap=${cap}MiB does NOT start: $(grep -m1 -oE 'runtime failed to initialize|memory.*(limit|exceed)[^"]{0,60}|UNMEASURED[^"]{0,80}' /tmp/cap_${v}_$cap.out | head -1)"; fi
  done
done
echo CAP-DONE
