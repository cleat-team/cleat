package cleat;

/**
 * Fluent JSON builder for constructing JSON strings without manual
 * {@link StringBuilder} usage.
 * <p>
 * TeaVM-safe: uses only {@link StringBuilder}, no reflection, no
 * {@link String#format(String, Object...) String.format}.
 * </p>
 * <h3>Usage</h3>
 * <pre>{@code
 * String json = new JsonBuilder()
 *     .add("severity", "critical")
 *     .add("confidence", "high")
 *     .add("summary", "CPU saturated")
 *     .build();
 * // => {"severity":"critical","confidence":"high","summary":"CPU saturated"}
 *
 * // Nested objects and arrays:
 * String nested = new JsonBuilder()
 *     .add("name", "alice")
 *     .addObject("metadata", new JsonBuilder()
 *         .add("role", "admin")
 *         .add("active", true))
 *     .addArray("tags",
 *         new JsonBuilder().add("id", "a"),
 *         new JsonBuilder().add("id", "b"))
 *     .build();
 * // => {"name":"alice","metadata":{"role":"admin","active":true},"tags":[{"id":"a"},{"id":"b"}]}
 * }</pre>
 *
 * @see JsonHelper
 */
public class JsonBuilder {

    private final StringBuilder sb;
    private boolean hasContent;

    /**
     * Create a new, empty JSON builder.
     * <p>
     * The opening brace is written immediately; {@link #build()} closes it.
     */
    public JsonBuilder() {
        this.sb = new StringBuilder();
        this.hasContent = false;
    }

    // ========================================================================
    // add methods — primitive and String values
    // ========================================================================

    /**
     * Add a string key-value pair to the JSON object.
     * <p>
     * The value is properly JSON-escaped (quotes, backslashes, newlines, etc.).
     *
     * @param key   the JSON key (must not be null)
     * @param value the JSON string value (may be null, in which case the JSON
     *              literal {@code null} is written)
     * @return {@code this} for fluent chaining
     */
    public JsonBuilder add(String key, String value) {
        appendKey(key);
        if (value == null) {
            sb.append("null");
        } else {
            sb.append('"').append(JsonHelper.escapeJson(value)).append('"');
        }
        return this;
    }

    /**
     * Add an integer key-value pair to the JSON object.
     *
     * @param key   the JSON key (must not be null)
     * @param value the integer value
     * @return {@code this} for fluent chaining
     */
    public JsonBuilder add(String key, int value) {
        appendKey(key);
        sb.append(value);
        return this;
    }

    /**
     * Add a boolean key-value pair to the JSON object.
     *
     * @param key   the JSON key (must not be null)
     * @param value the boolean value
     * @return {@code this} for fluent chaining
     */
    public JsonBuilder add(String key, boolean value) {
        appendKey(key);
        sb.append(value);
        return this;
    }

    /**
     * Add a long integer key-value pair to the JSON object.
     *
     * @param key   the JSON key (must not be null)
     * @param value the long integer value
     * @return {@code this} for fluent chaining
     */
    public JsonBuilder add(String key, long value) {
        appendKey(key);
        sb.append(value);
        return this;
    }

    /**
     * Add a double-precision floating-point key-value pair to the JSON object.
     * <p>
     * The value is written using {@link Double#toString(double)}, which
     * produces a JSON-compatible number representation.  NaN, positive
     * infinity, and negative infinity are written as the JSON literals
     * {@code null}, {@code "Infinity"}, and {@code "-Infinity"} respectively,
     * following {@link Double#toString(double)} conventions.
     *
     * @param key   the JSON key (must not be null)
     * @param value the double value
     * @return {@code this} for fluent chaining
     */
    public JsonBuilder add(String key, double value) {
        appendKey(key);
        sb.append(value);
        return this;
    }

    // ========================================================================
    // addObject — nested object
    // ========================================================================

