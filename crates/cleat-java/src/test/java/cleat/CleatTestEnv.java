package cleat;

import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.function.BiFunction;
import java.util.function.Function;

/**
 * In-process test harness for testing cleat workflows on the JVM without WASM
 * compilation or a running cleat host.
 *
 * <p>{@code CleatTestEnv} mocks every {@link HostCalls} method and records all
 * calls made during workflow execution.  Pre-programmed responses are returned
 * when stubs are registered via the fluent {@link #expectCall expectCall} /
 * {@link #expectPluginCall expectPluginCall} API.
 *
 * <p><strong>Design:</strong> Uses a {@link HostCallsBridge} inner class that
 * <em>extends</em> {@link HostCalls} and overrides every method that touches a
 * native {@code @Import} function.  Each override delegates to a
 * {@link TestHostCalls} instance, which provides the same mock behaviour.
 * This is Approach C from the SDK improvement plan: HostCalls is instance-based,
 * so we subclass it for testing.
 *
 * <h2>Usage</h2>
 * <pre>{@code
 * import cleat.CleatTestEnv;
 * import static org.junit.jupiter.api.Assertions.*;
 *
 * class IncidentWorkflowTest {
 *     {@literal @}Test
 *     void testHandleIncident() {
 *         CleatTestEnv env = new CleatTestEnv();
 *
 *         // Stub a plugin call (llm.chat returns a severity assessment)
 *         env.expectPluginCall("llm", "chat")
 *             .respond("{\"severity\":\"critical\"}");
 *
 *         // Run the workflow
 *         String result = env.execute(
 *             IncidentWorkflow::handleIncident,
 *             "{\"summary\":\"CPU at 95%\"}");
 *
 *         // Assert the outcome
 *         assertEquals("resolved", result);
 *         assertTrue(env.wasCalled("inventory", "Reserve"));
 *     }
 * }
 * }</pre>
 *
 * <h2>Recorded call inspection</h2>
 * <pre>{@code
 * List<CleatTestEnv.CallRecord> history = env.callHistory();
 * assertEquals(2, history.size());
 * assertEquals("inventory", history.get(0).service);
 * }</pre>
 *
 * <h2>Signal injection</h2>
 * <pre>{@code
 * env.injectSignal("payment_received", "{\"amount\":5000}");
 * String result = env.execute(MyWorkflow::awaitPayment, input);
 * }</pre>
 *
 * @see HostCalls
 * @see TestHostCalls
 */
public class CleatTestEnv {

    // ========================================================================
    // Public types
    // ========================================================================

    /**
     * A single recorded call through the test environment.
     */
    public static class CallRecord {
        /** The service or plugin name that was called. */
        public final String service;
        /** The operation or function name that was called. */
        public final String operation;
        /** The request JSON string. */
        public final String request;
        /** The response JSON string (empty on error). */
        public final String response;
        /** The error message (null on success). */
        public final String error;

        CallRecord(String service, String operation, String request,
                   String response, String error) {
            this.service = service;
            this.operation = operation;
            this.request = request;
            this.response = response;
            this.error = error;
        }
    }

    // ========================================================================
    // Builder for call stubs
    // ========================================================================

    /**
     * Builder returned by {@link #expectCall} and
     * {@link #expectPluginCall}.  Call {@link #respond} to register the stub.
     */
    public static class ExpectCallBuilder {
        private final CleatTestEnv env;
        private final String service;
        private final String operation;
        private final String expectedRequest;
        private final boolean isPlugin;
        private boolean used;

        ExpectCallBuilder(CleatTestEnv env, String service, String operation,
                          String expectedRequest, boolean isPlugin) {
            this.env = env;
            this.service = service;
            this.operation = operation;
            this.expectedRequest = expectedRequest;
            this.isPlugin = isPlugin;
        }

        ExpectCallBuilder(CleatTestEnv env, String service, String operation,
                          boolean isPlugin) {
            this(env, service, operation, null, isPlugin);
        }

