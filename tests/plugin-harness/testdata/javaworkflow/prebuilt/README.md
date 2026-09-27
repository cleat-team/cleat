# Prebuilt Java WASM fixture

`workflow.wasm` is a checked-in build of `../src/main/java/com/cleat/example/`,
compiled to WASM by TeaVM. Two tests read it and neither can build it, because
building it needs a JDK, Gradle and a Gradle distribution download that a
Go-only CI job does not have:

- `wasm/import_section_test.go` — asserts the import section parses to 7
  imports, first `teavm.putwcharsOut`, including `env.plugin_call`
- `cmd/cleat-worker/backend_routing_test.go`, `TestRealFixturesRouteToWasmtime`
  — asserts `DetectLanguage` says `java` and that Java routes to wasmtime
- `wasm/a_prebuilt_exports_what_its_source_declares_test.go`,
  `TestAPrebuiltExportsWhatItsSourceDeclares` — compares this binary's export
  section against the `@CleatEntry` annotations in `../src/main/java/`

It lives here rather than under `../build/`, where Gradle put it, because
`TestPluginCalls_Wasm_Java` compiles the same workflow on every run and
rewrites everything Gradle owns. That left a 342 KB binary showing as modified
in `git status` after any test run, and its bytes are not reproducible —
successive TeaVM builds of unchanged source produce the same length and a
different hash.

## Regenerating

```bash
cd ..
./gradlew build          # writes build/wasm/wasm/workflow.wasm
cp build/wasm/wasm/workflow.wasm prebuilt/workflow.wasm
go test ./wasm/ -run TestReadImportSection_ParsesEveryImport
go test ./wasm/ -run TestAPrebuiltExportsWhatItsSourceDeclares
go test ./cmd/cleat-worker/ -run TestRealFixturesRouteToWasmtime
```

Use the full test names above. `-run` matching nothing prints `ok`, so a
mistyped pattern looks exactly like a passing test.

Regenerate when the Java source, the TeaVM version, or the host-call ABI
changes. If those tests fail after a toolchain change, rebuild this file before
assuming the parser is wrong.

**A missing entry point is now detected; nothing else is.**
`TestAPrebuiltExportsWhatItsSourceDeclares` fails if this binary lacks an
export that a `@CleatEntry` annotation declares. It needs no JDK, which is the
point — the condition under which a stale binary goes unnoticed is exactly the
condition under which no toolchain is present to rebuild it. cleat#1660.

Everything else is still undetected, and here it is undetectable by
comparison: `TestPluginCalls_Wasm_Java` builds its own copy and keeps passing
while the assertions above read a stale artifact, and unlike the
AssemblyScript fixture these bytes are **not reproducible**, so a hash
difference against a fresh build proves nothing.

Note that the export name comes from the ANNOTATION here and from the FUNCTION
NAME in AssemblyScript — `@CleatEntry(name = "CallAllPlugins")` on
`callAllPlugins` exports `CallAllPlugins`, while `@cleatEntry("CallAllPlugins")`
on `call_all_plugins` exports `call_all_plugins` and discards the string
entirely. `CleatEntryProcessor` falls back to the method name when `name()` is
empty. The two look alike and mean opposite things, so a check written for one
is wrong about the other.
