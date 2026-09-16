package cleat;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.junit.jupiter.api.Assertions.fail;

import java.lang.reflect.Method;

import org.junit.jupiter.api.Test;

/**
 * What this SDK does with the whole payload, pinned rather than described.
 *
 * <p>cleat#1691. {@code tests/conformance/entry_point_binding_cases.json}
 * deliberately carries no {@code java} column, and its {@code how} says why:
 * only Go, Python and AssemblyScript bind parameters BY NAME. {@code @CleatEntry}
 * takes exactly one user parameter and hands it the whole payload through
 * {@link JsonHelper#parse}, so "an absent parameter" is not a concept this SDK
 * has. That exclusion is correct and this file does not challenge it.
 *
 * <p>WHY THIS FILE EXISTS ANYWAY. The facts the exclusion rests on are recorded
 * as PROSE, in the table's {@code no_per_parameter_binding}, and that note went
 * STALE without anything noticing. It still reads:
 *
 * <pre>
 *   java: one user parameter through JsonHelper.parse; a second generates an
 *         uncallable wrapper and a typed one throws at run time (cleat#1636)
 * </pre>
 *
 * <p>Both halves describe the behaviour cleat#1636 REMOVED. Measured
 * 2026-09-16 against the real annotation processor, a second parameter and a
 * custom type are now refused at COMPILE time with named diagnostics. The note
 * cites the fix as the cause of the bug.
 *
 * <p>A note nothing asserts is a confident, well-formed, wrong answer waiting
 * for the next reader. These tests assert it, so the next time the behaviour
 * moves the note goes red instead of quietly becoming false.
 */
class WholePayloadBindingConformanceTest {

    /**
     * THE ROW THIS SDK SHARES WITH GO AND ASSEMBLYSCRIPT, and the one the table
     * cannot show because Java has no column.
     *
     * <p>The table's "lone string parameter" case is not a name-binding
     * question: it asks what a single string parameter receives. Go and
     * AssemblyScript hand it the whole payload, so {@code {}} binds the
     * two-character string {@code "{}"}. Python refuses, and so does Rust --
     * serde is asked for a String and given an object. Java agrees with Go and
     * AssemblyScript, which makes that row a 3-2 split rather than the 2-2 the
     * table can currently display.
     */
    @Test
    void aLoneStringParameterReceivesTheWholePayload() {
        assertEquals("{}", JsonHelper.parse("{}", String.class),
            "a String parameter no longer receives the raw payload. If this is "
            + "deliberate, Java has left the Go/AssemblyScript side of the "
            + "lone-string row and no_per_parameter_binding must say so.");

        assertEquals("{\"note\":\"hi\"}", JsonHelper.parse("{\"note\":\"hi\"}", String.class),
            "the payload is handed over verbatim, not unwrapped by key -- which "
            + "is exactly why 'an absent parameter' is not a concept here");

        // The control: a genuine JSON string literal is still decoded as one,
        // rather than handed back with its quotes. Without this, "returns the
        // input unchanged" would satisfy the assertions above.
        assertEquals("hi", JsonHelper.parse("\"hi\"", String.class),
            "a quoted JSON string must decode to its value");
    }

    /**
     * A non-string scalar does NOT get the whole-payload treatment: it is
     * decoded, and an object payload is refused.
     *
     * <p>This is the asymmetry that makes the String case above a special path
     * rather than a general rule, and it is worth pinning separately -- a
     * reader who generalises "one parameter, whole payload" to every type would
     * expect these to bind something.
     */
    @Test
    void aNonStringScalarRefusesAnObjectPayload() {
        assertThrows(RuntimeException.class, () -> JsonHelper.parse("{}", Integer.class),
            "an Integer parameter accepted an object payload");
        assertThrows(RuntimeException.class, () -> JsonHelper.parse("{}", Boolean.class),
            "a Boolean parameter accepted an object payload");

        assertEquals(Integer.valueOf(42), JsonHelper.parse("42", Integer.class),
            "the control: a genuine number still decodes");
    }

    /**
     * A custom type is refused, which is why this SDK has no field-level
     * analogue of the table's scalar cases.
     *
     * <p>Go, Python and AssemblyScript can declare {@code note} and leave it out
     * of the payload. Java cannot: there is no struct to hold a named field,
     * because the only accepted types are the ones {@link JsonHelper#parse}
     * decodes. So the scalar rows of the table are not merely unmeasured here --
     * they are inexpressible, which is a stronger and better reason to leave the
     * column out.
     *
     * <p>Asserted through the processor's own predicate rather than by running
     * javac, the same seam {@code EntryParameterTypesMatchTheDecoderTest} uses.
     */
    @Test
    void aCustomTypeIsRefusedSoThereIsNoNamedFieldToBeAbsent() {
        Method isSupported;
        try {
            isSupported = CleatEntryProcessor.class
                .getDeclaredMethod("isSupportedParameterTypeByName", String.class, boolean.class);
            isSupported.setAccessible(true);
        } catch (NoSuchMethodException e) {
            fail("CleatEntryProcessor.isSupportedParameterTypeByName(String,boolean) is gone; "
                + "this test and EntryParameterTypesMatchTheDecoderTest both drive it. "
                + "If the seam moved, point both at the new one rather than deleting them.");
            return;
        }

        try {
            assertTrue((Boolean) isSupported.invoke(null, "java.lang.String", false),
                "String must remain an accepted parameter type");
            assertTrue(!(Boolean) isSupported.invoke(null, "com.example.Order", false),
                "a custom type is accepted as an entry parameter. JsonHelper.parse cannot "
                + "decode one, so this is cleat#1636 returning: it compiles, passes the "
                + "supported-type check, and throws on the workflow's first start.");
        } catch (ReflectiveOperationException e) {
            fail("invoking the processor's supported-type predicate failed: " + e);
        }
    }
}
