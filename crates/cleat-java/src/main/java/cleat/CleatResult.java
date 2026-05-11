package cleat;

/**
 * A simple result type for cleat workflow operations.
 * Encapsulates a success value or an error message.
 * Inspired by Rust's {@code Result<T, E>} pattern, but simplified for
 * the cleat Java SDK.
 *
 * @param <T> the type of the success value
 */
public class CleatResult<T> {

    private final T value;
    private final String error;

    private CleatResult(T value, String error) {
        this.value = value;
        this.error = error;
    }

    /**
     * Create a success result wrapping the given value.
     *
     * @param value the success value
     * @param <T>   the type of the value
     * @return a new {@code CleatResult} representing success
     */
    public static <T> CleatResult<T> ok(T value) {
        return new CleatResult<>(value, null);
    }

    /**
     * Create an error result with the given error message.
     *
     * @param error the error description
     * @param <T>   the type parameter (inferred from context)
     * @return a new {@code CleatResult} representing failure
     */
    public static <T> CleatResult<T> err(String error) {
        return new CleatResult<>(null, error);
    }

    /**
     * Returns {@code true} if this result represents a success.
     */
    public boolean isOk() {
        return error == null;
    }

    /**
     * Returns {@code true} if this result represents an error.
     */
    public boolean isErr() {
        return error != null;
    }

    /**
     * Returns the success value, or {@code null} if this is an error result.
     */
    public T getValue() {
        return value;
    }

    /**
     * Returns the error message, or {@code null} if this is a success result.
     */
    public String getError() {
        return error;
    }

    /**
     * Unwraps the value as a string, returning a default if this is an error
     * or the value is null.
     *
     * @param defaultValue the fallback string
     * @return the string representation of the value, or the default
     */
    public String unwrapOrElse(String defaultValue) {
        if (isOk() && value != null) {
            return value.toString();
        }
        return defaultValue;
    }

    @Override
    public String toString() {
        if (isOk()) {
            return "Ok(" + (value != null ? value : "null") + ")";
        }
        return "Err(" + error + ")";
    }

    @Override
    public boolean equals(Object o) {
        if (this == o) return true;
        if (!(o instanceof CleatResult)) return false;
        CleatResult<?> that = (CleatResult<?>) o;
        if (isOk() != that.isOk()) return false;
        if (isOk()) {
            return value != null ? value.equals(that.value) : that.value == null;
        }
        return error != null ? error.equals(that.error) : that.error == null;
    }

    @Override
    public int hashCode() {
        if (isOk()) {
            return value != null ? value.hashCode() : 0;
        }
        return 31 + (error != null ? error.hashCode() : 0);
    }
}
