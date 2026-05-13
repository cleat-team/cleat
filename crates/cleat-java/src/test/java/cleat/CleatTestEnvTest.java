package cleat;

import static org.junit.jupiter.api.Assertions.*;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

import java.util.List;

/**
 * Comprehensive unit tests for {@link CleatTestEnv}.
 *
 * @see CleatTestEnv
 */
class CleatTestEnvTest {

    private CleatTestEnv env;

    @BeforeEach
    void setUp() {
        env = new CleatTestEnv();
    }

    // ======================================================================
    // 1. Basic cleatCall stub and execution
    // ======================================================================

    /** A minimal workflow that calls a single remote service. */
    static String singleCallWorkflow(HostCalls h, String input) {
        CleatResult<String> result = h.cleatCall("orders", "Create", input);
        if (result.isErr()) {
            return "error: " + result.getError();
        }
        return "ok:" + result.getValue();
    }

    @Test
    void testCleatCallWithStubReturnsResponse() {
        // WHAT: Verify expectCall + respond returns the stubbed response through execute
        // WHY: The core purpose of CleatTestEnv is to stub service calls and run workflows

        env.expectCall("orders", "Create")
            .respond("{\"orderID\":\"ord-1\"}");

        String result = env.execute(CleatTestEnvTest::singleCallWorkflow,
            "{\"item\":\"book\"}");

        assertEquals("ok:{\"orderID\":\"ord-1\"}", result,
            "Expected workflow result to include the stubbed response");
    }

    // ======================================================================
    // 2. pluginCall stub and execution
    // ======================================================================

    /** A workflow that calls an LLM plugin. */
    static String llmWorkflow(HostCalls h, String input) {
        CleatResult<String> result = h.pluginCall("llm", "chat", input);
        if (result.isErr()) {
            return "error: " + result.getError();
        }
        return result.getValue();
    }

    @Test
    void testPluginCallWithStubReturnsResponse() {
        // WHAT: Verify expectPluginCall + respond returns the stubbed plugin response
        // WHY: Plugin calls (non-durable, non-recorded) are a different code path from cleatCall

        env.expectPluginCall("llm", "chat")
            .respond("{\"severity\":\"critical\"}");

        String result = env.execute(CleatTestEnvTest::llmWorkflow,
            "{\"prompt\":\"assess incident\"}");

        assertEquals("{\"severity\":\"critical\"}", result,
            "Expected plugin call to return the stubbed LLM response");
    }

    // ======================================================================
    // 3. Missing stub returns error
    // ======================================================================

    @Test
    void testMissingCleatCallStubReturnsError() {
        // WHAT: Verify a cleatCall with no matching stub causes the workflow to get an error
        // WHY: Missing stubs should be detectable to prevent silent test failures

        String result = env.execute(CleatTestEnvTest::singleCallWorkflow, "{}");

        assertTrue(result.startsWith("error:"),
            "Expected error result when no stub is registered, but got: [" + result + "]");
        assertTrue(result.contains("no stub registered"),
            "Expected error to mention 'no stub registered', but got: [" + result + "]");
    }

    // ======================================================================
    // 4. Multiple stubs consumed in order
    // ======================================================================

    /** A workflow that makes two sequential calls. */
    static String twoCallWorkflow(HostCalls h, String input) {
        CleatResult<String> first = h.cleatCall("svc", "op1", input);
        CleatResult<String> second = h.cleatCall("svc", "op2", first.getValue());
        if (second.isErr()) {
            return "err:" + second.getError();
        }
        return second.getValue();
    }

    @Test
    void testMultipleStubsConsumedInOrder() {
        // WHAT: Verify stubs are consumed in FIFO order, matching sequential calls
        // WHY: Workflows may call multiple services sequentially; each needs its own stub

        env.expectCall("svc", "op1").respond("first-result");
        env.expectCall("svc", "op2").respond("second-result");

        String result = env.execute(CleatTestEnvTest::twoCallWorkflow, "input");

        assertEquals("second-result", result,
            "Expected second call to return its own stub response");
    }

    // ======================================================================
    // 5. Call history recording
    // ======================================================================

