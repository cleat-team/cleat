package cleat;

/**
 * A marker exception for non-retryable workflow errors.
 * <p>
 * When a workflow throws or returns a {@code TerminalError}, the Cleat host
 * will <strong>not</strong> retry the workflow. This is the Java equivalent of
 * Go's {@code cleat.TerminalError} sentinel and the AS
 * {@code TERMINAL_ERROR_CODE} constant.
 * <p>
 * Usage in a workflow entry point:
 * <pre>{@code
 * @CleatEntry(name = "process_payment")
 * public static String processPayment(HostCalls h, String input) {
 *     Map<String, Object> parsed = JsonHelper.parseObject(input);
 *     String orderId = (String) parsed.get("order_id");
 *     if (orderId == null || orderId.isEmpty()) {
 *         throw new TerminalError("order_id is required");
 *     }
 *     // ... proceed with payment ...
 *     return "{\"status\":\"ok\"}";
 * }
 * }</pre>
 * <p>
 * When the annotation processor ({@link CleatEntryProcessor}) encounters a
 * thrown {@code TerminalError}, it signals the host with
 * {@code TERMINAL_ERROR_CODE} (value 2) so the workflow run is marked as
 * failed without automatic retry.
 * <p>
 * This class extends {@link RuntimeException} so it can be thrown from
 * workflow code without requiring checked-exception declarations.
 *
 * @see HostCalls
 * @see CleatEntry
 * @see CleatEntryProcessor
 */
public class TerminalError extends RuntimeException {

    /**
     * Create a terminal error with the given message.
     *
     * @param message a human-readable description of the error that caused
     *                the workflow to fail
     */
    public TerminalError(String message) {
        super(message);
    }

    /**
     * Create a terminal error with the given message and cause.
     *
     * @param message a human-readable description of the error
     * @param cause   the underlying cause (may be {@code null})
     */
    public TerminalError(String message, Throwable cause) {
        super(message, cause);
    }
}
