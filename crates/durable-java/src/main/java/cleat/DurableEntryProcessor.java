package cleat;

import javax.annotation.processing.AbstractProcessor;
import javax.annotation.processing.RoundEnvironment;
import javax.annotation.processing.SupportedAnnotationTypes;
import javax.annotation.processing.SupportedSourceVersion;
import javax.lang.model.SourceVersion;
import javax.lang.model.element.Element;
import javax.lang.model.element.ElementKind;
import javax.lang.model.element.ExecutableElement;
import javax.lang.model.element.TypeElement;
import javax.lang.model.element.VariableElement;
import javax.lang.model.type.TypeKind;
import javax.lang.model.type.TypeMirror;
import javax.tools.Diagnostic;
import javax.tools.JavaFileObject;
import java.io.IOException;
import java.io.PrintWriter;
import java.util.Set;

/**
 * Compile-time annotation processor for {@link DurableEntry @DurableEntry}.
 * <p>
 * For every method annotated with {@code @DurableEntry}, this processor
 * generates a companion class containing a WASM export wrapper that
 * conforms to the cleat ABI:
 * <pre>
 *   (argsPtr: i32, argsLen: i32, outPtr: i32, maxOutLen: i32) -> i64
 * </pre>
 * <p>
 * The generated class:
 * <ol>
 *   <li>Reads the input JSON from WASM linear memory</li>
 *   <li>Deserializes it into the method's parameter types</li>
 *   <li>Creates a {@link HostCalls} instance</li>
 *   <li>Invokes the user's workflow method</li>
 *   <li>Serializes the return value to JSON</li>
 *   <li>Writes the result to the output buffer</li>
 *   <li>Returns the packed {@code (errCode, actualLen)} result</li>
 * </ol>
 * <p>
 * If the workflow method throws an exception, the generated wrapper catches
 * it and returns an error JSON with {@code errCode = 1}.
 * <p>
 * <strong>Usage:</strong> The processor is automatically discovered via the
 * {@code META-INF/services/javax.annotation.processing.Processor} service
 * file.  No manual registration is needed.
 *
 * @see DurableEntry
 * @see HostCalls
 */
@SupportedAnnotationTypes("cleat.DurableEntry")
@SupportedSourceVersion(SourceVersion.RELEASE_11)
public class DurableEntryProcessor extends AbstractProcessor {

    @Override
    public boolean process(Set<? extends TypeElement> annotations, RoundEnvironment roundEnv) {
        if (annotations.isEmpty()) {
            return false;
        }

        for (Element element : roundEnv.getElementsAnnotatedWith(DurableEntry.class)) {
            if (element.getKind() != ElementKind.METHOD) {
                processingEnv.getMessager().printMessage(
                    Diagnostic.Kind.ERROR,
                    "@DurableEntry can only be applied to methods, not to "
                        + element.getKind().name().toLowerCase(),
                    element);
                continue;
            }

            ExecutableElement method = (ExecutableElement) element;
            TypeElement classElement = (TypeElement) method.getEnclosingElement();

            DurableEntry annotation = method.getAnnotation(DurableEntry.class);
            String exportName = annotation.name();
            if (exportName.isEmpty()) {
                exportName = method.getSimpleName().toString();
            }

            generateExportWrapper(classElement, method, exportName);
        }
        return true;
    }

    /**
     * Generate a Java source file that implements the WASM export wrapper
     * for the annotated workflow method.
     */
    private void generateExportWrapper(TypeElement classElement,
                                        ExecutableElement method,
                                        String exportName) {
        String packageName = processingEnv.getElementUtils()
            .getPackageOf(classElement).getQualifiedName().toString();
        String className = classElement.getSimpleName().toString();
        String methodName = method.getSimpleName().toString();
        String wrapperClassName = className + "_" + methodName + "_Export";

        // Analyze method parameters.  The first parameter must be HostCalls.
        var params = method.getParameters();
        int userParamCount = params.size() - 1; // Exclude HostCalls

        // Determine return type
        String returnType = method.getReturnType().toString();
        boolean returnsVoid = "void".equals(returnType);

        // Determine the input type from the actual parameter type rather than
        // hardcoding "String".  This supports non-String workflow parameters.
        String inputType = "String";
        String paramName = "input";
        if (userParamCount >= 1) {
            VariableElement userParam = params.get(1); // index 0 is HostCalls
            TypeMirror paramTypeMirror = userParam.asType();
            inputType = paramTypeMirror.toString();
            paramName = userParam.getSimpleName().toString();

            // Validate that the parameter type is supported for JSON deserialization.
            if (!isSupportedParameterType(paramTypeMirror)) {
                processingEnv.getMessager().printMessage(
                    Diagnostic.Kind.ERROR,
                    "Unsupported parameter type '" + inputType + "' for parameter '"
                        + paramName + "' in method '" + method.getSimpleName()
                        + "'.  Supported types are: String, int, Integer, long, Long, "
                        + "double, Double, boolean, Boolean, and custom reference types "
                        + "that are JSON-serializable.",
                    userParam);
            }
        }

        // If there are no user parameters, we don't need deserialization.
        boolean hasInput = userParamCount >= 1;
        if (userParamCount == 0) {
            paramName = null;
        }

        try {
            String qualifiedName = packageName.isEmpty()
                ? wrapperClassName
                : packageName + "." + wrapperClassName;

            JavaFileObject file = processingEnv.getFiler()
                .createSourceFile(qualifiedName);

            try (PrintWriter out = new PrintWriter(file.openWriter())) {
                writeGeneratedClass(
                    out, packageName, wrapperClassName,
                    className, methodName, exportName,
                    inputType, paramName, returnType, returnsVoid, hasInput);
            }
        } catch (IOException e) {
            processingEnv.getMessager().printMessage(
                Diagnostic.Kind.ERROR,
                "Failed to generate export wrapper for " + className + "."
                    + methodName + ": " + e.getMessage());
        }
    }

