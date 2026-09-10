package cleat;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.junit.jupiter.api.Assertions.fail;

import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;
import java.util.Map;

import org.junit.jupiter.api.DynamicTest;
import org.junit.jupiter.api.TestFactory;

/**
 * The shared quorum cases, run against the Java SDK.
 *
 * <p>cleat#1132 was one defect written five times: every SDK passed the FULL
 * name set to the host on every iteration, so a quorum counted deliveries
 * rather than distinct voters. Go was fixed in #1135 and the other four stayed
 * wrong, because nothing compared the SDKs.
 *
 * <p>The reason no Java test could have caught it is structural: every method
 * on {@link HostCalls} calls an {@code extern "C"} import that exists only
 * inside a cleat WASM runtime, so the loop was unreachable from this language.
 * {@link HostCalls#quorumOver} exists to make it reachable, and this file
 * drives it against a scripted host with no runtime at all.
 *
 * <p>What it asserts, and the second half is the one that matters: not only the
 * outcome, but <em>what the loop asked for on each iteration</em>. The
 * narrowing is the mechanism; a test that checks only the result passes against
 * an implementation that fails for an unrelated reason.
 */
class QuorumConformanceTest {

    /**
     * Maps this SDK's wording onto the table's semantic tag.
     *
     * <p>The table carries no message text — five SDKs word these differently
     * and always will — so this is the only place the Java tests know a message
     * string. An unrecognised message returns a tagged value rather than a
     * guess, so a reworded error fails loudly here instead of silently
     * matching the wrong kind.
     */
    private static String kindOf(String message) {
        if (message.contains("unsatisfiable")) {
            return "unsatisfiable";
        }
        if (message.contains("not among them")) {
            return "out_of_set";
        }
        if (message.contains("quorum timeout")) {
            return "timeout";
        }
        if (message.contains("exceeded max rejections")) {
            return "rejections";
        }
        return "UNCLASSIFIED(" + message + ")";
    }

    @SuppressWarnings("unchecked")
    private static List<String> stringList(Object o) {
        List<String> out = new ArrayList<>();
        if (o instanceof List) {
            for (Object e : (List<Object>) o) {
                out.add(String.valueOf(e));
            }
        }
        return out;
    }

    @SuppressWarnings("unchecked")
    private static List<List<String>> setsOf(Object o) {
        List<List<String>> out = new ArrayList<>();
        if (o instanceof List) {
            for (Object e : (List<Object>) o) {
                out.add(stringList(e));
            }
        }
        return out;
    }

    private static int intOf(Object o, int fallback) {
        if (o instanceof Number) {
            return ((Number) o).intValue();
        }
        return fallback;
    }

    @TestFactory
    @SuppressWarnings("unchecked")
    List<DynamicTest> everyCaseInTheSharedQuorumTableHolds() throws Exception {
        // The gradle project dir is crates/cleat-java; the table is repo-level
        // because it belongs to no single SDK.
        Path table = Paths.get("..", "..", "tests", "conformance", "quorum_cases.json");
        assertTrue(Files.isReadable(table),
            "the shared table must be readable at " + table.toAbsolutePath());

        Map<String, Object> doc = JsonHelper.parseObject(new String(Files.readAllBytes(table)));
        List<Object> cases = (List<Object>) doc.get("cases");
        assertTrue(cases != null && !cases.isEmpty(),
            "an empty table would pass vacuously");

        List<DynamicTest> tests = new ArrayList<>();
        for (Object raw : cases) {
            Map<String, Object> tc = (Map<String, Object>) raw;
            tests.add(DynamicTest.dynamicTest(String.valueOf(tc.get("name")), () -> runCase(tc)));
        }
        return tests;
    }

    @SuppressWarnings("unchecked")
    private void runCase(Map<String, Object> tc) {
        List<String> names = stringList(tc.get("signal_names"));
        int minCount = intOf(tc.get("min_count"), 0);
        int maxRejections = intOf(tc.get("max_rejections"), -1);
        List<String> deliveries = stringList(tc.get("deliveries"));
        Map<String, Object> payloads = tc.get("payloads") instanceof Map
            ? (Map<String, Object>) tc.get("payloads")
            : java.util.Collections.emptyMap();
        boolean polite = "polite".equals(String.valueOf(tc.get("host")));
        Map<String, Object> expect = (Map<String, Object>) tc.get("expect");

        final List<List<String>> asked = new ArrayList<>();
        final int[] cursor = {0};

        // A polite host hands over a queued name only while that name is still
        // awaited, which is what the engine's own DurableAwaitSignals does --
        // it exercises the NARROWING. An impolite one hands over whatever is
        // queued and exercises the OUT-OF-SET GUARD. A fix resting only on the
        // polite case rests on the host's manners.
        java.util.function.BiFunction<List<String>, Long,
            CleatResult<HostCalls.AwaitSignalsResult>> stub = (want, waitMs) -> {
                asked.add(new ArrayList<>(want));
                while (cursor[0] < deliveries.size()) {
                    String name = deliveries.get(cursor[0]);
                    cursor[0]++;
                    if (!polite || want.contains(name)) {
                        Object p = payloads.get(name);
                        String payload = p == null ? "{\"ok\":true}" : String.valueOf(p);
                        return CleatResult.ok(
                            new HostCalls.AwaitSignalsResult(name, payload, false));
                    }
                }
                return CleatResult.ok(new HostCalls.AwaitSignalsResult("", "", true));
            };

        String[] callerArray = names.toArray(new String[0]);
        List<String> callerSnapshot = new ArrayList<>(names);

        // A minute, so the deadline is never what ends a case: the host's own
        // timed-out reply is the only source of a timeout, and the assertions
        // are about the loop rather than about the clock.
        CleatResult<List<HostCalls.AwaitSignalsResult>> got = HostCalls.quorumOver(
            callerArray, minCount, maxRejections, () -> 60_000L, stub);

        String outcome = String.valueOf(expect.get("outcome"));
        if ("ok".equals(outcome)) {
            assertFalse(got.isErr(), () -> "expected success, got: " + got.getError());
            List<String> gotNames = new ArrayList<>();
            for (HostCalls.AwaitSignalsResult r : got.getValue()) {
                gotNames.add(r.signalName);
            }
            assertEquals(stringList(expect.get("result_names")), gotNames, "result names");
        } else if ("error".equals(outcome)) {
            if (!got.isErr()) {
                List<String> gotNames = new ArrayList<>();
                for (HostCalls.AwaitSignalsResult r : got.getValue()) {
                    gotNames.add(r.signalName);
                }
                fail("expected an error, got " + gotNames);
            }
            assertEquals(String.valueOf(expect.get("error_kind")), kindOf(got.getError()),
                "failed for the wrong reason: " + got.getError());
        } else {
            fail("unknown expected outcome " + outcome);
        }

        // The narrowing is the mechanism, and this is the assertion that sees
        // it. Checking only the outcome passes against an implementation that
        // fails for an unrelated reason.
        assertEquals(setsOf(expect.get("awaited_sets")), asked, "the sets it awaited");

        if (Boolean.TRUE.equals(expect.get("caller_set_unchanged"))) {
            assertEquals(callerSnapshot, Arrays.asList(callerArray),
                "the caller's array was edited");
        }
    }
}
