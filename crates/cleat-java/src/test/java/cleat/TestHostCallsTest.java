package cleat;

import static org.junit.jupiter.api.Assertions.*;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

/**
 * Comprehensive unit tests for {@link TestHostCalls}.
 * Every assertion failure includes a descriptive message covering WHAT failed,
 * WHY it matters, and WHAT was expected vs actual.
 *
 * @see TestHostCalls
 */
class TestHostCallsTest {

    private TestHostCalls host;

    @BeforeEach
    void setUp() {
        host = new TestHostCalls();
    }

    // ======================================================================
    // 1. cleatCall with stub
    // ======================================================================

    @Test
    void testCleatCallWithStubReturnsSuccess() {
        // WHAT: Verify cleatCall returns the stubbed response when a stub is registered
        // WHY: The stub mechanism is the primary way to control external service responses in tests
        String expectedResponse = "{\"orderID\":\"ord-1\",\"status\":\"created\"}";
        host.registerCallStub("orders", "Create", expectedResponse);

        CleatResult<String> result = host.cleatCall("orders", "Create", "{\"item\":\"book\"}");

        assertTrue(result.isOk(),
            "Expected cleatCall to succeed when a stub is registered for orders.Create, "
            + "but got error: " + result.getError());
        assertEquals(expectedResponse, result.getValue(),
            "Expected cleatCall to return the stubbed response [" + expectedResponse + "], "
            + "but got: [" + result.getValue() + "]");
    }

    @Test
    void testCleatCallWithStubRecordsCallHistory() {
        // WHAT: Verify assertCalled returns true after a stubbed cleatCall
        // WHY: Assertions depend on accurate call recording
        host.registerCallStub("orders", "Create", "{\"ok\":true}");

        host.cleatCall("orders", "Create", "{\"item\":\"book\"}");

        assertTrue(host.assertCalled("orders", "Create"),
            "Expected assertCalled(orders, Create) to return true after a call was made, "
            + "but it returned false. The callHistory was not updated correctly.");
    }

    // ======================================================================
    // 2. cleatCall without stub
    // ======================================================================

    @Test
    void testCleatCallWithoutStubReturnsError() {
        // WHAT: Verify cleatCall returns error when no stub is registered
        // WHY: Missing stubs should produce a clear error to prevent silent test failures
        CleatResult<String> result = host.cleatCall("orders", "Create", "{}");

        assertTrue(result.isErr(),
            "Expected cleatCall to return error when no stub is registered for orders.Create, "
            + "but got success with value: " + result.getValue());
        String errMsg = result.getError();
        assertNotNull(errMsg,
            "Expected error message to be non-null when no stub is registered");
        assertTrue(errMsg.contains("no stub registered"),
            "Expected error message to mention 'no stub registered', but got: [" + errMsg + "]");
    }

    // ======================================================================
    // 3. cleatCall with error stub
    // ======================================================================

    @Test
    void testCleatCallWithErrorStubReturnsError() {
        // WHAT: Verify registerCallError causes cleatCall to return an error
        // WHY: Simulating service errors is critical for error-handling test coverage
        host.registerCallError("payments", "Charge", "insufficient funds");

        CleatResult<String> result = host.cleatCall("payments", "Charge", "{\"amount\":100}");

        assertTrue(result.isErr(),
            "Expected cleatCall to return error when an error stub is registered for payments.Charge, "
            + "but got success with value: " + result.getValue());
        assertEquals("insufficient funds", result.getError(),
            "Expected error message to match the registered error stub");
    }

    // ======================================================================
    // 4. callCount
    // ======================================================================

    @Test
    void testCallCountReturnsCorrectNumber() {
        // WHAT: Verify callCount returns accurate counts for multiple identical calls
        // WHY: Call counting is essential for verifying retry and fan-out patterns
        host.registerCallStub("svc", "op", "resp");
        host.registerCallStub("svc", "op", "resp");
        host.registerCallStub("svc", "op", "resp");

        host.cleatCall("svc", "op", "req1");
        host.cleatCall("svc", "op", "req2");
        host.cleatCall("svc", "op", "req3");

        assertEquals(3, host.callCount("svc", "op"),
            "Expected callCount(svc, op) to be 3 after three calls, "
            + "but got: " + host.callCount("svc", "op"));
    }

    @Test
    void testCallCountReturnsZeroForUncalledService() {
        assertEquals(0, host.callCount("unknown", "op"),
            "Expected callCount to return 0 for a service/operation that was never called, "
            + "but got: " + host.callCount("unknown", "op"));
    }

    // ======================================================================
    // 5. assertNotCalled
    // ======================================================================

    @Test
    void testAssertNotCalledReturnsTrueForUninvoked() {
        // WHAT: Verify assertNotCalled returns true when no call was made
        // WHY: Verifying absence of calls is as important as verifying presence
        assertTrue(host.assertNotCalled("shipping", "CreateShipment"),
            "Expected assertNotCalled(shipping, CreateShipment) to return true "
            + "when no calls were made to that service/operation, but it returned false");
    }

    @Test
    void testAssertNotCalledReturnsFalseAfterCall() {
        // WHAT: Verify assertNotCalled returns false after a call has been made
        // WHY: AssertNotCalled should accurately reflect call history
        host.registerCallStub("shipping", "CreateShipment", "{}");
        host.cleatCall("shipping", "CreateShipment", "{}");

        assertFalse(host.assertNotCalled("shipping", "CreateShipment"),
            "Expected assertNotCalled(shipping, CreateShipment) to return false "
            + "after a call was made, but it returned true. The call was not recorded.");
    }

    // ======================================================================
    // 6. assertCallCount
    // ======================================================================

