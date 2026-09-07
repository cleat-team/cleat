package cleat;

import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.Map;
import org.teavm.interop.Import;

/**
 * High-level wrapper around all cleat WASM host function imports.
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
 * {@code HostCalls} instance as their first parameter. Return a {@link Map},
 * not a {@link String} holding hand-built JSON text -- the generated export
 * wrapper calls {@link JsonHelper#stringify(Object)} on the return value, and
 * stringifying a String that already contains JSON produces a JSON string
 * containing JSON rather than an object:
 * <pre>{@code
 * @CleatEntry(name = "place_order")
 * public static Map<String, Object> placeOrder(HostCalls h, String input) {
 *     h.cleatLog("Processing order");
 *     CleatResult<String> reserved = h.cleatCall("inventory", "Reserve", input);
 *     if (reserved.isErr()) {
 *         return Map.of("error", "reservation failed");
 *     }
 *     // reserved.getValue() is itself JSON text from the host call --
 *     // parse it so it nests as an object instead of escaped text.
 *     return JsonHelper.parseObject(reserved.getValue());
 * }
 * }</pre>
 *
 * @see CleatEntry
 * @see CleatResult
 * @see Memory
 */
public class HostCalls {

    /** Current scope prefix for virtual object state operations. */
    private String _scopePrefix = "";

    /**
     * Registered update handlers, by name.
     * <p>
     * Static rather than per-instance for the same reason {@link Defer} keeps
     * its bodies static: a workflow may construct more than one HostCalls, and
     * a handler registered through one must be reachable from the dispatch that
     * happens through another. A WASM guest is single-threaded and one segment
     * is one instance, so there is no sharing hazard.
     */
    private static final java.util.Map<String, UpdateHandlerEntry> UPDATE_HANDLERS =
        new java.util.HashMap<>();

    /**
     * Reentrancy guard for {@link #dispatchUpdates()}.
     * <p>
     * Every dispatch point is a suspension point, and an update handler is
     * ordinary workflow code that may sleep or await -- so without this a
     * handler doing either would re-enter dispatch and recurse. Nesting would
     * also be wrong if it terminated: the inner dispatch would interleave a
     * second update's events inside the first one's, and the received/completed
     * pair would no longer bracket the handler that produced it.
     */
    private static boolean dispatchingUpdates = false;

    /** One registered handler and its optional validator. */
    private static final class UpdateHandlerEntry {
        final java.util.function.Function<String, String> handler;
        final java.util.function.Function<String, String> validator;

        UpdateHandlerEntry(java.util.function.Function<String, String> handler,
                           java.util.function.Function<String, String> validator) {
            this.handler = handler;
            this.validator = validator;
        }
    }

    // ========================================================================
    // Raw WASM imports (18 host functions from the "env" module)
    // ========================================================================

    @Import(module = "env", name = "cleat_call")
    private static native long cleatCallRaw(
        int svcPtr, int svcLen,
        int opPtr, int opLen,
        int reqPtr, int reqLen,
        int respPtr, int respMaxLen);

    @Import(module = "env", name = "cleat_sleep")
    private static native long cleatSleepRaw(long durationMs);

    @Import(module = "env", name = "cleat_now")
    private static native long cleatNowRaw();

    @Import(module = "env", name = "cleat_random")
    private static native long cleatRandomRaw();

    @Import(module = "env", name = "cleat_log")
    private static native long cleatLogRaw(int msgPtr, int msgLen);

    @Import(module = "env", name = "cleat_version")
    private static native long cleatVersionRaw();

    @Import(module = "env", name = "cleat_min_version")
    private static native long cleatMinVersionRaw();

    @Import(module = "env", name = "cleat_defer")
    private static native long cleatDeferRaw(
        int descPtr, int descLen, int outPtr, int maxLen);

    @Import(module = "env", name = "cleat_poll_cancellation")
    private static native long cleatPollCancellationRaw(int outPtr, int maxLen);

    @Import(module = "env", name = "cleat_poll_signal")
    private static native long cleatPollSignalRaw(
        int namePtr, int nameLen, int outPtr, int maxLen);

    @Import(module = "env", name = "cleat_continue_as_new")
    private static native long cleatContinueAsNewRaw(int inPtr, int inLen);

    @Import(module = "env", name = "cleat_create_promise")
    private static native long cleatCreatePromiseRaw(
        int namePtr, int nameLen, int idOutPtr, int idOutMax);

    @Import(module = "env", name = "cleat_child_workflow")
    private static native long cleatChildWorkflowRaw(
        int namePtr, int nameLen,
        int inPtr, int inLen,
        int outPtr, int maxLen);

    // priority is the second i64 and it was MISSING here until 2026-09-05.
    //
    // The host takes ten parameters -- engine/imports.go, "cleat_child_workflow
    // _with_options: (ptr,len x3, i64, i64, ptr,len, ptr,maxLen)" -- and this
    // declared nine. A WASM import whose arity disagrees with the host does not
    // fail at that call: the MODULE FAILS TO INSTANTIATE, so a Java workflow
    // that so much as referenced childWorkflowWithOptions could not run at all.
    //
    // It went unnoticed because TeaVM tree-shakes unreferenced imports, and no
    // Java test called this. Compile-time coverage cannot see it -- the Java
    // side compiled perfectly well against a signature the host does not have.
    // Found by the first run of tests/plugin-harness's Java host-call fixture,
    // which is the whole argument for executing a binding rather than checking
    // that it exists. Same defect class as IMPROVEMENT-PLAN §3.55, where
    // cleat_create_promise was registered with a parameter no guest passed.
    @Import(module = "env", name = "cleat_child_workflow_with_options")
    private static native long cleatChildWorkflowWithOptionsRaw(
        int namePtr, int nameLen,
        int inPtr, int inLen,
        long version,
        long priority,
        int policyPtr, int policyLen,
        int outPtr, int maxLen);


    @Import(module = "env", name = "cleat_await_child")
    private static native long cleatAwaitChildRaw(
        int runIdPtr, int runIdLen,
        int outPtr, int maxLen);

    @Import(module = "env", name = "cleat_await_promise")
    private static native long cleatAwaitPromiseRaw(
        int idPtr, int idLen, long timeoutMs,
        int resultOutPtr, int resultOutMax);

    @Import(module = "env", name = "cleat_await_signals")
    private static native long cleatAwaitSignalsRaw(
        int namesPtr, int namesLen, long timeoutMs,
        int sigNameOut, int sigNameMax,
        int payloadOut, int payloadMax);

    @Import(module = "env", name = "cleat_register_update_handler")
    private static native long cleatRegisterUpdateHandlerRaw(
        int namePtr, int nameLen);

    @Import(module = "env", name = "cleat_poll_update")
    private static native long cleatPollUpdateRaw(
        int envelopeOut, int envelopeMax);

    @Import(module = "env", name = "cleat_complete_update")
    private static native long cleatCompleteUpdateRaw(
        int requestIdPtr, int requestIdLen,
        int resultPtr, int resultLen,
        int errPtr, int errLen);

    @Import(module = "env", name = "set_query_state")
    private static native long setQueryStateRaw(
        int keyPtr, int keyLen, int valPtr, int valLen);


    @Import(module = "env", name = "plugin_call")
    private static native long importPluginCall(
        int pluginNamePtr, int pluginNameLen,
        int functionNamePtr, int functionNameLen,
        int inputPtr, int inputLen,
        int responsePtr, int responseMaxLen);

    @Import(module = "env", name = "cleat_workflow_id")
    private static native long cleatWorkflowIdRaw(int outPtr, int maxLen);

    @Import(module = "env", name = "cleat_run_id")
    private static native long cleatRunIdRaw(int outPtr, int maxLen);

    // cleat_send_signal_and_wait (ABI 2.23) and cleat_reply_to_signal
    // (ABI 2.24) are deliberately NOT imported. Both were inert engine-side,
    // and request/reply is now composed from createPromise + signalWorkflow
    // + awaitPromise + resolvePromise (IMPROVEMENT-PLAN 3.220). Declaring an
    // @Import this SDK never calls would make every Java guest import a host
    // function it does not use. The engine still exports both; removing the
    // exports is a separate change that has to come after every SDK stops
    // importing them.

    @Import(module = "env", name = "cleat_signal_workflow")
    private static native long cleatSignalWorkflowRaw(
        int targetRunIdPtr, int targetRunIdLen,
        int signalNamePtr, int signalNameLen,
        int payloadPtr, int payloadLen);

    @Import(module = "env", name = "cleat_resolve_promise")
    private static native long cleatResolvePromiseRaw(int idPtr, int idLen, int valuePtr, int valueLen);

    @Import(module = "env", name = "cleat_reject_promise")
    private static native long cleatRejectPromiseRaw(int idPtr, int idLen, int errorPtr, int errorLen);

    @Import(module = "env", name = "cleat_send")
    private static native long cleatSendRaw(int svcPtr, int svcLen, int opPtr, int opLen, int reqPtr, int reqLen);


    @Import(module = "env", name = "cleat_schedule_invoke")
    private static native long scheduleInvokeRaw(int svcPtr, int svcLen, int opPtr, int opLen, int reqPtr, int reqLen, long delayMs);

    @Import(module = "env", name = "cleat_run_detached")
    private static native long cleatRunDetachedRaw(int namePtr, int nameLen, int inputPtr, int inputLen);







    @Import(module = "env", name = "cleat_await_all_children")
    private static native long cleatAwaitAllChildrenRaw(int idsPtr, int idsLen, int outPtr, int maxLen);

    @Import(module = "env", name = "cleat_call_retry")
    private static native long cleatCallRetryRaw(
        int svcPtr, int svcLen,
        int opPtr, int opLen,
        int reqPtr, int reqLen,
        long maxAttempts, long initialIntervalMs, long backoffCoefficient100x, long maxIntervalMs,
        int nonRetryableErrorsPtr, int nonRetryableErrorsLen,
        int respPtr, int respMaxLen);

    @Import(module = "env", name = "cleat_call_heartbeat")
    private static native long cleatCallHeartbeatRaw(
        int svcPtr, int svcLen,
        int opPtr, int opLen,
        int reqPtr, int reqLen,
        long heartbeatIntervalMs,
        int respPtr, int respMaxLen);

    @Import(module = "env", name = "cleat_fetch")
    private static native long cleatFetchRaw(
        int methodPtr, int methodLen,
        int urlPtr, int urlLen,
        int headersPtr, int headersLen,
        int bodyPtr, int bodyLen,
        int respPtr, int respMaxLen);

    @Import(module = "env", name = "cleat_acquire_lock")
    private static native long cleatAcquireLockRaw(int keyPtr, int keyLen, long ttlMs);

    @Import(module = "env", name = "cleat_release_lock")
    private static native long cleatReleaseLockRaw(int keyPtr, int keyLen);

    @Import(module = "env", name = "cleat_poll_child")
    private static native long cleatPollChildRaw(
        int runIdPtr, int runIdLen,
        int resultPtr, int resultMaxLen);

    @Import(module = "env", name = "cleat_await_any_child")
    private static native long cleatAwaitAnyChildRaw(
        int runIdsPtr, int runIdsLen,
        int resultPtr, int resultMaxLen);

    // The cron family. Present on the host since ABI 2.31 and bound by the
    // AssemblyScript and Python SDKs; Rust and Java declared no cron surface at
    // all, which is the stated reason tiers.yaml holds workflow-callable-cron at
    // tier 2. IMPROVEMENT-PLAN 3.241.

    @Import(module = "env", name = "cleat_schedule_cron")
    private static native long cleatScheduleCronRaw(
        int wfPtr, int wfLen,
        int cronPtr, int cronLen,
        int tzPtr, int tzLen,
        int inputPtr, int inputLen,
        int idPtr, int idMaxLen);

    @Import(module = "env", name = "cleat_delete_cron")
    private static native long cleatDeleteCronRaw(int idPtr, int idLen);

    @Import(module = "env", name = "cleat_list_crons")
    private static native long cleatListCronsRaw(int outPtr, int outMaxLen);

    @Import(module = "env", name = "cleat_continue_as_new_versioned")
    private static native long cleatContinueAsNewVersionedRaw(
        int inputPtr, int inputLen, int newVersion);

    @Import(module = "env", name = "cleat_side_effect")
    private static native long cleatSideEffectRaw(
        int resultPtr, int resultLen,
        int outPtr, int outMaxLen);

