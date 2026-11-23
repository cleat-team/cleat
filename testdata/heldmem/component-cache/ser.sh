cd /src; export GOFLAGS=-buildvcs=false CGO_ENABLED=1
go test -tags zzmeasure -c -o /tmp/cc2.test ./engine 2>&1 | tail -5
for w in py_orig py_dense py_harness cs_defer cs_conf8; do MEAS_WASM=/cc_wasm/$w.wasm /tmp/cc2.test -test.run '^TestMeasureSerialized$' -test.count=1 2>&1 | grep -E "SERIALIZED|FAIL|panic" | head -2; done
