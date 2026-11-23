# cleat C# SDK (tier 2, prerelease toolchain)

C# is a **tier-2 guest language** (owner decision, recorded on
[cleat#4080](https://github.com/cleat-team/cleat/issues/4080)). This package is the SDK half of that work; `tiers.yaml`
(entry `sdk-csharp`) is the authority on what is claimed and on the open items, which include a prerelease toolchain, no NuGet
package, and no run on a real worker against a database. Nothing here is a claim that a C# workflow runs in production.

A C# workflow is compiled to a WebAssembly **component** by `componentize-dotnet` against the one WIT world the
engine reads, `python-sdk/wit/cleat.wit` (world `cleat-workflow`). This package supplies the layer between the
bindings that toolchain generates and the code a workflow author writes.

## Layout

| Path | What it is |
|---|---|
| `src/Runtime/` | The SDK's binding-free half: the entry boundary, defer table, exceptions, JSON helpers. Runs under a plain `dotnet test`. |
| `src/Bindings/HostCalls.cs` | `HostCalls`, the workflow's handle on the durable host calls. It sits on the generated bindings, so it only compiles inside a wasm build. |
| `generator/` | A Roslyn source generator. It emits `CleatWorkflowWorldExportsImpl` (the class the bindings require) with the dispatch table and the `run` / `run-deferred` exports, and it writes `obj/cleat-entry-points.txt`. |
| `build/Cleat.Sdk.props`, `build/Cleat.Sdk.targets` | What a workflow project imports: adds the SDK sources, points the toolchain at the WIT file, tells the generator where to write the entry-point list. |
| `fixture/`, `fixture-typed/` | Workflows written against the SDK and built to real components. Not examples to copy: each entry covers one path the engine must handle. |
| `tests/` | The managed unit tests and `test.sh`. |
| `toolchain.env`, `build-fixture.sh` | The digest-pinned container and the script that builds a fixture in it. |

The SDK is a **source** package. The bindings it sits on are generated *inside* the consuming project, so the SDK's
types cannot be precompiled: they are compiled into the workflow's own assembly.

## Writing a workflow

```csharp
using System.Text.Json;
using System.Text.Json.Nodes;
using Cleat;

public static class Orders
{
    [CleatEntry("PlaceOrder")]
    public static async Task<JsonNode> PlaceOrder(HostCalls h, JsonElement input)
    {
        var charge = h.Call("payments", "Charge", input.GetRawText());   // durable: recorded, replayed from history
        h.Sleep(TimeSpan.FromSeconds(5));                                 // durable: suspends, then resumes
        h.Defer("refund", () => h.Call("payments", "Refund", charge));    // cleanup, last registered runs first
        return JsonNode.Parse(charge)!;
    }
}
```

- `[CleatEntry("Name")]` goes on a `public static` method. The first parameter is `HostCalls`; an optional second
  parameter is the input. The generator refuses a method that does not fit, naming it (`CLEAT001` to `CLEAT009`).
- **Input and result forms that need nothing more:** `JsonElement`, `JsonNode`, `string` (the raw input JSON as input;
  a JSON string as result), and `bool`, `int`, `long`, `double` for results. A method may return `T`, `Task<T>`,
  `ValueTask<T>` or nothing (the result is `null`).
- **Several entry points** in one component are dispatched on the `__cleat_entry__` field the engine adds to the input.
  A component with one entry point needs no such field.
- A number in the input keeps its exact digits (a 20-digit integer, `1.50`). A string is re-encoded, so an escape may be
  spelled differently; the decoded value is the same.

## What a workflow author must know

**Host calls are synchronous, and `async` works over them.** There are no threads and no scheduler. A workflow can use
`async`/`await`, `ValueTask`, `await foreach` and `Task.WhenAll` over `HostCalls`, because every task it awaits is
already complete (measured on the real engine, cleat#4077). `Task.Delay`, `Task.Run` and a blocking wait on
an incomplete task throw `PlatformNotSupportedException` (`Task.Yield` returns; measured, cleat#4080). **To wait, call `HostCalls.Sleep`**, which is durable. The generated
entry blocks on the entry method with `GetAwaiter().GetResult()`, never `.Result`, which would wrap the exception in an
`AggregateException`.

**A suspension is an exception, and it can be caught. Do not.** `HostCalls.Sleep`, and a host call the engine has stopped, throw
`CleatSuspendException`; the generated entry turns it into `run-outcome::suspended`. Measured on the real engine:

- the engine records the suspension on the host side, so a `catch (Exception)` that swallows it **does not change the
  outcome**, which stays "suspended";
- but the workflow **keeps running** after the swallowed exception. Since cleat#4285 the host **refuses** the host calls it then makes (none reaches
  the service or is recorded in history); the code between them still runs. The same happens when the exception is captured in a `Task` that has not been awaited yet:
  code between the capture and the `await` still runs.

Replay reproduces the same sequence, so this is deterministic, but it is not what a Go or Rust workflow does (there the
suspend aborts at once). Never `catch (Exception)` without rethrowing `CleatSuspendException`. The determinism vet warns
about a catch that can swallow it (`CV101`); it is a warning because the same catch around code that cannot suspend is
harmless and a source check cannot tell the two apart.

**The clock and entropy.** Measured on the real engine (cleat#4080), a workflow that reads `DateTime.UtcNow`, `DateTime.Now`,
`DateTimeOffset.UtcNow`, `new Random()`, `Random.Shared` or `Guid.NewGuid()` gets the same values on replay: the engine
pins the wall clock to the workflow's start time and seeds entropy from the workflow's identity, and a run under a
different identity gets different values. `h.Now()` and `h.Random()` are the explicit forms and remain the clearer choice.
This section said the opposite until cleat#4080, and it was wrong for the same reason cleat#4078 fixed: the shadows reach
every WASI minor a C# toolchain emits. **Monotonic clocks are not pinned, in any language** (see `docs/determinism.md`, "The monotonic clock is real elapsed time"): `Stopwatch` returns a different value on every run, and `Environment.TickCount64`
(a few milliseconds since the instance started) differs from its replay in most runs and coincides in some, so the vet
refuses both. `Thread.Sleep` really sleeps (it holds the worker and is not
durable), `Task.Run`, `Task.Factory.StartNew`, `Task.Delay`, `.Wait()` and `new Thread` throw `PlatformNotSupportedException`, and `Task.Yield`
is fine. `RandomNumberGenerator` throws. `File` and `Directory` have no filesystem behind them (`File.Exists` answers
`false` without error). **`HttpClient` makes the whole component fail to instantiate**, every entry point and not only the
one that uses it, because it imports `wasi:http`, which the engine does not provide.

**The determinism vet** runs inside `cleat build --target csharp`, before the toolchain is looked for, and as
`cleat vet --lang csharp <dir>`. It reads the `.cs` files only, resolves names rather than matching text, and refuses
exactly the list above. Exit status 0 is clean, 1 is a finding, 2 means the vet could not read the project (UNMEASURED:
a failure of the check, not a finding). What it cannot see: there is no type information, so a refused API reached through
an alias it cannot resolve, a delegate, reflection or an extension method that wraps one is invisible; `.Result` and
`GetResult()` are not judged, since on a completed task they are ordinary; the effect of the thread pool was not
measured. A `Compile` item in the project file is not interpreted.

**Failures.** A failed host call throws `CleatCallException`, in the same classes as the Python SDK
(`CleatCallTransientException`, `CleatCallTimeoutException`, `CleatCallPermanentException`, chosen by the host's error
code). An exception that leaves the entry method becomes `run-outcome::failed` with `"<Type>: <message>"`, after the
workflow's defers run. **Do not set `UseSystemResourceKeys` in the project**: under it an exception's `Message` is a
resource key such as `Arg_PlatformNotSupported`, and `Message` is what the failure carries.

**Defers** run last-registered-first. The guest drains them when the workflow finishes or fails; when the workflow is
terminated the host drains them on the same instance (`run-deferred`). A body that throws does not stop the others. A
suspended run does **not** drain: the workflow has not exited.

## JSON, and what typed entries cost

Reflection-based `System.Text.Json` is unusable under NativeAOT (`JsonSerializerIsReflectionDisabled`). The built-in
forms above use `JsonElement` / `JsonNode` / `JsonValue`. A **typed** entry (a `record` for the input or the result) goes
through a source-generated `JsonSerializerContext` that the author declares and names in the attribute:

```csharp
public record Order(string Id, int Qty);
[JsonSerializable(typeof(Order))] public partial class Json : JsonSerializerContext { }

[CleatEntry("Typed", JsonContext = typeof(Json))]
public static Order Typed(HostCalls h, Order order) => order with { Qty = order.Qty * 2 };
```

(The context has to be written by the author because a Roslyn generator cannot read another generator's output, so this
SDK's generator cannot emit it.) Typed output uses the context's own options, not the relaxed encoder the built-in forms
use, so a non-ASCII character in a typed result is written as `\uXXXX`.

**Measured 2026-11-18** with `fixture-typed/`, on the wasmtime backend with a 256 MiB store, first execution on a fresh
engine (it includes wasmtime compiling the component), five runs each, replay as the median of 50 on a warm engine. The
compiler ran under amd64 emulation. All variants build the same SDK with the spike's size options.

| Variant | Artifact | vs no typed JSON | First execution | Replay (p50) |
|---|---:|---:|---:|---:|
| no typed JSON (`JsonElement` in and out) | 1,349,382 B | | 153 to 184 ms | 4.0 ms |
| typed, 1 root record (`Order`, nesting `Address`) | 2,926,176 B | +1.58 MB | 290 to 311 ms | 4.9 ms |
| typed, 8 root records (11 record types, an enum, `Dictionary`, `Guid`, a recursive type) | 3,465,191 B | +2.12 MB | 355 to 373 ms | 5.4 ms |

**The cost is mostly fixed.** The first typed record costs about 1.6 MB and about 140 ms of first execution, because it
links the serialiser itself. Seven more roots (nine more record types) add about 0.54 MB, roughly 60 KB per type, and
about 65 ms. So typed-by-default would charge a workflow with one record almost as much as one with ten, which is the
reason typed entries are opt-in. Re-derive with `./build-fixture.sh fixture-typed` under `CLEAT_CS_BUILD_ARGS`
`-p:DefineConstants=NONE` (no typed JSON), unset (one root) and `-p:DefineConstants=MANY` (eight roots).

## Building and testing

Everything runs in the digest-pinned `linux/amd64` .NET SDK container named in `toolchain.env`, because the compiler
only runs on x64 Linux or Windows. Do not run these on a natively installed `dotnet`: both scripts use one toolchain pin.

    packages/cleat-csharp/tests/test.sh                 # the managed unit tests
    packages/cleat-csharp/build-fixture.sh fixture      # builds out/fixture.wasm

`HostCalls` and the generated exports need the real bindings, so the unit tests cover everything else and the fixture
covers the rest on the engine: compile the fixture, stamp it with `cleat.metadata` language `csharp`, and run each entry
point on the wasmtime backend with a 256 MiB memory limit (a .NET guest needs at least 52 MiB to start; the engine's
default is 32 MiB, which is a separate change in cleat#4080). Until that engine change lands, the engine run is done
by hand and its output is recorded in the pull request, not enforced by CI.

`dotnet msbuild -getProperty:CleatEntryPointsFile` prints the path of the entry-point list
(`obj/cleat-entry-points.txt`: UTF-8, LF, one name per line, no blank lines) that the build chain reads into
`cleat.metadata.entry_points`.

## What is not here yet

`HostCalls` holds the calls an entry point needs first: `Call`, `Sleep`/`SleepMs`, `Now`, `Random`, `Log`, `WorkflowId`,
`RunId`, `Version`, `Defer`, `PollCancellation`. Every other function in `python-sdk/wit/cleat.wit` (retries and
heartbeats, signals, children, promises, handlers, messaging, plugin calls, locks, scopes, fetch, cron, continue-as-new)
is not wrapped yet; that is the host-call coverage work in cleat#4080. The packed NuGet package, the
`cleat build` target and the tier-2 gate are also later items in that issue.
