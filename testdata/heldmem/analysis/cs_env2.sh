#!/bin/bash
cd /src/engine; export GOFLAGS=-buildvcs=false
run() { tag=$1; envs=$2; rm -f /tmp/env2_$tag.jsonl
  HELDMEM_GUEST_ENV="$envs" HELDMEM_CS_WASM=../testdata/heldmem_cs_e.wasm HELDMEM_LANG=csharp HELDMEM_MODE=hold HELDMEM_N=4 HELDMEM_OUT=/tmp/env2_$tag.jsonl /tmp/engine8.test -test.run '^TestMeasureHeldInstanceMemory$' -test.count=1 -test.timeout 5m > /tmp/env2_$tag.out 2>&1
  if [ -s /tmp/env2_$tag.jsonl ]; then python3 -I -c "
import json
r=json.loads(open('/tmp/env2_$tag.jsonl').readline()); b,h=r['base'],r['held']
print('%-26s [%s]  per-instance Pss %6.2f MiB' % ('$tag', '$envs', (h['Pss']-b['Pss'])/4/1024))
"; else echo "$tag [$envs]: DID NOT START: $(grep -m1 -oE 'runtime failed to initialize|UNMEASURED[^"]{0,90}' /tmp/env2_$tag.out)"; fi; }
run control ""
run seg1M "DOTNET_GCSegmentSize=0x100000"
run seg4M "DOTNET_GCSegmentSize=0x400000"
run seg8M "DOTNET_GCSegmentSize=0x800000"
run seg16M "DOTNET_GCSegmentSize=0x1000000"
run seg4M_compat "COMPlus_GCSegmentSize=0x400000"
run hardlimitSOH "DOTNET_GCHeapHardLimitSOH=0x400000,DOTNET_GCHeapHardLimitLOH=0x400000,DOTNET_GCHeapHardLimitPOH=0x400000"
run lohthreshold "DOTNET_GCLOHThreshold=0x20000"
run gcheapcount1 "DOTNET_GCHeapCount=1,DOTNET_gcServer=0"
echo ENV2-DONE
