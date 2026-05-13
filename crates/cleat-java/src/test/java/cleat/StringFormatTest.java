package cleat;

import static org.junit.jupiter.api.Assertions.*;
import org.junit.jupiter.api.Test;

/**
 * Unit tests for {@link StringFormat}.
 */
class StringFormatTest {

    // ---- Basic %s ----

    @Test
    void testStringSubstitution() {
        assertEquals("Hello World",
            StringFormat.format("Hello %s", "World"));
    }

    @Test
    void testStringSubstitutionMultiple() {
        assertEquals("Hello Alice, welcome to cleat",
            StringFormat.format("Hello %s, welcome to %s", "Alice", "cleat"));
    }

    @Test
    void testStringSubstitutionNullArg() {
        assertEquals("null is the value",
            StringFormat.format("%s is the value", (Object) null));
    }

    @Test
    void testStringSubstitutionIntegerArg() {
        assertEquals("value: 42",
            StringFormat.format("value: %s", 42));
    }

    // ---- %d ----

    @Test
    void testIntegerSubstitution() {
        assertEquals("Count: 42",
            StringFormat.format("Count: %d", 42));
    }

    @Test
    void testIntegerSubstitutionNegative() {
        assertEquals("Delta: -10",
            StringFormat.format("Delta: %d", -10));
    }

    @Test
    void testIntegerSubstitutionZero() {
        assertEquals("Count: 0",
            StringFormat.format("Count: %d", 0));
    }

    @Test
    void testIntegerSubstitutionLong() {
        assertEquals("Big: 9000000000000",
            StringFormat.format("Big: %d", 9000000000000L));
    }

    @Test
    void testIntegerSubstitutionByte() {
        assertEquals("Byte: 127",
            StringFormat.format("Byte: %d", (byte) 127));
    }

    @Test
    void testIntegerSubstitutionShort() {
        assertEquals("Short: 32000",
            StringFormat.format("Short: %d", (short) 32000));
    }

    @Test
    void testIntegerSubstitutionStringFallback() {
        // Non-numeric argument passed to %d should fall back to String.valueOf
        assertEquals("Value: hello",
            StringFormat.format("Value: %d", "hello"));
    }

    // ---- %% ----

    @Test
    void testLiteralPercent() {
        assertEquals("100%",
            StringFormat.format("100%%"));
    }

    @Test
    void testPercentMidString() {
        assertEquals("Discount: 20% off",
            StringFormat.format("Discount: 20%% off"));
    }

    // ---- Mixed specifiers ----

    @Test
    void testMixedSpecifiers() {
        assertEquals("Hello Alice, you have 3 messages",
            StringFormat.format("Hello %s, you have %d messages", "Alice", 3));
    }

    @Test
    void testAllThreeSpecifiers() {
        assertEquals("Progress: 90% complete for task clean",
            StringFormat.format("Progress: 90%% complete for task %s", "clean"));
    }

    // ---- No specifiers ----

    @Test
    void testNoSpecifiers() {
        assertEquals("plain text",
            StringFormat.format("plain text"));
    }

    @Test
    void testEmptyPattern() {
        assertEquals("",
            StringFormat.format(""));
    }

    // ---- Null pattern ----

    @Test
    void testNullPattern() {
        assertNull(StringFormat.format(null));
    }

    // ---- Insufficient arguments ----

    @Test
    void testInsufficientArgsString() {
        assertEquals("Hello %s",
            StringFormat.format("Hello %s"));
    }

    @Test
    void testInsufficientArgsInt() {
        assertEquals("Count: %d",
            StringFormat.format("Count: %d"));
    }

    @Test
    void testInsufficientArgsMixed() {
        // The first %s consumes "Alice"; %d has no argument so it is
        // left as literal text.
        assertEquals("Hello Alice, you have %d messages",
            StringFormat.format("Hello %s, you have %d messages", "Alice"));
    }

    // ---- Extra arguments (ignored) ----

    @Test
    void testExtraArgsIgnored() {
        assertEquals("Hello World",
            StringFormat.format("Hello %s", "World", "extra", 42));
    }

    // ---- Unsupported specifier ----

    @Test
    void testUnsupportedSpecifierKeptAsIs() {
        assertEquals("Value: %f",
            StringFormat.format("Value: %f", 3.14));
    }

    @Test
    void testTrailingPercent() {
        assertEquals("Value: 50%",
            StringFormat.format("Value: 50%"));
    }

    // ---- Edge cases ----

    @Test
    void testOnlyPercent() {
        assertEquals("%",
            StringFormat.format("%%"));
    }

    @Test
    void testConsecutiveSpecifiers() {
        assertEquals("ab",
            StringFormat.format("%s%s", "a", "b"));
    }

    @Test
    void testConsecutivePercent() {
        assertEquals("50% discount",
            StringFormat.format("50%% discount"));
    }

    @Test
    void testPercentAtStart() {
        assertEquals("% complete",
            StringFormat.format("%% complete"));
    }

    @Test
    void testPercentAtEnd() {
        assertEquals("complete%",
            StringFormat.format("complete%%"));
    }

    @Test
    void testStringValueOfObject() {
        Object obj = new Object() {
            @Override
            public String toString() {
                return "custom";
            }
        };
        assertEquals("custom",
            StringFormat.format("%s", obj));
    }
}
