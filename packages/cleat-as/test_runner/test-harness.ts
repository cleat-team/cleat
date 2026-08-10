/**
 * AssemblyScript test harness for cleat workflows — WASM-free mock HostCalls
 * for testing without compiling to WASM or running the cleat host.
 *
 * Provides:
 *   - MockHostCalls class — drop-in replacement for HostCalls with record/replay
 *   - Call recording — every cleatCall, cleatSleep, cleatSend, etc. is tracked
 *   - Signal delivery — simulate external signals via deliverSignal()
 *   - Stub registration — pre-configure call responses with registerCallStub()
 *   - Assertions — assertCalled(), assertNotCalled(), assertState()
 *   - Retry simulation — simulate transient failures and retries
 *
 * Usage:
 * ```ts
 * // Arrange
 * let env = new TestEnv();
 * env.registerCallStub("payment", "charge", `{"id":"ch_123","amount":5000}`);
 * env.deliverSignal("order_confirmed", `{"orderId":"ord_1"}`);
 *
 * // Act
 * let result = env.runWorkflow(placeOrderWF, `{"items":[{"sku":"s1","qty":2}]}`);
 *
 * // Assert
 * env.assertCalled("payment", "charge");
 * env.assertNotCalled("shipping", "ship");
 * assert(env.callCount("payment", "charge") == 1);
 * ```
 *
 * Mirrors Go `durabletest.TestEnv` at durable/durabletest/durabletest.go.
 *
 * @packageDocumentation
 */

// ═════════════════════════════════════════════════════════════════════════════
// Call Record types
// ═════════════════════════════════════════════════════════════════════════════

/**
 * A single recorded call through the test environment.
 */
export class CallRecord {
  constructor(
    public readonly service: string,
    public readonly operation: string,
    public readonly request: string,
    public readonly response: string,
    public readonly error: string | null,
  ) {}
}

/**
 * Internal stub entry for pre-configured call responses.
 */
class CallStub {
  constructor(
    public readonly service: string,
    public readonly operation: string,
    public readonly response: string,
    public readonly error: string | null,
    public readonly consumeCount: i32,
  ) {}
}

/**
 * Internal record for a pending signal.
 */
class PendingSignal {
  constructor(
    public readonly name: string,
    public readonly payload: string,
  ) {}
}

/**
 * Internal record for a child workflow stub.
 */
class ChildWorkflowStub {
  constructor(
    public readonly result: string,
    public readonly error: string | null,
  ) {}
}

/**
 * Internal record for a plugin call stub.
 */
class PluginCallStub {
  constructor(
    public readonly pluginName: string,
    public readonly functionName: string,
    public readonly result: string,
    public readonly error: string | null,
  ) {}
}

// ═════════════════════════════════════════════════════════════════════════════
// Result types matching HostCalls from host-calls.ts
// ═════════════════════════════════════════════════════════════════════════════

/**
 * Outcome of a cleatCall operation.
 */
export class CleatCallOutcome {
  constructor(
    public readonly response: string,
    public readonly error: string | null,
    public readonly callErrorCode: u32,
  ) {}

  get isError(): bool {
    return this.error !== null;
  }
}

/**
 * Outcome of a plugin_call operation.
 */
export class PluginCallOutcome {
  constructor(
    public readonly response: string,
    public readonly error: string | null,
    public readonly callErrorCode: u32,
  ) {}

  get isError(): bool {
    return this.error !== null;
  }
}

/**
 * Result of pollCancellation.
 */
export class CancellationStatus {
  constructor(
    public readonly cancelled: bool,
    public readonly reason: string,
  ) {}
}

/**
 * Result of pollSignal.
 */
export class PollSignalOutcome {
  constructor(
    public readonly payload: string,
    public readonly found: bool,
    public readonly error: string | null,
  ) {}

  get isError(): bool {
    return this.error !== null;
  }
}

/**
 * Result of awaitSignals.
 */
export class AwaitSignalsOutcome {
  constructor(
    public readonly signalName: string,
    public readonly payload: string,
    public readonly timedOut: bool,
    public readonly error: string | null,
  ) {}

  get isError(): bool {
    return this.error !== null;
  }
}

/**
 * Generic result type representing either a success value or an error.
 */
export class DurableResult<T> {
  constructor(
    public readonly value: T,
    public readonly error: string | null,
  ) {}

  get isError(): bool {
    return this.error !== null;
  }
}

/**
 * Result of createPromise.
 */
export class PromiseResult {
  constructor(
    public readonly value: string,
    public readonly error: string | null,
  ) {}

  get isError(): bool {
    return this.error !== null;
  }
}

/**
 * Result of awaitPromise.
 */
export class AwaitPromiseOutcome {
  constructor(
    public readonly value: string,
    public readonly timedOut: bool,
    public readonly error: string | null,
  ) {}

  get isError(): bool {
    return this.error !== null;
  }
}

/**
 * Result of a cleatFetch HTTP request.
 */
export class FetchResult {
  constructor(
    public readonly statusCode: i32,
    public readonly headers: string,
    public readonly body: string,
    public readonly error: string | null,
  ) {}

  get isError(): bool {
    return this.error !== null;
  }
}

// ═════════════════════════════════════════════════════════════════════════════
// MockHostCalls — drop-in replacement for HostCalls
// ═════════════════════════════════════════════════════════════════════════════

