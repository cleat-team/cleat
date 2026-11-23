#!/bin/bash
cd /src/engine; export GOFLAGS=-buildvcs=false
for f in /tmp/wasm/synth/*.wasm; do
  b=$(basename $f .wasm); rm -f /tmp/img2_$b.jsonl
  HELDMEM_IMAGE_WASM=$f HELDMEM_N=8 HELDMEM_IMAGE_OUT=/tmp/img2_$b.jsonl /tmp/engine9.test -test.run '^TestMeasureStandaloneImage$' -test.count=1 > /tmp/img2_$b.out 2>&1
  if [ -s /tmp/img2_$b.jsonl ]; then python3 -I -c "
import json
r=json.loads(open('/tmp/img2_$b.jsonl').readline()); b,h=r['base'],r['held']; n=r['n']
d=lambda k:(h[k]-b[k])/n/1024
print('%-38s per instance: Anon %6.2f MiB  Shmem %5.2f | memfd maps +%d  -> %s' % ('$b', d('Anon'), d('Shmem'), h['MemfdMaps']-b['MemfdMaps'], 'IMAGE' if h['MemfdMaps']-b['MemfdMaps']>=n else 'NO IMAGE (copied)'))
"; else echo "$b: no result: $(grep -m1 -oE 'UNMEASURED[^\"]{0,120}' /tmp/img2_$b.out)"; fi
done
