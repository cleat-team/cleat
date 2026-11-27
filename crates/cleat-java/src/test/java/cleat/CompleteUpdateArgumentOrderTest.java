package cleat;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;

import org.junit.jupiter.api.Test;

/**
 * cleat#4524. {@code HostCalls.completeUpdate} passed {@code packStrings}' output to the {@code cleat_complete_update} import as it came,
 * {@code [off, off, off, len, len, len]}, where the import takes {@code (ptr, len) x 3}. The host then read the request id with the result's offset as
 * its length and found nothing, the update stayed pending, and the dispatch loop handled the same update again as fast as it could be polled. Every
 * Java workflow with an update handler hung.
 *
 * <p>{@code UpdateDispatchTest} could not see it: it runs against {@link TestHostCalls}, whose own {@code completeUpdate} never reaches the import. The
 * import cannot be called off a WASM guest, so this goes through {@link HostCalls#completeUpdateImport}, the one place the six ints are put in order, with
 * a recorder in the import's place. The offsets and lengths below are all different, so a swap of any two shows.
 */
class CompleteUpdateArgumentOrderTest {

    /** What packStrings returns for ("req-1", "{\"ok\":true}", ""): offsets first, then lengths. */
    private static final int[] PACKED = {1000, 1005, 1017, 5, 12, 0};

    @Test
    void theImportIsHandedPtrLenPairsInTheOrderItTakesThem() {
        int[][] seen = new int[1][];
        long answer = HostCalls.completeUpdateImport(PACKED, (a, b, c, d, e, f) -> {
            seen[0] = new int[] {a, b, c, d, e, f};
            return 42L;
        });

        assertArrayEquals(new int[] {1000, 5, 1005, 12, 1017, 0}, seen[0],
            "cleat_complete_update takes (ptr, len) x 3: request id, result, error. Got the offsets first and the lengths after, "
                + "which the host reads as a request id of the wrong length and leaves the update pending");
        org.junit.jupiter.api.Assertions.assertEquals(42L, answer, "the import's answer is the call's answer");
    }

    @Test
    void theOrderIsTheOneCleatCallUsesForItsThreeStrings() {
        // cleatCall unpacks [svcOff, opOff, reqOff, svcLen, opLen, reqLen] into (svcOff, svcLen, opOff, opLen, reqOff, reqLen). The same three-string
        // pack has to come out the same way for every import that takes (ptr, len) pairs.
        int[] p = {10, 20, 30, 1, 2, 3};
        int[][] seen = new int[1][];
        HostCalls.completeUpdateImport(p, (a, b, c, d, e, f) -> {
            seen[0] = new int[] {a, b, c, d, e, f};
            return 0L;
        });
        assertArrayEquals(new int[] {p[0], p[3], p[1], p[4], p[2], p[5]}, seen[0]);
    }
}