    @Test
    void testAssertCallCountExactMatch() {
        // WHAT: Verify assertCallCount returns true for exact count
        // WHY: Exact count assertions verify that operations are neither under- nor over-invoked
        host.registerCallStub("svc", "op", "r1");
        host.registerCallStub("svc", "op", "r2");

        host.cleatCall("svc", "op", "q1");
        host.cleatCall("svc", "op", "q2");

        assertTrue(host.assertCallCount("svc", "op", 2),
            "Expected assertCallCount(svc, op, 2) to return true after exactly 2 calls, "
            + "but it returned false. Actual count: " + host.callCount("svc", "op"));
    }

    @Test
    void testAssertCallCountMismatch() {
        host.registerCallStub("svc", "op", "r");
        host.cleatCall("svc", "op", "q");

        assertFalse(host.assertCallCount("svc", "op", 5),
            "Expected assertCallCount(svc, op, 5) to return false when actual count is 1, "
            + "but it returned true");
    }

    // ======================================================================
    // 7. getCallHistory
    // ======================================================================

    @Test
    void testGetCallHistoryContainsCorrectEntries() {
        // WHAT: Verify getCallHistory returns entries with correct service/operation/request
        // WHY: Call history inspection is needed for complex multi-call workflow verification
        host.registerCallStub("svc1", "op1", "resp1");
        host.registerCallStub("svc2", "op2", "resp2");

        host.cleatCall("svc1", "op1", "req1");
        host.cleatCall("svc2", "op2", "req2");

        java.util.List<TestHostCalls.CallRecord> history = host.getCallHistory();

        assertNotNull(history,
            "Expected getCallHistory() to return non-null list");
        assertEquals(2, history.size(),
            "Expected 2 entries in call history after 2 calls, but got: " + history.size());

        TestHostCalls.CallRecord first = history.get(0);
        assertEquals("svc1", first.service,
            "Expected first entry service to be 'svc1', but got: [" + first.service + "]");
        assertEquals("op1", first.operation,
            "Expected first entry operation to be 'op1', but got: [" + first.operation + "]");
        assertEquals("req1", first.request,
            "Expected first entry request to be 'req1', but got: [" + first.request + "]");

        TestHostCalls.CallRecord second = history.get(1);
        assertEquals("svc2", second.service,
            "Expected second entry service to be 'svc2', but got: [" + second.service + "]");
        assertEquals("op2", second.operation,
            "Expected second entry operation to be 'op2', but got: [" + second.operation + "]");
        assertEquals("req2", second.request,
            "Expected second entry request to be 'req2', but got: [" + second.request + "]");
    }

    // ======================================================================
    // 8. cleatSleep advances clock
    // ======================================================================

    @Test
    void testCleatSleepAdvancesClock() {
        // WHAT: Verify cleatSleep advances the simulated clock by the specified duration
        // WHY: Time-based workflows depend on accurate clock advancement for testing timers

        long before = host.now();
        host.cleatSleep(5000);
        long after = host.now();

        long expected = before + 5000;
        assertEquals(expected, after,
            "Expected now() to advance by 5000ms after cleatSleep(5000). "
            + "Before: " + before + ", after: " + after + ", expected: " + expected);
    }

    @Test
    void testCleatSleepReturnsFalse() {
        // WHAT: Verify cleatSleep returns false (never suspends in mock mode)
        // WHY: The return value is a signal for workflow suspension, which should not happen in tests
        boolean result = host.cleatSleep(1000);

        assertFalse(result,
            "Expected cleatSleep to return false in mock mode (never suspends), "
            + "but got true");
    }

    // ======================================================================
    // 9. setTime / advanceTime
    // ======================================================================

    @Test
    void testSetTimeAndAdvanceTime() {
        // WHAT: Verify setTime sets the clock to an absolute value and advanceTime adds to it
        // WHY: Absolute and relative time control are both needed for comprehensive time testing

        host.setTime(1000000L);
        assertEquals(1000000L, host.now(),
            "Expected now() to return 1000000 after setTime(1000000), "
            + "but got: " + host.now());

        host.advanceTime(5000L);
        assertEquals(1005000L, host.now(),
            "Expected now() to return 1005000 after advanceTime(5000) from 1000000, "
            + "but got: " + host.now());
    }

    @Test
    void testAdvanceTimeWithZero() {
        long before = host.now();
        host.advanceTime(0);
        assertEquals(before, host.now(),
            "Expected advanceTime(0) to not change the clock, "
            + "but it changed from " + before + " to " + host.now());
    }

    // ======================================================================
    // 10. random with sequence
    // ======================================================================

    @Test
    void testRandomReturnsPreconfiguredSequence() {
        // WHAT: Verify random() returns values in the pre-configured sequence
        // WHY: Deterministic randomness is essential for reproducible tests

        long[] seq = new long[]{42L, 100L, 999L};
        host.setRandomSeq(seq);

        for (int i = 0; i < seq.length; i++) {
            long val = host.random();
            assertEquals(seq[i], val,
                "Expected random() at index " + i + " to return " + seq[i]
                + " from configured sequence, but got: " + val);
        }
    }

    @Test
    void testRandomReturnsZeroAfterSequenceExhausted() {
        // WHAT: Verify random() returns 0 after the pre-configured sequence is exhausted
        // WHY: Default behavior when no more random values are configured should be predictable

        host.setRandomSeq(new long[]{123L});
        assertEquals(123L, host.random(),
            "Expected first random() to return first sequence element");
        assertEquals(0L, host.random(),
            "Expected random() to return 0 after sequence is exhausted, "
            + "but got non-zero value");
    }

    // ======================================================================
    // 11. version / minVersion
    // ======================================================================

    @Test
    void testVersionDefaultsTo1() {
        // WHAT: Verify version() returns 1 by default
        // WHY: Default version must be reasonable for backward compatibility
        assertEquals(1, host.version(),
            "Expected default version() to return 1, but got: " + host.version());
    }

    @Test
    void testVersionReturnsSetValue() {
        // WHAT: Verify setVersion changes version() return value
        // WHY: Versioned workflows need explicit version configuration in tests

        host.setVersion(5);
        assertEquals(5, host.version(),
            "Expected version() to return 5 after setVersion(5), but got: " + host.version());
    }

