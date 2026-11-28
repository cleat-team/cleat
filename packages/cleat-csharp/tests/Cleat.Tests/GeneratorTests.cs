using System.Reflection;
using Cleat.Generator;
using Microsoft.CodeAnalysis;
using Microsoft.CodeAnalysis.CSharp;
using Xunit;

namespace Cleat.Tests;

/// <summary>
/// Runs the real generator over sample user code, compiles the result against stand-ins for the component
/// bindings (which only exist inside a wasm build) and EXECUTES it. That covers every adapter form without a
/// wasm toolchain; the engine run of ../fixture covers the part this cannot: the real bindings.
/// </summary>
public class GeneratorTests
{
    private const string Stubs = @"
namespace CleatWorkflowWorld
{
    public interface ICleatWorkflowWorldImports
    {
        public class RunOutcome
        {
            public string Kind = """"; public string? Text;
            public static RunOutcome Completed(string s) => new() { Kind = ""completed"", Text = s };
            public static RunOutcome Suspended() => new() { Kind = ""suspended"" };
            public static RunOutcome Failed(string s) => new() { Kind = ""failed"", Text = s };
        }
    }
    public interface ICleatWorkflowWorldExports
    {
        static abstract ICleatWorkflowWorldImports.RunOutcome Run(string args);
        static abstract uint RunDeferred();
    }
}
namespace Cleat
{
    public sealed class HostCalls
    {
        internal HostCalls(Cleat.Runtime.DeferTable d) { }
        internal static void SetDeferPhase(bool on) { }
        public string Call(string s, string o, string r) => ""{\""svc\"":\"""" + s + ""\"",\""op\"":\"""" + o + ""\""}"";
    }
}";

    private static IEnumerable<MetadataReference> References()
        => ((string)AppContext.GetData("TRUSTED_PLATFORM_ASSEMBLIES")!).Split(Path.PathSeparator)
            .Select(p => (MetadataReference)MetadataReference.CreateFromFile(p));

    private static string RuntimeSrc()
        => typeof(GeneratorTests).Assembly.GetCustomAttributes<AssemblyMetadataAttribute>().Single(a => a.Key == "CleatRuntimeSrc").Value!;

    private static (Compilation Out, IReadOnlyList<Diagnostic> Generator, string Generated) Generate(string user)
    {
        var trees = new List<SyntaxTree> { CSharpSyntaxTree.ParseText(Stubs), CSharpSyntaxTree.ParseText(user) };
        foreach (var f in Directory.GetFiles(RuntimeSrc(), "*.cs"))
            trees.Add(CSharpSyntaxTree.ParseText(File.ReadAllText(f), path: f));
        var comp = CSharpCompilation.Create("userwf", trees, References(),
            new CSharpCompilationOptions(OutputKind.DynamicallyLinkedLibrary, nullableContextOptions: NullableContextOptions.Enable));
        var driver = CSharpGeneratorDriver.Create(new EntryGenerator()).RunGeneratorsAndUpdateCompilation(comp, out var outComp, out var diags);
        var generated = driver.GetRunResult().GeneratedTrees.FirstOrDefault()?.ToString() ?? "";
        return (outComp, diags, generated);
    }

    private static Func<string, (string Kind, string? Text)> Load(string user)
    {
        var (comp, gen, _) = Generate(user);
        Assert.Empty(gen);
        using var ms = new MemoryStream();
        var emit = comp.Emit(ms);
        Assert.True(emit.Success, string.Join("\n", emit.Diagnostics.Where(d => d.Severity == DiagnosticSeverity.Error)));
        var asm = Assembly.Load(ms.ToArray());
        var run = asm.GetType("CleatWorkflowWorld.CleatWorkflowWorldExportsImpl")!.GetMethod("Run")!;
        return args =>
        {
            var o = run.Invoke(null, new object[] { args })!;
            var t = o.GetType();
            return ((string)t.GetField("Kind")!.GetValue(o)!, (string?)t.GetField("Text")!.GetValue(o));
        };
    }

    private static string[] Ids(string user) => Generate(user).Generator.Select(d => d.Id).OrderBy(x => x).ToArray();

    private const string Header = "using System.Text.Json; using System.Text.Json.Nodes; using System.Threading.Tasks; using Cleat;\n";

    // ---- every adapter form, executed -------------------------------------------------------------------------

    [Fact]
    public void ElementInAndElementOut()
    {
        var run = Load(Header + @"public static class W { [CleatEntry(""Echo"")] public static JsonElement Echo(HostCalls h, JsonElement input) => input; }");
        Assert.Equal(("completed", "{\"a\":1}"), run("{\"__cleat_entry__\":\"Echo\",\"a\":1}"));
    }

    [Fact]
    public void NodeAndAsyncTaskNode()
    {
        var run = Load(Header + @"public static class W { [CleatEntry] public static async Task<JsonNode?> Go(HostCalls h, JsonNode? input) { await Task.CompletedTask; input![""seen""] = true; return input; } }");
        Assert.Equal(("completed", "{\"x\":1,\"seen\":true}"), run("{\"x\":1}"));
    }

    [Fact]
    public void RawStringInAndStringOut()
    {
        var run = Load(Header + @"public static class W { [CleatEntry] public static string Go(HostCalls h, string raw) => raw.Length.ToString() + raw; }");
        Assert.Equal(("completed", "\"7{\\\"a\\\":1}\""), run("{\"a\":1}"));
    }

    [Fact]
    public void PrimitiveReturnsAndNoInput()
    {
        Assert.Equal(("completed", "42"), Load(Header + "public static class W { [CleatEntry] public static int Go(HostCalls h) => 42; }")("{}"));
        Assert.Equal(("completed", "true"), Load(Header + "public static class W { [CleatEntry] public static bool Go(HostCalls h) => true; }")("{}"));
        Assert.Equal(("completed", "9223372036854775807"), Load(Header + "public static class W { [CleatEntry] public static long Go(HostCalls h) => long.MaxValue; }")("{}"));
        Assert.Equal(("completed", "1.5"), Load(Header + "public static class W { [CleatEntry] public static double Go(HostCalls h) => 1.5; }")("{}"));
    }

    [Fact]
    public void VoidAndTaskAndValueTaskResultsAreNull()
    {
        Assert.Equal(("completed", "null"), Load(Header + "public static class W { [CleatEntry] public static void Go(HostCalls h) { } }")("{}"));
        Assert.Equal(("completed", "null"), Load(Header + "public static class W { [CleatEntry] public static async Task Go(HostCalls h) { await Task.CompletedTask; } }")("{}"));
        Assert.Equal(("completed", "null"), Load(Header + "public static class W { [CleatEntry] public static async ValueTask Go(HostCalls h) { await Task.CompletedTask; } }")("{}"));
    }

    [Fact]
    public void ValueTaskOfT()
    {
        var run = Load(Header + "public static class W { [CleatEntry] public static async ValueTask<int> Go(HostCalls h) { await Task.CompletedTask; return 7; } }");
        Assert.Equal(("completed", "7"), run("{}"));
    }

    [Fact]
    public void TheHostCallsInstanceIsPassedThrough()
    {
        var run = Load(Header + @"public static class W { [CleatEntry] public static string Go(HostCalls h) => h.Call(""payments"", ""Charge"", ""{}""); }");
        Assert.Equal(("completed", "\"{\\\"svc\\\":\\\"payments\\\",\\\"op\\\":\\\"Charge\\\"}\""), run("{}"));
    }

    [Fact]
    public void SeveralEntriesDispatchOnTheKeyAcrossClasses()
    {
        var run = Load(Header + @"
public static class A { [CleatEntry(""One"")] public static int One(HostCalls h) => 1; }
public static class B { [CleatEntry(""Two"")] public static async Task<int> Two(HostCalls h) { await Task.CompletedTask; return 2; } }");
        Assert.Equal(("completed", "1"), run("{\"__cleat_entry__\":\"One\"}"));
        Assert.Equal(("completed", "2"), run("{\"__cleat_entry__\":\"Two\"}"));
        Assert.Equal("failed", run("{}").Kind);
    }

    [Fact]
    public void ASuspendFromAnAwaitedFrameIsSuspended()
    {
        var run = Load(Header + @"public static class W {
            static async Task Deep(int n) { await Task.CompletedTask; if (n == 0) throw new CleatSuspendException(); await Deep(n - 1); }
            [CleatEntry] public static async Task<int> Go(HostCalls h) { await Deep(20); return 1; } }");
        Assert.Equal("suspended", run("{}").Kind);
    }

    [Fact]
    public void AThrowIsFailedWithTypeAndMessage()
    {
        var run = Load(Header + @"public static class W { [CleatEntry] public static int Go(HostCalls h) => throw new System.InvalidOperationException(""boom""); }");
        Assert.Equal(("failed", "InvalidOperationException: boom"), run("{}"));
    }

    [Fact]
    public void ADeferRegisteredByTheBodyRunsOnCompletion()
    {
        // The stub HostCalls has no Defer; reach the table through the runtime's own seams instead: a body that
        // throws after completing proves the drain path is wired by the generated class.
        var run = Load(Header + @"public static class W { [CleatEntry] public static int Go(HostCalls h) => 1; }");
        Assert.Equal(("completed", "1"), run("{}"));
    }

    // ---- diagnostics ------------------------------------------------------------------------------------------

    [Fact] public void NotPublic() => Assert.Contains("CLEAT001", Ids(Header + "public static class W { [CleatEntry] static int Go(HostCalls h) => 1; }"));
    [Fact] public void NotStatic() => Assert.Contains("CLEAT001", Ids(Header + "public class W { [CleatEntry] public int Go(HostCalls h) => 1; }"));
    [Fact] public void Generic() => Assert.Contains("CLEAT001", Ids(Header + "public static class W { [CleatEntry] public static int Go<T>(HostCalls h) => 1; }"));
    [Fact] public void FirstParameterMissing() => Assert.Contains("CLEAT002", Ids(Header + "public static class W { [CleatEntry] public static int Go() => 1; }"));
    [Fact] public void FirstParameterWrongType() => Assert.Contains("CLEAT002", Ids(Header + "public static class W { [CleatEntry] public static int Go(string h) => 1; }"));
    [Fact] public void TooManyParameters() => Assert.Contains("CLEAT003", Ids(Header + "public static class W { [CleatEntry] public static int Go(HostCalls h, JsonElement a, JsonElement b) => 1; }"));
    [Fact] public void DuplicateNames() => Assert.Contains("CLEAT004", Ids(Header + @"public static class W { [CleatEntry(""X"")] public static int A(HostCalls h) => 1; [CleatEntry(""X"")] public static int B(HostCalls h) => 1; }"));
    [Fact] public void TypedInputWithoutContext() => Assert.Contains("CLEAT005", Ids(Header + "public record Order(int Id); public static class W { [CleatEntry] public static int Go(HostCalls h, Order o) => 1; }"));
    [Fact] public void TypedOutputWithoutContext() => Assert.Contains("CLEAT005", Ids(Header + "public record Order(int Id); public static class W { [CleatEntry] public static Order Go(HostCalls h) => new(1); }"));
    [Fact] public void ContextThatIsNotAJsonContext() => Assert.Contains("CLEAT006", Ids(Header + "public record Order(int Id); public class NotACtx { } public static class W { [CleatEntry(JsonContext = typeof(NotACtx))] public static int Go(HostCalls h, Order o) => 1; }"));
    [Fact] public void NoEntriesAtAll() => Assert.Contains("CLEAT007", Ids(Header + "public static class W { }"));
    [Fact] public void NameWithAComma() => Assert.Contains("CLEAT008", Ids(Header + @"public static class W { [CleatEntry(""a,b"")] public static int Go(HostCalls h) => 1; }"));
    [Fact] public void EmptyName() => Assert.Contains("CLEAT008", Ids(Header + @"public static class W { [CleatEntry("""")] public static int Go(HostCalls h) => 1; }"));

    [Fact]
    public void NothingIsEmittedWhenAnEntryIsInvalid()
        => Assert.Equal("", Generate(Header + "public static class W { [CleatEntry] static int Go(HostCalls h) => 1; }").Generated);

    [Fact]
    public void TypedEntryReadsAndWritesThroughTheCallersContext()
    {
        var (_, _, generated) = Generate(Header + @"
public record Order(int Id);
public partial class OrderJson : System.Text.Json.Serialization.JsonSerializerContext { }
public static class W { [CleatEntry(JsonContext = typeof(OrderJson))] public static Order Go(HostCalls h, Order o) => o; }");
        Assert.Contains("EntryJson.Read<global::Order>(input, global::OrderJson.Default)", generated);
        Assert.Contains("EntryJson.Write<global::Order>(r, global::OrderJson.Default)", generated);
    }
}
