using System.Text.Json;
using System.Text.Json.Nodes;
using System.Text.Json.Serialization;
using Cleat;

namespace Cleat.Conformance;

public sealed record PluginEcho(string Text, int Times);

/// <summary>Deliberately NOT declared in <see cref="GroupHJson"/>: primitives such as string are resolved without a declaration, a record is not.</summary>
public sealed record PluginUndeclared(int X);

/// <summary>The source-generated serializer context the typed plugin call needs under NativeAOT.</summary>
[JsonSerializable(typeof(PluginEcho))]
public sealed partial class GroupHJson : JsonSerializerContext { }

/// <summary>
/// Group H of cleat#4080's host-call coverage (tests/csharp/group_h_test.go): calling plugins. Entry names are prefixed <c>h_</c>. An entry
/// that can fail by design reports the failure as data; a <see cref="CleatSuspendException"/> is never caught, because it is how a run ends.
/// </summary>
public static class GroupH
{
    private static JsonNode Failure(CleatCallException e) => new JsonObject
    {
        ["failed"] = true,
        ["type"] = e.GetType().Name,
        ["code"] = (int)e.Code,
        ["service"] = e.Service,
        ["operation"] = e.Operation,
        ["message"] = e.Message,
    };

    /// <summary>Input: plugin, function, input (a string of JSON).</summary>
    [CleatEntry("h_plugin_call")]
    public static JsonNode PluginCall(HostCalls h, JsonElement input)
    {
        try
        {
            var reply = h.PluginCall(input.GetProperty("plugin").GetString()!, input.GetProperty("function").GetString()!, input.GetProperty("input").GetString()!);
            return new JsonObject { ["reply"] = reply };
        }
        catch (CleatCallException e) { return Failure(e); }
    }

    /// <summary>Input: plugin, function, text, times. Calls the typed overload with a PluginEcho and reads a PluginEcho back.</summary>
    [CleatEntry("h_plugin_call_typed")]
    public static JsonNode PluginCallTyped(HostCalls h, JsonElement input)
    {
        try
        {
            var back = h.PluginCall<PluginEcho, PluginEcho>(input.GetProperty("plugin").GetString()!, input.GetProperty("function").GetString()!,
                new PluginEcho(input.GetProperty("text").GetString()!, input.GetProperty("times").GetInt32()), GroupHJson.Default);
            return new JsonObject { ["text"] = back.Text, ["times"] = back.Times };
        }
        catch (CleatCallException e) { return Failure(e); }
    }

    /// <summary>Input: plugin, function, input.</summary>
    [CleatEntry("h_plugin_stream")]
    public static JsonNode PluginStream(HostCalls h, JsonElement input)
    {
        try
        {
            var chunks = h.PluginCallStreaming(input.GetProperty("plugin").GetString()!, input.GetProperty("function").GetString()!, input.GetProperty("input").GetString()!);
            var a = new JsonArray();
            foreach (var c in chunks)
                a.Add(new JsonObject { ["i"] = c.Index, ["c"] = c.Content, ["f"] = c.Finish });
            return new JsonObject { ["chunks"] = a };
        }
        catch (CleatCallException e) { return Failure(e); }
    }

    /// <summary>Input: plugin, function. The response type is one the context does not declare, which must fail BEFORE the plugin is called.</summary>
    [CleatEntry("h_plugin_call_undeclared")]
    public static JsonNode PluginCallUndeclared(HostCalls h, JsonElement input)
    {
        h.PluginCall<PluginEcho, PluginUndeclared>(input.GetProperty("plugin").GetString()!, input.GetProperty("function").GetString()!, new PluginEcho("x", 1), GroupHJson.Default);
        return new JsonObject { ["reached"] = true };
    }
}