    @Test
    void testMinVersionDefaultsTo1() {
        assertEquals(1, host.minVersion(),
            "Expected default minVersion() to return 1, but got: " + host.minVersion());
    }

    @Test
    void testMinVersionReturnsSetValue() {
        host.setMinVersion(3);
        assertEquals(3, host.minVersion(),
            "Expected minVersion() to return 3 after setMinVersion(3), but got: " + host.minVersion());
    }

    // ======================================================================
    // 12. childWorkflow
    // ======================================================================

    @Test
    void testChildWorkflowReturnsRunId() {
        // WHAT: Verify childWorkflow returns a run ID and awaitChild returns a default result
        // WHY: Child workflow orchestration is a core pattern in cleat workflows

        CleatResult<String> childResult = host.childWorkflow("myChild", "{\"data\":1}");
        assertTrue(childResult.isOk(),
            "Expected childWorkflow to succeed, but got error: "
            + (childResult.isErr() ? childResult.getError() : "none"));
        String runId = childResult.getValue();
        assertNotNull(runId,
            "Expected childWorkflow run ID to be non-null");
        assertTrue(runId.contains("child-myChild"),
            "Expected runId to contain 'child-myChild', but got: [" + runId + "]");

        // Default auto-complete behavior
        CleatResult<String> awaitResult = host.awaitChild(runId);
        assertTrue(awaitResult.isOk(),
            "Expected awaitChild to succeed for runId: " + runId + ", but got error: "
            + (awaitResult.isErr() ? awaitResult.getError() : "none"));
        assertTrue(awaitResult.getValue().contains("completed"),
            "Expected default child result to contain 'completed', "
            + "but got: [" + awaitResult.getValue() + "]");
    }

    @Test
    void testChildWorkflowIncrementsRunId() {
        CleatResult<String> first = host.childWorkflow("wf", "{}");
        CleatResult<String> second = host.childWorkflow("wf", "{}");

        assertTrue(first.isOk() && second.isOk(),
            "Expected both childWorkflow calls to succeed");
        assertNotEquals(first.getValue(), second.getValue(),
            "Expected consecutive childWorkflow calls to return different runIds, "
            + "but they returned the same: [" + first.getValue() + "]");
    }

    // ======================================================================
    // 13. registerChildWorkflowStub
    // ======================================================================

    @Test
    void testRegisterChildWorkflowStubReturnsStubbedResult() {
        // WHAT: Verify registerChildWorkflowStub overrides the default child result
        // WHY: Tests need to control child workflow return values for comprehensive scenarios

        host.registerChildWorkflowStub("paymentChild", "{\"status\":\"paid\"}");

        CleatResult<String> childResult = host.childWorkflow("paymentChild", "{}");
        assertTrue(childResult.isOk(),
            "Expected childWorkflow to succeed with stub registered, but got error");
        String runId = childResult.getValue();

        CleatResult<String> awaitResult = host.awaitChild(runId);
        assertTrue(awaitResult.isOk(),
            "Expected awaitChild to succeed for stubbed child, but got error: "
            + (awaitResult.isErr() ? awaitResult.getError() : "none"));
        assertEquals("{\"status\":\"paid\"}", awaitResult.getValue(),
            "Expected stubbed child result to be '{\"status\":\"paid\"}', "
            + "but got: [" + awaitResult.getValue() + "]");
    }

    // ======================================================================
    // 14. awaitSignals
    // ======================================================================

    @Test
    void testAwaitSignalsReceivesDeliveredSignal() {
        // WHAT: Verify awaitSignals returns a signal that was delivered via deliverSignal
        // WHY: Signal-based workflow coordination is a key pattern

        host.deliverSignal("payment_received", "{\"amount\":5000}");

        CleatResult<HostCalls.AwaitSignalsResult> result = host.awaitSignals(
            new String[]{"payment_received"}, 1000);

        assertTrue(result.isOk(),
            "Expected awaitSignals to succeed after delivering 'payment_received', "
            + "but got error: " + result.getError());
        HostCalls.AwaitSignalsResult signalsResult = result.getValue();
        assertNotNull(signalsResult,
            "Expected awaitSignals result to be non-null");
        assertFalse(signalsResult.timedOut,
            "Expected awaitSignals to NOT time out when 'payment_received' was delivered");

        assertEquals("payment_received", signalsResult.signalName,
            "Expected received signal name to be 'payment_received', "
            + "but got: [" + signalsResult.signalName + "]");
        assertEquals("{\"amount\":5000}", signalsResult.payload,
            "Expected received signal payload to be '{\"amount\":5000}', "
            + "but got: [" + signalsResult.payload + "]");
    }

    // ======================================================================
    // 15. awaitSignals timeout
    // ======================================================================

    @Test
    void testAwaitSignalsTimeoutReturnsTimedOut() {
        // WHAT: Verify awaitSignals returns timedOut=true when no signal matches
        // WHY: Workflow timeout logic must be testable when signals are not received

        CleatResult<HostCalls.AwaitSignalsResult> result = host.awaitSignals(
            new String[]{"never_sent"}, 100);

        assertTrue(result.isOk(),
            "Expected awaitSignals to return Ok result even on timeout");
        assertTrue(result.getValue().timedOut,
            "Expected awaitSignals timedOut to be true when no matching signal was delivered, "
            + "but it was false");
        assertEquals("", result.getValue().signalName,
            "Expected signal name to be empty on timeout, "
            + "but got: [" + result.getValue().signalName + "]");
    }

    // ======================================================================
    // 16. setQueryState / getQueryState
    // ======================================================================

    @Test
    void testSetQueryStateAndGetQueryState() {
        // WHAT: Verify setQueryState/getQueryState round-trip works correctly
        // WHY: Query state is the mechanism for exposing workflow state to external callers

        host.setQueryState("orderStatus", "shipped");

        CleatResult<String> result = host.getQueryState("orderStatus");
        assertTrue(result.isOk(),
            "Expected getQueryState('orderStatus') to succeed after setting it, "
            + "but got error: " + result.getError());
        assertEquals("shipped", result.getValue(),
            "Expected getQueryState('orderStatus') to return 'shipped', "
            + "but got: [" + result.getValue() + "]");
    }

