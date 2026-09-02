package cleat;

import java.util.ArrayList;
import java.util.List;

/**
 * Guest-side defer registry.
 *
 * <p>IMPROVEMENT-PLAN 3.70 for Go, 3.73 for the other SDKs.
 * {@link HostCalls#cleatDefer(String)} sends a <em>description</em> across the
 * boundary and nothing else. The host records that a defer exists; no code
 * anywhere can run it, because there is no body to run. That was true of every
 * Java workflow while the SDK's own javadoc described "a deferred cleanup
 * callback" executed "in LIFO order, analogous to Go's {@code defer} or Java's
 * {@code try/finally}".
 *
 * <p>{@link HostCalls#deferFunc(Runnable)} is the one with a body. The guest
 * runs it itself, in the instance that holds it, at the moment the entry point
 * finishes.
 *
 * <p>The generated export wrapper drains the table on the success and error
 * paths but NOT on suspension: a suspended workflow has not exited, its defers
 * are still pending, and firing them at the first sleep would release locks a
 * workflow that is about to continue still holds. That distinction only became
 * expressible once Java could suspend at all (3.74).
 */
public final class Defer {

    /** Registered bodies, in registration order. A WASM guest is single-threaded. */
    private static final List<Runnable> BODIES = new ArrayList<>();

    /**
     * True while {@link #runDeferred()} is draining the table.
     *
     * <p>IMPROVEMENT-PLAN 3.35 phase 4. Two things a defer body must not do,
     * both measured 2026-09-02 before they were blocked, and both producing a
     * workflow that reported SUCCESS with a durable record that could not be
     * honoured:
     *
     * <ul>
     *   <li>registering another defer -- the table is drained BEFORE the first
     *       body runs, so the new entry lands in a list nobody walks again,
     *       while the host has already written a durable {@code defer} event
     *       for it;</li>
     *   <li>{@code continueAsNew} -- the host records the event AND the
     *       generated wrapper reports the already-decided result, so the worker
     *       stores {@code done} and the continuation is silently never
     *       taken.</li>
     * </ul>
     *
     * <p>Both guards run BEFORE the host call. Checking after would leave the
     * durable event behind, which is the defect rather than the fix.
     */
    private static boolean inDeferPhase = false;

    /** Reports whether defer bodies are currently running. */
    public static boolean inDeferPhase() {
        return inDeferPhase;
    }

    /** The message both refusals carry. */
    public static String deferPhaseRefusal(String what) {
        return "cleat: " + what + " is not allowed from inside a defer body: the defer "
            + "table is drained before the first body runs and the workflow's result is "
            + "already decided, so this would be recorded durably and never taken "
            + "(IMPROVEMENT-PLAN 3.35 phase 4)";
    }

    private Defer() {
        // static only
    }

    /**
     * Records a defer body. Called by {@link HostCalls#deferFunc(Runnable)}
     * after the host has minted an ID for it.
     *
     * @param body the cleanup to run; null is ignored
     */
    public static void register(Runnable body) {
        if (body != null) {
            BODIES.add(body);
        }
    }

    /**
     * Runs registered defer bodies in LIFO order and returns how many ran.
     *
     * <p>The table is drained BEFORE the first body runs, which makes this
     * idempotent: a second call runs nothing. That matters because a caller
     * cannot always tell whether the defers already ran, and cleanup that runs
     * twice releases a lock twice or refunds a charge twice.
     *
     * <p>LIFO is not cosmetic. A defer releases what the defer before it
     * acquired, so running them in registration order unwinds the workflow
     * inside-out.
     *
     * <p>An exception in one body does not stop the others and does not disturb
     * the workflow's result, which is already decided by the time this runs. The
     * one exception is {@link SuspendSignal}, which is not an error: it
     * propagates so the generated wrapper sees it and the segment suspends.
     * Swallowing it would complete a workflow the host has already recorded as
     * suspended.
     *
     * @return how many bodies ran
     */
    public static int runDeferred() {
        List<Runnable> taken = new ArrayList<>(BODIES);
        BODIES.clear();

        // try/finally, not a pair of assignments: SuspendSignal is rethrown out
        // of this method, and a flag left set would make the next segment's
        // first deferFunc refuse.
        inDeferPhase = true;
        try {
            int ran = 0;
            for (int i = taken.size() - 1; i >= 0; i--) {
                ran++;
                try {
                    taken.get(i).run();
                } catch (SuspendSignal e) {
                    throw e;
                } catch (RuntimeException | Error e) {
                    // One bad cleanup must not stop the rest.
                    continue;
                }
            }
            return ran;
        } finally {
            inDeferPhase = false;
        }
    }

    /**
     * Drains the table for the HOST, for a workflow that never reached the
     * generated wrapper that normally does it -- one killed by the execution
     * fence, the instruction limit, or an unrecoverable runtime failure.
     * IMPROVEMENT-PLAN 3.35 phase 4.
     *
     * <p>Safe to call on a guest that already ran its defers: the table is
     * drained, so a second call runs nothing and returns 0.
     *
     * <p>Every throwable is swallowed here, INCLUDING {@link SuspendSignal},
     * which {@link #runDeferred()} deliberately lets through. That is right for
     * this caller and wrong for the other: the wrapper needs the signal so its
     * segment suspends, but a workflow reached through this entry point is
     * already dead and has no segment left. Letting it out would turn the host's
     * cleanup call into a trap.
     *
     * @return how many bodies ran, or 0 if the drain itself failed
     */
    public static int runDeferredForHost() {
        try {
            return runDeferred();
        } catch (RuntimeException | Error e) {
            return 0;
        }
    }
}
