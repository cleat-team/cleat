package cleat;

import java.nio.charset.StandardCharsets;
import org.teavm.interop.Import;

/**
 * High-level wrapper around all 15 cleat WASM host function imports.
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

    // ========================================================================
    // Raw WASM imports (15 host functions from the "env" module)
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

    @Import(module = "env", name = "durable_child_workflow")
    private static native long durableChildWorkflowRaw(
        int namePtr, int nameLen,
        int inPtr, int inLen,
        int outPtr, int maxLen);

    @Import(module = "env", name = "durable_await_child")
    private static native long durableAwaitChildRaw(
        int runIdPtr, int runIdLen,
        int outPtr, int maxLen);

    @Import(module = "env", name = "durable_await_signals")
    private static native long durableAwaitSignalsRaw(
        int namesPtr, int namesLen, long timeoutMs,
        int sigNameOut, int sigNameMax,
        int payloadOut, int payloadMax);

    @Import(module = "env", name = "set_query_state")
    private static native long setQueryStateRaw(
        int keyPtr, int keyLen, int valPtr, int valLen);

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
     * @return milliseconds since Unix epoch (32-bit value, zero-extended)
     */
    public long now() {
        long result = durableNowRaw();
        return Memory.decodeSimpleExtra(result) & 0xFFFFFFFFL;
    }

    /**
     * Get a deterministic random value.
     * <p>
     * The same value is returned on replay as during the original execution.
     * The value is a 32-bit unsigned integer, zero-extended to {@code long}.
     *
     * @return a deterministic 32-bit random value
     */
    public long random() {
        long result = durableRandomRaw();
        return Memory.decodeSimpleExtra(result) & 0xFFFFFFFFL;
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
        return Memory.decodeSimpleExtra(result);
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
        return Memory.decodeSimpleExtra(result);
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
}
