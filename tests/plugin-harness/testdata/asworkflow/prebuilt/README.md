# Prebuilt AssemblyScript WASM fixture

`workflow.wasm` is a checked-in build of `../assembly/`, compiled to WASM by
`cleat build --target assemblyscript` (AssemblyScript `asc`). Two tests read it
and neither can build it, because building it needs Node and `npx`, which a
Go-only CI job does not have:

- `wasm/import_section_test.go` — parses the import section and asserts
  `DetectLanguage` says `assemblyscript`
- `cmd/cleat-worker/backend_routing_test.go`, `TestRealFixturesRouteToWasmtime`
  — asserts AssemblyScript routes to wasmtime

It lives here rather than under `../dist/`, where `asc` puts it, because
`TestPluginCalls_Wasm_AS` compiles the same workflow on every run and rewrites
everything the build owns — including the fixture itself. That left a 13 KB
binary showing as modified in `git status` after any test run.

This is the same fix, and the same reason, as `../../javaworkflow/prebuilt/`.
The two differ in one way worth knowing: the TeaVM build is **not**
reproducible, while this one is. Measured 2026-09-04, two consecutive runs of
`TestPluginCalls_Wasm_AS` produced byte-identical output
(`17cb617f1563a736…`, 13672 bytes). So for this fixture a difference between
the committed bytes and a fresh build means the fixture is stale, not that the
compiler wandered.

It was stale when it moved here: 13369 bytes committed against 13672 built, with
`asc` inside the `^0.28.19` pin (`package.json`) resolving to 0.28.20. Nothing
had noticed, because every AS test run silently refreshed it in place — which is
also why moving it without refreshing would have frozen the staleness rather
than fixed it. Both reader tests pass against either version; the refresh
changed no assertion.

## Regenerating

```bash
cd /path/to/cleat
go test ./tests/plugin-harness/ -run 'TestPluginCalls_Wasm_AS$' -count=1
cp tests/plugin-harness/testdata/asworkflow/dist/workflow.wasm \
   tests/plugin-harness/testdata/asworkflow/prebuilt/workflow.wasm
go test ./wasm/ -run TestReadImportSection_ParsesEveryImport -count=1
go test ./cmd/cleat-worker/ -run TestRealFixturesRouteToWasmtime -count=1
```

Use the full test names above. `-run` matching nothing prints `ok`, so a
mistyped pattern looks exactly like a passing test.

Regenerate when the AssemblyScript source, the `asc` version, or the host-call
ABI changes. Nothing detects staleness automatically — `dist/` is gitignored, so
a rebuild no longer shows up in `git status` at all. Compare deliberately:

```bash
shasum -a 256 tests/plugin-harness/testdata/asworkflow/{dist,prebuilt}/workflow.wasm
```
