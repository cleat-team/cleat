package cleat;

import java.util.ArrayList;
import java.util.List;

/**
 * A simple saga (compensating transaction) framework for cleat workflows.
 * <p>
 * A saga coordinates a sequence of steps, each with a forward action and a
 * compensating action.  If any forward step fails, all previously completed
 * steps are compensated in reverse order.
 * <p>
 * Usage:
 * <pre>{@code
 * Saga saga = new Saga()
 *     .addStep("reserve inventory",
 *         h -> h.durableCall("inv", "Reserve", input).getValue(),
 *         h -> h.durableCall("inv", "Release", input))
 *     .addStep("charge payment",
 *         h -> h.durableCall("pay", "Charge", input).getValue(),
 *         h -> h.durableCall("pay", "Refund", input));
 *
 * saga.run(hostCalls);
 * }</pre>
 */
public class Saga {

    @FunctionalInterface
    public interface SagaFunction {
        String apply(HostCalls h) throws Exception;
    }

    @FunctionalInterface
    public interface SagaCompensator {
        void accept(HostCalls h) throws Exception;
    }

    /**
     * A single saga step with a description, a forward action, and a
     * compensating action.
     */
    public static class Step {
        /** Human-readable description of this step. */
        public final String description;

        /** The forward action to execute. */
        public final SagaFunction forward;

        /** The compensating action to run on rollback. */
        public final SagaCompensator compensate;

        /**
         * Construct a new saga step.
         *
         * @param description human-readable description
         * @param forward     the forward action
         * @param compensate  the compensating action
         */
        public Step(String description, SagaFunction forward, SagaCompensator compensate) {
            this.description = description;
            this.forward = forward;
            this.compensate = compensate;
        }
    }

    private final List<Step> steps = new ArrayList<>();

    /**
     * Add a step to the saga.
     *
     * @param description human-readable description of the step
     * @param forward     the forward action
     * @param compensate  the compensating action
     * @return {@code this} for chaining
     */
    public Saga addStep(String description, SagaFunction forward, SagaCompensator compensate) {
        steps.add(new Step(description, forward, compensate));
        return this;
    }

    /**
     * Execute all forward steps in order.  If any step throws an exception,
     * all previously completed steps are compensated in reverse order.
     *
     * @param h the HostCalls instance for this workflow execution
     * @throws Exception if a forward step fails and compensation completes
     * @throws RuntimeException if compensation itself fails (the original
     *         forward exception is added as a suppressed exception)
     */
    public void run(HostCalls h) throws Exception {
        List<Step> completed = new ArrayList<>();

        try {
            for (Step step : steps) {
                step.forward.apply(h);
                completed.add(step);
            }
        } catch (Exception e) {
            // Compensate completed steps in reverse order.
            Exception compensateFailure = null;
            for (int i = completed.size() - 1; i >= 0; i--) {
                try {
                    completed.get(i).compensate.accept(h);
                } catch (Exception ce) {
                    if (compensateFailure == null) {
                        compensateFailure = ce;
                    } else {
                        compensateFailure.addSuppressed(ce);
                    }
                }
            }

            if (compensateFailure != null) {
                RuntimeException rte = new RuntimeException(
                    "saga compensation failed for step '" + completed.get(completed.size() - 1).description
                        + "'; original forward error: " + e.getMessage(), e);
                rte.addSuppressed(compensateFailure);
                throw rte;
            }

            throw e;
        }
    }
}
