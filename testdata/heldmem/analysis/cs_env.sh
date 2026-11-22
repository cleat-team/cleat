#!/bin/bash
cd /src/engine; export GOFLAGS=-buildvcs=false
run() { tag=$1; envs=$2; rm -f /tmp/env_$tag.jsonl
  HELDMEM_GUEST_ENV="$envs" HELDMEM_CS_WASM=../testdata/heldmem_cs_e.wasm HELDMEM_LANG=csharp HELDMEM_MODE=hold HELDMEM_N=4 HELDMEM_OUT=/tmp/env_$tag.jsonl /tmp/engine8.test -test.run '^TestMeasureHeldInstanceMemory$' -test.count=1 -test.timeout 5m > /tmp/env_$tag.out 2>&1
  if [ -s /tmp/env_$tag.jsonl ]; then python3 -I -c "
import json
r=json.loads(open('/tmp/env_$tag.jsonl').readline()); b,h=r['base'],r['held']
i=json.loads(r['guest_info']); j=json.loads(r['guest_info2'])
print('%-34s per-instance Pss %6.2f MiB | GC available %5d MiB | after-GC committed %6d KiB' % ('$tag [$envs]'[:34], (h['Pss']-b['Pss'])/4/1024, i['available']/2**20, j['after_gc_committed']/1024))
"; else echo "$tag [$envs]: DID NOT START: $(grep -m1 -oE 'runtime failed to initialize|UNMEASURED[^"]{0,90}' /tmp/env_$tag.out)"; fi; }
run control ""
run hardlimit64 "DOTNET_GCHeapHardLimit=0x4000000"
run hardlimit128 "DOTNET_GCHeapHardLimit=0x8000000"
run gen0size "DOTNET_GCgen0size=0x100000"
run gen0max "DOTNET_GCgen0MaxBudget=0x100000"
run conserve "DOTNET_GCConserveMemory=9"
run regionrange "DOTNET_GCRegionRange=0x8000000"
run regionsize "DOTNET_GCRegionSize=0x40000"
run concurrent0 "DOTNET_gcConcurrent=0"
run combined "DOTNET_GCHeapHardLimit=0x8000000,DOTNET_GCgen0size=0x100000,DOTNET_GCConserveMemory=9"
echo ENV-DONE