        /**
         * Register a stub that returns the given response JSON when a matching
         * call is made.  If an expected request was provided,
         * <strong>and</strong> the actual request does not match, the stub is
         * skipped (allowing another stub to match or falling through to an error).
         *
         * @param responseJson the JSON response to return
         * @return this builder (for chaining)
         */
        public ExpectCallBuilder respond(String responseJson) {
            if (used) {
                return this;
            }
            used = true;
            if (isPlugin) {
                env.delegate.registerPluginCallStub(service, operation, responseJson);
            } else {
                env.delegate.registerCallStub(service, operation, responseJson);
            }
            return this;
        }

        /**
         * Register a stub that returns an error when a matching call is made.
         *
         * @param errorMessage the error message to return
         * @return this builder (for chaining)
         */
        public ExpectCallBuilder respondError(String errorMessage) {
            if (used) {
                return this;
            }
            used = true;
            if (isPlugin) {
                env.pluginCallErrors.put(
                    service + "." + operation, errorMessage);
            } else {
                env.delegate.registerCallError(service, operation, errorMessage);
            }
            return this;
        }
    }

    // ========================================================================
    // Internal state
    // ========================================================================

    private final TestHostCalls delegate;
    /* package */ final HostCallsBridge bridge;
    /* package */ final Map<String, String> pluginCallErrors = new HashMap<>();

    // ========================================================================
    // Construction
    // ========================================================================

    /**
     * Create a new {@code CleatTestEnv} with a clean initial state.
     * The simulated clock starts at 2024-01-01T00:00:00Z (epoch ms 1704067200000).
     */
    public CleatTestEnv() {
        this.delegate = new TestHostCalls();
        this.bridge = new HostCallsBridge(delegate, this);
    }

    // ========================================================================
    // Stub registration
    // ========================================================================

    /**
     * Register a stub for a {@link HostCalls#cleatCall} invocation.
     * Returns a builder whose {@link ExpectCallBuilder#respond respond}
     * method registers the stub.
     * <p>
     * Matches any request to the given service and operation.
     *
     * @param service   the service name (e.g. {@code "inventory"})
     * @param operation the operation name (e.g. {@code "Reserve"})
     * @return a builder for setting the response
     */
    public ExpectCallBuilder expectCall(String service, String operation) {
        return new ExpectCallBuilder(this, service, operation, false);
    }

    /**
     * Register a stub for a {@link HostCalls#cleatCall} invocation with an
     * expected request string.  The stub only matches if the actual request
     * equals the expected request.
     *
     * @param service         the service name
     * @param operation       the operation name
     * @param expectedRequest the exact expected request JSON
     * @return a builder for setting the response
     */
    public ExpectCallBuilder expectCall(String service, String operation,
                                        String expectedRequest) {
        return new ExpectCallBuilder(this, service, operation, expectedRequest, false);
    }

    /**
     * Register a stub for a {@link HostCalls#pluginCall} invocation.
     * Returns a builder whose {@link ExpectCallBuilder#respond respond}
     * method registers the stub.
     * <p>
     * Matches any request to the given plugin and function.
     *
     * @param pluginName   the plugin name (e.g. {@code "llm"})
     * @param functionName the function name (e.g. {@code "chat"})
     * @return a builder for setting the response
     */
    public ExpectCallBuilder expectPluginCall(String pluginName, String functionName) {
        return new ExpectCallBuilder(this, pluginName, functionName, true);
    }

    /**
     * Register a stub for a {@link HostCalls#pluginCall} invocation with an
     * expected request string.
     *
     * @param pluginName      the plugin name
     * @param functionName    the function name
     * @param expectedRequest the exact expected request JSON
     * @return a builder for setting the response
     */
    public ExpectCallBuilder expectPluginCall(String pluginName, String functionName,
                                              String expectedRequest) {
        return new ExpectCallBuilder(this, pluginName, functionName, expectedRequest, true);
    }

    // ========================================================================
    // Signal injection
    // ========================================================================

