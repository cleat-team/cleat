package cleat;

import java.lang.annotation.ElementType;
import java.lang.annotation.Retention;
import java.lang.annotation.RetentionPolicy;
import java.lang.annotation.Target;

/**
 * Marks a method as a cleat workflow entry point.
 * <p>
 * Methods annotated with {@code @CleatEntry} must be {@code public static}
 * and take a {@link HostCalls} instance as their first parameter.
 * Additional parameters are deserialized from the workflow input JSON.
 * <p>
 * The annotation processor ({@link CleatEntryProcessor}) generates a
 * WASM export wrapper at compile time for each annotated method.  The
 * generated class implements the cleat ABI export signature:
 * <pre>
 *   (argsPtr: i32, argsLen: i32, outPtr: i32, maxOutLen: i32) -> i64
 * </pre>
 * <p>
 * <strong>Example usage:</strong>
 * <pre>{@code
 * public class MyWorkflow {
 *     @CleatEntry(name = "place_order")
 *     public static String placeOrder(HostCalls h, String input) {
 *         h.cleatLog("Processing order");
 *         String result = h.cleatCall("orders", "create", input).getValue();
 *         return result;
 *     }
 * }
 * }</pre>
 *
 * @see HostCalls
 * @see CleatEntryProcessor
 */
@Retention(RetentionPolicy.SOURCE)
@Target(ElementType.METHOD)
public @interface CleatEntry {

    /**
     * The WASM export name for this workflow entry point.
     * Defaults to the Java method name if left empty.
     */
    String name() default "";
}
