package com.cleat.saga;

import cleat.HostCalls;
import cleat.CleatEntry;
import cleat.CleatResult;

/**
 * Money Transfer Saga -- a stress test for cleat's Java SDK under TeaVM/WASM.
 *
 * Implements the classic saga compensation pattern for transferring money
 * between accounts:
 * <ol>
 *   <li>Withdraw from source account</li>
 *   <li>Deposit to destination account</li>
 *   <li>If deposit fails, compensate by reversing the withdrawal</li>
 * </ol>
 *
 * <strong>Stress test targets:</strong>
 * <ul>
 *   <li>Annotation processing ({@code @CleatEntry} on multiple methods)</li>
 *   <li>Manual JSON construction without a proper JSON library</li>
 *   <li>Manual JSON field extraction without {@code java.util.regex.Pattern}</li>
 *   <li>{@code durableDefer} for cleanup registration</li>
 *   <li>{@code setQueryState} / {@code pollCancellation} integration</li>
 *   <li>TeaVM's emulation of {@code StringBuilder}, {@code String.replace},
 *       and {@code String.format}</li>
 *   <li>Error handling and compensation orchestration</li>
 * </ul>
 */
public class MoneyTransfer {

    /**
     * Main money transfer saga entry point.
     * <p>
     * Expected input format (JSON, must be a String because
     * {@link cleat.JsonHelper#parse JsonHelper.parse} only supports String):
     * <pre>
     * {"from":"accountA","to":"accountB","amount":100,"currency":"USD"}
     * </pre>
     *
     * @param h     the {@link HostCalls} instance for durable orchestration
     * @param input the JSON-encoded transfer input string
     * @return a JSON result string indicating success or failure
     */
    @CleatEntry(name = "transfer_money")
    public static String transferMoney(HostCalls h, String input) {
        // ---- Phase 0: Initialize queryable state ----
        h.setQueryState("status", "started");
        h.cleatLog("Money transfer saga started");

        if (input == null || input.equals("{}") || input.isEmpty()) {
            h.setQueryState("status", "failed");
            h.setQueryState("error", "empty input");
            return errorJson("empty input");
        }

        // ---- Phase 1: Parse input (manual JSON field extraction) ----
        // JsonHelper.parse() only supports String.class, so we do our own
        // lightweight extraction. This also stress-tests TeaVM's char/string
        // handling because we avoid java.util.regex.Pattern entirely.
        String fromAccount = extractJsonString(input, "from");
        String toAccount = extractJsonString(input, "to");
        String amountStr = extractJsonNumber(input, "amount");
        String currency = extractJsonString(input, "currency");
        String description = extractJsonString(input, "description");

        if (fromAccount == null || toAccount == null || amountStr == null) {
            h.setQueryState("status", "failed");
            h.setQueryState("error", "missing required fields");
            return errorJson("required fields: from, to, amount");
        }

        if (fromAccount.equals(toAccount)) {
            h.setQueryState("status", "failed");
            h.setQueryState("error", "source and destination must differ");
            return errorJson("source and destination accounts must be different");
        }

        if (currency == null) {
            currency = "USD";
        }
        if (description == null) {
            description = "";
        }

        h.setQueryState("from", fromAccount);
        h.setQueryState("to", toAccount);
        h.setQueryState("amount", amountStr);
        h.setQueryState("currency", currency);

        // ---- Phase 2: Register deferred cleanup ----
        // durableDefer registers a deferred cleanup description with the host.
        // Note: The callback logic is a string description -- not executable
        // code. The host uses this description to coordinate cleanup if the
        // workflow fails or is cancelled.
        CleatResult<String> deferResult = h.cleatDefer(
            "release_funds:" + fromAccount + ":" + amountStr + ":" + currency);
        if (deferResult.isErr()) {
            h.cleatLog("Warning: durableDefer failed: " + deferResult.getError());
        } else {
            h.cleatLog("Deferred cleanup registered: " + deferResult.getValue());
        }

        // ---- Phase 3: Step 1 - Withdraw from source ----
        String withdrawRef = null;
        {
            h.setQueryState("phase", "withdraw");
            String withdrawReq = buildJson(
                "account", fromAccount,
                "amount", amountStr,
                "currency", currency,
                "description", description);

            h.cleatLog("Step 1/2: Withdrawing " + amountStr + " " + currency + " from " + fromAccount);

            CleatResult<String> withdrawResult =
                h.cleatCall("accounts", "Withdraw", withdrawReq);

            if (withdrawResult.isErr()) {
                h.setQueryState("status", "failed");
                h.setQueryState("phase", "failed:withdraw");
                h.setQueryState("error", "withdrawal failed: " + withdrawResult.getError());
                h.cleatLog("Step 1 failed: " + withdrawResult.getError());
                return errorJson("withdrawal failed: " + escapeJSON(withdrawResult.getError()));
            }

            withdrawRef = withdrawResult.getValue();
            h.setQueryState("withdraw_ref", withdrawRef);
            h.setQueryState("status", "withdrawn");
            h.cleatLog("Step 1 succeeded: ref=" + withdrawRef);
        }

        // ---- Phase 4: Check for cancellation after withdrawal ----
        {
            CleatResult<Boolean> cancelCheck = h.pollCancellation();
            if (cancelCheck.isOk() && cancelCheck.getValue() != null && cancelCheck.getValue()) {
                h.cleatLog("Cancellation detected after withdrawal, compensating...");

                // Compensate: deposit the funds back to the source account
                String compensationReq = buildJson(
                    "account", fromAccount,
                    "amount", amountStr,
                    "currency", currency,
                    "reason", "cancelled_after_withdraw",
                    "reference", "cancel-" + withdrawRef);

                CleatResult<String> compResult =
                    h.cleatCall("accounts", "Deposit", compensationReq);

                h.setQueryState("status", "cancelled");
                h.setQueryState("compensated", "true");
                if (compResult.isErr()) {
                    h.cleatLog("CRITICAL: Compensation also failed: " + compResult.getError());
                    // The saga is in an inconsistent state! Log it.
                    h.setQueryState("compensation_error",
                        "compensation failed: " + compResult.getError());
                    return errorJson("cancelled but compensation failed: "
                        + escapeJSON(compResult.getError()));
                }

                h.cleatLog("Compensation succeeded, funds returned to " + fromAccount);
                return jsonObject(
                    "status", "cancelled",
                    "detail", "withdrawal reversed",
                    "from", fromAccount,
                    "amount", amountStr,
                    "currency", currency);
            }
        }

        // ---- Phase 5: Step 2 - Deposit to destination ----
        String depositRef = null;
        {
            h.setQueryState("phase", "deposit");
            String depositReq = buildJson(
                "account", toAccount,
                "amount", amountStr,
                "currency", currency,
                "reference", fromAccount + "_to_" + toAccount,
                "description", description);

            h.cleatLog("Step 2/2: Depositing " + amountStr + " " + currency + " to " + toAccount);

            CleatResult<String> depositResult =
                h.cleatCall("accounts", "Deposit", depositReq);

            if (depositResult.isErr()) {
                // SAGA COMPENSATION: Reverse the withdrawal by depositing
                // back to the source account. This is the core saga pattern.
                h.cleatLog("Step 2 failed, initiating compensation: "
                    + depositResult.getError());
                h.setQueryState("phase", "compensating");

                String compensateReq = buildJson(
                    "account", fromAccount,
                    "amount", amountStr,
                    "currency", currency,
                    "reason", "deposit_failed",
                    "reference", "compensate-" + withdrawRef);

                CleatResult<String> compensateResult =
                    h.cleatCall("accounts", "Deposit", compensateReq);

                if (compensateResult.isErr()) {
                    // Double failure -- the saga is in an inconsistent state.
                    // The funds were withdrawn from the source but not deposited
                    // to the destination AND the compensation to return the
                    // funds also failed.
                    h.cleatLog("CRITICAL: Double failure -- withdrawal not compensated: "
                        + compensateResult.getError());
                    h.setQueryState("status", "inconsistent");
                    h.setQueryState("error",
                        "deposit failed and compensation failed");
                    h.setQueryState("deposit_error", depositResult.getError());
                    h.setQueryState("compensation_error", compensateResult.getError());

                    return errorJson("INCONSISTENT STATE: deposit failed ("
                        + escapeJSON(depositResult.getError())
                        + ") and compensation also failed ("
                        + escapeJSON(compensateResult.getError()) + ")");
                }

                // Compensation succeeded -- saga is consistent.
                h.cleatLog("Compensation succeeded, funds returned to " + fromAccount);
                h.setQueryState("status", "compensated");
                h.setQueryState("error", "deposit failed: " + depositResult.getError());
                h.setQueryState("compensated", "true");
                h.setQueryState("compensated_at", "after_deposit_failure");

                return buildJson(
                    "status", "compensated",
                    "error", "deposit failed: " + depositResult.getError(),
                    "from", fromAccount,
                    "to", toAccount,
                    "amount", amountStr,
                    "currency", currency,
                    "detail", "withdrawal reversed");
            }

            depositRef = depositResult.getValue();
            h.setQueryState("deposit_ref", depositRef);
            h.cleatLog("Step 2 succeeded: ref=" + depositRef);
        }

        // ---- Phase 6: Success ----
        h.setQueryState("status", "completed");
        h.setQueryState("phase", "done");
        h.cleatLog("Transfer completed: " + amountStr + " " + currency
            + " from " + fromAccount + " to " + toAccount);

        return buildJson(
            "status", "completed",
            "from_account", fromAccount,
            "to_account", toAccount,
            "amount", amountStr,
            "currency", currency,
            "withdraw_ref", withdrawRef,
            "deposit_ref", depositRef,
            "description", description);
    }

