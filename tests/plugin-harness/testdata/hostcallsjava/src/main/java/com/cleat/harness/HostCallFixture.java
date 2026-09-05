package com.cleat.harness;

import cleat.CleatEntry;
import cleat.CleatResult;
import cleat.HostCalls;

/**
 * Java reference fixture for the host-call execution harness: it runs exactly
 * ONE host call per invocation and reports what that call did.
 *
 * <p>One call per invocation, not all 24 in one workflow, for the reason the Go
 * fixture states: a host call may SUSPEND, everything after it in the same
 * invocation then never runs, and calls that never ran are indistinguishable
 * from calls that ran and returned nothing. It also lets the harness redden on
 * one call in one language, which is the acceptance test for the design.
 *
 * <p>Arguments are the Go fixture's arguments wherever the SDKs allow it --
 * the same run ID, the same promise ID, the same signal name, the same
 * millisecond timeouts. That is deliberate: several rows assert the host's
 * suspend reason, which embeds those values, so a Java row and a Go row
 * asserting different text would not be comparable. Where Java's binding takes
 * seconds and Go's takes a Duration, the {@code Ms} variant is used for exactly
 * this reason.
 *
 * <p>TeaVM constraints shape the style: no streams, no String.format, no
 * reflection. JSON is built and parsed by hand for the same reason the sibling
 * Java workflow does it -- a JSON library is one more thing that must survive
 * tree-shaking.
 */
public class HostCallFixture {

    /** The child run ID every fixture uses, so suspend reasons are comparable. */
    private static final String CHILD_RUN_ID = "00000000-0000-0000-0000-000000000001";
    /** The promise ID every fixture uses. */
    private static final String PROMISE_ID = "00000000-0000-0000-0000-000000000002";
    private static final String SIGNAL_NAME = "harness-signal";

    // The name is snake_case, unlike the sibling Java workflow's
    // "CallAllPlugins", because Java exports the @CleatEntry name VERBATIM
    // while the Go, Rust and AssemblyScript SDKs snake_case theirs. The shared
    // harness calls one entry point, "exercise_host_call", for every language
    // (hostcall_harness_test.go, executeOneCall), so the Java fixture spells it
    // the way the harness asks for it. Getting this wrong does not fail the
    // build: it produces a module that instantiates and then traps with
    // `export "exercise_host_call" not found`.
    @CleatEntry(name = "exercise_host_call")
    public static String exerciseHostCall(HostCalls h, String input) {
        String call = extractCall(input);
        if (call.isEmpty()) {
            return emit("<none>", "error", "fixture: no \"call\" key in input: " + input);
        }
        try {
            return dispatch(h, call);
        } catch (RuntimeException e) {
            // A binding that throws is a real outcome and must not be
            // reported as the fixture crashing: the harness would see a failed
            // execution and lose which call caused it. SuspendSignal is NOT
            // caught here -- it must propagate so the engine reports the
            // suspension, which is a first-class status.
            return emit(call, "error", "threw " + e.getClass().getName() + ": " + e.getMessage());
        }
    }