    // ======================================================================
    // 17. getQueryState missing key
    // ======================================================================

    @Test
    void testGetQueryStateMissingKeyReturnsError() {
        // WHAT: Verify getQueryState returns an error for a key that was never set
        // WHY: Tests need to verify proper error handling for missing query state keys

        CleatResult<String> result = host.getQueryState("nonexistent");

        assertTrue(result.isErr(),
            "Expected getQueryState('nonexistent') to return error for a missing key, "
            + "but got success with value: " + result.getValue());
        String errMsg = result.getError();
        assertNotNull(errMsg,
            "Expected error message to be non-null for missing key");
        assertTrue(errMsg.contains("query state key not found"),
            "Expected error message to mention 'query state key not found', "
            + "but got: [" + errMsg + "]");
    }

    // ======================================================================
    // 18. setState / getState / deleteState / hasState / listState
    // ======================================================================

    @Test
    void testStateFullCRUD() {
        // WHAT: Verify full CRUD lifecycle for workflow state
        // WHY: Durable state is a fundamental building block for cleat workflows

        // Create / set
        CleatResult<Void> setResult = host.setState("myKey", "myValue");
        assertTrue(setResult.isOk(),
            "Expected setState('myKey', 'myValue') to succeed, "
            + "but got error: " + setResult.getError());

        // Read
        assertTrue(host.hasState("myKey"),
            "Expected hasState('myKey') to return true after setState");
        CleatResult<String> getResult = host.getState("myKey");
        assertTrue(getResult.isOk(),
            "Expected getState('myKey') to succeed after setting it, "
            + "but got error: " + getResult.getError());
        assertEquals("myValue", getResult.getValue(),
            "Expected getState('myKey') to return 'myValue', "
            + "but got: [" + getResult.getValue() + "]");

        // List with prefix
        CleatResult<String> listResult = host.listState("my");
        assertTrue(listResult.isOk(),
            "Expected listState('my') to succeed, "
            + "but got error: " + listResult.getError());
        String listJson = listResult.getValue();
        assertTrue(listJson.contains("myKey"),
            "Expected listState('my') result to contain 'myKey', "
            + "but got: [" + listJson + "]");

        // Delete
        CleatResult<Void> delResult = host.deleteState("myKey");
        assertTrue(delResult.isOk(),
            "Expected deleteState('myKey') to succeed, "
            + "but got error: " + delResult.getError());
        assertFalse(host.hasState("myKey"),
            "Expected hasState('myKey') to return false after deleteState");

        // Read after delete
        CleatResult<String> getAfterDel = host.getState("myKey");
        assertTrue(getAfterDel.isErr(),
            "Expected getState('myKey') to return error after delete, "
            + "but got success with value: " + getAfterDel.getValue());
    }

    @Test
    void testGetStateMissingKeyReturnsError() {
        CleatResult<String> result = host.getState("does_not_exist");
        assertTrue(result.isErr(),
            "Expected getState for missing key to return error");
        String errMsg = result.getError();
        assertTrue(errMsg.contains("no such key"),
            "Expected error message to mention 'no such key', "
            + "but got: [" + errMsg + "]");
    }

    @Test
    void testHasStateReturnsFalseForMissingKey() {
        assertFalse(host.hasState("absent"),
            "Expected hasState('absent') to return false for a key that was never set");
    }

    @Test
    void testListStateWithNoMatchesReturnsEmptyArray() {
        host.setState("abc", "val");

        CleatResult<String> result = host.listState("xyz");
        assertTrue(result.isOk(),
            "Expected listState('xyz') to succeed even with no matches");
        assertEquals("[]", result.getValue(),
            "Expected listState('xyz') to return '[]' when no keys match, "
            + "but got: [" + result.getValue() + "]");
    }

    // ======================================================================
    // 19. incrState
    // ======================================================================

    @Test
    void testIncrStateIncrementsFromZero() {
        // WHAT: Verify incrState returns the delta for a new key (starting from 0)
        // WHY: Atomic incrementing counters is a common workflow pattern

        long newVal = host.incrState("counter", 5);
        assertEquals(5L, newVal,
            "Expected incrState('counter', 5) to return 5 for a new key, "
            + "but got: " + newVal);
    }

    @Test
    void testIncrStateMultipleIncrements() {
        // WHAT: Verify incrState accumulative increments
        // WHY: Counter values must compound correctly over multiple operations

        assertEquals(5L, host.incrState("counter", 5),
            "First incrState should return 5");
        assertEquals(8L, host.incrState("counter", 3),
            "Second incrState(3) should return 8 (5+3), "
            + "but got: " + host.incrState("counter", 0));
    }

    @Test
    void testIncrStateNegativeDelta() {
        assertEquals(10L, host.incrState("counter", 10),
            "First incrState(10) should return 10");
        assertEquals(7L, host.incrState("counter", -3),
            "incrState(-3) from 10 should return 7, "
            + "but got: " + host.incrState("counter", 0));
    }

    // ======================================================================
    // 20. scope operations
    // ======================================================================

    @Test
    void testSetScopeReturnsPreviousScope() {
        // WHAT: Verify setScope returns empty string as initial previous scope
        // WHY: Scope management affects state key isolation in virtual object workflows

        String prev = host.setScope("Order", "ord-123");
        assertEquals("",
            prev,
            "Expected initial setScope to return empty string as previous scope, "
            + "but got: [" + prev + "]");

        // Subsequent setScope returns previous
        String prev2 = host.setScope("Invoice", "inv-456");
        assertEquals("vo:Order:ord-123:", prev2,
            "Expected second setScope to return previous scope prefix, "
            + "but got: [" + prev2 + "]");
    }

