package cleatexample.statemachine;

/**
 * TeaVM entry point for the payment state machine WASM compilation.
 * <p>
 * This class provides the {@code main} method that TeaVM uses as the root
 * of its reachability (tree-shaking) analysis.  The actual workflow logic
 * lives in {@link PaymentProcessor} and {@link Account}.
 * <p>
 * <strong>Tree-shaking protection:</strong> TeaVM treats {@code @Export}
 * annotated methods as reachability roots, so the generated export wrapper
 * classes (produced by {@link cleat.CleatEntryProcessor}) are preserved
 * automatically — each carries a {@code @Export(name = "...")} annotation
 * that makes the method a WASM export.
 * <p>
 * As an additional safety measure, this main class references all workflow
 * classes directly, ensuring that every {@code @CleatEntry} method is
 * reachable from TeaVM's analysis and that the generated
 * {@code CleatEntryAggregator} includes all wrappers.
 */
public class WorkflowEntry {

    /**
     * Main entry point for TeaVM analysis.
     * <p>
     * This method does not perform any real work at runtime.  It exists
     * solely as a root for TeaVM's tree-shaking analysis.
     *
     * @param args command-line arguments (unused)
     */
    public static void main(String[] args) {
        // Reference all workflow classes to ensure they are reachable
        // from TeaVM's analysis root.  The @Export-annotated generated
        // wrappers (PaymentProcessor_makePayment_Export,
        // PaymentProcessor_cancelPayment_Export, etc.) are preserved by
        // TeaVM's export handling.
        System.out.println("Cleat state machine: " + PaymentProcessor.class.getName());
        System.out.println("Cleat account: " + Account.class.getName());
    }
}
