# C# quick start

> **Status: tier 2** (`tiers.yaml`, entry `sdk-csharp`; owner decision 2026-11-19). This page describes a workflow that is built and run by
> a test in this repository (`TestCSharpQuickStartExampleSuspendsResumesFromHistoryAndRunsItsDefer`), so it cannot drift from one that works.
> Tier 2 means the tests must run and may fail without blocking a merge. Read "What has not been run" before depending on it.

Cleat runs a C# workflow as a WebAssembly Component: your code is compiled ahead of time with the .NET NativeAOT-LLVM toolchain
(through `componentize-dotnet`) and runs inside the same engine and wasmtime backend that run Go, Rust, Python, Java and TypeScript
workflows. A small SDK (`packages/cleat-csharp`) gives the code its durable calls.

## What has not been run

| | Status |
|---|---|
| A C# workflow built by `cleat build --target csharp` and run on the engine and wasmtime backend, in process, with a recording caller | **yes**: this page's example, and the conformance components under `packages/cleat-csharp/conformance/` |
| A C# workflow on a real `cleat-worker` against a database | **no**: nothing tests it, and nothing here says it works |
| MySQL and SQL Server | **no**: not measured for C# at all |
| A NuGet package | **none**: the SDK is a source package imported by path from a cleat checkout, so a C# workflow is built inside a checkout |
| The toolchain | **a prerelease**: a release-candidate ILCompiler.LLVM and a `0.8.0-preview` componentize-dotnet, pinned by digest and hash (`packages/cleat-csharp/toolchain.env`); a pin is not a support commitment from upstream, and builds are not byte-reproducible |
| Typed plugin clients | **none**: `cleat-gen-plugin` has no C# target |

## 1. Prerequisites

A cleat checkout, Go, and Docker. The compiler runs only on x64 Linux or Windows, so `cleat build --target csharp` runs it in a pinned
Linux container (linux/amd64, or linux/arm64 natively on an arm64 host). There is no native macOS build.

## 2. Write a workflow

`examples/csharp-workflow/Orders.cs`:

```csharp
[CleatEntry("PlaceOrder")]
public static JsonNode PlaceOrder(HostCalls h, JsonElement input)
{
    // Registered first, so it runs LAST; a workflow that fails, or is terminated, still runs its defers.
    h.Defer("release-hold", () => h.Call("inventory", "ReleaseHold", input.GetRawText()));

    var charge = h.Call("payments", "Charge", input.GetRawText());   // durable: recorded, replayed from history
    h.Sleep(TimeSpan.FromSeconds(5));                                  // durable: the run suspends here and resumes later
    var recorded = h.Call("orders", "Record", charge);                 // durable: only called once, however often the run is replayed

    return new JsonObject { ["charge"] = JsonNode.Parse(charge), ["order"] = JsonNode.Parse(recorded) };
}
```

Everything durable goes through `HostCalls`. The engine records each call, so after a restart, or when the run resumes after the sleep,
the guest is replayed and each call answers from history instead of repeating its effect.

Three rules the example follows, and one the build enforces:

- **To wait, call `HostCalls.Sleep`.** `Task.Delay`, `Task.Yield`, `Task.Run`, a thread, and a blocking wait on an incomplete task throw
  `PlatformNotSupportedException`. `async`/`await` over host calls works only because every awaited task is already complete.
- **A suspension is an exception, and it can be caught.** A `catch (Exception)` that swallows `CleatSuspendException` does not change the
  run's outcome, but the workflow keeps running past it and the host calls it then makes are real. Do not swallow it; the vet warns (`CV101`).
- **The determinism vet** runs as part of the build (`cleat vet --lang csharp <project>` runs it alone). It is a text scan with no type
  information, so an alias declared in another file, reflection, or an extension method wrapping a refused API are invisible to it. It
  refuses what was measured to differ or fail on the real engine: `Environment.TickCount`, `Stopwatch`, `Thread.Sleep`, `Task.Run`,
  `Task.Delay`, threads, `File` and `HttpClient`. `DateTime.UtcNow`, `Random` and `Guid.NewGuid()` were measured replay-stable and are
  not refused.

## 3. Build it

From the checkout root:

```
cleat build --target csharp -o out --version 1 examples/csharp-workflow
```

The output is `out/orders.wasm`: a component stamped with `cleat.metadata` naming `csharp`, which is what routes it to the wasmtime
backend as a C# guest. Exit status `0` is a built component, `1` is a finding about the project (the vet, or the build), and `2` is
`UNMEASURED`: the build could not run (no Docker, no pinned image), which is a failure of the build and says nothing about your project.

## 4. What the test measured

`TestCSharpQuickStartExampleSuspendsResumesFromHistoryAndRunsItsDefer` builds exactly that project and runs it on the engine:

1. **The first run suspends in the sleep** after exactly one call, `payments/Charge`.
2. **The resumed run answers the charge from history.** The test ages the recorded charge past the five seconds; the second run then
   makes `orders/Record` and the defer's `inventory/ReleaseHold`, and does **not** call `payments/Charge` again.
3. **The defer runs once, last**, and the result is the charge and the order the services answered.

That is an in-process run with a recording caller. Killing a real worker in the middle of a C# workflow is covered at the engine level
(the terminate and defer-segment tests), not by killing a worker process.

## Where to go next

- `packages/cleat-csharp/README.md` for the SDK surface, typed entries (a source-generated `JsonSerializerContext` you write; the first
  typed record costs about 1.6 MB and about 140 ms of first execution, measured under amd64 emulation), and the full list of limits.
- `tiers.yaml`, entry `sdk-csharp`, for the support claim and its open items.
