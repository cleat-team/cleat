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
`@DurableEntry`.  The processor is registered via
`META-INF/services/javax.annotation.processing.Processor`.

### Gradle configuration

The SDK should be included as a separate Gradle subproject with the annotation
processor dependency:

```gradle
// In your workflow project's build.gradle:
dependencies {
    // SDK as annotation processor (generates export wrappers):
    annotationProcessor project(":durable-java")

    // SDK as compile dependency (HostCalls, Memory, etc.):
    implementation project(":durable-java")
}
```

The `durable-java` subproject itself applies the `org.teavm` plugin and
depends on `teavm-classlib` and `teavm-jso-apis`:

```gradle
apply plugin: "java"
apply plugin: "org.teavm"

dependencies {
    implementation "org.teavm:teavm-classlib:0.10.2"
    implementation "org.teavm:teavm-jso-apis:0.10.2"
}
```

## Tree-shaking

TeaVM's WASM compiler performs dead-code elimination.  Generated `@Export`
classes (produced by `DurableEntryProcessor`) must be preserved -- they are
the WASM entry points.  The SDK provides an auto-generated
`DurableEntryAggregator` class that references all generated `@Export`
classes, keeping them alive through the tree-shaker's reachability analysis.

If you add workflow classes after the aggregator is generated, or if you
disable the aggregator, preserve the `@Export` classes manually in
`build.gradle`:

```gradle
teavm {
    wasm {
        // Preserve generated export wrapper classes:
        preservedClasses = [
            "com.example.MyWorkflow_processOrder_Export",
            "com.example.MyWorkflow_cancelOrder_Export",
        ]
    }
}
```

## Known limitations

| Feature | Status | Workaround |
|---------|--------|------------|
| `JsonHelper.parse()` non-`String` types | Not implemented | Workflow methods should accept/return `String` (JSON) |
| `getQueryState()` | Host-side read only | Use `setQueryState()` to expose state; external clients read via host API |
| `String.replace(CharSequence, CharSequence)` | Compiles to `java.util.regex.Pattern` | Use char-by-char iteration with `StringBuilder` |
| `java.util.regex` | Unavailable | Avoid; use manual string matching |
| Dynamic class loading / reflection | Unavailable | Use concrete types; `JsonHelper` does manual field extraction |
| Concurrent collections | Unavailable | Not needed (single-threaded WASM execution) |
