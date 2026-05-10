package cleat;

import static org.junit.jupiter.api.Assertions.*;

import org.junit.jupiter.api.BeforeAll;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import javax.tools.*;
import javax.tools.JavaCompiler.CompilationTask;
import java.io.*;
import java.net.URI;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.*;

/**
 * Tests for {@link CleatEntryProcessor} — the compile-time annotation processor
 * that generates WASM export wrappers from {@link CleatEntry @CleatEntry} methods.
 *
 * <p>Uses {@link javax.tools.JavaCompiler} to compile in-memory test sources
 * with the processor and verify the generated files.
 */
class CleatEntryProcessorTest {

    /** Temp directory for generated source output. */
    @TempDir
    static Path tempDir;

    private static JavaCompiler compiler;
    private static String validTestSource;

    @BeforeAll
    static void setUp() {
        compiler = ToolProvider.getSystemJavaCompiler();
        assertNotNull(compiler,
            "Expected system JavaCompiler to be available. "
            + "Running on a JDK, not a JRE, is required for annotation processor tests.");

        validTestSource = ""
            + "package test;\n"
            + "import cleat.HostCalls;\n"
            + "import cleat.CleatEntry;\n"
            + "import cleat.CleatResult;\n"
            + "public class TestWorkflow {\n"
            + "    @CleatEntry(name = \"test_entry\")\n"
            + "    public static String testEntry(HostCalls h, String input) {\n"
            + "        h.cleatLog(\"test\");\n"
            + "        return \"{\\\"ok\\\":true}\";\n"
            + "    }\n"
            + "}\n";
    }

    // ========================================================================
    // Helper: build classpath for in-process javac
    // ========================================================================

    /**
     * Build the classpath for the in-process Java compiler. Combines:
     * <ol>
     *   <li>{@code java.class.path} system property (set by Gradle during tests)</li>
     *   <li>The location of the {@link CleatEntryProcessor} class itself (the main
     *       classes output directory)</li>
     * </ol>
     */
    private static String buildClasspath() {
        Set<String> entries = new LinkedHashSet<>();

        // 1. System classpath
        String sysCp = System.getProperty("java.class.path");
        if (sysCp != null && !sysCp.isEmpty()) {
            for (String entry : sysCp.split(File.pathSeparator)) {
                if (!entry.isEmpty()) {
                    entries.add(entry);
                }
            }
        }

        // 2. Main classes directory (location of CleatEntryProcessor.class)
        try {
            java.net.URL location = CleatEntryProcessor.class
                .getProtectionDomain()
                .getCodeSource()
                .getLocation();
            if (location != null) {
                String path = location.getPath();
                // Decode URL-encoded path (e.g. spaces -> %20)
                try {
                    path = java.net.URLDecoder.decode(path, "UTF-8");
                } catch (Exception ignored) {
                }
                if (path != null && !path.isEmpty()) {
                    entries.add(path);
                }
            }
        } catch (Exception e) {
            // Fall through — system classpath may already include this
        }

        // 3. Add the temp source directory as well (for generated sources)
        if (tempDir != null) {
            entries.add(tempDir.resolve("sources").toString());
        }

        if (entries.isEmpty()) {
            return ".";
        }
        return String.join(File.pathSeparator, entries);
    }

    // ========================================================================
    // Helper: create an in-memory source file object
    // ========================================================================

    private static JavaFileObject source(String packageName, String className, String sourceCode) {
        String qualified = (packageName.isEmpty() ? "" : packageName.replace('.', '/') + "/") + className;
        return new SimpleJavaFileObject(
            URI.create("string:///" + qualified + ".java"),
            JavaFileObject.Kind.SOURCE) {
            @Override
            public CharSequence getCharContent(boolean ignoreEncodingErrors) {
                return sourceCode;
            }
        };
    }

    // ========================================================================
    // Helper: compile with the annotation processor
    // ========================================================================

