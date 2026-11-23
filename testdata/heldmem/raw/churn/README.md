Churn measurement, 2026-11-23 (cleat#4377, the owner's request: the post-fix after-churn high-water).
HELDMEM_CHURN = MiB allocated and dropped in 1 MiB blocks; HELDMEM_CHURN_SMALL = the same total in 16 KiB blocks. Guests: testdata/heldmem/cs-churn/Program.cs (the C# `heldmem` case plus churn;
build with and without the GCSegmentSize IlcArg) and testdata/heldmem/held_mem_churn.py (built with python-sdk/scripts/build_wasm.py at the cleat#4387 head).
churn.jsonl: 1 MiB blocks, python + csharp at 8 MiB segments. churn_pos.jsonl: the C# known-positive at DEFAULT segments (it did NOT reproduce the old 65.7: 48.99 vs 48.89).
churn_small.jsonl: 16 KiB blocks; python, csharp at 8 MiB (wasm 1177394) and at default segments (wasm 1177370). Fit: delta Pss = shared + N x per-workflow, N = 4..32.