    @Test
    void testScopePrefixesStateKeys() {
        // WHAT: Verify state keys are prefixed with the scope
        // WHY: Scoped state isolation prevents key collisions in virtual objects

        host.setScope("Order", "ord-123");
        host.setState("status", "pending");

        // Without scope, the key 'status' should not exist
        assertFalse(host.hasState("status"),
            "Expected hasState('status') to return false when scope is set "
            + "(key should be prefixed)");
    }

    @Test
    void testGetScopeReturnsScopedValues() {
        host.setScope("Order", "ord-123");

        String[] scope = host.getScope();
        assertNotNull(scope,
            "Expected getScope() to return non-null array");
        assertEquals(2, scope.length,
            "Expected getScope() to return 2-element array");
        assertEquals("Order", scope[0],
            "Expected getScope()[0] to be 'Order', "
            + "but got: [" + scope[0] + "]");
        assertEquals("ord-123", scope[1],
            "Expected getScope()[1] to be 'ord-123', "
            + "but got: [" + scope[1] + "]");
    }

    @Test
    void testClearScopeReturnsPreviousScope() {
        host.setScope("Order", "ord-123");

        String prev = host.clearScope();
        assertEquals("vo:Order:ord-123:", prev,
            "Expected clearScope() to return the previous scope prefix, "
            + "but got: [" + prev + "]");

        String[] scope = host.getScope();
        assertEquals("", scope[0],
            "Expected getScope()[0] to be empty after clearScope, "
            + "but got: [" + scope[0] + "]");
        assertEquals("", scope[1],
            "Expected getScope()[1] to be empty after clearScope, "
            + "but got: [" + scope[1] + "]");
    }

    @Test
    void testClearScopeWhenNoScopeSet() {
        String prev = host.clearScope();
        assertEquals("",
            prev,
            "Expected clearScope() with no scope set to return empty string, "
            + "but got: [" + prev + "]");
    }

    // ======================================================================
    // 21. createPromise / resolvePromise / awaitPromise
    // ======================================================================

    @Test
    void testPromiseFullLifecycle() {
        // WHAT: Verify full promise lifecycle: create, resolve, await
        // WHY: Durable promises enable cross-workflow coordination

        CleatResult<String> createResult = host.createPromise("order-approved");
        assertTrue(createResult.isOk(),
            "Expected createPromise('order-approved') to succeed, "
            + "but got error: " + createResult.getError());
        String promiseId = createResult.getValue();
        assertNotNull(promiseId,
            "Expected promise ID to be non-null after creation");
        assertTrue(promiseId.contains("order-approved"),
            "Expected promise ID to contain 'order-approved', "
            + "but got: [" + promiseId + "]");

        // Resolve
        CleatResult<Void> resolveResult = host.resolvePromise(promiseId, "{\"approved\":true}");
        assertTrue(resolveResult.isOk(),
            "Expected resolvePromise to succeed, "
            + "but got error: " + resolveResult.getError());

        // Await
        CleatResult<HostCalls.AwaitPromiseResult> awaitResult = host.awaitPromise(promiseId, 5000);
        assertTrue(awaitResult.isOk(),
            "Expected awaitPromise to succeed after resolve, "
            + "but got error: " + awaitResult.getError());
        HostCalls.AwaitPromiseResult apResult = awaitResult.getValue();
        assertNotNull(apResult,
            "Expected awaitPromise result to be non-null");
        assertFalse(apResult.timedOut,
            "Expected awaitPromise to NOT time out when promise was resolved");
        assertEquals("{\"approved\":true}", apResult.result,
            "Expected awaitPromise result to be the resolved value, "
            + "but got: [" + apResult.result + "]");
    }

    // ======================================================================
    // 22. rejectPromise
    // ======================================================================

    @Test
    void testRejectPromiseCausesAwaitToReturnError() {
        // WHAT: Verify awaitPromise returns an error after rejectPromise
        // WHY: Promise rejection should propagate errors to awaiting workflows

        CleatResult<String> createResult = host.createPromise("my-promise");
        assertTrue(createResult.isOk(), "Expected createPromise to succeed");
        String promiseId = createResult.getValue();

        CleatResult<Void> rejectResult = host.rejectPromise(promiseId, "order cancelled");
        assertTrue(rejectResult.isOk(),
            "Expected rejectPromise to succeed, "
            + "but got error: " + rejectResult.getError());

        CleatResult<HostCalls.AwaitPromiseResult> awaitResult = host.awaitPromise(promiseId, 5000);
        assertTrue(awaitResult.isErr(),
            "Expected awaitPromise to return error after the promise was rejected, "
            + "but got success");
        String errMsg = awaitResult.getError();
        assertTrue(errMsg.contains("order cancelled"),
            "Expected awaitPromise error message to contain 'order cancelled', "
            + "but got: [" + errMsg + "]");
    }

    @Test
    void testRejectPromiseOnNonExistentIdReturnsError() {
        CleatResult<Void> result = host.rejectPromise("not-exist", "error");
        assertTrue(result.isErr(),
            "Expected rejectPromise on non-existent ID to return error");
        assertTrue(result.getError().contains("promise not found"),
            "Expected error to mention 'promise not found', "
            + "but got: [" + result.getError() + "]");
    }

    // ======================================================================
    // 23. signalWorkflow
    // ======================================================================

    @Test
    void testSignalWorkflowDeliversSignal() {
        // WHAT: Verify signalWorkflow delivers a signal that can be received by awaitSignals
        // WHY: Signal delivery is the inter-workflow communication mechanism

        CleatResult<Void> signalResult = host.signalWorkflow(
            "target-run-1", "order_shipped", "{\"orderID\":\"ord-1\"}");
        assertTrue(signalResult.isOk(),
            "Expected signalWorkflow to succeed, "
            + "but got error: " + signalResult.getError());

        // Verify the signal was delivered and can be awaited
        CleatResult<HostCalls.AwaitSignalsResult> awaitResult = host.awaitSignals(
            new String[]{"order_shipped"}, 1000);
        assertTrue(awaitResult.isOk(),
            "Expected awaitSignals to find the signal sent by signalWorkflow, "
            + "but got error: " + awaitResult.getError());
        HostCalls.AwaitSignalsResult asr = awaitResult.getValue();
        assertEquals("order_shipped", asr.signalName,
            "Expected received signal name to match the one sent, "
            + "but got: [" + asr.signalName + "]");
        assertEquals("{\"orderID\":\"ord-1\"}", asr.payload,
            "Expected received signal payload to match the one sent, "
            + "but got: [" + asr.payload + "]");
    }