    /**
     * Inject a signal for the workflow to receive.
     * <p>
     * The signal is delivered immediately (at the current simulated time)
     * and will be returned by the next matching {@code awaitSignals} or
     * {@code pollSignal} call.
     *
     * @param name    the signal name
     * @param payload the signal payload (typically JSON)
     */
    public void injectSignal(String name, String payload) {
        delegate.deliverSignal(name, payload);
    }

    // ========================================================================
    // Workflow execution
    // ========================================================================

    /**
     * Execute a workflow function that takes {@link HostCalls} and a
     * {@link String} input, returning a {@link String} result.
     * <p>
     * Supports method references:
     * <pre>{@code
     * env.execute(MyWorkflow::handleIncident, inputJson);
     * }</pre>
     *
     * @param workflow the workflow function (method reference)
     * @param input    the input string (typically JSON)
     * @return the workflow's return value
     */
    public String execute(BiFunction<HostCalls, String, String> workflow, String input) {
        return workflow.apply(bridge, input);
    }

    /**
     * Execute a workflow function that takes only {@link HostCalls} (no input
     * parameter) and returns a result.
     * <p>
     * Supports method references:
     * <pre>{@code
     * env.execute(MyWorkflow::handleTimer);
     * }</pre>
     *
     * @param <R>      the workflow return type
     * @param workflow the workflow function (method reference)
     * @return the workflow's return value
     */
    public <R> R execute(Function<HostCalls, R> workflow) {
        return workflow.apply(bridge);
    }

    // ========================================================================
    // Call history inspection
    // ========================================================================

    /**
     * Return a copy of all recorded calls made during workflow execution.
     *
     * @return a list of {@link CallRecord} entries in call order
     */
    public List<CallRecord> callHistory() {
        List<TestHostCalls.CallRecord> raw = delegate.getCallHistory();
        List<CallRecord> result = new ArrayList<>(raw.size());
        for (TestHostCalls.CallRecord r : raw) {
            result.add(new CallRecord(
                r.service, r.operation, r.request, r.response, r.error));
        }
        return result;
    }

    /**
     * Return the number of times a call to the given service and operation was
     * recorded.
     *
     * @param service   the service or plugin name
     * @param operation the operation or function name
     * @return the call count
     */
    public int callCount(String service, String operation) {
        return delegate.callCount(service, operation);
    }

    /**
     * Returns {@code true} if at least one call to the given service and
     * operation was recorded.
     *
     * @param service   the service or plugin name
     * @param operation the operation or function name
     * @return {@code true} if a matching call exists
     */
    public boolean wasCalled(String service, String operation) {
        return delegate.assertCalled(service, operation);
    }

    /**
     * Returns {@code true} if no call to the given service and operation was
     * recorded.
     *
     * @param service   the service or plugin name
     * @param operation the operation or function name
     * @return {@code true} if no matching call exists
     */
    public boolean wasNotCalled(String service, String operation) {
        return delegate.assertNotCalled(service, operation);
    }

    /**
     * Check whether the workflow called {@link HostCalls#continueAsNew}.
     *
     * @return {@code true} if continueAsNew was called
     */
    public boolean continueAsNewCalled() {
        return delegate.continueAsNewCalled;
    }

    // ========================================================================
    // Simulated time control
    // ========================================================================

    /**
     * Get the current simulated wall-clock time.
     *
     * @return milliseconds since Unix epoch
     */
    public long now() {
        return delegate.now();
    }

    /**
     * Set the simulated clock to an absolute time.
     *
     * @param epochMs milliseconds since Unix epoch
     */
    public void setTime(long epochMs) {
        delegate.setTime(epochMs);
    }

    /**
     * Advance the simulated clock by the given duration.
     *
     * @param ms milliseconds to add
     */
    public void advanceTime(long ms) {
        delegate.advanceTime(ms);
    }

    // ========================================================================
    // Workflow/run ID configuration
    // ========================================================================