/**
 * Mock implementation of the cleat HostCalls API for testing workflows
 * without WASM compilation or a running cleat host.
 *
 * Every host call is recorded for later assertion. Pre-programmed responses
 * are returned when stubs are registered via TestEnv.
 *
 * This class is designed for `--runtime stub` — no closures, no try/catch,
 * no dynamic dispatch.
 */
export class MockHostCalls {
  /** Simulated clock time (ms since epoch). Starts at 2024-01-01T00:00:00Z. */
  nowMs: i64 = 1704067200000;

  /** Workflow version returned by version(). */
  versionVal: i32 = 1;

  /** Minimum version returned by minVersion(). */
  minVersionVal: i32 = 1;

  /** Pre-programmed call stubs (service+operation -> response). */
  callStubs: CallStub[] = [];

  /** Pre-programmed child workflow stubs. */
  childWorkflowStubs: Map<string, ChildWorkflowStub> = new Map();

  /** Pre-programmed plugin call stubs. */
  pluginCallStubs: PluginCallStub[] = [];

  /** Queue of pending signals awaiting delivery. */
  pendingSignals: PendingSignal[] = [];

  /** Recorded call history for assertions. */
  callHistory: CallRecord[] = [];

  /** Random value sequence for deterministic testing. */
  randomSeq: i64[] = [];

  /** Current index into randomSeq. */
  randomIdx: i32 = 0;

  /** Query state set via setQueryState(). */
  queryState: Map<string, string> = new Map();

  /** Durable workflow state set via setState/getState. */
  workflowState: Map<string, string> = new Map();

  /** Defer counter for generating defer IDs. */
  deferCounter: i32 = 0;

  /** Promises created via createPromise(). */
  promises: Map<string, string> = new Map(); // promiseID -> status ("pending"/"resolved"/"rejected")

  /** Promise results. */
  promiseResults: Map<string, string> = new Map(); // promiseID -> result JSON

  /** Promise errors. */
  promiseErrors: Map<string, string> = new Map(); // promiseID -> error string

  /** Scope prefix for virtual object state operations. */
  private _scopePrefix: string = "";

  /** Simulated cancellation status. */
  cancelled: bool = false;
  cancelReason: string = "";

  /** Retry simulation: how many times to fail before success. */
  retrySimCount: i32 = 0;

  /** Retry simulation: per-call attempt tracking. */
  retrySimAttempts: Map<string, i32> = new Map();

  /** Signal reply channels for sendSignalAndWait / replyToSignal. */
  signalReplyChannels: Map<string, string> = new Map();
  signalReplyCorrIdCounter: i32 = 0;

  /** Child workflow run results populated by registerChildResult. */
  childResults: Map<string, string> = new Map(); // runId -> result JSON

  /** Child workflow errors. */
  childErrors: Map<string, string> = new Map(); // runId -> error string

  /** Current run ID for child workflow tracking. */
  childRunIdCounter: i32 = 0;

  /** Registered update handler names. */
  updateHandlers: string[] = [];

  /** Registered deferred action descriptions. */
  deferredActions: string[] = [];

  /** Scheduled invocations registered via scheduleInvoke. */
  scheduledInvocations: string[] = [];

  /** Signals sent via signalWorkflow. */
  sentSignals: string[] = [];

  /** Workflow ID to return from currentWorkflowId(). */
  workflowId: string = "test-workflow";

  /** Run ID to return from currentRunId(). */
  runId: string = "test-run-001";

  /** Whether continueAsNew was called. */
  continueAsNewCalled: bool = false;

  /** Input passed to continueAsNew. */
  continueAsNewInput: string = "";

  // ────────────────────────────────────────────
  // 1. cleatCall
  // ────────────────────────────────────────────

  /**
   * Make a recorded API call to an external service.
   * Returns a pre-programmed stub response if one is registered.
   */
  cleatCall(service: string, operation: string, requestJson: string): CleatCallOutcome {
    // Retry simulation
    if (this.retrySimCount > 0) {
      let key: string = service + "/" + operation;
      let attempt: i32 = 0;
      if (this.retrySimAttempts.has(key)) {
        attempt = this.retrySimAttempts.get(key);
      }
      if (attempt < this.retrySimCount) {
        this.retrySimAttempts.set(key, attempt + 1);
        let errMsg: string = "simulated transient failure for " + key + " (attempt " + (attempt + 1).toString() + "/" + this.retrySimCount.toString() + ")";
        this.callHistory.push(new CallRecord(service, operation, requestJson, "", errMsg));
        return new CleatCallOutcome("", errMsg, 1);
      }
    }

    // Find a matching stub
    for (let i: i32 = 0; i < this.callStubs.length; i++) {
      let stub: CallStub = this.callStubs[i];
      if (stub.service == service && stub.operation == operation) {
        let resp: string = stub.response;
        let err: string | null = stub.error;
        // Consume stub (decrement count, remove if exhausted)
        if (stub.consumeCount > 0) {
          // For simplicity, just use it once
          this.callStubs.splice(i, 1);
        }
        this.callHistory.push(new CallRecord(service, operation, requestJson, resp, err));
        if (err !== null) {
          return new CleatCallOutcome("", err, 1);
        }
        return new CleatCallOutcome(resp, null, 0);
      }
    }

    // No stub registered
    let errMsg: string = "no stub registered for " + service + "." + operation;
    this.callHistory.push(new CallRecord(service, operation, requestJson, "", errMsg));
    return new CleatCallOutcome("", errMsg, 1);
  }

  // ────────────────────────────────────────────
  // 2. cleatSleep
  // ────────────────────────────────────────────

