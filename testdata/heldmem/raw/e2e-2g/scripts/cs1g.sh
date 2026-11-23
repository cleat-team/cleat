export MEM=1g SAMPLE_S=0.5 E2E_TEST=TestCSharpCompileMemory E2E_VARIANTS=8 E2E_CS="small=/e2e/cs/defer_order.wasm=run=hello,large=/e2e/cs/conformance.wasm=smoke=smoke"
echo "##### OLD head (fixed 800 MiB need), 1 GiB container, --component-precompile=false, 16 distinct C# components"
BIN=/out/slot-after bash /tmp/e2e_run.sh cs1old --component-precompile=false 2>&1 | grep -E "CS-COMPILE-END|exit state|oom_kill|OOMKilled" | cut -c1-150
echo "##### NEW (per-size need), same"
BIN=/out/slot-new bash /tmp/e2e_run.sh cs1new --component-precompile=false 2>&1 | grep -E "CS-COMPILE-END|exit state|oom_kill|OOMKilled" | cut -c1-150
for d in cs1old cs1new; do echo "$d: low_headroom refusals=$(grep -c '"reason":"low_headroom"' /tmp/out4377/e2e_$d.worker.log) peak=$(sed -E 's/.*peak=([0-9]+).*/\1/' /tmp/out4377/e2e_$d.samples | sort -n | tail -1) MiB"; done
echo CS1G-DONE