    @Test
    void testCallHistoryRecordsCalls() {
        // WHAT: Verify callHistory() contains entries with correct service/operation/request
        // WHY: Post-execution call inspection is essential for verifying workflow behavior

        env.expectCall("svc", "op1").respond("{\"id\":\"r1\"}");
        env.expectCall("svc", "op2").respond("{\"id\":\"c1\"}");

        env.execute(CleatTestEnvTest::twoCallWorkflow, "input");

        List<CleatTestEnv.CallRecord> history = env.callHistory();

        assertNotNull(history, "Call history must not be null");
        assertEquals(2, history.size(), "Expected 2 calls in history");

        assertEquals("svc", history.get(0).service,
            "First call should be to 'svc'");
        assertEquals("op1", history.get(0).operation,
            "First call operation should be 'op1'");

        assertEquals("svc", history.get(1).service,
            "Second call should be to 'svc'");
        assertEquals("op2", history.get(1).operation,
            "Second call operation should be 'op2'");
    }

    // ======================================================================
    // 6. wasCalled / wasNotCalled
    // ======================================================================

    @Test
    void testWasCalledReturnsTrueForInvokedService() {
        env.expectCall("orders", "Create").respond("ok");
        env.execute(CleatTestEnvTest::singleCallWorkflow, "req");

        assertTrue(env.wasCalled("orders", "Create"),
            "wasCalled should return true for a service that was called");
    }

    @Test
    void testWasNotCalledReturnsTrueForUninvokedService() {
        env.execute(CleatTestEnvTest::singleCallWorkflow, "req");

        assertTrue(env.wasNotCalled("payments", "Charge"),
            "wasNotCalled should return true for a service never called");
    }

    @Test
    void testWasNotCalledReturnsFalseAfterCall() {
        env.expectCall("orders", "Create").respond("ok");
        env.execute(CleatTestEnvTest::singleCallWorkflow, "req");

        assertFalse(env.wasNotCalled("orders", "Create"),
            "wasNotCalled should return false after a call was made");
    }

    // ======================================================================
    // 7. callCount
    // ======================================================================

    @Test
    void testCallCountReturnsZeroForUncalledService() {
        assertEquals(0, env.callCount("nonexistent", "op"),
            "callCount should return 0 for a service that was never called");
    }

    @Test
    void testCallCountReturnsCorrectNumber() {
        env.expectCall("svc", "op1").respond("r1");
        env.expectCall("svc", "op2").respond("r2");
        env.execute(CleatTestEnvTest::twoCallWorkflow, "x");

        // twoCallWorkflow makes 1 call to svc.op1 and 1 call to svc.op2
        assertEquals(1, env.callCount("svc", "op1"),
            "Expected 1 call to svc.op1");
        assertEquals(1, env.callCount("svc", "op2"),
            "Expected 1 call to svc.op2");
    }

    // ======================================================================
    // 8. Signal injection
    // ======================================================================

    /** A workflow that awaits a signal. */
    static String signalWorkflow(HostCalls h, String input) {
        CleatResult<HostCalls.AwaitSignalsResult> result =
            h.awaitSignals(new String[]{"payment_received"}, 5000);
        if (result.isOk() && !result.getValue().timedOut) {
            return "signal:" + result.getValue().signalName
                + " payload:" + result.getValue().payload;
        }
        return "timeout";
    }

    @Test
    void testInjectSignalDeliversToAwaitSignals() {
        // WHAT: Verify injectSignal makes a signal available to awaitSignals
        // WHY: Signal-based coordination is a key workflow pattern that must be testable

        env.injectSignal("payment_received", "{\"amount\":5000}");

        String result = env.execute(CleatTestEnvTest::signalWorkflow, "{}");

        assertTrue(result.startsWith("signal:"),
            "Expected workflow to receive the injected signal, but got: [" + result + "]");
        assertTrue(result.contains("payment_received"),
            "Expected signal name in result, but got: [" + result + "]");
    }

