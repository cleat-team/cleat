using System;
using System.Collections.Generic;
using System.IO;
using System.Collections.Immutable;
using System.Linq;
using System.Text;
using System.Threading;
using Microsoft.CodeAnalysis;
using Microsoft.CodeAnalysis.CSharp.Syntax;

namespace Cleat.Generator;

/// <summary>
/// Emits <c>CleatWorkflowWorldExportsImpl</c>: the class the generated component bindings require, with the
/// dispatch table over every <c>[CleatEntry]</c> method and the <c>Run</c>/<c>RunDeferred</c> exports. User code
/// never writes the exception boundary, so it cannot forget it.
/// </summary>
[Generator(LanguageNames.CSharp)]
public sealed class EntryGenerator : IIncrementalGenerator
{
    internal const string AttributeName = "Cleat.CleatEntryAttribute";

    private static readonly DiagnosticDescriptor NotPublicStatic = Error("CLEAT001",
        "An entry point must be a public, non-generic, static method of a non-private type",
        "'{0}' is marked [CleatEntry] but is not a public, non-generic, static method of an accessible type");
    private static readonly DiagnosticDescriptor FirstParameter = Error("CLEAT002",
        "The first parameter of an entry point must be Cleat.HostCalls",
        "'{0}' must take Cleat.HostCalls as its first parameter");
    private static readonly DiagnosticDescriptor TooManyParameters = Error("CLEAT003",
        "An entry point takes the host and at most one input",
        "'{0}' has {1} parameters after the host; an entry point takes at most one input (a record for several fields)");
    private static readonly DiagnosticDescriptor DuplicateName = Error("CLEAT004",
        "Two entry points have the same name",
        "The entry name '{0}' is used by more than one method");
    private static readonly DiagnosticDescriptor NeedsContext = Error("CLEAT005",
        "A typed entry point needs a JsonSerializerContext",
        "'{0}' uses {1}, which is not JsonElement, JsonNode, string, bool, int, long or double; set " +
        "[CleatEntry(JsonContext = typeof(...))] to a JsonSerializerContext that declares " +
        "[JsonSerializable(typeof({1}))], or use JsonElement/JsonNode");
    private static readonly DiagnosticDescriptor BadContext = Error("CLEAT006",
        "JsonContext must be a JsonSerializerContext",
        "'{0}' names {1} as its JsonContext, but that type does not derive from " +
        "System.Text.Json.Serialization.JsonSerializerContext");
    private static readonly DiagnosticDescriptor NoEntries = Error("CLEAT007",
        "A workflow component needs at least one entry point",
        "No method is marked [CleatEntry], so the component would fail every run");
    private static readonly DiagnosticDescriptor EmptyName = Error("CLEAT008",
        "An entry point name cannot be empty or contain a comma or a line break",
        "'{0}' has an empty [CleatEntry] name, or one containing a comma or a line break");

    private static DiagnosticDescriptor Error(string id, string title, string message)
        => new(id, title, message, "Cleat", DiagnosticSeverity.Error, isEnabledByDefault: true);

    public void Initialize(IncrementalGeneratorInitializationContext context)
    {
        var entries = context.SyntaxProvider
            .ForAttributeWithMetadataName(AttributeName, static (n, _) => n is MethodDeclarationSyntax, Extract)
            .Collect();
        // The path of the entry-point list, from the compiler-visible MSBuild property (build/Cleat.Sdk.targets).
        var listPath = context.AnalyzerConfigOptionsProvider.Select(static (p, _) =>
            p.GlobalOptions.TryGetValue("build_property.CleatEntryPointsFile", out var v) ? v : "");
        context.RegisterSourceOutput(entries.Combine(listPath), static (ctx, pair) =>
        {
            Emit(ctx, pair.Left);
            WriteEntryList(ctx, pair.Left, pair.Right);
        });
    }

    // ---- model ------------------------------------------------------------------------------------------------

    private enum Kind { None, Element, Node, Raw, String, Bool, Int, Long, Double, Typed }

