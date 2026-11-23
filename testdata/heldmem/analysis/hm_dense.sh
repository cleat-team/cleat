#!/bin/bash
rm -f /tmp/hm_dense.jsonl /tmp/hm_dense_ctl.jsonl /tmp/hm_dense_pos.jsonl
for rep in 1 2 3; do for n in 1 2 4 8 16 32 64; do echo "$(date -u +%H:%M:%S) dense rep=$rep n=$n"; HELDMEM_PY_WASM=/tmp/wasm/python_dense.wasm bash /tmp/hm11.sh python hold $n 0 0 /tmp/hm_dense.jsonl; done; done
for rep in 1 2; do for n in 2 8 32; do echo "$(date -u +%H:%M:%S) control(original) rep=$rep n=$n"; HELDMEM_PY_WASM=/tmp/wasm/python.wasm bash /tmp/hm11.sh python hold $n 0 0 /tmp/hm_dense_ctl.jsonl; done; done
for rep in 1 2; do for n in 2 8 32; do echo "$(date -u +%H:%M:%S) dense+16MiB rep=$rep n=$n"; HELDMEM_PY_WASM=/tmp/wasm/python_dense.wasm bash /tmp/hm11.sh python hold $n 0 16 /tmp/hm_dense_pos.jsonl; done; done
echo DENSE-DONE