    @Import(module = "env", name = "cleat_set_scope")
    private static native long cleatSetScopeRaw(
        int objTypePtr, int objTypeLen,
        int instKeyPtr, int instKeyLen,
        int prevScopePtr, int prevScopeMaxLen);

    @Import(module = "env", name = "cleat_get_scope")
    private static native long cleatGetScopeRaw(
        int objTypePtr, int objTypeMaxLen,
        int instKeyPtr, int instKeyMaxLen);

    @Import(module = "env", name = "cleat_uuid")
    private static native long cleatUuidRaw(
        int seedPtr, int seedLen,
        int uuidPtr, int uuidMaxLen);

    @Import(module = "env", name = "plugin_call_streaming")
    private static native long importPluginCallStreaming(
        int pluginNamePtr, int pluginNameLen,
        int functionNamePtr, int functionNameLen,
        int inputPtr, int inputLen,
        int responsePtr, int responseMaxLen);

    @Import(module = "env", name = "cleat_json_parse")
    private static native long cleatJsonParseRaw(
        int jsonPtr, int jsonLen,
        int outPtr, int outMaxLen);

    @Import(module = "env", name = "cleat_json_stringify")
    private static native long cleatJsonStringifyRaw(
        int ptr, int len,
        int outPtr, int outMaxLen);

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
    /**
     * The host's own error message, or {@code fallback} when it wrote none.
     *
     * <p>A call with an output buffer usually puts its failure reason there --
     * engine/children.go writes {@code rec.Err}, AwaitPromise writes
     * {@code rec.PromiseError} -- and a guest reporting the bare error code
     * throws that away. IMPROVEMENT-PLAN 3.200 fixed this for the generated Go
     * adapters; eleven wrappers here still did it.
     *
     * <p>Truncating at the first NUL is the whole correctness of this method.
     * "The host wrote nothing" and "the host reported a length" are
     * independent: on a bad-parameter refusal engine/imports.go returns
     * errBadParam <em>before</em> the handler runs, so nothing is written, yet
     * a length is still decoded from the sentinel's bits. readOutput clamps
     * that to the buffer, so a naive isEmpty() check sees a buffer of zero
     * bytes, decides the host "wrote a message", and returns 64KB of NULs as
     * the error text -- strictly worse than the code it replaced. The host
     * writes UTF-8 with no interior NUL, so stopping at the first one is
     * exactly what it wrote.
     *
     * <p>Java clamps in readOutput and renders a negative length as "", so the
     * out-of-bounds half of this never applied here -- but the empty-looking
     * buffer half does.
     */
    private static String hostMessageOr(int len, String fallback) {
        String raw = readOutput(len);
        int nul = raw.indexOf('\0');
        String msg = nul >= 0 ? raw.substring(0, nul) : raw;
        return msg.isEmpty() ? fallback : msg;
    }

    private static String readOutput(int maxLen) {
        if (maxLen <= 0) {
            return "";
        }
        int clamped = Math.min(maxLen, Memory.OUT_BUF_SIZE);
        return Memory.readString(Memory.OUTPUT_OFFSET, clamped);
    }

    /**
     * Prepend the current scope prefix (if set) to the given key.
     * Used by state operations within virtual object instances.
     */
    private String scopedKey(String key) {
        return _scopePrefix.isEmpty() ? key : _scopePrefix + key;
    }

    /**
     * Serialize a {@code Map<String, String>} to a JSON object string.
     * Used by {@link #cleatFetch(String, String, Map, String)}.
     */
    private static String headersToJson(Map<String, String> headers) {
        if (headers == null || headers.isEmpty()) {
            return "{}";
        }
        StringBuilder sb = new StringBuilder("{");
        boolean first = true;
        for (Map.Entry<String, String> entry : headers.entrySet()) {
            if (!first) {
                sb.append(",");
            }
            sb.append("\"").append(JsonHelper.escapeJson(entry.getKey())).append("\":\"")
                .append(JsonHelper.escapeJson(entry.getValue())).append("\"");
            first = false;
        }
        sb.append("}");
        return sb.toString();
    }

    // ========================================================================
    // Public API methods
    // ========================================================================