    /**
     * Get the current status of a transfer.
     * <p>
     * This entry point demonstrates a query workflow that returns
     * information about the saga's current state. Note that cleat's
     * queryable state is written via {@code setQueryState} during the
     * main workflow; this method provides a secondary entry point.
     *
     * @param h     the {@link HostCalls} instance
     * @param input the JSON input (ignored in this simple example)
     * @return a JSON string with status information
     */
    @CleatEntry(name = "get_transfer_status")
    public static String getTransferStatus(HostCalls h, String input) {
        // This entry point demonstrates a read-only workflow.
        // It does not set any queryable state but returns a static response.
        // In a real implementation, this would look up state from the host.
        h.cleatLog("getTransferStatus called with input: " + input);
        return "{\"note\":\"Status is tracked via setQueryState during transfer_money. "
            + "Queryable state is host-side only.\",\"version\":1}";
    }

    // ========================================================================
    // JSON helpers (no dependency on java.util.regex or external libraries)
    // ========================================================================

    /**
     * Extract a quoted JSON string field value without using regex.
     *
     * @param json  the JSON string
     * @param field the field name
     * @return the field's string value (unescaped), or null if not found
     */
    static String extractJsonString(String json, String field) {
        if (json == null || field == null) {
            return null;
        }
        String key = "\"" + field + "\":";
        int idx = json.indexOf(key);
        if (idx < 0) {
            return null;
        }
        int start = idx + key.length();
        // Skip whitespace
        while (start < json.length() && json.charAt(start) <= ' ') {
            start++;
        }
        if (start >= json.length()) {
            return null;
        }
        char first = json.charAt(start);
        if (first != '"') {
            return null; // Not a string value
        }
        start++; // Skip opening quote
        StringBuilder sb = new StringBuilder();
        for (int i = start; i < json.length(); i++) {
            char c = json.charAt(i);
            if (c == '\\') {
                // Handle escape sequences
                i++;
                if (i >= json.length()) {
                    break;
                }
                char next = json.charAt(i);
                switch (next) {
                    case '"':  sb.append('"');  break;
                    case '\\': sb.append('\\'); break;
                    case '/':  sb.append('/');  break;
                    case 'b':  sb.append('\b'); break;
                    case 'f':  sb.append('\f'); break;
                    case 'n':  sb.append('\n'); break;
                    case 'r':  sb.append('\r'); break;
                    case 't':  sb.append('\t'); break;
                    case 'u':
                        // Unicode escape: \\uXXXX
                        if (i + 4 < json.length()) {
                            String hex = json.substring(i + 1, i + 5);
                            try {
                                sb.append((char) Integer.parseInt(hex, 16));
                                i += 4;
                            } catch (NumberFormatException e) {
                                sb.append('?');
                                i += 4;
                            }
                        }
                        break;
                    default:
                        sb.append(next);
                        break;
                }
            } else if (c == '"') {
                break; // End of string
            } else {
                sb.append(c);
            }
        }
        return sb.toString();
    }

