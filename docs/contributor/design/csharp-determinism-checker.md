# The C# determinism checker: measure first, refuse what was measured

**Status: implemented, cleat#4080 (PR 6).** C# is a tier-2 language in `tiers.yaml` (promoted in PR 7); this checker is a text
scan, and what it cannot see is listed below and in that entry.

## What it is

`cmd/cleat/vet_csharp.go`, pure Go, run by `cleat build --target csharp` after the project is found
and before the toolchain is looked for (so a refused project is refused on a machine that could not
have built it, with no container start), and by `cleat vet --lang csharp <dir>`. Shaped like
`vet_java.go`: `csharpCodeOnly` blanks comments, regular/verbatim/interpolated/raw strings and
character literals while keeping every offset, then `findForbiddenCSharp` resolves names.

Exit status: `0` clean, `1` a finding, `2` UNMEASURED (the project could not be read: no directory,
no `.cs` file). `2` is a failure of the check and never a pass.

## Why the rules are what they are

A rule by name is a guess. The probe (`packages/cleat-csharp/probe`, driven by
`tests/csharp/determinism_probe_test.go`) is a component whose entries each use one API, run four
ways on the real engine and wasmtime backend: fresh, fresh again, replayed over the first run's
history, and fresh under a **different workflow identity**. The fourth is the control: a value the
engine pins to the identity must differ there, or "it replays" would be equally true of a constant.
The table is **asserted**, not only logged: `probeWant` and `probeSwallowWant` in the test fail on any
row that differs, any row nobody wrote a verdict for, and any run that failed outright, so the
permitted half of the vet (the replay-stable rows) cannot go stale silently if a toolchain or engine
change breaks replay for one of them. Re-derive the table with `CLEAT_PROBE_REPORT=<file> go test ./tests/csharp/ -run
TestCSharpDeterminismProbeMeasurements -count=1`.

| API | measured | vet |
|---|---|---|
| `Task.Run`, `Task.Factory.StartNew`, `Task.Delay`, `.Wait()`, `new Thread` | `PlatformNotSupportedException` | error CV001-CV004 |
| `Thread.Sleep` | really sleeps: holds the worker, not durable | error CV005 |
| `Stopwatch` | real monotonic clock (ns), differs on every run | error CV006 |
| `Environment.TickCount[64]` | ms since instance start, **not pinned**: differs from its replay in most runs and coincides in some (7 of 9 recorded runs differed), so it is refused as unreplayable rather than claimed to always differ | error CV007 |
| `RandomNumberGenerator` | `PlatformNotSupportedException` | error CV008 |
| `File`, `Directory` | no filesystem; `File.Exists` answers `false` quietly | error CV009 |
| `HttpClient` | imports `wasi:http`; **every entry** of the component fails to instantiate | error CV010 |
| `DateTime.UtcNow/Now`, `DateTimeOffset.UtcNow`, `Random`, `Guid.NewGuid()`, `Task.Yield`, `Console`, `Environment` constants | replay-stable, or harmless | not refused |
| a catch that swallows `CleatSuspendException` | engine still sees the suspension and the workflow keeps running past it, but the host refuses every host call made after the catch (cleat#4285): none reaches the service or is recorded | warning CV101 |

`HttpClient` is absent from the probe because it would make the probe itself uninstantiable; that
is the measurement.

The probe deliberately uses every refused API, so the build gate refuses it. The test builds it
with `build.sh` and stamps it directly, and `TestCSharpVetCorpus` asserts the vet refuses it.

## What it cannot see

No type information. A name is resolved by spelling plus the file's own `using` aliases (of a type
or of a namespace) and `using static`; `nameof(...)` is blanked because it calls nothing. Invisible:
reflection, a delegate taken from a variable, an extension method wrapping a refused API, an alias
declared in another file (including a `global using` alias in a file the vet reads separately). `.Result` and `GetResult()` are not judged (on a
completed task they are ordinary and common). The effect of the thread pool was not measured.
`Compile` items in the project file are not interpreted. `#if` branches are all scanned, which
over-reports a disabled branch and never under-reports. An interpolated raw string is blanked
whole. CV101 is a warning because the same catch around code that cannot suspend is harmless.

## Known false-positive shape

A BCL-named type reached through an assembly reference rather than declared in the project (a
third-party `File`, say) is refused by spelling. Declare an alias (`using File = MyLib.File;`) to
say which one is meant.
