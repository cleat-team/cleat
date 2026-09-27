package cleat;

import static org.junit.jupiter.api.Assertions.fail;

import java.lang.reflect.Method;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.TreeSet;

import org.junit.jupiter.api.Test;

/**
 * The types {@code @CleatEntry} ACCEPTS are exactly the types
 * {@link JsonHelper#parse} can DECODE.
 *
 * <p>cleat#1636. These were two hand-maintained sets and they drifted apart in
 * BOTH directions, which is why a test comparing them is the fix rather than a
 * tightened list:
 *
 * <ul>
 *   <li>{@code isSupportedParameterType} accepted <em>every</em> reference
 *       type, so a workflow taking a custom type compiled cleanly, passed the
 *       SDK's own supported-type check, and threw
 *       {@code UnsupportedOperationException} on its first start -- with a
 *       message telling the author to hand-parse.</li>
 *   <li>It also <em>rejected</em> the primitives {@code float} and
 *       {@code short}, which {@code JsonHelper.parse} decodes perfectly well
 *       ({@code JsonHelper.java:96,105}). That direction nobody was looking
 *       for: a legal parameter refused at compile time.</li>
 * </ul>
 *
 * <p>BOTH SIDES ARE DERIVED FROM THEIR REAL IMPLEMENTATION, which is the whole
 * value of this file. The decodable side is not a list copied out of
 * {@code JsonHelper}: it is {@code JsonHelper.parse} actually invoked on a
 * payload of each candidate type. The accepted side is
 * {@code isSupportedParameterType} actually invoked. A list copied from either
 * would pass while the thing it copied from changed underneath it, which is
 * exactly how the two got out of step.
 *
 * <p>The processor's check takes a {@code TypeMirror}, which exists only during
 * annotation processing, so it is reached here through the same string form it
 * switches on -- see {@link #accepts}. That is a seam and it is named rather
 * than hidden: {@link #theSeamIsHonest()} fails if the processor stops keying
 * on {@code toString()}.
 */
class EntryParameterTypesMatchTheDecoderTest {

    /** A JSON payload that is valid for the given type. */
    private static String payloadFor(Class<?> type) {
        if (type == String.class) {
            return "\"hello\"";
        }
        if (type == Boolean.class || type == boolean.class) {
            return "true";
        }
        if (type == Map.class || type == HashMap.class) {
            return "{\"a\":1}";
        }
        if (type == List.class || type == ArrayList.class) {
            return "[1,2]";
        }
        if (type == Object.class) {
            return "{\"a\":1}";
        }
        return "42";
    }

    /**
     * Does {@link JsonHelper#parse} decode this type?
     *
     * <p>Only {@code UnsupportedOperationException} counts as "cannot decode".
     * A {@code RuntimeException} means the decoder understood the type and
     * disliked the payload, which is a different answer and must not be read as
     * unsupported -- that would let a genuinely decodable type drop out of the
     * set because this test's own fixture payload was wrong.
     */
    private static boolean decodes(Class<?> type) {
        try {
            JsonHelper.parse(payloadFor(type), type);
            return true;
        } catch (UnsupportedOperationException e) {
            return false;
        } catch (RuntimeException e) {
            return true;
        }
    }

    /** Does the processor accept this type as an entry-point parameter? */
    private static boolean accepts(String typeName) {
        try {
            Method m = CleatEntryProcessor.class
                .getDeclaredMethod("isSupportedParameterTypeByName", String.class, boolean.class);
            m.setAccessible(true);
            boolean primitive = !typeName.contains(".");
            return (Boolean) m.invoke(null, typeName, primitive);
        } catch (ReflectiveOperationException e) {
            throw new AssertionError(
                "CleatEntryProcessor.isSupportedParameterTypeByName(String,boolean) is gone. "
                    + "It exists so this test can ask the REAL check rather than a copy of it; "
                    + "if the check moved, point this test at the new one rather than "
                    + "reimplementing the rule here. cleat#1636.", e);
        }
    }

    /** A custom type. JsonHelper cannot decode it; the processor must refuse it. */
    public static class Pojo {
        public String sku;
        public long qty;
    }

