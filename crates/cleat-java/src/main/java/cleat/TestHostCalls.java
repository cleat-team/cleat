package cleat;

import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

/**
 * Mock implementation of the cleat HostCalls API for testing workflows
 * without WASM compilation or a running cleat host.
 *
 * <p>Every host call is recorded for later assertion. Pre-programmed
 * responses are returned when stubs are registered via the {@code register*}
 * methods.
 *
 * <p><strong>Design principle:</strong> Keeps the API simple and explicit.
 * TeaVM limits reflection, so this class avoids dynamic proxies and
 * reflective dispatch.
 *
 * <h2>Test Runner Pattern</h2>
 *
 * <pre>{@code
 * import cleat.TestHostCalls;
 * import cleat.TestHostCalls.CallRecord;
 * import static org.junit.jupiter.api.Assertions.*;
 *
 * class MyWorkflowTest {
 *     &#064;Test
 *     void testPlaceOrder() {
 *         TestHostCalls host = new TestHostCalls();
 *
 *         // Stub external service calls
 *         host.registerCallStub("inventory", "Reserve",
 *             "{\"reservationID\":\"r1\",\"totalCents\":5000}");
 *         host.registerCallStub("payments", "Charge",
 *             "{\"chargeID\":\"c1\"}");
 *
 *         // Run workflow
 *         String input = "{\"userID\":\"u1\",\"items\":[{\"sku\":\"s1\",\"qty\":2}]}";
 *         String result = placeOrderWorkflow(host, input);
 *
 *         // Assert
 *         assertTrue(host.assertCalled("inventory", "Reserve"));
 *         assertTrue(host.assertCalled("payments", "Charge"));
 *         assertFalse(host.assertCalled("shipping", "CreateShipment"));
 *     }
 * }
 * }</pre>
 *
 * <p>Mirrors Go {@code durabletest.TestEnv} at
 * durable/durabletest/durabletest.go and follows the same patterns as the
 * Rust {@code MockHostCalls} and AssemblyScript {@code MockHostCalls}.
 *
 * @see HostCalls
 */
public class TestHostCalls {

    // ========================================================================
    // Internal types
    // ========================================================================

    /**
     * A single recorded call through the test environment.
     */
    public static class CallRecord {
        /** The service name that was called. */
        public final String service;
        /** The operation name that was called. */
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

    /** Internal stub entry for pre-configured call responses. */
    private static class CallStub {
        final String service;
        final String operation;
        final String response;
        final String error;
        final int consumeCount;

        CallStub(String service, String operation, String response,
                 String error, int consumeCount) {
            this.service = service;
            this.operation = operation;
            this.response = response;
            this.error = error;
            this.consumeCount = consumeCount;
        }
    }

    /** Internal record for a pending signal. */
    private static class PendingSignal {
        final String name;
        final String payload;

        PendingSignal(String name, String payload) {
            this.name = name;
            this.payload = payload;
        }
    }

    /** Internal record for a child workflow stub. */
    private static class ChildWorkflowStub {
        final String result;
        final String error;

        ChildWorkflowStub(String result, String error) {
            this.result = result;
            this.error = error;
        }
    }

    /** Internal record for a plugin call stub. */
    private static class PluginCallStub {
        final String pluginName;
        final String functionName;
        final String result;
        final String error;

        PluginCallStub(String pluginName, String functionName,
                       String result, String error) {
            this.pluginName = pluginName;
            this.functionName = functionName;
            this.result = result;
            this.error = error;
        }
    }

    // ========================================================================
    // State
    // ========================================================================

    // Call recording
    private final List<CallRecord> callHistory = new ArrayList<>();

    // Stubs
    private final List<CallStub> callStubs = new ArrayList<>();
    private final Map<String, ChildWorkflowStub> childWorkflowStubs = new HashMap<>();
    private final Map<String, String> childResults = new HashMap<>();
    private final Map<String, String> childErrors = new HashMap<>();
    private final List<PluginCallStub> pluginCallStubs = new ArrayList<>();

    // Signals
    private final List<PendingSignal> pendingSignals = new ArrayList<>();
    private final List<String> sentSignals = new ArrayList<>();

