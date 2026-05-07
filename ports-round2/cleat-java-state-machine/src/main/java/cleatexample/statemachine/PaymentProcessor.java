package cleatexample.statemachine;

import cleat.DurableEntry;
import cleat.DurableResult;
import cleat.HostCalls;
import cleat.JsonHelper;
import java.util.HashMap;
import java.util.Map;

/**
 * Payment state machine workflow — Cleat Java SDK port of the Restate
 * {@code PaymentProcessor} Virtual Object.
 * <p>
 * <strong>Original Restate pattern:</strong> A Virtual Object with three
 * handlers ({@code makePayment}, {@code cancelPayment}, {@code expire})
 * that serialise execution per payment ID and use Restate-managed K/V
 * state for lifecycle tracking.
 * <p>
 * <strong>Cleat mapping:</strong>
 * <ul>
 *   <li>Each handler becomes a separate {@code @DurableEntry} static method</li>
 *   <li>State is stored in Cleat query state with manually prefixed keys
 *       ({@code payment:<paymentId>:status}, {@code payment:<paymentId>:details})</li>
 *   <li>No Virtual Object serialisation — callers should use the workflow ID
 *       as the payment ID to avoid concurrent modification races</li>
 *   <li>Restate's delayed message ({@code send().expire()}) is not supported;
 *       expiry must be triggered externally or via a separate scheduler workflow</li>
 * </ul>
 *
 * <h3>State transitions</h3>
 * <pre>
 *         NEW ──────► COMPLETED_SUCCESSFULLY ──► CANCELLED ──► (expired)
 *          │                                           │
 *          └──────────► CANCELLED ──────────────────────┘
 * </pre>
 */
public class PaymentProcessor {

    /** Prefix for payment state keys. */
    private static final String STATE_PREFIX = "payment:";
    private static final String KEY_STATUS = ":status";
    private static final String KEY_DETAILS = ":details";

    // ========================================================================
    // Entry points
    // ========================================================================

    /**
     * Process a payment for the given payment ID.
     * <p>
     * State transitions: NEW {@literal ->} COMPLETED_SUCCESSFULLY (on success),
     * otherwise stays unchanged (caller may retry).
     *
     * @param h         the {@link HostCalls} instance
     * @param rawInput JSON with keys {@code "paymentId"} (String),
     *                  {@code "accountId"} (String), and
     *                  {@code "amountCents"} (long)
     * @return JSON result: {@code {"success":true, "reason":"..."}} on success,
     *         or {@code {"success":false, "reason":"..."}} on failure
     */
    @DurableEntry(name = "make_payment")
    public static String makePayment(HostCalls h, String rawInput) {
        Map<String, Object> input = JsonHelper.parseObject(rawInput);

        String paymentId = extractString(input, "paymentId");
        if (paymentId == null || paymentId.isEmpty()) {
            return JsonHelper.errorJson("Payment ID is required");
        }

        // Read current payment status from state
        String status = readStatus(h, paymentId);

        if ("CANCELLED".equals(status)) {
            return resultJson(false, "Payment already cancelled");
        }
        if ("COMPLETED_SUCCESSFULLY".equals(status)) {
            return resultJson(false, "Payment already completed");
        }

        String accountId = extractString(input, "accountId");
        if (accountId == null || accountId.isEmpty()) {
            return resultJson(false, "Account ID is required");
        }

        long amountCents = extractLong(input, "amountCents");
        if (amountCents <= 0) {
            return resultJson(false, "Amount must be greater than 0");
        }

        // Build the withdraw request for the account service
        Map<String, Object> withdrawReq = new HashMap<>();
        withdrawReq.put("accountId", accountId);
        withdrawReq.put("amountCents", amountCents);
        String withdrawJSON = JsonHelper.stringify(withdrawReq);

        // Call account service to withdraw funds (durable, journaled call)
        DurableResult<String> callResult = h.durableCall("account", "withdraw", withdrawJSON);

        if (callResult.isErr()) {
            return resultJson(false, "Withdrawal failed: " + callResult.getError());
        }

        // Parse the withdrawal response to check success
        Map<String, Object> withdrawResponse = JsonHelper.parseObject(callResult.getValue());
        boolean withdrawSuccess = extractBoolean(withdrawResponse, "success");
        if (!withdrawSuccess) {
            String reason = extractString(withdrawResponse, "reason");
            return resultJson(false, reason != null ? reason : "Withdrawal declined");
        }

        // Remember state only on success — on failure the caller may retry
        h.setQueryState(stateKey(paymentId, KEY_STATUS), "COMPLETED_SUCCESSFULLY");
        h.setQueryState(stateKey(paymentId, KEY_DETAILS), rawInput);

        h.durableLog("Payment " + paymentId + " completed: " + amountCents
            + " cents withdrawn from " + accountId);

        return resultJson(true, "Payment processed successfully");
    }

