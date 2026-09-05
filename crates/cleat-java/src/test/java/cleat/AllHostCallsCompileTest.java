package cleat;

import static org.junit.jupiter.api.Assertions.*;

import java.util.HashMap;
import java.util.Map;
import org.junit.jupiter.api.Test;

/**
 * Every method on {@link HostCalls}, called once, so the whole surface is
 * compiled.
 *
 * <p>This exists to be COMPILED, not run. {@code exerciseEveryHostCall} is
 * never invoked: each call would trap without a host, and several of them
 * ({@code continueAsNew}, {@code releaseLock}) would change a running
 * workflow's fate. The assertion is that {@code javac} accepts every call and
 * the JVM verifies the resulting bytecode when the class loads.
 *
 * <p>Why it is needed: {@code examples/java-workflow} and the plugin-harness
 * fixture between them called 8 of 68 methods, so passing the Java tests meant
 * "a Java workflow builds", not "the Java host-call surface builds". The Go SDK
 * had exactly that hole and four host calls shipped uncompilable through it --
 * locks, promises and side effects could not be built from any workflow. See
 * IMPROVEMENT-PLAN.md 3.204, 3.206 and 3.208.
 *
 * <p>This is a compile check, not a behaviour check. It says every method
 * exists with the signature a workflow can call; it says nothing about what the
 * method does, and nothing about the TeaVM WASM codegen, which the
 * plugin-harness tests cover. Both matter and they are different questions:
 * 3.303 found 16 of 17 plugin calls failing while every language test passed.
 *
 * <p>When adding a host call, add it here.
 * {@code scripts/sdk-host-call-coverage.py} reports the gap and fails when it
 * grows.
 */
class AllHostCallsCompileTest {

    /** Calls every HostCalls method. Never invoked. */
    @SuppressWarnings("unused")
    private static void exerciseEveryHostCall(HostCalls h) {
        String[] names = {"s"};
        String[] runIDs = {"run-1"};
        Map<String, String> headers = new HashMap<>();

        // ---- durable calls ----
        h.call("svc", "op", "{}");
        h.cleatCall("svc", "op", "{}");
        h.cleatCallHeartbeat("svc", "op", "{}", 1000L);
        h.cleatCallTyped("svc", "op", "{}", String.class);
        h.cleatCallWithRetry("svc", "op", "{}", String.class, new HostCalls.RetryPolicy(3, 100L, 2.0, 5000L, new String[0]));
        h.cleatSend("svc", "op", "{}");
        h.scheduleInvoke("svc", "op", "{}", 1L);
        h.scheduleInvokeMs("svc", "op", "{}", 1000L);
        h.runDetached("wf", "{}");

        // ---- sleep, identity, logging ----
        h.cleatSleep(1L);
        h.cleatSleepMs(1000L);
        h.now();
        h.random();
        h.cleatLog("m");
        h.version();
        h.minVersion();
        h.uuid("seed");
        h.currentWorkflowId();
        h.currentRunId();

        // ---- scope ----
        h.setScope("obj", "key");
        h.getScope();
        h.clearScope();

        // ---- defer, lifecycle ----
        h.cleatDefer("cleanup");
        h.deferFunc(() -> {});
        h.continueAsNew("{}");
        h.continueAsNewVersioned("{}", 2);
        h.sideEffect("computed");

        // ---- signals ----
        h.pollCancellation();
        h.pollSignal("s");
        h.awaitSignals(names, 1L);
        h.awaitSignalsMs(names, 1000L);
        h.awaitSignalsWithQuorum(names, 1, 0, 1L);
        h.awaitSignalsWithQuorumMs(names, 1, 0, 1000L);
        h.signalWorkflow("run-1", "s", "{}");
        h.sendSignalAndWait("run-1", "s", "{}", 1L);
        h.sendSignalAndWaitMs("run-1", "s", "{}", 1000L);
        h.replyToSignal("corr", "{}");

        // ---- children ----
        h.childWorkflow("child", "{}");
        h.childWorkflowWithOptions("child", "{}", 0L, "abandon");
        h.childWorkflowTyped("child", "{}");
        h.awaitChild("run-1");
        h.awaitChildTyped("run-1", String.class);
        h.awaitAllChildren(runIDs);
        h.awaitAnyChild(runIDs);
        h.pollChild("run-1");

        // ---- promises ----
        h.createPromise("p");
        h.awaitPromise("prom-1", 1L);
        h.awaitPromiseMs("prom-1", 1000L);
        h.resolvePromise("prom-1", "v");
        h.rejectPromise("prom-1", "e");

        // ---- state ----
        h.setQueryState("k", "v");
        h.setState("k", "v");
        h.getState("k");
        h.deleteState("k");
        h.incrState("k", 1L);
        h.hasState("k");
        h.listState("pre");

        // ---- locks ----
        h.acquireLock("k", 1L);
        h.acquireLockMs("k", 1000L);
        h.releaseLock("k");

        // ---- http ----
        h.cleatFetch("GET", "http://x", headers, "");
        h.fetchGet("http://x");
        h.fetchGetJson("http://x");
        h.fetchGetJson("http://x", headers);

        // ---- plugins ----
        h.pluginCall("p", "f", "{}");
        h.pluginCallStreaming("p", "f", "{}");
        h.pluginCallTyped("p", "f", "{}", String.class);
        h.pluginCallOutcome("p", "f", "{}");

        // ---- updates, json helpers ----
        h.registerUpdateHandler("upd");
        h.jsonParse("{}");
        h.jsonStringify("\"v\"");
    }

    /**
     * Loading this class verifies the bytecode for every call above.
     *
     * <p>The compile is the assertion; this test is what makes the JVM do the
     * loading and verification, and what fails visibly if the method is ever
     * removed rather than silently dropping the coverage.
     */
    @Test
    void everyHostCallCompilesAndVerifies() throws Exception {
        assertNotNull(
            AllHostCallsCompileTest.class.getDeclaredMethod("exerciseEveryHostCall", HostCalls.class),
            "exerciseEveryHostCall is the whole point of this class; if it is gone, "
                + "the Java host-call surface is no longer compiled anywhere");
    }
}
