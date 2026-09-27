package cleat;

/**
 * Thrown when a host call determines the workflow must suspend.
 *
 * <p>The generated export wrapper catches this and returns
 * {@link Memory#SUSPEND_SENTINEL} to the host, which is the value the engine
 * checks for ({@code engine/backend_wasmtime.go}: {@code if raw == (1 << 62)}).
 *
 * <p>IMPROVEMENT-PLAN 3.74. Before this existed, suspension was documented and
 * unreachable. {@code cleatSleepMs} returned {@code true} meaning "the workflow
 * should propagate the suspension by returning {@code Memory.SUSPEND_SENTINEL}
 * from the export" — but the author does not write the export, the annotation
 * processor generates it, and the generated wrapper had no branch that could
 * return that value. It stringified whatever the workflow returned and reported
 * {@code encodeExportResult(0, written)}: a plain success. So a Java workflow
 * that slept on a fresh execution completed with a bogus result instead of
 * suspending.
 *
 * <p>Unchecked on purpose. A workflow author should not have to declare or
 * handle it — suspension is not a failure, it is the runtime pausing the
 * workflow between segments, and every other cleat SDK signals it by unwinding
 * (Go and Rust panic, Python raises).
 *
 * <p>Do not catch this in workflow code. Catching it completes a workflow the
 * host has already recorded as suspended.
 */
public final class SuspendSignal extends RuntimeException {

    private static final long serialVersionUID = 1L;

    /** Creates the signal. There is nothing to report: this is not an error. */
    public SuspendSignal() {
        // No stack trace: this is control flow on a hot path, not a fault, and
        // filling one in on a WASM guest costs more than it can ever tell us.
        super("cleat: workflow suspended", null, false, false);
    }
}
