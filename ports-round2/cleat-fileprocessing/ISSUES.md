# Issues Encountered Porting Temporal fileprocessing to Cleat

## Issue 1: No Session / Worker Affinity (Gap)

**Temporal feature**: `workflow.CreateSession` / `workflow.CompleteSession` ensures all activities within a session run on the same worker process, which is essential when activities share local filesystem state.

**Cleat status**: No equivalent API exists. The Cleat SDK has no concept of sessions or worker affinity.

**Impact**: The fileprocessing sample depends on sessions because each step writes to a temp file on the worker's local filesystem, and the next step must run on the same worker to access it. Without sessions, this pattern cannot be directly reproduced.

**Workaround in this port**: The file operations are moved to host-side services that manage their own file lifecycle. Since the Cleat model treats the workflow as a single unit of execution, the pipeline steps execute sequentially through `DurableCall` to the same host. For the embedded runner approach, all steps naturally run in the same process. For distributed deployments, shared storage (NFS, S3, etc.) would be needed instead of local temp files.

---

## Issue 2: No Activity-Side Heartbeat API (Different Model)

**Temporal feature**: `activity.RecordHeartbeat(ctx, progress)` is called from within an activity to report progress and detect stalled activities via `HeartbeatTimeout`.

**Cleat status**: Cleat has `DurableCallWithHeartbeat` which is a workflow-side API. The workflow specifies a heartbeat interval and receives an `onProgress` callback. There is no activity-side `RecordHeartbeat` equivalent.

**Impact**: The model is inverted. In Temporal, the activity pushes heartbeats; in Cleat, the workflow pulls/receives them. The `HeartbeatTimeout` mechanism (detecting stalled activities) does not exist.

**Workaround**: The `onProgress` callback in `DurableCallWithHeartbeat` allows the workflow to observe progress. For timeout detection, the workflow could combine this with `DurableSleep` and manual timeouts, but there is no built-in silent-activity detection.

---

## Issue 3: No StartToCloseTimeout / HeartbeatTimeout Equivalent

**Temporal feature**: `ActivityOptions.StartToCloseTimeout` limits the maximum duration of an activity. `HeartbeatTimeout` detects when an activity has stopped reporting progress.

**Cleat status**: `CallOptions.Timeout` exists and serves a similar purpose to `StartToCloseTimeout`. There is no heartbeat timeout.

**Impact**: Partial parity. The timeout field works, but there is no mechanism to detect a long-running operation that has become unresponsive but hasn't technically timed out.

---

## Issue 4: No mock.Anything / testify/mock Integration

**Temporal feature**: `env.OnActivity(a.DownloadFileActivity, mock.Anything, "file1")` uses `testify/mock` for flexible argument matching.

**Cleat status**: `durabletest.TestEnv.OnCall` supports nil matchers (match any), string matchers (exact match), and function matchers (`func(string) bool`). There is no `mock.Anything` constant.

**Impact**: Trivial. Nil matchers provide the same behaviour as `mock.Anything`. Function matchers give more control when needed.

---

## Issue 5: No Built-in Test Suite / Suite Runner

**Temporal feature**: `testsuite.WorkflowTestSuite` integrated with `testify/suite` provides `SetupTest` / `TearDownTest` and a structured suite runner.

**Cleat status**: `durabletest.TestEnv` is a standalone test helper. It does not integrate with `testify/suite`. There is no built-in suite runner.

**Impact**: Low. Standard Go subtests and `testing.T` cover the same ground, just without the suite ceremony.

---

## Issue 6: Call Stubs Are Consumed on Use

**Temporal feature**: Activity mock expectations remain valid across multiple calls (e.g., for retries) unless explicitly cleared.

**Cleat status**: `OnCall` stubs are consumed (removed from the list) after one match. If a call retries, a new stub must be registered for each retry attempt.

**Impact**: Retry tests need explicit stub registration for each attempt. For workflows with dynamic retry counts, this makes tests brittle. A `Persistent()` or `Times(n)` modifier would improve the API.

---

## Issue 7: No Worker Registration Model

**Temporal feature**: Workers register workflows and activities via `w.RegisterWorkflow(wf)` / `w.RegisterActivity(a)`. Workers are long-lived processes.

**Cleat status**: No worker concept. Workflows are either compiled to WASM and served by a runtime host, or run via the embedded runner (`embedded.Runner`). Activities are replaced by host-side services.

**Impact**: The deployment model is fundamentally different. The Temporal starter/worker/client split does not apply. The embedded runner provides the closest equivalent for local development.

---

## Issue 8: WASM Sandbox Prevents Direct Filesystem Access

**Temporal feature**: Activities are native Go functions running on the worker with full filesystem access.

**Cleat status**: Workflows run inside a WASM sandbox with no filesystem access. All I/O must go through host calls (`DurableCall`).

**Impact**: File processing patterns must be re-architected. File operations (read, write, temp file creation) cannot live in the workflow. They must be extracted to host-side services. This is not a bug but a fundamental architectural difference that affects all file-processing workflows.

---

## Issue 9: go.mod Requires replace Directive

**Temporal feature**: SDK is published to a Go module registry (`go.temporal.io/sdk`).

**Cleat status**: The SDK (`github.com/rcownie/durable`) is not published to any registry. Any module importing it must use a `replace` directive pointing to a local checkout.

**Impact**: Non-trivial for CI/CD. Every environment that builds the workflow must have the SDK source at the expected path. There is no way to pin a version or use normal Go module versioning.

---

## Summary

| # | Issue | Severity | Workaround Available? |
|---|-------|----------|----------------------|
| 1 | Session / worker affinity | High | Partial (host services + shared storage) |
| 2 | Activity-side heartbeat | Medium | Partial (workflow-side heartbeat callback) |
| 3 | Heartbeat timeout | Medium | No (manual timeout logic needed) |
| 4 | mock.Anything integration | Low | Yes (nil matcher) |
| 5 | Test suite runner | Low | Yes (standard Go testing) |
| 6 | Consumed stubs | Low | Yes (explicit multi-stub registration) |
| 7 | Worker registration model | Medium | Yes (embedded runner or WASM host) |
| 8 | WASM filesystem sandbox | High | Yes (host-side services) |
| 9 | replace directive needed | Low | Yes (local checkout) |