    // Promises
    private final Map<String, String> promises = new HashMap<>(); // promiseID -> status

    // Promise results/errors
    private final Map<String, String> promiseResults = new HashMap<>();
    private final Map<String, String> promiseErrors = new HashMap<>();

    // State
    private final Map<String, String> queryState = new HashMap<>();
    private final Map<String, String> workflowState = new HashMap<>();

    // Scope
    private String scopePrefix = "";

    // Time
    private long nowMs = 1704067200000L; // 2024-01-01T00:00:00Z

    // Version
    private int versionVal = 1;
    private int minVersionVal = 1;

    // Random
    private long[] randomSeq = new long[0];
    private int randomIdx = 0;

    // Counters
    private long deferCounter = 0;
    private long childRunIdCounter = 0;
    private long signalReplyCorrIdCounter = 0;

    // Cancellation
    private boolean cancelled = false;
    private String cancelReason = "";

    // Retry simulation
    private int retrySimCount = 0;
    private final Map<String, Integer> retrySimAttempts = new HashMap<>();

    // Signal reply channels
    private final Map<String, String> signalReplyChannels = new HashMap<>();

    // Metadata
    private String workflowId = "test-workflow";
    private String runId = "test-run-001";
    boolean continueAsNewCalled = false;

    // ========================================================================
    // HostCalls API methods
    // ========================================================================

    /**
     * Make a recorded API call to an external service.
     * Returns a pre-programmed stub response if one is registered.
     */
    public CleatResult<String> cleatCall(String service, String operation, String requestJSON) {
        // Retry simulation
        if (retrySimCount > 0) {
            String key = service + "/" + operation;
            int attempt = retrySimAttempts.getOrDefault(key, 0);
            if (attempt < retrySimCount) {
                retrySimAttempts.put(key, attempt + 1);
                String errMsg = "simulated transient failure for " + key
                    + " (attempt " + (attempt + 1) + "/" + retrySimCount + ")";
                callHistory.add(new CallRecord(service, operation, requestJSON, "", errMsg));
                return CleatResult.err(errMsg);
            }
        }

        // Find matching stub
        for (int i = 0; i < callStubs.size(); i++) {
            CallStub stub = callStubs.get(i);
            if (stub.service.equals(service) && stub.operation.equals(operation)) {
                callStubs.remove(i);
                String resp = stub.response;
                String err = stub.error;
                callHistory.add(new CallRecord(service, operation, requestJSON, resp, err));
                if (err != null) {
                    return CleatResult.err(err);
                }
                return CleatResult.ok(resp);
            }
        }

        // No stub registered
        String errMsg = "no stub registered for " + service + "." + operation;
        callHistory.add(new CallRecord(service, operation, requestJSON, "", errMsg));
        return CleatResult.err(errMsg);
    }

    /**
     * Simulate workflow suspension for a duration.
     * Advances the simulated clock.
     */
    public boolean cleatSleep(long durationMs) {
        nowMs += durationMs;
        return false; // In mock mode, never suspend
    }

    /**
     * Get the current simulated wall-clock time.
     */
    public long now() {
        return nowMs;
    }

    /**
     * Get a deterministic random value from the pre-configured sequence.
     */
    public long random() {
        if (randomIdx < randomSeq.length) {
            return randomSeq[randomIdx++];
        }
        return 0;
    }

    /**
     * Log a message (no-op in test mode).
     */
    public void cleatLog(String message) {
        // No-op for testing
    }

    /**
     * Get the configured workflow definition version.
     */
    public int version() {
        return versionVal;
    }

    /**
     * Get the configured minimum supported version.
     */
    public int minVersion() {
        return minVersionVal;
    }

    /**
     * Register a deferred cleanup action.
     */
    public CleatResult<String> cleatDefer(String description) {
        deferCounter++;
        String deferId = "defer-" + deferCounter;
        return CleatResult.ok(deferId);
    }

    /**
     * Check whether cancellation has been requested.
     */
    public CleatResult<Boolean> pollCancellation() {
        return CleatResult.ok(cancelled);
    }

