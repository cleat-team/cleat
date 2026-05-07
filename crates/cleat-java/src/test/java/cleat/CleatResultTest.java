package cleat;

import static org.junit.jupiter.api.Assertions.*;
import org.junit.jupiter.api.Test;

/**
 * Unit tests for {@link CleatResult}.
 */
class CleatResultTest {

    @Test
    void testOk() {
        CleatResult<String> result = CleatResult.ok("hello");
        assertTrue(result.isOk());
        assertFalse(result.isErr());
        assertEquals("hello", result.getValue());
        assertNull(result.getError());
    }

    @Test
    void testErr() {
        CleatResult<Integer> result = CleatResult.err("something broke");
        assertFalse(result.isOk());
        assertTrue(result.isErr());
        assertNull(result.getValue());
        assertEquals("something broke", result.getError());
    }

    @Test
    void testOkNullValue() {
        CleatResult<String> result = CleatResult.ok(null);
        assertTrue(result.isOk());
        assertNull(result.getValue());
    }

    @Test
    void testOkWithVoid() {
        CleatResult<Void> result = CleatResult.ok(null);
        assertTrue(result.isOk());
        assertNull(result.getValue());
    }

    @Test
    void testUnwrapOrElseOk() {
        CleatResult<String> result = CleatResult.ok("value");
        assertEquals("value", result.unwrapOrElse("default"));
    }

    @Test
    void testUnwrapOrElseErr() {
        CleatResult<String> result = CleatResult.err("error");
        assertEquals("default", result.unwrapOrElse("default"));
    }

    @Test
    void testUnwrapOrElseNullValue() {
        CleatResult<String> result = CleatResult.ok(null);
        assertEquals("default", result.unwrapOrElse("default"));
    }

    @Test
    void testEquals() {
        CleatResult<String> a = CleatResult.ok("x");
        CleatResult<String> b = CleatResult.ok("x");
        assertEquals(a, b);
        assertEquals(a.hashCode(), b.hashCode());
    }

    @Test
    void testNotEquals() {
        CleatResult<String> a = CleatResult.ok("x");
        CleatResult<String> b = CleatResult.err("x");
        assertNotEquals(a, b);
        assertNotEquals(a.hashCode(), b.hashCode());
    }

    @Test
    void testToString() {
        assertEquals("Ok(value)", CleatResult.ok("value").toString());
        assertEquals("Err(error)", CleatResult.err("error").toString());
    }
}
