package cleat;

import static org.junit.jupiter.api.Assertions.*;
import org.junit.jupiter.api.Test;

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
    void testParseUnsupportedTypeThrows() {
        assertThrows(UnsupportedOperationException.class,
            () -> JsonHelper.parse("42", Integer.class));
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
