cd /src/python-sdk
for i in 0 1 2 3 4 5 6 7 8; do
  python3 scripts/build_wasm.py --entry /e2e/src/hm$i.py:held_mem --output /e2e/wasm/hm$i.wasm --runtime wasmtime --name hm$i --version 1 > /tmp/b$i.log 2>&1 && echo "built hm$i: $(stat -c%s /e2e/wasm/hm$i.wasm) bytes $(grep -o 'Dense data:[^.]*' /tmp/b$i.log | head -1 | cut -c1-60)" || { echo "FAILED hm$i"; tail -3 /tmp/b$i.log; }
done
sha256sum /e2e/wasm/*.wasm | cut -c1-16,65- 