    private static CompilationResult compile(
            JavaFileObject sourceFile, List<String> extraOptions) {

        DiagnosticCollector<JavaFileObject> diagnostics = new DiagnosticCollector<>();
        StandardJavaFileManager stdFm = compiler.getStandardFileManager(diagnostics, null, null);

        // Create a file manager that writes generated sources to the temp dir
        // and class files to the temp classes dir.
        Path classesDir = tempDir.resolve("classes");
        Path sourcesDir = tempDir.resolve("sources");
        try {
            Files.createDirectories(classesDir);
            Files.createDirectories(sourcesDir);
        } catch (IOException e) {
            throw new RuntimeException("Failed to create temp dirs", e);
        }

        // Collect generated source file paths
        List<Path> generatedFiles = new ArrayList<>();

        ForwardingJavaFileManager<StandardJavaFileManager> fm =
            new ForwardingJavaFileManager<StandardJavaFileManager>(stdFm) {
                @Override
                public JavaFileObject getJavaFileForOutput(Location location,
                        String className, JavaFileObject.Kind kind, FileObject sibling)
                        throws IOException {
                    if (kind == JavaFileObject.Kind.SOURCE) {
                        // Write generated sources to the temp source dir and track them
                        Path outputFile = sourcesDir.resolve(
                            className.replace('.', '/') + ".java");
                        Files.createDirectories(outputFile.getParent());
                        generatedFiles.add(outputFile);
                        return new SimpleJavaFileObject(outputFile.toUri(),
                                JavaFileObject.Kind.SOURCE) {
                            @Override
                            public Writer openWriter() throws IOException {
                                return Files.newBufferedWriter(outputFile);
                            }
                            @Override
                            public CharSequence getCharContent(
                                    boolean ignoreEncodingErrors) throws IOException {
                                return new String(Files.readAllBytes(outputFile));
                            }
                        };
                    }
                    if (kind == JavaFileObject.Kind.CLASS) {
                        Path outputFile = classesDir.resolve(
                            className.replace('.', '/') + ".class");
                        Files.createDirectories(outputFile.getParent());
                        return new SimpleJavaFileObject(outputFile.toUri(),
                                JavaFileObject.Kind.CLASS) {
                            @Override
                            public OutputStream openOutputStream() throws IOException {
                                return Files.newOutputStream(outputFile);
                            }
                        };
                    }
                    return super.getJavaFileForOutput(location, className, kind, sibling);
                }
            };

        // Build options
        List<String> options = new ArrayList<>();
        options.add("-d");
        options.add(classesDir.toString());
        options.add("-s");
        options.add(sourcesDir.toString());
        options.add("-classpath");
        options.add(buildClasspath());
        // Suppress warnings about bootstrap classpath
        options.add("-Xlint:-options");
        if (extraOptions != null) {
            options.addAll(extraOptions);
        }

        CompilationTask task = compiler.getTask(
            null, fm, diagnostics, options, null, Collections.singletonList(sourceFile));
        task.setProcessors(Collections.singletonList(new CleatEntryProcessor()));

        boolean success = task.call();

        // Read the diagnostics for error assertions
        List<String> errorMessages = new ArrayList<>();
        List<String> warningMessages = new ArrayList<>();
        for (Diagnostic<? extends JavaFileObject> d : diagnostics.getDiagnostics()) {
            if (d.getKind() == Diagnostic.Kind.ERROR) {
                errorMessages.add(d.getMessage(null));
            } else if (d.getKind() == Diagnostic.Kind.WARNING
                    || d.getKind() == Diagnostic.Kind.MANDATORY_WARNING) {
                warningMessages.add(d.getMessage(null));
            }
        }

        return new CompilationResult(success, errorMessages, warningMessages, generatedFiles);
    }

    /** Holds the outcome of an in-process compilation. */
    private static class CompilationResult {
        final boolean success;
        final List<String> errors;
        final List<String> warnings;
        final List<Path> generatedSourceFiles;

        CompilationResult(boolean success, List<String> errors, List<String> warnings,
                          List<Path> generatedSourceFiles) {
            this.success = success;
            this.errors = errors;
            this.warnings = warnings;
            this.generatedSourceFiles = generatedSourceFiles;
        }

        boolean hasErrorContaining(String substring) {
            return errors.stream().anyMatch(e -> e.contains(substring));
        }

        boolean hasGeneratedFile(String relativePath) {
            return generatedSourceFiles.stream()
                .anyMatch(p -> p.toString().replace('\\', '/').endsWith(relativePath));
        }
    }

