# Java Workflow

Demonstrates a durable order processing workflow written in Java and compiled to WASM via TeaVM.

## What it shows

- `@CleatEntry` annotation for marking WASM-exported workflow entry points
- Saga-like compensation: reserve inventory, charge payment, create shipment, notify
- `HostCalls.cleatCall` for durable service invocations
- `HostCalls.pollCancellation` for cancellation-aware workflows
- TeaVM Gradle plugin (`org.teavm`) with WASM target
- Java 11 source/target compatibility under WASM constraints

## Build

```bash
cleat build --target java -o /tmp/out ./examples/java-workflow/
```

`./gradlew build` alone is not enough to produce a `.wasm` -- it compiles the
Java sources but never runs the `generateWasm` task, which `cleat build`
invokes directly. Use `./gradlew build` only to check that the project
compiles; use `cleat build` to actually produce the artifact.

## Run

```bash
cleat run --wasm /tmp/out/java_workflow.wasm --entry-point place_order \
  --input '{"product":"widget","quantity":2}'
```

`cleat run` executes the module standalone against its built-in mock hosts
(each `HostCalls.cleatCall` echoes a synthetic response) and prints the
result -- no `cleat deploy` or database required. Verified output:

```
Result: "{\"status\":\"shipped\"}"
```

## Key files

- `src/main/java/com/cleat/example/PlaceOrder.java` — workflow entry points (`placeOrder`, `cancelOrder`)
- `src/main/java/com/cleat/example/WorkflowEntry.java` — generated TeaVM entry point
- `build.gradle.kts` — TeaVM build configuration
- `settings.gradle.kts` — project settings with `cleat-java` SDK dependency
