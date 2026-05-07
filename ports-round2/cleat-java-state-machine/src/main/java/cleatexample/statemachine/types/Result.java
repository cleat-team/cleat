package cleatexample.statemachine.types;

/**
 * Simple operation result with success/failure flag and reason message.
 * <p>
 * Designed for JsonHelper POJO serialisation: public fields.
 */
public class Result {

    /** Whether the operation succeeded. */
    public boolean success;

    /** Human-readable reason or error message. */
    public String reason;

    /** Default constructor for JsonHelper deserialisation. */
    public Result() {
    }

    /**
     * Construct a result.
     *
     * @param success whether the operation succeeded
     * @param reason  human-readable reason or error message
     */
    public Result(boolean success, String reason) {
        this.success = success;
        this.reason = reason;
    }

    public boolean isSuccess() {
        return success;
    }

    public String getReason() {
        return reason;
    }
}