    /**
     * Extract a JSON number field value without using regex.
     *
     * @param json  the JSON string
     * @param field the field name
     * @return the field's number value as a string, or null if not found
     */
    static String extractJsonNumber(String json, String field) {
        if (json == null || field == null) {
            return null;
        }
        String key = "\"" + field + "\":";
        int idx = json.indexOf(key);
        if (idx < 0) {
            return null;
        }
        int start = idx + key.length();
        // Skip whitespace
        while (start < json.length() && json.charAt(start) <= ' ') {
            start++;
        }
        if (start >= json.length()) {
            return null;
        }
        char first = json.charAt(start);
        if (first == '"') {
            // Number might be quoted as a string
            start++;
            StringBuilder sb = new StringBuilder();
            for (int i = start; i < json.length(); i++) {
                char c = json.charAt(i);
                if (c == '"') break;
                sb.append(c);
            }
            String val = sb.toString().trim();
            return val.isEmpty() ? null : val;
        }
        if (first == '-' || first == '+' || (first >= '0' && first <= '9')) {
            StringBuilder sb = new StringBuilder();
            for (int i = start; i < json.length(); i++) {
                char c = json.charAt(i);
                if ((c >= '0' && c <= '9') || c == '.' || c == '-'
                    || c == '+' || c == 'e' || c == 'E') {
                    sb.append(c);
                } else {
                    break;
                }
            }
            String val = sb.toString().trim();
            return val.isEmpty() ? null : val;
        }
        return null;
    }

