package cleat;

import static org.junit.jupiter.api.Assertions.*;
import org.junit.jupiter.api.Test;

/**
 * Unit tests for {@link JsonBuilder}.
 */
class JsonBuilderTest {

    // ---- Empty ----

    @Test
    void testEmptyObject() {
        assertEquals("{}", new JsonBuilder().build());
    }

    // ---- String values ----

    @Test
    void testAddString() {
        String json = new JsonBuilder()
            .add("severity", "critical")
            .build();
        assertEquals("{\"severity\":\"critical\"}", json);
    }

    @Test
    void testAddStringEscapes() {
        String json = new JsonBuilder()
            .add("msg", "hello \"world\"\nline2")
            .build();
        assertEquals("{\"msg\":\"hello \\\"world\\\"\\nline2\"}", json);
    }

    @Test
    void testAddStringNull() {
        String json = new JsonBuilder()
            .add("key", (String) null)
            .build();
        assertEquals("{\"key\":null}", json);
    }

    @Test
    void testAddStringWithBackslash() {
        String json = new JsonBuilder()
            .add("path", "C:\\Users\\test")
            .build();
        assertEquals("{\"path\":\"C:\\\\Users\\\\test\"}", json);
    }

    // ---- Numeric values ----

    @Test
    void testAddInt() {
        String json = new JsonBuilder()
            .add("count", 42)
            .build();
        assertEquals("{\"count\":42}", json);
    }

    @Test
    void testAddIntNegative() {
        String json = new JsonBuilder()
            .add("delta", -10)
            .build();
        assertEquals("{\"delta\":-10}", json);
    }

    @Test
    void testAddIntZero() {
        String json = new JsonBuilder()
            .add("count", 0)
            .build();
        assertEquals("{\"count\":0}", json);
    }

    @Test
    void testAddLong() {
        String json = new JsonBuilder()
            .add("big", 9000000000000L)
            .build();
        assertEquals("{\"big\":9000000000000}", json);
    }

    @Test
    void testAddLongNegative() {
        String json = new JsonBuilder()
            .add("deficit", -1234567890123L)
            .build();
        assertEquals("{\"deficit\":-1234567890123}", json);
    }

    @Test
    void testAddDouble() {
        String json = new JsonBuilder()
            .add("pi", 3.14159)
            .build();
        assertEquals("{\"pi\":3.14159}", json);
    }

    // ---- Boolean values ----

    @Test
    void testAddBooleanTrue() {
        String json = new JsonBuilder()
            .add("active", true)
            .build();
        assertEquals("{\"active\":true}", json);
    }

    @Test
    void testAddBooleanFalse() {
        String json = new JsonBuilder()
            .add("active", false)
            .build();
        assertEquals("{\"active\":false}", json);
    }

    // ---- Multiple fields ----

    @Test
    void testMultipleFields() {
        String json = new JsonBuilder()
            .add("severity", "critical")
            .add("confidence", "high")
            .add("summary", "CPU saturated")
            .build();
        assertEquals(
            "{\"severity\":\"critical\",\"confidence\":\"high\",\"summary\":\"CPU saturated\"}",
            json);
    }

    @Test
    void testMixedTypes() {
        String json = new JsonBuilder()
            .add("name", "test")
            .add("count", 100)
            .add("active", true)
            .add("size", 5000L)
            .build();
        assertEquals(
            "{\"name\":\"test\",\"count\":100,\"active\":true,\"size\":5000}",
            json);
    }

    // ---- Nested objects ----

    @Test
    void testAddObject() {
        String json = new JsonBuilder()
            .add("name", "alice")
            .addObject("metadata", new JsonBuilder()
                .add("role", "admin")
                .add("active", true))
            .build();
        assertEquals(
            "{\"name\":\"alice\",\"metadata\":{\"role\":\"admin\",\"active\":true}}",
            json);
    }

    @Test
    void testAddObjectEmpty() {
        String json = new JsonBuilder()
            .addObject("empty", new JsonBuilder())
            .build();
        assertEquals("{\"empty\":{}}", json);
    }

    @Test
    void testAddObjectNull() {
        // Null builder should be silently ignored (no-op).
        String json = new JsonBuilder()
            .add("a", "b")
            .addObject("nullObj", null)
            .build();
        assertEquals("{\"a\":\"b\"}", json);
    }

    @Test
    void testDeepNesting() {
        String json = new JsonBuilder()
            .add("level1", "outer")
            .addObject("inner", new JsonBuilder()
                .add("level2", "middle")
                .addObject("innermost", new JsonBuilder()
                    .add("level3", "inner")))
            .build();
        assertEquals(
            "{\"level1\":\"outer\",\"inner\":{\"level2\":\"middle\",\"innermost\":{\"level3\":\"inner\"}}}",
            json);
    }

    // ---- Arrays ----

    @Test
    void testAddArrayEmpty() {
        String json = new JsonBuilder()
            .addArray("items", new JsonBuilder[0])
            .build();
        assertEquals("{\"items\":[]}", json);
    }

    @Test
    void testAddArrayWithObjects() {
        String json = new JsonBuilder()
            .add("name", "alice")
            .addArray("tags",
                new JsonBuilder().add("id", "a"),
                new JsonBuilder().add("id", "b"))
            .build();
        assertEquals(
            "{\"name\":\"alice\",\"tags\":[{\"id\":\"a\"},{\"id\":\"b\"}]}",
            json);
    }