    @Test
    void testInjectSignalWorksWithPollSignal() {
        // WHAT: Verify injectSignal also makes signals available via pollSignal
        // WHY: Some workflows use pollSignal (non-blocking) instead of awaitSignals

        env.injectSignal("alarm", "{\"level\":\"critical\"}");

        String result = env.execute(h -> {
            CleatResult<String> signalResult = h.pollSignal("alarm");
            if (signalResult.isOk()) {
                return "found:" + signalResult.getValue();
            }
            return "not-found";
        });

        assertEquals("found:{\"level\":\"critical\"}", result,
            "Expected pollSignal to find the injected signal");
    }

    // ======================================================================
    // 9. Time control
    // ======================================================================

    @Test
    void testDefaultTimeIs20240101() {
        assertEquals(1704067200000L, env.now(),
            "Default simulated time should be 2024-01-01T00:00:00Z");
    }

    @Test
    void testSetTimeChangesClock() {
        env.setTime(1000000L);
        assertEquals(1000000L, env.now(),
            "setTime should change the simulated clock");
    }

    @Test
    void testAdvanceTimeIncrementsClock() {
        long before = env.now();
        env.advanceTime(5000L);
        assertEquals(before + 5000, env.now(),
            "advanceTime should increment the simulated clock by the given ms");
    }

    @Test
    void testCleatSleepAdvancesClock() {
        env.execute(h -> {
            h.cleatSleep(3); // 3 seconds
            return "";
        });

        assertEquals(1704067200000L + 3000, env.now(),
            "cleatSleep should advance the simulated clock");
    }

    // ======================================================================
    // 10. Workflow / Run ID configuration
    // ======================================================================

    @Test
    void testDefaultWorkflowId() {
        assertEquals("test-workflow", env.workflowId(),
            "Default workflow ID should be 'test-workflow'");
    }

    @Test
    void testSetWorkflowId() {
        env.setWorkflowId("my-workflow");
        String result = env.execute(h -> h.currentWorkflowId());
        assertEquals("my-workflow", result,
            "currentWorkflowId should return the configured workflow ID");
    }

    @Test
    void testDefaultRunId() {
        assertEquals("test-run-001", env.runId(),
            "Default run ID should be 'test-run-001'");
    }

    @Test
    void testSetRunId() {
        env.setRunId("my-run");
        String result = env.execute(h -> h.currentRunId());
        assertEquals("my-run", result,
            "currentRunId should return the configured run ID");
    }

    // ======================================================================
    // 11. Random values
    // ======================================================================

    @Test
    void testRandomReturnsConfiguredSequence() {
        env.setRandomSeq(new long[]{42L, 99L, 100L});

        String result = env.execute(h -> {
            long r1 = h.random();
            long r2 = h.random();
            return r1 + "," + r2;
        });

        assertEquals("42,99", result,
            "random() should return the configured sequence");
    }

    // ======================================================================
    // 12. Version
    // ======================================================================

    @Test
    void testVersionDefaults() {
        String result = env.execute(h -> h.version() + "," + h.minVersion());
        assertEquals("1,1", result,
            "Default version and minVersion should be 1,1");
    }

    // ======================================================================
    // 13. State operations via bridge
    // ======================================================================

    @Test
    void testStateOperations() {
        env.execute(h -> {
            h.setState("key1", "val1");
            return "";
        });

        String result = env.execute(h -> {
            CleatResult<String> getResult = h.getState("key1");
            return getResult.isOk() ? getResult.getValue() : "error";
        });

        assertEquals("val1", result,
            "getState should return the value set by setState");
    }

    // ======================================================================
    // 14. Promise operations
    // ======================================================================

    @Test
    void testPromiseLifecycle() {
        String result = env.execute(h -> {
            CleatResult<String> createResult = h.createPromise("test-prom");
            if (createResult.isErr()) {
                return "create-error";
            }
            String promiseId = createResult.getValue();

            CleatResult<Void> resolveResult = h.resolvePromise(promiseId, "prom-value");
            if (resolveResult.isErr()) {
                return "resolve-error";
            }

            CleatResult<HostCalls.AwaitPromiseResult> awaitResult =
                h.awaitPromise(promiseId, 5000);
            if (awaitResult.isOk()) {
                return awaitResult.getValue().result;
            }
            return "await-error:" + awaitResult.getError();
        });

        assertEquals("prom-value", result,
            "awaitPromise should return the resolved value");
    }

