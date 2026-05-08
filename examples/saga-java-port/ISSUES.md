# Saga Java Port -- Stress Test Issues

## Summary

**Status:** SUCCESSFUL COMPILATION (with fixes and workarounds)

The money transfer saga written in cleat's Java SDK compiles successfully to
WASM via TeaVM 0.10.2. Both saga entry points (`transfer_money` and
`get_transfer_status`) are properly exported in the resulting WASM binary.

**WASM binary:** `build/wasm/wasm/workflow.wasm` (692 KB, BALANCED optimization)

---

## Issues Found

### 1. TeaVM Gradle Plugin Resolution

**Severity:** BLOCKER (pre-existing SDK build issue)

The Gradle plugin `id("org.teavm") version "0.10.2"` cannot be resolved from
the Gradle Plugin Portal using the `plugins {}` DSL block. The build fails
with "Unresolved reference: mainClass" before any compilation starts.

**Error:**
```
e: file:///.../build.gradle.kts:24:5: Unresolved reference: mainClass
```

**Root cause:** The TeaVM Gradle plugin is published to Maven Central (artifact
`org.teavm:teavm-gradle-plugin`) but lacks the Gradle Plugin Portal marker
artifact or the version resolution fails in this environment.

**Workaround:** Use `buildscript` classpath resolution:
```groovy
buildscript {
    repositories { mavenCentral() }
    dependencies { classpath "org.teavm:teavm-gradle-plugin:0.10.2" }
}
apply plugin: "org.teavm"
```

This applies to BOTH the SDK (`crates/durable-java/`) and any example project
that uses the TeaVM plugin.

---

### 2. TeaVM Plugin API Mismatch

**Severity:** BLOCKER (pre-existing SDK build issue)

The existing `build.gradle.kts` files use properties that do not exist in the
TeaVM Gradle plugin v0.10.2:

| Property used in build.gradle.kts | Actual property in 0.10.2     |
|----------------------------------|-------------------------------|
| `mainClass`                      | `wasm { mainClass }`          |
| `targetFileName`                 | `wasm { targetFileName }`     |
| `targetDirectory`                | `wasm { outputDir }`          |
| `targetType = "WASM"`            | Nested inside `wasm { }`      |
| `optimizationLevel = "FULL"`     | `wasm { optimization }`       |
| `debugInformationGenerated`      | `wasm { debugInformation }`   |
| `sourceMapsGenerated`            | Does not exist                |
| `minifying`                      | Does not exist                |

**Correct API:**
```groovy
teavm {
    wasm {
        mainClass = "com.example.MyClass"
        targetFileName = "workflow.wasm"
        outputDir = file("build/wasm")
        optimization = org.teavm.gradle.api.OptimizationLevel.BALANCED
    }
}
```

The optimization level enum constants changed from string values like `"FULL"`
to enum values: `NONE`, `BALANCED`, `AGGRESSIVE`.

---

### 3. Java Unicode Escape in Comments (Pre-existing SDK Bug)

**Severity:** BLOCKER

Both the SDK source (`JsonHelper.java:129`) and the saga example contained
comments with literal `\uXXXX` text, which the Java compiler treats as Unicode
escape sequences even inside `//` line comments:

```java
// Control characters: encode as \uXXXX.   // <-- COMPILER ERROR
```

**Error:**
```
error: illegal unicode escape
```

**Fix:** Escape the backslash in the comment: `\\uXXXX`.

This is a well-known Java gotcha (`
` is a newline, `\uXXXX` is always
processed by the lexer before comment parsing). Both the SDK and the saga
example need this fix.

---

### 4. `Memory.writeByte()` Uses Wrong TeaVM Address Method (Pre-existing SDK Bug)

**Severity:** BLOCKER

In `Memory.java:79`, the method `Address.setByte(byte)` is called but
TeaVM 0.10.2's `Address` class uses `putByte(byte)` instead:

