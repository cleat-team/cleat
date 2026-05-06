package cleat;

import static org.junit.jupiter.api.Assertions.*;
import org.junit.jupiter.api.Test;

/**
 * Unit tests for {@link DurableResult}.
 */
class DurableResultTest {

    @Test
    void testOk() {
        DurableResult<String> result = DurableResult.ok("hello");
        assertTrue(result.isOk());
        assertFalse(result.isErr());
        assertEquals("hello", result.getValue());
        assertNull(result.getError());
    }

    @Test
    void testErr() {
        DurableResult<Integer> result = DurableResult.err("something broke");
        assertFalse(result.isOk());
        assertTrue(result.isErr());
        assertNull(result.getValue());
        assertEquals("something broke", result.getError());
    }

    @Test
    void testOkNullValue() {
        DurableResult<String> result = DurableResult.ok(null);
        assertTrue(result.isOk());
        assertNull(result.getValue());
    }

    @Test
    void testOkWithVoid() {
        DurableResult<Void> result = DurableResult.ok(null);
        assertTrue(result.isOk());
        assertNull(result.getValue());
    }

    @Test
    void testUnwrapOrElseOk() {
        DurableResult<String> result = DurableResult.ok("value");
        assertEquals("value", result.unwrapOrElse("default"));
    }

    @Test
    void testUnwrapOrElseErr() {
        DurableResult<String> result = DurableResult.err("error");
        assertEquals("default", result.unwrapOrElse("default"));
    }

    @Test
    void testUnwrapOrElseNullValue() {
        DurableResult<String> result = DurableResult.ok(null);
        assertEquals("default", result.unwrapOrElse("default"));
    }

    @Test
    void testEquals() {
        DurableResult<String> a = DurableResult.ok("x");
        DurableResult<String> b = DurableResult.ok("x");
        assertEquals(a, b);
        assertEquals(a.hashCode(), b.hashCode());
    }

    @Test
    void testNotEquals() {
        DurableResult<String> a = DurableResult.ok("x");
        DurableResult<String> b = DurableResult.err("x");
        assertNotEquals(a, b);
        assertNotEquals(a.hashCode(), b.hashCode());
    }

    @Test
    void testToString() {
        assertEquals("Ok(value)", DurableResult.ok("value").toString());
        assertEquals("Err(error)", DurableResult.err("error").toString());
    }
}
