# Saga Java Port

A Java port of the Temporal `samples-go` money transfer saga, validating completeness of cleat's Java SDK for saga patterns.

## What it shows

- `@CleatEntry` annotation on multiple methods for WASM export
- Saga compensation pattern: withdraw from source, deposit to destination, reverse on failure
- Manual JSON construction without a JSON library (TeaVM constraints)
- `setQueryState` / `pollCancellation` integration
- `cleatDefer` for deferred cleanup registration
- TeaVM stress testing: `StringBuilder`, char iteration, annotation processing
- Double-failure detection and inconsistent-state reporting

## Build

```bash
cd examples/saga-java-port
./gradlew build
```

## Run

```bash
cleat deploy saga-java-port build/wasm/workflow.wasm
cleat run transfer_money '{"from":"account-a","to":"account-b","amount":100,"currency":"USD"}'
```

## Key files

- `src/main/java/com/cleat/saga/MoneyTransfer.java` — saga entry point (`transferMoney`, `getTransferStatus`)
- `build.gradle` — Java + TeaVM build configuration
- `settings.gradle.kts` — project settings with `cleat-java` SDK dependency
- `ISSUES.md` — documented TeaVM and SDK compatibility issues