```java
// Wrong -- fails compilation:
Address.fromInt(address).setByte(value);

// Correct for TeaVM 0.10.2:
Address.fromInt(address).putByte(value);
```

The `Address` class in TeaVM 0.10.2 has `getByte()` and `putByte()`, not
`setByte()`. This suggests the SDK was written against a different TeaVM
version or uses a method name from memory.

---

### 5. Missing `WorkflowEntry` Main Class (Pre-existing SDK Issue)

**Severity:** BLOCKER (when running TeaVM on SDK)

The SDK build file (`crates/durable-java/build.gradle.kts`) sets:
```kotlin
mainClass = "cleat.WorkflowEntry"
```

But no such class exists in the SDK. When TeaVM tries to build the SDK
project, it fails with:
```
There's no main class: 'cleat.WorkflowEntry'
```

This is not a problem when the SDK is used as a library dependency (the saga
project sets its own `mainClass`), but it blocks standalone SDK builds or
WASM generation for the SDK itself.

---

### 6. `@DurableEntry` Export Wrappers Tree-Shaken by TeaVM

**Severity:** MAJOR

The annotation processor (`DurableEntryProcessor`) generates `*_Export`
classes with `@Export(name = "...")` annotations. However, TeaVM's
tree-shaking (dead code elimination) removes these generated classes because
they are not reachable from the `mainClass`.

**Effect:** The WASM binary has no `transfer_money` or `get_transfer_status`
exports. Only `start`, `_start`, `memory`, and internal `teavm_*` exports
appear.

**Workaround:** Explicitly list generated export wrapper classes in
`preservedClasses`:
```groovy
teavm {
    wasm {
        preservedClasses = [
            "com.cleat.saga.MoneyTransfer_transferMoney_Export",
            "com.cleat.saga.MoneyTransfer_getTransferStatus_Export"
        ]
    }
}
```

This is a significant DX issue. Every `@DurableEntry` method generates an
export wrapper whose class name must be manually listed. For projects with
many entry points, this is cumbersome and error-prone.

**Possible fix:** Either:
- Make the annotation processor generate an aggregator class that references
  all export wrappers
- Or modify `DurableEntryProcessor` to generate the TeaVM `preservedClasses`
  list as a build artifact
- Or use TeaVM's `@Export` reachability differently

---

### 7. No Built-in Saga Abstraction in the Java SDK

**Severity:** MINOR (expected)

Unlike the Go SDK (which provides `durable.NewSaga()`, `saga.AddStep()` with
forward/compensate closures, and `saga.AddParallel()`), the Java SDK has no
saga abstraction. Compensation logic must be written manually with try-catch
or if-else patterns.

In the saga example, compensation is implemented as:
```java
DurableResult<String> depositResult = h.durableCall("accounts", "Deposit", req);
if (depositResult.isErr()) {
    // Manual compensation
    h.durableCall("accounts", "Deposit", compensateReq);
    return errorJson("deposit failed, compensated");
}
```

This is straightforward but verbose. A future `SagaBuilder` or `SagaStep`
abstraction would improve developer experience.

---

### 8. `JsonHelper.parse()` Only Supports String.class

**Severity:** MINOR (known limitation)

The `JsonHelper.parse()` method throws `UnsupportedOperationException` for
any type other than `String.class`:

```java
if (type == String.class) {
    return (T) json;
}
throw new UnsupportedOperationException(
    "JSON parsing for " + type.getName() + " not yet implemented.");
```

Workflow input parameters must be declared as `String` (JSON strings). The
saga example works around this by doing manual JSON field extraction using
character-by-character parsing (no regex, no reflection).

---

### 9. No `getQueryState()` in SDK

**Severity:** MINOR

The SDK provides `setQueryState(String key, String value)` for writing
queryable workflow state, but there is no corresponding `getQueryState()`
method. Queryable state is host-side only -- once written, it cannot be read
back from within the same workflow.

