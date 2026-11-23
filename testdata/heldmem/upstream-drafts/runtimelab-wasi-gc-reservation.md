# DRAFT, NOT POSTED. Needs the project owner's say-so before it goes to dotnet/runtimelab.

**Title (feature/NativeAOT-LLVM, WASI):** an empty workflow costs ~48 MiB: the GC "reserves" three 16 MiB segments with `posix_memalign` + `memset(0)`

On `wasm32-unknown-wasip2`, the first call into managed code grows linear memory from 20 pages (1.25 MiB) to 809 pages (50.6 MiB) and **writes 48.6 MiB of zeros**: `src/coreclr/gc/wasm/gcenv.wasm.cpp` `VirtualReserveInner` is `posix_memalign` followed by `memset(pRetVal, 0, size)`, so a reservation costs its whole size in resident memory. The sizes: `src/coreclr/gc/gc.cpp` `INITIAL_ALLOC` and `LHEAP_ALLOC` are 16 MiB each for a 32-bit workstation GC (small, large and pinned heaps: 3 x 16 MiB = 48 MiB). The WASI "physical memory" is hard-coded to 2 GiB, which the GC sizes itself against.

Workaround found: `--runtimeopt:GCSegmentSize=0x400000` (floor 4 MiB) takes it to 14.5 MiB of linear memory / 12.5 MiB resident. It is a private GC setting and only `--runtimeopt` reaches it (`RuntimeHostConfigurationOption` becomes `--runtimeknob:` for public keys, which do not include it).

**Ask:** a smaller default for WASI (or not zero-filling a reservation that the GC will commit lazily), and/or documenting `GCSegmentSize` for NativeAOT-LLVM. Measured on `Microsoft.DotNet.ILCompiler.LLVM 10.0.0-rc.1.26306.1`, wasi-sdk 29, wasmtime 48 (aarch64).
