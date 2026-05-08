# Saga Temporal Port: API Findings

## Overview

This document catalogs API mismatches, missing features, and design
differences discovered while porting the Temporal samples-go saga
(money transfer) example to cleat's Go SDK.

---

## 1. DurableDefer accepts a string description, not a closure

**Status: Design difference (may be intentional)**

The cleat `HostCalls.DurableDefer(description string)` method takes a
string description and returns a defer ID. It does **not** accept a Go
closure. This means the familiar Go pattern:

```go
defer h.DurableDefer(func() { /* compensate */ })
```

does not work. If the description-based approach maps to host-side
compensation logic that the host runtime knows about, this is correct by
design. But for workflow-defined compensation (e.g., calling
`WithdrawCompensation`), the description-only API does not carry the
actual logic.

**Workaround:** Use `durable.NewSaga()` with `AddStep()` and `Run()`,
which provides structured forward+compensate semantics. This is the
idiomatic cleat approach.

**Gap:** No closure-based `DurableDefer` means inline deferred
compensation (common in Temporal workflows) is not directly portable.

---

## 2. No DurableCallTypedWithOptions

**Status: Missing ergonomic API**

The interface has `DurableCallWithOptions` and
`DurableCallJSONWithOptions`, but no `DurableCallTypedWithOptions`.
To use retry policies with typed (struct-based) request/response calls,
you must manually marshal the request to JSON:

```go
reqJSON, _ := json.Marshal(request)
resp, err := h.DurableCallWithOptions(opts, service, operation, string(reqJSON))
```

This is less ergonomic than the equivalent Temporal pattern of
`workflow.WithActivityOptions(ctx, options)` followed by
`workflow.ExecuteActivity(ctx, fn, arg)`.

**Impact:** Forces manual JSON marshaling at every call site that needs
retry configuration. A `DurableCallTypedWithOptions(opts, service,
operation, request, result)` convenience method would close this gap.

---

## 3. DurableCallJSONWithOptions does not handle nil result

**Status: Bug / missing nil guard**

`DurableCallJSONWithOptions` calls `json.Unmarshal(resp, result)` without
checking whether `result` is nil. Unlike `DurableCallTyped` (which has
`if result == nil { return nil }`), this will return an
`InvalidUnmarshalError` for calls with no meaningful response.

**Workaround:** Use `DurableCallWithOptions` (raw string return) when
the response is not needed, as done in this port's `callWithRetry`
helper:
```go
_, err = h.DurableCallWithOptions(opts, service, operation, string(reqJSON))
```

---

## 4. Saga compensation silently ignores errors

**Status: Design difference from Temporal**

The `Saga.AddStep` compensate function signature is `func(HostCalls)` —
it returns no error. This means compensation failures are silently
dropped. The original Temporal example accumulates all compensation
errors via `multierr.Append`.

**Impact:** A failed compensation step does not propagate its error.
While this may be acceptable for fire-and-forget cleanup, the Temporal
pattern of collecting and returning all compensation errors is more
diagnosable.

---

## 5. No per-activity StartToCloseTimeout

**Status: Missing concept**

The Temporal example configures `StartToCloseTimeout: 1 minute` per
activity. Cleat has no equivalent concept at the `CallOptions` level.
Timeouts are implicit in the service/operation layer.

**Impact:** Applications that depend on per-call timeouts must rely on
the host-side service configuration rather than workflow-level options.

---

## 6. Saga error wrapping differs from Temporal

**Status: Design difference**

The Saga's `Run()` wraps forward-step errors as:

```go
fmt.Errorf("saga step %q failed: %w", step.Description, err)
```

The Temporal example returns the original error directly (after
compensation). The wrapping adds context but changes error-checking
behavior for callers that match on specific error strings.

---

## Summary

| # | Issue | Severity | Workaround |
|---|-------|----------|------------|
| 1 | `DurableDefer` takes string, not closure | Medium | Use `NewSaga()` |
| 2 | No `DurableCallTypedWithOptions` | Medium | Manual JSON marshal |
| 3 | `DurableCallJSONWithOptions` nil result | Low | Use `DurableCallWithOptions` |
| 4 | Saga compensation ignores errors | Low | Accept fire-and-forget |
| 5 | No per-call StartToCloseTimeout | Low | Host-level config |
| 6 | Saga error wrapping | Low | Match on inner error via `errors.Is` |
