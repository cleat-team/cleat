using System.Text.Json;
using System.Text.Json.Nodes;
using Cleat;

namespace Cleat.Conformance;

/// <summary>
/// Group D of cleat#4080's host-call coverage (tests/csharp/group_d_test.go): locks, the virtual-object scope and deterministic ids.
/// Entry names are prefixed <c>d_</c>. An entry that can fail by design reports the failure as data; a
/// <see cref="CleatSuspendException"/> is never caught, because it is how a run ends.
/// </summary>
public static class GroupD
{
    private static JsonNode Failure(CleatCallException e) => new JsonObject
    {
        ["failed"] = true,
        ["type"] = e.GetType().Name,
        ["code"] = (int)e.Code,
        ["message"] = e.Message,
    };

    /// <summary>Input: key, ttlMs.</summary>
    [CleatEntry("d_acquire")]
    public static JsonNode Acquire(HostCalls h, JsonElement input)
    {
        try
        {
            var got = h.AcquireLock(input.GetProperty("key").GetString()!, TimeSpan.FromMilliseconds(input.GetProperty("ttlMs").GetDouble()));
            return new JsonObject { ["acquired"] = got };
        }
        catch (CleatCallException e) { return Failure(e); }
        catch (ArgumentOutOfRangeException e) { return new JsonObject { ["invalid"] = e.ParamName }; }
    }

    /// <summary>Input: key. Acquires it (ttl one minute), then releases it, and reports both.</summary>
    [CleatEntry("d_acquire_release")]
    public static JsonNode AcquireRelease(HostCalls h, JsonElement input)
    {
        var key = input.GetProperty("key").GetString()!;
        var got = h.AcquireLock(key, TimeSpan.FromMinutes(1));
        h.ReleaseLock(key);
        return new JsonObject { ["acquired"] = got, ["released"] = true };
    }

    /// <summary>Input: key.</summary>
    [CleatEntry("d_release")]
    public static JsonNode Release(HostCalls h, JsonElement input)
    {
        try
        {
            h.ReleaseLock(input.GetProperty("key").GetString()!);
            return new JsonObject { ["released"] = true };
        }
        catch (CleatCallException e) { return Failure(e); }
    }

    private static JsonNode Scope(HostCalls h)
    {
        var (t, k) = h.GetScope();
        return new JsonObject { ["type"] = t, ["key"] = k };
    }

    /// <summary>Input: first {type,key}, second {type,key}. Sets the first, reads, switches to the second, reads, clears, reads.</summary>
    [CleatEntry("d_scope_walk")]
    public static JsonNode ScopeWalk(HostCalls h, JsonElement input)
    {
        var o = new JsonObject { ["initial"] = Scope(h) };
        var a = input.GetProperty("first");
        var b = input.GetProperty("second");
        o["prevOnFirst"] = h.SetScope(a.GetProperty("type").GetString()!, a.GetProperty("key").GetString()!);
        o["afterFirst"] = Scope(h);
        o["prevOnSecond"] = h.SetScope(b.GetProperty("type").GetString()!, b.GetProperty("key").GetString()!);
        o["afterSecond"] = Scope(h);
        o["prevOnClear"] = h.SetScope("", "");
        o["afterClear"] = Scope(h);
        return o;
    }

    /// <summary>Input: type, key. A half-empty scope must be refused by the SDK before the host.</summary>
    [CleatEntry("d_scope_partial")]
    public static JsonNode ScopePartial(HostCalls h, JsonElement input)
    {
        try
        {
            h.SetScope(input.GetProperty("type").GetString()!, input.GetProperty("key").GetString()!);
            return new JsonObject { ["refused"] = false };
        }
        catch (ArgumentException) { return new JsonObject { ["refused"] = true }; }
    }

    /// <summary>Input: type, key. Sets the scope and then makes an ordinary call; on contention the host suspends the run but the call still happens (cleat#3916).</summary>
    [CleatEntry("d_scope_then_call")]
    public static JsonNode ScopeThenCall(HostCalls h, JsonElement input)
    {
        h.SetScope(input.GetProperty("type").GetString()!, input.GetProperty("key").GetString()!);
        h.Call("svc", "after-scope", "{}");
        return new JsonObject { ["reached"] = true };
    }

    /// <summary>Input: seeds (array of strings). Returns the id for each seed in order, then the first seed's id again.</summary>
    [CleatEntry("d_uuid")]
    public static JsonNode Uuid(HostCalls h, JsonElement input)
    {
        var ids = new JsonArray();
        string? first = null;
        foreach (var s in input.GetProperty("seeds").EnumerateArray())
        {
            var id = h.Uuid(s.GetString()!);
            first ??= id;
            ids.Add(JsonValue.Create(id));
        }
        return new JsonObject { ["ids"] = ids, ["firstAgain"] = h.Uuid(input.GetProperty("seeds")[0].GetString()!) };
    }
}