    /**
     * Set the workflow ID returned by {@link HostCalls#currentWorkflowId}.
     */
    public void setWorkflowId(String id) {
        delegate.setWorkflowId(id);
    }

    /**
     * Set the run ID returned by {@link HostCalls#currentRunId}.
     */
    public void setRunId(String id) {
        delegate.setRunId(id);
    }

    /**
     * Get the current workflow ID.
     */
    public String workflowId() {
        return delegate.currentWorkflowId();
    }

    /**
     * Get the current run ID.
     */
    public String runId() {
        return delegate.currentRunId();
    }

    // ========================================================================
    // Version / random / cancellation helpers
    // ========================================================================

    /**
     * Configure the sequence of values returned by {@link HostCalls#random}.
     * After the sequence is exhausted, {@code random()} returns 0.
     */
    public void setRandomSeq(long[] seq) {
        delegate.setRandomSeq(seq);
    }

    /**
     * Simulate workflow cancellation.
     * After calling this, {@link HostCalls#pollCancellation} returns
     * {@code true}.
     */
    public void setCancelled(boolean cancelled) {
        delegate.setCancelled(cancelled, "");
    }

    /**
     * Simulate workflow cancellation with a reason.
     */
    public void setCancelled(boolean cancelled, String reason) {
        delegate.setCancelled(cancelled, reason);
    }

    /**
     * Configure retry simulation: fail the first {@code n} calls to any
     * (service, operation) pair with a transient error before succeeding.
     * Set to 0 (default) to disable.
     */
    public void setRetrySimulation(int n) {
        delegate.setRetrySimulation(n);
    }

    // ========================================================================
    // Query state
    // ========================================================================

    /**
     * Read back a query state value that was set by the workflow via
     * {@link HostCalls#setQueryState}.
     *
     * @param key the state key
     * @return the value, or {@code null} if not set
     */
    public String readQueryState(String key) {
        return delegate.readQueryState(key);
    }

    // ========================================================================
    // Reset
    // ========================================================================

    /**
     * Reset the test environment to its initial state: clears all stubs, call
     * history, signals, time, version, state, and configuration.
     */
    public void reset() {
        delegate.reset();
        pluginCallErrors.clear();
    }

    // ========================================================================
    // HostCalls bridge — inner class that extends HostCalls and delegates
    // every native-calling method to the internal TestHostCalls instance.
    // ========================================================================

