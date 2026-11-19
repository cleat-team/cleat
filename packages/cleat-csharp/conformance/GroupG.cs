using System.Text.Json;
using System.Text.Json.Nodes;
using Cleat;

namespace Cleat.Conformance;

/// <summary>
/// Group G of cleat#4080's host-call coverage (tests/csharp/group_g_test.go): HTTP, recording a computed value, and an independent run.
/// Entry names are prefixed <c>g_</c>. An entry that can fail by design reports the failure as data; a
/// <see cref="CleatSuspendException"/> is never caught, because it is how a run ends.
/// </summary>
public static class GroupG
{
    private static JsonNode Failure(CleatCallException e) => new JsonObject
    {
        ["failed"] = true,
        ["type"] = e.GetType().Name,
        ["service"] = e.Service,
        ["operation"] = e.Operation,
        ["message"] = e.Message,
    };

    /// <summary>Input: url, method (optional), headers (optional object of strings), body (optional string).</summary>
    [CleatEntry("g_fetch")]
    public static JsonNode Fetch(HostCalls h, JsonElement input)
    {
        Dictionary<string, string>? headers = null;
        if (input.TryGetProperty("headers", out var hs))
        {
            headers = new Dictionary<string, string>();
            foreach (var p in hs.EnumerateObject())
                headers[p.Name] = p.Value.GetString()!;
        }
        var method = input.TryGetProperty("method", out var m) ? m.GetString()! : "GET";
        var body = input.TryGetProperty("body", out var b) ? b.GetString()! : "";
        try
        {
            var r = h.Fetch(input.GetProperty("url").GetString()!, method, headers, body);
            var hdr = new JsonObject();
            foreach (var (k, v) in r.Headers)
                hdr[k] = v;
            return new JsonObject { ["status"] = r.Status, ["body"] = r.Body, ["headers"] = hdr };
        }
        catch (CleatCallException e) { return Failure(e); }
    }

    /// <summary>Input: value (a string).</summary>
    [CleatEntry("g_side_effect")]
    public static JsonNode SideEffect(HostCalls h, JsonElement input)
    {
        try { return new JsonObject { ["recorded"] = h.SideEffect(input.GetProperty("value").GetString()!) }; }
        catch (CleatCallException e) { return Failure(e); }
    }

    /// <summary>Input: name, input (a string of JSON).</summary>
    [CleatEntry("g_run_detached")]
    public static JsonNode RunDetached(HostCalls h, JsonElement input)
    {
        try
        {
            h.RunDetached(input.GetProperty("name").GetString()!, input.GetProperty("input").GetString()!);
            return new JsonObject { ["started"] = true };
        }
        catch (CleatCallException e) { return Failure(e); }
    }
}