    // ========================================================================
    // Test: successful compilation and generated files
    // ========================================================================

    @Test
    void testSuccessfulCompilationGeneratesExpectedFiles() {
        // WHAT: Verify that compiling a valid @CleatEntry method generates the
        // export wrapper, CleatEntryIndex, and WorkflowEntry files.
        // WHY: The annotation processor must produce the correct generated sources
        // for the TeaVM WASM build pipeline.

        JavaFileObject sourceFile = source("test", "TestWorkflow", validTestSource);
        CompilationResult result = compile(sourceFile, null);

        // The processor generates source files even if their downstream
        // compilation fails due to missing classpath entries (e.g. teavm jars).
        // Check for generated files in all cases.
        assertTrue(
            result.hasGeneratedFile("test/TestWorkflow_testEntry_Export.java"),
            "Expected generated export wrapper 'test/TestWorkflow_testEntry_Export.java' "
            + "to be created by the processor. "
            + "Generated files: " + result.generatedSourceFiles);

        assertTrue(
            result.hasGeneratedFile("cleat/generated/CleatEntryIndex.java"),
            "Expected generated 'cleat/generated/CleatEntryIndex.java' to be created. "
            + "Generated files: " + result.generatedSourceFiles);

        assertTrue(
            result.hasGeneratedFile("cleat/WorkflowEntry.java"),
            "Expected generated 'cleat/WorkflowEntry.java' to be created. "
            + "Generated files: " + result.generatedSourceFiles);

        // Verify export wrapper content includes the annotation's export name
        Path exportFile = result.generatedSourceFiles.stream()
            .filter(p -> p.toString().replace('\\', '/').endsWith("test/TestWorkflow_testEntry_Export.java"))
            .findFirst()
            .orElse(null);
        if (exportFile != null && Files.exists(exportFile)) {
            String content = new String(Files.readAllBytes(exportFile));
            assertTrue(content.contains("test_entry"),
                "Expected the export wrapper to contain the export name 'test_entry' "
                + "from the @CleatEntry annotation, but content was:\n" + content);
            assertTrue(content.contains("TestWorkflow.testEntry"),
                "Expected the export wrapper to reference 'TestWorkflow.testEntry' "
                + "as the method being wrapped, but content was:\n" + content);
        }
    }

    @Test
    void testExportWrapperHasCorrectStructure() throws Exception {
        // WHAT: Verify that the generated export wrapper class has the expected
        // package, class name, @Export annotation, and method signature.
        // WHY: The WASM ABI requires a specific export signature for the host runtime.

        JavaFileObject sourceFile = source("test", "TestWorkflow", validTestSource);
        CompilationResult result = compile(sourceFile, null);

        Path exportFile = result.generatedSourceFiles.stream()
            .filter(p -> p.toString().replace('\\', '/').endsWith("test/TestWorkflow_testEntry_Export.java"))
            .findFirst()
            .orElse(null);
        assertNotNull(exportFile,
            "Export wrapper file must have been generated");
        assertTrue(Files.exists(exportFile),
            "Export wrapper file must exist on disk");

        // Read the full source and check structure
        String content = Files.readString(exportFile);

        // Verify package declaration
        assertTrue(content.contains("package test;"),
            "Generated export wrapper must declare package 'test'. "
            + "Content:\n" + content);

        // Verify class declaration
        assertTrue(content.contains("class TestWorkflow_testEntry_Export"),
            "Generated export wrapper class must be named 'TestWorkflow_testEntry_Export'. "
            + "Content:\n" + content);

        // Verify @Export annotation with the correct name
        assertTrue(content.contains("@Export(name = \"test_entry\")"),
            "Generated export wrapper must have @Export(name = \"test_entry\") annotation. "
            + "Content:\n" + content);

        // Verify the method signature matches the cleat ABI
        assertTrue(content.contains("int argsPtr, int argsLen, int outPtr, int maxOutLen"),
            "Generated export wrapper must have the cleat ABI method signature "
            + "'(int argsPtr, int argsLen, int outPtr, int maxOutLen) -> long'. "
            + "Content:\n" + content);

        // Verify imports
        assertTrue(content.contains("import org.teavm.interop.Export;"),
            "Generated export wrapper must import org.teavm.interop.Export. "
            + "Content:\n" + content);
        assertTrue(content.contains("import cleat.HostCalls;"),
            "Generated export wrapper must import cleat.HostCalls. "
            + "Content:\n" + content);
        assertTrue(content.contains("import cleat.Memory;"),
            "Generated export wrapper must import cleat.Memory. "
            + "Content:\n" + content);
        assertTrue(content.contains("import cleat.JsonHelper;"),
            "Generated export wrapper must import cleat.JsonHelper. "
            + "Content:\n" + content);

        // Verify HostCalls instantiation
        assertTrue(content.contains("HostCalls hostCalls = new HostCalls()"),
            "Generated export wrapper must instantiate HostCalls. "
            + "Content:\n" + content);

        // Verify the result is returned as a packed long
        assertTrue(content.contains("return Memory.encodeExportResult(0, written)"),
            "Generated export wrapper must return Memory.encodeExportResult on success. "
            + "Content:\n" + content);

        // Verify error handling
        assertTrue(content.contains("catch (cleat.TerminalError e)"),
            "Generated export wrapper must catch TerminalError. "
            + "Content:\n" + content);
        assertTrue(content.contains("catch (Exception e)"),
            "Generated export wrapper must catch generic Exception. "
            + "Content:\n" + content);
        assertTrue(content.contains("Memory.TERMINAL_ERROR_CODE"),
            "TerminalError handler must use Memory.TERMINAL_ERROR_CODE. "
            + "Content:\n" + content);
    }

