/**
 * AssemblyScript bindings for the cleat WASM host function imports.
 *
 * Provides raw `@external` import declarations and the `HostCalls` class
 * that wraps each import with idiomatic AssemblyScript methods.
 *
 * Matches the Rust SDK at crates/cleat-sdk/src/host_calls.rs and
 * the ABI specification in ABI.md.
 */

import {
  Memory,
  decodeCallResult,
  decodeSleepResult,
  decodeSimpleResult,
  decodeAwaitSignalsResult,
  decodePollSignalResult,
  decodePollCancellationResult,
  decodeAwaitPromiseResult,
  decodeGetScopeResult,
  SUSPEND_SENTINEL,
  OUT_BUF_SIZE,
  SCRATCH_BASE,
  OUTPUT_OFFSET,
  setWorkflowSuspended,
  isInDeferPhase,
} from "./memory";

import { jsonStrArray, jsonExtractString, jsonExtractNumber } from "./json";

/**
 * Map an error code from the host runtime to a human-readable name.
 */
function errorCodeName(code: u32): string {
  switch (code) {
    case 0: return "unknown";
    case 1: return "timeout";
    case 2: return "transient";
    case 3: return "not_found";
    case 4: return "invalid_request";
    case 5: return "permission_denied";
    default: return "unknown_code";
  }
}

// ═══════════════════════════════════════════════
// Raw host function imports from "env" module
// ═══════════════════════════════════════════════

/**
 * 1. cleat_sleep: Suspend workflow execution for a duration.
 * (import "env" "cleat_sleep") (param i64) (result i64)
 */
@external("env", "cleat_sleep")
export declare function import_cleat_sleep(durationMs: i64): i64;

/**
 * 2. cleat_now: Get current wall-clock time.
 * (import "env" "cleat_now") (result i64)
 */
@external("env", "cleat_now")
export declare function import_cleat_now(): i64;

/**
 * 3. cleat_random: Get a deterministic random value.
 * (import "env" "cleat_random") (result i64)
 */
@external("env", "cleat_random")
export declare function import_cleat_random(): i64;

/**
 * 4. cleat_log: Log a message to the host.
 * (import "env" "cleat_log") (param i32 i32) (result i64)
 */
@external("env", "cleat_log")
export declare function import_cleat_log(msgPtr: i32, msgLen: i32): i64;

/**
 * 5. cleat_version: Get the workflow definition version.
 * (import "env" "cleat_version") (result i64)
 */
@external("env", "cleat_version")
export declare function import_cleat_version(): i64;

/**
 * 6. cleat_min_version: Get the minimum supported version.
 * (import "env" "cleat_min_version") (result i64)
 */
@external("env", "cleat_min_version")
export declare function import_cleat_min_version(): i64;

/**
 * 7. cleat_defer: Register cleanup to run on workflow exit.
 * (import "env" "cleat_defer") (param i32 i32 i32 i32) (result i64)
 */
@external("env", "cleat_defer")
export declare function import_cleat_defer(
  descPtr: i32,
  descLen: i32,
  deferIdPtr: i32,
  deferIdMaxLen: i32,
): i64;

/**
 * 8. cleat_poll_cancellation: Check for cancellation request.
 * (import "env" "cleat_poll_cancellation") (param i32 i32) (result i64)
 */
@external("env", "cleat_poll_cancellation")
export declare function import_cleat_poll_cancellation(
  reasonPtr: i32,
  reasonMaxLen: i32,
): i64;

/**
 * 9. cleat_poll_signal: Poll for a specific pending signal.
 * (import "env" "cleat_poll_signal") (param i32 i32 i32 i32) (result i64)
 */
@external("env", "cleat_poll_signal")
export declare function import_cleat_poll_signal(
  namePtr: i32,
  nameLen: i32,
  payloadPtr: i32,
  payloadMaxLen: i32,
): i64;

/**
 * 10. cleat_continue_as_new: Start a new workflow run with fresh input.
 * (import "env" "cleat_continue_as_new") (param i32 i32) (result i64)
 */
@external("env", "cleat_continue_as_new")
export declare function import_cleat_continue_as_new(
  inputPtr: i32,
  inputLen: i32,
): i64;

/**
 * 11. cleat_child_workflow: Start a child workflow instance.
 * (import "env" "cleat_child_workflow") (param i32 i32 i32 i32 i32 i32) (result i64)
 */
@external("env", "cleat_child_workflow")
export declare function import_cleat_child_workflow(
  namePtr: i32,
  nameLen: i32,
  inputPtr: i32,
  inputLen: i32,
  runIdPtr: i32,
  runIdMaxLen: i32,
): i64;

/**
 * 11b. cleat_child_workflow_with_options: Start a child workflow with version and priority options.
 * (import "env" "cleat_child_workflow_with_options") (param i32 i32 i32 i32 i64 i64 i32 i32 i32 i32) (result i64)
 */
@external("env", "cleat_child_workflow_with_options")
export declare function import_cleat_child_workflow_with_options(
  namePtr: i32,
  nameLen: i32,
  inputPtr: i32,
  inputLen: i32,
  version: i64,
  priority: i64,
  policyPtr: i32,
  policyLen: i32,
  runIdPtr: i32,
  runIdMaxLen: i32,
): i64;

/**
 * 12. cleat_await_child: Wait for a child workflow to complete.
 * (import "env" "cleat_await_child") (param i32 i32 i32 i32) (result i64)
 */
@external("env", "cleat_await_child")
export declare function import_cleat_await_child(
  runIdPtr: i32,
  runIdLen: i32,
  resultPtr: i32,
  resultMaxLen: i32,
): i64;

/**
 * 13. cleat_await_signals: Wait for external signals with timeout.
 * (import "env" "cleat_await_signals") (param i32 i32 i64 i32 i32 i32 i32) (result i64)
 */
@external("env", "cleat_await_signals")
export declare function import_cleat_await_signals(
  namesPtr: i32,
  namesLen: i32,
  timeoutMs: i64,
  sigNamePtr: i32,
  sigNameMaxLen: i32,
  payloadPtr: i32,
  payloadMaxLen: i32,
): i64;

/**
 * 14. set_query_state: Set a key-value pair in query state.
 * (import "env" "set_query_state") (param i32 i32 i32 i32) (result i64)
 */
@external("env", "set_query_state")
export declare function import_set_query_state(
  keyPtr: i32,
  keyLen: i32,
  valPtr: i32,
  valLen: i32,
): i64;

/**
 * 15. cleat_call: Make a recorded API call to an external service.
 * (import "env" "cleat_call") (param i32 i32 i32 i32 i32 i32 i32 i32) (result i64)
 */
@external("env", "cleat_call")
export declare function import_cleat_call(
  svcPtr: i32,
  svcLen: i32,
  opPtr: i32,
  opLen: i32,
  reqPtr: i32,
  reqLen: i32,
  respPtr: i32,
  respMaxLen: i32,
): i64;

/**
 * 16. cleat_create_promise: Create a new durable promise.
 * (import "env" "cleat_create_promise") (param i32 i32 i32 i32) (result i64)
 */
@external("env", "cleat_create_promise")
export declare function import_cleat_create_promise(
  namePtr: i32,
  nameLen: i32,
  idOutPtr: i32,
  idOutMax: i32,
): i64;

/**
 * 17. cleat_await_promise: Wait for a durable promise to resolve.
 * (import "env" "cleat_await_promise") (param i32 i32 i64 i32 i32) (result i64)
 */
@external("env", "cleat_await_promise")
export declare function import_cleat_await_promise(
  idPtr: i32,
  idLen: i32,
  timeoutMs: i64,
  resultOutPtr: i32,
  resultOutMax: i32,
): i64;

/**
 * 18. cleat_register_update_handler: Register a handler for update calls.
 * (import "env" "cleat_register_update_handler") (param i32 i32) (result i64)
 */
@external("env", "cleat_register_update_handler")
export declare function import_cleat_register_update_handler(
  namePtr: i32,
  nameLen: i32,
): i64;

/**
 * 19. plugin_call: Host-only extension for plugin function calls.
 * (import "env" "plugin_call") (param i32 i32 i32 i32 i32 i32 i32 i32) (result i64)
 */
@external("env", "plugin_call")
export declare function import_plugin_call(
  pluginNamePtr: i32,
  pluginNameLen: i32,
  functionNamePtr: i32,
  functionNameLen: i32,
  inputPtr: i32,
  inputLen: i32,
  responsePtr: i32,
  responseMaxLen: i32,
): i64;

/**
 * 20. cleat_workflow_id: Get the current workflow ID.
 * (import "env" "cleat_workflow_id") (param i32 i32) (result i64)
 */
@external("env", "cleat_workflow_id")
export declare function import_cleat_workflow_id(
  idPtr: i32,
  idMaxLen: i32,
): i64;

/**
 * 21. cleat_run_id: Get the current run ID.
 * (import "env" "cleat_run_id") (param i32 i32) (result i64)
 */
@external("env", "cleat_run_id")
export declare function import_cleat_run_id(
  idPtr: i32,
  idMaxLen: i32,
): i64;

/**
 * 22. cleat_send_signal_and_wait: Send a signal and wait for a response.
 * (import "env" "cleat_send_signal_and_wait") (param i32 i32 i32 i32 i32 i32 i64 i32 i32) (result i64)
 */
@external("env", "cleat_send_signal_and_wait")
export declare function import_cleat_send_signal_and_wait(
  targetRunIdPtr: i32,
  targetRunIdLen: i32,
  signalNamePtr: i32,
  signalNameLen: i32,
  payloadPtr: i32,
  payloadLen: i32,
  timeoutMs: i64,
  responsePtr: i32,
  responseMaxLen: i32,
): i64;

/**
 * 23. cleat_reply_to_signal: Respond to a signal from within a handler.
 * (import "env" "cleat_reply_to_signal") (param i32 i32 i32 i32) (result i64)
 */
@external("env", "cleat_reply_to_signal")
export declare function import_cleat_reply_to_signal(
  correlationIdPtr: i32,
  correlationIdLen: i32,
  responsePtr: i32,
  responseLen: i32,
): i64;

/**
 * 24. cleat_signal_workflow: Send a signal to another workflow.
 * (import "env" "cleat_signal_workflow") (param i32 i32 i32 i32 i32 i32) (result i64)
 */
@external("env", "cleat_signal_workflow")
export declare function import_cleat_signal_workflow(
  targetRunIdPtr: i32,
  targetRunIdLen: i32,
  signalNamePtr: i32,
  signalNameLen: i32,
  payloadPtr: i32,
  payloadLen: i32,
): i64;

/**
 * 25. cleat_resolve_promise: Resolve a durable promise with a value.
 * (import "env" "cleat_resolve_promise") (param i32 i32 i32 i32) (result i64)
 */
@external("env", "cleat_resolve_promise")
export declare function import_cleat_resolve_promise(
  idPtr: i32,
  idLen: i32,
  valuePtr: i32,
  valueLen: i32,
): i64;

/**
 * 26. cleat_reject_promise: Reject a durable promise with an error.
 * (import "env" "cleat_reject_promise") (param i32 i32 i32 i32) (result i64)
 */
@external("env", "cleat_reject_promise")
export declare function import_cleat_reject_promise(
  idPtr: i32,
  idLen: i32,
  errorPtr: i32,
  errorLen: i32,
): i64;

/**
 * 27. cleat_send: Fire-and-forget durable call to a service.
 * (import "env" "cleat_send") (param i32 i32 i32 i32 i32 i32) (result i64)
 */
@external("env", "cleat_send")
export declare function import_cleat_send(
  svcPtr: i32,
  svcLen: i32,
  opPtr: i32,
  opLen: i32,
  reqPtr: i32,
  reqLen: i32,
): i64;

/**
 * 28. schedule_invoke: Schedule a one-shot delayed invocation.
 * (import "env" "cleat_schedule_invoke") (param i32 i32 i32 i32 i32 i32 i64) (result i64)
 */
@external("env", "cleat_schedule_invoke")
export declare function import_schedule_invoke(
  svcPtr: i32,
  svcLen: i32,
  opPtr: i32,
  opLen: i32,
  reqPtr: i32,
  reqLen: i32,
  delayMs: i64,
): i64;

// There is no import_cleat_register_query_handler here (removed 2026-08-09).
// It recorded a handler name with the host but nothing ever routed an
// external query to it -- see docs/determinism.md, "Why there is no
// RegisterQueryHandler". Use setQueryState instead.

/**
 * 30. cleat_run_detached: Run a function in a detached child workflow.
 * (import "env" "cleat_run_detached") (param i32 i32 i32 i32) (result i64)
 */
@external("env", "cleat_run_detached")
export declare function import_cleat_run_detached(
  namePtr: i32,
  nameLen: i32,
  inputPtr: i32,
  inputLen: i32,
): i64;

/**
 * 31. cleat_set_state: Set a key-value pair in workflow state.
 * (import "env" "cleat_set_state") (param i32 i32 i32 i32) (result i64)
 */
