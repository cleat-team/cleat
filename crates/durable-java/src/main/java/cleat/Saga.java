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

    private final List<Step> steps;

    /**
     * Construct a new Saga with no steps.
     * <p>
     * Steps can be added via {@link #addStep(String, SagaFunction, SagaCompensator)}
     * for chaining, or use {@link #builder()} for the builder pattern.
     */
    public Saga() {
        this.steps = new ArrayList<>();
    }

    /**
     * Construct a new Saga with the given steps.
     *
     * @param steps the steps for this saga
     */
    private Saga(List<Step> steps) {
        this.steps = new ArrayList<>(steps);
    }

    // ========================================================================
    // Builder
    // ========================================================================

    /**
     * Create a new {@link SagaBuilder} for constructing a Saga.
     * <p>
     * Usage:
     * <pre>{@code
     * Saga saga = Saga.builder()
     *     .addStep("bookFlight",
     *         h -> bookFlight(h),
     *         h -> cancelFlight(h))
     *     .addStep("chargePayment",
     *         h -> chargePayment(h),
     *         h -> refundPayment(h))
     *     .build();
     * saga.execute(h);
     * }</pre>
     *
     * @return a new {@link SagaBuilder}
     */
    public static SagaBuilder builder() {
        return new SagaBuilder();
    }

    /**
     * Builder for constructing a {@link Saga} with a fluent API.
     */
    public static class SagaBuilder {
        private final List<Step> steps = new ArrayList<>();

        private SagaBuilder() {
        }

        /**
         * Add a step to the saga.
         *
         * @param description human-readable description of the step
         * @param forward     the forward action
         * @param compensate  the compensating action
         * @return {@code this} for chaining
         */
        public SagaBuilder addStep(String description, SagaFunction forward, SagaCompensator compensate) {
            steps.add(new Step(description, forward, compensate));
            return this;
        }

        /**
         * Build the {@link Saga} with the configured steps.
         *
         * @return a new {@link Saga} instance
         */
        public Saga build() {
            return new Saga(steps);
        }
    }

    // ========================================================================
    // Direct step addition (chaining, non-builder style)
    // ========================================================================

    /**
     * Add a step to the saga.
     * <p>
     * This method supports the direct chaining style:
     * <pre>{@code
     * Saga saga = new Saga()
     *     .addStep("desc", fwd, cmp)
     *     .addStep("desc2", fwd2, cmp2);
     * }</pre>
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

    // ========================================================================
    // Execution
    // ========================================================================

    /**
     * Execute all forward steps in order.  If any step throws an exception,
     * all previously completed steps are compensated in reverse order.
     * <p>
     * Alias for {@link #run(HostCalls)}.
     *
     * @param h the HostCalls instance for this workflow execution
     * @throws Exception if a forward step fails and compensation completes
     * @throws RuntimeException if compensation itself fails (the original
     *         forward exception is added as a suppressed exception)
     */
    public void execute(HostCalls h) throws Exception {
        run(h);
    }

    // ========================================================================
    // Typed Saga (generic)
    // ========================================================================

    /**
     * A typed saga step where the forward action returns a value of type {@code T}.
     *
     * @param <T> the result type of the forward action
     */
    public static class StepTyped<T> {
        /** Human-readable description of this step. */
        public final String description;

        /** The forward action returning a value of type T. */
        public final java.util.function.Function<HostCalls, T> forward;

        /** The compensating action to run on rollback. */
        public final SagaCompensator compensate;

        /**
         * Construct a new typed saga step.
         *
         * @param description human-readable description
         * @param forward     the forward action returning T
         * @param compensate  the compensating action
         */
        public StepTyped(String description, java.util.function.Function<HostCalls, T> forward, SagaCompensator compensate) {
            this.description = description;
            this.forward = forward;
            this.compensate = compensate;
        }
    }

    /**
     * A saga with typed result collection.
     *
     * Generic parameter {@code T} is the return type of each step's forward action.
     * {@code execute()} returns {@code List<T>}.
     *
     * Usage:
     * <pre>{@code
     * Saga.SagaTyped<String> saga = new Saga.SagaTyped<>();
     * saga.addStep("reserve",
     *     h -> h.durableCall("inv", "Reserve", input).getValue(),
     *     h -> h.durableCall("inv", "Release", input));
     * List<String> results = saga.execute(h);
     * }</pre>
     *
     * @param <T> the result type of each step's forward action
     */
    public static class SagaTyped<T> {
        private final List<StepTyped<T>> steps;

        /** Create a new typed saga with no steps. */
        public SagaTyped() {
            this.steps = new ArrayList<>();
        }

        /**
         * Add a typed step to the saga.
         *
         * @param description human-readable description
         * @param forward     the forward action returning T
         * @param compensate  the compensating action (may be null)
         * @return {@code this} for chaining
         */
        public SagaTyped<T> addStep(String description, java.util.function.Function<HostCalls, T> forward, SagaCompensator compensate) {
            steps.add(new StepTyped<>(description, forward, compensate));
            return this;
        }

        /**
         * Execute all forward steps in order, collecting results.
         *
         * If any step throws an exception, all previously completed steps are
         * compensated in reverse order (skipping null compensations).
         *
         * @param h the HostCalls instance for this workflow execution
         * @return the collected results of all forward steps, in order
         * @throws Exception if a forward step fails and compensation completes
         * @throws RuntimeException if compensation itself fails (the original
         *         forward exception is added as a suppressed exception)
         */
        public List<T> execute(HostCalls h) throws Exception {
            List<StepTyped<T>> completed = new ArrayList<>();
            List<T> results = new ArrayList<>();

            try {
                for (StepTyped<T> step : steps) {
                    T result = step.forward.apply(h);
                    results.add(result);
                    completed.add(step);
                }
            } catch (Exception e) {
                // Compensate completed steps in reverse order.
                Exception compensateFailure = null;
                for (int i = completed.size() - 1; i >= 0; i--) {
                    StepTyped<T> failed = completed.get(i);
                    if (failed.compensate == null) continue;
                    try {
                        failed.compensate.accept(h);
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

            return results;
        }
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
