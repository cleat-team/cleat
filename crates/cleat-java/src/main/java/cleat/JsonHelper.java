package cleat;

import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

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
 * inputs and outputs.  The annotation processor (CleatEntryProcessor)
 * generates export wrappers that handle JSON serialization/deserialization
 * through this helper.
 * <p>
 * <strong>Usage recommendation:</strong> Define workflow entry-point methods
 * that accept and return {@link String} (representing JSON).  Parse and
 * construct structured data manually using this class for simple cases, or
 * use {@link #parseObject(String)} to deserialize JSON into POJOs.
 */
public final class JsonHelper {

    private JsonHelper() {
        // Utility class — no instantiation.
    }

    /**
     * Parse a JSON string into an object of the given type.
     * <p>
     * For {@code String} targets, returns the input as-is.  For
     * {@code Map.class}, returns a {@code Map<String, Object>} via
     * {@link #parseObject(String)}.  For custom POJO types, uses
     * {@link #parseObject(String, Class)} to deserialize.
     *
     * @param json the JSON string to parse
     * @param type the target type
     * @param <T>  the type parameter
     * @return the deserialized object
     * @throws RuntimeException if parsing fails
     */
    @SuppressWarnings("unchecked")
    public static <T> T parse(String json, Class<T> type) {
        if (json == null) {
            return null;
        }
        String trimmed = json.trim();

        if (type == String.class) {
            // For String targets, return the input as-is for plain strings,
            // but properly parse JSON string literals (strip quotes and unescape).
            if (trimmed.startsWith("\"")) {
                ParseResult result = parseStringValue(trimmed, 0);
                return (T) result.value;
            }
            return (T) json;
        }
        if (type == Integer.class || type == int.class) {
            ParseResult result = parseValue(trimmed, 0);
            if (!(result.value instanceof Number)) {
                throw new RuntimeException("Expected JSON number for Integer, got: " + trimmed);
            }
            return (T) Integer.valueOf(((Number) result.value).intValue());
        }
        if (type == Long.class || type == long.class) {
            ParseResult result = parseValue(trimmed, 0);
            if (!(result.value instanceof Number)) {
                throw new RuntimeException("Expected JSON number for Long, got: " + trimmed);
            }
            return (T) Long.valueOf(((Number) result.value).longValue());
        }
        if (type == Double.class || type == double.class) {
            ParseResult result = parseValue(trimmed, 0);
            if (!(result.value instanceof Number)) {
                throw new RuntimeException("Expected JSON number for Double, got: " + trimmed);
            }
            return (T) Double.valueOf(((Number) result.value).doubleValue());
        }
        if (type == Float.class || type == float.class) {
            ParseResult result = parseValue(trimmed, 0);
            if (!(result.value instanceof Number)) {
                throw new RuntimeException("Expected JSON number for Float, got: " + trimmed);
            }
            return (T) Float.valueOf(((Number) result.value).floatValue());
        }
        if (type == Short.class || type == short.class) {
            ParseResult result = parseValue(trimmed, 0);
            if (!(result.value instanceof Number)) {
                throw new RuntimeException("Expected JSON number for Short, got: " + trimmed);
            }
            return (T) Short.valueOf(((Number) result.value).shortValue());
        }
        if (type == Boolean.class || type == boolean.class) {
            ParseResult result = parseValue(trimmed, 0);
            if (!(result.value instanceof Boolean)) {
                throw new RuntimeException("Expected JSON boolean, got: " + trimmed);
            }
            return (T) result.value;
        }
        if (type == Map.class || type == HashMap.class) {
            return (T) parseObject(json);
        }
        if (type == List.class || type == ArrayList.class) {
            return (T) parseArray(json);
        }
        if (type == Object.class) {
            // Return the raw parsed JSON value for generic consumers.
            return (T) parseValue(trimmed, 0).value;
        }
        // Attempt POJO deserialization via parseObject(String, Class).
        return parseObject(json, type);
    }

    /**
     * Parse a JSON object string into a {@code Map<String, Object>}.
     * <p>
     * This parser handles objects, arrays, strings, numbers, booleans, and
     * null.  It does <em>not</em> use {@code java.util.regex} and is safe
     * for TeaVM WASM target.
     *
     * @param json the JSON object string (e.g. {@code {"key":"value"}})
     * @return the parsed map
     * @throws RuntimeException if the input is not a valid JSON object
     */
    public static Map<String, Object> parseObject(String json) {
        if (json == null || json.trim().isEmpty()) {
            return new HashMap<>();
        }
        json = json.trim();
        if (!json.startsWith("{")) {
            throw new RuntimeException("JSON object must start with '{', got: " + json.substring(0, Math.min(10, json.length())));
        }
        return (Map<String, Object>) parseValue(json, 0).value;
    }

    /**
     * Parse a JSON object string into a POJO of the given type.
     * <p>
     * The target class must have a public no-argument constructor.  Fields
     * are populated by matching JSON keys to field names (case-sensitive).
     * Only fields whose type is supported ({@link String}, {@link Integer},
     * {@link Long}, {@link Double}, {@link Boolean}, {@link Map},
     * {@link List}, or another POJO with a no-arg constructor) are set.
     * <p>
     * This works within TeaVM's limited reflection support on the WASM
     * target.
     *
     * @param json the JSON object string
     * @param type the target POJO class
     * @param <T>  the type parameter
     * @return the deserialized POJO
     * @throws RuntimeException if parsing or instantiation fails
     */
    public static <T> T parseObject(String json, Class<T> type) {
        Map<String, Object> map = parseObject(json);
        return mapToPojo(map, type);
    }

    /**
     * Parse a JSON array string into a {@code List<Object>}.
     *
     * @param json the JSON array string (e.g. {@code ["a","b","c"]})
     * @return the parsed list
     * @throws RuntimeException if the input is not a valid JSON array
     */
    public static List<Object> parseArray(String json) {
        if (json == null || json.trim().isEmpty()) {
            return new ArrayList<>();
        }
        json = json.trim();
        if (!json.startsWith("[")) {
            throw new RuntimeException("JSON array must start with '['");
        }
        return (List<Object>) parseValue(json, 0).value;
    }

    // ---- JSON parser ----

    private static class ParseResult {
        final Object value;
        final int endIndex;

        ParseResult(Object value, int endIndex) {
            this.value = value;
            this.endIndex = endIndex;
        }
    }

    /**
     * Parse a single JSON value starting at the given index.
     */
    private static ParseResult parseValue(String json, int start) {
        int i = skipWhitespace(json, start);
        if (i >= json.length()) {
            throw new RuntimeException("Unexpected end of JSON at position " + start);
        }
        char c = json.charAt(i);
        switch (c) {
            case '{':
                return parseObjectValue(json, i);
            case '[':
                return parseArrayValue(json, i);
            case '"':
                return parseStringValue(json, i);
            case 't':
            case 'f':
                return parseBooleanValue(json, i);
            case 'n':
                return parseNullValue(json, i);
            default:
                if (c == '-' || (c >= '0' && c <= '9')) {
                    return parseNumberValue(json, i);
                }
                throw new RuntimeException("Unexpected character '" + c + "' at position " + i + "; expected '{', '[', '\"', 't', 'f', 'n', or a digit");
        }
    }

    /**
     * Parse a JSON object starting at the given index (which must be '{').
     */
    private static ParseResult parseObjectValue(String json, int start) {
        Map<String, Object> map = new HashMap<>();
        int i = skipWhitespace(json, start + 1);
        if (i < json.length() && json.charAt(i) == '}') {
            return new ParseResult(map, i + 1);
        }
        while (i < json.length()) {
            i = skipWhitespace(json, i);
            // Expect a string key
            if (json.charAt(i) != '"') {
                throw new RuntimeException("Expected string key at position " + i + ", got: '" + json.charAt(i) + "'");
            }
            ParseResult keyResult = parseStringValue(json, i);
            String key = (String) keyResult.value;
            i = keyResult.endIndex;

            i = skipWhitespace(json, i);
            if (i >= json.length() || json.charAt(i) != ':') {
                throw new RuntimeException("Expected ':' at position " + i);
            }
            i++; // skip ':'

            ParseResult valResult = parseValue(json, i);
            map.put(key, valResult.value);
            i = valResult.endIndex;

            i = skipWhitespace(json, i);
            if (i < json.length() && json.charAt(i) == ',') {
                i++; // skip ','
                continue;
            }
            if (i < json.length() && json.charAt(i) == '}') {
                return new ParseResult(map, i + 1);
            }
            throw new RuntimeException("Expected ',' or '}' at position " + i);
        }
        throw new RuntimeException("Unterminated JSON object");
    }

    /**
     * Parse a JSON array starting at the given index (which must be '[').
     */
    private static ParseResult parseArrayValue(String json, int start) {
        List<Object> list = new ArrayList<>();
        int i = skipWhitespace(json, start + 1);
        if (i < json.length() && json.charAt(i) == ']') {
            return new ParseResult(list, i + 1);
        }
        while (i < json.length()) {
            ParseResult valResult = parseValue(json, i);
            list.add(valResult.value);
            i = valResult.endIndex;

            i = skipWhitespace(json, i);
            if (i < json.length() && json.charAt(i) == ',') {
                i++; // skip ','
                continue;
            }
            if (i < json.length() && json.charAt(i) == ']') {
                return new ParseResult(list, i + 1);
            }
            throw new RuntimeException("Expected ',' or ']' at position " + i);
        }
        throw new RuntimeException("Unterminated JSON array");
    }

    /**
     * Parse a JSON string starting at the given index (which must be '"').
     */
    private static ParseResult parseStringValue(String json, int start) {
        StringBuilder sb = new StringBuilder();
        int i = start + 1; // skip opening quote
        while (i < json.length()) {
            char c = json.charAt(i);
            if (c == '"') {
                return new ParseResult(sb.toString(), i + 1);
            }
            if (c == '\\') {
                i++;
                if (i >= json.length()) break;
                char next = json.charAt(i);
                switch (next) {
                    case '"': sb.append('"'); break;
                    case '\\': sb.append('\\'); break;
                    case '/': sb.append('/'); break;
                    case 'n': sb.append('\n'); break;
                    case 'r': sb.append('\r'); break;
                    case 't': sb.append('\t'); break;
                    case 'b': sb.append('\b'); break;
                    case 'f': sb.append('\f'); break;
                    case 'u':
                        if (i + 4 < json.length()) {
                            String hex = json.substring(i + 1, i + 5);
                            try {
                                sb.append((char) Integer.parseInt(hex, 16));
                            } catch (NumberFormatException e) {
                                throw new RuntimeException("Invalid \\uXXXX escape sequence: \\u" + hex + " at position " + i);
                            }
                            i += 4;
                        }
                        break;
                    default: sb.append(next); break;
                }
            } else {
                sb.append(c);
            }
            i++;
        }
        throw new RuntimeException("Unterminated JSON string starting at position " + start);
    }

    /**
     * Parse a JSON number starting at the given index.
     */
    private static ParseResult parseNumberValue(String json, int start) {
        int end = start;
        if (end < json.length() && json.charAt(end) == '-') end++;
        while (end < json.length() && json.charAt(end) >= '0' && json.charAt(end) <= '9') end++;
        boolean isFloating = false;
        if (end < json.length() && json.charAt(end) == '.') {
            isFloating = true;
            end++;
            while (end < json.length() && json.charAt(end) >= '0' && json.charAt(end) <= '9') end++;
        }
        if (end < json.length() && (json.charAt(end) == 'e' || json.charAt(end) == 'E')) {
            isFloating = true;
            end++;
            if (end < json.length() && (json.charAt(end) == '+' || json.charAt(end) == '-')) end++;
            while (end < json.length() && json.charAt(end) >= '0' && json.charAt(end) <= '9') end++;
        }
        String numStr = json.substring(start, end);
        if (isFloating) {
            return new ParseResult(Double.parseDouble(numStr), end);
        }
        long val = Long.parseLong(numStr);
        if (val >= Integer.MIN_VALUE && val <= Integer.MAX_VALUE) {
            return new ParseResult((int) val, end);
        }
        return new ParseResult(val, end);
    }

    /**
     * Parse a JSON boolean literal starting at the given index.
     */
    private static ParseResult parseBooleanValue(String json, int start) {
        if (json.startsWith("true", start)) {
            return new ParseResult(Boolean.TRUE, start + 4);
        }
        if (json.startsWith("false", start)) {
            return new ParseResult(Boolean.FALSE, start + 5);
        }
        throw new RuntimeException("Expected boolean at position " + start + ", got: '" + json.substring(start, Math.min(start + 10, json.length())) + "'");
    }

    /**
     * Parse a JSON null literal starting at the given index.
     */
    private static ParseResult parseNullValue(String json, int start) {
        if (json.startsWith("null", start)) {
            return new ParseResult(null, start + 4);
        }
        throw new RuntimeException("Expected null at position " + start + ", got: '" + json.substring(start, Math.min(start + 10, json.length())) + "'");
    }

    /**
     * Skip whitespace characters starting at the given index.
     */
    private static int skipWhitespace(String json, int start) {
        int i = start;
        while (i < json.length()) {
            char c = json.charAt(i);
            if (c > ' ') break; // All JSON whitespace is <= ' '
            i++;
        }
        return i;
    }

    // ---- POJO mapping ----

    /**
     * Populate a POJO of the given type from a {@code Map<String, Object>}.
     * <p>
     * Uses the type's public no-argument constructor to create an instance,
     * then sets public fields whose names match JSON keys.
     *
     * @param map  the map of field names to values
     * @param type the target POJO class
     * @param <T>  the type parameter
     * @return the populated POJO
     * @throws RuntimeException if instantiation or field access fails
     */
    @SuppressWarnings("unchecked")
    private static <T> T mapToPojo(Map<String, Object> map, Class<T> type) {
        if (map == null || map.isEmpty()) {
            try {
                return type.getDeclaredConstructor().newInstance();
            } catch (Exception e) {
                throw new RuntimeException("Failed to instantiate " + type.getName() + ". Ensure the class has a public no-argument constructor.", e);
            }
        }
        try {
            T instance = type.getDeclaredConstructor().newInstance();
            for (Map.Entry<String, Object> entry : map.entrySet()) {
                String fieldName = entry.getKey();
                Object value = entry.getValue();
                try {
                    java.lang.reflect.Field field = type.getField(fieldName);
                    Object converted = convertValue(value, field.getType());
                    field.set(instance, converted);
                } catch (NoSuchFieldException ignored) {
                    // Silently skip fields that don't exist on the POJO.
                }
            }
            return instance;
        } catch (RuntimeException e) {
            throw e;
        } catch (Exception e) {
            throw new RuntimeException("Failed to instantiate " + type.getName(), e);
        }
    }

    /**
     * Convert a parsed JSON value to the target field type.
     * <p>
     * Supports widening conversions (Integer to Long, etc.) and recursive
     * POJO deserialization for {@code Map} values.
     */
    @SuppressWarnings("unchecked")
    private static Object convertValue(Object value, Class<?> targetType) {
        if (value == null) {
            return null;
        }
        if (targetType.isInstance(value)) {
            return value;
        }
        // Numeric widening
        if (value instanceof Number) {
            Number num = (Number) value;
            if (targetType == long.class || targetType == Long.class) {
                return num.longValue();
            }
            if (targetType == int.class || targetType == Integer.class) {
                return num.intValue();
            }
            if (targetType == double.class || targetType == Double.class) {
                return num.doubleValue();
            }
            if (targetType == float.class || targetType == Float.class) {
                return num.floatValue();
            }
            if (targetType == short.class || targetType == Short.class) {
                return num.shortValue();
            }
            if (targetType == byte.class || targetType == Byte.class) {
                return num.byteValue();
            }
        }
        // Boolean widening
        if (value instanceof Boolean) {
            if (targetType == boolean.class || targetType == Boolean.class) {
                return value;
            }
        }
        // String to String
        if (value instanceof String && targetType == String.class) {
            return value;
        }
        // Map to POJO
        if (value instanceof Map) {
            // Try POJO deserialization for non-Map target types
            if (targetType != Map.class && targetType != HashMap.class) {
                return mapToPojo((Map<String, Object>) value, targetType);
            }
        }
        // List to List
        if (value instanceof List && (targetType == List.class || targetType == ArrayList.class)) {
            return value;
        }
        // Fallback: try string conversion
        if (targetType == String.class) {
            return value.toString();
        }
        return value;
    }

    // ---- Serialization ----

    /**
     * Serialize an object to its JSON string representation.
     * <p>
     * This implementation handles {@link String} directly, {@link Map} and
     * {@link List} recursively, and falls back to {@link Object#toString()}
     * for other types.
     * <p>
     * <strong>Limitation:</strong> Cycle detection is not implemented.
     * If the object graph contains circular references, this method will
     * recurse infinitely and cause a {@link StackOverflowError}. A full
     * fix would require tracking visited objects (e.g., using an
     * {@link java.util.IdentityHashMap}) in the recursive helpers.
     *
     * @param obj the object to serialize
     * @return the JSON string
     */
    @SuppressWarnings("unchecked")
    public static String stringify(Object obj) {
        if (obj == null) {
            return "null";
        }
        if (obj instanceof String) {
            return "\"" + escapeJson((String) obj) + "\"";
        }
        if (obj instanceof Map) {
            return stringifyMap((Map<String, Object>) obj);
        }
        if (obj instanceof List) {
            return stringifyList((List<Object>) obj);
        }
        if (obj instanceof Boolean || obj instanceof Number) {
            return obj.toString();
        }
        return obj.toString();
    }

    /**
     * Serialize a map to a JSON object string.
     */
    private static String stringifyMap(Map<String, Object> map) {
        if (map.isEmpty()) {
            return "{}";
        }
        StringBuilder sb = new StringBuilder("{");
        boolean first = true;
        for (Map.Entry<String, Object> entry : map.entrySet()) {
            if (!first) {
                sb.append(",");
            }
            sb.append("\"").append(escapeJson(entry.getKey())).append("\":");
            sb.append(stringify(entry.getValue()));
            first = false;
        }
        sb.append("}");
        return sb.toString();
    }

    /**
     * Serialize a list to a JSON array string.
     */
    private static String stringifyList(List<Object> list) {
        if (list.isEmpty()) {
            return "[]";
        }
        StringBuilder sb = new StringBuilder("[");
        for (int i = 0; i < list.size(); i++) {
            if (i > 0) {
                sb.append(",");
            }
            sb.append(stringify(list.get(i)));
        }
        sb.append("]");
        return sb.toString();
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
     * <p>
     * This implementation is TeaVM-safe: it uses character-by-character
     * processing with no regex or {@code String.format} calls.
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
                        // Control characters: encode as \\uXXXX without
                        // using String.format (TeaVM-safe).
                        sb.append("\\u");
                        sb.append(hex4(c));
                    } else {
                        sb.append(c);
                    }
                    break;
            }
        }
        return sb.toString();
    }

    /**
     * Convert a character (0-65535) to a 4-digit hex string.
     * TeaVM-safe: no regex or String.format.
     */
    private static String hex4(int value) {
        char[] hex = new char[4];
        String digits = "0123456789abcdef";
        for (int i = 3; i >= 0; i--) {
            hex[i] = digits.charAt(value & 0xF);
            value >>= 4;
        }
        return new String(hex);
    }

    /**
     * Unescape a JSON string value (reverse of {@link #escapeJson}).
     * <p>
     * Handles the standard JSON escape sequences plus \\uXXXX unicode
     * escapes.
     *
     * @param s the escaped JSON string (without surrounding quotes)
     * @return the unescaped string
     */
    public static String unescapeJson(String s) {
        if (s == null || s.isEmpty()) {
            return s;
        }
        StringBuilder sb = new StringBuilder(s.length());
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            if (c == '\\' && i + 1 < s.length()) {
                char next = s.charAt(i + 1);
                switch (next) {
                    case '"': sb.append('"'); i++; break;
                    case '\\': sb.append('\\'); i++; break;
                    case '/': sb.append('/'); i++; break;
                    case 'n': sb.append('\n'); i++; break;
                    case 'r': sb.append('\r'); i++; break;
                    case 't': sb.append('\t'); i++; break;
                    case 'b': sb.append('\b'); i++; break;
                    case 'f': sb.append('\f'); i++; break;
                    case 'u':
                        if (i + 5 < s.length()) {
                            String hex = s.substring(i + 2, i + 6);
                            sb.append((char) Integer.parseInt(hex, 16));
                            i += 5;
                        }
                        break;
                    default: sb.append(c); break;
                }
            } else {
                sb.append(c);
            }
        }
        return sb.toString();
    }
}
