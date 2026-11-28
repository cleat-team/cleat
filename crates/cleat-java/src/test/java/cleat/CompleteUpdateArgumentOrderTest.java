package cleat;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;

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
        assertEquals(42L, answer, "the import's answer is the call's answer");
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

    // ---- The call site. The two tests above pin completeUpdateImport; these pin that completeUpdate USES it. ----
    //
    // completeUpdate cannot run on a JVM (packStrings writes through a TeaVM Address), so its call site is read as source. The defect was a call site, and a
    // helper that is correct and not called leaves it byte for byte as it was: reviewed on cf3ea8d6 by putting develop's direct call back and leaving the helper right,
    // which kept the two tests above green.

    private static final Path HOST_CALLS = Paths.get("src", "main", "java", "cleat", "HostCalls.java");

    /** HostCalls.java with comments and string literals blanked, so a sentence about the import is not read as a use of it. */
    private static String code() throws IOException {
        String src = new String(Files.readAllBytes(HOST_CALLS), StandardCharsets.UTF_8);
        StringBuilder out = new StringBuilder(src.length());
        int i = 0;
        int n = src.length();
        while (i < n) {
            char c = src.charAt(i);
            if (c == '/' && i + 1 < n && src.charAt(i + 1) == '/') {
                while (i < n && src.charAt(i) != '\n') {
                    i++;
                }
            } else if (c == '/' && i + 1 < n && src.charAt(i + 1) == '*') {
                i += 2;
                while (i + 1 < n && !(src.charAt(i) == '*' && src.charAt(i + 1) == '/')) {
                    if (src.charAt(i) == '\n') {
                        out.append('\n');
                    }
                    i++;
                }
                i += 2;
            } else if (c == '"') {
                out.append('"');
                i++;
                while (i < n && src.charAt(i) != '"') {
                    i += src.charAt(i) == '\\' ? 2 : 1;
                }
                out.append('"');
                i++;
            } else {
                out.append(c);
                i++;
            }
        }
        return out.toString();
    }

    @Test
    void theRawImportIsNeverCalledDirectly() throws IOException {
        String code = code();
        String name = "cleatCompleteUpdateRaw";
        int declarations = 0;
        int references = 0;
        for (int at = code.indexOf(name); at >= 0; at = code.indexOf(name, at + name.length())) {
            String before = code.substring(Math.max(0, at - 16), at);
            if (before.endsWith("native long ")) {
                declarations++;
            } else if (before.endsWith("HostCalls::")) {
                references++;
            } else {
                throw new AssertionError("cleatCompleteUpdateRaw is used other than as the declaration or as HostCalls::cleatCompleteUpdateRaw handed to "
                    + "completeUpdateImport, at offset " + at + ": ..." + code.substring(Math.max(0, at - 30), Math.min(code.length(), at + 60)).replace('\n', ' ')
                    + ". A direct call passes packStrings' offsets and lengths in the wrong order (cleat#4524).");
            }
        }
        assertEquals(1, declarations, "the scan did not find the import's declaration, so it is not reading what it thinks it is");
        assertEquals(1, references, "the import must be reached exactly once, through completeUpdateImport");
    }

    @Test
    void completeUpdateHandsItsPackedStringsToTheOrderingFunction() throws IOException {
        String code = code();
        int start = code.indexOf("CleatResult<Void> completeUpdate(String requestId, String resultJSON, String errMsg) {");
        assertTrue(start >= 0, "completeUpdate was not found, so this test is not reading what it thinks it is");
        int end = code.indexOf("\n    }\n", start);
        String body = code.substring(start, end);
        assertTrue(body.contains("packStrings(requestId, resultJSON, errMsg)"), "completeUpdate must pack the request id, the result and the error in that order: " + body);
        assertTrue(body.contains("completeUpdateImport(p, HostCalls::cleatCompleteUpdateRaw)"),
            "completeUpdate must call the import through completeUpdateImport, which puts the six ints in (ptr, len) order: " + body);
    }
}