    private static String dispatch(HostCalls h, String call) {
        // ---- children ----
        if (call.equals("ChildWorkflow")) {
            return result(call, h.childWorkflow("child-workflow", "{}"));
        }
        if (call.equals("ChildWorkflowWithOptions")) {
            return result(call, h.childWorkflowWithOptions("child-workflow", "{}", 0, ""));
        }
        if (call.equals("AwaitChild")) {
            return result(call, h.awaitChild(CHILD_RUN_ID));
        }
        if (call.equals("AwaitAllChildren")) {
            return result(call, h.awaitAllChildren(new String[] { CHILD_RUN_ID }));
        }
        if (call.equals("AwaitAnyChild")) {
            return result(call, h.awaitAnyChild(new String[] { CHILD_RUN_ID }));
        }
        if (call.equals("PollChild")) {
            return result(call, h.pollChild(CHILD_RUN_ID));
        }

        // ---- promises and signals ----
        if (call.equals("CreatePromise")) {
            return result(call, h.createPromise("harness-promise"));
        }
        if (call.equals("AwaitPromise")) {
            CleatResult<HostCalls.AwaitPromiseResult> r = h.awaitPromiseMs(PROMISE_ID, 10);
            if (r.isErr()) {
                return emit(call, "error", r.getError());
            }
            HostCalls.AwaitPromiseResult v = r.getValue();
            return emit(call, "ok", "timedOut=" + v.timedOut + " result=" + v.result);
        }
        if (call.equals("DurableAwaitSignals")) {
            CleatResult<HostCalls.AwaitSignalsResult> r =
                h.awaitSignalsMs(new String[] { SIGNAL_NAME }, 10);
            if (r.isErr()) {
                return emit(call, "error", r.getError());
            }
            HostCalls.AwaitSignalsResult v = r.getValue();
            return emit(call, "ok",
                "timedOut=" + v.timedOut + " signal=" + v.signalName + " payload=" + v.payload);
        }
        if (call.equals("PollSignal")) {
            return result(call, h.pollSignal(SIGNAL_NAME));
        }

        // ---- durable calls ----
        if (call.equals("DurableCall")) {
            return result(call, h.cleatCall("harness-service", "op", "{}"));
        }
        if (call.equals("DurableCallWithHeartbeat")) {
            return result(call, h.cleatCallHeartbeat("harness-service", "op", "{}", 1000));
        }
        if (call.equals("DurableCallWithRetry")) {
            // Typed on both ends: the Java binding is generic and offers no
            // string-in/string-out form, the same shape the Rust fixture
            // records. maxAttempts 1 so the row does not depend on backoff.
            HostCalls.RetryPolicy policy = new HostCalls.RetryPolicy(1, 1, 1.0, 1);
            return result(call,
                h.cleatCallWithRetry("harness-service", "op", "{}", String.class, policy));
        }

        // ---- defers ----
        if (call.equals("DurableDefer")) {
            return result(call, h.cleatDefer("harness-defer"));
        }
        if (call.equals("DurableDeferFunc")) {
            return result(call, h.deferFunc(new Runnable() {
                @Override
                public void run() {
                    // Registering the defer is what this row exercises; the
                    // body runs at workflow end, past this invocation.
                }
            }));
        }

        // ---- cron ----
        //
        // The Java SDK declares NO cron bindings -- `grep -rn cron` over
        // crates/cleat-java/src/main/java/cleat/ returns nothing. The host
        // exports cleat_schedule_cron and cleat_list_crons and the
        // AssemblyScript SDK binds both, so this is a guest-side gap and not a
        // host limitation. Reported as "unsupported" rather than "error"
        // because a binding that does not exist and a binding that ran and was
        // refused are different facts.
        if (call.equals("ScheduleCron")) {
            return emit(call, "unsupported", "no cleat_schedule_cron import in the Java SDK");
        }
        if (call.equals("ListCrons")) {
            return emit(call, "unsupported", "no cleat_list_crons import in the Java SDK");
        }

        // ---- plugins ----
        if (call.equals("PluginCall")) {
            // Both paths in one row, as the Go fixture does. llm.list_models is
            // the one plugin call that works with no tenant, so on its own it
            // exercises only the success path -- and §3.200, the defect this
            // harness is accepted against, lived entirely on the ERROR path:
            // the guest decoded the host's error length and threw the message
            // away. blobstore.put fails with the host's "no tenant context", so
            // the detail below carries the host's own text and a guest that
            // discards it changes this row and nothing else.
            CleatResult<String> okPath = h.pluginCall("llm", "list_models", "{}");
            if (okPath.isErr()) {
                return emit(call, "error", okPath.getError());
            }
            CleatResult<String> errPath =
                h.pluginCall("blobstore", "put", "{\"key\":\"k\",\"data\":\"aGk=\"}");
            if (errPath.isOk()) {
                return emit(call, "ok",
                    "ok=" + okPath.getValue() + " err=<none: blobstore.put unexpectedly succeeded>");
            }
            return emit(call, "ok", "ok=" + okPath.getValue() + " err=" + errPath.getError());
        }
        if (call.equals("PluginCallStreaming")) {
            return result(call,
                h.pluginCallStreaming("llm", "chat_stream",
                    "{\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"));
        }

        // ---- locks ----
        if (call.equals("AcquireLock")) {
            // Twice, as the Go fixture does: the SUCCESS path decodes a
            // host-computed bit rather than a buffer, so a guest returning a
            // constant true would compile and be silently wrong about holding
            // a lock.
            CleatResult<Boolean> first = h.acquireLockMs("harness-lock", 60000);
            if (first.isErr()) {
                return emit(call, "error", first.getError());
            }
            CleatResult<Boolean> second = h.acquireLockMs("harness-lock", 60000);
            if (second.isErr()) {
                return emit(call, "error", second.getError());
            }
            return emit(call, "ok", "first=" + first.getValue() + " second=" + second.getValue());
        }

        // ---- identity and control ----
        if (call.equals("WorkflowID")) {
            return emit(call, "ok", h.currentWorkflowId());
        }
        if (call.equals("RunID")) {
            return emit(call, "ok", h.currentRunId());
        }
        if (call.equals("PollCancellation")) {
            return result(call, h.pollCancellation());
        }
        if (call.equals("SideEffect")) {
            // Java's binding takes the already-computed value rather than a
            // closure, so this row asserts the value crossed the boundary and
            // does NOT prove the host suppressed a recomputation on replay --
            // which Go's closure form can. Stated so the row is not read as
            // stronger than it is.
            return result(call, h.sideEffect("side-effect-value"));
        }

        return emit(call, "error", "fixture: no case for call " + call);
    }