    // ======================================================================
    // 15. Child workflow
    // ======================================================================

    @Test
    void testChildWorkflow() {
        String result = env.execute(h -> {
            CleatResult<String> childResult = h.childWorkflow("myChild", "{}");
            if (childResult.isErr()) {
                return "child-error:" + childResult.getError();
            }
            CleatResult<String> awaitResult = h.awaitChild(childResult.getValue());
            return awaitResult.isOk() ? awaitResult.getValue() : "await-error";
        });

        assertTrue(result.contains("completed"),
            "Default child workflow result should contain 'completed', but got: ["
            + result + "]");
    }

    // ======================================================================
    // 16. Query state
    // ======================================================================

    @Test
    void testQueryState() {
        env.execute(h -> {
            h.setQueryState("status", "active");
            return "";
        });

        assertEquals("active", env.readQueryState("status"),
            "readQueryState should return the value set by setQueryState");
    }

    // ======================================================================
    // 17. Defer
    // ======================================================================

    @Test
    void testDefer() {
        String result = env.execute(h -> {
            CleatResult<String> deferResult = h.cleatDefer("cleanup");
            return deferResult.isOk() ? deferResult.getValue() : "error";
        });

        assertTrue(result.startsWith("defer-"),
            "cleatDefer should return a defer ID starting with 'defer-', but got: ["
            + result + "]");
    }

    // ======================================================================
    // 18. Continue-as-new
    // ======================================================================

    @Test
    void testContinueAsNew() {
        env.execute(h -> {
            h.continueAsNew("{\"restart\":true}");
            return "";
        });

        assertTrue(env.continueAsNewCalled(),
            "continueAsNewCalled should return true after continueAsNew was invoked");
    }

    // ======================================================================
    // 19. Scope operations
    // ======================================================================

    @Test
    void testScopeOperations() {
        String result = env.execute(h -> {
            String prev = h.setScope("Order", "ord-123");
            String[] scope = h.getScope();
            String cleared = h.clearScope();
            return prev + "|" + scope[0] + "|" + scope[1] + "|" + cleared;
        });

        assertEquals("|Order|ord-123|vo:Order:ord-123:", result,
            "Scope operations should work correctly through the bridge");
    }

    // ======================================================================
    // 20. Cancellation
    // ======================================================================

    @Test
    void testCancellation() {
        env.setCancelled(true, "workflow timeout");

        Boolean cancelled = env.execute(h -> h.pollCancellation().getValue());

        assertTrue(cancelled,
            "pollCancellation should return true after setCancelled(true)");
    }

    // ======================================================================
    // 21. Retry simulation
    // ======================================================================

    @Test
    void testRetrySimulation() {
        env.setRetrySimulation(1);
        env.expectCall("svc", "op").respond("success");

        String result = env.execute(CleatTestEnvTest::singleCallWorkflow, "req");

        // First call fails due to retry simulation, but there's no retry
        // logic in singleCallWorkflow — so it returns the error.
        assertTrue(result.startsWith("error:"),
            "With retrySimulation=1 and no retry logic, the call should fail");
    }

    // ======================================================================
    // 22. Reset
    // ======================================================================

    @Test
    void testReset() {
        // State the env
        env.expectCall("svc", "op").respond("resp");
        env.execute(CleatTestEnvTest::singleCallWorkflow, "req");
        env.setTime(9999999L);
        env.setWorkflowId("custom-id");
        env.injectSignal("sig", "{}");

        // Reset
        env.reset();

        // Verify clean state
        assertEquals(1704067200000L, env.now(),
            "Time should reset to default after reset");
        assertEquals("test-workflow", env.workflowId(),
            "Workflow ID should reset to default after reset");
        assertEquals(0, env.callHistory().size(),
            "Call history should be empty after reset");
        assertEquals(0, env.callCount("svc", "op"),
            "Call count should be 0 after reset");

        // Calls without stubs should produce errors
        String result = env.execute(CleatTestEnvTest::singleCallWorkflow, "req");
        assertTrue(result.startsWith("error:"),
            "Calls should fail after reset since stubs were cleared");
    }