  /**
   * Simulate workflow suspension for a duration.
   * Advances the simulated clock.
   */
  cleatSleep(durationMs: i64): bool {
    this.nowMs += durationMs;
    return false; // In mock mode, never suspend
  }

  // ────────────────────────────────────────────
  // 3. now
  // ────────────────────────────────────────────

  /**
   * Get the current simulated wall-clock time.
   */
  now(): i64 {
    return this.nowMs;
  }

  // ────────────────────────────────────────────
  // 4. random
  // ────────────────────────────────────────────

  /**
   * Get a deterministic random value from the pre-configured sequence.
   */
  random(): i64 {
    if (this.randomIdx < this.randomSeq.length) {
      let val: i64 = this.randomSeq[this.randomIdx];
      this.randomIdx++;
      return val;
    }
    return 0;
  }

  // ────────────────────────────────────────────
  // 5. log
  // ────────────────────────────────────────────

  /**
   * Log a message (best-effort, stored in memory).
   */
  log(message: string): void {
    // No-op for testing
  }

  // ────────────────────────────────────────────
  // 6. version
  // ────────────────────────────────────────────

  /**
   * Get the configured workflow definition version.
   */
  version(): i32 {
    return this.versionVal;
  }

  // ────────────────────────────────────────────
  // 7. minVersion
  // ────────────────────────────────────────────

  /**
   * Get the configured minimum supported version.
   */
  minVersion(): i32 {
    return this.minVersionVal;
  }

  // ────────────────────────────────────────────
  // 8. defer
  // ────────────────────────────────────────────

  /**
   * Register a deferred cleanup action.
   */
  defer(description: string): DurableResult<string> {
    this.deferCounter++;
    let deferId: string = "defer-" + this.deferCounter.toString();
    this.deferredActions.push(deferId + ":" + description);
    return new DurableResult<string>(deferId, null);
  }

  // ────────────────────────────────────────────
  // 9. pollCancellation
  // ────────────────────────────────────────────

  /**
   * Check whether cancellation has been requested.
   */
  pollCancellation(): CancellationStatus {
    return new CancellationStatus(this.cancelled, this.cancelReason);
  }

  // ────────────────────────────────────────────
  // 10. pollSignal
  // ────────────────────────────────────────────

  /**
   * Poll for a specific pending signal (non-blocking).
   */
  pollSignal(name: string): PollSignalOutcome {
    for (let i: i32 = 0; i < this.pendingSignals.length; i++) {
      let sig: PendingSignal = this.pendingSignals[i];
      if (sig.name == name) {
        this.pendingSignals.splice(i, 1);
        return new PollSignalOutcome(sig.payload, true, null);
      }
    }
    return new PollSignalOutcome("", false, null);
  }

  // ────────────────────────────────────────────
  // 11. continueAsNew
  // ────────────────────────────────────────────

  /**
   * Simulate continue-as-new.
   */
  continueAsNew(inputJson: string): string | null {
    this.continueAsNewCalled = true;
    this.continueAsNewInput = inputJson;
    return null;
  }

  // ────────────────────────────────────────────
  // 12. childWorkflow
  // ────────────────────────────────────────────

  /**
   * Start a child workflow instance.
   */
  childWorkflow(name: string, inputJson: string): DurableResult<string> {
    this.childRunIdCounter++;
    let runId: string = "child-" + name + "-" + this.childRunIdCounter.toString();

    // Check for a pre-registered child result
    if (this.childResults.has(runId)) {
      // Already have a result — use it
      return new DurableResult<string>(runId, null);
    }

    // Check for a child workflow stub
    if (this.childWorkflowStubs.has(name)) {
      let stub: ChildWorkflowStub = this.childWorkflowStubs.get(name);
      if (stub.error !== null) {
        this.childErrors.set(runId, stub.error as string);
      } else {
        this.childResults.set(runId, stub.result);
      }
    } else {
      // Default: auto-complete with empty result
      this.childResults.set(runId, `{"status":"completed"}`);
    }

    return new DurableResult<string>(runId, null);
  }

  // ────────────────────────────────────────────
  // 13. awaitChild
  // ────────────────────────────────────────────

  /**
   * Wait for a child workflow to complete.
   */
  awaitChild(runId: string): DurableResult<string> {
    if (this.childErrors.has(runId)) {
      return new DurableResult<string>("", this.childErrors.get(runId));
    }
    if (this.childResults.has(runId)) {
      return new DurableResult<string>(this.childResults.get(runId), null);
    }
    return new DurableResult<string>(`{"status":"completed"}`, null);
  }

  // ────────────────────────────────────────────
  // 14. awaitSignals
  // ────────────────────────────────────────────

  /**
   * Wait for one or more external signals with a timeout.
   */
  awaitSignals(namesJson: string, timeoutMs: i64): AwaitSignalsOutcome {
    // Parse signal names from JSON array (simplified parsing)
    let names: string = namesJson;
    let foundSig: PendingSignal | null = null;
    let foundIdx: i32 = -1;

    for (let i: i32 = 0; i < this.pendingSignals.length; i++) {
      let sig: PendingSignal = this.pendingSignals[i];
      // Check if signal name is in the requested names (simple substring match vs JSON array)
      if (names.indexOf(sig.name) >= 0) {
        foundSig = sig;
        foundIdx = i;
        break;
      }
    }

    if (foundSig !== null && foundIdx >= 0) {
      this.pendingSignals.splice(foundIdx, 1);
      return new AwaitSignalsOutcome(
        foundSig.name,
        foundSig.payload,
        false,
        null,
      );
    }

    // No matching signal — return timeout
    if (timeoutMs <= 0) {
      return new AwaitSignalsOutcome("", "", true, null);
    }
    this.nowMs += timeoutMs;
    return new AwaitSignalsOutcome("", "", true, null);
  }