    /**
     * Poll for a specific pending signal (non-blocking).
     */
    public CleatResult<String> pollSignal(String signalName) {
        for (int i = 0; i < pendingSignals.size(); i++) {
            PendingSignal sig = pendingSignals.get(i);
            if (sig.name.equals(signalName)) {
                pendingSignals.remove(i);
                return CleatResult.ok(sig.payload);
            }
        }
        return CleatResult.err("signal not found: " + signalName);
    }

    /**
     * Simulate continue-as-new.
     */
    public CleatResult<Void> continueAsNew(String newInputJSON) {
        this.continueAsNewCalled = true;
        return CleatResult.ok(null);
    }

    /**
     * Start a child workflow instance.
     */
    public CleatResult<String> childWorkflow(String name, String inputJSON) {
        childRunIdCounter++;
        String runId = "child-" + name + "-" + childRunIdCounter;

        // Check for a child workflow stub
        ChildWorkflowStub stub = childWorkflowStubs.get(name);
        if (stub != null) {
            if (stub.error != null) {
                childErrors.put(runId, stub.error);
            } else {
                childResults.put(runId, stub.result);
            }
        } else {
            // Default: auto-complete with empty result
            childResults.put(runId, "{\"status\":\"completed\"}");
        }

        return CleatResult.ok(runId);
    }

    /**
     * Wait for a child workflow to complete.
     */
    public CleatResult<String> awaitChild(String runID) {
        String err = childErrors.get(runID);
        if (err != null) {
            return CleatResult.err(err);
        }
        String result = childResults.get(runID);
        if (result != null) {
            return CleatResult.ok(result);
        }
        return CleatResult.ok("{\"status\":\"completed\"}");
    }

    /**
     * Wait for one or more external signals with a timeout.
     */
    public CleatResult<HostCalls.AwaitSignalsResult> awaitSignals(
            String[] signalNames, long timeoutMs) {
        for (int i = 0; i < pendingSignals.size(); i++) {
            PendingSignal sig = pendingSignals.get(i);
            if (contains(signalNames, sig.name)) {
                pendingSignals.remove(i);
                return CleatResult.ok(
                    new HostCalls.AwaitSignalsResult(sig.name, sig.payload, false));
            }
        }

        // No matching signal — return timeout
        if (timeoutMs > 0) {
            nowMs += timeoutMs;
        }
        return CleatResult.ok(
            new HostCalls.AwaitSignalsResult("", "", true));
    }

    /**
     * Set a key-value pair in query state.
     */
    public void setQueryState(String key, String value) {
        queryState.put(scopedKey(key), value);
    }

    /**
     * Get a key-value pair from query state.
     */
    public CleatResult<String> getQueryState(String key) {
        String val = queryState.get(key);
        if (val != null) {
            return CleatResult.ok(val);
        }
        return CleatResult.err("query state key not found: " + key);
    }

    /**
     * Create a new cleat promise.
     */
    public CleatResult<String> createPromise(String name) {
        deferCounter++;
        String promiseId = "prom-" + name + "-" + deferCounter;
        promises.put(promiseId, "pending");
        return CleatResult.ok(promiseId);
    }

    /**
     * Wait for a cleat promise to resolve.
     */
    public CleatResult<HostCalls.AwaitPromiseResult> awaitPromise(
            String id, long timeoutMs) {
        String status = promises.get(id);
        if (status == null) {
            return CleatResult.err("promise not found: " + id);
        }

        if ("resolved".equals(status)) {
            String val = promiseResults.getOrDefault(id, "");
            return CleatResult.ok(new HostCalls.AwaitPromiseResult(val, false));
        }

        if ("rejected".equals(status)) {
            String err = promiseErrors.getOrDefault(id, "rejected");
            return CleatResult.err(err);
        }

        // Pending — advance time to simulate timeout
        nowMs += timeoutMs;
        return CleatResult.ok(new HostCalls.AwaitPromiseResult("", true));
    }

    /**
     * Register an update handler.
     */
    public void registerUpdateHandler(String name) {
        // No-op in mock mode — we just acknowledge it was called
    }