    /**
     * Add a nested JSON object value for the given key.
     * <p>
     * The provided {@link JsonBuilder} is fully built and its content is
     * embedded as a nested object value.  The sub-builder can be reused after
     * this call; its state is unaffected.
     *
     * @param key     the JSON key (must not be null)
     * @param builder the {@link JsonBuilder} producing the nested object value
     *                (must not be null)
     * @return {@code this} for fluent chaining
     */
    public JsonBuilder addObject(String key, JsonBuilder builder) {
        if (builder == null) {
            return this;
        }
        appendKey(key);
        sb.append(builder.build());
        return this;
    }

    // ========================================================================
    // addArray — array values
    // ========================================================================

    /**
     * Add a JSON array value containing the given object elements.
     * <p>
     * Each element is produced by calling {@link #build()} on the
     * corresponding {@link JsonBuilder}.  The result is a JSON array of
     * objects: {@code [{"k":"v"}, ...]}.
     *
     * @param key      the JSON key (must not be null)
     * @param elements zero or more {@link JsonBuilder} instances, each
     *                 representing one array element
     * @return {@code this} for fluent chaining
     */
    public JsonBuilder addArray(String key, JsonBuilder... elements) {
        appendKey(key);
        sb.append('[');
        for (int i = 0; i < elements.length; i++) {
            if (i > 0) {
                sb.append(',');
            }
            sb.append(elements[i].build());
        }
        sb.append(']');
        return this;
    }

    /**
     * Add a JSON array of strings for the given key.
     * <p>
     * Each string value is properly JSON-escaped.
     *
     * @param key    the JSON key (must not be null)
     * @param values zero or more string values to include in the array
     * @return {@code this} for fluent chaining
     */
    public JsonBuilder addArray(String key, String... values) {
        appendKey(key);
        sb.append('[');
        for (int i = 0; i < values.length; i++) {
            if (i > 0) {
                sb.append(',');
            }
            if (values[i] == null) {
                sb.append("null");
            } else {
                sb.append('"').append(JsonHelper.escapeJson(values[i])).append('"');
            }
        }
        sb.append(']');
        return this;
    }

    /**
     * Add a JSON array of integers for the given key.
     *
     * @param key    the JSON key (must not be null)
     * @param values zero or more integer values to include in the array
     * @return {@code this} for fluent chaining
     */
    public JsonBuilder addArray(String key, int... values) {
        appendKey(key);
        sb.append('[');
        for (int i = 0; i < values.length; i++) {
            if (i > 0) {
                sb.append(',');
            }
            sb.append(values[i]);
        }
        sb.append(']');
        return this;
    }

    /**
     * Add a JSON array of long integers for the given key.
     *
     * @param key    the JSON key (must not be null)
     * @param values zero or more long integer values to include in the array
     * @return {@code this} for fluent chaining
     */
    public JsonBuilder addArray(String key, long... values) {
        appendKey(key);
        sb.append('[');
        for (int i = 0; i < values.length; i++) {
            if (i > 0) {
                sb.append(',');
            }
            sb.append(values[i]);
        }
        sb.append(']');
        return this;
    }

    // ========================================================================
    // build
    // ========================================================================

    /**
     * Build the JSON object string.
     * <p>
     * Returns the accumulated key-value pairs wrapped in {@code {}}.
     * If no key-value pairs have been added, returns {@code "{}"}.
     *
     * @return the JSON object string (never null)
     */
    public String build() {
        return "{" + sb.toString() + "}";
    }

    /**
     * Returns the JSON object string (same as {@link #build()}).
     *
     * @return the JSON object string
     */
    @Override
    public String toString() {
        return build();
    }

    // ========================================================================
    // Internal helpers
    // ========================================================================

    /**
     * Append a comma separator if needed, then the quoted-and-escaped key
     * followed by {@code :}.
     */
    private void appendKey(String key) {
        if (key == null) {
            throw new IllegalArgumentException("JSON key must not be null");
        }
        if (hasContent) {
            sb.append(',');
        }
        sb.append('"').append(JsonHelper.escapeJson(key)).append('"').append(':');
        hasContent = true;
    }
}
