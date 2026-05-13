package com.cleat.example;

/**
 * TeaVM entry point for the plugin harness workflow compilation.
 *
 * Provides the {@code main} method that TeaVM uses as the root of its
 * reachability (tree-shaking) analysis. References the generated WASM
 * export wrapper class to prevent tree-shaking.
 */
public class WorkflowEntry {

    /**
     * Static reference to the generated export wrapper, preventing TeaVM
     * from tree-shaking the @Export annotated method.
     */
    private static final Class<?> EXPORT_REF =
        PluginHarnessWorkflow_callAllPlugins_Export.class;

    /**
     * Main entry point for TeaVM analysis.
     */
    public static void main(String[] args) {
        System.out.println("Cleat workflow: " + PluginHarnessWorkflow.class.getName());
    }
}