    // ======================================================================
    // 23. execute with no-input workflow
    // ======================================================================

    static String noInputWorkflow(HostCalls h) {
        return "no-input-result";
    }

    @Test
    void testExecuteWithNoInput() {
        String result = env.execute(CleatTestEnvTest::noInputWorkflow);
        assertEquals("no-input-result", result,
            "Execute with no-input workflow should work");
    }

    // ======================================================================
    // 24. log does not throw
    // ======================================================================

    @Test
    void testCleatLogDoesNotThrow() {
        env.execute(h -> {
            h.cleatLog("test message");
            return "";
        });
        // No assertion — verifying no exception
    }

    // ======================================================================
    // 25. typed child workflow
    // ======================================================================

    @Test
    void testTypedChildWorkflow() {
        String result = env.execute(h -> {
            CleatResult<String> childResult = h.childWorkflowTyped("child", "{}");
            if (childResult.isErr()) {
                return "err:" + childResult.getError();
            }
            CleatResult<String> awaitResult = h.awaitChildTyped(childResult.getValue(), String.class);
            return awaitResult.isOk() ? "ok:" + awaitResult.getValue() : "err:" + awaitResult.getError();
        });

        assertTrue(result.contains("completed"),
            "Typed child workflow should return a result containing 'completed'");
    }

    // ======================================================================
    // 26. pluginCallOutcome
    // ======================================================================

    @Test
    void testPluginCallOutcome() {
        env.expectPluginCall("llm", "chat").respond("{\"response\":\"hello\"}");

        String result = env.execute(h -> {
            HostCalls.PluginCallOutcome outcome = h.pluginCallOutcome("llm", "chat", "{}");
            if (outcome.isError()) {
                return "error:" + outcome.error;
            }
            return "ok:" + outcome.response;
        });

        assertEquals("ok:{\"response\":\"hello\"}", result,
            "pluginCallOutcome should return the stubbed response");
    }

    // ======================================================================
    // 27. sendSignalAndWait / replyToSignal
    // ======================================================================

    @Test
    void testSendSignalAndWaitTimesOut() {
        String result = env.execute(h -> {
            CleatResult<String> swResult = h.sendSignalAndWait(
                "target-1", "ask", "{}", 1);
            if (swResult.isErr()) {
                return "timeout:" + swResult.getError();
            }
            return "response:" + swResult.getValue();
        });

        assertTrue(result.startsWith("timeout:"),
            "sendSignalAndWait should time out when no reply is sent, but got: ["
            + result + "]");
    }

    // ======================================================================
    // 28. Fire-and-forget (cleatSend)
    // ======================================================================

    @Test
    void testCleatSendRecordsCall() {
        env.execute(h -> {
            h.cleatSend("notifications", "alert", "{}");
            return "";
        });

        assertTrue(env.wasCalled("notifications", "alert"),
            "cleatSend should be recorded in call history");
    }

    // ======================================================================
    // 29. RunDetached
    // ======================================================================

    @Test
    void testRunDetached() {
        env.execute(h -> {
            CleatResult<Void> result = h.runDetached("cleanup", "{}");
            assertTrue(result.isOk(), "runDetached should succeed");
            return "";
        });
        // No crash = success
    }

    // ======================================================================
    // 30. Register handlers
    // ======================================================================

    @Test
    void testRegisterHandlers() {
        env.execute(h -> {
            h.registerUpdateHandler("myUpdate");
            h.registerQueryHandler("myQuery");
            return "";
        });
        // No crash = success
    }

    // ======================================================================
    // 31. Durable state (setState / getState / deleteState / hasState)
    // ======================================================================

