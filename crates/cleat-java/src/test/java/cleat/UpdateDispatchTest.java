package cleat;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.List;
import org.junit.jupiter.api.Test;

/**
 * Workflow updates in the Java SDK.
 *
 * <p>An update is a request/reply call into a running workflow: the only
 * external interaction that both changes workflow state and returns a value to
 * the caller. Before updates were implemented end to end, handlers were
 * registered into a map that nothing read, and a caller got a 202 with a
 * promise_id that nothing ever settled (IMPROVEMENT-PLAN 3.239, cleat#849).
 */
class UpdateDispatchTest {

    @Test
    void anUpdateRunsItsHandlerAndSettlesTheCallersPromise() {
        TestHostCalls h = new TestHostCalls();

        int[] total = {0};
        h.registerUpdateHandler("add",
            payload -> {
                total[0] += payload.length();
                return "{\"ok\":true}";
            },
            null);

        String promiseId = h.createPromise("caller").getValue();
        h.enqueueUpdate("add", "12345", promiseId);
        h.dispatchUpdates();

        assertEquals(5, total[0], "the handler did not change workflow state");

        List<TestHostCalls.UpdateOutcome> done = h.completedUpdates();
        assertEquals(1, done.size(), "expected exactly one completed update");
        assertEquals("add", done.get(0).name);
        assertEquals("{\"ok\":true}", done.get(0).result);
        assertEquals("", done.get(0).error);

        HostCalls.AwaitPromiseResult awaited = h.awaitPromise(promiseId, 1).getValue();
        assertFalse(awaited.timedOut,
            "the caller's promise never settled -- the defect updates were built to fix");
        assertEquals("{\"ok\":true}", awaited.result);
    }

    @Test
    void aValidatorRefusalNeverReachesTheHandler() {
        TestHostCalls h = new TestHostCalls();

        boolean[] ran = {false};
        h.registerUpdateHandler("approve",
            payload -> {
                ran[0] = true;
                return "approved";
            },
            payload -> "amount must be positive");

        String promiseId = h.createPromise("caller").getValue();
        h.enqueueUpdate("approve", "{\"amount\":-1}", promiseId);
        h.dispatchUpdates();

        assertFalse(ran[0], "the handler ran despite the validator refusing");

        List<TestHostCalls.UpdateOutcome> done = h.completedUpdates();
        assertEquals(1, done.size(), "a refused update must still answer the caller");
        assertFalse(done.get(0).error.isEmpty(),
            "a refused update completed with no error tells the caller it succeeded");

        assertTrue(h.awaitPromise(promiseId, 1).isErr()
                || !h.awaitPromise(promiseId, 1).getValue().result.isEmpty(),
            "a refused update must not leave the caller's promise pending");
    }

    @Test
    void anUnregisteredUpdateAnswersRatherThanHanging() {
        // The name is chosen by the CALLER, so it can name a handler this
        // workflow does not have. That must be an answer, not silence.
        TestHostCalls h = new TestHostCalls();

        String promiseId = h.createPromise("caller").getValue();
        h.enqueueUpdate("no-such-handler", "{}", promiseId);
        h.dispatchUpdates();

        List<TestHostCalls.UpdateOutcome> done = h.completedUpdates();
        assertEquals(1, done.size(), "an update naming no handler must still be completed");
        assertFalse(done.get(0).error.isEmpty(),
            "an update naming no handler must be completed with an error");
    }

    @Test
    void updatesAreNotDispatchedWithoutADispatchPoint() {
        // The negative control for the whole design. Dispatch happens at fixed
        // program positions, not on arrival. If this ever fails, dispatch has
        // become time- or host-driven and the interleaving replay depends on is
        // no longer a property of the program.
        TestHostCalls h = new TestHostCalls();

        boolean[] ran = {false};
        h.registerUpdateHandler("add", payload -> {
            ran[0] = true;
            return "";
        }, null);
        h.enqueueUpdate("add", "x", "");

        assertFalse(ran[0], "an update ran without the workflow reaching a dispatch point");
        assertEquals(0, h.completedUpdates().size(),
            "an update completed before any dispatch point");

        h.dispatchUpdates();
        assertTrue(ran[0], "dispatchUpdates did not dispatch");
    }

    @Test
    void everyPendingUpdateIsDrainedInOneDispatch() {
        // Dispatch loops until the queue is empty, so two updates arriving
        // between suspensions are both handled at the next one. Draining only
        // the first would leave the second waiting for a suspension that may
        // never come.
        TestHostCalls h = new TestHostCalls();

        StringBuilder seen = new StringBuilder();
        h.registerUpdateHandler("append", payload -> {
            seen.append(payload);
            return "ok";
        }, null);

        h.enqueueUpdate("append", "a", "");
        h.enqueueUpdate("append", "b", "");
        h.dispatchUpdates();

        assertEquals("ab", seen.toString(), "both pending updates must be handled, in order");
        assertEquals(2, h.completedUpdates().size());
    }
}