    // ======================================================================
    // 24. sendSignalAndWait / replyToSignal
    // ======================================================================

    @Test
    void testSendSignalAndWaitTimesOutByDefault() {
        // WHAT: Verify sendSignalAndWait returns a timeout error when no reply is sent
        // WHY: Signal-and-wait is the request-reply pattern for cross-workflow communication

        CleatResult<String> result = host.sendSignalAndWait(
            "target-1", "request_info", "{\"ask\":\"status\"}",
            100);

        assertTrue(result.isErr(),
            "Expected sendSignalAndWait to time out when no reply is sent, "
            + "but got success with value: " + result.getValue());
        String errMsg = result.getError();
        assertTrue(errMsg.contains("timed out"),
            "Expected error message to mention 'timed out', "
            + "but got: [" + errMsg + "]");
    }

    @Test
    void testReplyToSignalWithKnownCorrelationIdSucceeds() {
        // WHAT: Verify replyToSignal succeeds for a known correlation ID that was
        //       registered by a previous sendSignalAndWait call
        // WHY: The reply mechanism enables the signal sender to receive a response

        // sendSignalAndWait creates a reply channel and then times out.
        // We can reply after the timeout because the channel remains registered.
        host.sendSignalAndWait("target-1", "mySignal", "{}", 100);

        // The correlation ID pattern: "corr-target-1-mySignal-1" (first call, counter=1)
        CleatResult<Void> replyResult = host.replyToSignal(
            "corr-target-1-mySignal-1", "{\"status\":\"ok\"}");

        assertTrue(replyResult.isOk(),
            "Expected replyToSignal to succeed for a known correlation ID "
            + "'corr-target-1-mySignal-1' that was registered by sendSignalAndWait, "
            + "but got error: " + replyResult.getError());
    }

    @Test
    void testReplyToSignalWithUnknownIdReturnsError() {
        CleatResult<Void> result = host.replyToSignal(
            "unknown-correlation-id", "response");

        assertTrue(result.isErr(),
            "Expected replyToSignal with unknown correlation ID to return error, "
            + "but got success");
        assertTrue(result.getError().contains("no pending signal"),
            "Expected error to mention 'no pending signal', "
            + "but got: [" + result.getError() + "]");
    }

    // ======================================================================
    // 25. pollCancellation
    // ======================================================================

    @Test
    void testPollCancellationReturnsFalseByDefault() {
        // WHAT: Verify pollCancellation returns false when not cancelled
        // WHY: Default cancellation state must be false to not interrupt normal workflow flow

        CleatResult<Boolean> result = host.pollCancellation();
        assertTrue(result.isOk(),
            "Expected pollCancellation to succeed, but got error: " + result.getError());
        assertFalse(result.getValue(),
            "Expected pollCancellation to return false by default, but got true");
    }

    @Test
    void testPollCancellationReturnsTrueAfterSetCancelled() {
        // WHAT: Verify pollCancellation returns true after setCancelled(true, ...)
        // WHY: Workflows must be able to detect cancellation requests

        host.setCancelled(true, "workflow timeout");

        CleatResult<Boolean> result = host.pollCancellation();
        assertTrue(result.isOk(),
            "Expected pollCancellation to succeed after setCancelled");
        assertTrue(result.getValue(),
            "Expected pollCancellation to return true after setCancelled(true), "
            + "but got false");
    }

    @Test
    void testPollCancellationAfterSetCancelledFalse() {
        host.setCancelled(true, "reason");
        host.setCancelled(false, "");

        CleatResult<Boolean> result = host.pollCancellation();
        assertFalse(result.getValue(),
            "Expected pollCancellation to return false after setCancelled(false), "
            + "but got true");
    }

    // ======================================================================
    // 26. cleatDefer
    // ======================================================================

    @Test
    void testCleatDeferReturnsDeferId() {
        // WHAT: Verify cleatDefer returns a valid defer ID
        // WHY: Deferred cleanup is needed for proper resource management in workflows

        CleatResult<String> result = host.cleatDefer("close database connection");
        assertTrue(result.isOk(),
            "Expected cleatDefer to succeed, but got error: " + result.getError());
        String deferId = result.getValue();
        assertNotNull(deferId,
            "Expected defer ID to be non-null");
        assertTrue(deferId.contains("defer-"),
            "Expected defer ID to contain 'defer-', but got: [" + deferId + "]");
    }

    @Test
    void testCleatDeferIncrementsDeferId() {
        CleatResult<String> first = host.cleatDefer("first");
        CleatResult<String> second = host.cleatDefer("second");

        assertTrue(first.isOk() && second.isOk(),
            "Expected both cleatDefer calls to succeed");
        assertNotEquals(first.getValue(), second.getValue(),
            "Expected defer IDs to be unique across calls, "
            + "but first: [" + first.getValue() + "], second: [" + second.getValue() + "]");
    }

    // ======================================================================
    // 27. runDetached
    // ======================================================================

    @Test
    void testRunDetachedReturnsSuccess() {
        // WHAT: Verify runDetached returns a success result
        // WHY: Detached workflow execution is needed for fire-and-forget patterns

        CleatResult<Void> result = host.runDetached("cleanupWorkflow", "{\"task\":\"clean\"}");
        assertTrue(result.isOk(),
            "Expected runDetached to succeed, but got error: " + result.getError());
    }

    // ======================================================================
    // 28. retry simulation
    // ======================================================================