    /**
     * Call a plugin function via the host runtime.
     */
    public CleatResult<String> pluginCall(String pluginName, String functionName, String inputJson) {
        for (PluginCallStub stub : pluginCallStubs) {
            if (stub.pluginName.equals(pluginName)
                    && stub.functionName.equals(functionName)) {
                if (stub.error != null) {
                    return CleatResult.err("plugin_call(" + pluginName + "." + functionName + ") failed: " + stub.error);
                }
                return CleatResult.ok(stub.result);
            }
        }
        return CleatResult.err(
            "plugin_call failed: no stub registered for "
            + pluginName + "." + functionName);
    }

    /**
     * Get the current workflow ID.
     */
    public String currentWorkflowId() {
        return workflowId;
    }

    /**
     * Set the state key prefix for virtual object instances.
     */
    public String setScope(String objectType, String instanceKey) {
        String prev = scopePrefix;
        scopePrefix = (objectType != null && !objectType.isEmpty()
            && instanceKey != null && !instanceKey.isEmpty())
            ? "vo:" + objectType + ":" + instanceKey + ":"
            : "";
        return prev;
    }

    /**
     * Get the current virtual object scope.
     */
    public String[] getScope() {
        if (scopePrefix.isEmpty()) {
            return new String[]{"", ""};
        }
        String trimmed = scopePrefix.substring(0, scopePrefix.length() - 1);
        String[] parts = trimmed.split(":", 3);
        if (parts.length == 3 && "vo".equals(parts[0])) {
            return new String[]{parts[1], parts[2]};
        }
        return new String[]{"", ""};
    }

    /**
     * Remove the current scope.
     */
    public String clearScope() {
        String prev = scopePrefix;
        scopePrefix = "";
        return prev;
    }

    /**
     * Resolve a promise with a value.
     */
    public CleatResult<Void> resolvePromise(String id, String value) {
        if (promises.containsKey(id)) {
            promises.put(id, "resolved");
            promiseResults.put(id, value);
            return CleatResult.ok(null);
        }
        return CleatResult.err("promise not found: " + id);
    }

    /**
     * Reject a promise with an error.
     */
    public CleatResult<Void> rejectPromise(String id, String error) {
        if (promises.containsKey(id)) {
            promises.put(id, "rejected");
            promiseErrors.put(id, error);
            return CleatResult.ok(null);
        }
        return CleatResult.err("promise not found: " + id);
    }

    /**
     * Fire-and-forget cleat call.
     */
    public CleatResult<Void> cleatSend(String service, String operation, String requestJSON) {
        callHistory.add(new CallRecord(service, operation, requestJSON, "", null));
        return CleatResult.ok(null);
    }

    /**
     * Schedule a delayed invocation.
     */
    public CleatResult<Void> scheduleInvoke(String service, String operation,
                                              String requestJSON, long delayMs) {
        return CleatResult.ok(null);
    }

    /**
     * Register a query handler.
     */
    public void registerQueryHandler(String name) {
        // No-op in mock mode
    }

    /**
     * Run a workflow in detached mode.
     */
    public CleatResult<Void> runDetached(String workflowName, String inputJSON) {
        return CleatResult.ok(null);
    }

    /**
     * Set a state value.
     */
    public CleatResult<Void> setState(String key, String value) {
        workflowState.put(scopedKey(key), value);
        return CleatResult.ok(null);
    }

    /**
     * Get a state value.
     */
    public CleatResult<String> getState(String key) {
        String val = workflowState.get(scopedKey(key));
        if (val != null) {
            return CleatResult.ok(val);
        }
        return CleatResult.err("no such key: " + key);
    }

    /**
     * Delete a state key.
     */
    public CleatResult<Void> deleteState(String key) {
        workflowState.remove(scopedKey(key));
        return CleatResult.ok(null);
    }

    /**
     * Atomically increment a numeric state value.
     */
    public long incrState(String key, long delta) {
        String scoped = scopedKey(key);
        long current = 0;
        String existing = workflowState.get(scoped);
        if (existing != null) {
            try {
                current = Long.parseLong(existing);
            } catch (NumberFormatException e) {
                System.err.println("Warning: non-numeric state value for key '" + key + "': " + existing + ". Resetting to 0.");
            }
        }
        current += delta;
        workflowState.put(scoped, String.valueOf(current));
        return current;
    }

