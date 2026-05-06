package cleat;

/**
 * Minimal JSON helper for the cleat Java SDK.
 * <p>
 * Under TeaVM targeting WASM, full reflection-based JSON libraries are not
 * available. This helper provides basic JSON operations suitable for
 * workflow I/O where inputs and outputs are JSON strings.
 * <p>
 * In a full production implementation, this would use TeaVM's JSO
 * (JavaScript Object) interop or a bundled minimal JSON parser. For now,
 * workflows pass JSON strings directly and use String types for their
 * inputs and outputs.  The annotation processor (DurableEntryProcessor)
 * generates export wrappers that handle JSON serialization/deserialization
 * through this helper.
 * <p>
 * <strong>Usage recommendation:</strong> Define workflow entry-point methods
 * that accept and return {@link String} (representing JSON).  Parse and
 * construct structured data manually using this class for simple cases, or
 * extend JsonHelper with a proper parser for complex types.
 */
public final class JsonHelper {

    private JsonHelper() {
        // Utility class — no instantiation.
    }

    /**
     * Parse a JSON string into an object of the given type.
     * <p>
     * For {@code String} targets, returns the input as-is.  For other types,
     * this default implementation throws {@link UnsupportedOperationException}.
     * Workflow authors who need structured deserialization should override or
     * extend this method to parse JSON into their domain types.
     *
     * @param json the JSON string to parse
     * @param type the target type
     * @param <T>  the type parameter
     * @return the deserialized object
     * @throws UnsupportedOperationException if parsing for the given type is
     *                                       not yet implemented
     */
    @SuppressWarnings("unchecked")
    public static <T> T parse(String json, Class<T> type) {
        if (json == null) {
            return null;
        }
        // For strings, return as-is (workflows use String input/output).
        if (type == String.class) {
            return (T) json;
        }
        // For other types, TeaVM JSO can be used when targeting JS,
        // but for WASM we need a bundled parser.
        throw new UnsupportedOperationException(
            "JSON parsing for " + type.getName() + " not yet implemented. "
                + "Use String types for workflow I/O.");
    }

    /**
     * Serialize an object to its JSON string representation.
     * <p>
     * This implementation handles {@link String} directly and falls back
     * to {@link Object#toString()} for other types. Override or extend
     * when structured serialization is needed.
     *
     * @param obj the object to serialize
     * @return the JSON string
     */
    public static String stringify(Object obj) {
        if (obj == null) {
            return "null";
        }
        if (obj instanceof String) {
            // Wrap strings in JSON quotes and escape special characters.
            return "\"" + escapeJson((String) obj) + "\"";
        }
        // For other types, use toString() and assume it returns valid JSON.
        // This is acceptable for simple types like numbers and booleans.
        return obj.toString();
    }

    /**
     * Construct a JSON error object string.
     *
     * @param message the error message
     * @return a JSON string of the form {@code {"error":"<message>"}}
     */
    public static String errorJson(String message) {
        return "{\"error\":\"" + escapeJson(message) + "\"}";
    }

    /**
     * Escape special characters for embedding in a JSON string value.
     *
     * @param s the raw string
     * @return the escaped string (safe for JSON)
     */
    public static String escapeJson(String s) {
        if (s == null || s.isEmpty()) {
            return "";
        }
        StringBuilder sb = new StringBuilder(s.length());
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            switch (c) {
                case '"':
                    sb.append("\\\"");
                    break;
                case '\\':
                    sb.append("\\\\");
                    break;
                case '\n':
                    sb.append("\\n");
                    break;
                case '\r':
                    sb.append("\\r");
                    break;
                case '\t':
                    sb.append("\\t");
                    break;
                case '\b':
                    sb.append("\\b");
                    break;
                case '\f':
                    sb.append("\\f");
                    break;
                default:
                    if (c < 0x20) {
                        // Control characters: encode as \uXXXX.
                        sb.append("\\u");
                        sb.append(String.format("%04x", (int) c));
                    } else {
                        sb.append(c);
                    }
                    break;
            }
        }
        return sb.toString();
    }
}
