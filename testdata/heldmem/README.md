MEASUREMENT harness for the residency memory budget (cleat#4162). NOT for merge to develop as it stands: it is the instrument behind the numbers in the design issue.

    # Go + Python + C# (Linux; the Python toolchain image has componentize-py and wasm-tools):
    go test -c -o /tmp/engine.test ./engine/
    cd engine && HELDMEM_LANG=go|python|csharp|javascript HELDMEM_N=<n> HELDMEM_CALLS=<c> HELDMEM_MB=<m> /tmp/engine.test -test.run '^TestMeasureHeldInstanceMemory$' -test.v

    # TypeScript: build the guest first (on any host), then put it at testdata/heldmem_ts.wasm
    packages/cleat-ts/bootstrap.sh
    OUT_DIR=<dir> packages/cleat-ts/build.sh testdata/heldmem/held_mem.ts held_mem

Report the SLOPE between two values of N (8 and 32), not one reading: a run's base already includes the warm-up instance's retained pages, so N=1 under-reads Python (1.4 MiB at N=1 against 12.1 MiB marginal).

## Measurement-branch patches (NOT for merge)
- `engine/backend_wasmtime.go`: a core-module compile counter and `HELDMEM_COW=0` (turns off `memory_init_cow`; with it off, compiling the Go core module aborts in Cranelift, `MemFlagsSetOverflow`).
- `engine/component_cgo.go`: `HELDMEM_GUEST_ENV="K=V,K=V"` adds environment variables to a component guest.
- `testdata/heldmem/cs/`: the C# measurement guest (a copy of the `csharp_defer_order` project with a `heldmem` mode); build per `engine/testdata/csharp_defer_order/PINS.md`. `testdata/heldmem/raw/`: every run, one JSON line each. `testdata/heldmem/analysis/`: the fits and the drivers.
