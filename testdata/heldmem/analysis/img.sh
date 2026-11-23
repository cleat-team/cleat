#!/bin/bash
cd /src/engine; export GOFLAGS=-buildvcs=false
for w in go cs_main py_main; do
  for cow in "" 0; do
    rm -f /tmp/img_$w$cow.jsonl
    HELDMEM_COW=$cow HELDMEM_IMAGE_WASM=/tmp/wasm/$w.wasm HELDMEM_N=8 HELDMEM_IMAGE_OUT=/tmp/img_$w$cow.jsonl /tmp/engine9.test -test.run '^TestMeasureStandaloneImage$' -test.count=1 > /tmp/img_$w$cow.out 2>&1
    if [ -s /tmp/img_$w$cow.jsonl ]; then python3 -I -c "
import json
r=json.loads(open('/tmp/img_$w$cow.jsonl').readline()); b,h=r['base'],r['held']; n=r['n']
d=lambda k:(h[k]-b[k])/n/1024
print('%-8s CoW=%-8s pages=%4d  per instance: Anon %6.2f  Shmem %5.2f  Pss %6.2f MiB | memfd maps +%d | %.2f ms/instantiate' % ('$w', '${cow:-default}', r['memory_pages'], d('Anon'), d('Shmem'), d('Pss'), h['MemfdMaps']-b['MemfdMaps'], r['ms_per_instantiate']))
"; else echo "$w CoW=${cow:-default}: no result: $(grep -m1 -oE 'UNMEASURED[^"]{0,120}|panicked[^"]{0,80}' /tmp/img_$w$cow.out)"; fi
  done
done
