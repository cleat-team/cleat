package cleat;

import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import org.teavm.interop.Import;

/**
 * High-level wrapper around all 18 cleat WASM host function imports.
 * <p>
 * Each WASM host function is imported from the {@code "env"} module via
 * {@link Import @Import}.  The raw native methods return packed {@code long}
 * values; the public wrapper methods unpack the results and present a
 * Java-friendly API.
 * <p>
 * <strong>Memory convention:</strong> Input strings are written to the
 * scratch region at {@link Memory#SCRATCH_BASE} before each host call.
 * Output strings are read from {@link Memory#OUTPUT_OFFSET}.  The scratch
 * region is a single-threaded buffer — safe because WASM execution is
 * single-threaded.
 * <p>
 * <strong>Thread safety:</strong> This class is <em>not</em> thread-safe.
 * WASM modules execute on a single thread, so concurrent access is not
 * expected.
 * <p>
 * <strong>Usage:</strong> Workflow entry-point methods receive a
 * {@code HostCalls} instance as their first parameter:
 * <pre>{@code
 * @DurableEntry(name = "place_order")
 * public static String placeOrder(HostCalls h, String input) {
 *     h.durableLog("Processing order");
 *     DurableResult<String> reserved = h.durableCall("inventory", "Reserve", input);
 *     if (reserved.isErr()) {
 *         return "{\"error\": \"reservation failed\"}";
 *     }
 *     return reserved.getValue();
 * }
 * }</pre>
 *
 * @see DurableEntry
 * @see DurableResult
 * @see Memory
 */
public class HostCalls {

    /** Current scope prefix for virtual object state operations. */
    private String _scopePrefix = "";

    // ========================================================================
    // Raw WASM imports (18 host functions from the "env" module)
    // ========================================================================

    @Import(module = "env", name = "durable_call")
    private static native long durableCallRaw(
        int svcPtr, int svcLen,
        int opPtr, int opLen,
        int reqPtr, int reqLen,
        int respPtr, int respMaxLen);

    @Import(module = "env", name = "durable_sleep")
    private static native long durableSleepRaw(long durationMs);

    @Import(module = "env", name = "durable_now")
    private static native long durableNowRaw();

    @Import(module = "env", name = "durable_random")
    private static native long durableRandomRaw();

    @Import(module = "env", name = "durable_log")
    private static native long durableLogRaw(int msgPtr, int msgLen);

    @Import(module = "env", name = "durable_version")
    private static native long durableVersionRaw();

    @Import(module = "env", name = "durable_min_version")
    private static native long durableMinVersionRaw();

    @Import(module = "env", name = "durable_defer")
    private static native long durableDeferRaw(
        int descPtr, int descLen, int outPtr, int maxLen);

    @Import(module = "env", name = "durable_poll_cancellation")
    private static native long durablePollCancellationRaw(int outPtr, int maxLen);

    @Import(module = "env", name = "durable_poll_signal")
    private static native long durablePollSignalRaw(
        int namePtr, int nameLen, int outPtr, int maxLen);

    @Import(module = "env", name = "durable_continue_as_new")
    private static native long durableContinueAsNewRaw(int inPtr, int inLen);

    @Import(module = "env", name = "durable_create_promise")
    private static native long durableCreatePromiseRaw(
        int namePtr, int nameLen, int idOutPtr, int idOutMax);

    @Import(module = "env", name = "durable_child_workflow")
    private static native long durableChildWorkflowRaw(
        int namePtr, int nameLen,
        int inPtr, int inLen,
        int outPtr, int maxLen);

    @Import(module = "env", name = "durable_await_child")
    private static native long durableAwaitChildRaw(
        int runIdPtr, int runIdLen,
        int outPtr, int maxLen);

    @Import(module = "env", name = "durable_await_promise")
    private static native long durableAwaitPromiseRaw(
        int idPtr, int idLen, long timeoutMs,
        int resultOutPtr, int resultOutMax);

    @Import(module = "env", name = "durable_await_signals")
    private static native long durableAwaitSignalsRaw(
        int namesPtr, int namesLen, long timeoutMs,
        int sigNameOut, int sigNameMax,
        int payloadOut, int payloadMax);

