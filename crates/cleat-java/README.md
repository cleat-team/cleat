# `durable-java` -- Java SDK for cleat durable workflows

Java SDK providing the `cleat` package with WASM ABI bindings for writing
durable workflows that compile via [TeaVM](https://teavm.org/) to WebAssembly.

## TeaVM constraints

TeaVM's WASM backend translates Java bytecode to WebAssembly, but does not
support the full JVM standard library.  Key constraints:

### No `java.util.regex`

The `java.util.regex` package is not available in TeaVM's WASM target.  This
means `String.replace(CharSequence, CharSequence)` compiles to a
`java.util.regex.Pattern` under the hood and **must be avoided**.

**Safe alternatives for string manipulation:**

```java
// BAD -- compiles to Pattern (runtime error):
String result = input.replace("old", "new");

// GOOD -- manual char-by-char iteration:
StringBuilder sb = new StringBuilder(input.length());
for (int i = 0; i < input.length(); i++) {
    char c = input.charAt(i);
    if (c == 'o' && input.startsWith("old", i)) {
        sb.append("new");
        i += 2; // skip "old"
    } else {
        sb.append(c);
    }
}
```

### Limited reflection support

TeaVM does not support `java.lang.reflect`, dynamic class loading, or
proxies.  JSON serialization libraries that rely on reflection (Jackson, Gson)
will not work.  Use the built-in `JsonHelper` class for JSON I/O, which
operates on raw string values with manual parsing.

### No `java.util.concurrent`

TeaVM's WASM target is single-threaded.  Classes from `java.util.concurrent`
(thread pools, locks, atomics, concurrent collections) are either unavailable
or behave unexpectedly.  Cleat workflows are inherently single-threaded and
deterministic, so synchronization is not needed.

## Build setup

The annotation processor (`DurableEntryProcessor`) must run during compilation.
It generates WASM export wrapper classes for each method annotated with
`@DurableEntry`, plus the `DurableEntryIndex` aggregator and `WorkflowEntry`
analysis root.  The processor is registered via
`META-INF/services/javax.annotation.processing.Processor`.

### Single-project setup

```gradle
buildscript {
    repositories { mavenCentral() }
    dependencies {
        classpath 'org.teavm:teavm-gradle-plugin:0.10.2'
    }
}

apply plugin: "java"
apply plugin: "org.teavm"

dependencies {
    implementation "org.teavm:teavm-classlib:0.10.2"
    implementation "org.teavm:teavm-jso-apis:0.10.2"
}

teavm {
    mainClass = "cleat.WorkflowEntry"
    fileName = "workflow.wasm"
    outputDir = file("build/wasm")
    targetType = "WASM"
    optimizationLevel = "BALANCED"
    obfuscated = false
}
```

### Multi-project setup

When the SDK is included as a Gradle subproject (recommended for real workflows),
use the `apply false` pattern in the root project to avoid plugin version
conflicts between the SDK's buildscript and the root project:

**Root `settings.gradle.kts`**:
```kotlin
rootProject.name = "my-workflow"
include(":durable-java")
project(":durable-java").projectDir = file("path/to/cleat/crates/durable-java")
```

**Root `build.gradle.kts`** (applies `org.teavm` plugin without loading it):
```kotlin
plugins {
    id("org.teavm") version "0.10.2" apply false
}
```

**Workflow subproject `build.gradle.kts`**:
```kotlin
plugins {
    id("org.teavm")          // resolved from the root project
}

dependencies {
    // SDK as annotation processor (generates export wrappers):
    annotationProcessor(project(":durable-java"))

    // SDK as compile dependency (HostCalls, Memory, etc.):
    implementation(project(":durable-java"))
}

teavm {
    mainClass.set("cleat.WorkflowEntry")
    fileName.set("workflow.wasm")
    outputDir.set(layout.buildDirectory.dir("wasm"))
    targetType.set("WASM")
    optimizationLevel.set("BALANCED")
    obfuscated.set(false)
}
```

The `durable-java` subproject itself declares the `org.teavm` plugin and TeaVM
dependencies in its own `build.gradle`/`build.gradle.kts`.  The `apply false`
pattern ensures the plugin classpath is resolved only once, at the root level.

## Tree-shaking

TeaVM's WASM compiler performs dead-code elimination.  Generated `@Export`
classes (produced by `DurableEntryProcessor`) must be preserved -- they are
the WASM entry points.  The SDK's annotation processor generates
`cleat.generated.DurableEntryIndex` which references all generated `@Export`
classes via `Class<?>[]`.  The `cleat.WorkflowEntry` analysis root (also
generated) holds a static reference to `DurableEntryIndex.WRAPPER_CLASSES`,
keeping the entire export chain alive through the tree-shaker's reachability
analysis.

If you need to preserve additional classes manually, use the `preservedClasses`
property in your `teavm` block:

```gradle
teavm {
    preservedClasses = [
        "com.example.MyWorkflow_processOrder_Export",
        "com.example.MyWorkflow_cancelOrder_Export",
    ]
}
```

Note: The `preservedClasses` option is a fallback.  The auto-generated
`DurableEntryIndex`/`WorkflowEntry` mechanism handles tree-shaking
automatically for all methods annotated with `@DurableEntry`.

## Detailed constraint notes

### String.replace(CharSequence, CharSequence) compiles to Pattern

TeaVM's WASM backend maps `String.replace(CharSequence, CharSequence)` to
`java.util.regex.Pattern`, which is **not available** on the WASM target.

This means:

```java
// BAD -- will fail at runtime with NoClassDefFoundError for Pattern:
String result = input.replace("old", "new");

// BAD -- replaceAll and replaceFirst also use Pattern:
String result = input.replaceAll("[a-z]", "x");
String result = input.replaceFirst("foo", "bar");
```

**Safe workaround** -- character-by-character iteration with `StringBuilder`:

```java
public static String replaceLiteral(String input, String from, String to) {
    if (from == null || from.isEmpty()) {
        return input;
    }
    StringBuilder sb = new StringBuilder(input.length());
    for (int i = 0; i < input.length(); i++) {
        if (input.startsWith(from, i)) {
            sb.append(to);
            i += from.length() - 1;
        } else {
            sb.append(input.charAt(i));
        }
    }
    return sb.toString();
}
```

This is a **fundamental WASM target limitation**: TeaVM's WASM backend does
not include the `java.util.regex` library classes. Any code that transitively
depends on regex will fail at runtime. Always verify string-manipulation code
compiles under TeaVM with `./gradlew classes`.

### getQueryState() is a host-side read

The `getQueryState()` method in `HostCalls` retrieves state that was
previously set via `setQueryState()`. However, this state is stored on the
**host side**, not in workflow-local memory:

- **`setQueryState(key, value)`**: Writes a key-value pair to the host's
  queryable state store. External clients can read this state via the Cleat
  REST API while the workflow is running or after completion.

- **`getQueryState(key)`**: Reads a value that was previously written by
  `setQueryState()` in this or a prior execution. This is NOT a
  workflow-internal state operation -- it makes a host call to retrieve the
  stored value.

For workflow-internal (deterministic) state that is replayed from history,
use the workflow function's local variables or design the workflow so that
intermediate results are passed forward through function composition rather
than stored in query state.

**Host-side state** (queryable by external clients):
```
workflow                         host                          external client
    |                              |                                |
    |--- setQueryState("status") ->|                                |
    |                              |                                |
    |--- getQueryState("status") ->|                                |
    |<- {"active"} ---------------|                                |
    |                              |--- GET /api/workflows/{id}/state/status
    |                              |<- {"active"}                  |
```

## Known limitations

| Feature | Status | Workaround |
|---------|--------|------------|
| `JsonHelper.parse()` non-`String` types | Supported | Handles Integer, Long, Double, Boolean, Map, List, Object, and POJOs |
| `getQueryState()` | Host-side read only | See [Detailed constraint notes](#detailed-constraint-notes) above |
| `String.replace(CharSequence, CharSequence)` | Compiles to `java.util.regex.Pattern` | See [Detailed constraint notes](#detailed-constraint-notes) above |
| `java.util.regex` | Unavailable | Avoid; use manual string matching |
| Dynamic class loading / reflection | Unavailable | Use concrete types; `JsonHelper` does manual field extraction |
| Concurrent collections | Unavailable | Not needed (single-threaded WASM execution) |
