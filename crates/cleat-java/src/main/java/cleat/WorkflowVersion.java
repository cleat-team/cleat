package cleat;

/**
 * Build-time version constants for cleat Java workflows.
 *
 * These values are set via JVM system properties at compile time (TeaVM
 * preserves compile-time constants). The post-compile inject-metadata.sh
 * script reads a compiled WASM file and stamps the "cleat.metadata" custom
 * section using the values from environment variables.
 *
 * Usage:
 *   CLEAT_WORKFLOW_NAME=PlaceOrder \
 *   CLEAT_WORKFLOW_VERSION=3 \
 *   CLEAT_MIN_COMPATIBLE_VERSION=1 \
 *   CLEAT_ABI_VERSION=1 \
 *   CLEAT_PLUGIN_DEPS='{"llm":">=1.2.0"}' \
 *   bash scripts/inject-metadata.sh workflow.wasm
 *
 * System property alternatives (for JVM-based builds):
 *   -Dcleat.workflow.name=PlaceOrder
 *   -Dcleat.workflow.version=3
 *   -Dcleat.workflow.minVersion=1
 *   -Dcleat.workflow.abiVersion=1
 *   -Dcleat.workflow.pluginDeps='{"llm":">=1.2.0"}'
 */
public final class WorkflowVersion {

    /** Human-readable name of this workflow definition. */
    public static final String WORKFLOW_NAME =
        System.getProperty("cleat.workflow.name", "unknown");

    /** Monotonic version number for this workflow definition. */
    public static final int WORKFLOW_VERSION =
        Integer.getInteger("cleat.workflow.version", 0);

    /** Minimum compatible workflow definition version (for child workflows). */
    public static final int MIN_COMPATIBLE_VERSION =
        Integer.getInteger("cleat.workflow.minVersion", 1);

    /** WASM host ABI version this module targets. */
    public static final int ABI_VERSION =
        Integer.getInteger("cleat.workflow.abiVersion", 1);

    /**
     * Plugin dependencies as a JSON string.
     * Example: {"llm":">=1.2.0","blobstore":"~2.0.0"}
     */
    public static final String PLUGIN_DEPS =
        System.getProperty("cleat.workflow.pluginDeps", "{}");

    private WorkflowVersion() {
        // Utility class - no instantiation.
    }

    /**
     * Returns a summary of all version constants as a formatted string.
     */
    public static String summary() {
        return String.format(
            "WorkflowVersion[name=%s, version=%d, minVersion=%d, abiVersion=%d, pluginDeps=%s]",
            WORKFLOW_NAME, WORKFLOW_VERSION, MIN_COMPATIBLE_VERSION,
            ABI_VERSION, PLUGIN_DEPS
        );
    }
}
