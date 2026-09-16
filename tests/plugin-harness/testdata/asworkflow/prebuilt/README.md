# Prebuilt AssemblyScript WASM fixture

`workflow.wasm` is a checked-in build of `../assembly/`, compiled to WASM by
`cleat build --target assemblyscript` (AssemblyScript `asc`). Two tests read it
and neither can build it, because building it needs Node and `npx`, which a
Go-only CI job does not have:

- `wasm/import_section_test.go` — parses the import section and asserts
  `DetectLanguage` says `assemblyscript`
- `cmd/cleat-worker/backend_routing_test.go`, `TestRealFixturesRouteToWasmtime`
  — asserts AssemblyScript routes to wasmtime
- `wasm/a_prebuilt_exports_what_its_source_declares_test.go`,
  `TestAPrebuiltExportsWhatItsSourceDeclares` — compares this binary's export
  section against the `@cleatEntry` declarations in `../assembly/`. This is the
  one that goes red when the fixture is stale; see below.

It lives here rather than under `../dist/`, where `asc` puts it, because
`TestPluginCalls_Wasm_AS` compiles the same workflow on every run and rewrites
everything the build owns — including the fixture itself. That left a 13 KB
binary showing as modified in `git status` after any test run.

This is the same fix, and the same reason, as `../../javaworkflow/prebuilt/`.
The two differ in one way worth knowing: the TeaVM build is **not**
reproducible, while this one is. Two consecutive runs of
`TestPluginCalls_Wasm_AS` produce byte-identical output — measured 2026-09-04
and re-measured 2026-09-16 under `asc` 0.28.20. So for this fixture a
difference between the committed bytes and a fresh build means the fixture is
stale, not that the compiler wandered. Re-derive rather than trusting a hash
written down here, which is stale the next time anything in
`packages/cleat-as/` changes:

```bash
cd tests/plugin-harness/testdata/asworkflow
go test ../.. -run 'TestPluginCalls_Wasm_AS$' -count=1 && shasum -a 256 dist/workflow.wasm
go test ../.. -run 'TestPluginCalls_Wasm_AS$' -count=1 && shasum -a 256 dist/workflow.wasm
```

**Copy from `dist/`, not from `cleat build -o <dir>`.** They are different
files. `cleat build` runs `asc` into `dist/` and then stamps a `cleat.metadata`
custom section onto its own output, so the `-o` artifact is ~140 bytes larger
and carries a section the committed fixture does not have. The fixture is the
raw `asc` output, which is why `DetectLanguage` reaches it through the import
heuristic rather than through declared metadata.

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
go test ./wasm/ -run TestAPrebuiltExportsWhatItsSourceDeclares -count=1
go test ./cmd/cleat-worker/ -run TestRealFixturesRouteToWasmtime -count=1
```

Use the full test names above. `-run` matching nothing prints `ok`, so a
mistyped pattern looks exactly like a passing test.

Regenerate when the AssemblyScript source, the `asc` version, or the host-call
ABI changes.

**One kind of staleness is now detected and the rest is not**, and the
difference matters. `TestAPrebuiltExportsWhatItsSourceDeclares` fails if this
binary is missing an entry point `../assembly/` declares, so an entry point
added and never compiled in cannot ship silently again — that is cleat#1660,
where `bind_multiple_params` was declared on 2026-09-09 against a binary built
on 2026-09-04 and nothing noticed for five days.

It compares the **export set**, so it says nothing about a change that does not
add or remove an entry point. The same five-day gap also left this fixture
built against an SDK **13 commits** behind, and the only symptom of that was a
fourth host-call import (`cleat_defer_phase`, #1240) appearing on rebuild — an
export-set check cannot see that, and `dist/` is gitignored so a rebuild does
not show in `git status` either. For a body change, compare deliberately:

```bash
shasum -a 256 tests/plugin-harness/testdata/asworkflow/{dist,prebuilt}/workflow.wasm
```