    /**
     * Make a durable (deterministically replayed) call to an external service.
     * <p>
     * This is the preferred name. Alias for {@link #cleatCall(String, String, String)}.
     * {@code cleatCall} is retained for backward compatibility.
     *
     * @param service     the service name (e.g. {@code "orders"})
     * @param operation   the operation name (e.g. {@code "create"})
     * @param requestJSON the JSON request payload
     * @return a result containing the JSON response on success, or an error
     *         description on failure
     */
    public CleatResult<String> call(String service, String operation, String requestJSON) {
        return cleatCall(service, operation, requestJSON);
    }

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
     * @see #call(String, String, String)
     */
    public CleatResult<String> cleatCall(String service, String operation, String requestJSON) {
        int[] p = packStrings(service, operation, requestJSON);
        int svcOff = p[0], opOff = p[1], reqOff = p[2];
        int svcLen = p[3], opLen = p[4], reqLen = p[5];

        long result = cleatCallRaw(
            svcOff, svcLen,
            opOff, opLen,
            reqOff, reqLen,
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        Memory.throwIfStopped(result);

        int errCode = Memory.decodeCallErrCode(result);
        int responseLen = Memory.decodeCallResponseLen(result);

        if (errCode != 0) {
            String errMsg = readOutput(responseLen);
            return CleatResult.err(errMsg);
        }

        String response = readOutput(responseLen);
        return CleatResult.ok(response);
    }

    /**
     * Suspend workflow execution for a duration specified in milliseconds.
     * <p>
     * On replay ({@link Memory#SLEEP_STATUS_COMPLETED}), this returns
     * {@code false} — the workflow should continue without actually sleeping.
     * On fresh execution ({@link Memory#SLEEP_STATUS_SUSPEND}), returns
     * {@code true} — the workflow should propagate the suspension by
     * returning {@link Memory#SUSPEND_SENTINEL} from the export.
     * <p>
     * Prefer {@link #cleatSleep(long)} (which accepts seconds) over this
     * millisecond variant.
     *
     * @param timeoutMs sleep duration in milliseconds
     * @return {@code true} if the workflow should suspend (fresh execution),
     *         {@code false} to continue (replay)
     */
    public boolean cleatSleepMs(long timeoutMs) {
        dispatchUpdates(); // dispatch point; see dispatchUpdates()
        long result = cleatSleepRaw(timeoutMs);
        int status = Memory.decodeSleepStatus(result);
        if (status == Memory.SLEEP_STATUS_SUSPEND) {
            // Unwind rather than return, which is what every other cleat SDK
            // does (Go and Rust panic, Python raises). Returning `true` and
            // asking the author to "propagate the suspension by returning
            // Memory.SUSPEND_SENTINEL from the export" was unactionable: the
            // author does not write the export. See IMPROVEMENT-PLAN 3.74.
            //
            // The boolean return is kept so replay reads naturally -- it is
            // always false, because the suspending case no longer returns.
            throw new SuspendSignal();
        }
        return false;
    }

    /**
     * Suspend workflow execution for a duration specified in seconds.
     * <p>
     * This is the preferred API. Delegates to
     * {@link #cleatSleepMs(long)}.
     *
     * @param timeoutSeconds sleep duration in seconds
     * @return {@code true} if the workflow should suspend (fresh execution),
     *         {@code false} to continue (replay)
     * @see #cleatSleepMs(long)
     */
    public boolean cleatSleep(long timeoutSeconds) {
        return cleatSleepMs(timeoutSeconds * 1000);
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
        return cleatNowRaw();
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
        return cleatRandomRaw();
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
    public void cleatLog(String message) {
        int[] p = packStrings(message);
        cleatLogRaw(p[0], p[1]);
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
        long result = cleatVersionRaw();
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
        long result = cleatMinVersionRaw();
        return (int) (result & 0xFFFFFFFFL);
    }

    /**
     * Record that a cleanup action exists. <strong>Does not run anything.</strong>
     * <p>
     * This sends a <em>description</em> to the host and nothing else. The host
     * adds it to the workflow's deferrals so it is visible in history, but
     * there is no body attached and nothing anywhere executes one.
     * <p>
     * This javadoc used to describe "a deferred cleanup callback" executed "in
     * LIFO order, analogous to Go's {@code defer}". None of that was true, or
     * could be: there is no callback parameter. See IMPROVEMENT-PLAN 3.73.
     * <p>
     * Use {@link #deferFunc(Runnable)} for cleanup that actually runs.
     *
     * @param description a human-readable description of the cleanup action
     * @return a result containing the defer ID on success, or an error
     *         description on failure
     */
    /**
     * Register cleanup <em>with a body</em>, to run when the workflow finishes.
     *
     * <p>{@link #cleatDefer(String)} registers only a description: the host
     * records that a defer exists and nothing anywhere runs it. This is the one
     * with a {@link Runnable} attached. See IMPROVEMENT-PLAN 3.73.
     *
     * <p>The body runs in LIFO order when the entry point returns -- on the
     * success path and on the error path, because a defer is for the run that
     * did not finish the way it meant to. It does NOT run when the workflow
     * suspends: a suspended workflow has not exited and its cleanup is still
     * pending.
     *
     * @param body the cleanup to run. Exceptions it throws are swallowed so one
     *             bad cleanup cannot stop the others.
     * @return a result containing the defer ID on success, or an error
     *         description on failure
     */
    public CleatResult<String> deferFunc(Runnable body) {
        // Refused BEFORE the host call -- IMPROVEMENT-PLAN 3.35 phase 4.
        // Registering here used to mint a real defer ID and write a durable
        // `defer` event that nothing could ever run, because runDeferred drains
        // the table before the first body starts.
        if (Defer.inDeferPhase()) {
            return CleatResult.err(Defer.deferPhaseRefusal("deferFunc"));
        }
        CleatResult<String> registered = cleatDefer("deferred function");
        if (registered.isOk()) {
            Defer.register(body);
        }
        return registered;
    }

    public CleatResult<String> cleatDefer(String description) {
        int[] p = packStrings(description);

        long result = cleatDeferRaw(
            p[0], p[1],
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        int errCode = Memory.decodeSimpleErrCode(result);
        int deferIdLen = Memory.decodeSimpleExtra(result);

        if (errCode != 0) {
            return CleatResult.err(hostMessageOr(deferIdLen, "defer(description=\"" + description + "\") failed: host returned error code " + errCode + ". Check that the defer description is valid."));
        }

        String deferId = readOutput(deferIdLen);
        return CleatResult.ok(deferId);
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
    public CleatResult<Boolean> pollCancellation() {
        long result = cleatPollCancellationRaw(Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        boolean cancelled = Memory.decodePollCancelCancelled(result);
        int reasonLen = Memory.decodePollCancelReasonLen(result);

        // The cancellation reason is written to the output buffer when
        // cancelled.  We do not propagate the reason here; callers that
        // need it can expand this wrapper.
        return CleatResult.ok(cancelled);
    }

    /**
     * Poll for a specific pending external signal.
     * <p>
     * Unlike {@link #awaitSignalsMs(String[], long)}, this call is non-blocking
     * and checks once.  If no signal is pending with the given name, the
     * result carries an error.
     *
     * @param signalName the signal name to look up
     * @return a result containing the signal payload if found, or an error
     *         if the signal is not pending
     */
    public CleatResult<String> pollSignal(String signalName) {
        int[] p = packStrings(signalName);

        long result = cleatPollSignalRaw(
            p[0], p[1],
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        boolean found = Memory.decodePollSigFound(result);
        if (!found) {
            return CleatResult.err("signal not found: " + signalName);
        }

        int payloadLen = Memory.decodePollSigPayloadLen(result);
        String payload = readOutput(payloadLen);
        return CleatResult.ok(payload);
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
    public CleatResult<Void> continueAsNew(String newInputJSON) {
        // IMPROVEMENT-PLAN 3.35 phase 4. Before the host call: the workflow's
        // result is already decided by the time defers run, so a recorded
        // continuation is one the worker will never take.
        if (Defer.inDeferPhase()) {
            return CleatResult.err(Defer.deferPhaseRefusal("continueAsNew"));
        }
        int[] p = packStrings(newInputJSON);

        long result = cleatContinueAsNewRaw(p[0], p[1]);

        int errCode = Memory.decodeSimpleErrCode(result);
        if (errCode != 0) {
            return CleatResult.err("continueAsNew(...) failed: host returned error code " + errCode + ". Check that the input JSON is valid and continue-as-new is available.");
        }
        return CleatResult.ok(null);
    }

    /**
     * Replace the current workflow's input and restart execution from the
     * beginning with an explicit version ("continue-as-new-versioned").
     * <p>
     * Like {@link #continueAsNew(String)} but allows specifying the workflow
     * definition version explicitly for the restarted execution.
     *
     * @param newInputJSON the new input JSON for the restarted workflow
     * @param newVersion   the explicit workflow definition version to use
     * @return a result indicating success, or an error description
     */
    public CleatResult<Void> continueAsNewVersioned(String newInputJSON, int newVersion) {
        // IMPROVEMENT-PLAN 3.35 phase 4. Before the host call: the workflow's
        // result is already decided by the time defers run, so a recorded
        // continuation is one the worker will never take.
        if (Defer.inDeferPhase()) {
            return CleatResult.err(Defer.deferPhaseRefusal("continueAsNewVersioned"));
        }
        int[] p = packStrings(newInputJSON);

        long result = cleatContinueAsNewVersionedRaw(p[0], p[1], newVersion);

        int errCode = Memory.decodeSimpleErrCode(result);
        if (errCode != 0) {
            return CleatResult.err("continueAsNewVersioned(...) failed: host returned error code " + errCode + ". Check that the input JSON is valid and version is valid.");
        }
        return CleatResult.ok(null);
    }

    /**
     * Create a promise with the given name.
     * <p>
     * Promises are durable, first-class entities in cleat. They can be
     * resolved by this or another workflow, and awaited by one or more
     * workflows using {@link #awaitPromiseMs(String, long)}.
     *
     * @param name the promise name
     * @return a result containing the promise ID on success, or an error
     *         description on failure
     */
    public CleatResult<String> createPromise(String name) {
        int[] p = packStrings(name);

        long result = cleatCreatePromiseRaw(
            p[0], p[1],
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        int errCode = Memory.decodeSimpleErrCode(result);
        int idLen = Memory.decodeSimpleExtra(result);

        if (errCode != 0) {
            return CleatResult.err(hostMessageOr(idLen, "createPromise(name=\"" + name + "\") failed: host returned error code " + errCode + ". Check that the promise name is valid."));
        }

        String promiseId = readOutput(idLen);
        return CleatResult.ok(promiseId);
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
    public CleatResult<String> childWorkflow(String name, String inputJSON) {
        int[] p = packStrings(name, inputJSON);
        int nameOff = p[0], inOff = p[1];
        int nameLen = p[2], inLen = p[3];

        long result = cleatChildWorkflowRaw(
            nameOff, nameLen,
            inOff, inLen,
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        Memory.throwIfStopped(result);

        int errCode = Memory.decodeSimpleErrCode(result);
        int runIdLen = Memory.decodeSimpleExtra(result);

        if (errCode != 0) {
            return CleatResult.err(hostMessageOr(runIdLen, "childWorkflow(name=\"" + name + "\") failed: host returned error code " + errCode + ". Check that the child workflow name is correct and the workflow definition exists."));
        }

        String runId = readOutput(runIdLen);
        return CleatResult.ok(runId);
    }

    /**
     * Start a child workflow instance with an explicit version.
     * <p>
     * Like {@link #childWorkflow(String, String)} but allows specifying
     * the workflow definition version explicitly.
     *
     * @param name              the child workflow definition name
     * @param inputJSON         the input JSON for the child workflow
     * @param version           the explicit workflow definition version
     *                          (0 = use parent's version / default resolution)
     * @param parentClosePolicy parent close policy ("abandon", "terminate", "request_cancel")
     * @return a result containing the child's run ID on success, or an error
     *         description on failure
     */
    public CleatResult<String> childWorkflowWithOptions(String name, String inputJSON, long version, String parentClosePolicy) {
        int[] p = packStrings(name, inputJSON, parentClosePolicy);
        int nameOff = p[0], inOff = p[1], policyOff = p[2];
        int nameLen = p[3], inLen = p[4], policyLen = p[5];

        long result = cleatChildWorkflowWithOptionsRaw(
            nameOff, nameLen,
            inOff, inLen,
            version,
            // priority 0, which the Go SDK documents as the default and the
            // highest ("0 = highest priority", cleat/runtime_children.go).
            // Java's public API does not expose priority yet; adding an
            // overload that does is an additive change and deliberately not
            // bundled with fixing the arity, which is what stops the module
            // linking at all.
            0L,
            policyOff, policyLen,
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        Memory.throwIfStopped(result);

        int errCode = Memory.decodeSimpleErrCode(result);
        int runIdLen = Memory.decodeSimpleExtra(result);

        if (errCode != 0) {
            return CleatResult.err(hostMessageOr(runIdLen, "childWorkflowWithOptions(name=\"" + name + "\", version=" + version + ") failed: host returned error code " + errCode + ". Check that the child workflow name is correct."));
        }

        String runId = readOutput(runIdLen);
        return CleatResult.ok(runId);
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
    public CleatResult<String> awaitChild(String runID) {
        dispatchUpdates(); // dispatch point; see dispatchUpdates()
        int[] p = packStrings(runID);

        long result = cleatAwaitChildRaw(
            p[0], p[1],
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        // Check for suspension sentinel before decoding the result.
        if (result == Memory.SUSPEND_SENTINEL) {
            return CleatResult.err("child workflow not yet complete; workflow must suspend");
        }

        int errCode = Memory.decodeSimpleErrCode(result);
        int resultLen = Memory.decodeSimpleExtra(result);

        if (errCode != 0) {
            return CleatResult.err(hostMessageOr(resultLen, "awaitChild(runID=\"" + runID + "\") failed: host returned error code " + errCode + ". Check that the run ID is valid."));
        }

        String childResult = readOutput(resultLen);
        return CleatResult.ok(childResult);
    }

    /**
     * Poll for a child workflow result without blocking.
     * <p>
     * Unlike {@link #awaitChild(String)}, this call is non-blocking and
     * checks once.  If the child has not yet completed, the result carries
     * an error.  Does not check for SUSPEND_SENTINEL.
     *
     * @param runID the child workflow run ID
     * @return a result containing the child's output JSON on success, or an
     *         error description on failure (including if not yet complete)
     */
    public CleatResult<String> pollChild(String runID) {
        int[] p = packStrings(runID);

        long result = cleatPollChildRaw(
            p[0], p[1],
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        int errCode = Memory.decodeSimpleErrCode(result);
        int resultLen = Memory.decodeSimpleExtra(result);

        if (errCode != 0) {
            return CleatResult.err(hostMessageOr(resultLen, "pollChild(runID=\"" + runID + "\") failed: host returned error code " + errCode + ". Check that the run ID is valid or the child has completed."));
        }

        String childResult = readOutput(resultLen);
        return CleatResult.ok(childResult);
    }

    /**
     * Wait for a promise to resolve, with an optional timeout.
     * <p>
     * Blocks until the promise with the given ID is resolved or the timeout
     * expires. On timeout, {@link AwaitPromiseResult#timedOut} is
     * {@code true}.
     * <p>
     * Prefer {@link #awaitPromise(String, long)} (which accepts seconds) over
     * this millisecond variant.
     *
     * @param promiseId the promise ID to wait for
     * @param timeoutMs maximum wait time in milliseconds (use
     *                  {@link Long#MAX_VALUE} for no timeout)
     * @return a result containing an {@link AwaitPromiseResult} with the
     *         resolved value and timeout indicator
     */
    public CleatResult<AwaitPromiseResult> awaitPromiseMs(String promiseId, long timeoutMs) {
        dispatchUpdates(); // dispatch point; see dispatchUpdates()
        int[] p = packStrings(promiseId);

        long result = cleatAwaitPromiseRaw(
            p[0], p[1], timeoutMs,
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        int errCode = Memory.decodeAwaitErrCode(result);
        boolean timedOut = Memory.decodeAwaitPromiseTimedOut(result);
        int resultLen = Memory.decodeSimpleExtra(result);

        if (errCode != 0) {
            return CleatResult.err(hostMessageOr(resultLen, "awaitPromise(promiseId=\"" + promiseId + "\") failed: host returned error code " + errCode + ". Check that the promise ID is valid."));
        }

        String promiseResult = readOutput(resultLen);
        return CleatResult.ok(new AwaitPromiseResult(promiseResult, timedOut));
    }

    /**
     * Wait for a promise to resolve, with an optional timeout specified in
     * seconds.
     * <p>
     * This is the preferred API. Delegates to
     * {@link #awaitPromiseMs(String, long)}.
     *
     * @param promiseId      the promise ID to wait for
     * @param timeoutSeconds maximum wait time in seconds (use
     *                       {@link Long#MAX_VALUE} for no timeout)
     * @return a result containing an {@link AwaitPromiseResult} with the
     *         resolved value and timeout indicator
     * @see #awaitPromiseMs(String, long)
     */
    public CleatResult<AwaitPromiseResult> awaitPromise(String promiseId, long timeoutSeconds) {
        return awaitPromiseMs(promiseId, timeoutSeconds * 1000);
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
     * <p>
     * Prefer {@link #awaitSignals(String[], long)} (which accepts seconds)
     * over this millisecond variant.
     *
     * @param signalNames the signal names to wait for
     * @param timeoutMs   maximum wait time in milliseconds (use
     *                    {@link Long#MAX_VALUE} for no timeout)
     * @return a result containing an {@link AwaitSignalsResult} with the
     *         received signal name, payload, and timeout indicator
     */
    public CleatResult<AwaitSignalsResult> awaitSignalsMs(String[] signalNames, long timeoutMs) {
        dispatchUpdates(); // dispatch point; see dispatchUpdates()
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

        long result = cleatAwaitSignalsRaw(
            namesOff, namesLen, timeoutMs,
            Memory.OUTPUT_OFFSET, sigNameBufSize,
            payloadBufOffset, payloadBufSize);

        Memory.throwIfStopped(result);

        int errCode = Memory.decodeAwaitErrCode(result);
        boolean timedOut = Memory.decodeAwaitTimedOut(result);
        int sigNameLen = Memory.decodeAwaitSigNameLen(result);
        int payloadLen = Memory.decodeAwaitPayloadLen(result);

        if (errCode != 0) {
            // Two buffers here, unlike the others, so the generic wrapper's
            // implicit length does not apply. The signal NAME buffer sits at
            // OUTPUT_OFFSET and is the one a reason would be written into, and
            // its own clamp is preserved rather than falling back to
            // OUT_BUF_SIZE -- without it an over-long reported length would read
            // past the name region and into the payload buffer beside it.
            //
            // Every DurableAwaitSignals path in engine/signaller.go passes
            // errCode 0, so in practice this fires only on errBadParam, where
            // nothing is written and the fallback is returned. Wired anyway:
            // "unreachable today" is a property of the host, not of this guest.
            return CleatResult.err(hostMessageOr(Math.min(sigNameLen, sigNameBufSize),
                "awaitSignals(names=" + namesJSON + ") failed: host returned error code " + errCode + ". Check that the signal names are valid."));
        }

        String sigName = Memory.readString(Memory.OUTPUT_OFFSET,
            Math.min(sigNameLen, sigNameBufSize));
        String payload = Memory.readString(payloadBufOffset,
            Math.min(payloadLen, payloadBufSize));

        // Strip the reply envelope, if this is a request/reply signal, so the
        // receiver reads its payload exactly as the sender passed it and gets
        // the address separately rather than parsing it out. IMPROVEMENT-PLAN 3.220.
        String replyTo = "";
        String[] unwrapped = SignalEnvelope.decode(payload);
        if (unwrapped != null) {
            replyTo = unwrapped[0];
            payload = unwrapped[1];
        }

        return CleatResult.ok(new AwaitSignalsResult(sigName, payload, timedOut, replyTo));
    }

    /**
     * Wait for one or more external signals, with an optional timeout
     * specified in seconds.
     * <p>
     * This is the preferred API. Delegates to
     * {@link #awaitSignalsMs(String[], long)}.
     *
     * @param signalNames   the signal names to wait for
     * @param timeoutSeconds maximum wait time in seconds (use
     *                       {@link Long#MAX_VALUE} for no timeout)
     * @return a result containing an {@link AwaitSignalsResult} with the
     *         received signal name, payload, and timeout indicator
     * @see #awaitSignalsMs(String[], long)
     */
    public CleatResult<AwaitSignalsResult> awaitSignals(String[] signalNames, long timeoutSeconds) {
        return awaitSignalsMs(signalNames, timeoutSeconds * 1000);
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
        cleatRegisterUpdateHandlerRaw(p[0], p[1]);
    }

    /**
     * Register an update handler with an optional validator.
     * <p>
     * The validator returns null to accept, or a message to refuse. It runs
     * first and must be read-only: a refusal costs nothing beyond the
     * completion -- no state change, no durable work. That is the half of the
     * API that makes an update different from a signal.
     * <p>
     * The name is registered with the host as well, which records it in the
     * event history; the handler stays here, because only guest code can call
     * it.
     *
     * @param name      the update name
     * @param handler   receives the payload JSON, returns the result JSON
     * @param validator receives the payload JSON, returns null or a refusal;
     *                  may be null
     */
    public void registerUpdateHandler(String name,
                                      java.util.function.Function<String, String> handler,
                                      java.util.function.Function<String, String> validator) {
        UPDATE_HANDLERS.put(name, new UpdateHandlerEntry(handler, validator));
        registerUpdateHandler(name);
    }

    /**
     * Deliver and run every update currently pending for this workflow.
     * <p>
     * The SDK already calls this before each suspension, so an ordinary
     * workflow needs no update-specific code. It is exposed for workflows that
     * want to service updates at additional points.
     * <p>
     * An update handler is a closure in guest memory, so only guest code can
     * invoke it -- an arriving update cannot interrupt the workflow. And it
     * must be asked for at a fixed PROGRAM POSITION rather than a moment in
     * time, because replay re-executes the guest and matches host calls against
     * the recorded history in order. See engine/updater.go.
     * <p>
     * The consequence, stated rather than hidden: an update is handled at the
     * next dispatch point, not the instant it arrives.
     */
    public void dispatchUpdates() {
        if (dispatchingUpdates) {
            return;
        }
        dispatchingUpdates = true;
        try {
            while (true) {
                CleatResult<String> polled = pollUpdate();
                if (polled.isErr()) {
                    return;
                }
                String envelope = polled.getValue();
                if (envelope == null || envelope.isEmpty()) {
                    return;
                }
                java.util.Map<String, Object> d = JsonHelper.parseObject(envelope);
                Object name = d.get("name");
                Object requestId = d.get("request_id");
                if (!(name instanceof String) || !(requestId instanceof String)) {
                    // The envelope is written by the host, so this is not a
                    // caller error. Returning rather than continuing avoids
                    // spinning on a delivery that decodes the same way next
                    // time.
                    return;
                }
                Object payload = d.get("payload");
                runUpdate((String) name, payload instanceof String ? (String) payload : "",
                    (String) requestId);
            }
        } finally {
            dispatchingUpdates = false;
        }
    }

    /**
     * Apply one delivered update and report the outcome.
     * <p>
     * Every path completes the request. A handler that is not registered, a
     * validator that refuses and a handler that throws are all answers the
     * caller is entitled to -- leaving any of them uncompleted would leave the
     * caller holding a promise nothing settles, which is the defect updates
     * exist to end.
     */
    private void runUpdate(String name, String payload, String requestId) {
        UpdateHandlerEntry entry = UPDATE_HANDLERS.get(name);
        if (entry == null) {
            completeUpdate(requestId, "", "cleat: no update handler registered for \"" + name + "\"");
            return;
        }
        if (entry.validator != null) {
            String refusal;
            try {
                refusal = entry.validator.apply(payload);
            } catch (SuspendSignal e) {
                throw e;
            } catch (RuntimeException e) {
                completeUpdate(requestId, "", String.valueOf(e.getMessage()));
                return;
            }
            if (refusal != null && !refusal.isEmpty()) {
                completeUpdate(requestId, "", refusal);
                return;
            }
        }
        String result;
        try {
            result = entry.handler.apply(payload);
        } catch (SuspendSignal e) {
            // A stop must propagate: the segment is ending, and swallowing it
            // here would complete the request with a message while the host has
            // already refused the work.
            throw e;
        } catch (RuntimeException e) {
            completeUpdate(requestId, "", String.valueOf(e.getMessage()));
            return;
        }
        completeUpdate(requestId, result == null ? "" : result, "");
    }

    /**
     * Poll for the next pending update request.
     * <p>
     * Low-level: prefer {@link #dispatchUpdates()}, which pairs this with
     * handler lookup, validation, and the guarantee that every delivered update
     * is answered. Delivery is durable, so an update returned here is recorded
     * as delivered whether or not you complete it.
     *
     * @return the delivery envelope JSON, or an empty result when none is
     *         pending
     */
    public CleatResult<String> pollUpdate() {
        long result = cleatPollUpdateRaw(Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);
        // Ask before decoding: a stop is bit 31, which in this layout sits
        // inside the flags word a decoder reads as an ordinary result.
        Memory.throwIfStopped(result);

        if (!Memory.decodePollSigFound(result)) {
            return CleatResult.ok("");
        }
        int len = Memory.decodePollSigPayloadLen(result);
        return CleatResult.ok(readOutput(len));
    }

    /**
     * Record an update handler's outcome and settle the caller's promise.
     * <p>
     * A non-empty {@code errMsg} rejects; an empty one resolves. An empty
     * {@code resultJSON} with an empty {@code errMsg} resolves -- an empty
     * result is an outcome, not a missing one.
     * <p>
     * Low-level: prefer {@link #dispatchUpdates()}, which cannot forget to call
     * this. An update delivered and never completed leaves its caller holding a
     * promise nothing settles.
     *
     * @param requestId  the request id from {@link #pollUpdate()}, unchanged
     * @param resultJSON the handler's result
     * @param errMsg     the failure message, or empty on success
     * @return a result indicating success, or an error description
     */
    public CleatResult<Void> completeUpdate(String requestId, String resultJSON, String errMsg) {
        int[] p = packStrings(requestId, resultJSON, errMsg);
        long result = cleatCompleteUpdateRaw(p[0], p[1], p[2], p[3], p[4], p[5]);
        Memory.throwIfStopped(result);
        int errCode = (int) (result & 0xFFFFFFFFL);
        if (errCode != 0) {
            return CleatResult.err("completeUpdate failed: host error code " + errCode);
        }
        return CleatResult.ok(null);
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
    // plugin_call — call a plugin host function (ABI 2.19)
    // ========================================================================

    /**
     * Call a plugin host function (non-journaled) and return the response.
     * <p>
     * This is the preferred name for non-durable plugin calls. Alias for
     * {@link #pluginCall(String, String, String)}.
     * {@code pluginCall} is retained for backward compatibility.
     *
     * @param pluginName   name of the plugin (e.g. {@code "blobstore"},
     *                     {@code "slacknotify"})
     * @param functionName name of the function within the plugin
     *                     (e.g. {@code "put"}, {@code "send_message"})
     * @param inputJson    input JSON for the plugin function
     * @return a result containing the plugin function's response JSON on
     *         success, or an error description on failure
     */
    /**
     * Call a plugin host function and return the response.
     * <p>
     * Plugins extend the host runtime with custom functionality beyond the
     * standard host imports. Plugin calls are journaled for deterministic
     * replay, same as {@link #cleatCall(String, String, String)}.
     *
     * @param pluginName   name of the plugin (e.g. {@code "blobstore"},
     *                     {@code "slacknotify"})
     * @param functionName name of the function within the plugin
     *                     (e.g. {@code "put"}, {@code "send_message"})
     * @param inputJson    input JSON for the plugin function
     * @return a result containing the plugin function's response JSON on
     *         success, or an error description on failure
     */
    public CleatResult<String> pluginCall(String pluginName, String functionName, String inputJson) {
        int[] p = packStrings(pluginName, functionName, inputJson);
        int pnOff = p[0], fnOff = p[1], inOff = p[2];
        int pnLen = p[3], fnLen = p[4], inLen = p[5];

        long result = importPluginCall(
            pnOff, pnLen,
            fnOff, fnLen,
            inOff, inLen,
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        Memory.throwIfStopped(result);

        int errCode = Memory.decodeCallErrCode(result);
        int responseLen = Memory.decodeCallResponseLen(result);

        if (errCode != 0) {
            String errMsg = readOutput(responseLen);
            return CleatResult.err(errMsg);
        }

        return CleatResult.ok(readOutput(responseLen));
    }

    /**
     * Call a plugin host function with streaming support.
     * <p>
     * Like {@link #pluginCall(String, String, String)} but uses the
     * streaming host function variant which supports incremental response
     * delivery.  The API is identical to the caller.
     *
     * @param pluginName   name of the plugin (e.g. {@code "blobstore"})
     * @param functionName name of the function within the plugin
     * @param inputJson    input JSON for the plugin function
     * @return a result containing the plugin function's response JSON on
     *         success, or an error description on failure
     */
    public CleatResult<String> pluginCallStreaming(String pluginName, String functionName, String inputJson) {
        int[] p = packStrings(pluginName, functionName, inputJson);
        int pnOff = p[0], fnOff = p[1], inOff = p[2];
        int pnLen = p[3], fnLen = p[4], inLen = p[5];

        long result = importPluginCallStreaming(
            pnOff, pnLen,
            fnOff, fnLen,
            inOff, inLen,
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        Memory.throwIfStopped(result);

        int errCode = Memory.decodeCallErrCode(result);
        int responseLen = Memory.decodeCallResponseLen(result);

        if (errCode != 0) {
            String errMsg = readOutput(responseLen);
            return CleatResult.err(errMsg);
        }

        return CleatResult.ok(readOutput(responseLen));
    }

    /**
     * Typed variant of {@link #pluginCall(String, String, String)}.
     * Deserialises the JSON response into the given type using the
     * configured JSON mapper.
     *
     * @param pluginName   name of the plugin
     * @param functionName name of the function within the plugin
     * @param input        input object (serialised to JSON)
     * @param resultType   class of the response type
     * @param <T>          response type
     * @return a result containing the deserialised response on success
     */
    public <T> CleatResult<T> pluginCallTyped(String pluginName, String functionName,
                                               Object input, Class<T> resultType) {
        String inputJson = JsonHelper.toJson(input);
        CleatResult<String> raw = pluginCall(pluginName, functionName, inputJson);
        if (raw.isErr()) {
            return CleatResult.err(raw.errorOrNull());
        }
        try {
            T result = JsonHelper.fromJson(raw.valueOrNull(), resultType);
            return CleatResult.ok(result);
        } catch (Exception e) {
            return CleatResult.err("deserializing plugin response: " + e.getMessage());
        }
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

        Memory.throwIfStopped(result);

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
        long result = cleatWorkflowIdRaw(Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);
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
        // Record the previous scope for return.
        String prev = this._scopePrefix;

        int[] p = packStrings(objectType, instanceKey);
        int objTypeOff = p[0], instKeyOff = p[1];
        int objTypeLen = p[2], instKeyLen = p[3];

        long result = cleatSetScopeRaw(
            objTypeOff, objTypeLen,
            instKeyOff, instKeyLen,
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        // Still update _scopePrefix for backward compat with state operations.
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
        // Split the output buffer: first half for objectType, second for instanceKey.
        final int halfBufSize = Memory.OUT_BUF_SIZE / 2;

        long result = cleatGetScopeRaw(
            Memory.OUTPUT_OFFSET, halfBufSize,
            Memory.OUTPUT_OFFSET + halfBufSize, halfBufSize);

        int[] lengths = Memory.decodeGetScopeResult(result);
        int objTypeLen = lengths[0];
        int instKeyLen = lengths[1];

        String objType = Memory.readString(Memory.OUTPUT_OFFSET,
            Math.min(objTypeLen, halfBufSize));
        String instKey = Memory.readString(Memory.OUTPUT_OFFSET + halfBufSize,
            Math.min(instKeyLen, halfBufSize));

        return new String[]{objType, instKey};
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
        int[] p = packStrings(seed);

        long result = cleatUuidRaw(
            p[0], p[1],
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        int errCode = Memory.decodeSimpleErrCode(result);
        int uuidLen = Memory.decodeSimpleExtra(result);

        if (errCode != 0 || uuidLen == 0) {
            return "00000000-0000-0000-0000-000000000000";
        }

        return readOutput(uuidLen);
    }

    // ========================================================================
    // Signal correlation / response & quorum APIs
    // ========================================================================

    /**
     * Send a signal to a target workflow and wait for a response, with a
     * timeout in milliseconds.
     * <p>
     * The signal carries an embedded correlation ID. The target workflow
     * can use {@link #replyToSignal(String, String)} to send a response back.
     * <p>
     * Prefer {@link #sendSignalAndWait(String, String, String, long)}
     * (which accepts seconds) over this millisecond variant.
     *
     * @param targetRunId the target workflow's run ID
     * @param signalName  the signal name to send
     * @param payload     the signal payload JSON
     * @param timeoutMs   maximum wait time in milliseconds
     * @return a result containing the response on success, or an error
     *         description on failure
     */
    public CleatResult<String> sendSignalAndWaitMs(
        String targetRunId, String signalName, String payload, long timeoutMs) {
        // Composed from three durable primitives rather than being a host call
        // of its own: a promise is the reply channel, its ID is the correlation
        // ID, and answering is resolving it (IMPROVEMENT-PLAN 3.220).
        // cleat_send_signal_and_wait was inert engine-side -- it never
        // delivered the signal it then waited for -- so this is the first
        // version that works at all.
        CleatResult<String> promise = createPromise("__reply:" + signalName);
        if (promise.isErr()) {
            return CleatResult.err("sendSignalAndWait: create reply promise: " + promise.getError());
        }
        String replyTo = promise.getValue();

        CleatResult<Void> sent = signalWorkflow(
            targetRunId, signalName, SignalEnvelope.encode(replyTo, payload));
        if (sent.isErr()) {
            return CleatResult.err("sendSignalAndWait: send signal \"" + signalName
                + "\" to \"" + targetRunId + "\": " + sent.getError());
        }

        CleatResult<AwaitPromiseResult> awaited = awaitPromiseMs(replyTo, timeoutMs);
        if (awaited.isErr()) {
            return CleatResult.err("sendSignalAndWait: await reply to signal \""
                + signalName + "\": " + awaited.getError());
        }
        // Returning an error on timedOut is correct even though awaitPromise
        // reports timedOut for a SUSPENSION as well as a real timeout. The host
        // distinguishes them and this code does not have to: engine/promises.go
        // sets session.suspendErr before returning, and engine/executor.go:264
        // treats a workflow error as a failure only when suspendErr is nil --
        // ":315 deliberately lets a suspension win over the error that
        // accompanied it". There is nothing in the result to check: the engine
        // signals suspension host-side, not through a sentinel.
        if (awaited.getValue().timedOut) {
            return CleatResult.err("sendSignalAndWait: no reply to signal \"" + signalName
                + "\" from workflow \"" + targetRunId + "\" within " + timeoutMs + "ms");
        }
        return CleatResult.ok(awaited.getValue().result);
    }

    /**
     * Send a signal to a target workflow and wait for a response, with a
     * timeout specified in seconds.
     * <p>
     * This is the preferred API. Delegates to
     * {@link #sendSignalAndWaitMs(String, String, String, long)}.
     *
     * @param targetRunId   the target workflow's run ID
     * @param signalName    the signal name to send
     * @param payload       the signal payload JSON
     * @param timeoutSeconds maximum wait time in seconds
     * @return a result containing the response on success, or an error
     *         description on failure
     * @see #sendSignalAndWaitMs(String, String, String, long)
     */
    public CleatResult<String> sendSignalAndWait(
        String targetRunId, String signalName, String payload, long timeoutSeconds) {
        return sendSignalAndWaitMs(targetRunId, signalName, payload, timeoutSeconds * 1000);
    }

    /**
     * Send a response back to the sender of a signal.
     * <p>
     * Only valid inside a signal handler context where the correlation ID
     * was embedded in the received signal payload.
     *
     * @param correlationId the correlation ID from the received signal payload
     * @param response      the response payload JSON
     * @return a result indicating success, or an error description on failure
     */
    public CleatResult<Void> replyToSignal(String correlationId, String response) {
        // correlationId is AwaitSignalsResult.replyTo, which is the reply
        // promise's ID, so replying is resolving that promise. An ID matching
        // no promise is an error rather than a silent success, which is what
        // makes a stale address visible instead of leaving the sender
        // suspended until its timeout. IMPROVEMENT-PLAN 3.220.
        if (correlationId == null || correlationId.isEmpty()) {
            return CleatResult.err("replyToSignal: empty correlation ID. Pass "
                + "AwaitSignalsResult.replyTo from the signal being answered; it is empty "
                + "when the sender used signalWorkflow and is not waiting for a reply.");
        }
        CleatResult<Void> settled = resolvePromise(correlationId, response);
        if (settled.isErr()) {
            return CleatResult.err("replyToSignal(correlationId=\"" + correlationId
                + "\"): " + settled.getError());
        }
        return CleatResult.ok(null);
    }

    /**
     * Wait for at least {@code minCount} signals from the named set, with a
     * timeout in milliseconds.
     * <p>
     * Collects signals until {@code minCount} is reached,
     * {@code maxRejections} is exceeded (if {@code >= 0}), or the timeout
     * expires.
     * <p>
     * Prefer {@link #awaitSignalsWithQuorum(String[], int, int, long)}
     * (which accepts seconds) over this millisecond variant.
     *
     * @param signalNames   the signal names to wait for
     * @param minCount      minimum number of signals required to proceed
     * @param maxRejections maximum rejections tolerated before aborting
     *                      ({@code -1} to disable)
     * @param timeoutMs     maximum wait time in milliseconds
     * @return a result containing the list of collected signals, or an error
     */
    public CleatResult<java.util.List<AwaitSignalsResult>> awaitSignalsWithQuorumMs(
        String[] signalNames, int minCount, int maxRejections, long timeoutMs) {
        java.util.List<AwaitSignalsResult> results = new java.util.ArrayList<>();
        long deadline = this.now() + timeoutMs;
        int rejectionCount = 0;

        while (results.size() < minCount) {
            long remainingMs = deadline - this.now();
            if (remainingMs <= 0) {
                return CleatResult.err(
                    "quorum timeout waiting for signals [" + String.join(", ", signalNames) + "]: got " + results.size() + "/" + minCount + " signals");
            }

            CleatResult<AwaitSignalsResult> signalResult = this.awaitSignalsMs(signalNames, remainingMs);
            if (signalResult.isErr()) {
                return CleatResult.err("quorum signal error waiting for signals [" + String.join(", ", signalNames) + "]: " + signalResult.getError());
            }
            AwaitSignalsResult asr = signalResult.getValue();
            if (asr.timedOut) {
                return CleatResult.err(
                    "quorum timeout waiting for signals [" + String.join(", ", signalNames) + "]: got " + results.size() + "/" + minCount + " signals");
            }

            results.add(asr);

            // Check for rejection if maxRejections >= 0.
            if (maxRejections >= 0 && asr.payload != null && !asr.payload.isEmpty()) {
                try {
                    java.util.Map<String, Object> payloadMap = JsonHelper.parseObject(asr.payload);
                    Object rejectedVal = payloadMap.get("rejected");
                    if (rejectedVal instanceof Boolean && (Boolean) rejectedVal) {
                        rejectionCount++;
                        if (rejectionCount > maxRejections) {
                            return CleatResult.err(
                                "quorum exceeded max rejections (" + maxRejections + ") while waiting for signals [" + String.join(", ", signalNames) + "]");
                        }
                    }
                } catch (Exception e) {
                    // Non-JSON payload, not a rejection.
                }
            }
        }

        return CleatResult.ok(results);
    }

    /**
     * Wait for at least {@code minCount} signals from the named set, with a
     * timeout specified in seconds.
     * <p>
     * This is the preferred API. Delegates to
     * {@link #awaitSignalsWithQuorumMs(String[], int, int, long)}.
     *
     * @param signalNames    the signal names to wait for
     * @param minCount       minimum number of signals required to proceed
     * @param maxRejections  maximum rejections tolerated before aborting
     *                       ({@code -1} to disable)
     * @param timeoutSeconds maximum wait time in seconds
     * @return a result containing the list of collected signals, or an error
     * @see #awaitSignalsWithQuorumMs(String[], int, int, long)
     */
    public CleatResult<java.util.List<AwaitSignalsResult>> awaitSignalsWithQuorum(
        String[] signalNames, int minCount, int maxRejections, long timeoutSeconds) {
        return awaitSignalsWithQuorumMs(signalNames, minCount, maxRejections, timeoutSeconds * 1000);
    }

    /**
     * Send a signal to a target workflow (fire-and-forget).
     * <p>
     * Unlike {@link #sendSignalAndWaitMs(String, String, String, long)},
     * this method does not wait for a response. The signal is enqueued and
     * the workflow continues immediately. This is a recorded (journaled)
     * operation.
     *
     * @param targetRunId the target workflow's run ID
     * @param signalName  the signal name to send
     * @param payload     the signal payload JSON
     * @return a result indicating success, or an error description on failure
     */
    public CleatResult<Void> signalWorkflow(String targetRunId, String signalName, String payload) {
        int[] p = packStrings(targetRunId, signalName, payload);
        int targetOff = p[0], sigOff = p[1], payOff = p[2];
        int targetLen = p[3], sigLen = p[4], payLen = p[5];

        long result = cleatSignalWorkflowRaw(
            targetOff, targetLen,
            sigOff, sigLen,
            payOff, payLen);

        // The host refuses new work in a defer segment and marks the refusal with
        // bit 31 (IMPROVEMENT-PLAN 3.302). Ask BEFORE decoding: decodeSimpleErrCode
        // reads the low byte, where a stop is 0 -- an ordinary success for a
        // fire-and-forget call, so the guest would report the send as done.
        Memory.throwIfStopped(result);

        int errCode = Memory.decodeSimpleErrCode(result);
        if (errCode != 0) {
            return CleatResult.err("signalWorkflow(targetRunId=\"" + targetRunId + "\", signalName=\"" + signalName + "\") failed: host returned error code " + errCode + ". Check that the target run ID and signal name are valid.");
        }
        return CleatResult.ok(null);
    }

    // ========================================================================
    // Promise operations
    // ========================================================================

    /**
     * Resolve a promise with a value, making it available to any workflow
     * awaiting it via {@link #awaitPromiseMs(String, long)}.
     *
     * @param id    the promise ID (from {@link #createPromise(String)})
     * @param value the resolved value (typically a JSON string)
     * @return a result indicating success, or an error description on failure
     */
    public CleatResult<Void> resolvePromise(String id, String value) {
        int[] p = packStrings(id, value);
        int idOff = p[0], valOff = p[1];
        int idLen = p[2], valLen = p[3];

        long result = cleatResolvePromiseRaw(idOff, idLen, valOff, valLen);

        int errCode = Memory.decodeSimpleErrCode(result);
        if (errCode != 0) {
            return CleatResult.err("resolvePromise(id=\"" + id + "\") failed: host returned error code " + errCode + ". Check that the promise ID is valid.");
        }
        return CleatResult.ok(null);
    }

    /**
     * Reject a promise with an error, causing any workflow awaiting it via
     * {@link #awaitPromiseMs(String, long)} to receive a failure.
     *
     * @param id    the promise ID (from {@link #createPromise(String)})
     * @param error the error description
     * @return a result indicating success, or an error description on failure
     */
    public CleatResult<Void> rejectPromise(String id, String error) {
        int[] p = packStrings(id, error);
        int idOff = p[0], errOff = p[1];
        int idLen = p[2], errLen = p[3];

        long result = cleatRejectPromiseRaw(idOff, idLen, errOff, errLen);

        int errCode = Memory.decodeSimpleErrCode(result);
        if (errCode != 0) {
            return CleatResult.err("rejectPromise(id=\"" + id + "\") failed: host returned error code " + errCode + ". Check that the promise ID is valid.");
        }
        return CleatResult.ok(null);
    }

    // ========================================================================
    // Fire-and-forget service calls
    // ========================================================================

    /**
     * Send a fire-and-forget message to a service (non-blocking).
     * <p>
     * Unlike {@link #cleatCall(String, String, String)}, this method does
     * not wait for a response and does not record the call in the workflow
     * event history.  The message is delivered asynchronously.
     *
     * @param service     the service name
     * @param operation   the operation name
     * @param requestJSON the JSON request payload
     * @return a result indicating success, or an error description on failure
     */
    public CleatResult<Void> cleatSend(String service, String operation, String requestJSON) {
        int[] p = packStrings(service, operation, requestJSON);
        int svcOff = p[0], opOff = p[1], reqOff = p[2];
        int svcLen = p[3], opLen = p[4], reqLen = p[5];

        long result = cleatSendRaw(svcOff, svcLen, opOff, opLen, reqOff, reqLen);

        // The host refuses new work in a defer segment and marks the refusal with
        // bit 31 (IMPROVEMENT-PLAN 3.302). Ask BEFORE decoding: decodeSimpleErrCode
        // reads the low byte, where a stop is 0 -- an ordinary success for a
        // fire-and-forget call, so the guest would report the send as done.
        Memory.throwIfStopped(result);

        int errCode = Memory.decodeSimpleErrCode(result);
        if (errCode != 0) {
            return CleatResult.err("cleatSend(service=\"" + service + "\", operation=\"" + operation + "\") failed: host returned error code " + errCode + ". Check that the service and operation names are valid.");
        }
        return CleatResult.ok(null);
    }

    /**
     * Schedule a service invocation to occur after a delay specified in
     * milliseconds.
     * <p>
     * The invocation is queued and will be delivered to the target service
     * after the specified delay.  This is a fire-and-forget operation.
     * <p>
     * Prefer {@link #scheduleInvoke(String, String, String, long)}
     * (which accepts seconds) over this millisecond variant.
     *
     * @param service     the service name
     * @param operation   the operation name
     * @param requestJSON the JSON request payload
     * @param delayMs     delay in milliseconds before the invocation
     * @return a result indicating success, or an error description on failure
     */
    public CleatResult<Void> scheduleInvokeMs(String service, String operation, String requestJSON, long delayMs) {
        int[] p = packStrings(service, operation, requestJSON);
        int svcOff = p[0], opOff = p[1], reqOff = p[2];
        int svcLen = p[3], opLen = p[4], reqLen = p[5];

        long result = scheduleInvokeRaw(svcOff, svcLen, opOff, opLen, reqOff, reqLen, delayMs);

        // The host refuses new work in a defer segment and marks the refusal with
        // bit 31 (IMPROVEMENT-PLAN 3.302). Ask BEFORE decoding: decodeSimpleErrCode
        // reads the low byte, where a stop is 0 -- an ordinary success for a
        // fire-and-forget call, so the guest would report the send as done.
        Memory.throwIfStopped(result);

        int errCode = Memory.decodeSimpleErrCode(result);
        if (errCode != 0) {
            return CleatResult.err("scheduleInvoke(service=\"" + service + "\", operation=\"" + operation + "\") failed: host returned error code " + errCode + ". Check that the service and operation are valid.");
        }
        return CleatResult.ok(null);
    }

    /**
     * Schedule a service invocation to occur after a delay specified in
     * seconds.
     * <p>
     * This is the preferred API. Delegates to
     * {@link #scheduleInvokeMs(String, String, String, long)}.
     *
     * @param service      the service name
     * @param operation    the operation name
     * @param requestJSON  the JSON request payload
     * @param delaySeconds delay in seconds before the invocation
     * @return a result indicating success, or an error description on failure
     * @see #scheduleInvokeMs(String, String, String, long)
     */
    public CleatResult<Void> scheduleInvoke(String service, String operation, String requestJSON, long delaySeconds) {
        return scheduleInvokeMs(service, operation, requestJSON, delaySeconds * 1000);
    }


    // ========================================================================
    // Detached execution
    // ========================================================================
    //
    // There is no registerQueryHandler here (removed 2026-08-09; previously
    // in this section). Its doc comment claimed "External clients can query
    // this workflow using the cleat query API. The registered handler name
    // is advertised to clients" -- neither half was true: nothing ever
    // advertised the name anywhere, and no worker code ever routed an
    // external query to the handler. See docs/determinism.md, "Why there is
    // no RegisterQueryHandler". Use setQueryState instead; it is durable and
    // externally readable via GET /api/workflows/:id/query?key=X regardless
    // of whether a worker currently has the workflow loaded.

    /**
     * Run a workflow in detached mode (fire-and-forget).
     * <p>
     * The workflow runs independently and its result is not awaited.
     * Useful for fan-out patterns where the caller does not need the result.
     *
     * @param workflowName the workflow type/name to run
     * @param inputJSON    the input JSON for the detached workflow
     * @return a result indicating success, or an error description on failure
     */
    public CleatResult<Void> runDetached(String workflowName, String inputJSON) {
        int[] p = packStrings(workflowName, inputJSON);
        int nameOff = p[0], inOff = p[1];
        int nameLen = p[2], inLen = p[3];

        long result = cleatRunDetachedRaw(nameOff, nameLen, inOff, inLen);

        // The host refuses new work in a defer segment and marks it with bit 31
        // (IMPROVEMENT-PLAN 3.111). Before decoding: this is a simple-result
        // layout in which bit 31 is not a field, so a stop decoded field-first
        // is errCode 0 -- a SUCCESS, and the guest goes on to do the work the
        // segment exists to prevent.
        Memory.throwIfStopped(result);

        int errCode = Memory.decodeSimpleErrCode(result);
        if (errCode != 0) {
            return CleatResult.err("runDetached failed with code " + errCode);
        }
        return CleatResult.ok(null);
    }

    // ========================================================================
    // Run ID
    // ========================================================================

    /**
     * Get the current run ID for this workflow execution.
     * <p>
     * The run ID uniquely identifies this specific execution of the workflow
     * (as opposed to the workflow ID which identifies the logical workflow
     * instance across potential re-executions and continue-as-new).
     *
     * @return the run ID string, or empty string if unavailable
     */
    public String currentRunId() {
        long result = cleatRunIdRaw(Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);
        int errCode = Memory.decodeSimpleErrCode(result);
        int idLen = Memory.decodeSimpleExtra(result);
        if (errCode != 0 || idLen == 0) {
            return "";
        }
        return readOutput(idLen);
    }

    // ========================================================================
    // State operations (scoped for virtual objects)
    // ========================================================================


    /**
     * Record a side-effect result for deterministic replay.
     * <p>
     * Side effects are non-deterministic operations whose results are recorded
     * in the workflow event history.  On replay, the recorded result is
     * returned instead of re-executing the side effect.
     *
     * @param computedResult the result of the side effect computation
     * @return a result containing the side effect output on success, or an
     *         error description on failure
     */
    public CleatResult<String> sideEffect(String computedResult) {
        int[] p = packStrings(computedResult);

        long result = cleatSideEffectRaw(
            p[0], p[1],
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        // Bit 31 before any field. IMPROVEMENT-PLAN 3.300: a stop decodes here as
        // errCode=0 with extra=0 -- an empty SUCCESSFUL response, so the guest
        // would return "" and carry on.
        Memory.throwIfStopped(result);

        int errCode = Memory.decodeSimpleErrCode(result);
        int outLen = Memory.decodeSimpleExtra(result);

        if (errCode != 0) {
            return CleatResult.err(hostMessageOr(outLen, "sideEffect(...) failed: host returned error code " + errCode + ". Check that the input is valid."));
        }

        String output = readOutput(outLen);
        return CleatResult.ok(output);
    }






    // ========================================================================
    // awaitAllChildren
    // ========================================================================

    /**
     * Wait for all specified child workflow run IDs to complete.
     * <p>
     * The run IDs are passed as a JSON array string (e.g.
     * {@code ["run-1","run-2"]}).  The method suspends until all children
     * have completed, then returns their results as a JSON array.
     *
     * @param runIDs the child workflow run IDs to wait for
     * @return a result containing the results JSON array on success, or an
     *         error description on failure
     */
    public CleatResult<String> awaitAllChildren(String[] runIDs) {
        // Serialize run IDs as a JSON string array.
        StringBuilder sb = new StringBuilder("[");
        for (int i = 0; i < runIDs.length; i++) {
            if (i > 0) {
                sb.append(",");
            }
            sb.append("\"").append(JsonHelper.escapeJson(runIDs[i])).append("\"");
        }
        sb.append("]");
        String idsJson = sb.toString();

        int[] p = packStrings(idsJson);

        long result = cleatAwaitAllChildrenRaw(
            p[0], p[1],
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        int errCode = Memory.decodeSimpleErrCode(result);
        int resultLen = Memory.decodeSimpleExtra(result);

        if (errCode != 0) {
            return CleatResult.err(hostMessageOr(resultLen, "awaitAllChildren(runIDs=" + idsJson + ") failed: host returned error code " + errCode + ". Check that the run IDs are valid."));
        }

        String response = readOutput(resultLen);
        return CleatResult.ok(response);
    }

    // ========================================================================
    // awaitAnyChild
    // ========================================================================

    /**
     * Wait for any of the specified child workflows to complete.
     * <p>
     * The run IDs are passed as a JSON array string (e.g.
     * {@code ["run-1","run-2"]}).  The method suspends until at least one
     * child has completed, then returns its result.
     *
     * @param runIDs the child workflow run IDs to wait for
     * @return a result containing the completed child's output JSON on success,
     *         or an error description on failure
     */
    public CleatResult<String> awaitAnyChild(String[] runIDs) {
        // Serialize run IDs as a JSON string array.
        StringBuilder sb = new StringBuilder("[");
        for (int i = 0; i < runIDs.length; i++) {
            if (i > 0) {
                sb.append(",");
            }
            sb.append("\"").append(JsonHelper.escapeJson(runIDs[i])).append("\"");
        }
        sb.append("]");
        String idsJson = sb.toString();

        int[] p = packStrings(idsJson);

        long result = cleatAwaitAnyChildRaw(
            p[0], p[1],
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        // Check for suspension sentinel before decoding the result.
        if (result == Memory.SUSPEND_SENTINEL) {
            return CleatResult.err("no child workflow yet complete; workflow must suspend");
        }

        int errCode = Memory.decodeSimpleErrCode(result);
        int resultLen = Memory.decodeSimpleExtra(result);

        if (errCode != 0) {
            return CleatResult.err(hostMessageOr(resultLen, "awaitAnyChild(runIDs=" + idsJson + ") failed: host returned error code " + errCode + ". Check that the run IDs are valid."));
        }

        String childResult = readOutput(resultLen);
        return CleatResult.ok(childResult);
    }

    // ========================================================================
    // cron
    // ========================================================================

    /**
     * Create a recurring workflow trigger from a cron expression.
     * <p>
     * Returns the schedule ID, which {@link #deleteCron(String)} takes.
     * Mirrors Go's {@code ScheduleCron(workflowName, cronExpr, timezone, inputJSON)}.
     * <p>
     * {@code timezone} is optional: {@code ""} means the engine's default. The
     * host reads it as a payload rather than as a required string, which is
     * what makes the empty value legal rather than a bad-parameter error.
     *
     * @param workflowName the workflow definition name to trigger
     * @param cronExpr     a standard 5-field cron expression
     * @param timezone     an IANA timezone name, or "" for the engine default
     * @param inputJSON    the JSON input handed to each triggered run
     * @return a result containing the schedule ID, or an error description
     */
    public CleatResult<String> scheduleCron(String workflowName, String cronExpr, String timezone, String inputJSON) {
        int[] p = packStrings(workflowName, cronExpr, timezone, inputJSON);

        long result = cleatScheduleCronRaw(
            p[0], p[4],
            p[1], p[5],
            p[2], p[6],
            p[3], p[7],
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        // Ask BEFORE decoding. A cron schedule is new work with the longest
        // reach of anything in this family -- it registers a RECURRING trigger,
        // so a workflow that kept going after a refusal would leave something
        // starting fresh runs indefinitely. decodeSimpleErrCode reads the low
        // byte, where a stop is 0, and the length as 0: an empty SUCCESSFUL
        // response carrying an empty schedule ID.
        Memory.throwIfStopped(result);

        int errCode = Memory.decodeSimpleErrCode(result);
        int resultLen = Memory.decodeSimpleExtra(result);
        if (errCode != 0) {
            // Read the BUFFER, not the code. The host writes its own message
            // there on failure -- engine/schedules.go writes rec.Err into the
            // id buffer and returns packSimpleResult(1, written) -- so a guest
            // that prints the bare code throws away the only thing that says
            // what went wrong. That is IMPROVEMENT-PLAN 3.200, fixed there for
            // the generated Go adapters.
            //
            // Note this does NOT match what most of this file does: 11 of the
            // 20 wrappers here with an output buffer still report a bare code.
            // The seven that read it are cleatCall, cleatCallHeartbeat,
            // cleatFetch, pluginCall, pluginCallStreaming, jsonParse and
            // jsonStringify. The remaining 11 are tracked separately.
            String msg = readOutput(resultLen);
            if (msg.isEmpty()) {
                return CleatResult.err("scheduleCron(workflowName=\"" + workflowName + "\", cronExpr=\"" + cronExpr
                    + "\") failed: host returned error code " + errCode + ".");
            }
            return CleatResult.err(msg);
        }
        return CleatResult.ok(readOutput(resultLen));
    }

    /**
     * Remove a previously registered cron schedule by its ID.
     * <p>
     * There is no stop-bit check here, and that is deliberate rather than an
     * omission: {@code DeleteCron} does not call {@code stopBeforeNewWork}
     * host-side, because removing a schedule is not new work. Verified against
     * {@code engine/schedules.go} on 2026-09-07; {@code ScheduleCron} is the
     * only one of the three that can be refused.
     *
     * @param scheduleID the schedule ID returned by scheduleCron
     * @return a result indicating success, or an error description
     */
    public CleatResult<Void> deleteCron(String scheduleID) {
        int[] p = packStrings(scheduleID);
        long result = cleatDeleteCronRaw(p[0], p[1]);

        int errCode = Memory.decodeSimpleErrCode(result);
        if (errCode != 0) {
            return CleatResult.err("deleteCron(scheduleID=\"" + scheduleID
                + "\") failed: host returned error code " + errCode
                + ". Check that the schedule ID exists.");
        }
        return CleatResult.ok(null);
    }

    /**
     * List all registered cron schedules, as a JSON array.
     * <p>
     * See {@link #deleteCron(String)} for why there is no stop-bit check here
     * either.
     *
     * @return a result containing the JSON array of schedules, or an error
     */
    public CleatResult<String> listCrons() {
        long result = cleatListCronsRaw(Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        int errCode = Memory.decodeSimpleErrCode(result);
        int resultLen = Memory.decodeSimpleExtra(result);
        if (errCode != 0) {
            // The host's message, not the code -- see scheduleCron above.
            String msg = readOutput(resultLen);
            if (msg.isEmpty()) {
                return CleatResult.err("listCrons() failed: host returned error code " + errCode + ".");
            }
            return CleatResult.err(msg);
        }
        return CleatResult.ok(readOutput(resultLen));
    }

    // ========================================================================
    // Typed child workflow wrappers
    // ========================================================================

    /**
     * Start a child workflow with a typed input.
     * <p>
     * The input is serialized to JSON using {@link JsonHelper#stringify(Object)}.
     * This method otherwise behaves identically to
     * {@link #childWorkflow(String, String)}.
     *
     * @param name  the child workflow type/name
     * @param input the input object (serialized to JSON)
     * @param <T>   the input type
     * @return a result containing the child's run ID on success, or an error
     *         description on failure
     */
    public <T> CleatResult<String> childWorkflowTyped(String name, T input) {
        String inputJson = JsonHelper.stringify(input);
        return childWorkflow(name, inputJson);
    }

    /**
     * Wait for a child workflow to complete and deserialize its result.
     * <p>
     * This is a typed wrapper around {@link #awaitChild(String)} that
     * deserializes the result JSON into the requested type using
     * {@link JsonHelper#parse(String, Class)}.
     *
     * @param runID the child workflow run ID
     * @param clazz the expected result type class
     * @param <T>   the result type
     * @return a result containing the deserialized child output on success,
     *         or an error description on failure
     */
    public <T> CleatResult<T> awaitChildTyped(String runID, Class<T> clazz) {
        CleatResult<String> result = awaitChild(runID);
        if (result.isErr()) {
            return CleatResult.err(result.getError());
        }
        try {
            T value = JsonHelper.parse(result.getValue(), clazz);
            return CleatResult.ok(value);
        } catch (Exception e) {
            String jsonStr = result.getValue();
            String truncatedJson = jsonStr.substring(0, Math.min(jsonStr.length(), 200));
            return CleatResult.err("awaitChildTyped: failed to parse result: " + e.getMessage() + ". JSON: " + truncatedJson);
        }
    }

    /**
     * Make a durable (deterministically replayed) call to an external service
     * with periodic heartbeat progress updates.
     * <p>
     * The host sends periodic heartbeat updates during the call.  This is
     * useful for long-running operations where the caller needs progress
     * visibility or to detect stalled calls.
     *
     * @param service            the service name (e.g. {@code "orders"})
     * @param operation          the operation name (e.g. {@code "create"})
     * @param requestJSON        the JSON request payload
     * @param heartbeatIntervalMs interval between heartbeat progress updates
     *                            in milliseconds
     * @return a result containing the JSON response on success, or an error
     *         description on failure
     */
    public CleatResult<String> cleatCallHeartbeat(String service, String operation, String requestJSON, long heartbeatIntervalMs) {
        int[] p = packStrings(service, operation, requestJSON);
        int svcOff = p[0], opOff = p[1], reqOff = p[2];
        int svcLen = p[3], opLen = p[4], reqLen = p[5];

        long result = cleatCallHeartbeatRaw(
            svcOff, svcLen,
            opOff, opLen,
            reqOff, reqLen,
            heartbeatIntervalMs,
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        Memory.throwIfStopped(result);

        int errCode = Memory.decodeCallErrCode(result);
        int responseLen = Memory.decodeCallResponseLen(result);

        if (errCode != 0) {
            String errMsg = readOutput(responseLen);
            return CleatResult.err(errMsg);
        }

        String response = readOutput(responseLen);
        return CleatResult.ok(response);
    }

    // ========================================================================
    // Typed cleatCall wrappers
    // ========================================================================

    /**
     * Make a typed durable call to an external service.
     * <p>
     * The request object is serialized to JSON and the response is
     * deserialized to the requested type.
     *
     * @param service       the service name
     * @param operation     the operation name
     * @param request       the request object (serialized to JSON)
     * @param responseClass the expected response type class
     * @param <T>           the request type
     * @param <R>           the response type
     * @return a result containing the deserialized response on success, or an
     *         error description on failure
     */
    public <T, R> CleatResult<R> cleatCallTyped(
            String service, String operation, T request, Class<R> responseClass) {
        String requestJson = JsonHelper.stringify(request);

        CleatResult<String> result = cleatCall(service, operation, requestJson);
        if (result.isErr()) {
            return CleatResult.err(result.getError());
        }

        try {
            R response = JsonHelper.parse(result.getValue(), responseClass);
            return CleatResult.ok(response);
        } catch (Exception e) {
            String respJson = result.getValue();
            String truncatedJson = respJson.substring(0, Math.min(respJson.length(), 200));
            return CleatResult.err("cleatCallTyped: failed to parse response: " + e.getMessage() + ". JSON: " + truncatedJson);
        }
    }

    /**
     * Make a typed durable call with a retry policy.
     * <p>
     * The retry policy is serialized and passed to the host runtime, which
     * handles retries automatically.  The request and response are
     * automatically serialized/deserialized.
     *
     * @param service       the service name
     * @param operation     the operation name
     * @param request       the request object (serialized to JSON)
     * @param responseClass the expected response type class
     * @param retryPolicy   the retry policy configuration
     * @param <T>           the request type
     * @param <R>           the response type
     * @return a result containing the deserialized response on success, or an
     *         error description on failure
     */
    public <T, R> CleatResult<R> cleatCallWithRetry(
            String service, String operation, T request,
            Class<R> responseClass, RetryPolicy retryPolicy) {
        String requestJson = JsonHelper.stringify(request);

        // Serialize nonRetryableErrors as a JSON array string.
        String nonRetryableErrorsJson = retryPolicy.nonRetryableErrorsToJson();

        // Pack service, operation, requestJson into scratch memory (three strings).
        int[] p = packStrings(service, operation, requestJson);
        int svcOff = p[0], opOff = p[1], reqOff = p[2];
        int svcLen = p[3], opLen = p[4], reqLen = p[5];

        // Write nonRetryableErrorsJson into scratch memory after the packed strings.
        int current = Memory.SCRATCH_BASE + svcLen + opLen + reqLen;
        byte[] nreBytes = nonRetryableErrorsJson.getBytes(java.nio.charset.StandardCharsets.UTF_8);
        int nreLen = Math.min(nreBytes.length, Memory.OUT_BUF_SIZE);
        for (int i = 0; i < nreLen; i++) {
            org.teavm.interop.Address.fromInt(current + i).putByte(nreBytes[i]);
        }
        int nreOff = current;

        // Compute backoffCoefficient100x from backoffMultiplier.
        long backoffCoefficient100x = (long) (retryPolicy.backoffMultiplier * 100.0);

        long result = cleatCallRetryRaw(
            svcOff, svcLen,
            opOff, opLen,
            reqOff, reqLen,
            retryPolicy.maxAttempts, retryPolicy.initialIntervalMs,
            backoffCoefficient100x, retryPolicy.maximumIntervalMs,
            nreOff, nreLen,
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        Memory.throwIfStopped(result);

        int errCode = Memory.decodeCallErrCode(result);
        int responseLen = Memory.decodeCallResponseLen(result);

        if (errCode != 0) {
            String errMsg = readOutput(responseLen);
            return CleatResult.err(errMsg);
        }

        String response = readOutput(responseLen);
        try {
            R parsed = JsonHelper.parse(response, responseClass);
            return CleatResult.ok(parsed);
        } catch (Exception e) {
            String truncatedJson = response.substring(0, Math.min(response.length(), 200));
            return CleatResult.err("cleatCallWithRetry: failed to parse response: " + e.getMessage() + ". JSON: " + truncatedJson);
        }
    }

    // ========================================================================
    // HTTP fetch helper
    // ========================================================================

    /**
     * Make an HTTP request using the durable fetch host function.
     * <p>
     * The fetch call is deterministic and recorded in the workflow event
     * history for replay.  The response includes the HTTP status code,
     * response headers, and body.
     *
     * @param method  the HTTP method (e.g. {@code "GET"}, {@code "POST"})
     * @param url     the request URL
     * @param headers optional HTTP headers (may be null)
     * @param body    optional request body (may be null for GET requests)
     * @return a result containing the {@link FetchResult} on success, or an
     *         error description on failure
     */
    public CleatResult<FetchResult> cleatFetch(
            String method, String url, Map<String, String> headers, String body) {
        String headersJson = headersToJson(headers);
        if (body == null) {
            body = "";
        }

        int[] p = packStrings(method, url, headersJson, body);
        int methodOff = p[0], urlOff = p[1], hdrOff = p[2], bodyOff = p[3];
        int methodLen = p[4], urlLen = p[5], hdrLen = p[6], bodyLen = p[7];

        long result = cleatFetchRaw(
            methodOff, methodLen,
            urlOff, urlLen,
            hdrOff, hdrLen,
            bodyOff, bodyLen,
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        Memory.throwIfStopped(result);

        int errCode = Memory.decodeCallErrCode(result);
        int responseLen = Memory.decodeCallResponseLen(result);

        if (errCode != 0) {
            String errMsg = readOutput(responseLen);
            return CleatResult.err(errMsg);
        }

        String responseJson = readOutput(responseLen);
        try {
            java.util.Map<String, Object> parsed = JsonHelper.parseObject(responseJson);

            int statusCode = 0;
            Object sc = parsed.get("status_code");
            if (sc instanceof Number) {
                statusCode = ((Number) sc).intValue();
            }

            java.util.Map<String, String> respHeaders = new java.util.HashMap<>();
            Object h = parsed.get("headers");
            if (h instanceof Map) {
                for (Map.Entry<String, Object> e : ((Map<String, Object>) h).entrySet()) {
                    respHeaders.put(e.getKey(), e.getValue() != null ? e.getValue().toString() : "");
                }
            }

            String respBody = parsed.get("body") != null ? parsed.get("body").toString() : "";

            return CleatResult.ok(new FetchResult(statusCode, respHeaders, respBody));
        } catch (Exception e) {
            String truncatedJson = responseJson.substring(0, Math.min(responseJson.length(), 200));
            return CleatResult.err("cleatFetch: failed to parse response: " + e.getMessage() + ". JSON: " + truncatedJson);
        }
    }

    /**
     * Convenience method for HTTP GET requests.
     * <p>
     * Equivalent to {@code cleatFetch("GET", url, null, null)}.
     *
     * @param url the request URL
     * @return a result containing the {@link FetchResult} on success, or an
     *         error description on failure
     */
    public CleatResult<FetchResult> fetchGet(String url) {
        return cleatFetch("GET", url, null, null);
    }

    /**
     * Convenience method for HTTP GET requests, returning only the body.
     * <p>
     * Equivalent to {@code fetchGet(url)} but extracts the body from the
     * {@link FetchResult}.
     *
     * @param url the request URL
     * @return a result containing the response body on success, or an error
     *         description on failure
     */
    public CleatResult<String> fetchGetJson(String url) {
        CleatResult<FetchResult> result = cleatFetch("GET", url, null, null);
        if (result.isErr()) {
            return CleatResult.err(result.getError());
        }
        return CleatResult.ok(result.getValue().body);
    }

    /**
     * Convenience method for HTTP GET requests with custom headers,
     * returning only the body.
     * <p>
     * Equivalent to {@code fetchGet(url)} with custom headers, but extracts
     * the body from the {@link FetchResult}.
     *
     * @param url     the request URL
     * @param headers optional HTTP headers
     * @return a result containing the response body on success, or an error
     *         description on failure
     */
    public CleatResult<String> fetchGetJson(String url, Map<String, String> headers) {
        CleatResult<FetchResult> result = cleatFetch("GET", url, headers, null);
        if (result.isErr()) {
            return CleatResult.err(result.getError());
        }
        return CleatResult.ok(result.getValue().body);
    }

    // ========================================================================
    // Lock operations
    // ========================================================================

    /**
     * Attempt to acquire a concurrency lock for the given key, with a TTL
     * specified in milliseconds.
     * <p>
     * The lock is held for at most {@code ttlMs} milliseconds.  Returns
     * {@code true} if the lock was acquired, {@code false} if it was already
     * held by another workflow.
     * <p>
     * Prefer {@link #acquireLock(String, long)} (which accepts seconds) over
     * this millisecond variant.
     *
     * @param key   the lock key
     * @param ttlMs time-to-live in milliseconds
     * @return a result containing {@code true} if the lock was acquired,
     *         {@code false} if already held; or an error description on failure
     */
    public CleatResult<Boolean> acquireLockMs(String key, long ttlMs) {
        int[] p = packStrings(key);
        int keyOff = p[0], keyLen = p[1];

        long result = cleatAcquireLockRaw(keyOff, keyLen, ttlMs);

        // The host refuses new work in a defer segment and marks the refusal with
        // bit 31 (IMPROVEMENT-PLAN 3.301). Ask BEFORE decoding: this layout puts
        // `acquired` at bit 8 and errCode in the low byte, so a stop decodes as
        // errCode=0, acquired=false -- an ordinary "someone else holds it", and
        // the workflow takes its did-not-get-the-lock branch and runs on.
        Memory.throwIfStopped(result);

        int errCode = (int) (result & 0xFFL);
        if (errCode != 0) {
            return CleatResult.err("acquireLock(key=\"" + key + "\", ttlMs=" + ttlMs + ") failed: host returned error code " + errCode + ". Check that the lock key is valid.");
        }

        return CleatResult.ok(((result >> 8) & 0x1L) != 0);
    }

    /**
     * Attempt to acquire a concurrency lock for the given key, with a TTL
     * specified in seconds.
     * <p>
     * This is the preferred API. Delegates to
     * {@link #acquireLockMs(String, long)}.
     *
     * @param key         the lock key
     * @param ttlSeconds  time-to-live in seconds
     * @return a result containing {@code true} if the lock was acquired,
     *         {@code false} if already held; or an error description on failure
     * @see #acquireLockMs(String, long)
     */
    public CleatResult<Boolean> acquireLock(String key, long ttlSeconds) {
        return acquireLockMs(key, ttlSeconds * 1000);
    }

    /**
     * Release a concurrency lock previously acquired by this workflow.
     *
     * @param key the lock key to release
     * @return a result indicating success, or an error description on failure
     */
    public CleatResult<Void> releaseLock(String key) {
        int[] p = packStrings(key);
        int keyOff = p[0], keyLen = p[1];

        long result = cleatReleaseLockRaw(keyOff, keyLen);

        int errCode = (int) (result & 0xFFL);
        if (errCode != 0) {
            return CleatResult.err("releaseLock(key=\"" + key + "\") failed: host returned error code " + errCode + ". Check that the lock key is valid and the lock is held.");
        }
        return CleatResult.ok(null);
    }

    // ========================================================================
    // JSON utility operations
    // ========================================================================

    /**
     * Parse and normalize a JSON string, returning a normalized (canonical)
     * form.
     * <p>
     * This is a deterministic host-call that ensures consistent JSON format
     * across workflow replays.
     *
     * @param json the JSON string to parse
     * @return a result containing the normalized JSON string on success, or
     *         an error description on failure
     */
    public CleatResult<String> jsonParse(String json) {
        int[] p = packStrings(json);

        long result = cleatJsonParseRaw(
            p[0], p[1],
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        int errCode = Memory.decodeCallErrCode(result);
        int outLen = Memory.decodeCallResponseLen(result);

        if (errCode != 0) {
            String errMsg = readOutput(outLen);
            return CleatResult.err(errMsg);
        }

        String normalized = readOutput(outLen);
        return CleatResult.ok(normalized);
    }

    /**
     * Convert a JSON-compatible value to its canonical JSON string
     * representation via the host runtime.
     * <p>
     * This is a deterministic host-call that ensures consistent JSON output
     * across workflow replays.
     *
     * @param json the JSON-compatible string to stringify
     * @return a result containing the canonical JSON string on success, or
     *         an error description on failure
     */
    public CleatResult<String> jsonStringify(String json) {
        int[] p = packStrings(json);

        long result = cleatJsonStringifyRaw(
            p[0], p[1],
            Memory.OUTPUT_OFFSET, Memory.OUT_BUF_SIZE);

        int errCode = Memory.decodeCallErrCode(result);
        int outLen = Memory.decodeCallResponseLen(result);

        if (errCode != 0) {
            String errMsg = readOutput(outLen);
            return CleatResult.err(errMsg);
        }

        String canonical = readOutput(outLen);
        return CleatResult.ok(canonical);
    }

    // ========================================================================
    // Inner result type for awaitSignals
    // ========================================================================

    /**
     * Result of an {@link #awaitSignalsMs(String[], long)} call.
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
         * The address to answer this signal at, or empty for a one-way signal.
         * <p>
         * Non-empty only when the sender used
         * {@link HostCalls#sendSignalAndWaitMs(String, String, String, long)}
         * and is suspended waiting for a reply; pass it to
         * {@link HostCalls#replyToSignal(String, String)}. A signal sent with
         * {@link HostCalls#signalWorkflow(String, String, String)} leaves it
         * empty, which is how a receiver tells a request that wants an answer
         * from a one-way notification. IMPROVEMENT-PLAN 3.220.
         */
        public final String replyTo;

        /**
         * Construct a new await-signals result with no reply address, for a
         * one-way signal.
         *
         * @param signalName the received signal name
         * @param payload    the signal payload
         * @param timedOut   whether the wait timed out
         */
        public AwaitSignalsResult(String signalName, String payload, boolean timedOut) {
            this(signalName, payload, timedOut, "");
        }

        /**
         * Construct a new await-signals result.
         *
         * @param signalName the received signal name
         * @param payload    the signal payload
         * @param timedOut   whether the wait timed out
         * @param replyTo    the address to answer at, or empty
         */
        public AwaitSignalsResult(String signalName, String payload, boolean timedOut, String replyTo) {
            this.signalName = signalName;
            this.payload = payload;
            this.timedOut = timedOut;
            this.replyTo = replyTo;
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
     * Result of an {@link #awaitPromiseMs(String, long)} call.
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

    // ========================================================================
    // RetryPolicy
    // ========================================================================

    /**
     * Configuration for retry behaviour in
     * {@link #cleatCallWithRetry(String, String, Object, Class, RetryPolicy)}.
     * <p>
     * Controls the maximum number of attempts, initial backoff interval,
     * backoff multiplier, and maximum backoff interval for retried calls.
     */
    /**
     * Options for starting a child workflow with version control.
     * Use with {@link #childWorkflowWithOptions(String, String, long, String)}.
     * {@code version = 0} means default resolution (parent's version).
     */
    public static class ChildWorkflowOptions {
        /** Explicit workflow definition version to use. 0 = default. */
        public long version;
        /** Parent close policy (e.g. "abandon", "terminate", "request_cancel"). */
        public String parentClosePolicy;

        public ChildWorkflowOptions() {
            this.version = 0;
            this.parentClosePolicy = "";
        }

        public ChildWorkflowOptions(long version) {
            this.version = version;
            this.parentClosePolicy = "";
        }
    }

    public static class RetryPolicy {
        /** Maximum number of retry attempts (including the initial call). */
        public final int maxAttempts;

        /** Initial backoff interval in milliseconds. */
        public final long initialIntervalMs;

        /** Multiplier applied to the interval after each retry (e.g. 2.0 for exponential backoff). */
        public final double backoffMultiplier;

        /** Maximum backoff interval in milliseconds. */
        public final long maximumIntervalMs;

        /** List of non-retryable error codes (serialized as JSON array). */
        public final String[] nonRetryableErrors;

        /**
         * Construct a new retry policy.
         *
         * @param maxAttempts         maximum number of retry attempts
         * @param initialIntervalMs   initial backoff interval in milliseconds
         * @param backoffMultiplier   backoff multiplier (> 1.0 for exponential backoff)
         * @param maximumIntervalMs   maximum backoff interval in milliseconds
         * @param nonRetryableErrors  list of non-retryable error codes (may be null or empty)
         */
        public RetryPolicy(int maxAttempts, long initialIntervalMs,
                           double backoffMultiplier, long maximumIntervalMs,
                           String[] nonRetryableErrors) {
            this.maxAttempts = maxAttempts;
            this.initialIntervalMs = initialIntervalMs;
            this.backoffMultiplier = backoffMultiplier;
            this.maximumIntervalMs = maximumIntervalMs;
            this.nonRetryableErrors = nonRetryableErrors != null ? nonRetryableErrors : new String[0];
        }

        /**
         * Construct a new retry policy with no non-retryable errors.
         */
        public RetryPolicy(int maxAttempts, long initialIntervalMs,
                           double backoffMultiplier, long maximumIntervalMs) {
            this(maxAttempts, initialIntervalMs, backoffMultiplier, maximumIntervalMs, null);
        }

        /**
         * Serialize {@link #nonRetryableErrors} as a JSON array string.
         * Returns {@code "[]"} if the array is null or empty.
         */
        String nonRetryableErrorsToJson() {
            if (nonRetryableErrors == null || nonRetryableErrors.length == 0) {
                return "[]";
            }
            StringBuilder sb = new StringBuilder("[");
            for (int i = 0; i < nonRetryableErrors.length; i++) {
                if (i > 0) {
                    sb.append(",");
                }
                sb.append("\"").append(JsonHelper.escapeJson(nonRetryableErrors[i])).append("\"");
            }
            sb.append("]");
            return sb.toString();
        }
    }

    // ========================================================================
    // FetchResult
    // ========================================================================

    /**
     * Result of an HTTP fetch via {@link #cleatFetch(String, String, Map, String)}
     * or {@link #fetchGet(String)}.
     * <p>
     * Contains the HTTP status code, response headers, and response body.
     */
    public static class FetchResult {
        /** The HTTP status code (e.g. 200, 404, 500). */
        public final int statusCode;

        /** Response headers as a map of header name to header value. */
        public final Map<String, String> headers;

        /** The response body as a string. */
        public final String body;

        /**
         * Construct a new fetch result.
         *
         * @param statusCode the HTTP status code
         * @param headers    response headers
         * @param body       response body
         */
        public FetchResult(int statusCode, Map<String, String> headers, String body) {
            this.statusCode = statusCode;
            this.headers = headers;
            this.body = body;
        }
    }
}