    @Test
    void testRetrySimulationFailsFirstNCallsThenSucceeds() {
        // WHAT: Verify setRetrySimulation makes the first N calls fail, then the next succeeds
        // WHY: Retry logic is critical for reliable workflows and must be testable

        host.setRetrySimulation(2);
        host.registerCallStub("svc", "op", "success-response");

        // First call should fail (retry simulation)
        CleatResult<String> first = host.cleatCall("svc", "op", "req");
        assertTrue(first.isErr(),
            "Expected first cleatCall to fail with retrySimulation=2, "
            + "but got success: " + first.getValue());
        assertTrue(first.getError().contains("simulated transient failure"),
            "Expected first error to mention 'simulated transient failure', "
            + "but got: [" + first.getError() + "]");

        // Second call should also fail
        CleatResult<String> second = host.cleatCall("svc", "op", "req");
        assertTrue(second.isErr(),
            "Expected second cleatCall to also fail with retrySimulation=2, "
            + "but got success: " + second.getValue());

        // Third call should succeed (stub takes over)
        CleatResult<String> third = host.cleatCall("svc", "op", "req");
        assertTrue(third.isOk(),
            "Expected third cleatCall to succeed after retrySimulation=2 is exhausted, "
            + "but got error: " + third.getError());
        assertEquals("success-response", third.getValue(),
            "Expected third call to return the stubbed response");
    }

    @Test
    void testRetrySimulationResetsOnSetRetrySimulation() {
        // WHAT: Verify setRetrySimulation clears previous attempt state
        // WHY: Calling setRetrySimulation should reset the attempt counter

        host.setRetrySimulation(1);
        assertTrue(host.cleatCall("svc", "op", "req").isErr(),
            "Expected first call to fail with retrySimulation=1");

        // Reset and set to 0 — no more retry simulation
        host.setRetrySimulation(0);
        host.registerCallStub("svc", "op", "ok");
        CleatResult<String> result = host.cleatCall("svc", "op", "req");
        assertTrue(result.isOk(),
            "Expected cleatCall to succeed after setRetrySimulation(0), "
            + "but got error: " + result.getError());
    }

    // ======================================================================
    // 29. reset
    // ======================================================================

    @Test
    void testResetReturnsToCleanState() {
        // WHAT: Verify reset() returns TestHostCalls to its initial state
        // WHY: Between-test isolation depends on complete state cleanup

        // Perform various operations to dirty the state
        host.registerCallStub("svc", "op", "resp");
        host.cleatCall("svc", "op", "req");
        host.setTime(999999L);
        host.setVersion(5);
        host.setMinVersion(3);
        host.setWorkflowId("custom-id");
        host.setRunId("custom-run");
        host.setRandomSeq(new long[]{1L, 2L});
        host.setScope("Order", "ord-1");
        host.setState("key", "val");
        host.setQueryState("qkey", "qval");
        host.createPromise("test-prom");
        host.deliverSignal("sig", "{}");
        host.cleatDefer("defer-me");
        host.setRetrySimulation(3);
        host.setCancelled(true, "reason");

        // Reset
        host.reset();

        // 1. Default time
        assertEquals(1704067200000L, host.now(),
            "Expected now() to return initial timestamp after reset, "
            + "but got: " + host.now());

        // 2. Default versions
        assertEquals(1, host.version(),
            "Expected version() to return 1 after reset, but got: " + host.version());
        assertEquals(1, host.minVersion(),
            "Expected minVersion() to return 1 after reset, but got: " + host.minVersion());

        // 3. Default workflow/run IDs
        assertEquals("test-workflow", host.currentWorkflowId(),
            "Expected currentWorkflowId() to return 'test-workflow' after reset, "
            + "but got: [" + host.currentWorkflowId() + "]");
        assertEquals("test-run-001", host.currentRunId(),
            "Expected currentRunId() to return 'test-run-001' after reset, "
            + "but got: [" + host.currentRunId() + "]");

        // 4. No call history
        assertFalse(host.assertCalled("svc", "op"),
            "Expected assertCalled to return false after reset, "
            + "but call history was not cleared");

        // 5. No state
        assertFalse(host.hasState("key"),
            "Expected hasState('key') to return false after reset");

        // 6. No query state
        assertTrue(host.getQueryState("qkey").isErr(),
            "Expected getQueryState to return error after reset");

        // 7. No random sequence
        assertEquals(0L, host.random(),
            "Expected random() to return 0 after reset");

        // 8. No cancellation
        assertFalse(host.pollCancellation().getValue(),
            "Expected pollCancellation to return false after reset");

        // 9. Empty scope
        String[] scope = host.getScope();
        assertEquals("", scope[0],
            "Expected scope to be cleared after reset");

        // 10. No stubs -> calls should fail
        assertTrue(host.cleatCall("svc", "op", "req").isErr(),
            "Expected cleatCall to fail after reset since stubs were cleared");

        // 11. No retry simulation -> should fall through to stub logic (no stub = error)
        assertTrue(host.cleatCall("any", "op", "req").isErr(),
            "Expected cleatCall to fail after reset with no stubs");
    }

    // ======================================================================
    // 30. currentWorkflowId / currentRunId
    // ======================================================================

    @Test
    void testCurrentWorkflowIdDefaults() {
        // WHAT: Verify currentWorkflowId returns the default workflow ID
        // WHY: Workflow identity is needed for logging, signals, and child workflows

        assertEquals("test-workflow", host.currentWorkflowId(),
            "Expected default currentWorkflowId() to return 'test-workflow', "
            + "but got: [" + host.currentWorkflowId() + "]");
    }

    @Test
    void testSetWorkflowIdChangesReturnValue() {
        host.setWorkflowId("my-custom-workflow");
        assertEquals("my-custom-workflow", host.currentWorkflowId(),
            "Expected currentWorkflowId() to return the set value, "
            + "but got: [" + host.currentWorkflowId() + "]");
    }

