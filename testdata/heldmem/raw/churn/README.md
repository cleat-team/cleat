Churn measurement, 2026-11-23 (cleat#4377, the owner's request: the post-fix after-churn high-water).
HELDMEM_CHURN = MiB allocated and dropped in 1 MiB blocks; HELDMEM_CHURN_SMALL = the same total in 16 KiB blocks. Guests: testdata/heldmem/cs-churn/Program.cs (the C# `heldmem` case plus churn;
build with and without the GCSegmentSize IlcArg) and testdata/heldmem/held_mem_churn.py (built with python-sdk/scripts/build_wasm.py at the cleat#4387 head).
churn.jsonl: 1 MiB blocks, python + csharp at 8 MiB segments. churn_pos.jsonl: the C# known-positive at DEFAULT segments (it did NOT reproduce the old 65.7: 48.99 vs 48.89).
churn_small.jsonl: 16 KiB blocks; python, csharp at 8 MiB (wasm 1177394) and at default segments (wasm 1177370). Fit: delta Pss = shared + N x per-workflow, N = 4..32.

churn_big.jsonl (HELDMEM_CHURN_BIG, the stage-0 probe's own pattern: 4 MiB chunks, one byte per 4 KiB page, each kept in a static until replaced, then dropped): REPRODUCES the old basis.
Order of rows: for cap in 64,256: for churn in 0,256: for n in 4,8,16: 2 reps x [default-segment build, 8 MiB-segment build]; then python (cap 256). Results (Pss per instance, RSS at N=16):
  default segments, cap 256, churn 256: 64.97 / 65.26 (old probe 65.696)   cap 64: 48.9 / 49.19 (old 49.511)   no churn: 48.9
  8 MiB segments, churn 256, either cap: 57.44 / 57.72   no churn: 16.85        python churn 256: 9.48   no churn: 1.32
churn.jsonl, churn_pos.jsonl, churn_small.jsonl, churn_cap.jsonl used DIFFERENT patterns (blocks dropped at once, 1 MiB or 16 KiB) and show no growth; that was NOT the old basis and I mistook it for it.