    @Test
    void testDurableStateCRUD() {
        env.execute(h -> {
            h.setState("key", "value");
            return "";
        });

        String result = env.execute(h -> {
            boolean has = h.hasState("key");
            CleatResult<String> getResult = h.getState("key");
            h.deleteState("key");
            boolean hasAfterDelete = h.hasState("key");
            return has + "|" + getResult.getValue() + "|" + hasAfterDelete;
        });

        assertEquals("true|value|false", result,
            "State CRUD operations should work through the bridge");
    }

    // ======================================================================
    // 32. incrState
    // ======================================================================

    @Test
    void testIncrState() {
        Long result = env.execute(h -> {
            CleatResult<Long> r1 = h.incrState("counter", 5);
            return r1.getValue();
        });

        assertEquals(5L, result.longValue(),
            "incrState should return the incremented value");
    }

    // ======================================================================
    // 33. List state
    // ======================================================================

    @Test
    void testListState() {
        env.execute(h -> {
            h.setState("alpha", "1");
            h.setState("beta", "2");
            return "";
        });

        String result = env.execute(h -> h.listState("al").getValue());

        assertTrue(result.contains("alpha"),
            "listState('al') should include 'alpha', but got: [" + result + "]");
        assertFalse(result.contains("beta"),
            "listState('al') should NOT include 'beta', but got: [" + result + "]");
    }

    // ======================================================================
    // 34. HTTP fetch
    // ======================================================================

    @Test
    void testFetch() {
        String result = env.execute(h -> {
            CleatResult<HostCalls.FetchResult> fetchResult = h.fetchGet("http://example.com");
            if (fetchResult.isErr()) {
                return "error:" + fetchResult.getError();
            }
            return "status:" + fetchResult.getValue().statusCode;
        });

        assertEquals("status:200", result,
            "fetchGet should return a mock 200 response");
    }

    // ======================================================================
    // 35. Lock operations
    // ======================================================================

    @Test
    void testLocks() {
        String result = env.execute(h -> {
            CleatResult<Boolean> lockResult = h.acquireLock("my-lock", 60);
            if (lockResult.isErr() || !lockResult.getValue()) {
                return "lock-failed";
            }
            CleatResult<Void> releaseResult = h.releaseLock("my-lock");
            return releaseResult.isOk() ? "locked-and-released" : "release-failed";
        });

        assertEquals("locked-and-released", result,
            "Lock operations should succeed through the bridge");
    }

    // ======================================================================
    // 36. CleatCall with exact request matching
    // ======================================================================

    @Test
    void testExpectCallWithExactRequestMatching() {
        // WHAT: Verify that expectCall with an exact request string only matches
        // when the actual request equals the expected request
        // WHY: Request matching enables different responses for different inputs

        // Register two stubs with different expected request matchers
        env.expectCall("llm", "chat", "{\"prompt\":\"hello\"}")
            .respond("{\"response\":\"hi\"}");
        env.expectCall("llm", "chat", "{\"prompt\":\"bye\"}")
            .respond("{\"response\":\"goodbye\"}");

        // Use the bridge directly to call with different requests
        CleatResult<String> result1 = env.bridge.cleatCall("llm", "chat", "{\"prompt\":\"hello\"}");
        CleatResult<String> result2 = env.bridge.cleatCall("llm", "chat", "{\"prompt\":\"bye\"}");

        assertEquals("{\"response\":\"hi\"}", result1.getValue(),
            "First call should match the first stub");
        assertEquals("{\"response\":\"goodbye\"}", result2.getValue(),
            "Second call should match the second stub");
    }

    // ======================================================================
    // 37. Signal workflow (signalWorkflow)
    // ======================================================================

    @Test
    void testSignalWorkflowDeliversToSelf() {
        env.execute(h -> {
            h.signalWorkflow("self", "my_signal", "payload-data");
            return "";
        });

        // The signal should be pending in the env
        String result = env.execute(h -> {
            CleatResult<String> pollResult = h.pollSignal("my_signal");
            return pollResult.isOk() ? pollResult.getValue() : "not-found";
        });

        assertEquals("payload-data", result,
            "signalWorkflow should deliver a signal receivable by pollSignal");
    }

    // ======================================================================
    // 38. Cron operations
    // ======================================================================