    /**
     * Build a simple flat JSON object from alternating key-value pairs.
     * Values are automatically quoted if they are not numeric or boolean.
     */
    static String buildJson(String... pairs) {
        StringBuilder sb = new StringBuilder("{");
        boolean first = true;
        for (int i = 0; i < pairs.length; i += 2) {
            if (!first) {
                sb.append(",");
            }
            first = false;
            String key = pairs[i];
            String value = (i + 1 < pairs.length) ? pairs[i + 1] : "";
            sb.append('"').append(escapeJSON(key)).append('"').append(':');
            if (isJsonNumber(value) || value.equals("true") || value.equals("false")
                || value.equals("null")) {
                sb.append(value);
            } else {
                sb.append('"').append(escapeJSON(value)).append('"');
            }
        }
        sb.append("}");
        return sb.toString();
    }

    /**
     * Build a simple JSON object from a key and value pair.
     */
    static String jsonObject(String k1, String v1) {
        return "{\"" + escapeJSON(k1) + "\":\"" + escapeJSON(v1) + "\"}";
    }

    /**
     * Build a JSON object from 4 key-value pairs.
     */
    static String jsonObject(String k1, String v1, String k2, String v2) {
        return "{\"" + escapeJSON(k1) + "\":\"" + escapeJSON(v1) + "\",\""
            + escapeJSON(k2) + "\":\"" + escapeJSON(v2) + "\"}";
    }

    /**
     * Build a JSON object from 6 key-value pairs.
     */
    static String jsonObject(String k1, String v1, String k2, String v2,
                              String k3, String v3) {
        return jsonObject(k1, v1, k2, v2).replace("}",
            ",\"" + escapeJSON(k3) + "\":\"" + escapeJSON(v3) + "\"}");
    }

    /**
     * Build a JSON object from 8 key-value pairs.
     */
    static String jsonObject(String k1, String v1, String k2, String v2,
                              String k3, String v3, String k4, String v4) {
        return jsonObject(k1, v1, k2, v2, k3, v3).replace("}",
            ",\"" + escapeJSON(k4) + "\":\"" + escapeJSON(v4) + "\"}");
    }