    /**
     * Write the generated export wrapper class source.
     */
    private void writeGeneratedClass(
            PrintWriter out,
            String packageName,
            String wrapperClassName,
            String className,
            String methodName,
            String exportName,
            String inputType,
            String paramName,
            String returnType,
            boolean returnsVoid,
            boolean hasInput) {

        // Package declaration
        if (!packageName.isEmpty()) {
            out.print("package ");
            out.print(packageName);
            out.println(";");
            out.println();
        }

        out.println("import org.teavm.interop.Export;");
        out.println("import cleat.HostCalls;");
        out.println("import cleat.Memory;");
        out.println("import cleat.JsonHelper;");
        out.println();

        // Class Javadoc
        out.println("/**");
        out.println(" * Generated WASM export wrapper for {@link " + className + "#" + methodName + "}.");
        out.println(" * <p>");
        out.println(" * Conforms to the cleat ABI export signature:");
        out.println(" * {@code (argsPtr: i32, argsLen: i32, outPtr: i32, maxOutLen: i32) -> i64}.");
        out.println(" * </p>");
        out.println(" * <p>Auto-generated by {@link cleat.DurableEntryProcessor}.</p>");
        out.println(" */");
        out.print("public class ");
        out.print(wrapperClassName);
        out.println(" {");
        out.println();

        // The @Export-annotated method
        out.print("    @Export(name = \"");
        out.print(escapeJavaString(exportName));
        out.println("\")");
        out.println("    public static long " + exportName
            + "(int argsPtr, int argsLen, int outPtr, int maxOutLen) {");
        out.println("        try {");

        if (hasInput) {
            out.println("            // Read input JSON from WASM linear memory.");
            out.println("            String inputJSON = Memory.readString(argsPtr, argsLen);");
            out.println();
            out.println("            // Deserialize the workflow input.");
            out.println("            " + inputType + " " + paramName + " = JsonHelper.parse(inputJSON, " + inputType + ".class);");
            out.println();
        }

        out.println("            // Create HostCalls instance for the workflow.");
        out.println("            HostCalls hostCalls = new HostCalls();");
        out.println();

        // Generate the method invocation
        out.print("            ");
        if (!returnsVoid) {
            out.print(returnType + " result = ");
        }
        out.print(className + "." + methodName + "(hostCalls");
        if (hasInput) {
            out.print(", " + paramName);
        }
        out.println(");");
        out.println();

        if (!returnsVoid) {
            out.println("            // Serialize the result to JSON.");
            out.println("            String resultJSON = JsonHelper.stringify(result);");
        } else {
            out.println("            // Return an empty success JSON for void methods.");
            out.println("            String resultJSON = \"{}\";");
        }

        out.println();
        out.println("            // Write result JSON to the output buffer.");
        out.println("            int written = Memory.writeString(outPtr, maxOutLen, resultJSON);");
        out.println("            return Memory.encodeExportResult(0, written);");
        out.println();

        // Catch block
        out.println("        } catch (Exception e) {");
        out.println("            // Catch all exceptions and return as error JSON.");
        out.println("            String errorJSON = JsonHelper.errorJson(");
        out.println("                e.getMessage() != null ? e.getMessage() : \"Unknown error\");");
        out.println("            int written = Memory.writeString(outPtr, maxOutLen, errorJSON);");
        out.println("            return Memory.encodeExportResult(1, written);");
        out.println("        }");
        out.println("    }");
        out.println();

        // Utility method for escapeJavaString isn't needed as a helper
        // in the generated class — we use JsonHelper.escapeJson instead.
        out.println("}");
    }

    /**
     * Returns true if the given type is supported as a workflow input parameter.
     * <p>
     * Supported types include {@link String}, the common primitives and their
     * boxed equivalents ({@code int}/{@link Integer}, {@code long}/{@link Long},
     * {@code double}/{@link Double}, {@code boolean}/{@link Boolean}), and any
     * custom reference type (which {@link JsonHelper} will attempt to deserialize).
     * </p>
     */
    private static boolean isSupportedParameterType(TypeMirror type) {
        TypeKind kind = type.getKind();
        if (kind == TypeKind.VOID || kind == TypeKind.ARRAY) {
            return false;
        }
        if (kind.isPrimitive()) {
            String name = type.toString();
            return "int".equals(name) || "long".equals(name)
                || "double".equals(name) || "boolean".equals(name);
        }
        // All other reference types (String, custom types, etc.) are allowed.
        return true;
    }

    /**
     * Escape a string for embedding in Java source code (literal strings).
     */
    private static String escapeJavaString(String s) {
        if (s == null || s.isEmpty()) {
            return "";
        }
        StringBuilder sb = new StringBuilder(s.length());
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            switch (c) {
                case '\\':
                    sb.append("\\\\");
                    break;
                case '"':
                    sb.append("\\\"");
                    break;
                case '\n':
                    sb.append("\\n");
                    break;
                case '\r':
                    sb.append("\\r");
                    break;
                case '\t':
                    sb.append("\\t");
                    break;
                default:
                    sb.append(c);
                    break;
            }
        }
        return sb.toString();
    }
}