    @Test
    void testCleatEntryIndexContainsWrapperReference() throws Exception {
        // WHAT: Verify CleatEntryIndex references the generated export wrapper class.
        // WHY: CleatEntryIndex.WRAPPER_CLASSES prevents TeaVM from tree-shaking the exports.

        JavaFileObject sourceFile = source("test", "TestWorkflow", validTestSource);
        CompilationResult result = compile(sourceFile, null);

        Path indexFile = result.generatedSourceFiles.stream()
            .filter(p -> p.toString().replace('\\', '/').endsWith("cleat/generated/CleatEntryIndex.java"))
            .findFirst()
            .orElse(null);
        assertNotNull(indexFile,
            "CleatEntryIndex file must have been generated");
        assertTrue(Files.exists(indexFile),
            "CleatEntryIndex file must exist on disk");

        String content = Files.readString(indexFile);

        assertTrue(content.contains("package cleat.generated;"),
            "CleatEntryIndex must be in package cleat.generated. "
            + "Content:\n" + content);
        assertTrue(content.contains("class CleatEntryIndex"),
            "CleatEntryIndex must declare class CleatEntryIndex. "
            + "Content:\n" + content);
        assertTrue(content.contains("TestWorkflow_testEntry_Export.class"),
            "CleatEntryIndex.WRAPPER_CLASSES must reference the generated export wrapper class. "
            + "Content:\n" + content);
        assertTrue(content.contains("getEntries()"),
            "CleatEntryIndex must have getEntries() method. "
            + "Content:\n" + content);
        assertTrue(content.contains("\"test_entry\""),
            "getEntries() must return the export name 'test_entry'. "
            + "Content:\n" + content);
    }

    @Test
    void testWorkflowEntryReferencesIndex() throws Exception {
        // WHAT: Verify WorkflowEntry references CleatEntryIndex.WRAPPER_CLASSES.
        // WHY: WorkflowEntry is the TeaVM static analysis root; the reference chain
        // prevents tree-shaking of the export wrappers.

        JavaFileObject sourceFile = source("test", "TestWorkflow", validTestSource);
        CompilationResult result = compile(sourceFile, null);

        Path wfEntryFile = result.generatedSourceFiles.stream()
            .filter(p -> p.toString().replace('\\', '/').endsWith("cleat/WorkflowEntry.java"))
            .findFirst()
            .orElse(null);
        assertNotNull(wfEntryFile,
            "WorkflowEntry file must have been generated");
        assertTrue(Files.exists(wfEntryFile),
            "WorkflowEntry file must exist on disk");

        String content = Files.readString(wfEntryFile);

        assertTrue(content.contains("package cleat;"),
            "WorkflowEntry must be in package cleat. "
            + "Content:\n" + content);
        assertTrue(content.contains("class WorkflowEntry"),
            "WorkflowEntry must declare class WorkflowEntry. "
            + "Content:\n" + content);
        assertTrue(content.contains("CleatEntryIndex.WRAPPER_CLASSES"),
            "WorkflowEntry must reference CleatEntryIndex.WRAPPER_CLASSES. "
            + "Content:\n" + content);
    }

