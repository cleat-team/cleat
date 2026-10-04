export async function instantiate(module, imports = {}) {
  const adaptedImports = {
    env: Object.setPrototypeOf({
      abort(message, fileName, lineNumber, columnNumber) {
        // ~lib/builtins/abort(~lib/string/String | null?, ~lib/string/String | null?, u32?, u32?) => void
        message = __liftString(message >>> 0);
        fileName = __liftString(fileName >>> 0);
        lineNumber = lineNumber >>> 0;
        columnNumber = columnNumber >>> 0;
        (() => {
          // @external.js
          throw Error(`${message} in ${fileName}:${lineNumber}:${columnNumber}`);
        })();
      },
      cleat_child_workflow(namePtr, nameLen, inputPtr, inputLen, runIdPtr, runIdMaxLen) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_child_workflow(i32, i32, i32, i32, i32, i32) => i64
        return cleat_child_workflow(namePtr, nameLen, inputPtr, inputLen, runIdPtr, runIdMaxLen) || 0n;
      },
      cleat_child_workflow_with_options(namePtr, nameLen, inputPtr, inputLen, version, priority, policyPtr, policyLen, runIdPtr, runIdMaxLen) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_child_workflow_with_options(i32, i32, i32, i32, i64, i64, i32, i32, i32, i32) => i64
        return cleat_child_workflow_with_options(namePtr, nameLen, inputPtr, inputLen, version, priority, policyPtr, policyLen, runIdPtr, runIdMaxLen) || 0n;
      },
      cleat_poll_update(envelopePtr, envelopeMaxLen) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_poll_update(i32, i32) => i64
        return cleat_poll_update(envelopePtr, envelopeMaxLen) || 0n;
      },
      cleat_complete_update(requestIdPtr, requestIdLen, resultPtr, resultLen, errPtr, errLen) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_complete_update(i32, i32, i32, i32, i32, i32) => i64
        return cleat_complete_update(requestIdPtr, requestIdLen, resultPtr, resultLen, errPtr, errLen) || 0n;
      },
      cleat_await_child(runIdPtr, runIdLen, resultPtr, resultMaxLen) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_await_child(i32, i32, i32, i32) => i64
        return cleat_await_child(runIdPtr, runIdLen, resultPtr, resultMaxLen) || 0n;
      },
      cleat_await_all_children(runIdsJsonPtr, runIdsJsonLen, outPtr, maxLen) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_await_all_children(i32, i32, i32, i32) => i64
        return cleat_await_all_children(runIdsJsonPtr, runIdsJsonLen, outPtr, maxLen) || 0n;
      },
      cleat_await_any_child(runIdsPtr, runIdsLen, resultPtr, resultMaxLen) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_await_any_child(i32, i32, i32, i32) => i64
        return cleat_await_any_child(runIdsPtr, runIdsLen, resultPtr, resultMaxLen) || 0n;
      },
      cleat_poll_child(runIdPtr, runIdLen, resultPtr, resultMaxLen) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_poll_child(i32, i32, i32, i32) => i64
        return cleat_poll_child(runIdPtr, runIdLen, resultPtr, resultMaxLen) || 0n;
      },
      cleat_create_promise(namePtr, nameLen, idOutPtr, idOutMax) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_create_promise(i32, i32, i32, i32) => i64
        return cleat_create_promise(namePtr, nameLen, idOutPtr, idOutMax) || 0n;
      },
      cleat_await_promise(idPtr, idLen, timeoutMs, resultOutPtr, resultOutMax) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_await_promise(i32, i32, i64, i32, i32) => i64
        return cleat_await_promise(idPtr, idLen, timeoutMs, resultOutPtr, resultOutMax) || 0n;
      },
      cleat_await_signals(namesPtr, namesLen, timeoutMs, sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_await_signals(i32, i32, i64, i32, i32, i32, i32) => i64
        return cleat_await_signals(namesPtr, namesLen, timeoutMs, sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen) || 0n;
      },
      cleat_poll_signal(namePtr, nameLen, payloadPtr, payloadMaxLen) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_poll_signal(i32, i32, i32, i32) => i64
        return cleat_poll_signal(namePtr, nameLen, payloadPtr, payloadMaxLen) || 0n;
      },
      cleat_call(svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen, respPtr, respMaxLen) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_call(i32, i32, i32, i32, i32, i32, i32, i32) => i64
        return cleat_call(svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen, respPtr, respMaxLen) || 0n;
      },
      cleat_call_heartbeat(svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen, heartbeatIntervalMs, respPtr, respMaxLen) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_call_heartbeat(i32, i32, i32, i32, i32, i32, i64, i32, i32) => i64
        return cleat_call_heartbeat(svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen, heartbeatIntervalMs, respPtr, respMaxLen) || 0n;
      },
      cleat_call_retry(svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen, maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs, nonRetryableErrorsPtr, nonRetryableErrorsLen, respPtr, respMaxLen) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_call_retry(i32, i32, i32, i32, i32, i32, i64, i64, i64, i64, i32, i32, i32, i32) => i64
        return cleat_call_retry(svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen, maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs, nonRetryableErrorsPtr, nonRetryableErrorsLen, respPtr, respMaxLen) || 0n;
      },
      cleat_defer(descPtr, descLen, deferIdPtr, deferIdMaxLen) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_defer(i32, i32, i32, i32) => i64
        return cleat_defer(descPtr, descLen, deferIdPtr, deferIdMaxLen) || 0n;
      },
      cleat_schedule_cron(workflowNamePtr, workflowNameLen, cronExprPtr, cronExprLen, tzPtr, tzLen, inputPtr, inputLen, scheduleIdOutPtr, scheduleIdOutMax) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_schedule_cron(i32, i32, i32, i32, i32, i32, i32, i32, i32, i32) => i64
        return cleat_schedule_cron(workflowNamePtr, workflowNameLen, cronExprPtr, cronExprLen, tzPtr, tzLen, inputPtr, inputLen, scheduleIdOutPtr, scheduleIdOutMax) || 0n;
      },
      cleat_list_crons(outPtr, outMaxLen) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_list_crons(i32, i32) => i64
        return cleat_list_crons(outPtr, outMaxLen) || 0n;
      },
      plugin_call(pluginNamePtr, pluginNameLen, functionNamePtr, functionNameLen, inputPtr, inputLen, responsePtr, responseMaxLen) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_plugin_call(i32, i32, i32, i32, i32, i32, i32, i32) => i64
        return plugin_call(pluginNamePtr, pluginNameLen, functionNamePtr, functionNameLen, inputPtr, inputLen, responsePtr, responseMaxLen) || 0n;
      },
      plugin_call_streaming(pluginNamePtr, pluginNameLen, functionNamePtr, functionNameLen, inputPtr, inputLen, responsePtr, responseMaxLen) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_plugin_call_streaming(i32, i32, i32, i32, i32, i32, i32, i32) => i64
        return plugin_call_streaming(pluginNamePtr, pluginNameLen, functionNamePtr, functionNameLen, inputPtr, inputLen, responsePtr, responseMaxLen) || 0n;
      },
      cleat_acquire_lock(keyPtr, keyLen, ttlMs) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_acquire_lock(i32, i32, i64) => i64
        return cleat_acquire_lock(keyPtr, keyLen, ttlMs) || 0n;
      },
      cleat_workflow_id(idPtr, idMaxLen) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_workflow_id(i32, i32) => i64
        return cleat_workflow_id(idPtr, idMaxLen) || 0n;
      },
      cleat_run_id(idPtr, idMaxLen) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_run_id(i32, i32) => i64
        return cleat_run_id(idPtr, idMaxLen) || 0n;
      },
      cleat_poll_cancellation(reasonPtr, reasonMaxLen) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_poll_cancellation(i32, i32) => i64
        return cleat_poll_cancellation(reasonPtr, reasonMaxLen) || 0n;
      },
      cleat_side_effect(resultPtr, resultLen, outPtr, outMaxLen) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_side_effect(i32, i32, i32, i32) => i64
        return cleat_side_effect(resultPtr, resultLen, outPtr, outMaxLen) || 0n;
      },
      cleat_defer_phase(on) {
        // ~lib/@cleat/sdk/assembly/host-calls/import_cleat_defer_phase(i32) => i64
        return cleat_defer_phase(on) || 0n;
      },
    }, Object.assign(Object.create(globalThis), imports.env || {})),
  };
  const { exports } = await WebAssembly.instantiate(module, adaptedImports);
  const memory = exports.memory || imports.env.memory;
  const adaptedExports = Object.setPrototypeOf({
    __durable_inner_exercise_host_call(h, input) {
      // assembly/index/__durable_inner_exercise_host_call(~lib/@cleat/sdk/assembly/host-calls/HostCalls, ~lib/string/String) => ~lib/string/String
      h = __retain(__lowerInternref(h) || __notnull());
      input = __lowerString(input) || __notnull();
      try {
        return __liftString(exports.__durable_inner_exercise_host_call(h, input) >>> 0);
      } finally {
        __release(h);
      }
    },
  }, exports);
  function __liftString(pointer) {
    if (!pointer) return null;
    const
      end = pointer + new Uint32Array(memory.buffer)[pointer - 4 >>> 2] >>> 1,
      memoryU16 = new Uint16Array(memory.buffer);
    let
      start = pointer >>> 1,
      string = "";
    while (end - start > 1024) string += String.fromCharCode(...memoryU16.subarray(start, start += 1024));
    return string + String.fromCharCode(...memoryU16.subarray(start, end));
  }
  function __lowerString(value) {
    if (value == null) return 0;
    const
      length = value.length,
      pointer = exports.__new(length << 1, 2) >>> 0,
      memoryU16 = new Uint16Array(memory.buffer);
    for (let i = 0; i < length; ++i) memoryU16[(pointer >>> 1) + i] = value.charCodeAt(i);
    return pointer;
  }
  class Internref extends Number {}
  function __lowerInternref(value) {
    if (value == null) return 0;
    if (value instanceof Internref) return value.valueOf();
    throw TypeError("internref expected");
  }
  const refcounts = new Map();
  function __retain(pointer) {
    if (pointer) {
      const refcount = refcounts.get(pointer);
      if (refcount) refcounts.set(pointer, refcount + 1);
      else refcounts.set(exports.__pin(pointer), 1);
    }
    return pointer;
  }
  function __release(pointer) {
    if (pointer) {
      const refcount = refcounts.get(pointer);
      if (refcount === 1) exports.__unpin(pointer), refcounts.delete(pointer);
      else if (refcount) refcounts.set(pointer, refcount - 1);
      else throw Error(`invalid refcount '${refcount}' for reference '${pointer}'`);
    }
  }
  function __notnull() {
    throw TypeError("value must not be null");
  }
  return adaptedExports;
}
