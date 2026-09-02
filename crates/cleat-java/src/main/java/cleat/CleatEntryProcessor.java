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
import java.util.LinkedHashSet;
import java.util.Set;
import java.util.HashSet;
import java.util.HashMap;
import java.util.Map;

/**
 * Compile-time annotation processor for {@link CleatEntry @CleatEntry}.
 * <p>
 * For every method annotated with {@code @CleatEntry}, this processor
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
 * @see CleatEntry
 * @see HostCalls
 */
@SupportedAnnotationTypes("*")
@SupportedSourceVersion(SourceVersion.RELEASE_11)
public class CleatEntryProcessor extends AbstractProcessor {

    private final Set<String> generatedWrappers = new HashSet<>();

    /** Maps wrapper FQCN -> export name for aggregator generation. */
    private final Map<String, String> wrapperExportNames = new HashMap<>();

    /** FQCNs of all classes that contain at least one @CleatEntry method. */
    private final Set<String> workflowClasses = new LinkedHashSet<>();

    /** Whether the aggregator and WorkflowEntry have been generated. */
    private boolean aggregatorGenerated = false;

    @Override
    public boolean process(Set<? extends TypeElement> annotations, RoundEnvironment roundEnv) {
        if (!annotations.isEmpty()) {
            for (Element element : roundEnv.getElementsAnnotatedWith(CleatEntry.class)) {
                if (element.getKind() != ElementKind.METHOD) {
                    processingEnv.getMessager().printMessage(
                        Diagnostic.Kind.ERROR,
                        "@CleatEntry can only be applied to methods, not to "
                            + element.getKind().name().toLowerCase(),
                        element);
                    continue;
                }

                // B1: Validate the method is static.
                if (!element.getModifiers().contains(javax.lang.model.element.Modifier.STATIC)) {
                    processingEnv.getMessager().printMessage(
                        Diagnostic.Kind.ERROR,
                        "@CleatEntry method must be static: WASM exports are top-level functions without a 'this' reference. Add the 'static' modifier.",
                        element);
                    continue;
                }

                // B1: Validate the method is public.
                if (!element.getModifiers().contains(javax.lang.model.element.Modifier.PUBLIC)) {
                    processingEnv.getMessager().printMessage(
                        Diagnostic.Kind.ERROR,
                        "@CleatEntry method must be public: The generated export wrapper class must be able to invoke this method. Add the 'public' modifier.",
                        element);
                    continue;
                }

                ExecutableElement method = (ExecutableElement) element;
                TypeElement classElement = (TypeElement) method.getEnclosingElement();

                CleatEntry annotation = method.getAnnotation(CleatEntry.class);
                String exportName = annotation.name();
                if (exportName.isEmpty()) {
                    exportName = method.getSimpleName().toString();
                }

                generateExportWrapper(classElement, method, exportName);
                // Record the enclosing workflow class FQCN for direct
                // .class references in the generated WorkflowEntry.
                String wfPkg = processingEnv.getElementUtils()
                    .getPackageOf(classElement).getQualifiedName().toString();
                String clsName = classElement.getSimpleName().toString();
                String classFqcn = wfPkg.isEmpty() ? clsName : wfPkg + "." + clsName;
                workflowClasses.add(classFqcn);
            }
        }

        // Always generate the aggregator and WorkflowEntry at the end of
        // annotation processing, even when there are no @CleatEntry methods.
        // This ensures the TeaVM analysis root class always exists.
        if (roundEnv.processingOver() && !aggregatorGenerated) {
            generateAggregator();
            generateWorkflowEntry();
            aggregatorGenerated = true;
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

        // B1: Validate first parameter type is cleat.HostCalls.
        if (params.isEmpty()
            || !params.get(0).asType().toString().equals("cleat.HostCalls")) {
            processingEnv.getMessager().printMessage(
                Diagnostic.Kind.ERROR,
                "@CleatEntry method first parameter must be cleat.HostCalls, got "
                    + (params.isEmpty() ? "none" : params.get(0).asType().toString())
                    + ". The first parameter provides the HostCalls runtime API. Change it to 'cleat.HostCalls h'.",
                method);
            return;
        }

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
            generatedWrappers.add(qualifiedName);
            wrapperExportNames.put(qualifiedName, exportName);

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
                    + methodName + ": " + e.getMessage(),
                method);
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
        out.println(" * <p>Auto-generated by {@link cleat.CleatEntryProcessor}.</p>");
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

        out.println();
        out.println("            // Run the workflow's own defers before reporting, so");
        out.println("            // anything they record lands inside this segment. A defer");
        out.println("            // that itself suspends throws SuspendSignal, which the");
        out.println("            // handler below catches -- suspension wins over the result,");
        out.println("            // exactly as when the workflow body suspends. 3.73.");
        out.println("            cleat.Defer.runDeferred();");
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

        // Catch block — TerminalError first (non-retryable), then general Exception.
        // Suspension first: it is not a failure and must not be caught by the
        // Exception handler below, which would turn a suspended workflow into
        // one that "failed" with the message "cleat: workflow suspended".
        //
        // IMPROVEMENT-PLAN 3.74. This branch did not exist, so no generated
        // wrapper could ever return the sentinel the host checks for
        // (engine/backend_wasmtime.go: `if raw == (1 << 62)`). The host half
        // was ready; the guest half was missing.
        out.println("        } catch (cleat.SuspendSignal e) {");
        out.println("            return cleat.Memory.SUSPEND_SENTINEL;");
        out.println("        } catch (cleat.TerminalError e) {");
        out.println("            // Cleanup exists for the run that did not finish the way it");
        out.println("            // meant to, so defers run on the error paths too. Drained for");
        out.println("            // the host rather than plainly, because a defer that suspends");
        out.println("            // here has no segment left to suspend: the result is already");
        out.println("            // decided.");
        out.println("            cleat.Defer.runDeferredForHost();");
        out.println("            String errorJSON = JsonHelper.errorJson(");
        out.println("                e.getMessage() != null ? e.getMessage() : \"Terminal error\");");
        out.println("            int written = Memory.writeString(outPtr, maxOutLen, errorJSON);");
        out.println("            return Memory.encodeExportResult(cleat.Memory.TERMINAL_ERROR_CODE, written);");
        out.println("        } catch (Exception e) {");
        out.println("            cleat.Defer.runDeferredForHost();");
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

    /**
     * Generate the CleatEntryIndex source file that references all
     * generated export wrapper classes.
     * <p>
     * This class is generated in the {@code cleat.generated} package.
     * Its {@code WRAPPER_CLASSES} field references all generated wrapper
     * classes via {@code .class} literals, which prevents TeaVM from
     * tree-shaking them during WASM compilation.
     */
    private void generateAggregator() {
        try {
            JavaFileObject file = processingEnv.getFiler()
                .createSourceFile("cleat.generated.CleatEntryIndex");

            try (PrintWriter out = new PrintWriter(file.openWriter())) {
                out.println("package cleat.generated;");
                out.println();
                out.println("/**");
                out.println(" * Auto-generated by CleatEntryProcessor. References all");
                out.println(" * generated WASM export wrapper classes via Class<?>[] to");
                out.println(" * prevent TeaVM tree-shaking. Also exposes entry point");
                out.println(" * metadata via getEntries().");
                out.println(" */");
                out.println("public class CleatEntryIndex {");
                out.println();
                out.println("    /**");
                out.println("     * Class references to all generated export wrappers.");
                out.println("     * Kept as a static field accessed by WorkflowEntry to");
                out.println("     * prevent TeaVM tree-shaking.");
                out.println("     */");
                out.println("    public static final Class<?>[] WRAPPER_CLASSES = new Class<?>[] {");
                for (String fqcn : generatedWrappers) {
                    out.print("        ");
                    out.print(fqcn);
                    out.println(".class,");
                }
                out.println("    };");
                out.println();
                out.println("    /**");
                out.println("     * Return the list of exported @CleatEntry entry point");
                out.println("     * names. Useful for runtime introspection and testing.");
                out.println("     */");
                out.println("    public static String[] getEntries() {");
                if (generatedWrappers.isEmpty()) {
                    out.println("        return new String[0];");
                } else {
                    out.println("        return new String[] {");
                    for (String fqcn : generatedWrappers) {
                        String exportName = wrapperExportNames.get(fqcn);
                        out.print("            \"");
                        out.print(escapeJavaString(exportName != null ? exportName : ""));
                        out.println("\",");
                    }
                    out.println("        };");
                }
                out.println("    }");
                out.println("}");
            }
        } catch (IOException e) {
            processingEnv.getMessager().printMessage(
                Diagnostic.Kind.ERROR,
                "Failed to generate CleatEntryIndex: " + e.getMessage(),
            (Element) null);
        }
    }

    /**
     * Generate the WorkflowEntry class that the TeaVM compiler uses as its
     * static analysis root.
     * <p>
     * This class provides both indirect references (via CleatEntryIndex)
     * and direct {@code .class} references to every workflow class that
     * contains {@code @CleatEntry} methods.  The chain of references
     * prevents TeaVM from removing the exports and workflow logic during
     * dead-code elimination.
     * <p>
     * The generated class is placed in the {@code cleat} package and set as
     * {@code mainClass} in the TeaVM Gradle configuration.
     */
    private void generateWorkflowEntry() {
        try {
            JavaFileObject file = processingEnv.getFiler()
                .createSourceFile("cleat.WorkflowEntry");

            try (PrintWriter out = new PrintWriter(file.openWriter())) {
                out.println("package cleat;");
                out.println();
                out.println("/**");
                out.println(" * Auto-generated analysis root for TeaVM WASM compilation.");
                out.println(" * References all @CleatEntry workflow classes and their");
                out.println(" * generated export wrappers to prevent tree-shaking.");
                out.println(" */");
                out.println("public class WorkflowEntry {");
                out.println();

                // Indirect reference via CleatEntryIndex (preserves export wrappers).
                out.println("    /**");
                out.println("     * Static reference to CleatEntryIndex.WRAPPER_CLASSES");
                out.println("     * prevents TeaVM from tree-shaking the export wrappers.");
                out.println("     */");
                out.println("    @SuppressWarnings(\"unused\")");
                out.println("    private static final Class<?>[] AGGREGATOR_REF =");
                out.println("        cleat.generated.CleatEntryIndex.WRAPPER_CLASSES;");
                out.println();

                // Direct .class references to each workflow class (belt-and-suspenders).
                out.println("    /**");
                out.println("     * Direct references to all classes that contain");
                out.println("     * @CleatEntry methods. Prevents TeaVM from");
                out.println("     * tree-shaking the user's workflow logic.");
                out.println("     */");
                out.println("    @SuppressWarnings(\"unused\")");
                out.println("    private static final Class<?>[] WORKFLOW_CLASSES = new Class<?>[] {");
                for (String fqcn : workflowClasses) {
                    out.print("        ");
                    out.print(fqcn);
                    out.println(".class,");
                }
                out.println("    };");
                out.println();

                // main method for TeaVM reachability analysis root.
                out.println("    /**");
                out.println("     * Main entry point for TeaVM reachability analysis.");
                out.println("     * References all workflow classes to ensure they");
                out.println("     * survive tree-shaking during WASM compilation.");
                out.println("     *");
                out.println("     * @param args unused");
                out.println("     */");
                out.println("    public static void main(String[] args) {");
                out.println("        java.util.Arrays.stream(WORKFLOW_CLASSES).forEach(c -> {");
                out.println("            System.out.println(\"Cleat workflow: \" + c.getName());");
                out.println("        });");
                out.println("    }");
                out.println();

                out.println("    private WorkflowEntry() {}");
                out.println("}");
            }
        } catch (IOException e) {
            processingEnv.getMessager().printMessage(
                Diagnostic.Kind.ERROR,
                "Failed to generate WorkflowEntry: " + e.getMessage(),
            (Element) null);
        }
    }
}