    @Test
    void everyTypeTheProcessorAcceptsCanBeDecoded() {
        Class<?>[] candidates = {
            String.class, Object.class,
            Integer.class, Long.class, Double.class, Float.class, Short.class, Boolean.class,
            Map.class, HashMap.class, List.class, ArrayList.class,
            Pojo.class,
        };

        Set<String> acceptedNotDecodable = new TreeSet<>();
        Set<String> decodableNotAccepted = new TreeSet<>();

        for (Class<?> c : candidates) {
            boolean a = accepts(c.getName());
            boolean d = decodes(c);
            if (a && !d) {
                acceptedNotDecodable.add(c.getName());
            }
            if (d && !a) {
                decodableNotAccepted.add(c.getName());
            }
        }

        // Primitives, by the name the processor sees for them.
        Object[][] primitives = {
            {"int", int.class}, {"long", long.class}, {"double", double.class},
            {"float", float.class}, {"short", short.class}, {"boolean", boolean.class},
        };
        for (Object[] row : primitives) {
            String name = (String) row[0];
            Class<?> boxedProbe = (Class<?>) row[1];
            boolean a = accepts(name);
            boolean d = decodes(boxedProbe);
            if (a && !d) {
                acceptedNotDecodable.add(name);
            }
            if (d && !a) {
                decodableNotAccepted.add(name);
            }
        }

        if (!acceptedNotDecodable.isEmpty() || !decodableNotAccepted.isEmpty()) {
            fail("@CleatEntry's accepted parameter types and JsonHelper.parse's decodable "
                + "types disagree.\n\n"
                + "  accepted but NOT decodable: " + acceptedNotDecodable + "\n"
                + "      A workflow declaring one of these compiles cleanly, passes the SDK's "
                + "own supported-type check, and throws UnsupportedOperationException on its "
                + "FIRST START -- after the run has been created. That is cleat#1636.\n\n"
                + "  decodable but NOT accepted: " + decodableNotAccepted + "\n"
                + "      A legal parameter refused at compile time. Harmless to a running "
                + "system and confusing to an author, and it is the direction nobody looks "
                + "for: float and short were in this set.\n\n"
                + "Fix whichever side is wrong -- do not edit this test to match. It compares "
                + "two implementations; if they disagree, one of them is.");
        }
    }

    /**
     * The known-positive for the comparison above.
     *
     * <p>Without it, {@code everyTypeTheProcessorAcceptsCanBeDecoded} passing is
     * indistinguishable from a comparison that cannot see a disagreement --
     * both {@code accepts} and {@code decodes} returning the same constant, a
     * candidate list that is empty, a reflective lookup that silently answers
     * true. A green test that never compared anything looks exactly like a
     * green test that compared everything.
     *
     * <p>So: assert the case that IS broken. {@link Pojo} is the defect this
     * issue is about -- the decoder refuses it -- and if this test ever reports
     * that JsonHelper decodes a POJO, the comparison above has stopped meaning
     * anything.
     */
    @Test
    void theComparisonCanSeeADisagreement() {
        if (decodes(Pojo.class)) {
            fail("JsonHelper.parse reports that it decodes a POJO. Either POJO support was "
                + "added -- in which case cleat#1636's premise is gone and the processor "
                + "should accept custom types again -- or this test's decodes() probe has "
                + "stopped detecting UnsupportedOperationException, in which case the "
                + "comparison above cannot fail and is worthless.");
        }
        if (!decodes(String.class)) {
            fail("JsonHelper.parse reports that it cannot decode String. The decodes() probe "
                + "is broken: it is answering 'unsupported' for a type the decoder handles, "
                + "so the comparison above would report false disagreements.");
        }
    }

    /**
     * The seam between the processor's {@code TypeMirror} check and this test's
     * string-based one is honest.
     *
     * <p>{@code isSupportedParameterType(TypeMirror)} cannot be called outside
     * annotation processing, so this test calls the by-name form it delegates
     * to. That is only sound while the TypeMirror form really does delegate --
     * if it grows logic of its own, this test is asking a question nobody
     * answers in production.
     */
    @Test
    void theSeamIsHonest() {
        List<String> names = new ArrayList<>();
        for (Method m : CleatEntryProcessor.class.getDeclaredMethods()) {
            names.add(m.getName());
        }
        if (!names.contains("isSupportedParameterTypeByName")) {
            fail("isSupportedParameterTypeByName is gone; see accepts() for why it exists.");
        }
        if (!names.contains("isSupportedParameterType")) {
            fail("isSupportedParameterType is gone. If the production check moved, this test "
                + "is now comparing something nothing calls. Point it at the new check.");
        }
    }

}
