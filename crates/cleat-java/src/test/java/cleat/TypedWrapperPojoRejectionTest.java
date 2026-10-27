package cleat;

import static org.junit.jupiter.api.Assertions.*;

import org.junit.jupiter.api.Test;

/**
 * cleat#3204: {@code childWorkflowTyped}/{@code awaitChildTyped}/
 * {@code cleatCallTyped}/{@code cleatCallWithRetry}/{@code pluginCallTyped}
 * are documented as typed convenience wrappers, but {@link JsonHelper}'s
 * (de)serialization underneath them only supports {@code String},
 * {@code Map}, {@code List}, boxed primitives, {@code Boolean} and
 * {@code Number} -- never an arbitrary POJO. Every existing test exercising
 * these five methods (AllHostCallsCompileTest, CleatTestEnvTest) passes only
 * {@code String}/{@code String.class}, so none of them had ever actually
 * been called with a real custom type.
 * <p>
 * This asserts the DOCUMENTED contract (HostCalls.java's doc comments, fixed
 * in this same change) rather than merely pinning whatever the code happens
 * to do: a test added before narrowing the doc comments would pin the
 * defect as if it were the promise, and read as coverage for a bug.
 * <p>
 * The failure mode is not uniform, and each assertion below is for the
 * mechanism actually present, not a guessed common one -- confirmed by
 * running each method against a real POJO before writing the assertion:
 * the four methods whose POJO argument is the OUTGOING request/input throw
 * {@link IllegalArgumentException} directly (serialization runs before any
 * host call, so nothing catches it), while {@code awaitChildTyped}'s POJO
 * argument is the type to deserialize the INCOMING result into -- by the
 * time that runs, {@code awaitChild} has already succeeded, and the parse
 * failure is caught and returned as {@link CleatResult#err}.
 */
class TypedWrapperPojoRejectionTest {

    /** A real custom type -- not String, Map, List, or a boxed primitive. */
    static class RealPojo {
        String field = "value";
    }

    @Test
    void childWorkflowTypedThrowsForARealPojoInput() {
        HostCalls h = new HostCalls();
        IllegalArgumentException e = assertThrows(IllegalArgumentException.class,
                () -> h.childWorkflowTyped("child", new RealPojo()));
        assertTrue(e.getMessage().contains("RealPojo"),
                "expected the message to name the unsupported type, got: " + e.getMessage());
    }

    @Test
    void cleatCallTypedThrowsForARealPojoRequest() {
        HostCalls h = new HostCalls();
        IllegalArgumentException e = assertThrows(IllegalArgumentException.class,
                () -> h.cleatCallTyped("svc", "op", new RealPojo(), String.class));
        assertTrue(e.getMessage().contains("RealPojo"),
                "expected the message to name the unsupported type, got: " + e.getMessage());
    }

    @Test
    void cleatCallWithRetryThrowsForARealPojoRequest() {
        HostCalls h = new HostCalls();
        IllegalArgumentException e = assertThrows(IllegalArgumentException.class,
                () -> h.cleatCallWithRetry("svc", "op", new RealPojo(), String.class,
                        new HostCalls.RetryPolicy(3, 100L, 2.0, 5000L, new String[0])));
        assertTrue(e.getMessage().contains("RealPojo"),
                "expected the message to name the unsupported type, got: " + e.getMessage());
    }

    @Test
    void pluginCallTypedThrowsForARealPojoInput() {
        HostCalls h = new HostCalls();
        IllegalArgumentException e = assertThrows(IllegalArgumentException.class,
                () -> h.pluginCallTyped("p", "f", new RealPojo(), String.class));
        assertTrue(e.getMessage().contains("RealPojo"),
                "expected the message to name the unsupported type, got: " + e.getMessage());
    }

    /**
     * awaitChildTyped does NOT throw for an unsupported response class --
     * its JsonHelper.parse call is wrapped in a try/catch that converts the
     * failure to CleatResult.err, because by the time it runs,
     * awaitChild(runID) has already completed successfully. Driven through
     * CleatTestEnv so awaitChild() itself succeeds without a real WASM host.
     */
    @Test
    void awaitChildTypedReturnsErrForARealPojoResultClass() {
        CleatTestEnv env = new CleatTestEnv();
        String outcome = env.execute(h -> {
            CleatResult<String> started = h.childWorkflowTyped("child", "{}");
            if (started.isErr()) {
                return "start-err:" + started.getError();
            }
            CleatResult<RealPojo> awaited = h.awaitChildTyped(started.getValue(), RealPojo.class);
            if (awaited.isErr()) {
                return "err:" + awaited.getError();
            }
            return "ok";
        });
        assertTrue(outcome.startsWith("err:"),
                "expected awaitChildTyped to return a CleatResult.err for an unsupported class, got: " + outcome);
        assertTrue(outcome.contains("RealPojo"),
                "expected the error to name the unsupported type, got: " + outcome);
    }
}