    @Test
    void testCurrentRunIdDefaults() {
        assertEquals("test-run-001", host.currentRunId(),
            "Expected default currentRunId() to return 'test-run-001', "
            + "but got: [" + host.currentRunId() + "]");
    }

    @Test
    void testSetRunIdChangesReturnValue() {
        host.setRunId("my-custom-run");
        assertEquals("my-custom-run", host.currentRunId(),
            "Expected currentRunId() to return the set value, "
            + "but got: [" + host.currentRunId() + "]");
    }

    // ======================================================================
    // Additional edge cases
    // ======================================================================

    @Test
    void testConsecutiveCallStubsAreConsumed() {
        // WHAT: Verify each registered stub is consumed on first use
        // WHY: Stubs should be consumed once to properly simulate sequential call patterns

        host.registerCallStub("svc", "op", "first");
        host.registerCallStub("svc", "op", "second");

        assertEquals("first", host.cleatCall("svc", "op", "req1").getValue(),
            "Expected first cleatCall to consume the first stub and return 'first'");

        assertEquals("second", host.cleatCall("svc", "op", "req2").getValue(),
            "Expected second cleatCall to consume the second stub and return 'second'");

        assertTrue(host.cleatCall("svc", "op", "req3").isErr(),
            "Expected third cleatCall to fail because all stubs were consumed");
    }

    @Test
    void testCleatLogDoesNotThrow() {
        // cleatLog is a no-op in TestHostCalls, but should not throw
        host.cleatLog("test message");
        // No assertion — verifying no exception is the test
    }

    @Test
    void testRegisterUpdateHandlerDoesNotThrow() {
        host.registerUpdateHandler("myUpdate");
        // No-op should not throw
    }

    @Test
    void testRegisterQueryHandlerDoesNotThrow() {
        host.registerQueryHandler("myQuery");
        // No-op should not throw
    }

    @Test
    void testCleatSendRecordsCallInHistory() {
        CleatResult<Void> result = host.cleatSend("svc", "op", "req");
        assertTrue(result.isOk(),
            "Expected cleatSend to succeed, but got error: " + result.getError());

        assertTrue(host.assertCalled("svc", "op"),
            "Expected cleatSend to be recorded in call history, "
            + "but assertCalled returned false");
    }

    @Test
    void testScheduleInvokeDoesNotThrow() {
        CleatResult<Void> result = host.scheduleInvoke("svc", "op", "{}", 1000);
        assertTrue(result.isOk(),
            "Expected scheduleInvoke to succeed, but got error: " + result.getError());
    }

    @Test
    void testContinueAsNewSetsFlag() {
        CleatResult<Void> result = host.continueAsNew("{\"restart\":true}");
        assertTrue(result.isOk(),
            "Expected continueAsNew to succeed, but got error: " + result.getError());
    }

    @Test
    void testReadQueryStateReturnsSetValue() {
        host.setQueryState("readKey", "readVal");
        String val = host.readQueryState("readKey");
        assertEquals("readVal", val,
            "Expected readQueryState to return 'readVal', "
            + "but got: [" + val + "]");
    }

    @Test
    void testReadQueryStateReturnsNullForMissingKey() {
        assertNull(host.readQueryState("missing"),
            "Expected readQueryState to return null for a key that was never set");
    }

    @Test
    void testPluginCallReturnsStubbedResult() {
        host.registerPluginCallStub("blobstore", "get", "{\"data\":\"content\"}");
        CleatResult<String> result = host.pluginCall("blobstore", "get", "{}");
        assertTrue(result.isOk(),
            "Expected pluginCall to succeed when a stub is registered for blobstore.get, "
            + "but got error: " + result.getError());
        assertEquals("{\"data\":\"content\"}", result.getValue(),
            "Expected pluginCall to return stubbed result, "
            + "but got: [" + result.getValue() + "]");
    }

    @Test
    void testPluginCallWithoutStubReturnsError() {
        CleatResult<String> result = host.pluginCall("unknown", "func", "{}");
        assertTrue(result.isErr(),
            "Expected pluginCall to return error when no stub is registered for unknown.func, "
            + "but it succeeded with value: " + result.getValue());
        assertTrue(result.getError().contains("no stub registered"),
            "Expected error message to mention 'no stub registered', "
            + "but got: [" + result.getError() + "]");
    }

    @Test
    void testPollSignalReturnsDeliveredSignal() {
        host.deliverSignal("my_signal", "payload-data");

        CleatResult<String> result = host.pollSignal("my_signal");
        assertTrue(result.isOk(),
            "Expected pollSignal to find 'my_signal' after deliverSignal, "
            + "but got error: " + result.getError());
        assertEquals("payload-data", result.getValue(),
            "Expected pollSignal payload to match delivered signal");
    }

    @Test
    void testPollSignalForUndeliveredSignalReturnsError() {
        CleatResult<String> result = host.pollSignal("nonexistent");
        assertTrue(result.isErr(),
            "Expected pollSignal to return error for undelivered signal");
        assertTrue(result.getError().contains("signal not found"),
            "Expected error to mention 'signal not found', "
            + "but got: [" + result.getError() + "]");
    }

    @Test
    void testAwaitAllChildrenReturnsResults() {
        CleatResult<String> child1 = host.childWorkflow("wf1", "{}");
        CleatResult<String> child2 = host.childWorkflow("wf2", "{}");

        CleatResult<String> result = host.awaitAllChildren(
            new String[]{child1.getValue(), child2.getValue()});

        assertTrue(result.isOk(),
            "Expected awaitAllChildren to succeed, but got error: " + result.getError());
        String resultJson = result.getValue();
        assertTrue(resultJson.contains(child1.getValue()),
            "Expected awaitAllChildren result to contain runId of child1, "
            + "but got: [" + resultJson + "]");
        assertTrue(resultJson.contains(child2.getValue()),
            "Expected awaitAllChildren result to contain runId of child2, "
            + "but got: [" + resultJson + "]");
    }
}