    /**
     * Build a JSON object from 10 key-value pairs.
     */
    static String jsonObject(String k1, String v1, String k2, String v2,
                              String k3, String v3, String k4, String v4,
                              String k5, String v5) {
        return jsonObject(k1, v1, k2, v2, k3, v3, k4, v4).replace("}",
            ",\"" + escapeJSON(k5) + "\":\"" + escapeJSON(v5) + "\"}");
    }

    /**
     * Build a JSON object from 12+ key-value pairs.
     */
    static String jsonObject(String k1, String v1, String k2, String v2,
                              String k3, String v3, String k4, String v4,
                              String k5, String v5, String k6, String v6) {
        return jsonObject(k1, v1, k2, v2, k3, v3, k4, v4, k5, v5).replace("}",
            ",\"" + escapeJSON(k6) + "\":\"" + escapeJSON(v6) + "\"}");
    }

    /**
     * Simple error JSON helper.
     */
    static String errorJson(String message) {
        return "{\"error\":\"" + escapeJSON(message) + "\"}";
    }

    /**
     * Check if a string looks like a JSON number.
     */
    private static boolean isJsonNumber(String s) {
        if (s == null || s.isEmpty()) {
            return false;
        }
        int i = 0;
        if (s.charAt(i) == '-' || s.charAt(i) == '+') {
            i++;
        }
        if (i >= s.length()) {
            return false;
        }
        boolean hasDigit = false;
        while (i < s.length() && s.charAt(i) >= '0' && s.charAt(i) <= '9') {
            hasDigit = true;
            i++;
        }
        if (i < s.length() && s.charAt(i) == '.') {
            i++;
            while (i < s.length() && s.charAt(i) >= '0' && s.charAt(i) <= '9') {
                hasDigit = true;
                i++;
            }
        }
        if (hasDigit && i < s.length() && (s.charAt(i) == 'e' || s.charAt(i) == 'E')) {
            i++;
            if (i < s.length() && (s.charAt(i) == '-' || s.charAt(i) == '+')) {
                i++;
            }
            while (i < s.length() && s.charAt(i) >= '0' && s.charAt(i) <= '9') {
                i++;
            }
        }
        return hasDigit && i == s.length();
    }

    /**
     * Escape a string for safe embedding in a JSON string value.
     * Does NOT use java.util.regex.Pattern -- char-by-char iteration.
     *
     * Note: This method uses String.replace() for simplicity in the
     * multi-replacement case. Under TeaVM/WASM, String.replace(CharSequence,
     * CharSequence) may compile to Pattern.compile internally, which would
     * fail if TeaVM doesn't support java.util.regex. If the build fails,
     * this is a likely culprit.
     */
    static String escapeJSON(String s) {
        if (s == null) {
            return "null";
        }
        // Manual char-by-char escape -- NO use of String.replace() to
        // avoid any dependency on java.util.regex.Pattern.
        StringBuilder sb = new StringBuilder(s.length() + 16);
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            switch (c) {
                case '"':  sb.append("\\\""); break;
                case '\\': sb.append("\\\\"); break;
                case '\b': sb.append("\\b");  break;
                case '\f': sb.append("\\f");  break;
                case '\n': sb.append("\\n");  break;
                case '\r': sb.append("\\r");  break;
                case '\t': sb.append("\\t");  break;
                default:
                    if (c < 0x20) {
                        sb.append("\\u");
                        sb.append(hexDigit((c >> 12) & 0xF));
                        sb.append(hexDigit((c >> 8) & 0xF));
                        sb.append(hexDigit((c >> 4) & 0xF));
                        sb.append(hexDigit(c & 0xF));
                    } else {
                        sb.append(c);
                    }
                    break;
            }
        }
        return sb.toString();
    }

    /**
     * Convert a nibble (0-15) to its hex digit character.
     */
    private static char hexDigit(int nibble) {
        if (nibble < 10) {
            return (char) ('0' + nibble);
        }
        return (char) ('a' + nibble - 10);
    }
}