@external("env", "cleat_set_state")
export declare function import_cleat_set_state(
  keyPtr: i32,
  keyLen: i32,
  valPtr: i32,
  valLen: i32,
): i64;

/**
 * 32. cleat_get_state: Get a value from workflow state by key.
 * (import "env" "cleat_get_state") (param i32 i32 i32 i32) (result i64)
 */
@external("env", "cleat_get_state")
export declare function import_cleat_get_state(
  keyPtr: i32,
  keyLen: i32,
  outPtr: i32,
  maxLen: i32,
): i64;

/**
 * 33. cleat_delete_state: Delete a key from workflow state.
 * (import "env" "cleat_delete_state") (param i32 i32) (result i64)
 */
@external("env", "cleat_delete_state")
export declare function import_cleat_delete_state(
  keyPtr: i32,
  keyLen: i32,
): i64;

/**
 * 34. cleat_incr_state: Atomically increment a numeric state value.
 * (import "env" "cleat_incr_state") (param i32 i32 i64) (result i64)
 */
@external("env", "cleat_incr_state")
export declare function import_cleat_incr_state(
  keyPtr: i32,
  keyLen: i32,
  delta: i64,
): i64;

/**
 * 35. cleat_has_state: Check if a key exists in workflow state.
 * (import "env" "cleat_has_state") (param i32 i32) (result i64)
 */
@external("env", "cleat_has_state")
export declare function import_cleat_has_state(
  keyPtr: i32,
  keyLen: i32,
): i64;

/**
 * 36. cleat_list_state: List state keys matching a prefix.
 * (import "env" "cleat_list_state") (param i32 i32 i32 i32) (result i64)
 */
@external("env", "cleat_list_state")
export declare function import_cleat_list_state(
  prefixPtr: i32,
  prefixLen: i32,
  outPtr: i32,
  maxLen: i32,
): i64;

/**
 * 37. cleat_await_all_children: Wait for multiple child workflows.
 * (import "env" "cleat_await_all_children") (param i32 i32 i32 i32) (result i64)
 */
@external("env", "cleat_await_all_children")
export declare function import_cleat_await_all_children(
  runIdsJsonPtr: i32,
  runIdsJsonLen: i32,
  outPtr: i32,
  maxLen: i32,
): i64;

/**
 * 38. cleat_fetch: Make an HTTP request via the host runtime.
 * (import "env" "cleat_fetch") (param i32 i32 i32 i32 i32 i32 i32 i32 i32 i32) (result i64)
 */
@external("env", "cleat_fetch")
export declare function import_cleat_fetch(
  methodPtr: i32,
  methodLen: i32,
  urlPtr: i32,
  urlLen: i32,
  headersPtr: i32,
  headersLen: i32,
  bodyPtr: i32,
  bodyLen: i32,
  respPtr: i32,
  respMaxLen: i32,
): i64;

/**
 * 39b. cleat_acquire_lock: Acquire a concurrency lock.
 * (import "env" "cleat_acquire_lock") (param i32 i32 i64) (result i64)
 */
@external("env", "cleat_acquire_lock")
export declare function import_cleat_acquire_lock(
  keyPtr: i32,
  keyLen: i32,
  ttlMs: i64,
): i64;

/**
 * 39c. cleat_release_lock: Release a concurrency lock.
 * (import "env" "cleat_release_lock") (param i32 i32) (result i64)
 */
@external("env", "cleat_release_lock")
export declare function import_cleat_release_lock(
  keyPtr: i32,
  keyLen: i32,
): i64;

/**
 * 39. schedule_cron: Register a recurring cron-triggered workflow.
 * (import "env" "cleat_schedule_cron") (param i32 i32 i32 i32 i32 i32 i32 i32 i32 i32) (result i64)
 */
@external("env", "cleat_schedule_cron")
export declare function import_schedule_cron(
  workflowNamePtr: i32,
  workflowNameLen: i32,
  cronExprPtr: i32,
  cronExprLen: i32,
  tzPtr: i32,
  tzLen: i32,
  inputPtr: i32,
  inputLen: i32,
  scheduleIdOutPtr: i32,
  scheduleIdOutMax: i32,
): i64;

/**
 * 40. delete_cron: Remove a previously registered cron schedule.
 * (import "env" "cleat_delete_cron") (param i32 i32) (result i64)
 */
@external("env", "cleat_delete_cron")
export declare function import_delete_cron(
  scheduleIdPtr: i32,
  scheduleIdLen: i32,
): i64;

/**
 * 41. list_crons: List all registered cron schedules.
 * (import "env" "cleat_list_crons") (param i32 i32) (result i64)
 */
@external("env", "cleat_list_crons")
export declare function import_list_crons(
  outPtr: i32,
  outMaxLen: i32,
): i64;

/**
 * 42. cleat_call_retry: Server-side retry variant of cleat_call.
 * (import "env" "cleat_call_retry") (param i32 i32 i32 i32 i32 i32 i64 i64 i64 i64 i32 i32 i32 i32) (result i64)
 *
 * Retries happen inside the host; one event is recorded regardless of
 * attempt count.
 */
@external("env", "cleat_call_retry")
export declare function import_cleat_call_retry(
  svcPtr: i32,
  svcLen: i32,
  opPtr: i32,
  opLen: i32,
  reqPtr: i32,
  reqLen: i32,
  maxAttempts: i64,
  initialIntervalMs: i64,
  backoffCoefficient100x: i64,
  maxIntervalMs: i64,
  nonRetryableErrorsPtr: i32,
  nonRetryableErrorsLen: i32,
  respPtr: i32,
  respMaxLen: i32,
): i64;

/**
 * 43. cleat_call_heartbeat: Long-running call with progress updates.
 * (import "env" "cleat_call_heartbeat") (param i32 i32 i32 i32 i32 i32 i64 i32 i32) (result i64)
 *
 * The host sends periodic progress updates; the progress callback is
 * handled at the SDK layer.
 */
@external("env", "cleat_call_heartbeat")
export declare function import_cleat_call_heartbeat(
  svcPtr: i32,
  svcLen: i32,
  opPtr: i32,
  opLen: i32,
  reqPtr: i32,
  reqLen: i32,
  heartbeatIntervalMs: i64,
  respPtr: i32,
  respMaxLen: i32,
): i64;

/**
 * 48. cleat_json_parse: Validate and normalize a JSON string via the host.
 * (import "env" "cleat_json_parse") (param i32 i32 i32 i32) (result i64)
 *
 * Parses the input JSON using the host's encoding/json library and returns
 * a normalized JSON string. Returns errCode=1 on parse error.
 */
@external("env", "cleat_json_parse")
export declare function import_cleat_json_parse(
  jsonPtr: i32,
  jsonLen: i32,
  outPtr: i32,
  outMaxLen: i32,
): i64;

/**
 * 49. cleat_json_stringify: Serialize a JSON value via the host.
 * (import "env" "cleat_json_stringify") (param i32 i32 i32 i32) (result i64)
 *
 * Validates the input JSON string and returns a re-serialized (normalized)
 * JSON string. Returns errCode=1 on parse error.
 */
@external("env", "cleat_json_stringify")
export declare function import_cleat_json_stringify(
  ptr: i32,
  len: i32,
  outPtr: i32,
  outMaxLen: i32,
): i64;

/**
 * cleat_set_scope: (ptr,len x2, ptr,maxLen) -> i64
 * (import "env" "cleat_set_scope")
 */
@external("env", "cleat_set_scope")
export declare function import_cleat_set_scope(
  objTypePtr: i32, objTypeLen: i32,
  instKeyPtr: i32, instKeyLen: i32,
  prevScopePtr: i32, prevScopeMaxLen: i32,
): i64;

/**
 * cleat_get_scope: (ptr,maxLen x2) -> i64
 * (import "env" "cleat_get_scope")
 */
@external("env", "cleat_get_scope")
export declare function import_cleat_get_scope(
  objTypePtr: i32, objTypeMaxLen: i32,
  instKeyPtr: i32, instKeyMaxLen: i32,
): i64;

/**
 * cleat_uuid: (ptr,len, ptr,maxLen) -> i64
 * (import "env" "cleat_uuid")
 */
@external("env", "cleat_uuid")
export declare function import_cleat_uuid(
  seedPtr: i32, seedLen: i32,
  uuidPtr: i32, uuidMaxLen: i32,
): i64;

/**
 * cleat_continue_as_new_versioned: (ptr,len,i32) -> i64
 * (import "env" "cleat_continue_as_new_versioned")
 */
@external("env", "cleat_continue_as_new_versioned")
export declare function import_cleat_continue_as_new_versioned(
  inputPtr: i32, inputLen: i32,
  newVersion: i32,
): i64;

/**
 * cleat_side_effect: (ptr,len, ptr,maxLen) -> i64
 * (import "env" "cleat_side_effect")
 */
@external("env", "cleat_side_effect")
export declare function import_cleat_side_effect(
  resultPtr: i32, resultLen: i32,
  outPtr: i32, outMaxLen: i32,
): i64;

/**
 * cleat_poll_child: (ptr,len, ptr,maxLen) -> i64
 * (import "env" "cleat_poll_child")
 */
@external("env", "cleat_poll_child")
export declare function import_cleat_poll_child(
  runIdPtr: i32, runIdLen: i32,
  resultPtr: i32, resultMaxLen: i32,
): i64;

/**
 * cleat_await_any_child: (ptr,len, ptr,maxLen) -> i64
 * (import "env" "cleat_await_any_child")
 */
@external("env", "cleat_await_any_child")
export declare function import_cleat_await_any_child(
  runIdsPtr: i32, runIdsLen: i32,
  resultPtr: i32, resultMaxLen: i32,
): i64;

/**
 * plugin_call_streaming: (ptr,len x3, ptr,maxLen) -> i64
 * (import "env" "plugin_call_streaming")
 */
@external("env", "plugin_call_streaming")
export declare function import_plugin_call_streaming(
  pluginNamePtr: i32, pluginNameLen: i32,
  functionNamePtr: i32, functionNameLen: i32,
  inputPtr: i32, inputLen: i32,
  responsePtr: i32, responseMaxLen: i32,
): i64;

// ═══════════════════════════════════════════════
// High-level result types for HostCalls methods
// ═══════════════════════════════════════════════

/**
 * Generic result type representing either a success value or an error
 * message. Used by HostCalls methods that can fail with a descriptive
 * error string.
 */
export class DurableResult<T> {
  constructor(
    /** The success value. Only meaningful when `error` is null. */
    public readonly value: T,
    /** Error message, or null on success. */
    public readonly error: string | null,
  ) {}

  /** Returns true when this result carries an error. */
  get isError(): bool {
    return this.error !== null;
  }
}

/** Outcome of a `cleatCall` operation. */
export class CleatCallOutcome {
  constructor(
    /** Response JSON from the service call. Empty on error. */
    public readonly response: string,
    /** Error message from the host, or null on success. */
    public readonly error: string | null,
    /** Structured call error code (reserved for future use). */
    public readonly callErrorCode: u32,
  ) {}

  /** Returns true when this outcome carries an error. */
  get isError(): bool {
    return this.error !== null;
  }
}

/** Outcome of a plugin_call operation. */
export class PluginCallOutcome {
  constructor(
    /** Response JSON from the plugin. Empty on error. */
    public readonly response: string,
    /** Error message, or null on success. */
    public readonly error: string | null,
    /** Structured call error code. */
    public readonly callErrorCode: u32,
  ) {}
  get isError(): bool { return this.error !== null; }
}

/** Result of `pollCancellation`. */
export class CancellationStatus {
  constructor(
    /** Whether cancellation has been requested. */
    public readonly cancelled: bool,
    /** Cancellation reason string. Empty if not cancelled. */
    public readonly reason: string,
  ) {}
}

/** Result of `pollSignal`. */
export class PollSignalOutcome {
  constructor(
    /** Signal payload JSON. Empty if not found or on error. */
    public readonly payload: string,
    /** Whether the signal was found. */
    public readonly found: bool,
    /** Error message, or null on success. */
    public readonly error: string | null,
  ) {}

  /** Returns true when this outcome carries an error. */
  get isError(): bool {
    return this.error !== null;
  }
}

/** Result of `awaitSignals`. */
export class AwaitSignalsOutcome {
  constructor(
    /** Name of the received signal. Empty on timeout or error. */
    public readonly signalName: string,
    /** Signal payload JSON. Empty on timeout or error. */
    public readonly payload: string,
    /** Whether the wait timed out without receiving any matching signal. */
    public readonly timedOut: bool,
    /** Error message, or null on success. */
    public readonly error: string | null,
  ) {}

  /** Returns true when this outcome carries an error. */
  get isError(): bool {
    return this.error !== null;
  }
}

/** Result of `createPromise`. */
export class PromiseResult {
  constructor(
    /** The promise ID string. Empty on error. */
    public readonly value: string,
    /** Error message, or null on success. */
    public readonly error: string | null,
  ) {}

  /** Returns true when this result carries an error. */
  get isError(): bool {
    return this.error !== null;
  }
}