    @Test
    void testCronOperations() {
        String result = env.execute(h -> {
            CleatResult<String> cronResult = h.scheduleCron(
                "daily", "0 0 * * *", "UTC", "{}");
            if (cronResult.isErr()) {
                return "error";
            }
            String id = cronResult.getValue();
            h.deleteCron(id);
            return id;
        });

        assertEquals("test-schedule-id", result,
            "scheduleCron should return a mock schedule ID");
    }

    // ======================================================================
    // 39. cleatCallHeartbeat
    // ======================================================================

    @Test
    void testCleatCallHeartbeat() {
        env.expectCall("svc", "op").respond("heartbeat-response");

        String result = env.execute(h -> {
            CleatResult<String> hr = h.cleatCallHeartbeat("svc", "op", "{}", 1000);
            return hr.isOk() ? hr.getValue() : "error";
        });

        assertEquals("heartbeat-response", result,
            "cleatCallHeartbeat should return the stubbed response");
    }

    // ======================================================================
    // 40. cleatCallWithRetry
    // ======================================================================

    @Test
    void testCleatCallWithRetry() {
        env.expectCall("svc", "op").respond("{\"status\":\"ok\"}");

        String result = env.execute(h -> {
            HostCalls.RetryPolicy policy = new HostCalls.RetryPolicy(3, 100, 2.0, 1000);
            CleatResult<String> rr = h.cleatCallWithRetry(
                "svc", "op", "{}", String.class, policy);
            return rr.isOk() ? rr.getValue() : "error:" + rr.getError();
        });

        assertTrue(result.contains("ok"),
            "cleatCallWithRetry should return the stubbed response, but got: ["
            + result + "]");
    }

    // ======================================================================
    // 41. UUID
    // ======================================================================

    @Test
    void testUuid() {
        env.setWorkflowId("wf-1");

        String result = env.execute(h -> h.uuid("my-seed"));

        assertNotNull(result, "uuid() should return a non-null value");
        assertEquals(36, result.length(),
            "UUID should be 36 characters long (including hyphens), but got length "
            + result.length() + ": [" + result + "]");
        assertTrue(result.contains("-"),
            "UUID should contain hyphens, but got: [" + result + "]");

        // Deterministic: same seed should produce same UUID
        String result2 = env.execute(h -> h.uuid("my-seed"));
        assertEquals(result, result2,
            "UUID should be deterministic for the same seed and workflow ID");
    }

    // ======================================================================
    // 42. SendSignalAndWait
    // ======================================================================

    @Test
    void testSendSignalAndWaitMs() {
        String result = env.execute(h -> {
            CleatResult<String> swResult = h.sendSignalAndWaitMs(
                "target-1", "ask", "{}", 100);
            if (swResult.isErr()) {
                return "timeout";
            }
            return swResult.getValue();
        });

        assertEquals("timeout", result,
            "sendSignalAndWaitMs should time out when no reply is configured");
    }

    // ======================================================================
    // 43. ReplyToSignal
    // ======================================================================

    @Test
    void testReplyToSignalWithUnknownId() {
        String result = env.execute(h -> {
            CleatResult<Void> reply = h.replyToSignal("unknown-id", "response");
            return reply.isErr() ? "error:" + reply.getError() : "ok";
        });

        assertTrue(result.contains("no pending signal"),
            "replyToSignal with unknown ID should return an error mentioning "
            + "'no pending signal', but got: [" + result + "]");
    }

    // ======================================================================
    // 44. ScheduleInvoke
    // ======================================================================

    @Test
    void testScheduleInvoke() {
        env.execute(h -> {
            CleatResult<Void> result = h.scheduleInvoke("svc", "op", "{}", 60);
            assertTrue(result.isOk(), "scheduleInvoke should succeed");
            return "";
        });
    }

    // ======================================================================
    // 45. asyncSignalWorkflow uses signalWorkflow on bridge correctly
    // ======================================================================