    // ========================================================================
    // Error validation tests
    // ========================================================================

    @Test
    void testNonStaticMethodProducesError() {
        // WHAT: Verify the processor rejects a non-static @CleatEntry method
        // WHY: WASM export functions must be static (no 'this' reference)

        String source = ""
            + "package test;\n"
            + "import cleat.HostCalls;\n"
            + "import cleat.CleatEntry;\n"
            + "public class BadWorkflow {\n"
            + "    @CleatEntry(name = \"bad\")\n"
            + "    public String nonStatic(HostCalls h, String input) {\n"
            + "        return \"{}\";\n"
            + "    }\n"
            + "}\n";

        JavaFileObject sourceFile = source("test", "BadWorkflow", source);
        CompilationResult result = compile(sourceFile, null);

        assertFalse(result.success,
            "Expected compilation to fail for a non-static @CleatEntry method, "
            + "but it succeeded. Errors: " + result.errors);

        boolean hasStaticError = result.hasErrorContaining("static")
            || result.hasErrorContaining("STATIC");
        assertTrue(hasStaticError,
            "Expected an error message mentioning 'static' for a non-static @CleatEntry method. "
            + "Actual errors: " + result.errors);
    }

    @Test
    void testNonPublicMethodProducesError() {
        // WHAT: Verify the processor rejects a non-public @CleatEntry method
        // WHY: The generated export wrapper must be able to invoke the method;
        // non-public methods are inaccessible from outside the package.

        String source = ""
            + "package test;\n"
            + "import cleat.HostCalls;\n"
            + "import cleat.CleatEntry;\n"
            + "public class BadWorkflow {\n"
            + "    @CleatEntry(name = \"bad\")\n"
            + "    static String packagePrivate(HostCalls h, String input) {\n"
            + "        return \"{}\";\n"
            + "    }\n"
            + "}\n";

        JavaFileObject sourceFile = source("test", "BadWorkflow", source);
        CompilationResult result = compile(sourceFile, null);

        assertFalse(result.success,
            "Expected compilation to fail for a non-public @CleatEntry method, "
            + "but it succeeded. Errors: " + result.errors);

        boolean hasPublicError = result.hasErrorContaining("public")
            || result.hasErrorContaining("PUBLIC");
        assertTrue(hasPublicError,
            "Expected an error message mentioning 'public' for a non-public @CleatEntry method. "
            + "Actual errors: " + result.errors);
    }

    @Test
    void testMissingHostCallsFirstParameterProducesError() {
        // WHAT: Verify the processor rejects a @CleatEntry method without HostCalls
        // as the first parameter
        // WHY: The HostCalls instance provides the WASM host function API; without it,
        // the workflow cannot interact with the host runtime.

        String source = ""
            + "package test;\n"
            + "import cleat.HostCalls;\n"
            + "import cleat.CleatEntry;\n"
            + "public class BadWorkflow {\n"
            + "    @CleatEntry(name = \"bad\")\n"
            + "    public static String noHostCalls(String input) {\n"
            + "        return \"{}\";\n"
            + "    }\n"
            + "}\n";

        JavaFileObject sourceFile = source("test", "BadWorkflow", source);
        CompilationResult result = compile(sourceFile, null);

        assertFalse(result.success,
            "Expected compilation to fail for a @CleatEntry method without HostCalls "
            + "as first parameter, but it succeeded. Errors: " + result.errors);

        boolean hasHostCallsError = result.hasErrorContaining("HostCalls")
            || result.errors.stream().anyMatch(e ->
                e.contains("first parameter") && e.contains("cleat"));
        assertTrue(hasHostCallsError,
            "Expected an error message mentioning 'HostCalls' as required first parameter. "
            + "Actual errors: " + result.errors);
    }