  // ────────────────────────────────────────────
  // 15. setQueryState
  // ────────────────────────────────────────────

  /**
   * Set a key-value pair in query state.
   */
  setQueryState(key: string, value: string): void {
    this.queryState.set(this.scopedKey(key), value);
  }

  // ────────────────────────────────────────────
  // 16. createPromise
  // ────────────────────────────────────────────

  /**
   * Create a new durable promise.
   */
  createPromise(name: string): PromiseResult {
    this.deferCounter++;
    let promiseId: string = "prom-" + name + "-" + this.deferCounter.toString();
    this.promises.set(promiseId, "pending");
    return new PromiseResult(promiseId, null);
  }

  // ────────────────────────────────────────────
  // 17. awaitPromise
  // ────────────────────────────────────────────

  /**
   * Wait for a durable promise to resolve.
   */
  awaitPromise(id: string, timeoutMs: i64): AwaitPromiseOutcome {
    if (!this.promises.has(id)) {
      return new AwaitPromiseOutcome("", false, "promise not found: " + id);
    }

    let status: string = this.promises.get(id);

    if (status == "resolved") {
      return new AwaitPromiseOutcome(
        this.promiseResults.has(id) ? this.promiseResults.get(id) : "",
        false,
        null,
      );
    }

    if (status == "rejected") {
      let errMsg: string = this.promiseErrors.has(id) ? this.promiseErrors.get(id) : "rejected";
      return new AwaitPromiseOutcome("", false, errMsg);
    }

    // Pending — advance time to simulate timeout
    this.nowMs += timeoutMs;
    return new AwaitPromiseOutcome("", true, null);
  }

  // ────────────────────────────────────────────
  // 18. registerUpdateHandler
  // ────────────────────────────────────────────

  /**
   * Register a handler for update calls.
   */
  registerUpdateHandler(name: string): void {
    this.updateHandlers.push(name);
  }

  // ────────────────────────────────────────────
  // 19. pluginCall
  // ────────────────────────────────────────────

  /**
   * Call a plugin function via the host runtime.
   */
  pluginCall(pluginName: string, functionName: string, inputJson: string): PluginCallOutcome {
    for (let i: i32 = 0; i < this.pluginCallStubs.length; i++) {
      let stub: PluginCallStub = this.pluginCallStubs[i];
      if (stub.pluginName == pluginName && stub.functionName == functionName) {
        if (stub.error !== null) {
          return new PluginCallOutcome("", stub.error, 1);
        }
        return new PluginCallOutcome(stub.result, null, 0);
      }
    }
    return new PluginCallOutcome("", "no stub registered for plugin " + pluginName + "." + functionName, 1);
  }

  // ────────────────────────────────────────────
  // 20. currentWorkflowId
  // ────────────────────────────────────────────

  /**
   * Get the configured workflow ID.
   */
  currentWorkflowId(): string {
    return this.workflowId;
  }

  // ────────────────────────────────────────────
  // 21. setScope
  // ────────────────────────────────────────────

  /**
   * Set the state key prefix for virtual object instances.
   */
  setScope(objectType: string, instanceKey: string): string {
    let prev: string = this._scopePrefix;
    this._scopePrefix =
      objectType.length > 0 && instanceKey.length > 0
        ? "vo:" + objectType + ":" + instanceKey + ":"
        : "";
    return prev;
  }

  // ────────────────────────────────────────────
  // 22. getScope
  // ────────────────────────────────────────────

  /**
   * Get the current virtual object scope.
   */
  getScope(): string[] {
    if (this._scopePrefix.length === 0) {
      return ["", ""];
    }
    let trimmed: string = this._scopePrefix.substring(0, this._scopePrefix.length - 1);
    let parts: string[] = trimmed.split(":");
    if (parts.length == 3 && parts[0] == "vo") {
      return [parts[1], parts[2]];
    }
    return ["", ""];
  }

  // ────────────────────────────────────────────
  // 23. clearScope
  // ────────────────────────────────────────────

  /**
   * Remove the current scope.
   */
  clearScope(): string {
    let prev: string = this._scopePrefix;
    this._scopePrefix = "";
    return prev;
  }

  // ────────────────────────────────────────────
  // 24. uuid
  // ────────────────────────────────────────────