    @Import(module = "env", name = "durable_register_update_handler")
    private static native long durableRegisterUpdateHandlerRaw(
        int namePtr, int nameLen);

    @Import(module = "env", name = "set_query_state")
    private static native long setQueryStateRaw(
        int keyPtr, int keyLen, int valPtr, int valLen);

    @Import(module = "env", name = "get_query_state")
    private static native long getQueryStateRaw(int keyPtr, int keyLen, int outPtr, int outMaxLen);

    @Import(module = "env", name = "plugin_call")
    private static native long importPluginCall(
        int pluginNamePtr, int pluginNameLen,
        int functionNamePtr, int functionNameLen,
        int inputPtr, int inputLen,
        int responsePtr, int responseMaxLen);

    @Import(module = "env", name = "durable_workflow_id")
    private static native long durableWorkflowIdRaw(int outPtr, int maxLen);

    @Import(module = "env", name = "durable_run_id")
    private static native long durableRunIdRaw(int outPtr, int maxLen);

    // ========================================================================
    // Internal helpers: pack strings in scratch region, read output buffer
    // ========================================================================

    /**
     * Writes multiple strings consecutively into the scratch region at
     * {@link Memory#SCRATCH_BASE}, returning their offsets and lengths.
     * <p>
     * Example: writing {@code ["svc", "op"]} at {@code SCRATCH_BASE} produces:
     * <pre>
     * offset 0: [s][v][c]
     * offset 3: [o][p]
     * </pre>
     * Returns {@code [off0, off1, len0, len1]} = {@code [0, 3, 3, 2]}.
     *
     * @param strings the strings to write consecutively into scratch memory
     * @return an array of 2*N ints: first N are offsets, remaining N are
     *         byte lengths
     */
    private static int[] packStrings(String... strings) {
        int count = strings.length;
        int[] offsets = new int[count];
        int[] lengths = new int[count];
        int current = Memory.SCRATCH_BASE;

        for (int i = 0; i < count; i++) {
            String s = strings[i];
            if (s == null) {
                s = "";
            }
            offsets[i] = current;
            byte[] bytes = s.getBytes(StandardCharsets.UTF_8);
            lengths[i] = bytes.length;
            int written = Memory.writeString(current, Memory.OUT_BUF_SIZE, s);
            current += written;
        }

        // Flatten into single array: [offsets..., lengths...]
        int[] result = new int[count * 2];
        for (int i = 0; i < count; i++) {
            result[i] = offsets[i];
            result[i + count] = lengths[i];
        }
        return result;
    }

    /**
     * Read a string from the output buffer ({@link Memory#OUTPUT_OFFSET}),
     * clamped to the buffer size.
     *
     * @param maxLen the number of bytes to read
     * @return the decoded string, or empty if maxLen is zero
     */
    private static String readOutput(int maxLen) {
        if (maxLen <= 0) {
            return "";
        }
        int clamped = Math.min(maxLen, Memory.OUT_BUF_SIZE);
        return Memory.readString(Memory.OUTPUT_OFFSET, clamped);
    }

    // ========================================================================
    // Public API methods
    // ========================================================================