    @Test
    void testMethodWithWrongFirstParameterTypeProducesError() {
        // WHAT: Verify the processor rejects a @CleatEntry method whose first
        // parameter is not HostCalls (e.g. String)
        // WHY: Only HostCalls is accepted as the first parameter

        String source = ""
            + "package test;\n"
            + "import cleat.HostCalls;\n"
            + "import cleat.CleatEntry;\n"
            + "public class BadWorkflow {\n"
            + "    @CleatEntry(name = \"bad\")\n"
            + "    public static String wrongFirstParam(String notHost, String input) {\n"
            + "        return \"{}\";\n"
            + "    }\n"
            + "}\n";

        JavaFileObject sourceFile = source("test", "BadWorkflow", source);
        CompilationResult result = compile(sourceFile, null);

        assertFalse(result.success,
            "Expected compilation to fail when the first parameter is not HostCalls, "
            + "but it succeeded. Errors: " + result.errors);

        boolean hasHostCallsError = result.hasErrorContaining("HostCalls")
            || result.errors.stream().anyMatch(e ->
                e.contains("first parameter"));
        assertTrue(hasHostCallsError,
            "Expected an error message mentioning 'HostCalls' as required first parameter. "
            + "Actual errors: " + result.errors);
    }

    // ========================================================================
    // Edge case tests
    // ========================================================================

    @Test
    void testAnnotationOnFieldProducesError() {
        // WHAT: Verify @CleatEntry on a field (not a method) produces an error
        // WHY: @CleatEntry is @Target(METHOD) — the processor should validate this

        String source = ""
            + "package test;\n"
            + "import cleat.CleatEntry;\n"
            + "public class BadWorkflow {\n"
            + "    @CleatEntry(name = \"bad\")\n"
            + "    public static String someField = \"value\";\n"
            + "}\n";

        JavaFileObject sourceFile = source("test", "BadWorkflow", source);
        CompilationResult result = compile(sourceFile, null);

        assertFalse(result.success,
            "Expected compilation to fail for @CleatEntry on a field, "
            + "but it succeeded. Errors: " + result.errors);
        boolean hasMethodError = result.hasErrorContaining("method")
            || result.hasErrorContaining("METHOD");
        assertTrue(hasMethodError,
            "Expected an error message mentioning 'method' for @CleatEntry on a field. "
            + "Actual errors: " + result.errors);
    }

    @Test
    void testDefaultNameUsesMethodName() throws Exception {
        // WHAT: Verify that when @CleatEntry has no explicit name, the processor
        // uses the Java method name as the export name
        // WHY: Default behavior should be convenient: method name = export name

        String source = ""
            + "package test;\n"
            + "import cleat.HostCalls;\n"
            + "import cleat.CleatEntry;\n"
            + "public class DefaultNameWorkflow {\n"
            + "    @CleatEntry\n"
            + "    public static String myWorkflow(HostCalls h, String input) {\n"
            + "        return \"{}\";\n"
            + "    }\n"
            + "}\n";

        JavaFileObject sourceFile = source("test", "DefaultNameWorkflow", source);
        CompilationResult result = compile(sourceFile, null);

        // Check the generated export wrapper uses the method name
        Path exportFile = result.generatedSourceFiles.stream()
            .filter(p -> p.toString().replace('\\', '/')
                .endsWith("test/DefaultNameWorkflow_myWorkflow_Export.java"))
            .findFirst()
            .orElse(null);
        assertNotNull(exportFile,
            "Expected generated export wrapper 'DefaultNameWorkflow_myWorkflow_Export' "
            + "to use the method name as the wrapper class name. "
            + "Generated files: " + result.generatedSourceFiles);

        if (Files.exists(exportFile)) {
            String content = Files.readString(exportFile);
            assertTrue(content.contains("@Export(name = \"myWorkflow\")"),
                "Expected @Export to use the method name 'myWorkflow' when no name "
                + "is specified in @CleatEntry. "
                + "Content:\n" + content);
        }
    }