  /**
   * Return a deterministic UUID from a seed.
   */
  uuid(seed: string): string {
    let wfId: string = this.currentWorkflowId();
    let data: string = wfId + ":" + seed;

    // FNV-1a 128-bit hash for deterministic UUID generation
    let h1: u64 = 0xcbf29ce484222325;
    let h2: u64 = 0x6c62272e07bb0142;
    for (let i: i32 = 0; i < data.length; i++) {
      let c: u32 = data.charCodeAt(i);
      h1 ^= c;
      h1 *= 0x100000001b3;
      h2 ^= c;
      h2 *= 0x100000001b3;
    }

    // Set UUIDv5 version bits
    h1 = (h1 & ~(u64(0xf0) << 8)) | (u64(0x50) << 8);
    // Set UUID variant 1 bits
    h2 = (h2 & ~(u64(0xc0) << 56)) | (u64(0x80) << 56);

    let hexChars: string = "0123456789abcdef";
    let parts: string[] = ["", "", "", "", ""];

    let temp: u64 = h1;
    for (let i: i32 = 0; i < 8; i++) {
      parts[0] += hexChars.charAt((temp >> 60) as u32);
      temp <<= 4;
    }
    for (let i: i32 = 0; i < 4; i++) {
      parts[1] += hexChars.charAt((temp >> 60) as u32);
      temp <<= 4;
    }
    for (let i: i32 = 0; i < 4; i++) {
      parts[2] += hexChars.charAt((temp >> 60) as u32);
      temp <<= 4;
    }

    temp = h2;
    for (let i: i32 = 0; i < 4; i++) {
      parts[3] += hexChars.charAt((temp >> 60) as u32);
      temp <<= 4;
    }
    for (let i: i32 = 0; i < 12; i++) {
      parts[4] += hexChars.charAt((temp >> 60) as u32);
      temp <<= 4;
    }

    return parts[0] + "-" + parts[1] + "-" + parts[2] + "-" + parts[3] + "-" + parts[4];
  }

  // ────────────────────────────────────────────
  // 25. sendSignalAndWait
  // ────────────────────────────────────────────

  /**
   * Send a signal to a target workflow and wait for a response.
   */
  sendSignalAndWait(
    targetRunId: string,
    signalName: string,
    payload: string,
    timeoutMs: i64,
  ): DurableResult<string> {
    this.signalReplyCorrIdCounter++;
    let correlationId: string = "corr-" + targetRunId + "-" + signalName + "-" + this.signalReplyCorrIdCounter.toString();

    // Register a reply channel
    this.signalReplyChannels.set(correlationId, "__pending__");

    // Send the signal
    this.signalWorkflow(targetRunId, signalName, payload);

    // Simulate waiting: check if reply was already sent
    if (this.signalReplyChannels.has(correlationId)) {
      let reply: string = this.signalReplyChannels.get(correlationId);
      if (reply != "__pending__") {
        this.signalReplyChannels.delete(correlationId);
        return new DurableResult<string>(reply, null);
      }
    }

    // Timeout
    this.nowMs += timeoutMs;
    return new DurableResult<string>("", "SendSignalAndWait timed out");
  }

  // ────────────────────────────────────────────
  // 26. replyToSignal
  // ────────────────────────────────────────────

  /**
   * Reply to a signal from within a handler.
   */
  replyToSignal(correlationId: string, response: string): string | null {
    if (this.signalReplyChannels.has(correlationId)) {
      this.signalReplyChannels.set(correlationId, response);
      return null;
    }
    return "no pending signal for correlation ID: " + correlationId;
  }

  // ────────────────────────────────────────────
  // 27. awaitSignalsWithQuorum
  // ────────────────────────────────────────────

  /**
   * Wait for a quorum of signals.
   */
  awaitSignalsWithQuorum(
    namesJson: string,
    minCount: i32,
    maxRejections: i32,
    timeoutMs: i64,
  ): AwaitSignalsOutcome[] {
    let results: AwaitSignalsOutcome[] = [];
    let deadline: i64 = this.nowMs + timeoutMs;
    let rejectionCount: i32 = 0;

    while (results.length < minCount) {
      let remainingMs: i64 = deadline - this.nowMs;
      if (remainingMs <= 0) {
        break;
      }

      let outcome: AwaitSignalsOutcome = this.awaitSignals(namesJson, remainingMs);
      if (outcome.timedOut) {
        break;
      }
      if (outcome.isError) {
        break;
      }

      results.push(outcome);

      // Check for rejection
      if (maxRejections >= 0 && outcome.payload.length > 0) {
        if (outcome.payload.indexOf('"rejected":true') >= 0 || outcome.payload.indexOf('"rejected": true') >= 0) {
          rejectionCount++;
          if (rejectionCount > maxRejections) {
            break;
          }
        }
      }
    }

    return results;
  }

  // ────────────────────────────────────────────
  // 28. signalWorkflow
  // ────────────────────────────────────────────

  /**
   * Send a signal to a target workflow (fire-and-forget).
   */
  signalWorkflow(targetRunId: string, signalName: string, payload: string): string | null {
    this.sentSignals.push(targetRunId + ":" + signalName);
    this.pendingSignals.push(new PendingSignal(signalName, payload));
    return null;
  }

  // ────────────────────────────────────────────
  // 29-30. resolvePromise / rejectPromise
  // ────────────────────────────────────────────

  /**
   * Resolve a durable promise with a value.
   */
  resolvePromise(id: string, value: string): string | null {
    if (this.promises.has(id)) {
      this.promises.set(id, "resolved");
      this.promiseResults.set(id, value);
      return null;
    }
    return "promise not found: " + id;
  }

  /**
   * Reject a durable promise with an error.
   */
  rejectPromise(id: string, error: string): string | null {
    if (this.promises.has(id)) {
      this.promises.set(id, "rejected");
      this.promiseErrors.set(id, error);
      return null;
    }
    return "promise not found: " + id;
  }

  // ────────────────────────────────────────────
  // 31. cleatSend (fire-and-forget)
  // ────────────────────────────────────────────

  /**
   * Fire-and-forget durable call.
   */
  cleatSend(service: string, operation: string, requestJson: string): string | null {
    this.callHistory.push(new CallRecord(service, operation, requestJson, "", null));
    return null;
  }

  // ────────────────────────────────────────────
  // 32. scheduleInvoke
  // ────────────────────────────────────────────