    @Test
    void testSignalWorkflowViaBridge() {
        CleatResult<HostCalls.AwaitSignalsResult> result = env.execute(h -> {
            h.signalWorkflow("other-wf", "notify", "{\"msg\":\"hello\"}");
            return h.awaitSignals(new String[]{"notify"}, 5000);
        });

        assertFalse(result.getValue().timedOut,
            "signalWorkflow should deliver the signal for awaitSignals to pick up");
        assertEquals("notify", result.getValue().signalName,
            "Signal name should be 'notify'");
        assertEquals("{\"msg\":\"hello\"}", result.getValue().payload,
            "Signal payload should be preserved");
    }

    // ======================================================================
    // 46. awaitAllChildren
    // ======================================================================

    @Test
    void testAwaitAllChildren() {
        String result = env.execute(h -> {
            CleatResult<String> c1 = h.childWorkflow("child1", "{}");
            CleatResult<String> c2 = h.childWorkflow("child2", "{}");
            return h.awaitAllChildren(new String[]{
                c1.getValue(), c2.getValue()
            }).getValue();
        });

        assertTrue(result.contains("child1"),
            "awaitAllChildren result should contain child1's run ID, but got: ["
            + result + "]");
        assertTrue(result.contains("child2"),
            "awaitAllChildren result should contain child2's run ID, but got: ["
            + result + "]");
    }

    // ======================================================================
    // 47. HostCallsBridge is a HostCalls instance
    // ======================================================================

    @Test
    void testBridgeIsHostCallsInstance() {
        assertTrue(env.bridge instanceof HostCalls,
            "The bridge must be an instance of HostCalls so that it can be "
            + "passed to workflow methods that expect HostCalls");
    }

    // ======================================================================
    // 48. Multiple plugin calls with same prefix
    // ======================================================================

    @Test
    void testMultiplePluginCalls() {
        env.expectPluginCall("llm", "chat").respond("{\"text\":\"response1\"}");
        env.expectPluginCall("llm", "embed").respond("{\"vector\":[0.1,0.2]}");

        String result = env.execute(h -> {
            String chatResp = h.pluginCall("llm", "chat", "{}").getValue();
            String embedResp = h.pluginCall("llm", "embed", "{}").getValue();
            return chatResp + "|" + embedResp;
        });

        assertEquals(
            "{\"text\":\"response1\"}|{\"vector\":[0.1,0.2]}",
            result,
            "Different plugin functions on the same plugin should each get their own stub");
    }

    // ======================================================================
    // 49. HostCallsBridge delegates pluginCall to delegate.registerPluginCallStub
    // ======================================================================

    @Test
    void testPluginCallErrorStub() {
        env.expectPluginCall("llm", "chat").respondError("rate limited");

        String result = env.execute(h -> {
            CleatResult<String> r = h.pluginCall("llm", "chat", "{}");
            return r.isErr() ? "err:" + r.getError() : r.getValue();
        });

        assertTrue(result.contains("rate limited"),
            "Plugin call error stub should return the configured error, but got: ["
            + result + "]");
    }

    // ======================================================================
    // 50. Empty env produces no call history
    // ======================================================================

    @Test
    void testEmptyEnvironmentHasNoHistory() {
        assertEquals(0, env.callHistory().size(),
            "A fresh CleatTestEnv should have no call history");
    }

    // ======================================================================
    // 51. Worker function returning void
    // ======================================================================

    @Test
    void testExecuteConsumerStyle() {
        // A workflow that only calls log, no return value
        env.execute(h -> {
            h.cleatLog("hello");
            return null;
        });
        // No exception = success
    }

    // ======================================================================
    // 52. Plugin option for bridging (expectPluginCall by-passes cleatCall)
    // ======================================================================

    @Test
    void testCleatCallAndPluginCallAreIndependent() {
        // Stub only cleatCall
        env.expectCall("llm", "chat").respond("cleat-call-response");

        // pluginCall should NOT be satisfied by cleatCall stubs
        String result = env.execute(h -> {
            CleatResult<String> pluginResult = h.pluginCall("llm", "chat", "{}");
            return pluginResult.isOk() ? pluginResult.getValue() : "plugin-not-stubbed";
        });

        assertEquals("plugin-not-stubbed", result,
            "pluginCall should not match cleatCall stubs — they use separate registries");
    }
}
