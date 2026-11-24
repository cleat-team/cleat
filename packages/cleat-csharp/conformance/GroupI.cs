using System.Text.Json;
using System.Text.Json.Nodes;
using Cleat;

namespace Cleat.Conformance;

/// <summary>
/// Group I of cleat#4080's host-call coverage (tests/csharp/group_i_test.go): large payloads and the memory limit. No WIT function of its own:
/// these are the sizes at which the boundary between the guest and the host can silently lose or refuse data. Entry names are prefixed
/// <c>i_</c>. An entry that can fail by design reports the failure as data; a <see cref="CleatSuspendException"/> is never caught.
/// </summary>
public static class GroupI
{
    private static JsonNode Failure(CleatCallException e) => new JsonObject
    {
        ["failed"] = true,
        ["type"] = e.GetType().Name,
        ["code"] = (int)e.Code,
        ["message"] = e.Message.Length > 300 ? e.Message[..300] + "..." : e.Message,
        ["messageChars"] = e.Message.Length,
    };

    /// <summary>FNV-1a over the UTF-8 bytes: a cheap content check that a truncated or altered response cannot pass by having the right length.</summary>
    private static ulong Fnv(string s)
    {
        ulong h = 14695981039346656037UL;
        foreach (var b in System.Text.Encoding.UTF8.GetBytes(s))
        {
            h ^= b;
            h *= 1099511628211UL;
        }
        return h;
    }

    /// <summary>Input: service, operation, requestChars. Sends a request whose JSON is that many characters long and reports what came back.</summary>
    [CleatEntry("i_call")]
    public static JsonNode Call(HostCalls h, JsonElement input)
    {
        var chars = input.GetProperty("requestChars").GetInt32();
        // A JSON string of 'x' padded so the whole request, quotes included, is exactly `chars` characters.
        var request = "\"" + new string('x', System.Math.Max(chars - 2, 0)) + "\"";
        try
        {
            var response = h.Call(input.GetProperty("service").GetString()!, input.GetProperty("operation").GetString()!, request);
            return new JsonObject
            {
                ["requestChars"] = request.Length,
                ["responseChars"] = response.Length,
                ["responseHash"] = Fnv(response).ToString(),
            };
        }
        catch (CleatCallException e) { return Failure(e); }
    }

    /// <summary>Input: mebibytes. Allocates and touches that much managed memory and reports it, to find where the guest's memory limit bites.</summary>
    [CleatEntry("i_alloc")]
    public static JsonNode Alloc(HostCalls h, JsonElement input)
    {
        var mib = input.GetProperty("mebibytes").GetInt32();
        var blocks = new System.Collections.Generic.List<byte[]>();
        for (var i = 0; i < mib; i++)
        {
            var b = new byte[1 << 20];
            for (var j = 0; j < b.Length; j += 4096)
                b[j] = (byte)i;
            blocks.Add(b);
        }
        return new JsonObject { ["allocatedMebibytes"] = blocks.Count };
    }
}
