package cleat;

/**
 * Minimal {@link String#format(String, Object...) String.format} equivalent
 * that is safe for TeaVM WASM compilation.
 * <p>
 * TeaVM does not support {@link String#format(String, Object...)}.  This
 * class provides a lightweight replacement that supports a limited subset of
 * format specifiers using only {@link StringBuilder} internally -- no
 * reflection, no regex, and no {@code String.format} calls.
 * </p>
 * <h3>Supported format specifiers</h3>
 * <table>
 *   <caption>Format specifiers</caption>
 *   <tr><th>Specifier</th><th>Description</th><th>Example</th></tr>
 *   <tr><td>{@code %s}</td><td>String substitution.  Calls
 *       {@link String#valueOf(Object)} on the argument.</td>
 *       <td>{@code format("Hello %s", "World") &rarr; "Hello World"}</td></tr>
 *   <tr><td>{@code %d}</td><td>Integer substitution.  Formats the argument as
 *       a decimal integer.  The argument should be an {@link Integer},
 *       {@link Long}, {@link Short}, or {@link Byte}; otherwise
 *       {@link String#valueOf(Object)} is used.</td>
 *       <td>{@code format("Count: %d", 42) &rarr; "Count: 42"}</td></tr>
 *   <tr><td>{@code %%}</td><td>Literal percent sign.</td>
 *       <td>{@code format("100%%") &rarr; "100%"}</td></tr>
 * </table>
 * <h3>Usage</h3>
 * <pre>{@code
 * String msg = StringFormat.format("Hello %s, you have %d messages", name, count);
 * }</pre>
 * <h3>Thread safety</h3>
 * This class is stateless and thread-safe.
 */
public final class StringFormat {

    private StringFormat() {
        // Utility class -- no instantiation.
    }

    /**
     * Format a pattern string with the given arguments.
     * <p>
     * Parses the pattern character-by-character.  When a {@code '%'} is
     * encountered, the next character determines the substitution:
     * <ul>
     *   <li>{@code s} -- calls {@link String#valueOf(Object)} on the next
     *       argument and appends the result</li>
     *   <li>{@code d} -- formats the next argument as a decimal integer
     *       (see {@link #formatInt(Object)})</li>
     *   <li>{@code %} -- appends a literal {@code '%'}</li>
     *   <li>any other character -- the {@code '%'} is appended as-is followed
     *       by the character</li>
     * </ul>
     * If there are fewer arguments than format specifiers, the pattern
     * specifier (e.g. {@code "%s"}) is appended literally.  Extra arguments
     * beyond those consumed are ignored.
     *
     * @param pattern the format pattern string (if null, returns null)
     * @param args    the arguments to substitute into the pattern
     * @return the formatted string (never null unless pattern is null)
     */
    public static String format(String pattern, Object... args) {
        if (pattern == null) {
            return null;
        }
        StringBuilder sb = new StringBuilder(pattern.length() + 64);
        int argIndex = 0;
        int i = 0;

        while (i < pattern.length()) {
            char c = pattern.charAt(i);

            if (c == '%' && i + 1 < pattern.length()) {
                char next = pattern.charAt(i + 1);
                switch (next) {
                    case 's':
                        if (argIndex < args.length) {
                            sb.append(String.valueOf(args[argIndex]));
                            argIndex++;
                        } else {
                            sb.append("%s");
                        }
                        i += 2;
                        break;
                    case 'd':
                        if (argIndex < args.length) {
                            sb.append(formatInt(args[argIndex]));
                            argIndex++;
                        } else {
                            sb.append("%d");
                        }
                        i += 2;
                        break;
                    case '%':
                        sb.append('%');
                        i += 2;
                        break;
                    default:
                        // Unsupported specifier: keep the '%' and the
                        // character as-is.
                        sb.append('%');
                        sb.append(next);
                        i += 2;
                        break;
                }
            } else {
                sb.append(c);
                i++;
            }
        }

        return sb.toString();
    }

    /**
     * Format an argument as a decimal integer.
     * <p>
     * If the argument is {@link Integer}, {@link Long}, {@link Short}, or
     * {@link Byte}, the corresponding {@code toString()} method is used to
     * produce the decimal representation.  Otherwise,
     * {@link String#valueOf(Object)} is used as a fallback.
     */
    private static String formatInt(Object arg) {
        if (arg instanceof Integer) {
            return Integer.toString((Integer) arg);
        }
        if (arg instanceof Long) {
            return Long.toString((Long) arg);
        }
        if (arg instanceof Short) {
            return Short.toString((Short) arg);
        }
        if (arg instanceof Byte) {
            return Byte.toString((Byte) arg);
        }
        return String.valueOf(arg);
    }
}
