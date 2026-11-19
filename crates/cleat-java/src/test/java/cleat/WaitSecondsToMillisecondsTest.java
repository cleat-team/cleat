package cleat;

import static org.junit.jupiter.api.Assertions.assertEquals;

import org.junit.jupiter.api.Test;

/**
 * The seconds forms of {@code awaitSignals} and {@code awaitPromise} hand the host milliseconds, and the multiplication
 * wrapped (cleat#4144). The Javadoc says to use {@link Long#MAX_VALUE} for no timeout; {@code Long.MAX_VALUE * 1000}
 * is {@code -1000} in 64-bit arithmetic, a negative the host reads as "do not wait".
 */
class WaitSecondsToMillisecondsTest {
    @Test
    void anOrdinaryWaitIsMultipliedByAThousand() {
        assertEquals(60_000L, HostCalls.secondsToMs(60));
        assertEquals(1_000L, HostCalls.secondsToMs(1));
    }

    @Test
    void zeroAndNegativeAreReturnedAsTheyAre() {
        assertEquals(0L, HostCalls.secondsToMs(0));
        assertEquals(-5_000L, HostCalls.secondsToMs(-5));
    }

    @Test
    void theDocumentedNoTimeoutValueSaturatesInsteadOfWrapping() {
        // The old expression, for the record: it is negative.
        assertEquals(-1000L, Long.MAX_VALUE * 1000);
        assertEquals(Long.MAX_VALUE, HostCalls.secondsToMs(Long.MAX_VALUE));
        assertEquals(Long.MAX_VALUE, HostCalls.secondsToMs(Long.MAX_VALUE / 1000 + 1));
    }

    @Test
    void theLargestValueThatDoesNotOverflowIsExact() {
        long edge = Long.MAX_VALUE / 1000;
        assertEquals(edge * 1000, HostCalls.secondsToMs(edge));
    }
}
