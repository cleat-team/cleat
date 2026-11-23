#!/bin/bash
cd /src; export GOFLAGS=-buildvcs=false CGO_ENABLED=1
go test -tags zzmeasure -c -o /tmp/cc.test ./engine 2>&1 | tail -3
rm -f /out/cc.txt
for rep in 1 2; do for w in py_orig py_dense py_harness cs_defer cs_conf8; do
  MEAS_WASM=/cc_wasm/$w.wasm MEAS_K=5 /tmp/cc.test -test.run '^TestMeasureComponentLiveCost$' -test.count=1 2>&1 | grep -E "^LIVE|FAIL|known-positive" | sed "s/^/rep$rep /" >> /out/cc.txt
done; done
echo CC-DONE >> /out/cc.txt