/** Result of `awaitPromise`. */
export class AwaitPromiseOutcome {
  constructor(
    /** The resolved promise value. Empty on timeout or error. */
    public readonly value: string,
    /** Whether the wait timed out. */
    public readonly timedOut: bool,
    /** Error message, or null on success. */
    public readonly error: string | null,
  ) {}

  /** Returns true when this outcome carries an error. */
  get isError(): bool {
    return this.error !== null;
  }
}

/** Result of a cleatFetch HTTP request. */
export class FetchResult {
  constructor(
    /** HTTP status code (200, 404, etc.). 0 on error. */
    public readonly statusCode: i32,
    /** Response headers as a JSON string (e.g., '{"content-type":"application/json"}'). */
    public readonly headers: string,
    /** Response body string. */
    public readonly body: string,
    /** Error message, or null on success. */
    public readonly error: string | null,
  ) {}

  /** Returns true when this result carries an error. */
  get isError(): bool {
    return this.error !== null;
  }
}

// ═══════════════════════════════════════════════
// HostCalls wrapper
// ═══════════════════════════════════════════════

/**
 * High-level AssemblyScript wrapper around the 19 cleat WASM host function
 * imports.
 *
 * Each method handles string I/O (encode input strings to memory, decode
 * output strings from memory), calls the raw import, and decodes the
 * bit-packed result into an idiomatic AssemblyScript return value.
 *
 * Usage:
 * ```ts
 * let host = new HostCalls();
 * let outcome = host.cleatCall("payment", "charge", `{"amount": 100}`);
 * if (outcome.isError) {
 *   host.log("payment failed: " + outcome.error);
 * }
 * ```
 *
 * <strong>Timeout convention:</strong> This SDK accepts timeout values in
 * <strong>seconds</strong> for the primary API methods (e.g.,
 * {@code awaitSignals}, {@code awaitPromise}, {@code cleatSleep}) and
 * <strong>milliseconds</strong> for the {@code *Ms} variants (e.g.,
 * {@code awaitSignalsMs}, {@code cleatSleepMs}). Prefer the seconds-based
 * primary methods for readability. The underlying host ABI always uses
 * milliseconds; the conversion is handled internally.
 *
 * Mirrors Rust SDK `crates/cleat-sdk/src/host_calls.rs` and
 * Go `durable.HostCalls` interface.
 */
/**
 * Options for starting a child workflow.
 */
export class ChildWorkflowOptions {
  /**
   * Explicit workflow definition version (0 = use parent's version).
   */
  version: i32 = 0;
  /**
   * 0 = highest priority; lower numbers are picked first.
   */
  priority: i32 = 0;

  constructor(version: i32 = 0, priority: i32 = 0) {
    this.version = version;
    this.priority = priority;
  }
}

export class HostCalls {
  /** Memory helper for string I/O in linear memory. */
  protected memory: Memory;

  /** Current scope prefix for virtual object state operations. */
  private _scopePrefix: string = "";

  /**
   * @param memory - Optional Memory instance. A default one is created if
   *                 not provided.
   */
  constructor(memory: Memory = new Memory()) {
    this.memory = memory;
  }

  /**
   * Write a string to the scratch buffer with bounds checking.
   *
   * Throws a descriptive error if the string does not fit in the remaining
   * buffer space, preventing silent memory corruption from buffer overflow.
   *
   * @param offset    - Write offset in the scratch buffer.
   * @param remaining - Remaining bytes available at offset.
   * @param s         - String to write.
   * @param label     - Human-readable label for error messages.
   * @returns The number of bytes written.
   */
  private writeScratch(offset: usize, remaining: i32, s: string, label: string): i32 {
    if (remaining <= 0) {
      throw new Error(
        "scratch buffer overflow: no space remaining for '" + label + "'",
      );
    }
    let byteLen: i32 = String.UTF8.byteLength(s) as i32;
    if (byteLen > remaining) {
      throw new Error(
        "scratch buffer overflow: '" +
          label +
          "' requires " +
          byteLen.toString() +
          " bytes but only " +
          remaining.toString() +
          " remain",
      );
    }
    return this.memory.writeString(offset, remaining, s);
  }

  // ────────────────────────────────────────────
  // 1. cleat_call
  // ────────────────────────────────────────────

  /**
   * Make a recorded API call to an external service (timeout in seconds).
   *
   * Service, operation, and request JSON are encoded to the scratch buffer,
   * the host call is made, and the response is read from the output buffer.
   *
   * @param service        - Service name (e.g., "payment", "email").
   * @param operation      - Operation name (e.g., "charge", "send").
   * @param requestJson    - Request payload as a JSON string.
   * @param timeoutSeconds - Optional per-call timeout in seconds (0 = no timeout).
   * @returns The call outcome containing response JSON or error details.
   */
  cleatCall(service: string, operation: string, requestJson: string, timeoutSeconds: i64 = 0): CleatCallOutcome {
    return this.cleatCallMs(service, operation, requestJson, timeoutSeconds * 1000);
  }

  /**
   * Make a recorded API call to an external service (timeout in milliseconds).
   *
   * Service, operation, and request JSON are encoded to the scratch buffer,
   * the host call is made, and the response is read from the output buffer.
   *
   * @param service      - Service name (e.g., "payment", "email").
   * @param operation    - Operation name (e.g., "charge", "send").
   * @param requestJson  - Request payload as a JSON string.
   * @param timeoutMs    - Optional per-call timeout in milliseconds (0 = no timeout).
   * @returns The call outcome containing response JSON or error details.
   */
  cleatCallMs(service: string, operation: string, requestJson: string, timeoutMs: i64 = 0): CleatCallOutcome {
    // Encode input strings sequentially into the scratch buffer
    let svcLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, service);
    let opOffset: usize = SCRATCH_BASE + svcLen;
    let remaining: i32 = OUT_BUF_SIZE - svcLen;
    let opLen: i32 = this.writeScratch(opOffset, remaining, operation, "operation");
    let reqOffset: usize = opOffset + opLen;
    remaining -= opLen;
    let reqLen: i32 = this.writeScratch(reqOffset, remaining, requestJson, "requestJson");

    // Call the host import
    let result: i64 = import_cleat_call(
      SCRATCH_BASE as i32,
      svcLen,
      opOffset as i32,
      opLen,
      reqOffset as i32,
      reqLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    // Decode the packed result
    let decoded = decodeCallResult(result);
    let responseLen: i32 = decoded.responseLen as i32;

    // On error, the output buffer contains an error message
    if (decoded.errCode !== 0) {
      let errMsg: string =
        responseLen > 0 ? this.memory.readString(OUTPUT_OFFSET, responseLen) : "cleat_call(service='" + service + "', operation='" + operation + "') failed: unknown error (code " + decoded.errCode.toString() + ")";
      return new CleatCallOutcome("", errMsg, decoded.callErrorCode);
    }

    // Success: read the response
    let resp: string =
      responseLen > 0 ? this.memory.readString(OUTPUT_OFFSET, responseLen) : "";
    return new CleatCallOutcome(resp, null, 0);
  }

  // ────────────────────────────────────────────
  // 1b. cleat_call_retry
  // ────────────────────────────────────────────

  /**
   * Make a recorded API call with server-side retry.
   *
   * Retries happen inside the host; one event is recorded regardless of
   * attempt count. This is more efficient than client-side retry loops.
   *
   * @param service                  - Service name (e.g., "payment", "email").
   * @param operation                - Operation name (e.g., "charge", "send").
   * @param requestJson              - Request payload as a JSON string.
   * @param maxAttempts              - Maximum number of retry attempts (default 3).
   * @param initialIntervalMs        - Initial retry interval in milliseconds (default 1000).
   * @param backoffCoefficient100x   - Backoff coefficient scaled by 100x, e.g., 200 = 2.0x (default 200).
   * @param maxIntervalMs            - Maximum retry interval in milliseconds (default 60000).
   * @param nonRetryableErrors       - JSON array of non-retryable error codes, e.g., '["INVALID_ARGUMENT"]' (default "[]").
   * @returns The call outcome containing response JSON or error details.
   */
  cleatCallRetry(
    service: string,
    operation: string,
    requestJson: string,
    maxAttempts: i64 = 3,
    initialIntervalMs: i64 = 1000,
    backoffCoefficient100x: i64 = 200,
    maxIntervalMs: i64 = 60000,
    nonRetryableErrors: string = "[]",
  ): CleatCallOutcome {
    // Encode input strings sequentially into the scratch buffer
    let svcLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, service);
    let opOffset: usize = SCRATCH_BASE + svcLen;
    let remaining: i32 = OUT_BUF_SIZE - svcLen;
    let opLen: i32 = this.writeScratch(opOffset, remaining, operation, "operation");
    let reqOffset: usize = opOffset + opLen;
    remaining -= opLen;
    let reqLen: i32 = this.writeScratch(reqOffset, remaining, requestJson, "requestJson");
    let nreOffset: usize = reqOffset + reqLen;
    remaining -= reqLen;
    let nreLen: i32 = this.writeScratch(nreOffset, remaining, nonRetryableErrors, "nonRetryableErrors");

    // Call the host import
    let result: i64 = import_cleat_call_retry(
      SCRATCH_BASE as i32,
      svcLen,
      opOffset as i32,
      opLen,
      reqOffset as i32,
      reqLen,
      maxAttempts,
      initialIntervalMs,
      backoffCoefficient100x,
      maxIntervalMs,
      nreOffset as i32,
      nreLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    // Decode the packed result (same bit layout as cleat_call)
    let decoded = decodeCallResult(result);
    let responseLen: i32 = decoded.responseLen as i32;

    // On error, the output buffer contains an error message
    if (decoded.errCode !== 0) {
      let errMsg: string =
        responseLen > 0
          ? this.memory.readString(OUTPUT_OFFSET, responseLen)
          : "cleatCallRetry(service='" + service + "', operation='" + operation + "') failed: unknown error (code " + decoded.errCode.toString() + ")";
      return new CleatCallOutcome("", errMsg, decoded.callErrorCode);
    }

    // Success: read the response
    let resp: string =
      responseLen > 0 ? this.memory.readString(OUTPUT_OFFSET, responseLen) : "";
    return new CleatCallOutcome(resp, null, 0);
  }

  // ────────────────────────────────────────────
  // 1c. cleat_call_heartbeat
  // ────────────────────────────────────────────

  /**
   * Make a long-running API call with progress updates from the host.
   *
   * The host sends periodic heartbeat/progress updates; the progress
   * callback is handled at the SDK layer. The workflow suspends until
   * the call completes.
   *
   * @param service              - Service name (e.g., "payment", "email").
   * @param operation            - Operation name (e.g., "charge", "send").
   * @param requestJson          - Request payload as a JSON string.
   * @param heartbeatIntervalMs  - Interval between progress updates in milliseconds.
   * @returns The call outcome containing response JSON or error details.
   */
  cleatCallHeartbeat(
    service: string,
    operation: string,
    requestJson: string,
    heartbeatIntervalMs: i64,
  ): CleatCallOutcome {
    // Encode input strings sequentially into the scratch buffer
    let svcLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, service);
    let opOffset: usize = SCRATCH_BASE + svcLen;
    let remaining: i32 = OUT_BUF_SIZE - svcLen;
    let opLen: i32 = this.writeScratch(opOffset, remaining, operation, "operation");
    let reqOffset: usize = opOffset + opLen;
    remaining -= opLen;
    let reqLen: i32 = this.writeScratch(reqOffset, remaining, requestJson, "requestJson");

    // Call the host import
    let result: i64 = import_cleat_call_heartbeat(
      SCRATCH_BASE as i32,
      svcLen,
      opOffset as i32,
      opLen,
      reqOffset as i32,
      reqLen,
      heartbeatIntervalMs,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    // Decode the packed result (same bit layout as cleat_call)
    let decoded = decodeCallResult(result);
    let responseLen: i32 = decoded.responseLen as i32;

    // On error, the output buffer contains an error message
    if (decoded.errCode !== 0) {
      let errMsg: string =
        responseLen > 0
          ? this.memory.readString(OUTPUT_OFFSET, responseLen)
          : "cleatCallHeartbeat(service='" + service + "', operation='" + operation + "') failed: unknown error (code " + decoded.errCode.toString() + ")";
      return new CleatCallOutcome("", errMsg, decoded.callErrorCode);
    }

    // Success: read the response
    let resp: string =
      responseLen > 0 ? this.memory.readString(OUTPUT_OFFSET, responseLen) : "";
    return new CleatCallOutcome(resp, null, 0);
  }

  // ────────────────────────────────────────────
  // 2. cleat_sleep
  // ────────────────────────────────────────────

  /**
   * Suspend workflow execution for a duration (seconds).
   *
   * On fresh execution, returns `true` to signal that the workflow should
   * suspend. On replay, returns `false` (the sleep already completed).
   *
   * @param timeoutSeconds - Sleep duration in seconds.
   * @returns `true` if the workflow should suspend, `false` if completed.
   */
  cleatSleep(timeoutSeconds: i64): bool {
    return this.cleatSleepMs(timeoutSeconds * 1000);
  }

