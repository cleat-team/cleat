package cleatexample.statemachine;

import cleat.DurableEntry;
import cleat.DurableResult;
import cleat.HostCalls;
import cleat.JsonHelper;
import java.util.HashMap;
import java.util.Map;

/**
 * A simple account service ported from the Restate {@code Account} Virtual Object.
 * <p>
 * In the Restate original, {@code Account} is a Virtual Object with per-key
 * balance state and automatic serialisation.  Since Cleat has no Virtual
 * Object concept, balance is stored in query state with key-prefixed names
 * ({@code account_balance:<accountId>}).
 * <p>
 * <strong>Cleat mapping notes:</strong>
 * <ul>
 *   <li>{@code @VirtualObject} + {@code @Handler} {@literal ->} {@code @DurableEntry} static methods</li>
 *   <li>{@code ObjectContext.get(StateKey)} {@literal ->} {@link HostCalls#getQueryState(String)}</li>
 *   <li>{@code ObjectContext.set(StateKey, value)} {@literal ->} {@link HostCalls#setQueryState(String, String)}</li>
 *   <li>{@code TerminalException} {@literal ->} error JSON string returned (non-retryable)</li>
 *   <li>{@code Math.random()} for initial balance {@literal ->} {@link HostCalls#random()} for determinism</li>
 * </ul>
 */
public class Account {

    /** Prefix for account balance state keys. */
    private static final String BALANCE_KEY_PREFIX = "account_balance:";

    /**
     * Deposit funds into an account.
     *
     * @param h         the {@link HostCalls} instance
     * @param rawInput JSON with keys {@code "accountId"} (String) and
     *                  {@code "amountCents"} (long)
     * @return JSON result: {@code {"success":true, "balance":<newBalance>}}
     *         on success, or an error JSON on failure
     */
    @DurableEntry(name = "account_deposit")
    public static String deposit(HostCalls h, String rawInput) {
        Map<String, Object> input = JsonHelper.parseObject(rawInput);

        String accountId = extractString(input, "accountId");
        if (accountId == null || accountId.isEmpty()) {
            return JsonHelper.errorJson("Account ID is required");
        }

        long amountCents = extractLong(input, "amountCents");
        if (amountCents <= 0) {
            return JsonHelper.errorJson("Amount must be greater than 0");
        }

        // Read current balance from query state (persistent across invocations)
        String balanceKey = BALANCE_KEY_PREFIX + accountId;
        long balance = readBalance(h, balanceKey);

        balance += amountCents;
        h.setQueryState(balanceKey, String.valueOf(balance));

        Map<String, Object> result = new HashMap<>();
        result.put("success", true);
        result.put("balance", balance);
        return JsonHelper.stringify(result);
    }

    /**
     * Withdraw funds from an account.
     *
     * @param h         the {@link HostCalls} instance
     * @param rawInput JSON with keys {@code "accountId"} (String) and
     *                  {@code "amountCents"} (long)
     * @return JSON result: {@code {"success":true, "balance":<newBalance>}}
     *         on success, or {@code {"success":false, "reason":"..."}} on
     *         insufficient funds
     */
    @DurableEntry(name = "account_withdraw")
    public static String withdraw(HostCalls h, String rawInput) {
        Map<String, Object> input = JsonHelper.parseObject(rawInput);

        String accountId = extractString(input, "accountId");
        if (accountId == null || accountId.isEmpty()) {
            return JsonHelper.errorJson("Account ID is required");
        }

        long amountCents = extractLong(input, "amountCents");
        if (amountCents <= 0) {
            return JsonHelper.errorJson("Amount must be greater than 0");
        }

        // Read current balance
        String balanceKey = BALANCE_KEY_PREFIX + accountId;
        long balance = readBalance(h, balanceKey);

        if (balance < amountCents) {
            Map<String, Object> err = new HashMap<>();
            err.put("success", false);
            err.put("reason", "Insufficient funds: " + balance + " cents");
            return JsonHelper.stringify(err);
        }

        balance -= amountCents;
        h.setQueryState(balanceKey, String.valueOf(balance));

        Map<String, Object> result = new HashMap<>();
        result.put("success", true);
        result.put("balance", balance);
        return JsonHelper.stringify(result);
    }

    /**
     * Get the current balance of an account.
     *
     * @param h         the {@link HostCalls} instance
     * @param rawInput JSON with key {@code "accountId"} (String)
     * @return JSON: {@code {"accountId":"...", "balance":<balance>}}
     */
    @DurableEntry(name = "account_get_balance")
    public static String getBalance(HostCalls h, String rawInput) {
        Map<String, Object> input = JsonHelper.parseObject(rawInput);
        String accountId = extractString(input, "accountId");
        if (accountId == null || accountId.isEmpty()) {
            return JsonHelper.errorJson("Account ID is required");
        }

        String balanceKey = BALANCE_KEY_PREFIX + accountId;
        long balance = readBalance(h, balanceKey);

        Map<String, Object> result = new HashMap<>();
        result.put("accountId", accountId);
        result.put("balance", balance);
        return JsonHelper.stringify(result);
    }

    // ========================================================================
    // Internal helpers
    // ========================================================================

    /**
     * Read the account balance from query state, initialising with a
     * deterministic random value if no balance exists yet.
     * <p>
     * Uses {@link HostCalls#random()} instead of {@code Math.random()}
     * to ensure deterministic replay behaviour.
     */
    private static long readBalance(HostCalls h, String balanceKey) {
        DurableResult<String> result = h.getQueryState(balanceKey);
        if (result.isOk() && result.getValue() != null && !result.getValue().isEmpty()) {
            try {
                return Long.parseLong(result.getValue());
            } catch (NumberFormatException e) {
                // Corrupted state — reset
            }
        }
        // Initialise with deterministic random balance between 100000 and 200000
        long initialBalance = 100000 + (Math.abs(h.random()) % 100001);
        h.setQueryState(balanceKey, String.valueOf(initialBalance));
        return initialBalance;
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
}