  /**
   * Schedule a delayed invocation.
   */
  scheduleInvoke(service: string, operation: string, requestJson: string, delayMs: i64): string | null {
    this.scheduledInvocations.push(service + "." + operation + ":" + delayMs.toString());
    return null;
  }

  // There is no registerQueryHandler here (removed 2026-08-09). See
  // docs/determinism.md, "Why there is no RegisterQueryHandler". Use
  // setQueryState instead.

  // ────────────────────────────────────────────
  // 34. runDetached
  // ────────────────────────────────────────────

  /**
   * Run a workflow in detached mode.
   */
  runDetached(name: string, inputJson: string): string | null {
    return null;
  }

  // ────────────────────────────────────────────
  // 35. setState
  // ────────────────────────────────────────────

  /**
   * Set a key-value pair in workflow state.
   */
  setState(key: string, value: string): string | null {
    this.workflowState.set(this.scopedKey(key), value);
    return null;
  }

  // ────────────────────────────────────────────
  // 36. getState
  // ────────────────────────────────────────────

  /**
   * Get a value from workflow state by key.
   */
  getState(key: string): string | null {
    let scoped: string = this.scopedKey(key);
    if (this.workflowState.has(scoped)) {
      return this.workflowState.get(scoped);
    }
    return null;
  }

  // ────────────────────────────────────────────
  // 37. deleteState
  // ────────────────────────────────────────────

  /**
   * Delete a key from workflow state.
   */
  deleteState(key: string): string | null {
    let scoped: string = this.scopedKey(key);
    if (this.workflowState.has(scoped)) {
      this.workflowState.delete(scoped);
    }
    return null;
  }

  // ────────────────────────────────────────────
  // 38. incrState
  // ────────────────────────────────────────────

  /**
   * Atomically increment a numeric state value.
   */
  incrState(key: string, delta: i64): i64 {
    let scoped: string = this.scopedKey(key);
    let current: i64 = 0;
    if (this.workflowState.has(scoped)) {
      current = <i64>I64.parseInt(this.workflowState.get(scoped));
    }
    current += delta;
    this.workflowState.set(scoped, current.toString());
    return current;
  }

  // ────────────────────────────────────────────
  // 39. hasState
  // ────────────────────────────────────────────

  /**
   * Check if a key exists in workflow state.
   */
  hasState(key: string): bool {
    return this.workflowState.has(this.scopedKey(key));
  }

  // ────────────────────────────────────────────
  // 40. listState
  // ────────────────────────────────────────────

  /**
   * List state keys matching a prefix.
   */
  listState(prefix: string): string[] {
    let scopedPrefix: string = this.scopedKey(prefix);
    let result: string[] = [];
    let keys: string[] = this.workflowState.keys();
    for (let i: i32 = 0; i < keys.length; i++) {
      if (keys[i].startsWith(scopedPrefix)) {
        result.push(keys[i]);
      }
    }
    return result;
  }

  // ────────────────────────────────────────────
  // 41. awaitAllChildren
  // ────────────────────────────────────────────

  /**
   * Wait for multiple child workflows to complete.
   */
  awaitAllChildren(runIdsJson: string): string | null {
    // Build a JSON array of child results
    let results: string = "[";
    // Parse run IDs from JSON (simplified)
    let ids: string = runIdsJson;
    let first: bool = true;

    // Even simpler: look up each run ID using awaitChild
    // For now, return a placeholder array
    let resultArr: string[] = [];
    for (let i: i32 = 0; i < this.childResults.keys().length; i++) {
      let key: string = this.childResults.keys()[i];
      resultArr.push(`{"runId":"${key}","result":"${this.childResults.get(key)}"}`);
    }
    for (let i: i32 = 0; i < resultArr.length; i++) {
      if (!first) {
        results += ",";
      }
      results += resultArr[i];
      first = false;
    }
    results += "]";
    return results;
  }

  // ────────────────────────────────────────────
  // 42. currentRunId
  // ────────────────────────────────────────────

  /**
   * Get the current run ID.
   */
  currentRunId(): string {
    return this.runId;
  }

  // ────────────────────────────────────────────
  // 43. cleatFetch
  // ────────────────────────────────────────────

  /**
   * Make an HTTP request via the host runtime.
   */
  cleatFetch(method: string, url: string, headersJson: string, body: string): FetchResult {
    // No stubs for fetch yet — return an error
    return new FetchResult(0, "{}", "", "cleatFetch not supported in mock mode");
  }

  // ────────────────────────────────────────────
  // 44. fetchGet
  // ────────────────────────────────────────────

  /**
   * Convenience HTTP GET.
   */
  fetchGet(url: string): FetchResult {
    return this.cleatFetch("GET", url, "{}", "");
  }

  // ────────────────────────────────────────────
  // Private helpers
  // ────────────────────────────────────────────

  /**
   * Prepend the current scope prefix to a key if setScope was called.
   */
  private scopedKey(key: string): string {
    if (this._scopePrefix.length > 0) {
      return this._scopePrefix + key;
    }
    return key;
  }

  // ────────────────────────────────────────────
  // Mock-specific configuration methods
  // ────────────────────────────────────────────

  /**
   * Register a pre-programmed response for a call to service.operation.
   */
  registerCallStub(service: string, operation: string, response: string): void {
    this.callStubs.push(new CallStub(service, operation, response, null, 1));
  }

  /**
   * Register a pre-programmed error for a call to service.operation.
   */
  registerCallError(service: string, operation: string, error: string): void {
    this.callStubs.push(new CallStub(service, operation, "", error, 1));
  }