    /**
     * Make a durable (deterministically replayed) call to an external service.
     * <p>
     * The call is recorded in the workflow event history.  On replay, the
     * recorded response is returned without making the real call, ensuring
     * deterministic re-execution.
     *
     * @param service     the service name (e.g. {@code "orders"})
     * @param operation   the operation name (e.g. {@code "create"})
     * @param requestJSON the JSON request payload
     * @return a result containing the JSON response on success, or an error
     *         description on failure
     */
    public DurableResult<String> durableCall(String service, String operation, String requestJSON) {
        int[] p = packStrings(service, operation, requestJSON);
        int svcOff = p[0], opOff = p[1], reqOff = p[2];
        int svcLen = p[3], opLen = p[4], reqLen = p[5];

        long result = durableCallRaw(
            svcOff, svcLen,
            opOff, opLen,
            reqOff, reqLen,
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        int errCode = Memory.decodeCallErrCode(result);
        int responseLen = Memory.decodeCallResponseLen(result);

        if (errCode != 0) {
            String errMsg = readOutput(responseLen);
            return DurableResult.err(errMsg);
        }

        String response = readOutput(responseLen);
        return DurableResult.ok(response);
    }

    /**
     * Suspend workflow execution for a duration.
     * <p>
     * On replay ({@link Memory#SLEEP_STATUS_COMPLETED}), this returns
     * {@code false} — the workflow should continue without actually sleeping.
     * On fresh execution ({@link Memory#SLEEP_STATUS_SUSPEND}), returns
     * {@code true} — the workflow should propagate the suspension by
     * returning {@link Memory#SUSPEND_SENTINEL} from the export.
     *
     * @param durationMs sleep duration in milliseconds
     * @return {@code true} if the workflow should suspend (fresh execution),
     *         {@code false} to continue (replay)
     */
    public boolean durableSleep(long durationMs) {
        long result = durableSleepRaw(durationMs);
        int status = Memory.decodeSleepStatus(result);
        return status == Memory.SLEEP_STATUS_SUSPEND;
    }

    /**
     * Get the deterministic current wall-clock time.
     * <p>
     * Returns the time in milliseconds since the Unix epoch.  The same value
     * is returned on replay as during the original execution.
     *
     * @return milliseconds since Unix epoch (full 64-bit value)
     */
    public long now() {
        return durableNowRaw();
    }

    /**
     * Get a deterministic random value.
     * <p>
     * The same value is returned on replay as during the original execution.
     * The value is a full 64-bit integer.
     *
     * @return a deterministic 64-bit random value
     */
    public long random() {
        return durableRandomRaw();
    }

    /**
     * Log a message to the workflow event history.
     * <p>
     * Log messages are recorded deterministically and replayed during
     * workflow re-execution.  This is intended for debugging and
     * observability, not for side-effect logic.
     *
     * @param message the log message (must not be null)
     */
    public void durableLog(String message) {
        int[] p = packStrings(message);
        durableLogRaw(p[0], p[1]);
    }

    /**
     * Get the current workflow definition version.
     * <p>
     * Used for versioned workflow deployments.  The version is set when the
     * workflow is compiled and deployed.
     *
     * @return the workflow definition version (unsigned 32-bit)
     */
    public int version() {
        long result = durableVersionRaw();
        return (int) (result & 0xFFFFFFFFL);
    }

    /**
     * Get the minimum supported workflow definition version.
     * <p>
     * Workflow instances at versions below this threshold are either migrated
     * to a newer version or rejected by the host.
     *
     * @return the minimum supported version (unsigned 32-bit)
     */
    public int minVersion() {
        long result = durableMinVersionRaw();
        return (int) (result & 0xFFFFFFFFL);
    }

    /**
     * Register a deferred cleanup callback to run when the workflow exits.
     * <p>
     * Deferred callbacks are executed in LIFO order (last-registered,
     * first-executed), analogous to Go's {@code defer} or Java's
     * {@code try/finally}.  The returned defer ID can be used to cancel
     * the deferred action before the workflow completes.
     *
     * @param description a human-readable description of the cleanup action
     * @return a result containing the defer ID on success, or an error
     *         description on failure
     */
    public DurableResult<String> durableDefer(String description) {
        int[] p = packStrings(description);

        long result = durableDeferRaw(
            p[0], p[1],
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        int errCode = Memory.decodeSimpleErrCode(result);
        int deferIdLen = Memory.decodeSimpleExtra(result);

        if (errCode != 0) {
            return DurableResult.err("defer failed with code " + errCode);
        }

        String deferId = readOutput(deferIdLen);
        return DurableResult.ok(deferId);
    }

    /**
     * Poll whether a cancellation has been requested for this workflow.
     * <p>
     * Workflows should periodically check for cancellation and perform
     * cleanup if cancelled.
     *
     * @return a result whose value is {@code true} if cancellation has been
     *         requested, {@code false} otherwise
     */
    public DurableResult<Boolean> pollCancellation() {
        long result = durablePollCancellationRaw(Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        boolean cancelled = Memory.decodePollCancelCancelled(result);
        int reasonLen = Memory.decodePollCancelReasonLen(result);

        // The cancellation reason is written to the output buffer when
        // cancelled.  We do not propagate the reason here; callers that
        // need it can expand this wrapper.
        return DurableResult.ok(cancelled);
    }

    /**
     * Poll for a specific pending external signal.
     * <p>
     * Unlike {@link #awaitSignals(String[], long)}, this call is non-blocking
     * and checks once.  If no signal is pending with the given name, the
     * result carries an error.
     *
     * @param signalName the signal name to look up
     * @return a result containing the signal payload if found, or an error
     *         if the signal is not pending
     */
    public DurableResult<String> pollSignal(String signalName) {
        int[] p = packStrings(signalName);

        long result = durablePollSignalRaw(
            p[0], p[1],
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        boolean found = Memory.decodePollSigFound(result);
        if (!found) {
            return DurableResult.err("signal not found: " + signalName);
        }

        int payloadLen = Memory.decodePollSigPayloadLen(result);
        String payload = readOutput(payloadLen);
        return DurableResult.ok(payload);
    }

    /**
     * Replace the current workflow's input and restart execution from the
     * beginning ("continue-as-new").
     * <p>
     * This is used for workflow history compaction — after many events, the
     * workflow can compact its history by starting fresh with new input.
     * After this call, the workflow should return
     * {@link Memory#SUSPEND_SENTINEL} to signal the host.
     *
     * @param newInputJSON the new input JSON for the restarted workflow
     * @return a result indicating success, or an error description
     */
    public DurableResult<Void> continueAsNew(String newInputJSON) {
        int[] p = packStrings(newInputJSON);

        long result = durableContinueAsNewRaw(p[0], p[1]);

        int errCode = Memory.decodeSimpleErrCode(result);
        if (errCode != 0) {
            return DurableResult.err("continueAsNew failed with code " + errCode);
        }
        return DurableResult.ok(null);
    }

    /**
     * Create a promise with the given name.
     * <p>
     * Promises are durable, first-class entities in cleat. They can be
     * resolved by this or another workflow, and awaited by one or more
     * workflows using {@link #awaitPromise(String, long)}.
     *
     * @param name the promise name
     * @return a result containing the promise ID on success, or an error
     *         description on failure
     */
    public DurableResult<String> createPromise(String name) {
        int[] p = packStrings(name);

        long result = durableCreatePromiseRaw(
            p[0], p[1],
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        int errCode = Memory.decodeSimpleErrCode(result);
        int idLen = Memory.decodeSimpleExtra(result);

        if (errCode != 0) {
            return DurableResult.err("createPromise failed with code " + errCode);
        }

        String promiseId = readOutput(idLen);
        return DurableResult.ok(promiseId);
    }

    /**
     * Start a child workflow execution.
     * <p>
     * The child runs asynchronously.  Use {@link #awaitChild(String)} to
     * wait for its completion and retrieve the result.
     *
     * @param name      the child workflow type/name
     * @param inputJSON the input JSON for the child workflow
     * @return a result containing the child's run ID on success, or an error
     *         description on failure
     */
    public DurableResult<String> childWorkflow(String name, String inputJSON) {
        int[] p = packStrings(name, inputJSON);
        int nameOff = p[0], inOff = p[1];
        int nameLen = p[2], inLen = p[3];

        long result = durableChildWorkflowRaw(
            nameOff, nameLen,
            inOff, inLen,
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        int errCode = Memory.decodeSimpleErrCode(result);
        int runIdLen = Memory.decodeSimpleExtra(result);

        if (errCode != 0) {
            return DurableResult.err("childWorkflow failed with code " + errCode);
        }

        String runId = readOutput(runIdLen);
        return DurableResult.ok(runId);
    }

    /**
     * Wait for a child workflow to complete and retrieve its result.
     * <p>
     * If the child has not yet completed, this call triggers a workflow
     * suspension.  The host resumes the workflow when the child finishes.
     *
     * @param runID the child workflow run ID (from
     *              {@link #childWorkflow(String, String)})
     * @return a result containing the child's output JSON on success, or an
     *         error description on failure
     */
    public DurableResult<String> awaitChild(String runID) {
        int[] p = packStrings(runID);

        long result = durableAwaitChildRaw(
            p[0], p[1],
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        int errCode = Memory.decodeSimpleErrCode(result);
        int resultLen = Memory.decodeSimpleExtra(result);

        if (errCode != 0) {
            return DurableResult.err("awaitChild failed with code " + errCode);
        }

        String childResult = readOutput(resultLen);
        return DurableResult.ok(childResult);
    }

    /**
     * Wait for a promise to resolve, with an optional timeout.
     * <p>
     * Blocks until the promise with the given ID is resolved or the timeout
     * expires. On timeout, {@link AwaitPromiseResult#timedOut} is
     * {@code true}.
     *
     * @param promiseId the promise ID to wait for
     * @param timeoutMs maximum wait time in milliseconds (use
     *                  {@link Long#MAX_VALUE} for no timeout)
     * @return a result containing an {@link AwaitPromiseResult} with the
     *         resolved value and timeout indicator
     */
    public DurableResult<AwaitPromiseResult> awaitPromise(String promiseId, long timeoutMs) {
        int[] p = packStrings(promiseId);

        long result = durableAwaitPromiseRaw(
            p[0], p[1], timeoutMs,
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        int errCode = Memory.decodeAwaitErrCode(result);
        boolean timedOut = Memory.decodeAwaitPromiseTimedOut(result);
        int resultLen = Memory.decodeSimpleExtra(result);

        if (errCode != 0) {
            return DurableResult.err("awaitPromise failed with code " + errCode);
        }

        String promiseResult = readOutput(resultLen);
        return DurableResult.ok(new AwaitPromiseResult(promiseResult, timedOut));
    }

    /**
     * Wait for one or more external signals, with an optional timeout.
     * <p>
     * Blocks until one of the named signals is received or the timeout
     * expires.  On timeout, {@link AwaitSignalsResult#timedOut} is
     * {@code true}.
     * <p>
     * The signal names are serialized internally as a JSON string array
     * (e.g. {@code ["payment_received","order_cancelled"]}).  The returned
     * signal name and payload are from whichever signal arrives first.
     *
     * @param signalNames the signal names to wait for
     * @param timeoutMs   maximum wait time in milliseconds (use
     *                    {@link Long#MAX_VALUE} for no timeout)
     * @return a result containing an {@link AwaitSignalsResult} with the
     *         received signal name, payload, and timeout indicator
     */
    public DurableResult<AwaitSignalsResult> awaitSignals(String[] signalNames, long timeoutMs) {
        // Serialize signal names as a JSON string array (matching Go adapter).
        StringBuilder sb = new StringBuilder("[");
        for (int i = 0; i < signalNames.length; i++) {
            if (i > 0) {
                sb.append(",");
            }
            sb.append("\"").append(JsonHelper.escapeJson(signalNames[i])).append("\"");
        }
        sb.append("]");
        String namesJSON = sb.toString();

        int[] p = packStrings(namesJSON);
        int namesOff = p[0];
        int namesLen = p[1];

        // Split the output buffer: first 1024 bytes for signal name, rest for
        // payload.
        final int sigNameBufSize = 1024;
        final int payloadBufOffset = Memory.OUTPUT_OFFSET + sigNameBufSize;
        final int payloadBufSize = Memory.OUT_BUF_SIZE - sigNameBufSize;

        long result = durableAwaitSignalsRaw(
            namesOff, namesLen, timeoutMs,
            Memory.OUTPUT_OFFSET, sigNameBufSize,
            payloadBufOffset, payloadBufSize);

        int errCode = Memory.decodeAwaitErrCode(result);
        boolean timedOut = Memory.decodeAwaitTimedOut(result);
        int sigNameLen = Memory.decodeAwaitSigNameLen(result);
        int payloadLen = Memory.decodeAwaitPayloadLen(result);

        if (errCode != 0) {
            return DurableResult.err("awaitSignals failed with code " + errCode);
        }

        String sigName = Memory.readString(Memory.OUTPUT_OFFSET,
            Math.min(sigNameLen, sigNameBufSize));
        String payload = Memory.readString(payloadBufOffset,
            Math.min(payloadLen, payloadBufSize));

        return DurableResult.ok(new AwaitSignalsResult(sigName, payload, timedOut));
    }

    /**
     * Register a handler for a named update.
     * <p>
     * External clients can send updates to this workflow using the cleat
     * update API. The registered handler is invoked when an update is
     * received with the matching name.
     *
     * @param name the update handler name
     */
    public void registerUpdateHandler(String name) {
        int[] p = packStrings(name);
        durableRegisterUpdateHandlerRaw(p[0], p[1]);
    }

    /**
     * Set a key-value pair in the workflow's queryable state.
     * <p>
     * External clients can query this state while the workflow is running or
     * after completion using the cleat query API.
     *
     * @param key   the state key
     * @param value the state value (typically a JSON string)
     */
    public void setQueryState(String key, String value) {
        int[] p = packStrings(key, value);
        int keyOff = p[0], valOff = p[1];
        int keyLen = p[2], valLen = p[3];
        setQueryStateRaw(keyOff, keyLen, valOff, valLen);
    }

    /**
     * Get a key-value pair from the workflow's queryable state.
     * <p>
     * Retrieves the state value previously set via
     * {@link #setQueryState(String, String)}.
     *
     * @param key the state key
     * @return a result containing the state value on success, or an error
     *         description on failure
     */
    public DurableResult<String> getQueryState(String key) {
        int[] p = packStrings(key);
        int keyOff = p[0], keyLen = p[1];

        long result = getQueryStateRaw(
            keyOff, keyLen,
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        int errCode = Memory.decodeCallErrCode(result);
        int responseLen = Memory.decodeCallResponseLen(result);

        if (errCode != 0) {
            String errMsg = readOutput(responseLen);
            return DurableResult.err(errMsg);
        }

        String response = readOutput(responseLen);
        return DurableResult.ok(response);
    }

    // ========================================================================
    // plugin_call — call a plugin host function (ABI 2.19)
    // ========================================================================

    /**
     * Call a plugin host function and return the response JSON string.
     * <p>
     * Plugins extend the host runtime with custom functionality beyond the
     * standard host imports.  Unlike {@link #durableCall(String, String, String)},
     * plugin calls are <em>not</em> recorded in the workflow event history
     * and are not replayed.
     *
     * @param pluginName   name of the plugin (e.g. {@code "blobstore"},
     *                     {@code "slacknotify"})
     * @param functionName name of the function within the plugin
     *                     (e.g. {@code "put"}, {@code "send_message"})
     * @param inputJson    input JSON for the plugin function
     * @return the plugin function's response as a JSON string
     * @throws RuntimeException if the host reports an error from the plugin
     *                          call
     */
    public String pluginCall(String pluginName, String functionName, String inputJson) {
        int[] p = packStrings(pluginName, functionName, inputJson);
        int pnOff = p[0], fnOff = p[1], inOff = p[2];
        int pnLen = p[3], fnLen = p[4], inLen = p[5];

        long result = importPluginCall(
            pnOff, pnLen,
            fnOff, fnLen,
            inOff, inLen,
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        int errCode = Memory.decodeCallErrCode(result);
        int responseLen = Memory.decodeCallResponseLen(result);

        if (errCode != 0) {
            String errMsg = readOutput(responseLen);
            throw new RuntimeException("plugin_call failed: " + errMsg);
        }

        return readOutput(responseLen);
    }

    /**
     * Call a plugin host function and return a structured
     * {@link PluginCallOutcome} with the response, error message, and
     * call-level error code.
     * <p>
     * Unlike {@link #pluginCall(String, String, String)}, this method does
     * <em>not</em> throw on protocol errors.  Instead, the error is reported
     * in the returned {@link PluginCallOutcome#error} field, allowing callers
     * to inspect both the response and error information without exception
     * handling.
     *
     * @param pluginName   name of the plugin
     * @param functionName name of the function within the plugin
     * @param inputJson    input JSON for the plugin function
     * @return a {@link PluginCallOutcome} with response, error, and
     *         call-level error code
     */
    public PluginCallOutcome pluginCallOutcome(String pluginName, String functionName, String inputJson) {
        int[] p = packStrings(pluginName, functionName, inputJson);
        int pnOff = p[0], fnOff = p[1], inOff = p[2];
        int pnLen = p[3], fnLen = p[4], inLen = p[5];

        long result = importPluginCall(
            pnOff, pnLen,
            fnOff, fnLen,
            inOff, inLen,
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        int errCode = Memory.decodeCallErrCode(result);
        int callErrorCode = Memory.decodeCallErrorCode(result);
        int responseLen = Memory.decodeCallResponseLen(result);

        if (errCode != 0) {
            String errMsg = readOutput(responseLen);
            return new PluginCallOutcome(null, errMsg, callErrorCode);
        }

        String response = readOutput(responseLen);
        return new PluginCallOutcome(response, null, callErrorCode);
    }

    // ========================================================================
    // Scope management for virtual object instances
    // ========================================================================

    /**
     * Get the current workflow ID from the host runtime.
     *
     * @return the workflow ID string
     */
    public String currentWorkflowId() {
        long result = durableWorkflowIdRaw(Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);
        int errCode = Memory.decodeSimpleErrCode(result);
        int idLen = Memory.decodeSimpleExtra(result);
        if (errCode != 0 || idLen == 0) {
            return "";
        }
        return readOutput(idLen);
    }

    /**
     * Set the state key prefix for virtual object instances.
     * All subsequent state operations are automatically prefixed
     * with "vo:&lt;objectType&gt;:&lt;instanceKey&gt;:".
     *
     * @param objectType  the virtual object type name
     * @param instanceKey the instance key for this specific object
     * @return the previous scope prefix (empty string if none was set)
     */
    public String setScope(String objectType, String instanceKey) {
        String prev = this._scopePrefix;
        this._scopePrefix = (objectType != null && !objectType.isEmpty()
            && instanceKey != null && !instanceKey.isEmpty())
            ? "vo:" + objectType + ":" + instanceKey + ":"
            : "";
        return prev;
    }

    /**
     * Get the current virtual object scope.
     *
     * @return a two-element array {@code [objectType, instanceKey]}, or
     *         {@code ["", ""]} if no scope is set
     */
    public String[] getScope() {
        if (this._scopePrefix.isEmpty()) {
            return new String[]{"", ""};
        }
        // Parse "vo:<type>:<key>:" format
        String trimmed = this._scopePrefix.substring(0, this._scopePrefix.length() - 1);
        String[] parts = trimmed.split(":", 3);
        if (parts.length == 3 && "vo".equals(parts[0])) {
            return new String[]{parts[1], parts[2]};
        }
        return new String[]{"", ""};
    }

    /**
     * Remove the current scope and return the previous scope prefix.
     *
     * @return the scope prefix that was active before clearing (empty string
     *         if none was set)
     */
    public String clearScope() {
        String prev = this._scopePrefix;
        this._scopePrefix = "";
        return prev;
    }

    /**
     * Return a deterministic UUID scoped to the current workflow
     * and the given seed. The same seed always produces the same UUID
     * for this workflow instance.
     * <p>
     * Uses SHA-256 of "{workflowID}:{seed}" to produce a UUIDv5-formatted
     * string.
     *
     * @param seed a seed string that determines the UUID within this workflow
     * @return a UUID-formatted string
     */
    public String uuid(String seed) {
        String wfId = this.currentWorkflowId();
        String data = wfId + ":" + seed;
        try {
            MessageDigest md = MessageDigest.getInstance("SHA-256");
            byte[] hash = md.digest(data.getBytes(StandardCharsets.UTF_8));

            // Take first 16 bytes and set version/variant bits
            hash[6] = (byte) ((hash[6] & 0x0f) | 0x50); // Version 5
            hash[8] = (byte) ((hash[8] & 0x3f) | 0x80); // Variant 1

            // Format as UUID: 8-4-4-4-12
            StringBuilder sb = new StringBuilder(36);
            for (int i = 0; i < 16; i++) {
                if (i == 4 || i == 6 || i == 8 || i == 10) {
                    sb.append('-');
                }
                sb.append(String.format("%02x", hash[i] & 0xff));
            }
            return sb.toString();
        } catch (NoSuchAlgorithmException e) {
            // Fallback: improbable - SHA-256 is always available
            return "00000000-0000-0000-0000-000000000000";
        }
    }

    // ========================================================================
    // Inner result type for awaitSignals
    // ========================================================================

    /**
     * Result of an {@link #awaitSignals(String[], long)} call.
     * <p>
     * Contains the signal that was received (or empty if timed out), its
     * payload, and whether the timeout expired before any signal arrived.
     */
    public static class AwaitSignalsResult {
        /** The name of the signal that was received (empty if timed out). */
        public final String signalName;

        /** The payload of the received signal (empty if timed out). */
        public final String payload;

        /** {@code true} if the timeout expired before any signal arrived. */
        public final boolean timedOut;

        /**
         * Construct a new await-signals result.
         *
         * @param signalName the received signal name
         * @param payload    the signal payload
         * @param timedOut   whether the wait timed out
         */
        public AwaitSignalsResult(String signalName, String payload, boolean timedOut) {
            this.signalName = signalName;
            this.payload = payload;
            this.timedOut = timedOut;
        }

        @Override
        public String toString() {
            if (timedOut) {
                return "AwaitSignalsResult(timedOut)";
            }
            return "AwaitSignalsResult(signalName=" + signalName
                + ", payload=" + payload + ")";
        }
    }

    /**
     * Result of an {@link #awaitPromise(String, long)} call.
     * <p>
     * Contains the promise's resolved value (or empty if timed out), and
     * whether the timeout expired before the promise was resolved.
     */
    public static class AwaitPromiseResult {
        /** The resolved value of the promise (empty if timed out). */
        public final String result;

        /** {@code true} if the timeout expired before the promise resolved. */
        public final boolean timedOut;

        /**
         * Construct a new await-promise result.
         *
         * @param result   the resolved value
         * @param timedOut whether the wait timed out
         */
        public AwaitPromiseResult(String result, boolean timedOut) {
            this.result = result;
            this.timedOut = timedOut;
        }

        @Override
        public String toString() {
            if (timedOut) {
                return "AwaitPromiseResult(timedOut)";
            }
            return "AwaitPromiseResult(result=" + result + ")";
        }
    }

    // ========================================================================
    // plugin_call outcome type
    // ========================================================================

    /**
     * Result of a {@link #pluginCallOutcome(String, String, String)} call.
     * <p>
     * Contains the plugin function's response JSON (on success), an error
     * message (on failure), and the call-level error code.  Use
     * {@link #isError()} to check whether the call succeeded.
     */
    public static class PluginCallOutcome {
        /** The plugin function's response JSON, or {@code null} on error. */
        public final String response;

        /** The error message, or {@code null} on success. */
        public final String error;

        /** The call-level error code (0 for success). */
        public final int callErrorCode;

        /**
         * Construct a new plugin call outcome.
         *
         * @param response      the response JSON, or {@code null} on error
         * @param error         the error message, or {@code null} on success
         * @param callErrorCode the call-level error code
         */
        public PluginCallOutcome(String response, String error, int callErrorCode) {
            this.response = response;
            this.error = error;
            this.callErrorCode = callErrorCode;
        }

        /**
         * Returns {@code true} if the plugin call resulted in an error.
         *
         * @return {@code true} if {@link #error} is non-null
         */
        public boolean isError() {
            return error != null;
        }

        @Override
        public String toString() {
            if (isError()) {
                return "PluginCallOutcome(error=" + error
                    + ", callErrorCode=" + callErrorCode + ")";
            }
            return "PluginCallOutcome(response=" + response + ")";
        }
    }
}
