package cleat;

/**
 * Auto-generated analysis root for TeaVM WASM compilation.
 * References all @CleatEntry workflow classes and their
 * generated export wrappers to prevent tree-shaking.
 */
public class WorkflowEntry {

    /**
     * Static reference to CleatEntryIndex.WRAPPER_CLASSES
     * prevents TeaVM from tree-shaking the export wrappers.
     */
    @SuppressWarnings("unused")
    private static final Class<?>[] AGGREGATOR_REF =
        cleat.generated.CleatEntryIndex.WRAPPER_CLASSES;

    /**
     * Direct references to all classes that contain
     * @CleatEntry methods. Prevents TeaVM from
     * tree-shaking the user's workflow logic.
     */
    @SuppressWarnings("unused")
    private static final Class<?>[] WORKFLOW_CLASSES = new Class<?>[] {
        com.cleat.example.PluginHarnessWorkflow.class,
    };

    /**
     * Main entry point for TeaVM reachability analysis.
     * References all workflow classes to ensure they
     * survive tree-shaking during WASM compilation.
     *
     * @param args unused
     */
    public static void main(String[] args) {
        java.util.Arrays.stream(WORKFLOW_CLASSES).forEach(c -> {
            System.out.println("Cleat workflow: " + c.getName());
        });
    }

    private WorkflowEntry() {}
}