    /** Renders a CleatResult as an outcome, preserving the host's error text. */
    private static <T> String result(String call, CleatResult<T> r) {
        if (r.isErr()) {
            return emit(call, "error", r.getError());
        }
        T v = r.getValue();
        return emit(call, "ok", v == null ? "" : String.valueOf(v));
    }

    /**
     * Extracts the value of the "call" key without a JSON parser.
     *
     * <p>The input is written by the harness and is always {"call":"Name"}, so
     * this does not need to be a general parser -- but it must not silently
     * return a wrong key's value, so it anchors on the "call" key itself.
     */
    private static String extractCall(String input) {
        if (input == null) {
            return "";
        }
        int k = input.indexOf("\"call\"");
        if (k < 0) {
            return "";
        }
        int colon = input.indexOf(':', k);
        if (colon < 0) {
            return "";
        }
        int open = input.indexOf('"', colon);
        if (open < 0) {
            return "";
        }
        int close = input.indexOf('"', open + 1);
        if (close < 0) {
            return "";
        }
        return input.substring(open + 1, close);
    }

    private static String emit(String call, String status, String detail) {
        StringBuilder sb = new StringBuilder();
        sb.append("{\"call\":\"").append(escape(call))
          .append("\",\"status\":\"").append(escape(status))
          .append("\",\"detail\":\"").append(escape(detail))
          .append("\"}");
        return sb.toString();
    }

    /** Minimal JSON string escaping. The detail carries host error text, which contains quotes. */
    private static String escape(String s) {
        if (s == null) {
            return "";
        }
        StringBuilder sb = new StringBuilder();
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            if (c == '"') {
                sb.append("\\\"");
            } else if (c == '\\') {
                sb.append("\\\\");
            } else if (c == '\n') {
                sb.append("\\n");
            } else if (c == '\r') {
                sb.append("\\r");
            } else if (c == '\t') {
                sb.append("\\t");
            } else if (c < 0x20) {
                sb.append(' ');
            } else {
                sb.append(c);
            }
        }
        return sb.toString();
    }
}
