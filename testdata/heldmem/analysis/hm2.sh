#!/bin/bash
# usage: hm2.sh lang mode n calls mb out
cd /src/engine && export GOFLAGS=-buildvcs=false CGO_ENABLED=1 HELDMEM_LANG=$1 HELDMEM_MODE=$2 HELDMEM_N=$3 HELDMEM_CALLS=$4 HELDMEM_MB=$5 HELDMEM_OUT=$6
/tmp/engine.test -test.run '^TestMeasureHeldInstanceMemory$' -test.count=1 -test.timeout 20m 2>&1 | grep -E "^--- FAIL|^FAIL|UNMEASURED|PRECONDITION|panic|\[build failed\]" | head -4
