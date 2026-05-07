package cleatexample.statemachine.types;

/**
 * A payment request containing the target account and amount.
 * <p>
 * Designed for JsonHelper POJO deserialisation: public no-arg constructor
 * and public fields matching JSON keys.
 */
public class Payment {

    /** The target account identifier. */
    public String accountId;

    /** The payment amount in cents (must be positive). */
    public long amountCents;

    /** Default constructor for JsonHelper deserialisation. */
    public Payment() {
    }

    /**
     * Construct a validated payment.
     *
     * @param accountId  the target account (must not be null or empty)
     * @param amountCents the amount in cents (must be &gt; 0)
     */
    public Payment(String accountId, long amountCents) {
        if (accountId == null || accountId.isEmpty()) {
            throw new IllegalArgumentException("Account ID is required");
        }
        if (amountCents <= 0) {
            throw new IllegalArgumentException("Amount must be greater than 0");
        }
        this.accountId = accountId;
        this.amountCents = amountCents;
    }

    public String getAccountId() {
        return accountId;
    }

    public long getAmountCents() {
        return amountCents;
    }
}
