MEASUREMENT harness for the residency memory budget (cleat#4162). NOT for merge to develop as it stands: it is the instrument behind the numbers in the design issue.

    # Go + Python + C# (Linux; the Python toolchain image has componentize-py and wasm-tools):
    go test -c -o /tmp/engine.test ./engine/
    cd engine && HELDMEM_LANG=go|python|csharp|javascript HELDMEM_N=<n> HELDMEM_CALLS=<c> HELDMEM_MB=<m> /tmp/engine.test -test.run '^TestMeasureHeldInstanceMemory$' -test.v

    # TypeScript: build the guest first (on any host), then put it at testdata/heldmem_ts.wasm
    packages/cleat-ts/bootstrap.sh
    OUT_DIR=<dir> packages/cleat-ts/build.sh testdata/heldmem/held_mem.ts held_mem

Report the SLOPE between two values of N (8 and 32), not one reading: a run's base already includes the warm-up instance's retained pages, so N=1 under-reads Python (1.4 MiB at N=1 against 12.1 MiB marginal).