    @Test
    void testVoidReturnTypeGeneratesEmptyJson() throws Exception {
        // WHAT: Verify that a void @CleatEntry method generates code that returns "{}"
        // WHY: Void methods still need to return a valid JSON result

        String source = ""
            + "package test;\n"
            + "import cleat.HostCalls;\n"
            + "import cleat.CleatEntry;\n"
            + "public class VoidWorkflow {\n"
            + "    @CleatEntry(name = \"void_entry\")\n"
            + "    public static void doVoid(HostCalls h, String input) {\n"
            + "        h.cleatLog(\"void method\");\n"
            + "    }\n"
            + "}\n";

        JavaFileObject sourceFile = source("test", "VoidWorkflow", source);
        CompilationResult result = compile(sourceFile, null);

        Path exportFile = result.generatedSourceFiles.stream()
            .filter(p -> p.toString().replace('\\', '/')
                .endsWith("test/VoidWorkflow_doVoid_Export.java"))
            .findFirst()
            .orElse(null);
        assertNotNull(exportFile,
            "Generated export wrapper must exist for void method");
        if (Files.exists(exportFile)) {
            String content = Files.readString(exportFile);
            assertTrue(content.contains("String resultJSON = \"{}\""),
                "Expected void method export wrapper to generate empty JSON result \"{}\". "
                + "Content:\n" + content);
        }
    }

    @Test
    void testMultipleCleatEntryMethods() throws Exception {
        // WHAT: Verify the processor handles multiple @CleatEntry methods in one class
        // WHY: A workflow class may expose multiple entry points

        String source = ""
            + "package test;\n"
            + "import cleat.HostCalls;\n"
            + "import cleat.CleatEntry;\n"
            + "public class MultiWorkflow {\n"
            + "    @CleatEntry(name = \"start\")\n"
            + "    public static String start(HostCalls h, String input) {\n"
            + "        return \"{}\";\n"
            + "    }\n"
            + "    @CleatEntry(name = \"cancel\")\n"
            + "    public static String cancel(HostCalls h, String input) {\n"
            + "        return \"{}\";\n"
            + "    }\n"
            + "}\n";

        JavaFileObject sourceFile = source("test", "MultiWorkflow", source);
        CompilationResult result = compile(sourceFile, null);

        assertTrue(
            result.hasGeneratedFile("test/MultiWorkflow_start_Export.java"),
            "Expected generated export wrapper for 'start' method. "
            + "Generated files: " + result.generatedSourceFiles);
        assertTrue(
            result.hasGeneratedFile("test/MultiWorkflow_cancel_Export.java"),
            "Expected generated export wrapper for 'cancel' method. "
            + "Generated files: " + result.generatedSourceFiles);
    }

    @Test
    void testProcessorDoesNotGenerateFilesWithoutCleatEntry() {
        // WHAT: Verify the processor generates no wrappers when no @CleatEntry is present,
        // but still generates the aggregator files (CleatEntryIndex, WorkflowEntry)
        // WHY: The aggregator must always exist to serve as the TeaVM analysis root,
        // even when there are no entries

        String source = ""
            + "package test;\n"
            + "public class NoAnnotationWorkflow {\n"
            + "    public static String plainMethod(String input) {\n"
            + "        return \"{}\";\n"
            + "    }\n"
            + "}\n";

        JavaFileObject sourceFile = source("test", "NoAnnotationWorkflow", source);
        CompilationResult result = compile(sourceFile, null);

        // No export wrappers should be generated
        boolean hasExportWrapper = result.generatedSourceFiles.stream()
            .anyMatch(p -> p.toString().contains("_Export"));
        assertFalse(hasExportWrapper,
            "Expected no export wrappers when no @CleatEntry annotation is present. "
            + "Generated files: " + result.generatedSourceFiles);

        // But CleatEntryIndex and WorkflowEntry should still be generated
        assertTrue(
            result.hasGeneratedFile("cleat/generated/CleatEntryIndex.java"),
            "Expected CleatEntryIndex to be generated even without @CleatEntry methods. "
            + "Generated files: " + result.generatedSourceFiles);
        assertTrue(
            result.hasGeneratedFile("cleat/WorkflowEntry.java"),
            "Expected WorkflowEntry to be generated even without @CleatEntry methods. "
            + "Generated files: " + result.generatedSourceFiles);
    }
}