  /**
   * Suspend workflow execution for a duration (milliseconds).
   *
   * On fresh execution, returns `true` to signal that the workflow should
   * suspend. On replay, returns `false` (the sleep already completed).
   *
   * @param timeoutMs - Sleep duration in milliseconds.
   * @returns `true` if the workflow should suspend, `false` if completed.
   */
  cleatSleepMs(timeoutMs: i64): bool {
    let result: i64 = import_cleat_sleep(timeoutMs);
    let decoded = decodeSleepResult(result);
    let shouldSuspend: bool = decoded.status === 1;
    if (shouldSuspend) {
      setWorkflowSuspended();
    }
    return shouldSuspend;
  }

  // ────────────────────────────────────────────
  // 3. now
  // ────────────────────────────────────────────

  /**
   * Get the current wall-clock time.
   *
   * @returns Current time in milliseconds since Unix epoch.
   */
  now(): i64 {
    return import_cleat_now();
  }

  // ────────────────────────────────────────────
  // 4. random
  // ────────────────────────────────────────────

  /**
   * Get a deterministic random value.
   *
   * The same value is returned on replay, ensuring determinism.
   *
   * @returns A deterministic i64 value.
   */
  random(): i64 {
    return import_cleat_random();
  }

  // ────────────────────────────────────────────
  // 5. log
  // ────────────────────────────────────────────