The saga example's `getTransferStatus` entry point cannot actually return the
current workflow state; it only returns a static response noting that state
is host-side.

---

### 10. Gradle Multi-Project Plugin Version Conflict

**Severity:** MODERATE

When the root project applies the TeaVM plugin via `buildscript` + `apply`
and a subproject uses `plugins { id("org.teavm") version "0.10.2" }`,
Gradle reports:

```
The request for this plugin could not be satisfied because the plugin is
already on the classpath with an unknown version, so compatibility cannot
be checked.
```

**Resolution:** All projects must use a consistent plugin application
approach. The subproject (`:durable-java`) must use `apply plugin` instead of
`plugins {}` with a version when included in a composite build that already
loads the plugin via `buildscript`.

---

### 11. Annotation Processing Requires Separate Project

**Severity:** MODERATE (build configuration)

When the SDK sources are included directly via `srcDirs` (in-tree approach),
the annotation processor (`DurableEntryProcessor`) is compiled alongside the
annotated code. This prevents Gradle from running the annotation processor
during compilation because it's not yet compiled when needed.

**Resolution:** The SDK must be a separate subproject with `annotationProcessor`
configuration:
```groovy
dependencies {
    implementation project(":durable-java")
    annotationProcessor project(":durable-java")
}
```

---

### 12. `durableDefer` Takes Only a String Description, Not Executable Code

**Severity:** MINOR (design note)

The `durableDefer(String description)` method registers a deferred cleanup
with the host, but the description is just a string -- no Java code is
associated with the deferral. This differs from Go's defer or Java's
try/finally pattern where cleanup logic is colocated with the code that
needs cleanup.

In the saga example, `durableDefer` is called to register a description but
actual compensation logic is implemented separately in if-else blocks.

---

### 13. String.replace() Usage Requires Care with TeaVM

**Severity:** MINOR (avoided in this example)

In the existing `PlaceOrder.java` example, `String.replace(CharSequence,
CharSequence)` is used for JSON escaping. In standard Java, this method
compiles to `Pattern.compile()` which requires `java.util.regex.Pattern`.
TeaVM's WASM target has limited/no support for `java.util.regex`.

The saga example avoids this entirely by using char-by-char iteration for
JSON escaping, which exercises only basic `StringBuilder` and `char`
operations. This pattern is recommended for TeaVM compatibility.

---

## Summary Table

| # | Issue | Category | Severity | Requires SDK Fix? |
|---|-------|----------|----------|-------------------|
| 1 | TeaVM Gradle plugin resolution | Build system | BLOCKER | Yes |
| 2 | TeaVM plugin API mismatch | Build system | BLOCKER | Yes |
| 3 | Unicode escape in comments | SDK bug | BLOCKER | Yes |
| 4 | `setByte` vs `putByte` in Address API | SDK bug | BLOCKER | Yes |
| 5 | Missing `WorkflowEntry` main class | SDK bug | BLOCKER | Yes |
| 6 | Export wrappers tree-shaken by TeaVM | SDK design | MAJOR | Yes |
| 7 | No saga abstraction | SDK feature | MINOR | Wishlist |
| 8 | JsonHelper limited to String | SDK feature | MINOR | Known |
| 9 | No getQueryState() | SDK feature | MINOR | Wishlist |
| 10 | Multi-project plugin conflict | Build system | MODERATE | Yes |
| 11 | Annotation processing setup | Build system | MODERATE | Document |
| 12 | durableDefer is description-only | SDK feature | MINOR | Design |
| 13 | String.replace() uses Pattern | TeaVM limitation | MINOR | Document |

## Conclusion

**The saga example compiles and produces a valid WASM binary.** However, 6
pre-existing issues in the SDK/build system had to be fixed first. The most
significant finding is Issue #6: TeaVM tree-shakes the generated `@Export`
wrappers, requiring manual class preservation. This is a fundamental DX gap
in the annotation-processor-based approach to cleat workflow exports.
