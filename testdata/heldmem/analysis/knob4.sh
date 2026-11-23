#!/bin/bash
cd /tmp/wasm
for opt in "" "-O memory-guaranteed-dense-image-size=67108864" "-O memory-init-cow=n"; do
  wasmtime compile $opt py_main.wasm -o /tmp/kp.cwasm >/dev/null 2>&1
  echo "py_main (real)  opt=[${opt:-default}]  precompiled: $(stat -c %s /tmp/kp.cwasm) bytes"
done
wasmtime compile py_main_dense.wasm -o /tmp/kp.cwasm >/dev/null 2>&1; echo "py_main_dense (rewritten)  default: precompiled: $(stat -c %s /tmp/kp.cwasm) bytes"
