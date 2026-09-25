# `cleat-java` -- Java SDK for cleat durable workflows

Java SDK providing the `cleat` package with WASM ABI bindings for writing
durable workflows that compile via [TeaVM](https://teavm.org/) to WebAssembly.

## Installation

**Not published to Maven Central or any other registry** -- `com.cleat` in
`build.gradle.kts` is only this project's internal Gradle group id, there is
no `maven-publish` block, and no artifact exists under that coordinate.
Source-only: clone the repo at a release tag and include `crates/cleat-java`
as a Gradle subproject.

```bash
git clone --branch v0.3.0 --depth 1 https://github.com/cleat-team/cleat cleat-src
```

**`settings.gradle.kts`**:
```kotlin
rootProject.name = "my-workflow"

dependencyResolutionManagement {
    repositories {
        mavenCentral()
    }
}

include(":cleat-java")
project(":cleat-java").projectDir = file("cleat-src/crates/cleat-java")
```

**Your workflow module's `build.gradle.kts`**:
```kotlin
dependencies {
    annotationProcessor(project(":cleat-java"))
    implementation(project(":cleat-java"))
}
```

Before `v0.3.0` is tagged, clone `--branch develop` (or a specific commit)
instead and re-clone at the tag once it exists -- the dependency form does not
change, only the ref.

Verified 2026-09-24 outside a checkout, pinned to a `develop` commit: cloning
and wiring the subproject as above resolved `cleat-java` and ran its
`@CleatEntry` annotation processor against a workflow class in a separate
Gradle module (`gradle :workflow:compileJava` generated
`GreetingWorkflow_hello_Export.java`, `CleatEntryIndex` and `WorkflowEntry`
from the SDK's processor). For complete TeaVM build configuration to take
that module the rest of the way to `.wasm`, see the [Build setup](#build-setup)
section.

## Quick start

Define a workflow by creating a class with a public static method annotated
with `@CleatEntry`. The annotation processor generates WASM export wrappers
automatically.

```java
package com.example;

import cleat.HostCalls;
import cleat.CleatEntry;

public class GreetingWorkflow {

    @CleatEntry
    public static String hello(HostCalls hc, String name) {
        hc.cleatLog("Hello workflow started for " + name);
        CleatResult<String> response = hc.cleatCall(
            "greeter", "Greet",
            "{\"name\": \"" + name + "\"}");
        if (response.isErr()) {
            return "{\"error\": \"" + response.getError() + "\"}";
        }
        hc.cleatLog("Got response: " + response.getValue());
        return response.getValue();
    }
}
```

The `@CleatEntry` annotation triggers `CleatEntryProcessor`, which generates
WASM export wrappers conforming to the Cleat ABI
`(argsPtr, argsLen, outPtr, maxOutLen) -> i64`. During TeaVM compilation,
the Java bytecode is translated to WebAssembly. On replay, completed calls
return cached results instead of re-executing.

Compile the workflow with Gradle (see [Build setup](#build-setup) for the
`teavm { wasm { ... } }` configuration this requires):

```bash
gradle generateWasm
# Output: build/wasm/workflow.wasm
```

`gradle build` does not invoke `generateWasm` -- run the wasm task
explicitly, or add it as a dependency of `build` in your own `build.gradle`.

## HostCalls overview

The `HostCalls` class wraps all WASM host function imports, grouped by
category. Each method returns a `CleatResult<T>` that encodes success or
failure:

### Workflow Identity
- `String currentWorkflowId()` -- the current workflow's unique ID
- `String currentRunId()` -- the current run's unique ID

### Time & Random
- `long now()` -- wall-clock time in ms since epoch
- `long random()` -- deterministic random value (same on replay)
- `int version()` -- workflow definition version
- `int minVersion()` -- minimum supported version

### Durable Execution
- `CleatResult<String> cleatCall(String service, String operation, String request)` -- recorded API call
- `CleatResult<Void> cleatSleepMs(long timeoutMs)` -- suspend for a duration
- `void cleatLog(String message)` -- emit a log message
- `CleatResult<FetchResult> cleatFetch(String method, String url, String headers, String body)` -- durable HTTP fetch
- `CleatResult<Void> cleatSend(String service, String operation, String request)` -- fire-and-forget
- `CleatResult<Void> scheduleInvokeMs(String service, String operation, String request, long delayMs)` -- delayed one-shot invoke

### Signals & Events
- `CleatResult<AwaitSignalsResult> awaitSignalsMs(String[] signalNames, long timeoutMs)` -- wait for external signals
- `CleatResult<String> pollSignal(String signalName)` -- non-blocking signal check
- `CleatResult<Boolean> pollCancellation()` -- check for cancellation
- `CleatResult<Void> signalWorkflow(String targetRunId, String signalName, String payload)` -- signal another workflow

### Child Workflows
- `CleatResult<String> childWorkflow(String name, String input)` -- start child, returns run ID
- `CleatResult<String> awaitChild(String runId)` -- await a single child
- `CleatResult<String> awaitAllChildren(String[] runIds)` -- await multiple children concurrently

### State & Promises
- `void setQueryState(String key, String value)` -- set externally queryable state
- `CleatResult<String> createPromise(String name)` -- create a durable promise
- `CleatResult<AwaitPromiseResult> awaitPromiseMs(String promiseId, long timeoutMs)` -- await a promise

### Plugin Calls
- `CleatResult<String> pluginCall(String pluginName, String functionName, String input)` -- call a host plugin function

All methods are deterministic and replayed from event history on subsequent
executions. Side effects (calls, sleeps, logs) are recorded in the workflow
event history and replayed consistently.

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

The annotation processor (`CleatEntryProcessor`) must run during compilation.
It generates WASM export wrapper classes for each method annotated with
`@CleatEntry`, plus the `CleatEntryIndex` aggregator and `WorkflowEntry`
analysis root.  The processor is registered via
`META-INF/services/javax.annotation.processing.Processor`.

### Single-project setup

Since `cleat-java` is not published (see [Installation](#installation)), a
genuinely single-`settings.gradle` project cannot declare it as an ordinary
`implementation "com.cleat:cleat-java"` dependency the way the pre-2026-09-24
version of this section implied. Use a Gradle **composite build**
(`includeBuild`) instead: it substitutes `com.cleat:cleat-java` with the
git-cloned project by matching group/name, without pulling `cleat-java` into
this project's own `settings.gradle`/`include(...)` graph the way
[Multi-project setup](#multi-project-setup) does.

```bash
git clone --branch v0.3.0 --depth 1 https://github.com/cleat-team/cleat cleat-src
```

**`settings.gradle`**:
```gradle
rootProject.name = 'my-workflow'
includeBuild('cleat-src/crates/cleat-java')
```

**`build.gradle`**:
```gradle
buildscript {
    repositories { mavenCentral() }
    dependencies {
        classpath 'org.teavm:teavm-gradle-plugin:0.10.2'
    }
}

apply plugin: "java"
apply plugin: "org.teavm"

repositories { mavenCentral() }

dependencies {
    annotationProcessor "com.cleat:cleat-java"
    implementation "com.cleat:cleat-java"
    implementation "org.teavm:teavm-classlib:0.10.2"
    implementation "org.teavm:teavm-jso-apis:0.10.2"
}

teavm {
    wasm {
        mainClass = "cleat.WorkflowEntry"
        targetFileName = "workflow.wasm"
        outputDir = layout.buildDirectory
        optimization = org.teavm.gradle.api.OptimizationLevel.BALANCED
    }
}
```

The `teavm { }` extension in `org.teavm:teavm-gradle-plugin:0.10.2` has no
top-level `mainClass`/`fileName`/`targetType`/`optimizationLevel`/`obfuscated`
properties -- those were a pre-0.10 shape that no longer exists. Per-target
settings (`mainClass`, `targetFileName`, `outputDir`, `optimization`, and
others) live inside a nested `wasm { }` block (`js { }`, `wasi { }` and
`c { }` also exist, for the plugin's other targets); `optimization` takes the
`org.teavm.gradle.api.OptimizationLevel` enum (`NONE`, `BALANCED`,
`AGGRESSIVE`), not a string; there is no `obfuscated` property in this
version. Confirmed by decompiling `teavm-gradle-plugin-0.10.2.jar`'s
`org.teavm.gradle.api.TeaVM*Configuration` interfaces with `javap`.

Run `gradle generateWasm` (not `gradle build`, which does not invoke the wasm
task) -- output lands at `build/wasm/workflow.wasm`, matching the path the
Quick Start section above already documents.

### Multi-project setup

When the SDK is included as a Gradle subproject (recommended for real workflows),
use the `apply false` pattern in the root project to avoid plugin version
conflicts between the SDK's buildscript and the root project:

**Root `settings.gradle.kts`**: same form as [Installation](#installation)
above -- `projectDir` points at `crates/cleat-java` inside a checkout, cloned
at a release tag when building outside the cleat repo itself:

```kotlin
rootProject.name = "my-workflow"
include(":cleat-java")
project(":cleat-java").projectDir = file("cleat-src/crates/cleat-java")
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
    java                      // required before org.teavm can configure itself
    id("org.teavm")           // resolved from the root project
}

dependencies {
    // SDK as annotation processor (generates export wrappers):
    annotationProcessor(project(":cleat-java"))

    // SDK as compile dependency (HostCalls, Memory, etc.):
    implementation(project(":cleat-java"))
}

teavm {
    wasm {
        mainClass.set("cleat.WorkflowEntry")
        targetFileName.set("workflow.wasm")
        outputDir.set(layout.buildDirectory)
        optimization.set(org.teavm.gradle.api.OptimizationLevel.BALANCED)
    }
}
```

Without `java` applied first, `org.teavm` fails to configure with
`Configuration with name 'testImplementation' not found` -- TeaVM's plugin
wires into the Java source sets and needs them to already exist. Run `gradle
:workflow:generateWasm` (see [Build setup](#build-setup) above for why not
plain `build`).

The `cleat-java` subproject itself declares the `org.teavm` plugin and TeaVM
dependencies in its own `build.gradle`/`build.gradle.kts`.  The `apply false`
pattern ensures the plugin classpath is resolved only once, at the root level.

## Tree-shaking

TeaVM's WASM compiler performs dead-code elimination.  Generated `@Export`
classes (produced by `CleatEntryProcessor`) must be preserved -- they are
the WASM entry points.  The SDK's annotation processor generates
`cleat.generated.CleatEntryIndex` which references all generated `@Export`
classes via `Class<?>[]`.  The `cleat.WorkflowEntry` analysis root (also
generated) holds a static reference to `CleatEntryIndex.WRAPPER_CLASSES`,
keeping the entire export chain alive through the tree-shaker's reachability
analysis.

If you need to preserve additional classes manually, use the `preservedClasses`
property inside your `teavm` block's `wasm { }` target (it lives on
`TeaVMCommonConfiguration`, shared by every target, not on `teavm { }`
directly -- see [Build setup](#build-setup)):

```gradle
teavm {
    wasm {
        preservedClasses = [
            "com.example.MyWorkflow_processOrder_Export",
            "com.example.MyWorkflow_cancelOrder_Export",
        ]
    }
}
```

Note: The `preservedClasses` option is a fallback.  The auto-generated
`CleatEntryIndex`/`WorkflowEntry` mechanism handles tree-shaking
automatically for all methods annotated with `@CleatEntry`.

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
    |                              |--- GET /api/workflows/{id}/query?key=status
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
