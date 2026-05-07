# Issues Found During Payment State Machine Port

## Environment: No JDK Available for Build Verification

The environment has no JDK installed (`java: command not found`).  Attempting to
run the Gradle wrapper produces:

```
ERROR: JAVA_HOME is not set and no 'java' command could be found in your PATH.
```

The ported source code was written to be compatible with the Cleat Java SDK
(Java 11 source level, TeaVM 0.10.2), but a full build verification with the
WASM TeaVM target could not be completed.  The issues below are based on code
analysis of the SDK.

---

## Gap 1: No Virtual Object / Key-scoped State API

**Severity: BLOCKING for stateful workflows**

The Cleat Java SDK has no equivalent of Restate's `VirtualObject` or its
per-key scoped state (`ObjectContext.get(StateKey)`, `ObjectContext.set(StateKey, val)`,
`ctx.key()`).

**Workaround used:** Query state (`setQueryState`/`getQueryState`) with manual
key prefixing: `payment:<paymentId>:status`.  This works but:
- Lacks per-key serialisation guarantees (no equivalent of Virtual Object
  single-threaded execution per key).
- No CAS (compare-and-swap) for the race between `makePayment` and
  `cancelPayment` acting on the same payment ID.
- The `setScope`/`getScope`/`clearScope` methods exist on `HostCalls` but are
  unused by `setQueryState`/`getQueryState`.

**Recommendation:** Either (a) add a `[gs]etState` host function pair to the
ABI that carries journaling/replay semantics separate from query state, or (b)
wire the existing `_scopePrefix` into the query state key construction so
callers get automatic scoping.

---

## Gap 2: No Delayed Message / Timer API

**Severity: FUNCTIONAL (pattern requires it)**

The Restate example uses `PaymentProcessorClient.send().expire(Duration.ofDays(1))`
to schedule a delayed invocation of the `expire` handler.  Cleat has no
equivalent "send delayed message to self" API.

**Options considered:**
1. `durableSleep` -- should work in principle but the export wrapper does not
   propagate the suspension sentinel correctly (see Gap 6).
2. External scheduler -- the caller must invoke `expirePayment` after the
   timeout.  This is what the port currently documents.
3. `signalWorkflow` + `durableSleep` -- complex and still needs suspension
   support.

**Recommendation:** Add a `durable_send_delayed` host function that schedules
a future WASM export invocation, matching the Restate delayed message pattern.

---

## Gap 3: Annotation Processor Hardcodes `inputJSON` Variable Name

**Severity: BUG (compilation failure with certain parameter names)**

The `DurableEntryProcessor` generates code that hardcodes the variable name
`inputJSON` for the memory read buffer:

```java
out.println("            String inputJSON = Memory.readString(argsPtr, argsLen);");
...
out.println("            " + inputType + " " + paramName + " = JsonHelper.parse(inputJSON, ...);");
```

If a user names their workflow parameter `inputJSON`, the generated code
declares `String inputJSON` twice, causing a compilation error.  The existing
example (`PlaceOrder`) names its parameter `input`, avoiding the collision.

**Workaround applied:** All parameters in this port are named `rawInput`.

**Fix:** Use a non-colliding local variable name in the generated code, e.g.
`__cleat_input` or `_inputBuf`.

---

## Gap 4: No `TerminalError` / Non-Retryable Exception

**Severity: DESIGN**

Restate provides `TerminalException` for errors that should not be retried.
Cleat has no equivalent.  All exceptions caught by the generated export wrapper
produce error JSON with `errCode = 1`, which the host may retry.  The port
returns error result JSON strings instead of throwing, but this changes the
error model.

---

## Gap 5: JSON Support Limitations

**Severity: MODERATE**

The `JsonHelper` class provides a no-dependency JSON parser for TeaVM WASM.
While functional, it has limitations versus Restate's Jackson-based
serialisation:

- POJO deserialisation uses `Class.getField()` -- requires **public fields**.
  Restate uses constructor injection with Jackson parameter-names module.
- No support for `java.time` types, enums as strings, or nested generics.
- No Jackson-style annotations (`@JsonProperty`, `@JsonIgnore`).
- `JsonHelper.stringify` does not handle all Java types (e.g., arrays, custom
  objects without `toString()`).

**Workaround:** All POJOs use public fields.  Enum values are stored as strings.

---

## Gap 6: Suspension Sentinel Not Propagated by Generated Export Wrapper

**Severity: RUNTIME (if `durableSleep` used)**

When `durableSleep` returns `true` (workflow should suspend), the caller should
return `Memory.SUSPEND_SENTINEL` from the WASM export so the host suspends and
re-invokes after the timer.  The generated export wrapper always serialises the
return value to JSON and calls `Memory.encodeExportResult(0, written)`.  It
never checks for suspension.

Because of this, the port avoids `durableSleep` for the expiry timer and
instead documents that expiry must be triggered externally.

---

## Gap 7: String Operations Restricted Under TeaVM WASM

**Severity: MODERATE**

TeaVM's WASM target does not support `java.util.regex.Pattern` or
`String.replaceAll`/`String.split` with regex arguments.  The Cleat SDK
correctly avoids regex throughout (`JsonHelper`, `HostCalls`, `Plugins`).
This port also avoids regex.

However, this means any user code that uses regex will fail at the WASM
compilation step.  Common patterns like `input.split(",")` or
`s.replaceAll("foo", "bar")` will throw `UnsupportedOperationException`.

**Mitigation:** Use `String.indexOf`/`substring` with manual loops.

---

## Gap 8: `build.gradle.kts` vs `build.gradle` Conflict in Subproject

**SeverITY: BUILD (possible)**

The `durable-java` subproject contains both `build.gradle.kts` and
`build.gradle`.  When included from a Kotlin DSL root project, Gradle prefers
the `.kts` file, which has its own `buildscript {}` block for the TeaVM
plugin.  This may duplicate classpath resolution or conflict with the root
project's TeaVM plugin configuration.  The Groovy `build.gradle` relies on
the root project's buildscript but may be ignored.

**Status:** Unknown -- no JDK to test.

---

## Gap 9: No TeaVM `setByte` / `putByte` Compatibility Check

**SeverITY: RUNTIME**

The `Memory` class in the SDK uses `Address.fromInt(addr).putByte(value)` for
byte writes.  This matches the `putByte` API in TeaVM 0.10.2 (the fix applied
in Round 1 for the `setByte` to `putByte` rename).  The generated code should
compile correctly, but this was not verified due to the missing JDK.

---

## Summary

| # | Issue | Severity | Status |
|---|-------|----------|--------|
| 1 | No Virtual Object / key-scoped state | BLOCKER | Workaround in port |
| 2 | No delayed message / timer API | FUNCTIONAL | Workaround in port |
| 3 | `inputJSON` variable name collision | BUG | Workaround in port |
| 4 | No `TerminalError` | DESIGN | Workaround in port |
| 5 | JSON limitations | MODERATE | Workaround in port |
| 6 | Suspension sentinel not propagated | RUNTIME | Documented, avoided |
| 7 | String/regex restrictions | MODERATE | Avoided in port |
| 8 | Dual build script conflict | BUILD | Untestable |
| 9 | `putByte` API compatibility | RUNTIME | Untestable |
