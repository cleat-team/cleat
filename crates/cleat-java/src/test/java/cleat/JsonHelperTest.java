package cleat;

import static org.junit.jupiter.api.Assertions.*;
import org.junit.jupiter.api.Test;
import java.util.Map;

/**
 * Unit tests for {@link JsonHelper}.
 */
class JsonHelperTest {

    // ---- parse ----

    @Test
    void testParseString() {
        assertEquals("hello", JsonHelper.parse("\"hello\"", String.class));
    }

    @Test
    void testParseNullString() {
        assertNull(JsonHelper.parse(null, String.class));
    }

    @Test
    void testParseInteger() {
        assertEquals(42, JsonHelper.parse("42", Integer.class));
    }

    @Test
    void testParseIntegerNegative() {
        assertEquals(-10, JsonHelper.parse("-10", Integer.class));
    }

    @Test
    void testParseLong() {
        assertEquals(9000000000000L, JsonHelper.parse("9000000000000", Long.class));
    }

    @Test
    void testParseDouble() {
        assertEquals(3.14, JsonHelper.parse("3.14", Double.class), 1e-10);
    }

    @Test
    void testParseDoubleInteger() {
        assertEquals(42.0, JsonHelper.parse("42.0", Double.class), 1e-10);
    }

    @Test
    void testParseScientificNotation() {
        assertEquals(1.5e10, JsonHelper.parse("1.5e10", Double.class), 1e0);
    }

    @Test
    void testParseBooleanTrue() {
        assertEquals(true, JsonHelper.parse("true", Boolean.class));
    }

    @Test
    void testParseBooleanFalse() {
        assertEquals(false, JsonHelper.parse("false", Boolean.class));
    }

    @Test
    void testParseObject() {
        assertEquals("world", JsonHelper.parse("{\"hello\":\"world\"}", Map.class).get("hello"));
    }

    @Test
    void testParseObjectGeneric() {
        Object val = JsonHelper.parse("{\"key\":42}", Object.class);
        assertTrue(val instanceof Map);
        assertEquals(42, ((Map<String, Object>) val).get("key"));
    }

    @Test
    void testParseNull() {
        assertNull(JsonHelper.parse("null", Object.class));
    }

    @Test
    void testParsePrimitiveIntThrowsOnString() {
        assertThrows(RuntimeException.class,
            () -> JsonHelper.parse("\"hello\"", Integer.class));
    }

    @Test
    void testParsePrimitiveDoubleThrowsOnString() {
        assertThrows(RuntimeException.class,
            () -> JsonHelper.parse("\"hello\"", Double.class));
    }

    // ---- stringify ----

    @Test
    void testStringifyNull() {
        assertEquals("null", JsonHelper.stringify(null));
    }

    @Test
    void testStringifyString() {
        assertEquals("\"hello\"", JsonHelper.stringify("hello"));
    }

    @Test
    void testStringifyStringWithEscaping() {
        assertEquals("\"hello\\nworld\"", JsonHelper.stringify("hello\nworld"));
    }

    @Test
    void testStringifyInteger() {
        assertEquals("42", JsonHelper.stringify(42));
    }

    @Test
    void testStringifyBoolean() {
        assertEquals("true", JsonHelper.stringify(true));
    }

    // ---- escapeJson ----

    @Test
    void testEscapeJsonPlain() {
        assertEquals("hello", JsonHelper.escapeJson("hello"));
    }

    @Test
    void testEscapeJsonNull() {
        assertEquals("", JsonHelper.escapeJson(null));
    }

    @Test
    void testEscapeJsonEmpty() {
        assertEquals("", JsonHelper.escapeJson(""));
    }

    @Test
    void testEscapeJsonQuotes() {
        assertEquals("say \\\"hello\\\"", JsonHelper.escapeJson("say \"hello\""));
    }

    @Test
    void testEscapeJsonBackslash() {
        assertEquals("path\\\\to\\\\file", JsonHelper.escapeJson("path\\to\\file"));
    }

    @Test
    void testEscapeJsonNewline() {
        assertEquals("line1\\nline2", JsonHelper.escapeJson("line1\nline2"));
    }

    @Test
    void testEscapeJsonCarriageReturn() {
        assertEquals("line1\\rline2", JsonHelper.escapeJson("line1\rline2"));
    }

    @Test
    void testEscapeJsonTab() {
        assertEquals("col1\\tcol2", JsonHelper.escapeJson("col1\tcol2"));
    }

    @Test
    void testEscapeJsonBackspace() {
        assertEquals("abc\\bdef", JsonHelper.escapeJson("abc\bdef"));
    }

    @Test
    void testEscapeJsonFormFeed() {
        assertEquals("abc\\fdef", JsonHelper.escapeJson("abc\fdef"));
    }

    @Test
    void testEscapeJsonControlChar() {
        String input = "a" + (char) 0x01 + "b";
        assertEquals("a\\u0001b", JsonHelper.escapeJson(input));
    }

    @Test
    void testEscapeJsonMultipleSpecials() {
        String input = "hello \"world\"\nand\ttab\\stuff";
        String expected = "hello \\\"world\\\"\\nand\\ttab\\\\stuff";
        assertEquals(expected, JsonHelper.escapeJson(input));
    }

    // ---- errorJson ----

    @Test
    void testErrorJson() {
        assertEquals("{\"error\":\"something broke\"}", JsonHelper.errorJson("something broke"));
    }

    @Test
    void testErrorJsonEscapesMessage() {
        assertEquals("{\"error\":\"say \\\"hi\\\"\"}", JsonHelper.errorJson("say \"hi\""));
    }
}