    /**
     * Check if a state key exists.
     * Uses the raw (unscoped) key, so that scoped state is isolated
     * from unscoped lookups.  This allows tests to verify scope isolation:
     * after {@link #setState} with a scope active, {@code hasState}
     * with the same raw key returns {@code false} because the stored
     * key is prefixed.
     */
    public boolean hasState(String key) {
        return workflowState.containsKey(key);
    }

    /**
     * List state keys matching a prefix.
     */
    public CleatResult<String> listState(String prefix) {
        String scoped = scopedKey(prefix);
        StringBuilder sb = new StringBuilder("[");
        boolean first = true;
        for (String k : workflowState.keySet()) {
            if (k.startsWith(scoped)) {
                if (!first) {
                    sb.append(",");
                }
                sb.append("\"").append(k).append("\"");
                first = false;
            }
        }
        sb.append("]");
        return CleatResult.ok(sb.toString());
    }

    /**
     * Await all children workflows.
     */
    public CleatResult<String> awaitAllChildren(String[] runIDs) {
        StringBuilder sb = new StringBuilder("[");
        boolean first = true;
        for (String runId : runIDs) {
            String childResult = childResults.get(runId);
            if (!first) {
                sb.append(",");
            }
            sb.append("{\"runId\":\"").append(runId).append("\"");
            sb.append(",\"result\":");
            if (childResult != null) {
                sb.append(childResult);
            } else {
                sb.append("\"\"");
            }
            sb.append("}");
            first = false;
        }
        sb.append("]");
        return CleatResult.ok(sb.toString());
    }

    /**
     * Get the current run ID.
     */
    public String currentRunId() {
        return runId;
    }

    /**
     * Send a signal to a target workflow and wait for a response.
     */
    public CleatResult<String> sendSignalAndWait(
            String targetRunId, String signalName, String payload, long timeoutMs) {
        signalReplyCorrIdCounter++;
        String correlationId = "corr-" + targetRunId + "-"
            + signalName + "-" + signalReplyCorrIdCounter;

        // Register a reply channel
        signalReplyChannels.put(correlationId, "__pending__");

        // Send the signal
        signalWorkflow(targetRunId, signalName, payload);

        // Check if reply already arrived
        String reply = signalReplyChannels.get(correlationId);
        if (!"__pending__".equals(reply)) {
            signalReplyChannels.remove(correlationId);
            return CleatResult.ok(reply);
        }

        // Simulate timeout
        nowMs += timeoutMs;
        return CleatResult.err("SendSignalAndWait(target=" + targetRunId + ", signal=" + signalName + ") timed out after " + timeoutMs + "ms");
    }

    /**
     * Reply to a signal from within a handler.
     */
    public CleatResult<Void> replyToSignal(String correlationId, String response) {
        if (signalReplyChannels.containsKey(correlationId)) {
            signalReplyChannels.put(correlationId, response);
            return CleatResult.ok(null);
        }
        return CleatResult.err("no pending signal for correlation ID: " + correlationId);
    }

    /**
     * Send a signal to a target workflow (fire-and-forget).
     */
    public CleatResult<Void> signalWorkflow(
            String targetRunId, String signalName, String payload) {
        sentSignals.add(targetRunId + ":" + signalName);
        pendingSignals.add(new PendingSignal(signalName, payload));
        return CleatResult.ok(null);
    }

    // ========================================================================
    // Private helpers
    // ========================================================================

    private String scopedKey(String key) {
        return scopePrefix.isEmpty() ? key : scopePrefix + key;
    }

    private boolean contains(String[] arr, String value) {
        for (String s : arr) {
            if (s.equals(value)) {
                return true;
            }
        }
        return false;
    }

    // ========================================================================
    // Mock-specific configuration methods
    // ========================================================================

    /**
     * Register a pre-programmed response for a call to service.operation.
     */
    public void registerCallStub(String service, String operation, String response) {
        callStubs.add(new CallStub(service, operation, response, null, 1));
    }

    /**
     * Register a pre-programmed error for a call to service.operation.
     */
    public void registerCallError(String service, String operation, String error) {
        callStubs.add(new CallStub(service, operation, "", error, 1));
    }