    /**
     * Cancel a pending or completed payment.
     * <p>
     * State transitions:
     * <ul>
     *   <li>NEW {@literal ->} CANCELLED (prevent future payment)</li>
     *   <li>COMPLETED_SUCCESSFULLY {@literal ->} CANCELLED + refund</li>
     *   <li>CANCELLED {@literal ->} no-op</li>
     * </ul>
     *
     * @param h         the {@link HostCalls} instance
     * @param rawInput JSON with key {@code "paymentId"} (String)
     * @return JSON result indicating cancellation status
     */
    @DurableEntry(name = "cancel_payment")
    public static String cancelPayment(HostCalls h, String rawInput) {
        Map<String, Object> input = JsonHelper.parseObject(rawInput);

        String paymentId = extractString(input, "paymentId");
        if (paymentId == null || paymentId.isEmpty()) {
            return JsonHelper.errorJson("Payment ID is required");
        }

        String status = readStatus(h, paymentId);

        switch (status) {
            case "NEW": {
                // Payment not yet made — mark as cancelled to prevent future execution
                h.setQueryState(stateKey(paymentId, KEY_STATUS), "CANCELLED");
                h.durableLog("Payment " + paymentId + " cancelled before execution");
                return resultJson(true, "Payment cancelled");
            }

            case "CANCELLED": {
                // Already cancelled — no-op
                return resultJson(true, "Payment already cancelled");
            }

            case "COMPLETED_SUCCESSFULLY": {
                // Mark as cancelled
                h.setQueryState(stateKey(paymentId, KEY_STATUS), "CANCELLED");

                // Read stored payment details to issue refund
                DurableResult<String> detailsResult = h.getQueryState(
                    stateKey(paymentId, KEY_DETAILS));
                if (detailsResult.isOk() && detailsResult.getValue() != null
                        && !detailsResult.getValue().isEmpty()) {
                    Map<String, Object> details = JsonHelper.parseObject(detailsResult.getValue());
                    String accountId = extractString(details, "accountId");
                    long amountCents = extractLong(details, "amountCents");

                    if (accountId != null && amountCents > 0) {
                        // Issue refund: deposit back to the account
                        Map<String, Object> depositReq = new HashMap<>();
                        depositReq.put("accountId", accountId);
                        depositReq.put("amountCents", amountCents);
                        h.durableCall("account", "deposit",
                            JsonHelper.stringify(depositReq));
                    }
                }

                h.durableLog("Payment " + paymentId + " cancelled with refund");
                return resultJson(true, "Payment cancelled and refunded");
            }

            default: {
                return resultJson(false, "Unknown payment status: " + status);
            }
        }
    }

    /**
     * Expire (clean up) state for a completed or cancelled payment.
     * <p>
     * In the Restate original this is invoked via a delayed message 1 day
     * after completion.  In Cleat, the caller or an external scheduler must
     * invoke this endpoint when state should be cleaned up.
     *
     * @param h         the {@link HostCalls} instance
     * @param rawInput JSON with key {@code "paymentId"} (String)
     * @return JSON result indicating cleanup status
     */
    @DurableEntry(name = "expire_payment")
    public static String expirePayment(HostCalls h, String rawInput) {
        Map<String, Object> input = JsonHelper.parseObject(rawInput);

        String paymentId = extractString(input, "paymentId");
        if (paymentId == null || paymentId.isEmpty()) {
            return JsonHelper.errorJson("Payment ID is required");
        }

        // Clear all state for this payment
        h.setQueryState(stateKey(paymentId, KEY_STATUS), "");
        h.setQueryState(stateKey(paymentId, KEY_DETAILS), "");

        h.durableLog("Payment " + paymentId + " state expired");
        return resultJson(true, "Payment state expired");
    }

    // ========================================================================
    // State helpers
    // ========================================================================

    /**
     * Build a namespaced state key for the given payment ID and suffix.
     * Example: {@code stateKey("pay-123", ":status")} returns
     * {@code "payment:pay-123:status"}.
     */
    private static String stateKey(String paymentId, String suffix) {
        return STATE_PREFIX + paymentId + suffix;
    }

    /**
     * Read the current payment status from query state.
     * Returns {@code "NEW"} if no status has been stored yet.
     */
    private static String readStatus(HostCalls h, String paymentId) {
        DurableResult<String> result = h.getQueryState(
            stateKey(paymentId, KEY_STATUS));
        if (result.isOk() && result.getValue() != null
                && !result.getValue().isEmpty()) {
            return result.getValue();
        }
        return "NEW";
    }

    // ========================================================================
    // JSON helpers
    // ========================================================================

    /**
     * Build a JSON result object string.
     */
    private static String resultJson(boolean success, String reason) {
        Map<String, Object> result = new HashMap<>();
        result.put("success", success);
        result.put("reason", reason);
        return JsonHelper.stringify(result);
    }

    /**
     * Extract a String value from a parsed JSON map, returning null if missing.
     */
    private static String extractString(Map<String, Object> map, String key) {
        Object val = map.get(key);
        return val instanceof String ? (String) val : null;
    }

    /**
     * Extract a long value from a parsed JSON map, returning 0 if missing.
     */
    private static long extractLong(Map<String, Object> map, String key) {
        Object val = map.get(key);
        if (val instanceof Number) {
            return ((Number) val).longValue();
        }
        return 0;
    }

    /**
     * Extract a boolean value from a parsed JSON map, returning false if missing.
     */
    private static boolean extractBoolean(Map<String, Object> map, String key) {
        Object val = map.get(key);
        return val instanceof Boolean && (Boolean) val;
    }
}
