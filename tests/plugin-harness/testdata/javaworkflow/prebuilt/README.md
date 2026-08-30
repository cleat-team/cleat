# Prebuilt Java WASM fixture

`workflow.wasm` is a checked-in build of `../src/main/java/com/cleat/example/`,
compiled to WASM by TeaVM. Two tests read it and neither can build it, because
building it needs a JDK, Gradle and a Gradle distribution download that a
Go-only CI job does not have:

- `wasm/import_section_test.go` — asserts the import section parses to 7
  imports, first `teavm.putwcharsOut`, including `env.plugin_call`
- `cmd/cleat-worker/backend_routing_test.go`, `TestRealFixturesRouteToWasmtime`
  — asserts `DetectLanguage` says `java` and that Java routes to wasmtime

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
go test ./cmd/cleat-worker/ -run TestRealFixturesRouteToWasmtime
```

Use the full test names above. `-run` matching nothing prints `ok`, so a
mistyped pattern looks exactly like a passing test.

Regenerate when the Java source, the TeaVM version, or the host-call ABI
changes. Nothing detects staleness automatically: `TestPluginCalls_Wasm_Java`
builds its own copy and would keep passing while the assertions above tested a
stale artifact. If those two tests fail after a toolchain change, rebuild this
file before assuming the parser is wrong.