  /**
   * Configure a stub for a child workflow with the given name.
   */
  registerChildWorkflowStub(name: string, result: string, error: string | null): void {
    this.childWorkflowStubs.set(name, new ChildWorkflowStub(result, error));
  }

  /**
   * Pre-set the result that a child workflow run will return.
   */
  registerChildResult(runId: string, result: string, error: string | null): void {
    if (error !== null) {
      this.childErrors.set(runId, error);
    } else {
      this.childResults.set(runId, result);
    }
  }

  /**
   * Register a plugin call stub.
   */
  registerPluginCallStub(pluginName: string, functionName: string, result: string): void {
    this.pluginCallStubs.push(new PluginCallStub(pluginName, functionName, result, null));
  }

  /**
   * Deliver a signal immediately.
   */
  deliverSignal(name: string, payload: string): void {
    this.pendingSignals.push(new PendingSignal(name, payload));
  }

  /**
   * Configure the random value sequence.
   */
  setRandomSeq(seq: i64[]): void {
    this.randomSeq = seq;
    this.randomIdx = 0;
  }

  /**
   * Set the simulated time.
   */
  setTime(ms: i64): void {
    this.nowMs = ms;
  }

  /**
   * Advance the simulated clock.
   */
  advanceTime(ms: i64): void {
    this.nowMs += ms;
  }

  /**
   * Set the version returned by version().
   */
  setVersion(v: i32): void {
    this.versionVal = v;
  }

  /**
   * Set the minimum version returned by minVersion().
   */
  setMinVersion(v: i32): void {
    this.minVersionVal = v;
  }

  /**
   * Configure cancellation simulation.
   */
  setCancelled(cancelled: bool, reason: string): void {
    this.cancelled = cancelled;
    this.cancelReason = reason;
  }

  /**
   * Configure retry simulation: fail the first n calls per (service, operation).
   */
  setRetrySimulation(n: i32): void {
    this.retrySimCount = n;
  }

  /**
   * Send a signal reply (to unblock a sendSignalAndWait call).
   */
  sendReply(correlationId: string, response: string): void {
    if (this.signalReplyChannels.has(correlationId)) {
      this.signalReplyChannels.set(correlationId, response);
    }
  }

  // ────────────────────────────────────────────
  // Query helpers
  // ────────────────────────────────────────────

  /**
   * Read back a value set via setQueryState.
   */
  readQueryState(key: string): string | null {
    if (this.queryState.has(key)) {
      return this.queryState.get(key);
    }
    return null;
  }

  // ────────────────────────────────────────────
  // Assertion helpers
  // ────────────────────────────────────────────

  /**
   * Check if a call to the given service+operation was recorded.
   */
  callCount(service: string, operation: string): i32 {
    let count: i32 = 0;
    for (let i: i32 = 0; i < this.callHistory.length; i++) {
      let rec: CallRecord = this.callHistory[i];
      if (rec.service == service && rec.operation == operation) {
        count++;
      }
    }
    return count;
  }

  /**
   * Get the call history as an array.
   */
  getCallHistory(): CallRecord[] {
    return this.callHistory;
  }

  /**
   * Clear all recorded calls, stubs, signals, and state.
   */
  reset(): void {
    this.callStubs = [];
    this.callHistory = [];
    this.pendingSignals = [];
    this.queryState = new Map();
    this.workflowState = new Map();
    this.randomSeq = [];
    this.randomIdx = 0;
    this.deferCounter = 0;
    this.childWorkflowStubs = new Map();
    this.childResults = new Map();
    this.childErrors = new Map();
    this.pluginCallStubs = [];
    this.promises = new Map();
    this.promiseResults = new Map();
    this.promiseErrors = new Map();
    this.signalReplyChannels = new Map();
    this.signalReplyCorrIdCounter = 0;
    this.deferredActions = [];
    this.scheduledInvocations = [];
    this.sentSignals = [];
    this.updateHandlers = [];
    this.cancelled = false;
    this.cancelReason = "";
    this.retrySimCount = 0;
    this.retrySimAttempts = new Map();
    this.continueAsNewCalled = false;
    this.continueAsNewInput = "";
    this.nowMs = 1704067200000; // Reset to 2024-01-01
    this.versionVal = 1;
    this.minVersionVal = 1;
    this.childRunIdCounter = 0;
    this.workflowId = "test-workflow";
    this.runId = "test-run-001";
    this._scopePrefix = "";
  }
}

// ═════════════════════════════════════════════════════════════════════════════
// TestEnv — high-level test environment orchestrator
// ═════════════════════════════════════════════════════════════════════════════

/**
 * High-level test environment for cleat workflows, mirroring Go's
 * `durabletest.TestEnv`.
 *
 * Usage:
 * ```ts
 * // Create test environment
 * let env = new TestEnv();
 *
 * // Stub external service calls
 * env.registerCallStub("payment", "charge", `{"id":"ch_123","amount":5000}`);
 *
 * // Deliver a signal
 * env.deliverSignal("order_confirmed", `{"orderId":"ord_1"}`);
 *
 * // Run the workflow with the mock host
 * let result = env.runWorkflow(myWorkflow, `{"items":[]}`);
 *
 * // Assert expected calls were made
 * env.assertCalled("payment", "charge");
 * env.assertNotCalled("shipping", "ship");
 * ```
 */
export class TestEnv {
  /** The underlying MockHostCalls instance. */
  mock: MockHostCalls;

  constructor() {
    this.mock = new MockHostCalls();
  }