    private sealed class Entry
    {
        public string Name = "";
        public string Type = "";       // fully qualified containing type
        public string Method = "";
        public Location Location = Location.None;
        public Kind Input = Kind.None;
        public string InputType = "";
        public Kind Output = Kind.None;   // None = no result (null)
        public string OutputType = "";
        public bool IsAsync;
        public string? Context;           // fully qualified JsonSerializerContext
        public List<Diagnostic> Diagnostics = new();
    }

    private static readonly SymbolDisplayFormat Fq = SymbolDisplayFormat.FullyQualifiedFormat;

    private static string Display(ITypeSymbol t)
        => t.WithNullableAnnotation(NullableAnnotation.NotAnnotated).ToDisplayString(Fq);

    private static Kind Classify(ITypeSymbol t, bool forInput)
    {
        switch (t.SpecialType)
        {
            case SpecialType.System_Boolean: return Kind.Bool;
            case SpecialType.System_Int32: return Kind.Int;
            case SpecialType.System_Int64: return Kind.Long;
            case SpecialType.System_Double: return Kind.Double;
            case SpecialType.System_String: return forInput ? Kind.Raw : Kind.String;
        }
        switch (Display(t))
        {
            case "global::System.Text.Json.JsonElement": return Kind.Element;
            case "global::System.Text.Json.Nodes.JsonNode": return Kind.Node;
        }
        return Kind.Typed;
    }

    private static Entry Extract(GeneratorAttributeSyntaxContext ctx, CancellationToken ct)
    {
        var m = (IMethodSymbol)ctx.TargetSymbol;
        var attr = ctx.Attributes[0];
        var e = new Entry
        {
            Type = Display(m.ContainingType),
            Method = m.Name,
            Location = m.Locations.FirstOrDefault() ?? Location.None,
        };
        var explicitName = attr.ConstructorArguments.Length == 1 ? attr.ConstructorArguments[0].Value as string : null;
        e.Name = explicitName ?? m.Name;
        if (e.Name.Length == 0 || e.Name.IndexOfAny(new[] { ',', '\n', '\r' }) >= 0)
            e.Diagnostics.Add(Diagnostic.Create(EmptyName, e.Location, m.Name));

        foreach (var named in attr.NamedArguments)
        {
            if (named.Key != "JsonContext" || named.Value.Value is not ITypeSymbol ctxType) continue;
            var derives = false;
            for (var b = ctxType.BaseType; b is not null; b = b.BaseType)
                if (b.ToDisplayString() == "System.Text.Json.Serialization.JsonSerializerContext") { derives = true; break; }
            if (!derives)
                e.Diagnostics.Add(Diagnostic.Create(BadContext, e.Location, m.Name, ctxType.ToDisplayString()));
            e.Context = Display(ctxType);
        }

        var accessible = m.DeclaredAccessibility == Accessibility.Public && m.IsStatic && !m.IsGenericMethod;
        for (var t = m.ContainingType; t is not null; t = t.ContainingType)
            if (t.DeclaredAccessibility is Accessibility.Private or Accessibility.Protected
                or Accessibility.ProtectedAndInternal || t.IsGenericType)
                accessible = false;
        if (!accessible)
            e.Diagnostics.Add(Diagnostic.Create(NotPublicStatic, e.Location, m.Name));

        if (m.Parameters.Length == 0 || Display(m.Parameters[0].Type) != "global::Cleat.HostCalls")
        {
            e.Diagnostics.Add(Diagnostic.Create(FirstParameter, e.Location, m.Name));
        }
        else if (m.Parameters.Length > 2)
        {
            e.Diagnostics.Add(Diagnostic.Create(TooManyParameters, e.Location, m.Name, m.Parameters.Length - 1));
        }
        else if (m.Parameters.Length == 2)
        {
            e.Input = Classify(m.Parameters[1].Type, forInput: true);
            e.InputType = Display(m.Parameters[1].Type);
        }

        // Return type: T, Task, Task<T>, ValueTask, ValueTask<T>, or void.
        ITypeSymbol? result = m.ReturnType;
        if (m.ReturnsVoid)
        {
            result = null;
        }
        else
        {
            var orig = m.ReturnType.OriginalDefinition.ToDisplayString();
            if (orig == "System.Threading.Tasks.Task" || orig == "System.Threading.Tasks.ValueTask")
            {
                e.IsAsync = true;
                result = null;
            }
            else if (orig == "System.Threading.Tasks.Task<TResult>" || orig == "System.Threading.Tasks.ValueTask<TResult>")
            {
                e.IsAsync = true;
                result = ((INamedTypeSymbol)m.ReturnType).TypeArguments[0];
            }
        }
        if (result is not null)
        {
            e.Output = Classify(result, forInput: false);
            e.OutputType = Display(result);
        }

        if (e.Context is null)
        {
            if (e.Input == Kind.Typed)
                e.Diagnostics.Add(Diagnostic.Create(NeedsContext, e.Location, m.Name, e.InputType));
            if (e.Output == Kind.Typed)
                e.Diagnostics.Add(Diagnostic.Create(NeedsContext, e.Location, m.Name, e.OutputType));
        }
        return e;
    }