    @Test
    void testAddArraySingleElement() {
        String json = new JsonBuilder()
            .addArray("items", new JsonBuilder().add("x", 1))
            .build();
        assertEquals("{\"items\":[{\"x\":1}]}", json);
    }

    @Test
    void testAddArrayStringValues() {
        String json = new JsonBuilder()
            .add("topic", "colors")
            .addArray("values", "red", "green", "blue")
            .build();
        assertEquals(
            "{\"topic\":\"colors\",\"values\":[\"red\",\"green\",\"blue\"]}",
            json);
    }

    @Test
    void testAddArrayStringValuesEscaping() {
        String json = new JsonBuilder()
            .addArray("msgs", "hello \"world\"", "line1\nline2")
            .build();
        assertEquals(
            "{\"msgs\":[\"hello \\\"world\\\"\",\"line1\\nline2\"]}",
            json);
    }

    @Test
    void testAddArrayStringNull() {
        String json = new JsonBuilder()
            .addArray("vals", "a", null, "c")
            .build();
        assertEquals("{\"vals\":[\"a\",null,\"c\"]}", json);
    }

    @Test
    void testAddArrayIntValues() {
        String json = new JsonBuilder()
            .addArray("scores", 95, 87, 100)
            .build();
        assertEquals("{\"scores\":[95,87,100]}", json);
    }

    @Test
    void testAddArrayIntEmpty() {
        String json = new JsonBuilder()
            .addArray("scores", new int[0])
            .build();
        assertEquals("{\"scores\":[]}", json);
    }

    @Test
    void testAddArrayLongValues() {
        String json = new JsonBuilder()
            .addArray("bigs", 1000000000000L, 2000000000000L)
            .build();
        assertEquals("{\"bigs\":[1000000000000,2000000000000]}", json);
    }

    @Test
    void testAddArrayBooleanValues() {
        String json = new JsonBuilder()
            .add("flags", "true,false,true")
            .build();
        assertEquals("{\"flags\":\"true,false,true\"}", json);
    }

    @Test
    void testAddArrayDoubleValues() {
        String json = new JsonBuilder()
            .addArray("measurements", "1.5", "2.5", "3.0")
            .build();
        assertEquals("{\"measurements\":[\"1.5\",\"2.5\",\"3.0\"]}", json);
    }

    // ---- Complex composition ----

    @Test
    void testComplexNestedStructure() {
        // Build a structure similar to what the incident workflow would produce:
        // {
        //   "severity": "critical",
        //   "summary": "CPU at 95%",
        //   "sources": ["prometheus", "datadog"],
        //   "metadata": {
        //     "source": "monitoring",
        //     "priority": 1
        //   },
        //   "actions": [
        //     {"id": "scale-up", "params": {"count": 3}},
        //     {"id": "alert-team", "params": {"channel": "ops"}}
        //   ]
        // }
        String json = new JsonBuilder()
            .add("severity", "critical")
            .add("summary", "CPU at 95%")
            .addArray("sources", "prometheus", "datadog")
            .addObject("metadata", new JsonBuilder()
                .add("source", "monitoring")
                .add("priority", 1))
            .addArray("actions",
                new JsonBuilder()
                    .add("id", "scale-up")
                    .addObject("params", new JsonBuilder().add("count", 3)),
                new JsonBuilder()
                    .add("id", "alert-team")
                    .addObject("params", new JsonBuilder().add("channel", "ops")))
            .build();
        assertEquals(
            "{\"severity\":\"critical\",\"summary\":\"CPU at 95%\",\"sources\":[\"prometheus\",\"datadog\"],\"metadata\":{\"source\":\"monitoring\",\"priority\":1},\"actions\":[{\"id\":\"scale-up\",\"params\":{\"count\":3}},{\"id\":\"alert-team\",\"params\":{\"channel\":\"ops\"}}]}",
            json);
    }

    // ---- Key escaping ----

    @Test
    void testKeyEscaping() {
        String json = new JsonBuilder()
            .add("key\"with\"quotes", "value")
            .build();
        assertEquals("{\"key\\\"with\\\"quotes\":\"value\"}", json);
    }

    @Test
    void testKeyWithBackslash() {
        String json = new JsonBuilder()
            .add("path\\to\\key", "value")
            .build();
        assertEquals("{\"path\\\\to\\\\key\":\"value\"}", json);
    }

    // ---- toString ----

    @Test
    void testToString() {
        JsonBuilder jb = new JsonBuilder().add("a", 1);
        assertEquals(jb.build(), jb.toString());
    }

    // ---- Null key ----

    @Test
    void testNullKeyThrows() {
        assertThrows(IllegalArgumentException.class,
            () -> new JsonBuilder().add(null, "value"));
    }

    // ---- Builder reuse ----

    @Test
    void testBuilderReuseForAddObject() {
        // A builder used as a sub-object value should still be usable
        // (its state is unaffected by being consumed).
        JsonBuilder inner = new JsonBuilder().add("x", 10);
        String first = new JsonBuilder()
            .addObject("obj", inner)
            .build();
        assertEquals("{\"obj\":{\"x\":10}}", first);

        String second = new JsonBuilder()
            .addObject("again", inner)
            .build();
        assertEquals("{\"again\":{\"x\":10}}", second);
    }

    // ---- Ordering ----

    @Test
    void testFieldOrderPreserved() {
        String json = new JsonBuilder()
            .add("z", "last")
            .add("a", "first")
            .add("m", "middle")
            .build();
        assertEquals("{\"z\":\"last\",\"a\":\"first\",\"m\":\"middle\"}", json);
    }
}
