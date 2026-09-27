package cleat;

import static org.junit.jupiter.api.Assertions.*;
import org.junit.jupiter.api.Test;

/**
 * The guest half of the defer-segment stop sentinel.
 *
 * <p>When a workflow is replayed as a defer segment — its terminal outcome
 * already decided, the replay existing only to run its outstanding defers —
 * the host refuses any call that would start new work and sets bit 31 of the
 * result word instead. IMPROVEMENT-PLAN 3.84 and ABI.md.
 *
 * <p>Until this existed the Java SDK read that word through whichever layout
 * the call it made returns, and every one of those readings is a plausible
 * ordinary result. That is not a Java problem: {@code deferSegmentLanguages}
 * in {@code engine/engine.go} fails a segment closed for any SDK not known to
 * decode the sentinel, which is why Java could not run one at all.
 */
class SuspendStopBitTest {

    @Test
    void aStoppedResultThrowsSuspendSignal() {
        assertThrows(SuspendSignal.class,
            () -> Memory.throwIfStopped(Memory.SUSPEND_STOP_BIT));
    }

    @Test
    void anOrdinaryResultDoesNotThrow() {
        // A successful cleat_call: responseLen=1024 in bits 40-63, errCode=0.
        long ok = (1024L << 40);
        assertDoesNotThrow(() -> Memory.throwIfStopped(ok));
        assertEquals(1024, Memory.decodeCallResponseLen(ok));
    }

    @Test
    void aFailedResultDoesNotThrow() {
        // errCode=1 with an error message in the buffer is an ordinary failure,
        // not a stop. A guard that threw on any non-zero word would break every
        // error path in the SDK.
        long err = (12L << 40) | 1L;
        assertDoesNotThrow(() -> Memory.throwIfStopped(err));
        assertEquals(1, Memory.decodeCallErrCode(err));
    }

    /**
     * The reason {@code throwIfStopped} must be called BEFORE any field is
     * decoded, stated as a test rather than as a comment.
     *
     * <p>In the await-signals layout bit 31 lands inside the timed-out field.
     * So a caller that decoded first would read a stop as an ordinary timeout,
     * return {@code CleatResult.ok(...)} with {@code timedOut=true}, and the
     * workflow would carry on — doing the new work the defer segment exists to
     * prevent, with nothing to see.
     *
     * <p>If this assertion ever fails because the layout moved, the ordering
     * requirement has not gone away; it has moved to whichever field now
     * overlaps bit 31.
     */
    @Test
    void decodingFirstWouldReadAStopAsATimeout() {
        assertTrue(Memory.decodeAwaitTimedOut(Memory.SUSPEND_STOP_BIT),
            "bit 31 no longer lands in the await-signals timed-out field; "
            + "re-check which field it overlaps and update the ordering note "
            + "in Memory.throwIfStopped");
    }

    /**
     * Bit 31, not bit 62. The two sentinels travel in opposite directions and
     * confusing them is silent: {@link Memory#SUSPEND_SENTINEL} is what the
     * guest returns to the host from an export, and the host never sets it in a
     * result word.
     */
    @Test
    void theStopBitIsNotTheExportSuspendSentinel() {
        assertEquals(1L << 31, Memory.SUSPEND_STOP_BIT);
        assertEquals(1L << 62, Memory.SUSPEND_SENTINEL);
        assertEquals(0L, Memory.SUSPEND_STOP_BIT & Memory.SUSPEND_SENTINEL);
        assertDoesNotThrow(() -> Memory.throwIfStopped(Memory.SUSPEND_SENTINEL));
    }
}
