using System;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Text.Json;
using System.Threading.Tasks;

namespace Cleat.Runtime;

internal enum OutcomeKind { Completed, Suspended, Failed }

/// <summary>The binding-free form of the world's <c>run-outcome</c>.</summary>
internal readonly record struct Outcome(OutcomeKind Kind, string? Text)
{
    internal static Outcome Completed(string json) => new(OutcomeKind.Completed, json);
    internal static Outcome Suspended() => new(OutcomeKind.Suspended, null);
    internal static Outcome Failed(string message) => new(OutcomeKind.Failed, message);
}

/// <summary>One entry point: it receives the host and the input (already stripped of the dispatch key) and
/// returns the result serialised as JSON.</summary>
internal delegate Task<string> EntryInvoker<THost>(THost host, JsonElement input);

internal sealed class EntryTable<THost>
{
    private readonly List<(string Name, EntryInvoker<THost> Invoke)> _entries = new();

    internal EntryTable(IEnumerable<(string Name, EntryInvoker<THost> Invoke)> entries)
    {
        foreach (var e in entries)
        {
            if (_entries.Any(x => x.Name == e.Name))
                throw new InvalidOperationException($"two entry points are named '{e.Name}'");
            _entries.Add(e);
        }
    }

    internal int Count => _entries.Count;

    /// <summary>
    /// The entry to run for <paramref name="name"/>. A single registered entry needs no name, exactly as in
    /// python-sdk/cleat_sdk/entry.py; with several, the engine's <c>__cleat_entry__</c> key is required.
    /// </summary>
    internal EntryInvoker<THost> Resolve(string? name)
    {
        if (name is null)
        {
            if (_entries.Count == 1) return _entries[0].Invoke;
            throw new InvalidOperationException(
                $"{_entries.Count} entry points are registered ({string.Join(", ", _entries.Select(e => e.Name))}) " +
                $"but the input has no '{EntryHost.EntryKey}' field");
        }
        foreach (var e in _entries)
            if (e.Name == name) return e.Invoke;
        throw new InvalidOperationException(
            $"no entry point named '{name}' (registered: {string.Join(", ", _entries.Select(e => e.Name))})");
    }
}

/// <summary>
/// The workflow boundary: everything the generated <c>run</c> and <c>run-deferred</c> do, with no reference to
/// the generated bindings, so it runs under a plain <c>dotnet test</c>.
/// </summary>
internal static class EntryHost
{
    /// <summary>The key the engine folds into the input JSON to name the entry point
    /// (<c>withCleatEntryKey</c>, engine/component_cgo.go).</summary>
    internal const string EntryKey = "__cleat_entry__";

    /// <summary>
    /// Runs one segment. The control flow is python-sdk/cleat_sdk/entry.py's <c>export_wrapper</c>:
    /// <list type="bullet">
    /// <item>The body returns: the guest's own defers run, then the result is reported. A defer that suspends
    /// wins over the result.</item>
    /// <item>The body suspends: report <c>suspended</c> WITHOUT draining. The workflow has not exited, and the
    /// host drains on this same instance through <c>run-deferred</c> only if it is a defer segment.</item>
    /// <item>The body throws anything else: defers still run (cleanup exists for the run that did not finish
    /// as meant), then <c>failed</c>. This catch is the workflow error boundary; an exception that escaped it
    /// would cross the ABI as a trap and the engine would see a dead guest instead of a failed workflow.</item>
    /// </list>
    /// The body is awaited with <c>GetAwaiter().GetResult()</c>, never <c>.Result</c>: the latter wraps the
    /// exception in <see cref="AggregateException"/>.
    /// </summary>
    internal static Outcome Run<THost>(string args, EntryTable<THost> table, THost host, DeferTable defers)
    {
        try
        {
            var (name, input) = SplitInput(args);
            var json = table.Resolve(name)(host, input).GetAwaiter().GetResult();
            defers.Drain(propagateSuspend: true);
            return Outcome.Completed(json);
        }
        catch (Exception ex) when (ExceptionInfo.IsSuspend(ex))
        {
            return Outcome.Suspended();
        }
        catch (Exception ex)
        {
            try
            {
                defers.Drain(propagateSuspend: true);
            }
            catch (Exception inner) when (ExceptionInfo.IsSuspend(inner))
            {
                return Outcome.Suspended();
            }
            return Outcome.Failed(ExceptionInfo.Describe(ex));
        }
    }

    /// <summary>The <c>run-deferred</c> export: the host's drain of a segment that has already suspended.</summary>
    internal static uint RunDeferred(DeferTable defers) => defers.Drain(propagateSuspend: false);

    /// <summary>
    /// Splits the engine's input into the entry name and the input the entry receives. Empty input is
    /// <c>{}</c>. A non-object input has no dispatch key and is passed through unchanged. Numbers keep the exact
    /// digits they arrived with (<c>JsonProperty.WriteTo</c> copies a number's raw token), so a 20-digit integer
    /// or <c>1.50</c> is not reformatted on the way in. Strings are re-encoded, so an escape may be spelled
    /// differently (<c>\u00e9</c> arrives as <c>é</c>); the decoded value is the same.
    /// </summary>
    internal static (string? Name, JsonElement Input) SplitInput(string args)
    {
        using var doc = JsonDocument.Parse(string.IsNullOrWhiteSpace(args) ? "{}" : args);
        var root = doc.RootElement;
        if (root.ValueKind != JsonValueKind.Object)
            return (null, root.Clone());

        string? name = null;
        using var ms = new MemoryStream();
        using (var w = new Utf8JsonWriter(ms, new JsonWriterOptions { Encoder = EntryJson.Encoder }))
        {
            w.WriteStartObject();
            foreach (var p in root.EnumerateObject())
            {
                if (p.Name == EntryKey && p.Value.ValueKind == JsonValueKind.String)
                    name = p.Value.GetString();
                else
                    p.WriteTo(w);
            }
            w.WriteEndObject();
        }
        return (name, JsonDocument.Parse(ms.ToArray()).RootElement);
    }
}