  // ────────────────────────────────────────────
  // Stub registration
  // ────────────────────────────────────────────

  /**
   * Register a stub response for a call to service.operation.
   */
  registerCallStub(service: string, operation: string, response: string): void {
    this.mock.registerCallStub(service, operation, response);
  }

  /**
   * Register a stub error for a call to service.operation.
   */
  registerCallError(service: string, operation: string, error: string): void {
    this.mock.registerCallError(service, operation, error);
  }

  /**
   * Register a stub for a child workflow.
   */
  registerChildWorkflowStub(name: string, result: string): void {
    this.mock.registerChildWorkflowStub(name, result, null);
  }

  /**
   * Pre-set a child workflow run result.
   */
  registerChildResult(runId: string, result: string): void {
    this.mock.registerChildResult(runId, result, null);
  }

  /**
   * Register a plugin call stub.
   */
  registerPluginCallStub(pluginName: string, functionName: string, result: string): void {
    this.mock.registerPluginCallStub(pluginName, functionName, result);
  }

  // ────────────────────────────────────────────
  // Signal delivery
  // ────────────────────────────────────────────

  /**
   * Deliver a signal immediately (at current simulated time).
   */
  deliverSignal(name: string, payload: string): void {
    this.mock.deliverSignal(name, payload);
  }

  // ────────────────────────────────────────────
  // Time management
  // ────────────────────────────────────────────

  /**
   * Set the simulated clock time.
   */
  setTime(ms: i64): void {
    this.mock.setTime(ms);
  }

  /**
   * Advance the simulated clock by the given number of milliseconds.
   */
  advanceTime(ms: i64): void {
    this.mock.advanceTime(ms);
  }

  /**
   * Get the current simulated time.
   */
  now(): i64 {
    return this.mock.now();
  }

  // ────────────────────────────────────────────
  // Configuration
  // ────────────────────────────────────────────

  /**
   * Set the workflow version.
   */
  setVersion(v: i32): void {
    this.mock.setVersion(v);
  }

  /**
   * Set the minimum workflow version.
   */
  setMinVersion(v: i32): void {
    this.mock.setMinVersion(v);
  }

  /**
   * Configure the random value sequence.
   */
  setRandomSeq(seq: i64[]): void {
    this.mock.setRandomSeq(seq);
  }

  /**
   * Configure retry simulation: fail the first n calls per (service, operation).
   */
  setRetrySimulation(n: i32): void {
    this.mock.setRetrySimulation(n);
  }

  /**
   * Configure cancellation simulation.
   */
  setCancelled(cancelled: bool, reason: string = ""): void {
    this.mock.setCancelled(cancelled, reason);
  }

  /**
   * Resolve a promise (external resolution by the test).
   */
  resolvePromise(id: string, value: string): void {
    this.mock.resolvePromise(id, value);
  }

  /**
   * Reject a promise (external rejection by the test).
   */
  rejectPromise(id: string, error: string): void {
    this.mock.rejectPromise(id, error);
  }

  // ────────────────────────────────────────────
  // Workflow runner
  // ────────────────────────────────────────────

  /**
   * Run a workflow function with the mock HostCalls.
   *
   * @param entryFn - The workflow entry function that accepts a MockHostCalls
   *                  and an input JSON string, and returns a result JSON string.
   * @param input   - The input JSON string to pass to the workflow.
   * @returns The workflow function's return value.
   */
  runWorkflow(entryFn: (host: MockHostCalls, input: string) => string, input: string): string {
    return entryFn(this.mock, input);
  }

  // ────────────────────────────────────────────
  // Assertions
  // ────────────────────────────────────────────

  /**
   * Assert that a call to the given service+operation was made.
   * Returns true if the call was found, false otherwise.
   */
  assertCalled(service: string, operation: string): bool {
    return this.mock.callCount(service, operation) > 0;
  }

  /**
   * Assert that a call to the given service+operation was NOT made.
   * Returns true if the call was NOT found, false otherwise.
   */
  assertNotCalled(service: string, operation: string): bool {
    return this.mock.callCount(service, operation) == 0;
  }

  /**
   * Assert that a call was made exactly n times.
   */
  assertCallCount(service: string, operation: string, expected: i32): bool {
    return this.mock.callCount(service, operation) == expected;
  }

  /**
   * Assert that the workflow state key has the given value.
   */
  assertState(key: string, value: string): bool {
    let stateVal: string | null = this.mock.getState(key);
    if (stateVal === null) {
      return false;
    }
    return stateVal == value;
  }

  /**
   * Assert that a signal with the given name was delivered.
   */
  assertSignalDelivered(signalName: string): bool {
    for (let i: i32 = 0; i < this.mock.sentSignals.length; i++) {
      if (this.mock.sentSignals[i].indexOf(":" + signalName) >= 0) {
        return true;
      }
    }
    return false;
  }

  // ────────────────────────────────────────────
  // Query helpers
  // ────────────────────────────────────────────

  /**
   * Read a query state value set by the workflow.
   */
  readQueryState(key: string): string | null {
    return this.mock.readQueryState(key);
  }

  /**
   * Get the full call history.
   */
  getCallHistory(): CallRecord[] {
    return this.mock.getCallHistory();
  }

  /**
   * Get the number of times a specific call was made.
   */
  callCount(service: string, operation: string): i32 {
    return this.mock.callCount(service, operation);
  }

  /**
   * Reset the entire test environment to its initial state.
   */
  reset(): void {
    this.mock.reset();
  }
}