  /**
   * Log a message to the host runtime.
   *
   * @param message - The message to log.
   */
  log(message: string): void {
    let msgLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, message);
    import_cleat_log(SCRATCH_BASE as i32, msgLen);
  }

  // ────────────────────────────────────────────
  // 6. version
  // ────────────────────────────────────────────

  /**
   * Get the workflow definition version.
   *
   * @returns The current workflow version as a 32-bit integer.
   */
  version(): i32 {
    return import_cleat_version() as i32;
  }

  // ────────────────────────────────────────────
  // 7. minVersion
  // ────────────────────────────────────────────

  /**
   * Get the minimum supported version for this workflow definition.
   *
   * @returns The minimum version as a 32-bit integer.
   */
  minVersion(): i32 {
    return import_cleat_min_version() as i32;
  }

  // ────────────────────────────────────────────
  // 8. defer
  // ────────────────────────────────────────────

  /**
   * Record with the host that a cleanup action exists.
   *
   * This registers a DESCRIPTION and nothing else. The host stores it in the
   * workflow's deferrals map; **no code anywhere runs it**, because there is
   * no body to run. This doc comment said "register cleanup to run on workflow
   * exit" until IMPROVEMENT-PLAN §3.73, which was not true of any
   * AssemblyScript workflow ever written.
   *
   * Use {@link deferFunc} for cleanup that actually runs.
   *
   * @param description - Human-readable description of the deferred action.
   * @returns A DurableResult containing the defer ID on success.
   */
  defer(description: string): DurableResult<string> {
    let descLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, description);

    let result: i64 = import_cleat_defer(
      SCRATCH_BASE as i32,
      descLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    let decoded = decodeSimpleResult(result);

    if (decoded.errCode !== 0) {
      return new DurableResult<string>(
        "",
        "defer(description='" + description + "') failed: " + errorCodeName(decoded.errCode) + " (code " + decoded.errCode.toString() + ")",
      );
    }

    let deferId: string = this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32);
    return new DurableResult<string>(deferId, null);
  }

  // ────────────────────────────────────────────
  // 9. pollCancellation
  // ────────────────────────────────────────────

  /**
   * Check if workflow cancellation has been requested.
   *
   * @returns The cancellation status, including any reason string.
   */
  pollCancellation(): CancellationStatus {
    let result: i64 = import_cleat_poll_cancellation(OUTPUT_OFFSET as i32, OUT_BUF_SIZE);
    let decoded = decodePollCancellationResult(result);

    let reason: string =
      decoded.cancelled && decoded.reasonLen > 0
        ? this.memory.readString(OUTPUT_OFFSET, decoded.reasonLen as i32)
        : "";

    return new CancellationStatus(decoded.cancelled, reason);
  }

  // ────────────────────────────────────────────
  // 10. pollSignal
  // ────────────────────────────────────────────

  /**
   * Poll for a specific pending signal.
   *
   * @param name - The signal name to poll for.
   * @returns The signal outcome with payload and found status.
   */
  pollSignal(name: string): PollSignalOutcome {
    let nameLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, name);

    let result: i64 = import_cleat_poll_signal(
      SCRATCH_BASE as i32,
      nameLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    let decoded = decodePollSignalResult(result);

    if (decoded.errCode !== 0) {
      return new PollSignalOutcome(
        "",
        false,
        "pollSignal(name='" + name + "') failed: " + errorCodeName(decoded.errCode) + " (code " + decoded.errCode.toString() + ")",
      );
    }

    let payload: string =
      decoded.found && decoded.payloadLen > 0
        ? this.memory.readString(OUTPUT_OFFSET, decoded.payloadLen as i32)
        : "";

    return new PollSignalOutcome(payload, decoded.found, null);
  }

  // ────────────────────────────────────────────
  // 11. continueAsNew
  // ────────────────────────────────────────────

  /**
   * Start a new workflow run with fresh input (history compaction).
   *
   * After this call, the workflow should return the suspension sentinel
   * to let the host restart it with the new input.
   *
   * @param inputJson - New input JSON for the restarted workflow.
   * @returns An error message on failure, or `null` on success.
   */
  continueAsNew(inputJson: string): string | null {
    // Refused from inside a defer body -- IMPROVEMENT-PLAN 3.35 phase 4.
    // Measured 2026-09-02 before this check: the host recorded a
    // `continue_as_new` event AND the wrapper went on to report the workflow's
    // already-decided result, so one history carried two contradictory
    // terminal facts. The worker stores `done` and the continuation silently
    // never happens.
    if (isInDeferPhase()) {
      return "continueAsNew() is not allowed from a defer body: the workflow's " +
        "result is already decided by the time defers run, so the continuation " +
        "would be recorded and never taken (IMPROVEMENT-PLAN 3.35 phase 4).";
    }

    let inputLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, inputJson);

    let result: i64 = import_cleat_continue_as_new(SCRATCH_BASE as i32, inputLen);
    let decoded = decodeSimpleResult(result);

    if (decoded.errCode !== 0) {
      return "continueAsNew() failed: " + errorCodeName(decoded.errCode) + " (code " + decoded.errCode.toString() + ")";
    }

    return null;
  }

  // ────────────────────────────────────────────
  // 11a. continueAsNewVersioned
  // ────────────────────────────────────────────

  /**
   * Start a new workflow run with fresh input and explicit version.
   *
   * After this call, the workflow should return the suspension sentinel
   * to let the host restart it with the new input and version.
   *
   * @param inputJson  - New input JSON for the restarted workflow.
   * @param newVersion - Explicit workflow version for the restarted run.
   * @returns An error message on failure, or `null` on success.
   */
  continueAsNewVersioned(inputJson: string, newVersion: i32): string | null {
    // Refused from inside a defer body -- IMPROVEMENT-PLAN 3.35 phase 4.
    // Measured 2026-09-02 before this check: the host recorded a
    // `continue_as_new` event AND the wrapper went on to report the workflow's
    // already-decided result, so one history carried two contradictory
    // terminal facts. The worker stores `done` and the continuation silently
    // never happens.
    if (isInDeferPhase()) {
      return "continueAsNewVersioned() is not allowed from a defer body: the workflow's " +
        "result is already decided by the time defers run, so the continuation " +
        "would be recorded and never taken (IMPROVEMENT-PLAN 3.35 phase 4).";
    }

    let inputLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, inputJson);

    let result: i64 = import_cleat_continue_as_new_versioned(
      SCRATCH_BASE as i32,
      inputLen,
      newVersion,
    );
    let decoded = decodeSimpleResult(result);

    if (decoded.errCode !== 0) {
      return "continueAsNewVersioned() failed: " + errorCodeName(decoded.errCode) + " (code " + decoded.errCode.toString() + ")";
    }

    return null;
  }

  // ────────────────────────────────────────────
  // 12. childWorkflow
  // ────────────────────────────────────────────

  /**
   * Start a child workflow instance.
   *
   * @param name      - Child workflow definition name.
   * @param inputJson - Input JSON for the child workflow.
   * @returns A DurableResult containing the child run ID on success.
   */
  childWorkflow(name: string, inputJson: string): DurableResult<string> {
    let nameLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, name);
    let inputOffset: usize = SCRATCH_BASE + nameLen;
    let remaining: i32 = OUT_BUF_SIZE - nameLen;
    let inputLen: i32 = this.writeScratch(inputOffset, remaining, inputJson, "inputJson");

    let result: i64 = import_cleat_child_workflow(
      SCRATCH_BASE as i32,
      nameLen,
      inputOffset as i32,
      inputLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    let decoded = decodeSimpleResult(result);

    if (decoded.errCode !== 0) {
      return new DurableResult<string>(
        "",
        "childWorkflow(name='" + name + "') failed: " + errorCodeName(decoded.errCode) + " (code " + decoded.errCode.toString() + ")",
      );
    }

    let runId: string = this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32);
    return new DurableResult<string>(runId, null);
  }

  /**
   * Start a child workflow instance with explicit version and priority options.
   *
   * @param name      - Child workflow definition name.
   * @param inputJson - Input JSON for the child workflow.
   * @param options   - ChildWorkflowOptions (version, priority, etc.).
   * @returns A DurableResult containing the child run ID on success.
   */
  childWorkflowWithOptions(name: string, inputJson: string, options: ChildWorkflowOptions = new ChildWorkflowOptions()): DurableResult<string> {
    let nameLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, name);
    let inputOffset: usize = SCRATCH_BASE + nameLen;
    let remaining: i32 = OUT_BUF_SIZE - nameLen;
    let inputLen: i32 = this.writeScratch(inputOffset, remaining, inputJson, "inputJson");

    let result: i64 = import_cleat_child_workflow_with_options(
      SCRATCH_BASE as i32,
      nameLen,
      inputOffset as i32,
      inputLen,
      options.version as i64,
      options.priority as i64,
      0,
      0,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    let decoded = decodeSimpleResult(result);

    if (decoded.errCode !== 0) {
      return new DurableResult<string>(
        "",
        "childWorkflowWithOptions(name='" + name + "') failed: " + errorCodeName(decoded.errCode) + " (code " + decoded.errCode.toString() + ")",
      );
    }

    let runId: string = this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32);
    return new DurableResult<string>(runId, null);
  }

  // ────────────────────────────────────────────
  // 13. awaitChild
  // ────────────────────────────────────────────

  /**
   * Wait for a child workflow to complete.
   *
   * If the child is not yet complete, the workflow should suspend by
   * returning the suspension sentinel.
   *
   * @param runId - The child workflow run ID.
   * @returns A DurableResult containing the child's result JSON on success.
   */
  awaitChild(runId: string): DurableResult<string> {
    let runIdLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, runId);

    let result: i64 = import_cleat_await_child(
      SCRATCH_BASE as i32,
      runIdLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    // Check for suspend signal: host sets bit 62 when child not yet complete
    if ((result as u64 & (1 << 62)) != 0) {
      setWorkflowSuspended();
      return new DurableResult<string>("", "");
    }

    let decoded = decodeSimpleResult(result);

    if (decoded.errCode !== 0) {
      return new DurableResult<string>(
        "",
        "awaitChild(runId='" + runId + "') failed: " + errorCodeName(decoded.errCode) + " (code " + decoded.errCode.toString() + ")",
      );
    }

    let childResult: string = this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32);
    return new DurableResult<string>(childResult, null);
  }

  // ────────────────────────────────────────────
  // 13a. pollChild — non-blocking child poll
  // ────────────────────────────────────────────

  /**
   * Poll a child workflow for completion without suspending.
   *
   * Non-blocking — returns immediately with the result if the child
   * has completed, or an empty result if not yet complete.
   *
   * @param runId - The child workflow run ID.
   * @returns A DurableResult containing the child's result JSON on success.
   */
  pollChild(runId: string): DurableResult<string> {
    let runIdLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, runId);

    let result: i64 = import_cleat_poll_child(
      SCRATCH_BASE as i32,
      runIdLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    // pollChild is non-blocking, no suspend check needed
    let decoded = decodeSimpleResult(result);

    if (decoded.errCode !== 0) {
      return new DurableResult<string>(
        "",
        "pollChild(runId='" + runId + "') failed: " + errorCodeName(decoded.errCode) + " (code " + decoded.errCode.toString() + ")",
      );
    }

    let childResult: string = decoded.extra > 0
      ? this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32)
      : "";
    return new DurableResult<string>(childResult, null);
  }

  // ────────────────────────────────────────────
  // 13b. awaitAnyChild — wait for first child to complete
  // ────────────────────────────────────────────

  /**
   * Wait for the first of the given child workflows to complete.
   *
   * If none of the children have completed yet, the workflow
   * suspends by returning the suspension sentinel.
   *
   * @param runIds - JSON array of child workflow run IDs.
   * @returns The result from the first completed child, or null on
   *          suspend or error.
   */
  awaitAnyChild(runIds: string): string | null {
    let runIdsLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, runIds);

    let result: i64 = import_cleat_await_any_child(
      SCRATCH_BASE as i32,
      runIdsLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    // Check for suspend sentinel
    if ((result as u64) === (SUSPEND_SENTINEL as u64)) {
      setWorkflowSuspended();
      return null;
    }

    let decoded = decodeSimpleResult(result);
    if (decoded.errCode !== 0 || decoded.extra === 0) {
      return null;
    }
    return this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32);
  }

  // ────────────────────────────────────────────
  // 14. awaitSignals
  // ────────────────────────────────────────────

  /**
   * Wait for one or more external signals, with a timeout in seconds.
   *
   * Signal names are passed as a JSON array string, e.g.,
   * `'["payment_received","order_cancelled"]'`.
   *
   * The scratch buffer is split: the first portion holds the input
   * (signal names JSON), and the remainder serves as the payload output
   * buffer. The output buffer at `OUTPUT_OFFSET` holds the received
   * signal name.
   *
   * @param namesJson     - JSON array of signal names to wait for.
   * @param timeoutSeconds - Timeout in seconds.
   * @returns The outcome with signal name, payload, and timeout status.
   */
  awaitSignals(namesJson: string, timeoutSeconds: i64): AwaitSignalsOutcome {
    return this.awaitSignalsMs(namesJson, timeoutSeconds * 1000);
  }

  /**
   * Wait for one or more external signals, with a timeout in milliseconds.
   *
   * Signal names are passed as a JSON array string, e.g.,
   * `'["payment_received","order_cancelled"]'`.
   *
   * The scratch buffer is split: the first portion holds the input
   * (signal names JSON), and the remainder serves as the payload output
   * buffer. The output buffer at `OUTPUT_OFFSET` holds the received
   * signal name.
   *
   * @param namesJson - JSON array of signal names to wait for.
   * @param timeoutMs - Timeout in milliseconds.
   * @returns The outcome with signal name, payload, and timeout status.
   */
  awaitSignalsMs(namesJson: string, timeoutMs: i64): AwaitSignalsOutcome {
    // Write the signal names JSON into the lower portion of the scratch buffer
    let namesLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE / 2, namesJson);

    // The upper half of the scratch buffer is used for the payload output
    let payloadOffset: usize = SCRATCH_BASE + OUT_BUF_SIZE / 2;
    let payloadMaxLen: i32 = OUT_BUF_SIZE / 2;

    let result: i64 = import_cleat_await_signals(
      SCRATCH_BASE as i32,
      namesLen,
      timeoutMs,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
      payloadOffset as i32,
      payloadMaxLen,
    );

    let decoded = decodeAwaitSignalsResult(result);

    if (decoded.errCode !== 0) {
      return new AwaitSignalsOutcome(
        "",
        "",
        false,
        "awaitSignalsMs() failed: " + errorCodeName(decoded.errCode) + " (code " + decoded.errCode.toString() + ")",
      );
    }

    let sigName: string =
      decoded.sigNameLen > 0
        ? this.memory.readString(OUTPUT_OFFSET, decoded.sigNameLen as i32)
        : "";

    let payload: string =
      !decoded.timedOut && decoded.payloadLen > 0
        ? this.memory.readString(payloadOffset, decoded.payloadLen as i32)
        : "";

    return new AwaitSignalsOutcome(sigName, payload, decoded.timedOut, null);
  }

  // ────────────────────────────────────────────
  // 15. setQueryState
  // ────────────────────────────────────────────

  /**
   * Set a key-value pair in the workflow's query state.
   *
   * Query state can be read by external clients while the workflow is
   * running, enabling interactive queries.
   *
   * @param key   - Query state key.
   * @param value - Query state value.
   */
  setQueryState(key: string, value: string): void {
    let keyLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, key);
    let valOffset: usize = SCRATCH_BASE + keyLen;
    let remaining: i32 = OUT_BUF_SIZE - keyLen;
    let valLen: i32 = this.writeScratch(valOffset, remaining, value, "value");

    import_set_query_state(SCRATCH_BASE as i32, keyLen, valOffset as i32, valLen);
  }

  // ────────────────────────────────────────────
  // Note: getQueryState is not available
  // ────────────────────────────────────────────

  /**
   * NOTE: There is no corresponding `get_query_state` host import in the
   * current ABI specification (ABI.md), and none is needed. Query state is
   * write-only from the workflow's perspective by design: setQueryState
   * persists it to the database, and external clients read it directly from
   * there via `GET /api/workflows/:id/query?key=X` -- without needing the
   * WASM guest to be running or to do anything on the read path.
   *
   * This is not `registerQueryHandler` (there is no such thing in this SDK;
   * see docs/determinism.md, "Why there is no RegisterQueryHandler" -- it
   * was removed 2026-08-09 because nothing ever routed an external query to
   * a registered handler). setQueryState/GetQueryState is the real, wired
   * mechanism, and it needs no host-side "get" import because the read
   * never goes through the guest at all.
   *
   * If a future ABI version adds a `cleat_get_query_state` import (e.g. to
   * let a workflow read its own previously-set query state), this section
   * should be updated to include a corresponding `getQueryState` method.
   */

  // ────────────────────────────────────────────
  // 16. createPromise
  // ────────────────────────────────────────────

  /**
   * Create a new durable promise with the given name.
   *
   * The host allocates a promise ID and writes it to the output buffer.
   *
   * @param name - The promise name.
   * @returns A PromiseResult containing the promise ID on success.
   */
  createPromise(name: string): PromiseResult {
    let nameLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, name);

    let result: i64 = import_cleat_create_promise(
      SCRATCH_BASE as i32,
      nameLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    let decoded = decodeSimpleResult(result);

    if (decoded.errCode !== 0) {
      return new PromiseResult(
        "",
        "createPromise(name='" + name + "') failed: " + errorCodeName(decoded.errCode) + " (code " + decoded.errCode.toString() + ")",
      );
    }

    let promiseId: string = this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32);
    return new PromiseResult(promiseId, null);
  }

  // ────────────────────────────────────────────
  // 17. awaitPromise
  // ────────────────────────────────────────────

  /**
   * Wait for a durable promise to resolve, with a timeout in seconds.
   *
   * If the promise is not yet resolved when the timeout elapses, the
   * workflow should suspend by returning the suspension sentinel.
   *
   * @param id             - The promise ID to wait for.
   * @param timeoutSeconds - Timeout in seconds.
   * @returns The outcome with the resolved value and timeout status.
   */
  awaitPromise(id: string, timeoutSeconds: i64): AwaitPromiseOutcome {
    return this.awaitPromiseMs(id, timeoutSeconds * 1000);
  }

  /**
   * Wait for a durable promise to resolve, with a timeout in milliseconds.
   *
   * If the promise is not yet resolved when the timeout elapses, the
   * workflow should suspend by returning the suspension sentinel.
   *
   * @param id        - The promise ID to wait for.
   * @param timeoutMs - Timeout in milliseconds.
   * @returns The outcome with the resolved value and timeout status.
   */
  awaitPromiseMs(id: string, timeoutMs: i64): AwaitPromiseOutcome {
    let idLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, id);

    let result: i64 = import_cleat_await_promise(
      SCRATCH_BASE as i32,
      idLen,
      timeoutMs,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    let decoded = decodeAwaitPromiseResult(result);

    if (decoded.errCode !== 0) {
      return new AwaitPromiseOutcome(
        "",
        false,
        "awaitPromiseMs(id='" + id + "') failed: " + errorCodeName(decoded.errCode) + " (code " + decoded.errCode.toString() + ")",
      );
    }

    let promiseResult: string =
      decoded.resultLen > 0
        ? this.memory.readString(OUTPUT_OFFSET, decoded.resultLen as i32)
        : "";

    return new AwaitPromiseOutcome(promiseResult, decoded.timedOut, null);
  }

  // ────────────────────────────────────────────
  // 18. registerUpdateHandler
  // ────────────────────────────────────────────

  /**
   * Register a handler for update calls on this workflow.
   *
   * Update handlers allow external clients to send update requests to
   * the workflow while it is executing.
   *
   * @param name - The update handler name.
   */
  registerUpdateHandler(name: string): void {
    let nameLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, name);
    import_cleat_register_update_handler(SCRATCH_BASE as i32, nameLen);
  }

  // ────────────────────────────────────────────
  // 19. pluginCall
  // ────────────────────────────────────────────

  /**
   * Call a plugin function via the host runtime.
   *
   * Plugin name, function name, and input JSON are encoded to the scratch
   * buffer sequentially, the host call is made, and the response is read
   * from the output buffer.
   *
   * @param pluginName    - Name of the plugin (e.g., "blobstore", "slacknotify").
   * @param functionName  - Plugin function name (e.g., "put", "get").
   * @param inputJson     - Input payload as a JSON string.
   * @returns The plugin call outcome with response JSON or error details.
   */
  pluginCall(pluginName: string, functionName: string, inputJson: string): PluginCallOutcome {
    // Encode input strings sequentially into the scratch buffer
    let pluginNameLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, pluginName);
    let fnOffset: usize = SCRATCH_BASE + pluginNameLen;
    let remaining: i32 = OUT_BUF_SIZE - pluginNameLen;
    let fnLen: i32 = this.writeScratch(fnOffset, remaining, functionName, "functionName");
    let inputOffset: usize = fnOffset + fnLen;
    remaining -= fnLen;
    let inputLen: i32 = this.writeScratch(inputOffset, remaining, inputJson, "inputJson");

    // Call the host import
    let result: i64 = import_plugin_call(
      SCRATCH_BASE as i32,
      pluginNameLen,
      fnOffset as i32,
      fnLen,
      inputOffset as i32,
      inputLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    // Decode the packed result (same bit layout as cleat_call)
    let decoded = decodeCallResult(result);
    let responseLen: i32 = decoded.responseLen as i32;

    // On error, the output buffer contains an error message
    if (decoded.errCode !== 0) {
      let errMsg: string =
        responseLen > 0 ? this.memory.readString(OUTPUT_OFFSET, responseLen) : "pluginCall(" + pluginName + "." + functionName + ") failed: unknown error (code " + decoded.errCode.toString() + ")";
      return new PluginCallOutcome("", errMsg, decoded.callErrorCode);
    }

    // Success: read the response
    let resp: string =
      responseLen > 0 ? this.memory.readString(OUTPUT_OFFSET, responseLen) : "";
    return new PluginCallOutcome(resp, null, 0);
  }

  // ────────────────────────────────────────────
  // 19a. pluginCallStreaming
  // ────────────────────────────────────────────

  /**
   * Call a plugin function via the host runtime with streaming support.
   *
   * Plugin name, function name, and input JSON are encoded to the scratch
   * buffer sequentially, the host call is made, and the response is read
   * from the output buffer. Supports streaming responses from the plugin.
   *
   * @param pluginName    - Name of the plugin (e.g., "blobstore", "slacknotify").
   * @param functionName  - Plugin function name (e.g., "put", "get").
   * @param inputJson     - Input payload as a JSON string.
   * @returns The plugin call outcome with response JSON or error details.
   */
  pluginCallStreaming(pluginName: string, functionName: string, inputJson: string): PluginCallOutcome {
    // Encode input strings sequentially into the scratch buffer
    let pluginNameLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, pluginName);
    let fnOffset: usize = SCRATCH_BASE + pluginNameLen;
    let remaining: i32 = OUT_BUF_SIZE - pluginNameLen;
    let fnLen: i32 = this.writeScratch(fnOffset, remaining, functionName, "functionName");
    let inputOffset: usize = fnOffset + fnLen;
    remaining -= fnLen;
    let inputLen: i32 = this.writeScratch(inputOffset, remaining, inputJson, "inputJson");

    // Call the host import
    let result: i64 = import_plugin_call_streaming(
      SCRATCH_BASE as i32,
      pluginNameLen,
      fnOffset as i32,
      fnLen,
      inputOffset as i32,
      inputLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    // Decode the packed result (same bit layout as plugin_call)
    let decoded = decodeCallResult(result);
    let responseLen: i32 = decoded.responseLen as i32;

    // On error, the output buffer contains an error message
    if (decoded.errCode !== 0) {
      let errMsg: string =
        responseLen > 0 ? this.memory.readString(OUTPUT_OFFSET, responseLen) : "pluginCallStreaming(" + pluginName + "." + functionName + ") failed: unknown error (code " + decoded.errCode.toString() + ")";
      return new PluginCallOutcome("", errMsg, decoded.callErrorCode);
    }

    // Success: read the response
    let resp: string =
      responseLen > 0 ? this.memory.readString(OUTPUT_OFFSET, responseLen) : "";
    return new PluginCallOutcome(resp, null, 0);
  }

  // ────────────────────────────────────────────
  // 20. currentWorkflowId
  // ────────────────────────────────────────────

  /**
   * Get the current workflow ID from the host runtime.
   *
   * @returns The workflow ID string.
   */
  currentWorkflowId(): string {
    let result: i64 = import_cleat_workflow_id(OUTPUT_OFFSET as i32, OUT_BUF_SIZE);
    let decoded = decodeSimpleResult(result);
    if (decoded.errCode !== 0 || decoded.extra === 0) {
      return "";
    }
    return this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32);
  }

  // ────────────────────────────────────────────
  // 21. setScope
  // ────────────────────────────────────────────

  /**
   * Set the state key prefix for virtual object instances via the host
   * and update the local scope prefix.
   *
   * All subsequent setQueryState calls are automatically prefixed
   * with "vo:<objectType>:<instanceKey>:".
   *
   * @param objectType  - The virtual object type name.
   * @param instanceKey - The instance key for this specific object.
   * @returns The previous scope prefix (empty string if none was set).
   */
  setScope(objectType: string, instanceKey: string): string {
    let objTypeLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, objectType);
    let instOffset: usize = SCRATCH_BASE + objTypeLen;
    let remaining: i32 = OUT_BUF_SIZE - objTypeLen;
    let instKeyLen: i32 = this.writeScratch(instOffset, remaining, instanceKey, "instanceKey");

    let result: i64 = import_cleat_set_scope(
      SCRATCH_BASE as i32,
      objTypeLen,
      instOffset as i32,
      instKeyLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    let decoded = decodeSimpleResult(result);
    let prevScope: string = decoded.extra > 0
      ? this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32)
      : "";
    this._scopePrefix =
      objectType.length > 0 && instanceKey.length > 0
        ? "vo:" + objectType + ":" + instanceKey + ":"
        : "";
    return prevScope;
  }

  // ────────────────────────────────────────────
  // 22. getScope
  // ────────────────────────────────────────────

  /**
   * Get the current virtual object scope from the host.
   *
   * @returns A tuple [objectType, instanceKey] or ["", ""] if no scope
   *          is set.
   */
  getScope(): string[] {
    let halfSize: i32 = OUT_BUF_SIZE / 2;
    let result: i64 = import_cleat_get_scope(
      OUTPUT_OFFSET as i32,
      halfSize,
      (OUTPUT_OFFSET + halfSize) as i32,
      halfSize,
    );
    let lengths = decodeGetScopeResult(result);
    let objTypeLen: i32 = lengths[0] as i32;
    let instKeyLen: i32 = lengths[1] as i32;
    let objType: string = objTypeLen > 0
      ? this.memory.readString(OUTPUT_OFFSET, objTypeLen)
      : "";
    let instKey: string = instKeyLen > 0
      ? this.memory.readString(OUTPUT_OFFSET + halfSize, instKeyLen)
      : "";
    return [objType, instKey];
  }

  // ────────────────────────────────────────────
  // 23. clearScope
  // ────────────────────────────────────────────

  /**
   * Remove the current scope on both the host and locally.
   *
   * Calls cleat_set_scope with empty strings to clear the host-side
   * virtual object scope, and resets the local scope prefix.
   *
   * @returns The previous scope JSON string (empty if none was set).
   */
  clearScope(): string {
    // Call cleat_set_scope with empty strings to clear host-side scope
    let result: i64 = import_cleat_set_scope(
      0, 0,
      0, 0,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    let decoded = decodeSimpleResult(result);
    let prevScope: string =
      decoded.extra > 0
        ? this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32)
        : "";

    // Clear local scope prefix
    let prev: string = this._scopePrefix;
    this._scopePrefix = "";
    return prevScope.length > 0 ? prevScope : prev;
  }

  // ────────────────────────────────────────────
  // 24. uuid — deterministic ID generation via host
  // ────────────────────────────────────────────

  /**
   * Return a deterministic UUID via the host runtime.
   *
   * The host generates a UUID scoped to the current workflow and seed.
   * Same seed always produces the same UUID for this workflow instance.
   *
   * @param seed - A seed string that determines the UUID within this
   *               workflow.
   * @returns A UUID-formatted string, or empty string on error.
   */
  uuid(seed: string): string {
    let seedLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, seed);
    let result: i64 = import_cleat_uuid(
      SCRATCH_BASE as i32,
      seedLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );
    let decoded = decodeSimpleResult(result);
    if (decoded.errCode !== 0 || decoded.extra === 0) {
      return "";
    }
    return this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32);
  }

  // ────────────────────────────────────────────
  // 25. sendSignalAndWait — send a signal and wait for a response
  // ────────────────────────────────────────────

  /**
   * Send a signal to a target workflow and wait for a response (seconds).
   *
   * The signal carries an embedded correlation ID. The target workflow
   * should use `replyToSignal` to send a response back.
   *
   * @param targetRunId    - The target workflow's run ID.
   * @param signalName     - The signal name to send.
   * @param payload        - The signal payload JSON.
   * @param timeoutSeconds - Maximum wait time in seconds.
   * @returns A DurableResult containing the response on success.
   */
  sendSignalAndWait(
    targetRunId: string,
    signalName: string,
    payload: string,
    timeoutSeconds: i64,
  ): DurableResult<string> {
    return this.sendSignalAndWaitMs(targetRunId, signalName, payload, timeoutSeconds * 1000);
  }

  /**
   * Send a signal to a target workflow and wait for a response (milliseconds).
   *
   * The signal carries an embedded correlation ID. The target workflow
   * should use `replyToSignal` to send a response back.
   *
   * @param targetRunId - The target workflow's run ID.
   * @param signalName  - The signal name to send.
   * @param payload     - The signal payload JSON.
   * @param timeoutMs   - Maximum wait time in milliseconds.
   * @returns A DurableResult containing the response on success.
   */
  sendSignalAndWaitMs(
    targetRunId: string,
    signalName: string,
    payload: string,
    timeoutMs: i64,
  ): DurableResult<string> {
    let targetLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, targetRunId);
    let sigOffset: usize = SCRATCH_BASE + targetLen;
    let remaining: i32 = OUT_BUF_SIZE - targetLen;
    let sigLen: i32 = this.writeScratch(sigOffset, remaining, signalName, "signalName");
    let payloadOffset: usize = sigOffset + sigLen;
    remaining -= sigLen;
    let payloadLen: i32 = this.writeScratch(payloadOffset, remaining, payload, "payload");

    let result: i64 = import_cleat_send_signal_and_wait(
      SCRATCH_BASE as i32,
      targetLen,
      sigOffset as i32,
      sigLen,
      payloadOffset as i32,
      payloadLen,
      timeoutMs,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    let decoded = decodeSimpleResult(result);
    if (decoded.errCode !== 0) {
      return new DurableResult<string>(
        "",
        "sendSignalAndWaitMs(targetRunId='" + targetRunId + "', signalName='" + signalName + "') failed: " + errorCodeName(decoded.errCode) + " (code " + decoded.errCode.toString() + ")",
      );
    }
    let response: string = this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32);
    return new DurableResult<string>(response, null);
  }

  // ────────────────────────────────────────────
  // 26. replyToSignal — respond to a signal from within a handler
  // ────────────────────────────────────────────

  /**
   * Send a response back to the sender of a signal.
   *
   * Only valid inside a signal handler context where the correlation ID
   * was embedded in the received signal payload.
   *
   * @param correlationId - The correlation ID from the received signal payload.
   * @param response      - The response payload JSON.
   * @returns An error message on failure, or null on success.
   */
  replyToSignal(correlationId: string, response: string): string | null {
    let cidLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, correlationId);
    let respOffset: usize = SCRATCH_BASE + cidLen;
    let remaining: i32 = OUT_BUF_SIZE - cidLen;
    let respLen: i32 = this.writeScratch(respOffset, remaining, response, "response");

    let result: i64 = import_cleat_reply_to_signal(
      SCRATCH_BASE as i32,
      cidLen,
      respOffset as i32,
      respLen,
    );

    let decoded = decodeSimpleResult(result);
    if (decoded.errCode !== 0) {
      return "replyToSignal(correlationId='" + correlationId + "') failed: " + errorCodeName(decoded.errCode) + " (code " + decoded.errCode.toString() + ")";
    }
    return null;
  }

  // ────────────────────────────────────────────
  // 27. awaitSignalsWithQuorum — wait for quorum of signals
  // ────────────────────────────────────────────

  /**
   * Wait for at least minCount signals from the named set (seconds).
   *
   * Collects signals until minCount is reached, maxRejections is exceeded,
   * or the timeout expires.
   *
   * @param namesJson      - JSON array of signal names to wait for.
   * @param minCount       - Minimum number of signals required to proceed.
   * @param maxRejections  - Maximum rejections tolerated (-1 to disable).
   * @param timeoutSeconds - Maximum wait time in seconds.
   * @returns The collected signal results.
   */
  awaitSignalsWithQuorum(
    namesJson: string,
    minCount: i32,
    maxRejections: i32,
    timeoutSeconds: i64,
  ): AwaitSignalsOutcome[] {
    return this.awaitSignalsWithQuorumMs(namesJson, minCount, maxRejections, timeoutSeconds * 1000);
  }

  /**
   * Wait for at least minCount signals from the named set (milliseconds).
   *
   * Collects signals until minCount is reached, maxRejections is exceeded,
   * or the timeout expires.
   *
   * @param namesJson     - JSON array of signal names to wait for.
   * @param minCount      - Minimum number of signals required to proceed.
   * @param maxRejections - Maximum rejections tolerated (-1 to disable).
   * @param timeoutMs     - Maximum wait time in milliseconds.
   * @returns The collected signal results.
   */
  awaitSignalsWithQuorumMs(
    namesJson: string,
    minCount: i32,
    maxRejections: i32,
    timeoutMs: i64,
  ): AwaitSignalsOutcome[] {
    // Simple implementation: call awaitSignalsMs in a loop.
    let results: AwaitSignalsOutcome[] = [];
    let deadline: i64 = this.now() + timeoutMs;
    let rejectionCount: i32 = 0;

    while (results.length < minCount) {
      let remainingMs: i64 = deadline - this.now();
      if (remainingMs <= 0) {
        throw new Error(
          "quorum timeout: got " + results.length.toString() + "/" + minCount.toString() + " signals",
        );
      }

      let outcome: AwaitSignalsOutcome = this.awaitSignalsMs(namesJson, remainingMs);
      if (outcome.timedOut) {
        throw new Error(
          "quorum timeout: got " + results.length.toString() + "/" + minCount.toString() + " signals",
        );
      }
      if (outcome.isError) {
        throw new Error("quorum signal error: " + (outcome.error as string));
      }

      results.push(outcome);

      // Check for rejection if maxRejections >= 0.
      if (maxRejections >= 0 && outcome.payload.length > 0) {
        // Look for "rejected": true in the JSON payload.
        if (outcome.payload.indexOf('"rejected":true') !== -1 || outcome.payload.indexOf('"rejected": true') !== -1) {
          rejectionCount++;
          if (rejectionCount > maxRejections) {
            throw new Error(
              "quorum exceeded max rejections (" + maxRejections.toString() + ")",
            );
          }
        }
      }
    }
    return results;
  }

  // ────────────────────────────────────────────
  // 28. signalWorkflow — send a signal to another workflow
  // ────────────────────────────────────────────

  /**
   * Send a signal to a target workflow (fire-and-forget).
   *
   * Unlike sendSignalAndWait, this method does not wait for a response.
   * The signal is enqueued and the workflow continues immediately.
   *
   * @param targetRunId - The target workflow's run ID.
   * @param signalName  - The signal name to send.
   * @param payload     - The signal payload JSON.
   * @returns An error message on failure, or null on success.
   */
  signalWorkflow(targetRunId: string, signalName: string, payload: string): string | null {
    let targetLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, targetRunId);
    let sigOffset: usize = SCRATCH_BASE + targetLen;
    let remaining: i32 = OUT_BUF_SIZE - targetLen;
    let sigLen: i32 = this.writeScratch(sigOffset, remaining, signalName, "signalName");
    let payloadOffset: usize = sigOffset + sigLen;
    remaining -= sigLen;
    let payloadLen: i32 = this.writeScratch(payloadOffset, remaining, payload, "payload");

    let result: i64 = import_cleat_signal_workflow(
      SCRATCH_BASE as i32,
      targetLen,
      sigOffset as i32,
      sigLen,
      payloadOffset as i32,
      payloadLen,
    );

    let decoded = decodeSimpleResult(result);
    if (decoded.errCode !== 0) {
      return "signalWorkflow(targetRunId='" + targetRunId + "', signalName='" + signalName + "') failed: " + errorCodeName(decoded.errCode) + " (code " + decoded.errCode.toString() + ")";
    }
    return null;
  }

  // ────────────────────────────────────────────
  // 28a. sideEffect — cache and return a deterministic side effect result
  // ────────────────────────────────────────────

  /**
   * Record a side effect result and return the cached value on replay.
   *
   * On first execution, the result is recorded in the event history.
   * On replay, the previously recorded result is returned instead of
   * re-executing the side effect.
   *
   * @param result - The side effect result JSON to record.
   * @returns The cached result on replay, or the same result on first
   *          execution. Returns null on error.
   */
  sideEffect(result: string): string | null {
    let resultLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, result);

    let hostResult: i64 = import_cleat_side_effect(
      SCRATCH_BASE as i32,
      resultLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    let decoded = decodeSimpleResult(hostResult);
    if (decoded.errCode !== 0 || decoded.extra === 0) {
      return null;
    }
    return this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32);
  }

  // ────────────────────────────────────────────
  // 29. resolvePromise — resolve a durable promise
  // ────────────────────────────────────────────

  /**
   * Resolve a durable promise with a value.
   *
   * The promise must have been created by `createPromise` and the ID
   * must be valid (obtained from the host when the promise was created).
   *
   * @param id    - The durable promise ID to resolve.
   * @param value - The value to resolve the promise with.
   * @returns An error message on failure, or null on success.
   */
  resolvePromise(id: string, value: string): string | null {
    let idLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, id);
    let valOffset: usize = SCRATCH_BASE + idLen;
    let remaining: i32 = OUT_BUF_SIZE - idLen;
    let valLen: i32 = this.writeScratch(valOffset, remaining, value, "value");

    let result: i64 = import_cleat_resolve_promise(
      SCRATCH_BASE as i32,
      idLen,
      valOffset as i32,
      valLen,
    );

    let decoded = decodeSimpleResult(result);
    if (decoded.errCode !== 0) {
      return "resolvePromise(id='" + id + "') failed: " + errorCodeName(decoded.errCode) + " (code " + decoded.errCode.toString() + ")";
    }
    return null;
  }

  // ────────────────────────────────────────────
  // 30. rejectPromise — reject a durable promise
  // ────────────────────────────────────────────

  /**
   * Reject a durable promise with an error message.
   *
   * @param id    - The durable promise ID to reject.
   * @param error - The error message to reject the promise with.
   * @returns An error message on failure, or null on success.
   */
  rejectPromise(id: string, error: string): string | null {
    let idLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, id);
    let errOffset: usize = SCRATCH_BASE + idLen;
    let remaining: i32 = OUT_BUF_SIZE - idLen;
    let errLen: i32 = this.writeScratch(errOffset, remaining, error, "error");

    let result: i64 = import_cleat_reject_promise(
      SCRATCH_BASE as i32,
      idLen,
      errOffset as i32,
      errLen,
    );

    let decoded = decodeSimpleResult(result);
    if (decoded.errCode !== 0) {
      return "rejectPromise(id='" + id + "') failed: " + errorCodeName(decoded.errCode) + " (code " + decoded.errCode.toString() + ")";
    }
    return null;
  }

  // ────────────────────────────────────────────
  // 31. cleatSend — fire-and-forget durable call
  // ────────────────────────────────────────────

  /**
   * Make a fire-and-forget durable call to an external service.
   *
   * Unlike `cleatCall`, this does NOT wait for a response. The call is
   * recorded for replay but the workflow continues immediately.
   *
   * @param service      - Service name (e.g., "payment", "email").
   * @param operation    - Operation name (e.g., "charge", "send").
   * @param requestJson  - Request payload as a JSON string.
   * @returns An error message on failure, or null on success.
   */
  cleatSend(service: string, operation: string, requestJson: string): string | null {
    let svcLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, service);
    let opOffset: usize = SCRATCH_BASE + svcLen;
    let remaining: i32 = OUT_BUF_SIZE - svcLen;
    let opLen: i32 = this.writeScratch(opOffset, remaining, operation, "operation");
    let reqOffset: usize = opOffset + opLen;
    remaining -= opLen;
    let reqLen: i32 = this.writeScratch(reqOffset, remaining, requestJson, "requestJson");

    let result: i64 = import_cleat_send(
      SCRATCH_BASE as i32,
      svcLen,
      opOffset as i32,
      opLen,
      reqOffset as i32,
      reqLen,
    );

    let decoded = decodeSimpleResult(result);
    if (decoded.errCode !== 0) {
      return "cleatSend(service='" + service + "', operation='" + operation + "') failed: " + errorCodeName(decoded.errCode) + " (code " + decoded.errCode.toString() + ")";
    }
    return null;
  }

  // ────────────────────────────────────────────
  // 32. scheduleInvoke — schedule delayed invocation
  // ────────────────────────────────────────────

  /**
   * Schedule a one-shot delayed invocation of a service operation (seconds).
   *
   * The invocation will be executed after the delay has elapsed.
   * This is fire-and-forget — no result is returned.
   *
   * @param service       - Service name.
   * @param operation     - Operation name.
   * @param requestJson   - Request payload as a JSON string.
   * @param delaySeconds  - Delay in seconds before the invocation fires.
   * @returns An error message on failure, or null on success.
   */
  scheduleInvoke(service: string, operation: string, requestJson: string, delaySeconds: i64): string | null {
    return this.scheduleInvokeMs(service, operation, requestJson, delaySeconds * 1000);
  }

  /**
   * Schedule a one-shot delayed invocation of a service operation (milliseconds).
   *
   * The invocation will be executed after the delay has elapsed.
   * This is fire-and-forget — no result is returned.
   *
   * @param service      - Service name.
   * @param operation    - Operation name.
   * @param requestJson  - Request payload as a JSON string.
   * @param delayMs      - Delay in milliseconds before the invocation fires.
   * @returns An error message on failure, or null on success.
   */
  scheduleInvokeMs(service: string, operation: string, requestJson: string, delayMs: i64): string | null {
    let svcLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, service);
    let opOffset: usize = SCRATCH_BASE + svcLen;
    let remaining: i32 = OUT_BUF_SIZE - svcLen;
    let opLen: i32 = this.writeScratch(opOffset, remaining, operation, "operation");
    let reqOffset: usize = opOffset + opLen;
    remaining -= opLen;
    let reqLen: i32 = this.writeScratch(reqOffset, remaining, requestJson, "requestJson");

    let result: i64 = import_schedule_invoke(
      SCRATCH_BASE as i32,
      svcLen,
      opOffset as i32,
      opLen,
      reqOffset as i32,
      reqLen,
      delayMs,
    );

    let decoded = decodeSimpleResult(result);
    if (decoded.errCode !== 0) {
      return "scheduleInvokeMs(service='" + service + "', operation='" + operation + "') failed: " + errorCodeName(decoded.errCode) + " (code " + decoded.errCode.toString() + ")";
    }
    return null;
  }

  // There is no registerQueryHandler here (removed 2026-08-09). Its doc
  // comment claimed "the host [will] invoke [it] when a query arrives" --
  // untrue: nothing ever routed an external query to a registered handler,
  // in this SDK or any other. See docs/determinism.md, "Why there is no
  // RegisterQueryHandler". Use setQueryState instead; it is durable and
  // externally readable via GET /api/workflows/:id/query?key=X regardless
  // of whether a worker currently has the workflow loaded.

  // ────────────────────────────────────────────
  // 34. runDetached — run fire-and-forget child workflow
  // ────────────────────────────────────────────

  /**
   * Run a function in a detached (fire-and-forget) child workflow context.
   *
   * The child workflow runs independently — no run ID or result is returned.
   * Uses name-based dispatch to identify the workflow function.
   *
   * @param name      - The workflow function name to invoke.
   * @param inputJson - Input JSON for the detached workflow.
   * @returns An error message on failure, or null on success.
   */
  runDetached(name: string, inputJson: string): string | null {
    let nameLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, name);
    let inputOffset: usize = SCRATCH_BASE + nameLen;
    let remaining: i32 = OUT_BUF_SIZE - nameLen;
    let inputLen: i32 = this.writeScratch(inputOffset, remaining, inputJson, "inputJson");

    let result: i64 = import_cleat_run_detached(
      SCRATCH_BASE as i32,
      nameLen,
      inputOffset as i32,
      inputLen,
    );

    let decoded = decodeSimpleResult(result);
    if (decoded.errCode !== 0) {
      return "runDetached(name='" + name + "') failed: " + errorCodeName(decoded.errCode) + " (code " + decoded.errCode.toString() + ")";
    }
    return null;
  }

  // ────────────────────────────────────────────
  // Private: scopedKey — apply scope prefix to state key
  // ────────────────────────────────────────────

  /**
   * Prepend the current scope prefix to a state key if setScope was called.
   *
   * @param key - The raw state key.
   * @returns The scoped key with prefix (if scope is set).
   */
  private scopedKey(key: string): string {
    if (this._scopePrefix.length > 0) {
      return this._scopePrefix + key;
    }
    return key;
  }

  // ────────────────────────────────────────────
  // 35. setState — set workflow state
  // ────────────────────────────────────────────

  /**
   * Set a key-value pair in workflow state.
   *
   * State keys are automatically scoped if `setScope` was called
   * (prefixed with "vo:<type>:<key>:").
   *
   * @param key   - State key.
   * @param value - State value.
   * @returns An error message on failure, or null on success.
   */
  setState(key: string, value: string): string | null {
    let fullKey: string = this.scopedKey(key);
    let keyLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, fullKey);
    let valOffset: usize = SCRATCH_BASE + keyLen;
    let remaining: i32 = OUT_BUF_SIZE - keyLen;
    let valLen: i32 = this.writeScratch(valOffset, remaining, value, "value");

    let result: i64 = import_cleat_set_state(
      SCRATCH_BASE as i32,
      keyLen,
      valOffset as i32,
      valLen,
    );

    let decoded = decodeSimpleResult(result);
    if (decoded.errCode !== 0) {
      return "setState(key='" + key + "') failed: " + errorCodeName(decoded.errCode) + " (code " + decoded.errCode.toString() + ")";
    }
    return null;
  }

  // ────────────────────────────────────────────
  // 36. getState — get workflow state
  // ────────────────────────────────────────────

  /**
   * Get a value from workflow state by key.
   *
   * Returns the value string, or null if the key does not exist
   * or an error occurred.
   *
   * @param key - State key to look up.
   * @returns The value string, or null if not found or on error.
   */
  getState(key: string): string | null {
    let fullKey: string = this.scopedKey(key);
    let keyLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, fullKey);

    let result: i64 = import_cleat_get_state(
      SCRATCH_BASE as i32,
      keyLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    let decoded = decodeSimpleResult(result);
    if (decoded.errCode !== 0 || decoded.extra === 0) {
      return null;
    }
    return this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32);
  }

  // ────────────────────────────────────────────
  // 37. deleteState — delete workflow state
  // ────────────────────────────────────────────

  /**
   * Delete a key from workflow state.
   *
   * @param key - State key to delete.
   * @returns An error message on failure, or null on success.
   */
  deleteState(key: string): string | null {
    let fullKey: string = this.scopedKey(key);
    let keyLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, fullKey);

    let result: i64 = import_cleat_delete_state(
      SCRATCH_BASE as i32,
      keyLen,
    );

    let decoded = decodeSimpleResult(result);
    if (decoded.errCode !== 0) {
      return "deleteState(key='" + key + "') failed: " + errorCodeName(decoded.errCode) + " (code " + decoded.errCode.toString() + ")";
    }
    return null;
  }

  // ────────────────────────────────────────────
  // 38. incrState — atomically increment state value
  // ────────────────────────────────────────────

  /**
   * Atomically increment a numeric state value by the given delta.
   *
   * If the key does not exist, it is initialized to the delta value.
   *
   * @param key   - State key to increment.
   * @param delta - Amount to add (can be negative for decrement).
   * @returns The new value after increment, or 0 on error.
   */
  incrState(key: string, delta: i64): i64 {
    let fullKey: string = this.scopedKey(key);
    let keyLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, fullKey);

    let result: i64 = import_cleat_incr_state(
      SCRATCH_BASE as i32,
      keyLen,
      delta,
    );

    return result; // Host returns the new value directly
  }

  // ────────────────────────────────────────────
  // 39. hasState — check if state key exists
  // ────────────────────────────────────────────

  /**
   * Check if a key exists in workflow state.
   *
   * @param key - State key to check.
   * @returns True if the key exists, false otherwise.
   */
  hasState(key: string): bool {
    let fullKey: string = this.scopedKey(key);
    let keyLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, fullKey);

    let result: i64 = import_cleat_has_state(
      SCRATCH_BASE as i32,
      keyLen,
    );

    // Host returns non-zero for true, zero for false
    return result !== 0;
  }

  // ────────────────────────────────────────────
  // 40. listState — list state keys by prefix
  // ────────────────────────────────────────────

  /**
   * List state keys matching a prefix.
   *
   * Returns an array of key names (not values) that match the prefix.
   *
   * @param prefix - Key prefix to match (empty string lists all keys).
   * @returns Array of matching key names.
   */
  listState(prefix: string): string[] {
    let fullPrefix: string = this.scopedKey(prefix);
    let prefixLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, fullPrefix);

    let result: i64 = import_cleat_list_state(
      SCRATCH_BASE as i32,
      prefixLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    let decoded = decodeSimpleResult(result);
    if (decoded.errCode !== 0 || decoded.extra === 0) {
      return [];
    }
    let jsonStr: string = this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32);
    return jsonStrArray(jsonStr);
  }

  // ────────────────────────────────────────────
  // 41. awaitAllChildren — wait for multiple child workflows
  // ────────────────────────────────────────────

  /**
   * Wait for multiple child workflows to complete.
   *
   * Accepts a JSON array of child run IDs and returns aggregated results
   * as a JSON array.
   *
   * @param runIdsJson - JSON array of child run IDs (e.g., '["run1","run2"]').
   * @returns JSON string of aggregated results, or null on error.
   */
  awaitAllChildren(runIdsJson: string): string | null {
    let runIdsLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, runIdsJson);

    let result: i64 = import_cleat_await_all_children(
      SCRATCH_BASE as i32,
      runIdsLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    let decoded = decodeSimpleResult(result);
    if (decoded.errCode !== 0 || decoded.extra === 0) {
      return null;
    }
    return this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32);
  }

  // ────────────────────────────────────────────
  // 43. isReplaying — deliberately NOT offered
  // ────────────────────────────────────────────
  //
  // This SDK used to expose isReplaying(): bool. It was hardcoded `return
  // false` with a TODO, and no host call backing it existed in the engine for
  // any SDK -- so every "only on first execution" branch fired on every replay
  // too, silently defeating its own purpose. Nothing failed, because a constant
  // is consistent between execute and replay; you just got duplicate logs,
  // duplicate metrics and duplicate notifications after each worker restart.
  //
  // Removed rather than implemented. The engine does know whether it is
  // replaying (execSession.isReplay), so wiring it up was possible -- but a raw
  // replay flag is a determinism footgun: a workflow that branches its LOGIC on
  // it records different events on replay than it did on execution, which is
  // precisely what replay exists to prevent.
  //
  // The one legitimate use -- not repeating a side effect on replay -- is what
  // sideEffect() is for. It records the result on first execution and returns
  // the recorded one afterwards, so the value is replay-consistent by
  // construction rather than by the author remembering to check a flag:
  //
  //   let id = h.sideEffect(generateRequestId());  // computed once, replayed after
  //
  // See docs/determinism.md, "Why there is no isReplaying()".

  // ────────────────────────────────────────────
  // 44. currentRunId — get current run ID
  // ────────────────────────────────────────────

  /**
   * Get the current workflow run ID from the host runtime.
   *
   * The run ID uniquely identifies this specific execution of the workflow.
   *
   * @returns The run ID string, or empty string on error.
   */
  currentRunId(): string {
    let result: i64 = import_cleat_run_id(OUTPUT_OFFSET as i32, OUT_BUF_SIZE);
    let decoded = decodeSimpleResult(result);
    if (decoded.errCode !== 0 || decoded.extra === 0) {
      return "";
    }
    return this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32);
  }

  // ────────────────────────────────────────────
  // 43. cleatFetch — make HTTP request
  // ────────────────────────────────────────────

  /**
   * Make an HTTP request via the host runtime.
   *
   * The host handles the actual HTTP call, recording it for replay.
   * Returns a FetchResult containing status code, headers, and body.
   *
   * @param method      - HTTP method (e.g., "GET", "POST", "PUT", "DELETE").
   * @param url         - The full URL to request.
   * @param headersJson - Request headers as a JSON string (e.g., '{"Authorization":"Bearer ..."}').
   * @param body        - Request body string (empty string for GET/HEAD).
   * @returns The fetch result with status code, headers, body, and optional error.
   */
  cleatFetch(method: string, url: string, headersJson: string, body: string): FetchResult {
    let methodLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, method);
    let urlOffset: usize = SCRATCH_BASE + methodLen;
    let remaining: i32 = OUT_BUF_SIZE - methodLen;
    let urlLen: i32 = this.writeScratch(urlOffset, remaining, url, "url");
    let headersOffset: usize = urlOffset + urlLen;
    remaining -= urlLen;
    let headersLen: i32 = this.writeScratch(headersOffset, remaining, headersJson, "headersJson");
    let bodyOffset: usize = headersOffset + headersLen;
    remaining -= headersLen;
    let bodyLen: i32 = this.writeScratch(bodyOffset, remaining, body, "body");

    let result: i64 = import_cleat_fetch(
      SCRATCH_BASE as i32,
      methodLen,
      urlOffset as i32,
      urlLen,
      headersOffset as i32,
      headersLen,
      bodyOffset as i32,
      bodyLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    let decoded = decodeSimpleResult(result);
    if (decoded.errCode !== 0 || decoded.extra === 0) {
      let errMsg: string =
        decoded.extra > 0
          ? this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32)
          : "fetch error";
      return new FetchResult(0, "", "", errMsg);
    }

    let respJson: string = this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32);
    // Parse the JSON response from the host
    let status: i32 = jsonExtractNumber(respJson, "status") as i32;
    let headers: string = jsonExtractString(respJson, "headers");
    let bodyResp: string = jsonExtractString(respJson, "body");
    return new FetchResult(status, headers, bodyResp, null);
  }

  // ────────────────────────────────────────────
  // 44. fetchGet — convenience HTTP GET
  // ────────────────────────────────────────────

  /**
   * Convenience wrapper that performs an HTTP GET request.
   *
   * @param url - The URL to fetch.
   * @returns The fetch result.
   */
  fetchGet(url: string): FetchResult {
    return this.cleatFetch("GET", url, "{}", "");
  }

  // ────────────────────────────────────────────
  // 44. acquireLock — acquire a concurrency lock
  // ────────────────────────────────────────────

  /**
   * Attempt to acquire a concurrency lock for the given key (seconds).
   *
   * The lock is held for at most `ttlSeconds` seconds. Returns
   * a DurableResult containing `true` if the lock was acquired,
   * `false` if it was already held by another workflow.
   *
   * @param key         - The lock key.
   * @param ttlSeconds  - Time-to-live in seconds.
   * @returns A DurableResult containing the acquired flag.
   */
  acquireLock(key: string, ttlSeconds: i64): DurableResult<bool> {
    return this.acquireLockMs(key, ttlSeconds * 1000);
  }

  /**
   * Attempt to acquire a concurrency lock for the given key (milliseconds).
   *
   * The lock is held for at most `ttlMs` milliseconds. Returns
   * a DurableResult containing `true` if the lock was acquired,
   * `false` if it was already held by another workflow.
   *
   * @param key   - The lock key.
   * @param ttlMs - Time-to-live in milliseconds.
   * @returns A DurableResult containing the acquired flag.
   */
  acquireLockMs(key: string, ttlMs: i64): DurableResult<bool> {
    let keyLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, key);

    let result: i64 = import_cleat_acquire_lock(
      SCRATCH_BASE as i32,
      keyLen,
      ttlMs,
    );

    let errCode: i64 = result & 0xFF;
    let acquired: bool = ((result >> 8) & 0x1) != 0;

    if (errCode !== 0) {
      return new DurableResult<bool>(
        false,
        "acquireLockMs(key='" + key + "') failed: " + errorCodeName(<u32>errCode) + " (code " + errCode.toString() + ")",
      );
    }

    return new DurableResult<bool>(acquired, null);
  }

  // ────────────────────────────────────────────
  // 45. releaseLock — release a concurrency lock
  // ────────────────────────────────────────────

  /**
   * Release a concurrency lock previously acquired by this workflow.
   *
   * @param key - The lock key to release.
   * @returns An error message on failure, or null on success.
   */
  releaseLock(key: string): string | null {
    let keyLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, key);

    let result: i64 = import_cleat_release_lock(
      SCRATCH_BASE as i32,
      keyLen,
    );

    let errCode: i64 = result & 0xFF;
    if (errCode !== 0) {
      return "releaseLock(key='" + key + "') failed: " + errorCodeName(<u32>errCode) + " (code " + errCode.toString() + ")";
    }
    return null;
  }

  // ────────────────────────────────────────────
  // 46. scheduleCron — register a recurring workflow
  // ────────────────────────────────────────────

  /**
   * Register a recurring cron-triggered workflow schedule.
   *
   * The actual cron execution engine runs on the HOST side. This SDK method
   * only provides the API for workflows to register cron schedules with the
   * host runtime.
   *
   * @param workflowName  - The workflow definition name to invoke on each tick.
   * @param cronExpr      - Standard 5-field cron expression.
   * @param timezone      - IANA timezone (e.g., "America/New_York", "UTC").
   * @param inputJson     - Input JSON passed to each workflow invocation.
   * @returns A DurableResult containing the schedule ID on success.
   */
  scheduleCron(
    workflowName: string,
    cronExpr: string,
    timezone: string,
    inputJson: string,
  ): DurableResult<string> {
    let wfLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, workflowName);
    let cronOffset: usize = SCRATCH_BASE + wfLen;
    let remaining: i32 = OUT_BUF_SIZE - wfLen;
    let cronLen: i32 = this.writeScratch(cronOffset, remaining, cronExpr, "cronExpr");
    let tzOffset: usize = cronOffset + cronLen;
    remaining -= cronLen;
    let tzLen: i32 = this.writeScratch(tzOffset, remaining, timezone, "timezone");
    let inputOffset: usize = tzOffset + tzLen;
    remaining -= tzLen;
    let inputLen: i32 = this.writeScratch(inputOffset, remaining, inputJson, "inputJson");

    let result: i64 = import_schedule_cron(
      SCRATCH_BASE as i32,
      wfLen,
      cronOffset as i32,
      cronLen,
      tzOffset as i32,
      tzLen,
      inputOffset as i32,
      inputLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    let decoded = decodeSimpleResult(result);
    if (decoded.errCode !== 0) {
      return new DurableResult<string>(
        "",
        "scheduleCron(workflowName='" + workflowName + "') failed: " + errorCodeName(decoded.errCode) + " (code " + decoded.errCode.toString() + ")",
      );
    }
    let scheduleId: string = this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32);
    return new DurableResult<string>(scheduleId, null);
  }

  // ────────────────────────────────────────────
  // 46. deleteCron — remove a cron schedule
  // ────────────────────────────────────────────

  /**
   * Remove a previously registered cron schedule by its ID.
   *
   * @param scheduleId - The schedule ID returned by scheduleCron.
   * @returns An error message on failure, or null on success.
   */
  deleteCron(scheduleId: string): string | null {
    let idLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, scheduleId);

    let result: i64 = import_delete_cron(
      SCRATCH_BASE as i32,
      idLen,
    );

    let decoded = decodeSimpleResult(result);
    if (decoded.errCode !== 0) {
      return "deleteCron(scheduleId='" + scheduleId + "') failed: " + errorCodeName(decoded.errCode) + " (code " + decoded.errCode.toString() + ")";
    }
    return null;
  }

  // ────────────────────────────────────────────
  // 47. listCrons — list all cron schedules
  // ────────────────────────────────────────────

  /**
   * List all registered cron schedules.
   *
   * Returns a JSON array of schedule objects, each containing the
   * schedule ID, workflow name, cron expression, timezone, and input.
   *
   * @returns A JSON string of schedule objects, or null on error.
   */
  listCrons(): string | null {
    let result: i64 = import_list_crons(
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    let decoded = decodeSimpleResult(result);
    if (decoded.errCode !== 0 || decoded.extra === 0) {
      return null;
    }
    return this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32);
  }

  // ────────────────────────────────────────────
  // 48. jsonParse — validate and normalize JSON
  // ────────────────────────────────────────────

  /**
   * Validate and normalize a JSON string via the host runtime.
   *
   * Parses the input JSON string using the host's JSON library and
   * returns a validated, normalized JSON string. Returns null on
   * parse error.
   *
   * @param jsonStr - JSON string to validate and normalize.
   * @returns Normalized JSON string, or null on parse error.
   */
  jsonParse(jsonStr: string): string | null {
    let jsonLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, jsonStr);

    let result: i64 = import_cleat_json_parse(
      SCRATCH_BASE as i32,
      jsonLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    let errCode: u32 = (result as u64 & 0xFFFFFFFF) as u32;
    let bytesWritten: u32 = ((result as u64 >> 32) & 0xFFFFFFFF) as u32;

    if (errCode !== 0) {
      return null;
    }

    if (bytesWritten === 0) {
      return "";
    }

    return this.memory.readString(OUTPUT_OFFSET, bytesWritten as i32);
  }

  // ────────────────────────────────────────────
  // 49. jsonStringify — serialize JSON value
  // ────────────────────────────────────────────

  /**
   * Validate and serialize a JSON value via the host runtime.
   *
   * Takes a JSON string, validates it, and returns a re-serialized
   * (normalized) JSON string. Returns null on parse error.
   *
   * @param value - JSON string to validate and serialize.
   * @returns Serialized JSON string, or null on parse error.
   */
  jsonStringify(value: string): string | null {
    let valueLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, value);

    let result: i64 = import_cleat_json_stringify(
      SCRATCH_BASE as i32,
      valueLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    let errCode: u32 = (result as u64 & 0xFFFFFFFF) as u32;
    let bytesWritten: u32 = ((result as u64 >> 32) & 0xFFFFFFFF) as u32;

    if (errCode !== 0) {
      return null;
    }

    if (bytesWritten === 0) {
      return "";
    }

    return this.memory.readString(OUTPUT_OFFSET, bytesWritten as i32);
  }
}