    // ---- emission ---------------------------------------------------------------------------------------------

    private static string Lit(string s) => "@\"" + s.Replace("\"", "\"\"") + "\"";

    private static string Read(Entry e) => e.Input switch
    {
        Kind.Element => "input",
        Kind.Node => "global::Cleat.Runtime.EntryJson.ToNode(input)",
        Kind.Raw => "input.GetRawText()",
        Kind.Bool => "input.GetBoolean()",
        Kind.Int => "input.GetInt32()",
        Kind.Long => "input.GetInt64()",
        Kind.Double => "input.GetDouble()",
        Kind.String => "input.GetString()!",
        Kind.Typed => $"global::Cleat.Runtime.EntryJson.Read<{e.InputType}>(input, {e.Context}.Default)",
        _ => "",
    };

    private static string Write(Entry e, string v) => e.Output switch
    {
        Kind.Element => $"global::Cleat.Runtime.EntryJson.Element({v})",
        Kind.Node => $"global::Cleat.Runtime.EntryJson.Node({v})",
        Kind.String => $"global::Cleat.Runtime.EntryJson.String({v})",
        Kind.Bool => $"global::Cleat.Runtime.EntryJson.Bool({v})",
        Kind.Int => $"global::Cleat.Runtime.EntryJson.Int({v})",
        Kind.Long => $"global::Cleat.Runtime.EntryJson.Long({v})",
        Kind.Double => $"global::Cleat.Runtime.EntryJson.Double({v})",
        Kind.Typed => $"global::Cleat.Runtime.EntryJson.Write<{e.OutputType}>({v}, {e.Context}.Default)",
        _ => "global::Cleat.Runtime.EntryJson.Null",
    };

    private static readonly DiagnosticDescriptor ListNotWritten = new("CLEAT009",
        "The entry-point list could not be written",
        "Could not write the entry-point list '{0}': {1}", "Cleat", DiagnosticSeverity.Error, isEnabledByDefault: true);

