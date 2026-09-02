package cleat;

import static org.junit.jupiter.api.Assertions.*;

import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

import java.util.ArrayList;
import java.util.List;

/**
 * Defer bodies actually run. IMPROVEMENT-PLAN 3.73.
 *
 * <p>{@link HostCalls#cleatDefer(String)} sends a description and nothing else,
 * so the host recorded that a defer existed and no code anywhere ran it --
 * while the javadoc described "a deferred cleanup callback" executed "in LIFO
 * order". {@link HostCalls#deferFunc(Runnable)} is the one with a body.
 */
class DeferTest {

    @BeforeEach
    void drainBetweenTests() {
        // The registry is static, so a leftover body from one test would run
        // inside the next and make its counts someone else's.
        Defer.runDeferredForHost();
    }

    @Test
    void runsBodiesInLifoOrder() {
        List<String> order = new ArrayList<>();
        Defer.register(() -> order.add("first"));
        Defer.register(() -> order.add("second"));
        Defer.register(() -> order.add("third"));

        assertEquals(3, Defer.runDeferred());
        // LIFO, not registration order. A defer releases what the one before it
        // acquired, so FIFO unwinds the workflow inside-out.
        assertEquals(List.of("third", "second", "first"), order);
    }

    @Test
    void drainingMakesASecondRunANoOp() {
        List<String> runs = new ArrayList<>();
        Defer.register(() -> runs.add("x"));

        assertEquals(1, Defer.runDeferred());
        assertEquals(0, Defer.runDeferred(), "the table was not drained");
        assertEquals(1, runs.size(),
            "cleanup ran twice; a lock would be released twice");
    }

    @Test
    void aThrowingBodyDoesNotStopTheOthers() {
        List<String> ran = new ArrayList<>();
        Defer.register(() -> ran.add("a"));
        Defer.register(() -> { throw new RuntimeException("cleanup blew up"); });
        Defer.register(() -> ran.add("c"));

        assertEquals(3, Defer.runDeferred());
        // "c" registered last so it runs first; "a" must still run after the
        // throwing one between them.
        assertEquals(List.of("c", "a"), ran);
    }

    /**
     * The two callers need OPPOSITE behaviour for a suspending defer, and this
     * pair pins the difference. runDeferred must let SuspendSignal out so the
     * generated wrapper suspends the segment; swallowing it there completes a
     * workflow the host has already recorded as suspended.
     */
    @Test
    void runDeferredLetsASuspendSignalThrough() {
        Defer.register(() -> { throw new SuspendSignal(); });

        assertThrows(SuspendSignal.class, Defer::runDeferred,
            "runDeferred swallowed SuspendSignal. The generated wrapper never "
            + "learns the segment suspended, so a suspended workflow is reported "
            + "as complete.");
    }

    /** ...and the host's drain must NOT let it out: a workflow reached that way
     * is already dead and has no segment left to suspend, so an escaping signal
     * would turn the host's cleanup call into a trap. */
    @Test
    void theHostDrainSwallowsASuspendSignal() {
        Defer.register(() -> { throw new SuspendSignal(); });

        assertDoesNotThrow(() -> Defer.runDeferredForHost(),
            "SuspendSignal escaped runDeferredForHost and would trap the host's "
            + "cleanup call on a workflow that is already dead");
    }

    @Test
    void theHostDrainIsSafeOnAnEmptyTable() {
        assertEquals(0, Defer.runDeferredForHost());
    }

    @Test
    void registeringNullIsIgnored() {
        Defer.register(null);
        assertEquals(0, Defer.runDeferred(),
            "a null body was counted as a defer that ran");
    }

    // ------------------------------------------------------------------
    // IMPROVEMENT-PLAN 3.35 phase 4: the defer-phase flag
    // ------------------------------------------------------------------

    /**
     * Asserted from INSIDE a body.
     *
     * <p>Checking from outside is exactly where the flag is always false, so a
     * test written there would pass against a flag that is never set at all.
     */
    @Test
    void inDeferPhaseIsTrueWhileABodyRuns() {
        java.util.List<Boolean> seen = new java.util.ArrayList<>();
        assertFalse(Defer.inDeferPhase(), "the flag must be clear before the drain");

        Defer.register(() -> seen.add(Defer.inDeferPhase()));
        assertEquals(1, Defer.runDeferred());

        assertEquals(java.util.List.of(Boolean.TRUE), seen,
            "a defer body must observe inDeferPhase() == true");
        assertFalse(Defer.inDeferPhase(),
            "the flag must be clear after the drain, or the next segment's first "
            + "deferFunc would be refused");
    }

    /**
     * The case a pair of plain assignments would get wrong: SuspendSignal is
     * rethrown out of runDeferred, so only try/finally clears the flag.
     */
    @Test
    void theFlagIsClearedWhenABodySuspends() {
        Defer.register(() -> {
            throw new SuspendSignal();
        });
        assertThrows(SuspendSignal.class, Defer::runDeferred);
        assertFalse(Defer.inDeferPhase());
    }

    @Test
    void theFlagIsClearedWhenABodyThrows() {
        Defer.register(() -> {
            throw new IllegalStateException("cleanup blew up");
        });
        assertEquals(1, Defer.runDeferred());
        assertFalse(Defer.inDeferPhase());
    }
}