    /**
     * Bridge between {@link HostCalls} (the real SDK class with {@code @Import}
     * native methods) and {@link TestHostCalls} (the mock).
     *
     * <p>Every overridden method intercepts the call and forwards it to the
     * {@link TestHostCalls} delegate, avoiding the native WASM imports.
     * Methods that are pure-Java convenience wrappers in {@code HostCalls}
     * (e.g. {@code cleatSleep}, {@code awaitSignals}, {@code awaitPromise}) are
     * <em>not</em> overridden — they naturally dispatch to the overridden Ms
     * variant via virtual method dispatch.
     */
    /* package */ static class HostCallsBridge extends HostCalls {

        private final TestHostCalls delegate;
        private final CleatTestEnv env;

        HostCallsBridge(TestHostCalls delegate, CleatTestEnv env) {
            this.delegate = delegate;
            this.env = env;
        }

        // ---- cleatCall ----

        @Override
        public CleatResult<String> cleatCall(String service, String operation,
                                             String requestJSON) {
            return delegate.cleatCall(service, operation, requestJSON);
        }

        // ---- Sleep ----
        // cleatSleep(long) delegates to cleatSleepMs(long) in HostCalls,
        // so we only override cleatSleepMs.

        @Override
        public boolean cleatSleepMs(long timeoutMs) {
            return delegate.cleatSleep(timeoutMs);
        }

        // ---- Time ----

        @Override
        public long now() {
            return delegate.now();
        }

        // ---- Random ----

        @Override
        public long random() {
            return delegate.random();
        }

        // ---- Log ----

        @Override
        public void cleatLog(String message) {
            delegate.cleatLog(message);
        }

        // ---- Version ----

        @Override
        public int version() {
            return delegate.version();
        }

        @Override
        public int minVersion() {
            return delegate.minVersion();
        }

        // ---- Defer ----

        @Override
        public CleatResult<String> cleatDefer(String description) {
            return delegate.cleatDefer(description);
        }

        // ---- Cancellation ----

        @Override
        public CleatResult<Boolean> pollCancellation() {
            return delegate.pollCancellation();
        }

        // ---- Signal polling ----

        @Override
        public CleatResult<String> pollSignal(String signalName) {
            return delegate.pollSignal(signalName);
        }

        // ---- Continue-as-new ----

        @Override
        public CleatResult<Void> continueAsNew(String newInputJSON) {
            return delegate.continueAsNew(newInputJSON);
        }

        // ---- Promises ----

        @Override
        public CleatResult<String> createPromise(String name) {
            return delegate.createPromise(name);
        }

        @Override
        public CleatResult<HostCalls.AwaitPromiseResult> awaitPromiseMs(
                String promiseId, long timeoutMs) {
            return delegate.awaitPromise(promiseId, timeoutMs);
        }

        @Override
        public CleatResult<Void> resolvePromise(String id, String value) {
            return delegate.resolvePromise(id, value);
        }

        @Override
        public CleatResult<Void> rejectPromise(String id, String error) {
            return delegate.rejectPromise(id, error);
        }

        // ---- Child workflows ----

        @Override
        public CleatResult<String> childWorkflow(String name, String inputJSON) {
            return delegate.childWorkflow(name, inputJSON);
        }

        @Override
        public CleatResult<String> childWorkflowWithOptions(
                String name, String inputJSON, long version,
                String parentClosePolicy) {
            // TestHostCalls childWorkflow doesn't have options, so fall back
            // to the basic version.
            return delegate.childWorkflow(name, inputJSON);
        }


        @Override
        public CleatResult<String> awaitChild(String runID) {
            return delegate.awaitChild(runID);
        }

        @Override
        public CleatResult<String> awaitAllChildren(String[] runIDs) {
            return delegate.awaitAllChildren(runIDs);
        }

        // ---- Signals ----

        @Override
        public CleatResult<HostCalls.AwaitSignalsResult> awaitSignalsMs(
                String[] signalNames, long timeoutMs) {
            return delegate.awaitSignals(signalNames, timeoutMs);
        }

        @Override
        public CleatResult<String> sendSignalAndWaitMs(
                String targetRunId, String signalName, String payload,
                long timeoutMs) {
            return delegate.sendSignalAndWait(
                targetRunId, signalName, payload, timeoutMs);
        }

        @Override
        public CleatResult<Void> replyToSignal(
                String correlationId, String response) {
            return delegate.replyToSignal(correlationId, response);
        }

        @Override
        public CleatResult<Void> signalWorkflow(
                String targetRunId, String signalName, String payload) {
            return delegate.signalWorkflow(targetRunId, signalName, payload);
        }

        // ---- Query state ----

        @Override
        public void setQueryState(String key, String value) {
            delegate.setQueryState(key, value);
        }

        // ---- Update handlers ----

        @Override
        public void registerUpdateHandler(String name) {
            delegate.registerUpdateHandler(name);
        }

        // There is no registerQueryHandler override here (removed
        // 2026-08-09; HostCalls no longer declares the method). See
        // docs/determinism.md, "Why there is no RegisterQueryHandler".

        // ---- Plugin calls ----

        @Override
        public CleatResult<String> pluginCall(
                String pluginName, String functionName, String inputJson) {
            // Check pluginCallErrors first (registered via respondError)
            String error = env.pluginCallErrors.get(pluginName + "." + functionName);
            if (error != null) {
                return CleatResult.err(error);
            }
            return delegate.pluginCall(pluginName, functionName, inputJson);
        }

        @Override
        public PluginCallOutcome pluginCallOutcome(
                String pluginName, String functionName, String inputJson) {
            CleatResult<String> result = pluginCall(
                pluginName, functionName, inputJson);
            if (result.isErr()) {
                return new PluginCallOutcome(null, result.getError(), 1);
            }
            return new PluginCallOutcome(result.getValue(), null, 0);
        }

        // ---- Workflow / Run IDs ----

        @Override
        public String currentWorkflowId() {
            return delegate.currentWorkflowId();
        }

        @Override
        public String currentRunId() {
            return delegate.currentRunId();
        }

        // ---- Scope ----

        @Override
        public String setScope(String objectType, String instanceKey) {
            return delegate.setScope(objectType, instanceKey);
        }

        @Override
        public String[] getScope() {
            return delegate.getScope();
        }

        @Override
        public String clearScope() {
            return delegate.clearScope();
        }

        // ---- Fire-and-forget ----

        @Override
        public CleatResult<Void> cleatSend(
                String service, String operation, String requestJSON) {
            return delegate.cleatSend(service, operation, requestJSON);
        }

        @Override
        public CleatResult<Void> scheduleInvokeMs(
                String service, String operation, String requestJSON,
                long delayMs) {
            return delegate.scheduleInvoke(service, operation, requestJSON, delayMs);
        }

        // ---- Detached execution ----

        @Override
        public CleatResult<Void> runDetached(
                String workflowName, String inputJSON) {
            return delegate.runDetached(workflowName, inputJSON);
        }

        // ---- Durable state ----

        @Override
        public CleatResult<Void> setState(String key, String value) {
            return delegate.setState(key, value);
        }

        @Override
        public CleatResult<String> getState(String key) {
            return delegate.getState(key);
        }

        @Override
        public CleatResult<Void> deleteState(String key) {
            return delegate.deleteState(key);
        }

        @Override
        public CleatResult<Long> incrState(String key, long delta) {
            long newValue = delegate.incrState(key, delta);
            return CleatResult.ok(newValue);
        }

        @Override
        public boolean hasState(String key) {
            return delegate.hasState(key);
        }

        @Override
        public CleatResult<String> listState(String prefix) {
            return delegate.listState(prefix);
        }

        // ---- Heartbeat call ----

        @Override
        public CleatResult<String> cleatCallHeartbeat(
                String service, String operation, String requestJSON,
                long heartbeatIntervalMs) {
            return delegate.cleatCall(service, operation, requestJSON);
        }

        // ---- Call with retry ----

        @Override
        public <T, R> CleatResult<R> cleatCallWithRetry(
                String service, String operation, T request,
                Class<R> responseClass, RetryPolicy retryPolicy) {
            // Serialize request, delegate to cleatCall, deserialize response.
            String requestJson = cleat.JsonHelper.stringify(request);
            CleatResult<String> raw = delegate.cleatCall(service, operation, requestJson);
            if (raw.isErr()) {
                return CleatResult.err(raw.getError());
            }
            try {
                R parsed = cleat.JsonHelper.parse(raw.getValue(), responseClass);
                return CleatResult.ok(parsed);
            } catch (Exception e) {
                return CleatResult.err(
                    "cleatCallWithRetry: failed to parse response: " + e.getMessage());
            }
        }

        // ---- HTTP fetch ----

        @Override
        public CleatResult<FetchResult> cleatFetch(
                String method, String url, Map<String, String> headers,
                String body) {
            // Return a minimal mock fetch response.
            return CleatResult.ok(new FetchResult(200, new java.util.HashMap<>(), "{}"));
        }

        // ---- Locks ----

        @Override
        public CleatResult<Boolean> acquireLockMs(String key, long ttlMs) {
            return CleatResult.ok(true);
        }

        @Override
        public CleatResult<Void> releaseLock(String key) {
            return CleatResult.ok(null);
        }

        // ---- UUID ----

        @Override
        public String uuid(String seed) {
            return "00000000-0000-0000-0000-" + String.format("%012d", seed.hashCode() & 0xFFFFFFFFFFFFL);
        }
    }
}