    /// <summary>
    /// The one deliberate side effect in this generator: it writes the names it has just validated to a file for
    /// the build chain. It writes only when the content changed, so an unchanged list keeps its timestamp, and only
    /// for a set of entries that passed validation.
    /// </summary>
    private static void WriteEntryList(SourceProductionContext ctx, ImmutableArray<Entry> entries, string path)
    {
        if (string.IsNullOrEmpty(path) || entries.IsDefaultOrEmpty) return;
        if (entries.Any(e => e.Diagnostics.Count > 0) || entries.GroupBy(e => e.Name).Any(g => g.Count() > 1)) return;
        var text = string.Concat(entries.Select(e => e.Name + "\n"));
        try
        {
            Directory.CreateDirectory(Path.GetDirectoryName(Path.GetFullPath(path))!);
            if (!File.Exists(path) || File.ReadAllText(path) != text)
                File.WriteAllText(path, text, new System.Text.UTF8Encoding(false));
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
        {
            ctx.ReportDiagnostic(Diagnostic.Create(ListNotWritten, Location.None, path, ex.Message));
        }
    }

    private static void Emit(SourceProductionContext ctx, ImmutableArray<Entry> entries)
    {
        foreach (var e in entries)
            foreach (var d in e.Diagnostics)
                ctx.ReportDiagnostic(d);

        foreach (var g in entries.GroupBy(e => e.Name).Where(g => g.Count() > 1))
            foreach (var e in g)
                ctx.ReportDiagnostic(Diagnostic.Create(DuplicateName, e.Location, g.Key));

        if (entries.IsDefaultOrEmpty)
        {
            ctx.ReportDiagnostic(Diagnostic.Create(NoEntries, Location.None));
            return;
        }
        // Do not emit code for entries that already failed validation: the diagnostics above are the useful
        // output, and emitted code would only add a cascade of unrelated compiler errors.
        if (entries.Any(e => e.Diagnostics.Count > 0) || entries.GroupBy(e => e.Name).Any(g => g.Count() > 1))
            return;

        var sb = new StringBuilder();
        sb.AppendLine("// <auto-generated/> by Cleat.Generator. Do not edit.");
        sb.AppendLine("#nullable enable");
        sb.AppendLine("#pragma warning disable CS1998 // an async adapter over a synchronous entry has nothing to await");
        sb.AppendLine("namespace CleatWorkflowWorld");
        sb.AppendLine("{");
        sb.AppendLine("    public class CleatWorkflowWorldExportsImpl : global::CleatWorkflowWorld.ICleatWorkflowWorldExports");
        sb.AppendLine("    {");
        sb.AppendLine("        private static readonly global::Cleat.Runtime.DeferTable Defers = new(global::Cleat.HostCalls.SetDeferPhase);");
        sb.AppendLine("        private static readonly global::Cleat.HostCalls Host = new(Defers);");
        sb.AppendLine("        private static readonly global::Cleat.Runtime.EntryTable<global::Cleat.HostCalls> Table = new(new (string Name, global::Cleat.Runtime.EntryInvoker<global::Cleat.HostCalls> Invoke)[]");
        sb.AppendLine("        {");
        foreach (var e in entries)
        {
            var args = e.Input == Kind.None ? "h" : "h, " + Read(e);
            var call = $"{e.Type}.{e.Method}({args})";
            sb.AppendLine($"            ({Lit(e.Name)}, async (h, input) =>");
            sb.AppendLine("            {");
            if (e.Output == Kind.None)
            {
                sb.AppendLine($"                {(e.IsAsync ? "await " : "")}{call};");
                sb.AppendLine("                return global::Cleat.Runtime.EntryJson.Null;");
            }
            else
            {
                sb.AppendLine($"                var r = {(e.IsAsync ? "await " : "")}{call};");
                sb.AppendLine($"                return {Write(e, "r")};");
            }
            sb.AppendLine("            }),");
        }
        sb.AppendLine("        });");
        sb.AppendLine();
        sb.AppendLine("        public static global::CleatWorkflowWorld.ICleatWorkflowWorldImports.RunOutcome Run(string args)");
        sb.AppendLine("        {");
        sb.AppendLine("            var o = global::Cleat.Runtime.EntryHost.Run(args, Table, Host, Defers);");
        sb.AppendLine("            return o.Kind switch");
        sb.AppendLine("            {");
        sb.AppendLine("                global::Cleat.Runtime.OutcomeKind.Completed => global::CleatWorkflowWorld.ICleatWorkflowWorldImports.RunOutcome.Completed(o.Text!),");
        sb.AppendLine("                global::Cleat.Runtime.OutcomeKind.Suspended => global::CleatWorkflowWorld.ICleatWorkflowWorldImports.RunOutcome.Suspended(),");
        sb.AppendLine("                _ => global::CleatWorkflowWorld.ICleatWorkflowWorldImports.RunOutcome.Failed(o.Text ?? string.Empty),");
        sb.AppendLine("            };");
        sb.AppendLine("        }");
        sb.AppendLine();
        sb.AppendLine("        public static uint RunDeferred() => global::Cleat.Runtime.EntryHost.RunDeferred(Defers);");
        sb.AppendLine("    }");
        sb.AppendLine("}");
        ctx.AddSource("CleatWorkflowWorldExportsImpl.g.cs", sb.ToString());
    }
}