    /**
     * Register a stub for a child workflow with the given name.
     */
    public void registerChildWorkflowStub(String name, String result) {
        childWorkflowStubs.put(name, new ChildWorkflowStub(result, null));
    }

    /**
     * Pre-set the result that a child workflow run will return.
     */
    public void registerChildResult(String runId, String result) {
        childResults.put(runId, result);
    }

    /**
     * Register a plugin call stub.
     */
    public void registerPluginCallStub(String pluginName, String functionName, String result) {
        pluginCallStubs.add(new PluginCallStub(pluginName, functionName, result, null));
    }

    /**
     * Deliver a signal immediately.
     */
    public void deliverSignal(String name, String payload) {
        pendingSignals.add(new PendingSignal(name, payload));
    }

    /**
     * Configure the random value sequence.
     */
    public void setRandomSeq(long[] seq) {
        this.randomSeq = seq;
        this.randomIdx = 0;
    }

    /**
     * Set the simulated clock time.
     */
    public void setTime(long ms) {
        this.nowMs = ms;
    }

    /**
     * Advance the simulated clock.
     */
    public void advanceTime(long ms) {
        this.nowMs += ms;
    }

    /**
     * Set the workflow version.
     */
    public void setVersion(int v) {
        this.versionVal = v;
    }

    /**
     * Set the minimum workflow version.
     */
    public void setMinVersion(int v) {
        this.minVersionVal = v;
    }

    /**
     * Configure cancellation simulation.
     */
    public void setCancelled(boolean cancelled, String reason) {
        this.cancelled = cancelled;
        this.cancelReason = reason;
    }

    /**
     * Configure retry simulation: fail the first n calls per (service, operation).
     */
    public void setRetrySimulation(int n) {
        this.retrySimCount = n;
        this.retrySimAttempts.clear();
    }

    /**
     * Set the workflow ID.
     */
    public void setWorkflowId(String id) {
        this.workflowId = id;
    }

    /**
     * Set the run ID.
     */
    public void setRunId(String id) {
        this.runId = id;
    }

    // ========================================================================
    // Query helpers
    // ========================================================================

    /**
     * Read a query state value set by the workflow via setQueryState.
     */
    public String readQueryState(String key) {
        return queryState.get(key);
    }

    // ========================================================================
    // Assertion helpers
    // ========================================================================

    /**
     * Return the number of times a call to the given service+operation was recorded.
     */
    public int callCount(String service, String operation) {
        int count = 0;
        for (CallRecord rec : callHistory) {
            if (rec.service.equals(service) && rec.operation.equals(operation)) {
                count++;
            }
        }
        return count;
    }

    /**
     * Assert that a call to the given service+operation was made.
     */
    public boolean assertCalled(String service, String operation) {
        return callCount(service, operation) > 0;
    }

    /**
     * Assert that a call to the given service+operation was NOT made.
     */
    public boolean assertNotCalled(String service, String operation) {
        return callCount(service, operation) == 0;
    }

    /**
     * Assert that a call was made exactly n times.
     */
    public boolean assertCallCount(String service, String operation, int expected) {
        return callCount(service, operation) == expected;
    }

    /**
     * Get a copy of the full call history.
     */
    public List<CallRecord> getCallHistory() {
        return new ArrayList<>(callHistory);
    }

    /**
     * Reset the entire test environment to its initial state.
     */
    public void reset() {
        callStubs.clear();
        callHistory.clear();
        pendingSignals.clear();
        queryState.clear();
        workflowState.clear();
        randomSeq = new long[0];
        randomIdx = 0;
        deferCounter = 0;
        childWorkflowStubs.clear();
        childResults.clear();
        childErrors.clear();
        pluginCallStubs.clear();
        promises.clear();
        promiseResults.clear();
        promiseErrors.clear();
        signalReplyChannels.clear();
        signalReplyCorrIdCounter = 0;
        sentSignals.clear();
        cancelled = false;
        cancelReason = "";
        retrySimCount = 0;
        retrySimAttempts.clear();
        continueAsNewCalled = false;
        nowMs = 1704067200000L;
        versionVal = 1;
        minVersionVal = 1;
        childRunIdCounter = 0;
        workflowId = "test-workflow";
        runId = "test-run-001";
        scopePrefix = "";
    }
}
