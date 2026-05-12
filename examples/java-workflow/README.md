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
cd examples/java-workflow
./gradlew build
```

## Run

```bash
cleat deploy java-workflow build/wasm/workflow.wasm
cleat run place_order '{"product":"widget","quantity":2}'
```

## Key files

- `src/main/java/com/cleat/example/PlaceOrder.java` — workflow entry points (`placeOrder`, `cancelOrder`)
- `src/main/java/com/cleat/example/WorkflowEntry.java` — generated TeaVM entry point
- `build.gradle.kts` — TeaVM build configuration
- `settings.gradle.kts` — project settings with `cleat-java` SDK dependency
