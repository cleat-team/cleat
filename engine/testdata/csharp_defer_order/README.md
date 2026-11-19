# csharp_defer_order

A C# component used by `engine/csharp_defer_segment_e2e_test.go`. `defer_order.wasm` is BUILT from the files here and
checked in so the test needs no .NET toolchain.

It is a spike guest, not an SDK: it hand-writes what an SDK's entry wrapper does (register a defer, make a body call,
drain the defers when the body completes, and treat the engine's `suspended` outcome as "stop"). The C# SDK that will
replace it must keep that contract; this test is the one it has to pass.

Rebuild (see `PINS.md` for the exact image digest and package versions; output is NOT byte-reproducible, so compare
by outcome, never by digest):

    docker run --rm --platform linux/amd64 -v <nuget-cache>:/root/.nuget -v <wasi-sdk-cache>:/root/.wasi-sdk \
      -v "$PWD":/work -w /work mcr.microsoft.com/dotnet/sdk:10.0 dotnet build -c Release
    cp bin/Release/net10.0/wasi-wasm/native/defer_order.wasm .

`wit/cleat.wit` is a copy of `python-sdk/wit/cleat.wit`; the world is shared by every component language. It was refreshed for
cleat#4079, where `durable-defer` became `result<string, call-failure>`, and `defer_order.wasm` was rebuilt against it. The guest
now registers a defer the way an SDK must: the body goes into the table BEFORE the host call, because the call can answer
`err(suspended)`.

`defer_order_pre4079.wasm` is the PREVIOUS build, against the old `-> string` signature, kept on purpose as a real stale component:
`engine/a_component_built_before_the_typed_defer_is_refused_before_the_host_runs_test.go` runs it and expects the refusal. Do not
rebuild it.
